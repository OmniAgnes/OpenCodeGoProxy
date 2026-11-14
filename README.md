# OpenCodeGoProxy

让 Qoder CN 能用上 OpenCode Go 套餐模型的 Windows 托盘小工具。

## 背景

OpenCode Go 网关现在要求每个请求都带 `x-opencode-session` 请求头，否则直接返回：

```
HTTP 400 {"type":"error","error":{"type":"MissingSessionID",
"message":"Request is missing x-opencode-session and cannot be routed efficiently..."}}
```

而 Qoder CN 的「自定义模型」只支持填 Base URL、API Key、Model ID，没有自定义请求头的地方。

这个工具在本机 `127.0.0.1` 起一个转发服务，替 Qoder 补上该请求头后再转发到 `opencode.ai`，其它内容原样透传。

## 功能

- 单文件 exe，不依赖 Node / Python / .NET 运行时
- 托盘图标显示状态：绿色=运行中，红色=未启动或启动失败
- 右键菜单：复制 Base URL、使用说明、打开日志、开机自启、重启服务、退出
- 端口自动选择：`8787`–`8797` 中第一个空闲端口，复制的地址会自动带上实际端口
- 会话 ID 按「会话首条用户消息」哈希生成，同一会话稳定、不同会话隔离，便于上游做路由和提示缓存
- 日志写在 `%LOCALAPPDATA%\OpenCodeGoProxy\proxy.log`，超过 5MB 自动转存

## 使用

1. 下载并双击 `OpenCodeGoProxy.exe`，托盘出现绿色图标（首次运行会自动开启开机自启）。
2. 右键托盘图标 → **复制 Base URL**，得到：

   ```
   http://127.0.0.1:8787/inference/go/openai/v1
   ```

3. 打开 Qoder CN：设置 → 模型 → 添加自定义模型，协议选 **OpenAI 兼容 / Chat Completions**。
4. Base URL 粘贴上一步的地址；API Key 填你自己的 OpenCode Go 密钥（`oc_sk_` 开头）；模型 ID 填 Go 套餐里的模型，例如 `deepseek-v4.1-flash`。
5. 保存后即可对话。Qoder 保存时会 `GET` 一次 Base URL 做校验，代理会把它转到 `/models` 返回 200。

## 自己编译

```powershell
cd app
go build -trimpath -ldflags "-H windowsgui -s -w" -o ..\OpenCodeGoProxy.exe .
```

需要 Go 1.21+。仓库里的 GitHub Actions（`.github/workflows/build.yml`）会在推送到 `main` 时构建并上传 artifact，打 `v*` 标签时自动发 Release。

## 工作方式

```
Qoder CN ──http──> 127.0.0.1:8787 ──https──> opencode.ai/inference/go
                        │
                        └─ 补 x-opencode-session、设置 User-Agent
```

- 只监听 `127.0.0.1`，局域网和其它机器连不进来
- 不存储、不读取 API Key：`Authorization` 头原样透传
- 日志只记录方法、路径、状态码，不记录请求体与对话内容
- 本地回环为明文但不出网卡，上游为 HTTPS

## 常见问题

**Q：重启后会失效吗？**
托盘菜单的「开机自启」默认开启（写入 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`），重启后自动运行。

**Q：Qoder 升级后不能用了？**
Qoder 可能把 Base URL 重置回 `https://opencode.ai/inference/go/openai/v1`，回到设置里改回代理地址即可。

**Q：端口被占用？**
程序会自动换到 `8788`–`8797`，以托盘「复制 Base URL」给出的地址为准。

**Q：首次运行被 SmartScreen 拦截？**
exe 未做代码签名，点「更多信息 → 仍要运行」即可。

**Q：和 Clash / FlClash 冲突吗？**
不冲突。只占本机 `8787` 端口；出站流量会照常走 TUN 或系统代理。

## 目录结构

```
.
├─ OpenCodeGoProxy.exe        # 构建产物（不纳入版本管理）
├─ app/
│  ├─ main.go                 # 托盘 + 转发服务
│  ├─ green.ico / red.ico     # 托盘图标
│  ├─ go.mod / go.sum
└─ .github/workflows/build.yml
```
