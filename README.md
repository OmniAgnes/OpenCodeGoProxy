# OpenCodeGoProxy

让 Qoder CN 用上 OpenCode Go 套餐模型的 Windows 托盘小工具，顺便充当本机的多供应商模型网关。

## 背景

OpenCode Go 网关要求每个请求都带 `x-opencode-session` 请求头，否则直接返回：

```
HTTP 400 {"type":"error","error":{"type":"MissingSessionID",
"message":"Request is missing x-opencode-session and cannot be routed efficiently..."}}
```

而 Qoder CN 的「自定义模型」只支持填 Base URL、API Key、Model ID，没有自定义请求头的地方。

这个工具在本机 `127.0.0.1` 起一个转发服务：统一补上该请求头，并按「模型管理」里配好的供应商 / 模型 / 密钥转发到真正的上游。

## 功能

- 单文件 exe，不依赖 Node / Python / .NET 运行时，**不内置任何 API Key**
- 多供应商：每个供应商一条接口地址 + 一个 API Key + 多个模型
- 每个模型可配上下文、最大输出、是否支持思考，并可设为默认模型
- 模型页每次打开自动从上方的 `GET /models` 拉取上游模型，元数据取不到就留空，界面会写明来源
- 接口地址是 `https://opencode.ai/inference/go/openai/v1` 时自动识别为 OpenCode Go：补会话头，并取官网 <https://opencode.ai/console/go> 的每个模型**月度额度**，从高到低排在前面（`∞` 记作不限额）
- 客户端只要填本机地址，API Key 与模型名随便写：模型名命中已配置模型就走它，没命中走默认供应商的默认模型
- 用量统计：按天 / 供应商 / 模型汇总请求数与 token，默认最近一个月，可选任意时间范围（最长一年），图表是按供应商堆叠的每日柱状图
- 管理界面是浅色网页，由托盘用系统 Edge/Chrome 的应用窗口打开（没有地址栏和标签页），配置即时生效、不用重启
- 托盘图标：绿色=运行中，红色=未启动或启动失败；右键菜单含模型管理、用量统计、复制 Base URL、使用说明、打开日志、开机自启、重启服务、退出
- 端口自动选择：`8787`–`8797` 中第一个空闲端口，复制的地址自动带实际端口
- 会话 ID 按「会话首条用户消息」哈希生成，同一会话稳定、不同会话隔离，便于上游做路由和提示缓存
- 配置、缓存、日志都在 `%LOCALAPPDATA%\OpenCodeGoProxy\`：`config.json`（0600）、`usage.jsonl`（用量记录，0600）、`models.dev.json`（7 天）、`opencode-go-console.html`（1 天）、`proxy.log`（超过 5MB 转存）

## 使用

1. 下载并双击 `OpenCodeGoProxy.exe`，托盘出现绿色图标（首次运行会自动开启开机自启）。
2. 右键托盘 → **模型管理**，打开管理窗口。默认供应商已经是 OpenCode Go（接口地址填好、密钥留空）：
   - 要在别的工具里透传自己的密钥，就留空 API Key；想由代理统一覆盖，就把 Key 填进供应商。
   - 打开该供应商的「模型」，列表会自动从上游拉取，按月度额度从高到低排好；勾选后「导入所选」，或单行「添加」，再给常用的那个模型点「设为默认」。
   - 上下文 / 最大输出 / 思考来自 <https://models.dev>，对不上就留空，可以自己补。
3. 右键托盘 → **复制 Base URL**，得到：

   ```
   http://127.0.0.1:8787/v1
   ```

4. 打开 Qoder CN：设置 → 模型 → 添加自定义模型，协议选 **OpenAI 兼容 / Chat Completions**，Base URL 粘贴上一步地址，API Key 与模型 ID 随便填。
5. 保存后即可对话。Qoder 保存时会 `GET` 一次 Base URL 做校验，代理返回本机已配置模型的列表（含上下文/最大输出/是否支持思考）。
6. 想看用了多少：右键托盘 → **用量统计**，默认给最近一个月的按天图表与汇总，可切「最近 7 天 / 最近 90 天 / 本年」或自己填起止日期；指标可在请求数与 token 之间切换。

换别的供应商也一样：新增供应商填它自己的接口地址即可，只有 OpenCode Go 那个地址会被特殊对待。

## 用量统计

- 每转发一次客户端的推理请求（`POST`）就写一行 `usage.jsonl`：日期、供应商、模型、状态码、输入/输出 token
- token 从上游响应里扫 `usage` 得到，非流式和流式（`message_start` / `message_delta` 取各字段最大值）都支持；上游没回 `usage` 时只记请求数
- 只统计真正打到上游的请求：`GET`（favicon、模型列表校验等）与本机直接返回的接口都不计入
- 模型名记的是实际打出去的那个：客户端模型名命中已配置模型就记配置值，没命中就记客户端带来的名字
- 记录只在本机，重启不清空；删掉 `usage.jsonl` 就是从零开始统计

## 工作方式

```
Qoder CN ──http──> 127.0.0.1:8787 ──┬──https──> opencode.ai/inference/go（补 x-opencode-session）
                                    └──https──> 其它 OpenAI 兼容供应商
```

- 只监听 `127.0.0.1`，局域网和其它机器连不进来；`/_admin`、`/_api` 也只在本机
- API Key 只存在本机 `config.json`（0600），管理接口只回掩码，原始密钥不出这台机器
- 供应商没填 Key 时 `Authorization` 原样透传客户端带来的 Key
- 请求体只改 `model` 与 `max_tokens` / `max_output_tokens`（要得更多会被压到模型配置值），其余原样转发
- 日志只记录方法、路径、状态码与命中的供应商/模型，不记录请求体与对话内容

## 自己编译

```powershell
cd app
go build -trimpath -ldflags "-H windowsgui -s -w" -o ..\OpenCodeGoProxy.exe .
```

需要 Go 1.26+。仓库里的 GitHub Actions（`.github/workflows/build.yml`）会在推送到 `main` 时构建并上传 artifact，打 `v*` 标签时自动发 Release。

## 常见问题

**Q：别人装完能用吗？会不会带上我的密钥？**
不能凭空用，但不会被我的配置影响。exe 不含任何密钥；新机器上首次运行只会生成一个接口地址为 OpenCode Go、密钥留空的默认供应商，用自己的 OpenCode Go 密钥（Qoder 里填或供应商里填都行）即可。

**Q：月度额度是哪来的？**
`https://opencode.ai/console/go` 的公开页面，解析表格里每个模型 Go 档的月度额度，缓存 1 天；页面结构变了会直接报「额度页没有解析到任何模型额度」，不会瞎填。

**Q：重启后会失效吗？**
「开机自启」默认开启（写入 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`），重启后自动运行。

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
│  ├─ main.go                 # 托盘、开机自启、用应用窗口打开管理页
│  ├─ gateway.go              # 转发与请求改写（模型名、token 上限、会话头）
│  ├─ usage.go                # 用量记录、响应体 usage 扫描、按天/供应商/模型汇总
│  ├─ usage_test.go           # 用量单元测试（含网关只计 POST、失败也计数）
│  ├─ config.go               # 供应商 / 模型配置，原子写 config.json
│  ├─ admin.go                # /_admin 页面与 /_api 接口，只回掩码密钥
│  ├─ admin.html              # 内嵌的浅色管理界面
│  ├─ metadata.go             # models.dev 元数据 + OpenCode Go 月度额度抓取
│  ├─ green.ico / red.ico     # 托盘图标
│  ├─ go.mod / go.sum
└─ .github/workflows/build.yml
```
