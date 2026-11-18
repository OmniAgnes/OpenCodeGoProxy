package main

// 管理界面: 浏览器打开 http://127.0.0.1:PORT/_admin
// 只提供本机回环访问; 接口只回掩码密钥, 原始 Key 不出配置文件的读接口

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed admin.html
var adminFS embed.FS

type adminAPI struct{}

func newAdminHandler() *adminAPI { return &adminAPI{} }

func (a *adminAPI) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_admin" || r.URL.Path == "/_admin/":
		page, err := adminFS.ReadFile("admin.html")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "admin_error", "管理页面读取失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
		return
	case r.Method != http.MethodPost && r.URL.Path != "/_api/config":
		writeJSONError(w, http.StatusMethodNotAllowed, "admin_error", "接口方法不允许: "+r.Method)
		return
	}

	switch r.URL.Path {
	case "/_api/config":
		a.writeConfig(w)
	case "/_api/provider/save":
		a.saveProvider(w, r)
	case "/_api/provider/delete":
		a.deleteProvider(w, r)
	case "/_api/provider/default":
		a.setDefaultProvider(w, r)
	case "/_api/model/save":
		a.saveModel(w, r)
	case "/_api/model/delete":
		a.deleteModel(w, r)
	case "/_api/model/default":
		a.setDefaultModel(w, r)
	case "/_api/models/fetch":
		a.fetchModels(w, r)
	case "/_api/models/import":
		a.importModels(w, r)
	case "/_api/usage":
		a.usage(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "admin_error", "未知接口: "+r.URL.Path)
	}
}

type providerView struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	BaseURL   string  `json:"base_url"`
	IsGo      bool    `json:"is_go"`
	KeyMasked string  `json:"key_masked"`
	KeySet    bool    `json:"key_set"`
	IsDefault bool    `json:"is_default"`
	Models    []Model `json:"models"`
}

func (a *adminAPI) writeConfig(w http.ResponseWriter) {
	c := snapshot()
	views := make([]providerView, 0, len(c.Providers))
	for _, p := range c.Providers {
		models := p.Models
		if models == nil {
			models = []Model{}
		}
		views = append(views, providerView{
			ID:        p.ID,
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			IsGo:      isGo(p.BaseURL),
			KeyMasked: maskKey(p.APIKey),
			KeySet:    p.APIKey != "",
			IsDefault: p.ID == c.DefaultProvider,
			Models:    models,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"providers":       views,
		"default":         c.DefaultProvider,
		"client_base_url": clientBaseURL(),
		"config_path":     cfgPath,
		"usage_path":      usagePath,
		"config_error":    cfgLoadErr,
	})
}

func decodeBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func (a *adminAPI) saveProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "供应商名称不能为空")
		return
	}
	base, err := normalizeBaseURL(in.BaseURL)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", err.Error())
		return
	}
	err = update(func(c *Config) error {
		if in.ID == "" {
			c.Providers = append(c.Providers, &Provider{
				ID:      newID(),
				Name:    name,
				BaseURL: base,
				APIKey:  strings.TrimSpace(in.APIKey),
			})
			if c.DefaultProvider == "" {
				c.DefaultProvider = c.Providers[len(c.Providers)-1].ID
			}
			return nil
		}
		p := findProvider(c, in.ID)
		if p == nil {
			return fmt.Errorf("供应商不存在: %s", in.ID)
		}
		p.Name, p.BaseURL = name, base
		if key := strings.TrimSpace(in.APIKey); key != "" {
			p.APIKey = key
		}
		return nil
	})
	writeResult(w, err, "已保存")
}

func (a *adminAPI) deleteProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	err := update(func(c *Config) error {
		for i, p := range c.Providers {
			if p.ID == in.ID {
				c.Providers = append(c.Providers[:i], c.Providers[i+1:]...)
				if c.DefaultProvider == in.ID {
					c.DefaultProvider = ""
					if len(c.Providers) > 0 {
						c.DefaultProvider = c.Providers[0].ID
					}
				}
				return nil
			}
		}
		return fmt.Errorf("供应商不存在: %s", in.ID)
	})
	writeResult(w, err, "已删除")
}

