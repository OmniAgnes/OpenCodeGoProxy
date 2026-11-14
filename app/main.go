package main

// OpenCode Go 代理托盘工具
// 背景: OpenCode Go 网关强制要求 x-opencode-session 头, Qoder CN 的自定义模型无法配置请求头,
//      因此在本机起一个转发服务, 统一补上该头, 让 Qoder 能正常调用 Go 套餐模型。
// 用法: 运行后托盘出现图标, 在 Qoder 的 Base URL 填 http://127.0.0.1:8787/inference/go/openai/v1

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed green.ico
var iconGreen []byte

//go:embed red.ico
var iconRed []byte

const (
	appTitle   = "OpenCode Go 代理"
	basePort   = 8787
	maxPort    = 8797
	sessionID  = "ses_qoder_cn"
	userAgent  = "qoder-cn-proxy/1.0"
	mutexName  = `Local\OpenCodeGoProxyTray`
	runKey     = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue   = "OpenCodeGoProxy"
	stateKey   = `Software\OpenCodeGoProxy`
	stateValue = "Installed"
	logMaxSize = 5 << 20
)

var (
	apiRootRe   = regexp.MustCompile(`^/inference/go/openai/v1/?$`)
	currentPort atomic.Int32
	serverMu    sync.Mutex
	server      *http.Server
	logMu       sync.Mutex
	logFile     *os.File
	logPath     string

	statusItem *systray.MenuItem
	autoItem   *systray.MenuItem
)

var (
	user32         = windows.NewLazySystemDLL("user32.dll")
	procMessageBox = user32.NewProc("MessageBoxW")
)

func main() {
	// 单实例: 托盘工具重复运行没有意义, 直接提示已存在的实例
	name, _ := windows.UTF16PtrFromString(mutexName)
	if _, err := windows.CreateMutex(nil, false, name); err == windows.ERROR_ALREADY_EXISTS {
		messageBox("OpenCode Go 代理已经在运行，请查看屏幕右下角托盘图标。")
		return
	}

	initLog()
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconRed)
	systray.SetTooltip(appTitle + " · 启动中…")
	systray.SetTitle("")

	statusItem = systray.AddMenuItem("状态：启动中…", "")
	statusItem.Disable()
	systray.AddSeparator()
	mCopy := systray.AddMenuItem("复制 Base URL", "复制后粘贴到 Qoder 的自定义模型")
	mHelp := systray.AddMenuItem("使用说明", "")
	mLog := systray.AddMenuItem("打开日志", "")
	systray.AddSeparator()
	autoItem = systray.AddMenuItemCheckbox("开机自启", "", autostartEnabled())
	systray.AddSeparator()
	mRestart := systray.AddMenuItem("重启服务", "")
	mQuit := systray.AddMenuItem("退出", "")

	if isFirstRun() {
		setAutostart(true)
		markInstalled()
		autoItem.Check()
	}

	if err := startServer(); err != nil {
		logf("启动失败: %v", err)
		systray.SetIcon(iconRed)
		systray.SetTooltip(appTitle + " · 启动失败")
		statusItem.SetTitle("状态：启动失败（" + err.Error() + "）")
		messageBox("转发服务启动失败：" + err.Error() + "\n\n常见原因：端口 " + strconv.Itoa(basePort) + "-" + strconv.Itoa(maxPort) + " 被其他程序占用。")
	} else {
		markRunning()
	}

	go func() {
		for range mCopy.ClickedCh {
			if err := setClipboard(baseURL()); err == nil {
				systray.SetTooltip("已复制：" + baseURL())
				time.AfterFunc(3*time.Second, func() { systray.SetTooltip(appTitle + " · 运行中") })
			}
		}
	}()

	go func() {
		for range mHelp.ClickedCh {
			messageBox(helpText())
		}
	}()

	go func() {
		for range mLog.ClickedCh {
			exec.Command("notepad.exe", logPath).Start()
		}
	}()

	go func() {
		for range autoItem.ClickedCh {
			if autoItem.Checked() {
				autoItem.Uncheck()
				setAutostart(false)
			} else {
				autoItem.Check()
				setAutostart(true)
			}
		}
	}()

	go func() {
		for range mRestart.ClickedCh {
			logf("手动重启服务")
			stopServer()
			if err := startServer(); err != nil {
				systray.SetIcon(iconRed)
				systray.SetTooltip(appTitle + " · 启动失败")
				statusItem.SetTitle("状态：启动失败（" + err.Error() + "）")
			} else {
				markRunning()
			}
		}
	}()

	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()
}

