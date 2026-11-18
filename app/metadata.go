package main

// 模型元数据: 上游 /models 通常只给 id, 上下文/输出/思考从 models.dev 的公开目录取。
// 匹配口径: 先按供应商接口地址精确对上 models.dev 的 provider.api, 对不上时只在
// 各档取值完全一致的情况下采用, 不一致就留空让用户自己填——宁缺毋滥。
// OpenCode Go 另有一个公开的额度页, 每个模型的月度额度从那里取。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	devCatalogURL  = "https://models.dev/api.json"
	devCatalogTTL  = 7 * 24 * time.Hour
	devCatalogFile = "models.dev.json"

	goConsoleURL   = "https://opencode.ai/console/go"
	goConsoleTTL   = 24 * time.Hour
	goConsoleFile  = "opencode-go-console.html"
	goUnlimited    = -1 // 页面用 ∞ 表示不限额
	goConsoleLimit = 8 << 20
)

// fetchedModel 是「自动获取」返回给界面的模型: 配置字段 + 只在当次展示用的月度额度
type fetchedModel struct {
	Model
	Monthly int `json:"monthly,omitempty"`
}

type devModel struct {
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Reasoning bool `json:"reasoning"`
}

type devProvider struct {
	API    string              `json:"api"`
	Models map[string]devModel `json:"models"`
}

// devCatalog 返回目录与来源说明，说明会显示在管理界面上
func devCatalog() (map[string]devProvider, string, error) {
	path := filepath.Join(configDir(), devCatalogFile)
	fi, statErr := os.Stat(path)
	note := "models.dev"

	if statErr != nil || time.Since(fi.ModTime()) >= devCatalogTTL {
		data, err := httpGet(devCatalogURL, 60*time.Second, 32<<20)
		if err != nil && statErr != nil {
			return nil, "", fmt.Errorf("模型元数据源不可达: %w", err)
		}
		if err != nil {
			note = "models.dev（源不可达，沿用 " + fi.ModTime().Format("11-02 15:04") + " 的缓存）"
			logf("刷新模型元数据失败，沿用缓存: %v", err)
		} else if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, "", fmt.Errorf("写入元数据缓存失败: %w", err)
		} else {
			logf("已更新模型元数据缓存 %s（%d 字节）", path, len(data))
			fi, statErr = os.Stat(path)
		}
	}
	if statErr != nil {
		return nil, "", fmt.Errorf("元数据缓存不可用: %w", statErr)
	}
	note += " · " + fi.ModTime().Format("01-02 15:04")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("读取元数据缓存失败: %w", err)
	}
	var catalog map[string]devProvider
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, "", fmt.Errorf("元数据缓存格式错误: %w", err)
	}
	return catalog, note, nil
}

// applyDevMetadata 就地补全模型的上下文/输出/思考，返回匹配口径
func applyDevMetadata(models []fetchedModel, baseURL string, catalog map[string]devProvider) string {
	trimmed := strings.TrimRight(baseURL, "/")
	for key, p := range catalog {
		if strings.TrimRight(p.API, "/") != trimmed {
			continue
		}
		for i := range models {
			if dm, ok := p.Models[models[i].ID]; ok {
				models[i].Context, models[i].MaxOutput, models[i].Thinking = dm.Limit.Context, dm.Limit.Output, dm.Reasoning
			}
		}
		return "接口地址对上 " + key
	}

	matched := 0
	for i := range models {
		var found *devModel
		ambiguous := false
		for _, p := range catalog {
			dm, ok := p.Models[models[i].ID]
			if !ok {
				continue
			}
			if found != nil && *found != dm {
				ambiguous = true
				break
			}
			cp := dm
			found = &cp
		}
		if found != nil && !ambiguous {
			models[i].Context, models[i].MaxOutput, models[i].Thinking = found.Limit.Context, found.Limit.Output, found.Reasoning
			matched++
		}
	}
	return fmt.Sprintf("按模型名匹配，%d/%d 取到一致数值（其余留空）", matched, len(models))
}

func httpGet(rawURL string, timeout time.Duration, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %d", rawURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

var (
	goRowRe    = regexp.MustCompile(`(?s)<tr[^>]*data-model="([^"]+)"[^>]*>(.*?)</tr>`)
	goUsageRe  = regexp.MustCompile(`(?s)data-allowance-type="usage".*?<span class="sr-only">([^<]*)</span>`)
	goTierRe   = regexp.MustCompile(`Go:\s*([^;]+);`)
	goNumClean = strings.NewReplacer("$", "", ",", "", " ", "")
)

// goMonthlyLimits 从 OpenCode Go 官网额度页取每个模型的月度额度（Go 档），键为小写模型 ID。
// 页面写的是美元额度，∞ 记为 goUnlimited。
func goMonthlyLimits() (map[string]int, string, error) {
	path := filepath.Join(configDir(), goConsoleFile)
	fi, statErr := os.Stat(path)
	note := "opencode.ai/console/go"

	if statErr != nil || time.Since(fi.ModTime()) >= goConsoleTTL {
		data, err := httpGet(goConsoleURL, 30*time.Second, goConsoleLimit)
		if err != nil && statErr != nil {
			return nil, "", fmt.Errorf("额度页面不可达: %w", err)
		}
		if err != nil {
			note = "额度页面（源不可达，沿用 " + fi.ModTime().Format("11-02 15:04") + " 的缓存）"
			logf("刷新 OpenCode Go 额度页失败，沿用缓存: %v", err)
		} else if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, "", fmt.Errorf("写入额度页缓存失败: %w", err)
		} else {
			logf("已更新 OpenCode Go 额度页缓存 %s（%d 字节）", path, len(data))
			fi, statErr = os.Stat(path)
		}
	}
	if statErr != nil {
		return nil, "", fmt.Errorf("额度页缓存不可用: %w", statErr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("读取额度页缓存失败: %w", err)
	}

	limits := map[string]int{}
	for _, row := range goRowRe.FindAllStringSubmatch(string(data), -1) {
		usage := goUsageRe.FindStringSubmatch(row[2])
		if usage == nil {
			continue
		}
		tier := goTierRe.FindStringSubmatch(usage[1])
		if tier == nil {
			continue
		}
		if v, ok := parseAllowance(tier[1]); ok {
			limits[strings.ToLower(row[1])] = v
		}
	}
	if len(limits) == 0 {
		return nil, "", errors.New("额度页没有解析到任何模型额度，可能页面结构已变")
	}
	note += " · " + fi.ModTime().Format("01-02 15:04")
	return limits, note, nil
}

func parseAllowance(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if s == "∞" {
		return goUnlimited, true
	}
	n, err := strconv.Atoi(goNumClean.Replace(s))
	if err != nil {
		return 0, false
	}
	return n, true
}