func (a *adminAPI) setDefaultProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	err := update(func(c *Config) error {
		if findProvider(c, in.ID) == nil {
			return fmt.Errorf("供应商不存在: %s", in.ID)
		}
		c.DefaultProvider = in.ID
		return nil
	})
	writeResult(w, err, "已设为默认")
}

func (a *adminAPI) saveModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProviderID string `json:"provider_id"`
		OriginalID string `json:"original_id"`
		ID         string `json:"id"`
		Context    string `json:"context"`
		MaxOutput  string `json:"max_output"`
		Thinking   bool   `json:"thinking"`
		Default    bool   `json:"default"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "模型 ID 不能为空")
		return
	}
	contextTokens, err := parseSize(in.Context, "上下文")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", err.Error())
		return
	}
	maxOutput, err := parseSize(in.MaxOutput, "最大输出")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", err.Error())
		return
	}
	if contextTokens > 0 && maxOutput > 0 && maxOutput >= contextTokens {
		writeJSONError(w, http.StatusBadRequest, "admin_error",
			fmt.Sprintf("最大输出(%d)不能不小于上下文(%d)", maxOutput, contextTokens))
		return
	}

	err = update(func(c *Config) error {
		p := findProvider(c, in.ProviderID)
		if p == nil {
			return fmt.Errorf("供应商不存在: %s", in.ProviderID)
		}
		idx := -1
		for i := range p.Models {
			if p.Models[i].ID == in.OriginalID || (in.OriginalID == "" && p.Models[i].ID == id) {
				idx = i
				break
			}
		}
		if idx < 0 && in.OriginalID == "" {
			for i := range p.Models {
				if strings.EqualFold(p.Models[i].ID, id) {
					return fmt.Errorf("模型 %s 已存在于供应商 %s", id, p.Name)
				}
			}
		}
		if idx < 0 {
			p.Models = append(p.Models, Model{ID: id})
			idx = len(p.Models) - 1
		}
		m := &p.Models[idx]
		m.ID, m.Context, m.MaxOutput, m.Thinking = id, contextTokens, maxOutput, in.Thinking
		if m.Default && !in.Default {
			m.Default = false
		}
		if in.Default {
			clearDefault(p)
			m.Default = true
		}
		return nil
	})
	writeResult(w, err, "已保存")
}

func (a *adminAPI) deleteModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProviderID string `json:"provider_id"`
		ModelID    string `json:"model_id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	err := update(func(c *Config) error {
		p := findProvider(c, in.ProviderID)
		if p == nil {
			return fmt.Errorf("供应商不存在: %s", in.ProviderID)
		}
		for i := range p.Models {
			if p.Models[i].ID == in.ModelID {
				p.Models = append(p.Models[:i], p.Models[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("模型不存在: %s", in.ModelID)
	})
	writeResult(w, err, "已删除")
}

func (a *adminAPI) setDefaultModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProviderID string `json:"provider_id"`
		ModelID    string `json:"model_id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	err := update(func(c *Config) error {
		p := findProvider(c, in.ProviderID)
		if p == nil {
			return fmt.Errorf("供应商不存在: %s", in.ProviderID)
		}
		for i := range p.Models {
			if p.Models[i].ID == in.ModelID {
				clearDefault(p)
				p.Models[i].Default = true
				c.DefaultProvider = p.ID
				return nil
			}
		}
		return fmt.Errorf("模型不存在: %s", in.ModelID)
	})
	writeResult(w, err, "已设为默认")
}

// fetchModels 调上游 OpenAI 格式的 GET /models, 顺带取回能拿到的上下文/输出/思考元数据
func (a *adminAPI) fetchModels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProviderID string `json:"provider_id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	cfgMu.RLock()
	p := findProvider(cfg, in.ProviderID)
	if p == nil {
		cfgMu.RUnlock()
		writeJSONError(w, http.StatusBadRequest, "admin_error", "供应商不存在")
		return
	}
	base, key := p.BaseURL, p.APIKey
	cfgMu.RUnlock()

	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", err.Error())
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if isGo(base) {
		req.Header.Set("x-opencode-session", sessionID)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "admin_error", "拉取模型列表失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "admin_error", "读取上游响应失败: "+err.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeJSONError(w, http.StatusBadGateway, "admin_error",
			fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, snippet(string(data))))
		return
	}
	var upstream struct {
		Data []struct {
			ID                  string   `json:"id"`
			ContextLength       int      `json:"context_length"`
			MaxCompletionTokens int      `json:"max_completion_tokens"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &upstream); err != nil {
		writeJSONError(w, http.StatusBadGateway, "admin_error", "上游不是 OpenAI 格式的模型列表: "+snippet(string(data)))
		return
	}
	out := make([]fetchedModel, 0, len(upstream.Data))
	for _, m := range upstream.Data {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		thinking := false
		for _, param := range m.SupportedParameters {
			if param == "reasoning" || param == "thinking" {
				thinking = true
				break
			}
		}
		out = append(out, fetchedModel{Model: Model{ID: m.ID, Context: m.ContextLength, MaxOutput: m.MaxCompletionTokens, Thinking: thinking}})
	}
	// 元数据取不到不影响导入模型名，界面如实标注原因
	source := ""
	catalog, note, metaErr := devCatalog()
	if metaErr != nil {
		source = metaErr.Error()
	} else {
		source = applyDevMetadata(out, base, catalog) + " · " + note
	}
	if isGo(base) {
		source += "；" + applyMonthlyLimits(out)
	}
	logf("自动获取 %s -> %d 个模型（%s）", base, len(out), source)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"models": out, "source": source})
}

// applyMonthlyLimits 给 Go 的模型补月度额度, 取不到就把原因写进来源说明
func applyMonthlyLimits(models []fetchedModel) string {
	limits, note, err := goMonthlyLimits()
	if err != nil {
		logf("取 OpenCode Go 月度额度失败: %v", err)
		return "月度额度取不到: " + err.Error()
	}
	matched := 0
	for i := range models {
		if v, ok := limits[strings.ToLower(models[i].ID)]; ok {
			models[i].Monthly = v
			matched++
		}
	}
	return fmt.Sprintf("月度额度 %d/%d（%s）", matched, len(models), note)
}

// importModels 把自动获取勾选的模型批量落进供应商, 已存在的跳过
func (a *adminAPI) importModels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProviderID string  `json:"provider_id"`
		Models     []Model `json:"models"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	if len(in.Models) == 0 {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "没有要导入的模型")
		return
	}
	imported, skipped := 0, 0
	err := update(func(c *Config) error {
		p := findProvider(c, in.ProviderID)
		if p == nil {
			return fmt.Errorf("供应商不存在: %s", in.ProviderID)
		}
		for _, m := range in.Models {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue
			}
			dup := false
			for _, has := range p.Models {
				if has.ID == id {
					dup = true
					break
				}
			}
			if dup {
				skipped++
				continue
			}
			p.Models = append(p.Models, Model{ID: id, Context: m.Context, MaxOutput: m.MaxOutput, Thinking: m.Thinking})
			imported++
		}
		if imported == 0 {
			return fmt.Errorf("所选模型全部已存在，未导入")
		}
		return nil
	})
	if err != nil {
		writeResult(w, err, "")
		return
	}
	logf("导入模型 %d 个（跳过已存在 %d 个）到供应商 %s", imported, skipped, in.ProviderID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}

// usage 按日期区间汇总转发记录, 供用量统计页画图和列表
func (a *adminAPI) usage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", "请求解析失败: "+err.Error())
		return
	}
	report, err := usageSummary(in.From, in.To)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "admin_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(report)
}

func findProvider(c *Config, id string) *Provider {
	for _, p := range c.Providers {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func clearDefault(p *Provider) {
	for i := range p.Models {
		p.Models[i].Default = false
	}
}

func parseSize(raw, label string) (int, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 0, nil
	}
	multiplier := 1
	if strings.HasSuffix(s, "k") {
		multiplier, s = 1000, strings.TrimSuffix(s, "k")
	} else if strings.HasSuffix(s, "m") {
		multiplier, s = 1000000, strings.TrimSuffix(s, "m")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%s 要填整数 token 数，例如 128000 或 128k", label)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s 不能是负数", label)
	}
	return n * multiplier, nil
}

func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "********"
	}
	return key[:4] + "********" + key[len(key)-4:]
}

func snippet(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func writeResult(w http.ResponseWriter, err error, okMsg string) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		logf("管理接口失败: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": okMsg})
}
