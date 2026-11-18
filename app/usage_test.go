package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useTempUsage 把用量记录指到临时文件，测试不能写进用户的 LOCALAPPDATA
func useTempUsage(t *testing.T) {
	t.Helper()
	usagePath = filepath.Join(t.TempDir(), usageFile)
	openUsage()
	if usageF == nil {
		t.Fatalf("临时用量文件没打开: %s", usagePath)
	}
	t.Cleanup(closeUsage)
}

func readUsage(t *testing.T) []usageRec {
	t.Helper()
	var out []usageRec
	if err := eachUsage(func(r usageRec) { out = append(out, r) }); err != nil {
		t.Fatalf("读取用量记录失败: %v", err)
	}
	return out
}

// chunkReader 每次 Read 只给一块，用来制造被切开的数据包
type chunkReader struct {
	chunks []string
	i      int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.i])
	c.i++
	return n, nil
}

func (c *chunkReader) Close() error { return nil }

// withConfigForTest 临时换掉全局配置，返回还原函数
func withConfigForTest(t *testing.T, c *Config) func() {
	t.Helper()
	cfgMu.Lock()
	old, oldPath := cfg, cfgPath
	cfg = c
	cfgMu.Unlock()
	return func() {
		cfgMu.Lock()
		cfg, cfgPath = old, oldPath
		cfgMu.Unlock()
	}
}

func splitEvery(s string, n int) []string {
	var out []string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

func drain(t *testing.T, body io.ReadCloser) {
	t.Helper()
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	body.Close()
}

func TestUsageTeeRecordsNonStreamUsage(t *testing.T) {
	useTempUsage(t)
	body := `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`
	tee := newUsageTee(&chunkReader{chunks: splitEvery(body, 7)}, usageRec{
		Date: "2026-01-05", Provider: "OpenCode Go", Model: "glm-5.3", Status: 200,
	})
	drain(t, tee)

	got := readUsage(t)
	if len(got) != 1 {
		t.Fatalf("记录数 = %d，想要 1 条: %+v", len(got), got)
	}
	if got[0].Prompt != 12 || got[0].Completion != 34 {
		t.Errorf("token = %d/%d，想要 12/34", got[0].Prompt, got[0].Completion)
	}
	if got[0].Provider != "OpenCode Go" || got[0].Model != "glm-5.3" || got[0].Date != "2026-01-05" || got[0].Status != 200 {
		t.Errorf("记录字段不对: %+v", got[0])
	}
}

func TestUsageTeeHandlesNullUsageAndInputTokens(t *testing.T) {
	useTempUsage(t)
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":null}\n\n",
		"data: {\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}\n\n",
	}
	tee := newUsageTee(&chunkReader{chunks: chunks}, usageRec{Date: "2026-01-05", Provider: "P", Model: "m", Status: 200})
	drain(t, tee)

	got := readUsage(t)
	if len(got) != 1 {
		t.Fatalf("记录数 = %d，想要 1 条", len(got))
	}
	if got[0].Prompt != 5 || got[0].Completion != 2 {
		t.Errorf("token = %d/%d，想要 5/2", got[0].Prompt, got[0].Completion)
	}
}

func TestUsageTeeKeepsLargestOfStreamingUpdates(t *testing.T) {
	useTempUsage(t)
	chunks := []string{
		`event: message_start
data: {"usage":{"input_tokens":100,"output_tokens":0}}

`,
		`event: message_delta
data: {"usage":{"output_tokens":42}}

`,
	}
	tee := newUsageTee(&chunkReader{chunks: chunks}, usageRec{Date: "2026-01-05", Provider: "P", Model: "m", Status: 200})
	drain(t, tee)

	got := readUsage(t)
	if len(got) != 1 || got[0].Prompt != 100 || got[0].Completion != 42 {
		t.Fatalf("记录 = %+v，想要输入 100 / 输出 42", got)
	}
}

// 模型输出里带引号转义的 usage 文本不能当成用量，否则会凭空造出 token
func TestUsageTeeIgnoresUsageMentionedInContent(t *testing.T) {
	useTempUsage(t)
	inner, err := json.Marshal(`字段 "usage": {"prompt_tokens": 999, "completion_tokens": 999}`)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":11,"completion_tokens":22}}`, inner)
	tee := newUsageTee(&chunkReader{chunks: []string{body}}, usageRec{Date: "2026-01-05", Provider: "P", Model: "m", Status: 200})
	drain(t, tee)

	got := readUsage(t)
	if len(got) != 1 || got[0].Prompt != 11 || got[0].Completion != 22 {
		t.Fatalf("记录 = %+v，想要 11/22", got)
	}
}

