package main

// OpenCode Go 代理托盘工具
// 背景: OpenCode Go 网关强制要求 x-opencode-session 头, Qoder CN 的自定义模型无法配置请求头,
//      因此在本机起一个网关, 统一补上该头并按「模型管理」里的配置选择供应商/模型/密钥。
// 用法: 运行后托盘出现图标, 在 Qoder 的 Base URL 填 http://127.0.0.1:8787/v1

import (
	_ "embed"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	user32            = windows.NewLazySystemDLL("user32.dll")
	procMessageBox    = user32.NewProc("MessageBoxW")
	procEnumWindows   = user32.NewProc("EnumWindows")
	procIsWindowVis   = user32.NewProc("IsWindowVisible")
	procGetWindowText = user32.NewProc("GetWindowTextW")
	procShowWindow    = user32.NewProc("ShowWindow")
)

// 管理窗口的标题: 页面加载完是「模型管理 · OpenCode Go 代理」，加载前是地址本身。
// 普通标签页窗口尾巴上还会跟「 - Microsoft Edge」，不会误伤。
const adminTitleEnd = "· OpenCode Go 代理"

var (
	enumMu      sync.Mutex
	enumHandles []uintptr
	// 枚举窗口用的回调: 收下标题是我们的管理页（加载前按地址、加载后按标题尾巴）的可见窗口
	enumCallback = syscall.NewCallback(func(h uintptr, lParam uintptr) uintptr {
		vis, _, _ := procIsWindowVis.Call(h)
		if vis == 0 {
			return 1
		}
		buf := make([]uint16, 512)
		n, _, _ := procGetWindowText.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 {
			return 1
		}
		t := windows.UTF16ToString(buf[:n])
		if strings.HasSuffix(t, adminTitleEnd) || strings.Contains(t, adminHostPort()) {
			enumMu.Lock()
			enumHandles = append(enumHandles, h)
			enumMu.Unlock()
		}
		return 1
	})
)

// adminHostPort 是加载前窗口标题里的地址片段，如 127.0.0.1:8787
func adminHostPort() string {
	return "127.0.0.1:" + strconv.Itoa(int(currentPort.Load()))
}

// adminWindowHandles 列出当前已打开的管理窗口句柄
func adminWindowHandles() []uintptr {
	enumMu.Lock()
	enumHandles = nil
	enumMu.Unlock()
	procEnumWindows.Call(enumCallback, 0)
	enumMu.Lock()
	defer enumMu.Unlock()
	return append([]uintptr(nil), enumHandles...)
}

// maximizeAdminWindow 一发现管理窗口就最大化，并在头两秒里持续压制。
// --start-maximized 只在浏览器进程冷启动时管用，Edge 已经在跑就会被忽略；
// 而且应用窗口在页面加载完会按自己记住的位置尺寸重摆一次，只调一次会被盖掉。
func maximizeAdminWindow() {
	deadline := time.Now().Add(12 * time.Second)
	var asserted time.Duration
	for time.Now().Before(deadline) && asserted < 2*time.Second {
		time.Sleep(100 * time.Millisecond)
		hs := adminWindowHandles()
		if len(hs) == 0 {
			continue
		}
		for _, h := range hs {
			procShowWindow.Call(h, 3) // SW_SHOWMAXIMIZED
		}
		asserted += time.Duration(100 * time.Millisecond)
	}
}