func onExit() {
	stopServer()
	closeLog()
}

func markRunning() {
	systray.SetIcon(iconGreen)
	systray.SetTooltip(appTitle + " · 运行中 " + baseURL())
	statusItem.SetTitle("状态：运行中（127.0.0.1:" + strconv.Itoa(int(currentPort.Load())) + "）")
}

func baseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(int(currentPort.Load())) + "/inference/go/openai/v1"
}

func startServer() error {
	serverMu.Lock()
	defer serverMu.Unlock()
	for port := basePort; port <= maxPort; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		currentPort.Store(int32(port))
		srv := &http.Server{
			Handler:  newProxy(),
			ErrorLog: log.New(io.Discard, "", 0),
		}
		server = srv
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				logf("服务退出: %v", err)
			}
		}()
		logf("转发服务已启动: %s", baseURL())
		return nil
	}
	return fmt.Errorf("端口 %d-%d 均被占用", basePort, maxPort)
}

func stopServer() {
	serverMu.Lock()
	defer serverMu.Unlock()
	if server != nil {
		server.Close()
		server = nil
	}
}

func newProxy() http.Handler {
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = "opencode.ai"
			req.Host = "opencode.ai"
			req.Header.Set("User-Agent", userAgent)
		},
		ModifyResponse: func(resp *http.Response) error {
			logf("%s %s -> %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
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
		// Qoder 保存模型时会 GET Base URL 根路径做校验, 上游无此路由, 转成模型列表
		if r.Method == http.MethodGet && apiRootRe.MatchString(r.URL.Path) {
			r.URL.Path = "/inference/go/openai/v1/models"
		}
		if r.Header.Get("x-opencode-session") == "" {
			r.Header.Set("x-opencode-session", sessionFor(r))
		}
		rp.ServeHTTP(w, r)
	})
}

// sessionFor 按“会话首条用户消息”派生稳定的会话 ID:
// 同一会话多轮请求哈希一致, 不同会话互不干扰, 便于上游做路由与提示缓存。
func sessionFor(r *http.Request) string {
	if r.Body == nil {
		return sessionID
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		r.Body = http.NoBody
		return sessionID
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

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

func helpText() string {
	return appTitle + " 使用说明\n\n" +
		"1. 托盘图标绿色 = 转发服务正常，红色 = 未启动。\n\n" +
		"2. 在 Qoder CN 中打开：设置 → 模型 → 添加自定义模型，\n" +
		"   协议选 OpenAI 兼容 / Chat Completions。\n\n" +
		"3. Base URL 填：\n   " + baseURL() + "\n\n" +
		"4. API Key 填你自己的 OpenCode Go 密钥（oc_sk_ 开头）。\n\n" +
		"5. 模型 ID 填 Go 套餐里的模型，例如 deepseek-v4.1-flash。\n\n" +
		"提示：右键托盘图标可复制 Base URL、查看日志、开关开机自启。"
}

func initLog() {
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenCodeGoProxy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	logPath = filepath.Join(dir, "proxy.log")
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > logMaxSize {
		os.Rename(logPath, filepath.Join(dir, "proxy.old.log"))
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	logFile = f
	logf("---- 启动 %s ----", appTitle)
}

func closeLog() {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}

func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile == nil {
		return
	}
	fmt.Fprintf(logFile, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	return err == nil && v != ""
}

func setAutostart(on bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	if on {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		k.SetStringValue(runValue, `"`+exe+`"`)
	} else {
		k.DeleteValue(runValue)
	}
}

func isFirstRun() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, stateKey, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(stateValue)
	return err != nil || v == 0
}

func markInstalled() {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, stateKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetDWordValue(stateValue, 1)
}

func messageBox(text string) {
	caption, _ := windows.UTF16PtrFromString(appTitle)
	body, _ := windows.UTF16PtrFromString(text)
	procMessageBox.Call(0, uintptr(unsafe.Pointer(body)), uintptr(unsafe.Pointer(caption)), 0x00010040)
}

// setClipboard 通过系统自带的 clip 写入剪贴板, 避免 unsafe 操作全局内存
func setClipboard(text string) error {
	cmd := exec.Command("cmd", "/c", "clip")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