// 客户端中途断开：请求数还得记上，取不到的 token 就留空
func TestUsageTeeFlushesOnEarlyClose(t *testing.T) {
	useTempUsage(t)
	body := `{"usage":{"prompt_tokens":9,"completion_tokens":9}}`
	tee := newUsageTee(&chunkReader{chunks: splitEvery(body, 6)}, usageRec{Date: "2026-01-05", Provider: "P", Model: "m", Status: 200})
	p := make([]byte, 6)
	if _, err := tee.Read(p); err != nil {
		t.Fatalf("第一块读取失败: %v", err)
	}
	if err := tee.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	got := readUsage(t)
	if len(got) != 1 {
		t.Fatalf("记录数 = %d，想要 1 条", len(got))
	}
	if got[0].Prompt != 0 || got[0].Completion != 0 {
		t.Errorf("没读完不该有 token，实际 %d/%d", got[0].Prompt, got[0].Completion)
	}
}

func TestJSONObjectAtKeepsStringsAndNesting(t *testing.T) {
	raw := []byte(`:{"a":{"b":"}\"}"},"c":1}尾巴`)
	obj, used, err := jsonObjectAt(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if string(obj) != `{"a":{"b":"}\"}"},"c":1}` {
		t.Errorf("对象取到 %s", obj)
	}
	if used != len(raw)-len("尾巴") {
		t.Errorf("consumed = %d，想要到对象末尾", used)
	}

	if _, _, err := jsonObjectAt([]byte(`{"a":1,`)); err != errBodyShort {
		t.Errorf("没收全时应该报 errBodyShort，实际 %v", err)
	}
	if _, _, err := jsonObjectAt([]byte(`:null`)); err == nil || err == errBodyShort {
		t.Errorf("usage 不是对象应该报错，实际 %v", err)
	}
}

func TestUsageSummaryGroupsByDayProviderModel(t *testing.T) {
	useTempUsage(t)
	for _, r := range []usageRec{
		{Date: "2025-12-31", Provider: "A", Model: "m1", Status: 200, Prompt: 99, Completion: 99},
		{Date: "2026-01-01", Provider: "A", Model: "m1", Status: 200, Prompt: 10, Completion: 5},
		{Date: "2026-01-02", Provider: "A", Model: "m1", Status: 502},
		{Date: "2026-01-03", Provider: "B", Model: "m2", Status: 200, Prompt: 1, Completion: 1},
	} {
		appendUsage(r)
	}

	rep, err := usageSummary("2026-01-01", "2026-01-03")
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	if rep["from"] != "2026-01-01" || rep["to"] != "2026-01-03" || rep["day_count"] != 3 {
		t.Errorf("区间不对: %v %v %v", rep["from"], rep["to"], rep["day_count"])
	}
	if rep["records"] != 4 || rep["first_date"] != "2025-12-31" || rep["last_date"] != "2026-01-03" {
		t.Errorf("记录概况不对: %v %v %v", rep["records"], rep["first_date"], rep["last_date"])
	}

	daily, ok := rep["daily"].([]usageDayRow)
	if !ok || len(daily) != 3 {
		t.Fatalf("daily = %+v", rep["daily"])
	}
	if daily[0].Date != "2026-01-01" || daily[0].Reqs != 1 || daily[0].Tokens != 15 {
		t.Errorf("第一天 = %+v，想要 1 次请求 / 15 token", daily[0])
	}
	if daily[1].Failed != 1 || daily[1].Tokens != 0 {
		t.Errorf("第二天 = %+v，想要 1 次失败 / 0 token", daily[1])
	}

	providers := rep["providers"].([]usageRow)
	if len(providers) != 2 || providers[0].Provider != "A" || providers[0].Reqs != 2 || providers[0].Failed != 1 {
		t.Errorf("供应商汇总 = %+v", providers)
	}
	models := rep["models"].([]usageRow)
	if len(models) != 2 || models[0].Model != "m1" {
		t.Errorf("模型汇总 = %+v", models)
	}

	series := rep["series"].([]usageSeries)
	if len(series) != 2 || series[0].Provider != "A" {
		t.Fatalf("图表序列 = %+v", series)
	}
	if fmt.Sprint(series[0].Reqs) != "[1 1 0]" || fmt.Sprint(series[1].Reqs) != "[0 0 1]" {
		t.Errorf("按天序列不对: %v / %v", series[0].Reqs, series[1].Reqs)
	}
	if fmt.Sprint(series[0].Tokens) != "[15 0 0]" || fmt.Sprint(series[1].Tokens) != "[0 0 2]" {
		t.Errorf("按天 token 不对: %v / %v", series[0].Tokens, series[1].Tokens)
	}

	totals := rep["totals"].(usageAgg)
	if totals.Reqs != 3 || totals.Failed != 1 || totals.Tokens != 17 {
		t.Errorf("合计 = %+v", totals)
	}
}

func TestUsageSummaryRejectsBadRanges(t *testing.T) {
	useTempUsage(t)
	if _, err := usageSummary("", "2026-01-03"); err == nil {
		t.Error("空开始日期应该报错")
	}
	if _, err := usageSummary("2026/01/03", "2026-01-01"); err == nil {
		t.Error("结束早于开始应该报错")
	}
	if _, err := usageSummary("2020-01-01", "2026-01-01"); err == nil || !strings.Contains(err.Error(), "366") {
		t.Errorf("超过一年应该报错，实际 %v", err)
	}
	if _, err := usageSummary("2026-01-01", "2026-01-01"); err != nil {
		t.Errorf("单日区间应该合法: %v", err)
	}
}

func TestUsageDateUsesLocalDay(t *testing.T) {
	got := usageDate(time.Date(2026, 1, 5, 23, 30, 0, 0, time.Local))
	if got != "2026-01-05" {
		t.Errorf("usageDate = %s", got)
	}
}

// GET 不算用量；非推理入口不读请求体，模型名只能记 "-"
func TestGatewayRecordsOnlyPostedRequests(t *testing.T) {
	useTempUsage(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
			return
		}
		http.Error(w, `nope`, http.StatusNotFound)
	}))
	defer up.Close()

	restore := withConfigForTest(t, &Config{
		DefaultProvider: "p1",
		Providers: []*Provider{{
			ID: "p1", Name: "桩", BaseURL: up.URL + "/v1",
			Models: []Model{{ID: "mock-fast", Default: true}},
		}},
	})
	defer restore()

	gw := httptest.NewServer(newGateway())
	defer gw.Close()

	for _, path := range []string{"/favicon.ico", "/v1/models"} {
		resp, err := http.Get(gw.URL + path)
		if err != nil {
			t.Fatalf("GET %s 失败: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := readUsage(t); len(got) != 0 {
		t.Fatalf("GET 不该记用量: %+v", got)
	}

	res, err := http.Post(gw.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"model":"mock-embed","input":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.ReadAll(res.Body)

	got := readUsage(t)
	if len(got) != 1 {
		t.Fatalf("POST 记录数 = %d: %+v", len(got), got)
	}
	if got[0].Model != "-" {
		t.Errorf("非推理入口的模型名 = %q，想要 \"-\"", got[0].Model)
	}
	if got[0].Prompt != 1 || got[0].Completion != 2 {
		t.Errorf("token = %d/%d，想要 1/2", got[0].Prompt, got[0].Completion)
	}
}

// 端到端：客户端打本机网关，记录里要能看到实际模型与上游给的 token
func TestGatewayWritesUsageRecord(t *testing.T) {
	useTempUsage(t)

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-key" {
			t.Errorf("上游没收到配置里的 Key: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("上游路径 = %s，想要 /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	})
	up := httptest.NewServer(upstream)
	defer up.Close()

	restore := withConfigForTest(t, &Config{
		DefaultProvider: "p1",
		Providers: []*Provider{{
			ID: "p1", Name: "测试供应商", BaseURL: up.URL + "/v1", APIKey: "upstream-key",
			Models: []Model{{ID: "glm-5.3", Context: 1000, MaxOutput: 100, Default: true}},
		}},
	})
	defer restore()

	gw := httptest.NewServer(newGateway())
	defer gw.Close()

	body := `{"model":"glm-5.3","messages":[{"role":"user","content":"你好"}],"max_tokens":999999}`
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	got := readUsage(t)
	if len(got) != 1 {
		t.Fatalf("记录数 = %d，想要 1 条: %+v", len(got), got)
	}
	r0 := got[0]
	if r0.Provider != "测试供应商" || r0.Model != "glm-5.3" || r0.Status != 200 {
		t.Errorf("记录 = %+v", r0)
	}
	if r0.Prompt != 3 || r0.Completion != 4 {
		t.Errorf("token = %d/%d，想要 3/4", r0.Prompt, r0.Completion)
	}
	if r0.Date != usageDate(time.Now()) {
		t.Errorf("日期 = %s，想要今天 %s", r0.Date, usageDate(time.Now()))
	}
}

// 转发失败也要留下一次请求，否则失败率永远是 0
func TestGatewayWritesFailedUsageRecord(t *testing.T) {
	useTempUsage(t)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	restore := withConfigForTest(t, &Config{
		DefaultProvider: "p1",
		Providers:       []*Provider{{ID: "p1", Name: "挂了", BaseURL: deadURL + "/v1"}},
	})
	defer restore()

	gw := httptest.NewServer(newGateway())
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"随便","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，想要 502", resp.StatusCode)
	}

	got := readUsage(t)
	if len(got) != 1 || got[0].Status != 502 || got[0].Provider != "挂了" {
		t.Fatalf("失败记录 = %+v", got)
	}
	if got[0].Model != "随便" {
		t.Errorf("没命中配置时该记客户端带来的模型名，实际 %q", got[0].Model)
	}
}
