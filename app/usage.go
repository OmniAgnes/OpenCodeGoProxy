package main

// 用量统计: 网关每转发一个请求就在 %LOCALAPPDATA%\OpenCodeGoProxy\usage.jsonl 追加一行,
// 界面按「天 × 供应商 × 模型」汇总。token 数从上游响应体里的 usage 对象取,
// 上游没给（例如流式没带 stream_options.include_usage）就只记请求数, 不做估算。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	usageFile       = "usage.jsonl"
	usageDateLayout = "2006-01-02"
	usageScanCap    = 16 << 10 // 回看响应体尾部多少字节找 usage 对象
	usageMaxDays    = 366      // 一次最多统计多少天
)

// usageRec 一行记录: 本地日期 + 供应商名 + 实际使用的模型 + 状态码 + token
type usageRec struct {
	Date       string `json:"d"`
	Provider   string `json:"p"`
	Model      string `json:"m"`
	Status     int    `json:"s"`
	Prompt     int    `json:"pr,omitempty"`
	Completion int    `json:"co,omitempty"`
}

var (
	usageMu      sync.Mutex
	usageF       *os.File
	usagePath    string
	usageKey     = []byte(`"usage"`)
	errBodyShort = errors.New("响应体还没收完")
)

func usageDate(t time.Time) string { return t.In(time.Local).Format(usageDateLayout) }

func initUsage() {
	usagePath = filepath.Join(configDir(), usageFile)
	openUsage()
}

// openUsage 打开记录文件；打不开只影响用量写不进去，不影响转发
func openUsage() {
	if err := os.MkdirAll(filepath.Dir(usagePath), 0o755); err != nil {
		logf("用量记录目录不可用: %v", err)
		return
	}
	f, err := os.OpenFile(usagePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		logf("用量记录写不了: %v", err)
		return
	}
	usageMu.Lock()
	usageF = f
	usageMu.Unlock()
}

func closeUsage() {
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageF != nil {
		usageF.Close()
		usageF = nil
	}
}

func appendUsage(rec usageRec) {
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageF == nil {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if _, err := usageF.Write(append(line, '\n')); err != nil {
		logf("用量记录写入失败: %v", err)
	}
}

// usageTee 包一层响应体: 边转发边找 usage 对象, 读完（或客户端中断被 Close）时落一条记录,
// 保证请求数不会因为流被掐掉而漏记。
type usageTee struct {
	src      io.ReadCloser
	buf      []byte
	prompt   int
	complete int
	rec      usageRec
	flushed  bool
}

func newUsageTee(src io.ReadCloser, rec usageRec) *usageTee {
	return &usageTee{src: src, rec: rec}
}

func (t *usageTee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.feed(p[:n])
	}
	if err != nil {
		t.flush()
	}
	return n, err
}

func (t *usageTee) Close() error {
	t.flush()
	return t.src.Close()
}

func (t *usageTee) feed(chunk []byte) {
	t.buf = append(t.buf, chunk...)
	for {
		i := bytes.Index(t.buf, usageKey)
		if i < 0 {
			t.keepTail()
			return
		}
		rest := t.buf[i+len(usageKey):]
		obj, used, err := jsonObjectAt(rest)
		if errors.Is(err, errBodyShort) {
			// 对象被分块切开了, 留住 key 等下一块
			t.buf = t.buf[i:]
			t.keepTail()
			return
		}
		if err != nil {
			// 不是对象（比如流式里的 "usage":null）, 跳过 key 继续找
			t.buf = rest[used:]
			continue
		}
		t.merge(obj)
		t.buf = rest[used:]
	}
}

// keepTail 只留尾部字节, 避免长响应把内存吃下来; 代价是 usage 出现在很靠前时取不到
func (t *usageTee) keepTail() {
	if len(t.buf) > usageScanCap {
		t.buf = t.buf[len(t.buf)-usageScanCap:]
	}
}

func (t *usageTee) merge(obj []byte) {
	var u usageFields
	if json.Unmarshal(obj, &u) != nil {
		return
	}
	in, out := u.pair()
	// 流式会分几段给（Anthropic 的 message_start 给输入、message_delta 给输出）, 各取见过的最大值
	if in > t.prompt {
		t.prompt = in
	}
	if out > t.complete {
		t.complete = out
	}
}

func (t *usageTee) flush() {
	if t.flushed {
		return
	}
	t.flushed = true
	t.rec.Prompt, t.rec.Completion = t.prompt, t.complete
	appendUsage(t.rec)
}

type usageFields struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

// pair 归一各家字段名: OpenAI 兼容是 prompt/completion_tokens, Responses 与 Anthropic 是 input/output_tokens
func (u usageFields) pair() (int, int) {
	in, out := u.PromptTokens, u.CompletionTokens
	if in == 0 {
		in = u.InputTokens
	}
	if out == 0 {
		out = u.OutputTokens
	}
	return in, out
}

// jsonObjectAt 跳过空白与冒号, 从 raw 开头取一个花括号配平的 JSON 对象
func jsonObjectAt(raw []byte) ([]byte, int, error) {
	i := 0
	for i < len(raw) && (raw[i] == ':' || raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
		i++
	}
	if i == len(raw) {
		return nil, i, errBodyShort
	}
	if raw[i] != '{' {
		return nil, i, fmt.Errorf("usage 不是对象")
	}
	var depth int
	var inStr, esc bool
	for j := i; j < len(raw); j++ {
		c := raw[j]
		switch {
		case inStr:
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return raw[i : j+1], j + 1, nil
			}
		}
	}
	return nil, len(raw), errBodyShort
}

