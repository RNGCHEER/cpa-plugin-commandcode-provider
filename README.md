# commandcode-provider — CLIProxyAPI 插件

把 [dsh-commandcode-provider](https://github.com/Mars-Sea/dsh-commandcode-provider)（DeepSeek Harness 的 Command Code 提供方插件）移植到 **CLIProxyAPI** 的原生动态插件。

- 上游：两个通道，按账号套餐自动选择
  - **Provider API**（`https://api.commandcode.ai/provider/v1`，OpenAI 兼容）：Provider 档账号
  - **CLI 私有网关**（`https://api.commandcode.ai/alpha/generate`）：Go / GOAT / Pro 档账号（见「需求 1」）
- 插件 ID：`commandcode-provider`（动态库文件名即 ID）
- 对外 provider key：`commandcode`（凭证记录与模型命名空间同名）
- 许可：MIT（原插件与本移植均为 MIT）

## 需求 1：支持 Command Code 的 Go 套餐

**Provider API 只对付费的 Provider 档开放。** Go / GOAT / Pro 订阅打过去必然被拒：

```
403 {"error":{"code":"upgrade_required","message":"Your Go plan doesn't include API access. Upgrade to Provider..."}}
```

这些档位只能走官方 `command-code` CLI 的私有网关 `POST {root}/alpha/generate`。插件因此对每把凭证**先试 Provider API，命中 `403 upgrade_required` 就立刻用同一把 key 改走 CLI 网关**，并把该凭证记为「仅网关」，后续请求直接走网关。记账按凭证进行，一个账号的套餐不会影响另一个账号（例如 Provider 档 + Go 档混用时各走各的）。

CLI 网关说的是 CLI 自己的信封并返回逐行 JSON 事件流（不是 OpenAI SSE），两侧翻译都在插件里：

- **请求**：OpenAI messages → 网关消息联合体（`user` / `assistant` / `tool`）。系统提示上提 `params.system`；`tool-calls` ↔ `tool-result` 配对补全（缺结果的调用补一条 error 结果）；tool call id 截到网关 64 字符上限且保持配对；tools 转 `{type,name,description,input_schema}`。`config.environment` 必须是字符串（对象会被 400 拒）。
- **响应**：`reasoning-delta` / `text-delta` / `tool-input-*` / `tool-call` / `finish` → OpenAI `chat.completion.chunk`（含 `reasoning_content` 回填与 usage）；非流式把整条流聚合成一个 `chat.completion`。

参考实现：[patlux/pi-commandcode-provider](https://github.com/patlux/pi-commandcode-provider) 的 transport router 与 [Mars-Sea/dsh-commandcode-provider](https://github.com/Mars-Sea/dsh-commandcode-provider) 的 `/alpha/generate` 协议。

适配的完整记录——协议实测（请求信封 / 消息联合体 / tools / 事件流 / 错误）、双向翻译细节、代码地图、验证结果与已知限制——见 **[docs/适配说明.md](docs/适配说明.md)**。

套餐不是写死的静态表，而是从上游**实时读取**，Go / GOAT / Pro / Provider / Max 任意套餐都能正确工作：

- 模型目录用 `GET {base}/models?view=client`，每行带 `minimumPlan`（如 `individual-go`）、`maxOutputTokens`、`reasoningEfforts`、`inputModalities`、`zdr`、`hidden`；裸视图作为降级备选。
- 账号套餐从 `/alpha/billing/subscriptions` 读取，配额与 5 小时 / 每周窗口从 `/alpha/billing/credits` 读取，账号邮箱从 `/alpha/whoami` 读取。
- `plan-filter: go` 时只注册 minimumPlan 权重 ≤ Go 的模型（即 Go 套餐实际可用的模型）；不设置则发布整个目录。
- 订阅权重表（`plans.go`）只用于配额页展示（套餐名 + 月度 credits 额度），路由判断始终以目录的 minimumPlan 为准。

```
yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    commandcode-provider:
      enabled: true
      plan-filter: go        # 只发布 Go 套餐可用的模型（可选：go/goat/pro/provider/max）
      # 以下三项可省略，默认值即上面的行为
      # generate-url: "https://api.commandcode.ai/alpha/generate"  # 默认由 base-url 去掉 /provider/v1 推导
      # cli-version: "1.84.0"   # x-command-code-version 头，需跟随 npm command-code 版本
      # working-dir: "D:/my-project"  # 上报给网关的项目目录，默认核心进程工作目录
```

## 需求 2：多 API 调用，模型不可用时自动切换

“多 API” 有两个来源，按顺序合成一次请求的尝试序列（`attemptOrder`）：

1. **宿主选中的凭证记录**（`auths/` 目录里的 commandcode 凭证，或 `api-keys` 池）——总是第一个尝试；
2. **插件配置的 `api-keys` 池**——按配置顺序排在后面。

切换发生在**任何可重试故障**上，且只在**上游首字节之前**：

| 状态 | 行为 |
| --- | --- |
| 401 / 402 / 403 | 配额或鉴权耗尽 → 换下一把 key |
| 429 | 限流 → 换下一把 key |
| 500 / 502 / 503 / 504 / 520-526 | 上游暂不可用 → 换下一把 key |
| 网络 / 传输错误 | 换下一把 key |
| 400 / 404 / 413 / 422 | 请求本身的问题，换 key 无意义 → 直接返回 |
| 2xx | 成功，终止切换 |

流式请求同理，但只在 **SSE 首个数据块之前**切换；一旦 2xx 流建立，已投递字节无法回滚，中途故障直接透传。每次切换通过 `host.log` 记录一行（永不记录密钥本身）。

```
yaml
plugins:
  configs:
    commandcode-provider:
      api-keys:
        - "${COMMANDCODE_API_KEY}"   # 支持环境变量
        - "user_second_account_key"
        - "user_third_account_key"
```

## 功能对照（相对 dsh 原插件）

| dsh 原插件能力 | 本移植 |
| --- | --- |
| 实时模型目录 + 计划标注 | ✅ model.static / model.for_auth，来自 ?view=client |
| 套餐感知模型可见性 | ✅ plan-filter + visible-models 白名单 |
| 多账号轮换 / 限额自动切换 | ✅ 凭证记录 + api-keys 池，attemptOrder 顺序切换 |
| 三协议客户端（OpenAI / Claude / Responses） | ⚠️ 三种入参格式都接受，但执行器只输出 OpenAI chat-completions，`/v1/messages` 客户端因此拿不到 Anthropic 格式（见「已知限制」） |
| Go / GOAT / Pro 套餐（CLI 网关） | ✅ Provider API 返回 `403 upgrade_required` 时自动改用同一凭证走 `/alpha/generate`，并记住该凭证为「仅网关」 |
| reasoning 保真（reasoning → reasoning_content） | ✅ 非流式 + 每个 SSE 数据块都回填 |
| stop 序列被上游拒绝 | ✅ 翻译时统一丢弃 stop 字段 |
| 配额面板（套餐 / 5 小时 / 每周窗口） | ✅ 资源页 /v0/resource/plugins/commandcode-provider/quota（凭证来自 config 的 api-keys 池与 auths/ 记录，两者合并） |
| 网页登录（cmd login 授权流） | ❌ 未移植：key 从 api-keys、凭证文件或 ~/.commandcode/auth.json 读取 |
| 会话成本试算 / 模型成本分析 | ❌ 不移植：dsh 专属的会话历史面，CPA 有自己的 usage 统计 |
| web_search 提供方 | ❌ 不移植：dsh 的搜索工具面，CPA 无对应能力 |
| 图片尺寸裁剪 / ZDR 头 | ⚠️ 部分：zdr 仅作目录元数据保留，未加 x-cmd-zdr 请求头 |

## 修复记录

- **Go / GOAT / Pro 套餐请求全部 403（已修复）**：早期版本只走 Provider API（`{base-url}/chat/completions`），而这些档位在该端点会被上游以 `403 upgrade_required` 拒绝，表现为「模型能列出来，一发请求就 403」。
  现在每把凭证先试 Provider API，命中 `upgrade_required` 就用同一把 key 改走 CLI 网关 `/alpha/generate`，并把该凭证记为「仅网关」；之后该凭证的请求直接走网关，不再白跑一次 403。网关的信封构造与消息/工具转换在 `internal/plugin/generate.go`，事件流 → OpenAI 分块在 `internal/plugin/generate_stream.go`，流式 pump 的网关分支在 `internal/plugin/plugin.go`（`pumpGatewayStream`）。
  新增可选配置 `generate-url` / `cli-version` / `working-dir`；`cli-version`（`x-command-code-version`）需跟随 npm `command-code` 的发布版本。
  移植来源：[patlux/pi-commandcode-provider](https://github.com/patlux/pi-commandcode-provider)（transport router、converters）与 [Mars-Sea/dsh-commandcode-provider](https://github.com/Mars-Sea/dsh-commandcode-provider)（`/alpha/generate` 协议）。**必须重新编译动态库才生效。**
- **配额页不显示凭证（已修复）**：早期版本只从宿主的 `auths/` 记录生成凭证卡片，因此只用 `plugins.configs.commandcode-provider.api-keys` 配置密钥的用户会看到「没有找到 commandcode 凭证」。
  现在参考 [mczhoucn/commandcode-go-cliproxyapi](https://github.com/mczhoucn/commandcode-go-cliproxyapi) 的做法：卡片由 **config 的 api-keys 池** + `auths/` 记录**合并**生成；凭证 ID 用 key 的 SHA-256 摘要（`commandcode-key-<hash[:12]>` 风格），页面只显示摘要、不落明文；刷新某个账号时用摘要回查原始 key。请求同时带上管理端 `Authorization: Bearer <management-key>`。
  注意：改动在 `internal/plugin/models.go` 与 `internal/plugin/quota_page.go`，**必须重新编译动态库才生效**。

## 已知限制

- **`/v1/messages`（Claude 客户端）不可用**：注册时 `executor_output_formats` 声明了 `openai` / `claude` / `openai-response` 三种，宿主据此认为执行器已经会说 Claude，于是不再翻译；而执行器实际只输出 OpenAI chat-completions，Claude 客户端因此收到 OpenAI 结构（流式还会以 `upstream stream closed before message_stop` 结束）。`/v1/chat/completions` 与 Responses 路线不受影响。
- **CLI 网关是未公开的 CLI 内部端点**：`/alpha/generate` 不在官方文档里，协议随 `command-code` 版本漂移，`cli-version` 需要跟着更新。
- **网关会附加它自己的 agent 系统提示**：`params.system` 为空时上游会注入 Command Code 自己的 agent 提示；客户端传了系统提示（如 Hermes）时以客户端的为准。

## 安全与隐私

- 仓库只包含源码，不含任何 API key、账号凭证或构建产物。
- `api-keys` 请用环境变量占位（如 `"${COMMANDCODE_API_KEY}"`），不要把明文 key 写进提交的 `config.yaml`。
- `auths/`、`config.yaml`、`*.log`、构建产物均已在 `.gitignore` 中排除。
- 发布动态库前务必带 `-trimpath`：否则 Go 会把构建机的绝对路径（用户名、项目目录）写进二进制，泄露本机环境。

## 构建

需要 Go 1.26+ 与 C 编译器（CGO）。依赖 CLIProxyAPI Go 模块 `github.com/router-for-me/CLIProxyAPI/v8`（当前已验证 v8.0.34）。

> 国内网络下 `proxy.golang.org` 常不可达，先设镜像：`$env:GOPROXY='https://goproxy.cn,direct'`

```
powershell
# Windows（MinGW/GCC 在 PATH 上）
$env:CGO_ENABLED = '1'
go build -trimpath -ldflags="-s -w" -buildmode=c-shared -o plugins\windows\amd64\commandcode-provider.dll .

# Linux
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -buildmode=c-shared -o plugins/linux/amd64/commandcode-provider.so .

# macOS
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -buildmode=c-shared -o plugins/darwin/arm64/commandcode-provider.dylib .
```

> 请务必带 `-trimpath`：不带它时 Go 会把**本机绝对路径**（如 `D:/xxx/plugin/internal/plugin/plans.go`、`C:/Users/xxx/go/pkg/mod/...`）写进动态库，发布产物会泄露构建机目录结构。

构建产物（`.dll` / `.so` / `.dylib` 与 cgo 生成的 `.h`）体积大且会嵌入构建机路径，不入库，`plugins/<GOOS>/<GOARCH>/` 只是默认输出目录；编译完成后把产物放进 CPA 的插件目录即可。

> Windows 上需要 gcc/MinGW 在 PATH 上（cgo 需要 C 编译器）；没有 C 编译器时 `go build -buildmode=c-shared` 会报 `cgo: C compiler "gcc" not found`。

## 安装与验证

1. 把动态库放进 CPA 的插件目录（`plugins/<GOOS>/<GOARCH>/` 或 `plugins/`）。
2. `config.yaml` 打开全局开关并添加插件配置：

```
yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    commandcode-provider:
      enabled: true
      priority: 1
      plan-filter: go
      api-keys:
        - "${COMMANDCODE_API_KEY}"
```

3. 启动 CLIProxyAPI，确认插件注册成功：

```
powershell
curl -H "X-Management-Key: <key>" http://127.0.0.1:8317/v0/management/plugins
# 期望 commandcode-provider: registered: true, effective_enabled: true
```

4. 确认模型已发布并可调用：

```
powershell
curl http://127.0.0.1:8317/v1/models          # 应出现 commandcode/... 前缀的模型
curl -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <client-key>" \
  -d '{"model":"commandcode/deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}'
```

5. 配额页：<http://127.0.0.1:8317/v0/resource/plugins/commandcode-provider/quota>

## 测试

```
powershell
gofmt -l .        # 无输出
go vet ./...      # 退出码 0
go test ./...     # 全部通过
```

测试覆盖：能力声明、套餐过滤、三协议请求翻译、reasoning 回填、key 切换顺序、HTTP 状态可重试分类、未知方法信封；以及 CLI 网关部分——`403 upgrade_required` 识别、网关信封构造（系统提示上提、max_tokens 按目录钳制、tools 转换）、消息联合体转换（reasoning / tool-call / tool-result 配对与合成结果）、事件流 → OpenAI 分块翻译（含 usage 与 finish_reason）、tool call id 截断与配对。

## 目录结构

```
main.go                     CGO ABI 胶水（cliproxy_plugin_init / Call / Free / Shutdown）
internal/plugin/
  plugin.go                 方法分发、生命周期、流式 pump（含网关流翻译分支）
  generate.go               CLI 网关信封构造、消息/工具转换（Go/GOAT/Pro 套餐）
  generate_stream.go        网关事件流 → OpenAI chat-completions 翻译
  config.go                 配置解析与校验（plan-filter / api-keys / model-prefix / cli-version ...）
  plans.go                  套餐权重与展示名（仅配额页用）
  catalog.go                模型目录拉取、缓存、可见性过滤
  auth.go                   凭证解析 / 刷新（含官方 CLI 的 ~/.commandcode/auth.json）
  executor.go               多 key 失败切换（非流式 + 流式）
  translate.go              openai / claude / openai-response → chat-completions
  reasoning.go              reasoning → reasoning_content 回填、SSE 行解析
  models.go                 model.static、管理路由、配额数据
  quota_page.go             配额资源页 HTML
  hostbridge.go             host.http.* / host.log / host.stream.* 桥接
  envelope.go               ok / error JSON 信封
  plugin_test.go            单元测试
plugins/windows/amd64/      默认构建输出目录（产物不入库）
```

## 与 CLIProxyAPI 插件契约的对应

| 契约 | 本插件 |
| --- | --- |
| plugin.register / plugin.reconfigure | handleLifecycle：解析 config_yaml、预热目录、启动后台刷新 |
| plugin.shutdown | 停止后台刷新 goroutine（宿主 dlclose 前必须无残留） |
| model.static / model.for_auth | 同一过滤后的目录 |
| auth.identifier / auth.parse / auth.refresh | commandcode + API key 凭证 |
| executor.identifier / execute / execute_stream / count_tokens | 三协议入出 + 失败切换 + 本地 token 估算 |
| management.register / management.handle | 配额页路由 + 数据接口 |
| host.http.do / do_stream / stream_read / stream_close | 目录、账号接口、上游请求全部走宿主 |
| host.stream.emit / host.stream.close | 下游 SSE 投递与结束 |
| host.auth.list / host.auth.get | 配额页按凭证读取套餐 |

## 来源

- 原插件：<https://github.com/Mars-Sea/dsh-commandcode-provider>（MIT），上游参考 <https://github.com/patlux/pi-commandcode-provider>（MIT）
- CLIProxyAPI 插件开发文档：<https://help.router-for.me/cn/plugin/development>
- CLIProxyAPI 示例（ABI 骨架、执行器、调度器、model-router）：`examples/plugin/*`
- 同类 CommandCode 插件参考：<https://github.com/mczhoucn/commandcode-go-cliproxyapi>、<https://github.com/ahoo/cpa-plugin-commandcode>

本项目为社区移植，与 Command Code, Inc. 无关联；使用需自备账号与 API key，并遵守 Command Code 服务条款。