func main() {
	// 单实例: 托盘工具重复运行没有意义, 直接提示已存在的实例
	name, _ := windows.UTF16PtrFromString(mutexName)
	if _, err := windows.CreateMutex(nil, false, name); err == windows.ERROR_ALREADY_EXISTS {
		messageBox("OpenCode Go 代理已经在运行，请查看屏幕右下角托盘图标。")
		return
	}
	// 配置与缓存都放在当前用户的 LOCALAPPDATA, 取不到就别静默写到别处
	if os.Getenv("LOCALAPPDATA") == "" {
		messageBox("当前环境没有 LOCALAPPDATA，无法保存配置和日志。")
		return
	}

	initLog()
	loadConfig()
	initUsage()
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconRed)
	systray.SetTooltip(appTitle + " · 启动中…")
	systray.SetTitle("")

	statusItem = systray.AddMenuItem("状态：启动中…", "")
	statusItem.Disable()
	systray.AddSeparator()
	mModels := systray.AddMenuItem("模型管理", "打开供应商与模型管理窗口")
	mUsage := systray.AddMenuItem("用量统计", "打开按天/供应商/模型的用量统计窗口")
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
		for range mModels.ClickedCh {
			openAdminWindow(adminURL())
		}
	}()

	go func() {
		for range mUsage.ClickedCh {
			openAdminWindow(adminURL() + "#/usage")
		}
	}()

	go func() {
		for range mCopy.ClickedCh {
			if err := setClipboard(clientBaseURL()); err == nil {
				systray.SetTooltip("已复制：" + clientBaseURL())
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
			loadConfig()
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
	closeUsage()
	closeLog()
}

func markRunning() {
	systray.SetIcon(iconGreen)
	systray.SetTooltip(appTitle + " · 运行中 " + clientBaseURL())
	statusItem.SetTitle("状态：运行中（127.0.0.1:" + strconv.Itoa(int(currentPort.Load())) + "）")
}

func clientBaseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(int(currentPort.Load())) + "/v1"
}

func adminURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(int(currentPort.Load())) + "/_admin"
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
			Handler:  newGateway(),
			ErrorLog: log.New(io.Discard, "", 0),
		}
		server = srv
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				logf("服务退出: %v", err)
			}
		}()
		logf("网关服务已启动: %s（管理页 %s）", clientBaseURL(), adminURL())
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

func helpText() string {
	return appTitle + " 使用说明\n\n" +
		"1. 托盘图标绿色 = 网关正常，红色 = 未启动。\n\n" +
		"2. 右键托盘 →「模型管理」，添加供应商（接口地址 + API Key）。\n" +
		"   本工具不内置任何 Key；供应商 API Key 留空时，直接用客户端带来的 Key。\n\n" +
		"3. 打开某个供应商的模型页会自动从上游拉取模型：\n" +
		"   接口地址是 " + GoBaseURL + " 时，自动补 Go 需要的会话头，\n" +
		"   并把官网每个模型的月度额度从高到低排在前面，勾选后导入，再设一个默认模型。\n\n" +
		"4. 在 Qoder CN 中打开：设置 → 模型 → 添加自定义模型，协议选 OpenAI 兼容。\n" +
		"   Base URL 填：\n   " + clientBaseURL() + "\n" +
		"   API Key 与模型 ID 随便填，实际调用由本工具的默认供应商/默认模型决定；\n" +
		"   模型 ID 若与「模型管理」里某个模型同名，则直接走那个模型。\n\n" +
		"5. 托盘「用量统计」看每天的用量：按天/供应商/模型统计请求数与 token，\n" +
		"   默认最近一个月，可以自己选时间范围；记录在\n   " + filepath.Join(configDir(), "usage.jsonl") + "\n\n" +
		"6. 配置即时生效，不用重启；配置文件在\n   " + filepath.Join(configDir(), "config.json") + "\n\n" +
		"提示：老地址 " + "http://127.0.0.1:" + strconv.Itoa(int(currentPort.Load())) + "/inference/go/openai/v1 仍然可用。"
}

func initLog() {
	dir := configDir()
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

// openAdminWindow 用系统自带的 Edge/Chrome 应用窗口打开管理界面:
// 独立窗口、没有地址栏和标签页，看起来就是个 Windows 程序，且不需要额外依赖。
func openAdminWindow(u string) {
	roots := []string{
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("ProgramFiles"),
		os.Getenv("LOCALAPPDATA"),
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, name := range []string{
			filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
		} {
			if _, err := os.Stat(name); err != nil {
				continue
			}
			cmd := exec.Command(name, "--app="+u, "--start-maximized")
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
			if err := cmd.Start(); err != nil {
				logf("以应用窗口打开管理页失败: %v", err)
				continue
			}
			logf("管理窗口已打开: %s", u)
			go maximizeAdminWindow()
			return
		}
	}
	logf("未找到 Edge/Chrome，用默认浏览器打开管理页")
	openInDefaultBrowser(u)
}

func openInDefaultBrowser(u string) {
	cmd := exec.Command("cmd", "/c", "start", "", u)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Start(); err != nil {
		logf("打开管理页失败: %v", err)
		messageBox("无法打开浏览器，请手动访问：" + u)
	}
}