type usageAgg struct {
	Reqs       int `json:"reqs"`
	Failed     int `json:"failed"`
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Tokens     int `json:"tokens"`
}

func (a *usageAgg) add(r usageRec) {
	a.Reqs++
	if r.Status >= 400 {
		a.Failed++
	}
	a.Prompt += r.Prompt
	a.Completion += r.Completion
	a.Tokens += r.Prompt + r.Completion
}

type usageDayRow struct {
	Date string `json:"date"`
	usageAgg
}

type usageRow struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	usageAgg
}

type usageSeries struct {
	Provider string `json:"provider"`
	Reqs     []int  `json:"reqs"`
	Tokens   []int  `json:"tokens"`
}

// eachUsage 逐行读记录, 解析不了的行跳过; 文件还没生成不算错误
func eachUsage(fn func(usageRec)) error {
	usageMu.Lock()
	defer usageMu.Unlock()
	f, err := os.Open(usagePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var r usageRec
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			fn(r)
		}
	}
	return sc.Err()
}

// usageSummary 按 [from, to] 闭区间汇总, 返回界面直接可用的图表数据与表格
func usageSummary(fromRaw, toRaw string) (map[string]any, error) {
	from, err := time.ParseInLocation(usageDateLayout, strings.TrimSpace(fromRaw), time.Local)
	if err != nil {
		return nil, fmt.Errorf("开始日期要填 %s 这种格式, 现在是 %q", usageDateLayout, fromRaw)
	}
	to, err := time.ParseInLocation(usageDateLayout, strings.TrimSpace(toRaw), time.Local)
	if err != nil {
		return nil, fmt.Errorf("结束日期要填 %s 这种格式, 现在是 %q", usageDateLayout, toRaw)
	}
	if to.Before(from) {
		return nil, errors.New("结束日期早于开始日期")
	}
	days := int(to.Sub(from).Hours()/24) + 1
	if days > usageMaxDays {
		return nil, fmt.Errorf("一次最多统计 %d 天", usageMaxDays)
	}

	labels := make([]string, 0, days)
	at := map[string]int{}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		at[d.Format(usageDateLayout)] = len(labels)
		labels = append(labels, d.Format(usageDateLayout))
	}
	daily := make([]usageDayRow, len(labels))
	for i, label := range labels {
		daily[i].Date = label
	}

	byProvider := map[string]*usageRow{}
	byModel := map[string]*usageRow{}
	series := map[string]*usageSeries{}
	var total usageAgg
	var records int
	firstDate, lastDate := "", ""

	err = eachUsage(func(r usageRec) {
		records++
		if firstDate == "" || r.Date < firstDate {
			firstDate = r.Date
		}
		if r.Date > lastDate {
			lastDate = r.Date
		}
		i, ok := at[r.Date]
		if !ok {
			return
		}
		daily[i].add(r)
		total.add(r)

		p := byProvider[r.Provider]
		if p == nil {
			p = &usageRow{Provider: r.Provider}
			byProvider[r.Provider] = p
		}
		p.add(r)

		key := r.Provider + "\x00" + r.Model
		m := byModel[key]
		if m == nil {
			m = &usageRow{Provider: r.Provider, Model: r.Model}
			byModel[key] = m
		}
		m.add(r)

		s := series[r.Provider]
		if s == nil {
			s = &usageSeries{Provider: r.Provider, Reqs: make([]int, len(labels)), Tokens: make([]int, len(labels))}
			series[r.Provider] = s
		}
		s.Reqs[i]++
		s.Tokens[i] += r.Prompt + r.Completion
	})
	if err != nil {
		return nil, fmt.Errorf("读取用量记录失败: %w", err)
	}

	providers := make([]usageRow, 0, len(byProvider))
	for _, p := range byProvider {
		providers = append(providers, *p)
	}
	sortRows(providers)

	models := make([]usageRow, 0, len(byModel))
	for _, m := range byModel {
		models = append(models, *m)
	}
	sortRows(models)

	lines := make([]usageSeries, 0, len(series))
	for _, s := range series {
		lines = append(lines, *s)
	}
	sort.Slice(lines, func(i, j int) bool { return sumSeries(lines[i]) > sumSeries(lines[j]) })

	return map[string]any{
		"from":       labels[0],
		"to":         labels[len(labels)-1],
		"day_count":  len(labels),
		"daily":      daily,
		"providers":  providers,
		"models":     models,
		"series":     lines,
		"totals":     total,
		"records":    records,
		"first_date": firstDate,
		"last_date":  lastDate,
	}, nil
}

func sortRows(rows []usageRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Reqs != rows[j].Reqs {
			return rows[i].Reqs > rows[j].Reqs
		}
		if rows[i].Tokens != rows[j].Tokens {
			return rows[i].Tokens > rows[j].Tokens
		}
		return rows[i].Provider < rows[j].Provider
	})
}

func sumSeries(s usageSeries) int {
	var v int
	for _, x := range s.Tokens {
		v += x
	}
	return v
}
