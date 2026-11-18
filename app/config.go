package main

// 配置层: 供应商(URL + Key) 与其下的多个模型(上下文/输出/思考), 落库 %LOCALAPPDATA%\OpenCodeGoProxy\config.json

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// GoBaseURL 是 OpenCode Go 的 OpenAI 兼容入口, 界面与后端都用它识别「这是 Go」
const GoBaseURL = "https://opencode.ai/inference/go/openai/v1"

type Model struct {
	ID        string `json:"id"`
	Context   int    `json:"context,omitempty"`
	MaxOutput int    `json:"max_output,omitempty"`
	Thinking  bool   `json:"thinking,omitempty"`
	Default   bool   `json:"default,omitempty"`
}

type Provider struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	BaseURL string  `json:"base_url"`
	APIKey  string  `json:"api_key"`
	Models  []Model `json:"models"`
}

type Config struct {
	DefaultProvider string      `json:"default_provider"`
	Providers       []*Provider `json:"providers"`
}

var (
	cfgMu      sync.RWMutex
	cfg        = &Config{}
	cfgPath    string
	cfgLoadErr string
)

func seedConfig() *Config {
	return &Config{
		DefaultProvider: "opencode-go",
		Providers: []*Provider{{
			ID:      "opencode-go",
			Name:    "OpenCode Go",
			BaseURL: GoBaseURL,
		}},
	}
}

// isGo 判断接口地址是不是 OpenCode Go: 命中就要补会话头, 并去官网取月度额度
func isGo(baseURL string) bool {
	return strings.EqualFold(strings.TrimRight(baseURL, "/"), GoBaseURL)
}

func configDir() string {
	// %LOCALAPPDATA%\OpenCodeGoProxy: 与其它版本同目录, 升级后配置不会「丢失」
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenCodeGoProxy")
}

func loadConfig() {
	cfgPath = filepath.Join(configDir(), "config.json")
	cfgMu.Lock()
	defer cfgMu.Unlock()
	data, err := os.ReadFile(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		cfg = seedConfig()
		if err := persistConfig(); err != nil {
			cfgLoadErr = err.Error()
		}
		return
	}
	if err != nil {
		cfgLoadErr = "读取配置文件失败: " + err.Error()
		cfg = seedConfig()
		logf("%s", cfgLoadErr)
		return
	}
	var loaded Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		cfgLoadErr = "配置文件格式错误: " + err.Error() + "（文件未改动，可手工修正后点重启服务）"
		cfg = seedConfig()
		logf("%s", cfgLoadErr)
		return
	}
	cfg = &loaded
}

// persistConfig 在持有写锁时原子落盘
func persistConfig() error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func saveConfig() error {
	if err := persistConfig(); err != nil {
		logf("配置保存失败: %v", err)
		return fmt.Errorf("配置写入 %s 失败: %w", cfgPath, err)
	}
	cfgLoadErr = ""
	return nil
}

// update 以写锁执行 fn, fn 返回错误则不落盘
func update(fn func(*Config) error) error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if err := fn(cfg); err != nil {
		return err
	}
	return saveConfig()
}

func snapshot() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	out := Config{DefaultProvider: cfg.DefaultProvider, Providers: make([]*Provider, 0, len(cfg.Providers))}
	for _, p := range cfg.Providers {
		cp := *p
		cp.Models = append([]Model(nil), p.Models...)
		out.Providers = append(out.Providers, &cp)
	}
	return out
}

func newID() string {
	var b [6]byte
	rand.Read(b[:])
	return "p_" + hex.EncodeToString(b[:])
}

// providerByID 取供应商, id 为空表示取默认供应商（未设默认时用第一个已配置的）
func (c *Config) providerByID(id string) *Provider {
	if id == "" {
		id = c.DefaultProvider
	}
	for _, p := range c.Providers {
		if p.ID == id {
			return p
		}
	}
	if id == "" && len(c.Providers) > 0 {
		return c.Providers[0]
	}
	return nil
}

// findModel 在所有供应商的模型里按 ID 匹配, 返回其下标; 未命中返回 (-1, nil)
func (c *Config) findModel(modelID string) (*Provider, int) {
	if modelID == "" {
		return nil, -1
	}
	for _, p := range c.Providers {
		for i := range p.Models {
			if strings.EqualFold(p.Models[i].ID, modelID) {
				return p, i
			}
		}
	}
	return nil, -1
}

// defaultRoute 取默认供应商及其默认模型下标; 未设默认模型时返回 -1, 表示沿用客户端传来的模型名
func (c *Config) defaultRoute() (*Provider, int) {
	p := c.providerByID("")
	if p == nil {
		return nil, -1
	}
	for i := range p.Models {
		if p.Models[i].Default {
			return p, i
		}
	}
	return p, -1
}

// normalizeBaseURL 接受用户粘贴的各种写法, 归一成 https://host/.../v1 形式
func normalizeBaseURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", errors.New("接口地址不能为空")
	}
	if !strings.Contains(u, "://") {
		// 本机回环只跑明文服务，补 https 会直接连不上
		if strings.HasPrefix(u, "127.0.0.1") || strings.HasPrefix(u, "localhost") || strings.HasPrefix(u, "[::1]") {
			u = "http://" + u
		} else {
			u = "https://" + u
		}
	}
	u = strings.TrimRight(u, "/")
	parsed, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", errors.New("接口地址缺少域名")
	}
	return u, nil
}
