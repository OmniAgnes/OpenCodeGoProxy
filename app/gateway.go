package main

// 网关: 客户端只连本机, 由配置决定真正的供应商/模型/密钥
// 规则: 客户端 model 命中已配置模型就用它, 未命中走默认供应商的默认模型;
//      供应商配了 Key 就覆盖 Authorization, 未配则透传客户端带来的 Key;
//      接口地址是 OpenCode Go 时补 x-opencode-session 头。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
)

type routeCtxKey struct{}

type route struct {
	provider *Provider
	base     *url.URL
	model    *Model // nil = 沿用客户端传来的模型名
	tail     string
	session  string
}

// chatTails 需要按配置改写请求体的推理入口
var chatTails = map[string]bool{
	"/chat/completions": true,
	"/completions":      true,
	"/responses":        true,
	"/messages":         true,
}

func newGateway() http.Handler {
	admin := newAdminHandler()
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			rt := req.Context().Value(routeCtxKey{}).(*route)
			req.URL.Scheme = rt.base.Scheme
			req.URL.Host = rt.base.Host
			req.URL.Path = path.Join(rt.base.Path, rt.tail)
			req.URL.RawPath = ""
			req.Host = rt.base.Host
			req.Header.Set("User-Agent", userAgent)
			if rt.provider.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+rt.provider.APIKey)
			}
			if rt.session != "" && req.Header.Get("x-opencode-session") == "" {
				req.Header.Set("x-opencode-session", rt.session)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			rt := resp.Request.Context().Value(routeCtxKey{}).(*route)
			target := rt.provider.Name
			if rt.model != nil {
				target += "/" + rt.model.ID
			}
			logf("%s %s -> %d [%s]", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, target)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logf("%s %s -> 转发失败: %v", r.Method, r.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":{"type":"proxy_error","message":%q}}`, err.Error())
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_admin" || strings.HasPrefix(r.URL.Path, "/_admin/") ||
			r.URL.Path == "/_api" || strings.HasPrefix(r.URL.Path, "/_api/") {
			admin.serve(w, r)
			return
		}
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/_admin", http.StatusFound)
			return
		}

		tail := clientTail(r.URL.Path)
		if tail == "/models" && r.Method == http.MethodGet {
			writeModelList(w)
			return
		}

		// 推理入口要读体改 model; 其余接口原样透传, 不碰请求体
		isChat := r.Method == http.MethodPost && chatTails[tail]
		var payload map[string]json.RawMessage
		if isChat {
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
				return
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "请求体必须是 JSON 对象")
				return
			}
			rt, err := buildRoute(tail, payload, body)
			if err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, "proxy_misconfigured", err.Error())
				return
			}
			outgoing, err := rt.rewrite(payload, body)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(outgoing))
			r.ContentLength = int64(len(outgoing))
			rp.ServeHTTP(w, r.Clone(contextWithRoute(r.Context(), rt)))
			return
		}

		rt, err := buildRoute(tail, nil, nil)
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "proxy_misconfigured", err.Error())
			return
		}
		rp.ServeHTTP(w, r.Clone(contextWithRoute(r.Context(), rt)))
	})
}

// buildRoute 复制出一份路由所需的供应商配置, 避免请求期间与界面编辑互相踩
func buildRoute(tail string, payload map[string]json.RawMessage, body []byte) (*route, error) {
	var clientModel string
	if raw, ok := payload["model"]; ok {
		json.Unmarshal(raw, &clientModel)
	}

	cfgMu.RLock()
	provider, index := cfg.findModel(clientModel)
	if provider == nil {
		provider, index = cfg.defaultRoute()
	}
	if provider == nil {
		cfgMu.RUnlock()
		return nil, fmt.Errorf("尚未配置供应商，请打开 http://127.0.0.1:%d/_admin 添加", currentPort.Load())
	}
	cp := *provider
	cp.Models = append([]Model(nil), provider.Models...)
	cfgMu.RUnlock()

	rt := &route{provider: &cp, tail: tail}
	if index >= 0 {
		rt.model = &cp.Models[index]
	}
	if isGo(cp.BaseURL) {
		rt.session = sessionFor(body)
	}
	base, err := url.Parse(cp.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("供应商 %q 的接口地址无法解析: %w", cp.Name, err)
	}
	rt.base = base
	return rt, nil
}

// rewrite 用配置值覆盖 model, 并把 token 上限收窄到模型配置值
func (rt *route) rewrite(payload map[string]json.RawMessage, raw []byte) ([]byte, error) {
	if rt.model == nil {
		return raw, nil
	}
	payload["model"], _ = json.Marshal(rt.model.ID)
	if limit := rt.model.MaxOutput; limit > 0 {
		for _, key := range []string{"max_tokens", "max_output_tokens"} {
			cur, ok := payload[key]
			if !ok {
				continue
			}
			n, err := json.Number(string(cur)).Int64()
			if err == nil && n > int64(limit) {
				payload[key], _ = json.Marshal(limit)
			}
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("请求体重写失败: %w", err)
	}
	return out, nil
}

func contextWithRoute(ctx context.Context, rt *route) context.Context {
	return context.WithValue(ctx, routeCtxKey{}, rt)
}

// clientTail 剥掉客户端可能带的任意版本前缀, 只留 /v1 之后的接口名
func clientTail(p string) string {
	if i := strings.LastIndex(p, "/v1"); i >= 0 {
		if rest := p[i+len("/v1"):]; rest != "" {
			return rest
		}
		return "/models"
	}
	if p == "" || p == "/" {
		return "/models"
	}
	return p
}

// writeModelList 对外公布本机管理的模型及上下文/输出/思考
func writeModelList(w http.ResponseWriter) {
	type entry struct {
		ID               string `json:"id"`
		Object           string `json:"object"`
		OwnedBy          string `json:"owned_by"`
		ContextLength    int    `json:"context_length,omitempty"`
		MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
		SupportsThinking bool   `json:"supports_thinking"`
		IsDefault        bool   `json:"is_default"`
	}
	c := snapshot()
	data := make([]entry, 0, 8)
	for _, p := range c.Providers {
		for _, m := range p.Models {
			data = append(data, entry{
				ID:               m.ID,
				Object:           "model",
				OwnedBy:          p.Name,
				ContextLength:    m.Context,
				MaxOutputTokens:  m.MaxOutput,
				SupportsThinking: m.Thinking,
				IsDefault:        m.Default && p.ID == c.DefaultProvider,
			})
		}
	}
	logf("GET /v1/models -> 200 [%d 个模型]", len(data))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// sessionFor 按“会话首条用户消息”派生稳定的会话 ID:
// 同一会话多轮请求哈希一致, 不同会话互不干扰, 便于上游做路由与提示缓存。
func sessionFor(body []byte) string {
	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return sessionID
	}
	for _, m := range payload.Messages {
		if m.Role == "user" {
			sum := sha256.Sum256(m.Content)
			return "ses_" + hex.EncodeToString(sum[:8])
		}
	}
	return sessionID
}

func writeJSONError(w http.ResponseWriter, status int, kind, msg string) {
	logf("%d %s: %s", status, kind, msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"type":%q,"message":%q}}`, kind, msg)
}
