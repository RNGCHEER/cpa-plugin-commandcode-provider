# commandcode-provider — CLIProxyAPI 插件

把 [dsh-commandcode-provider](https://github.com/Mars-Sea/dsh-commandcode-provider)（DeepSeek Harness 的 Command Code 提供方插件）移植到 **CLIProxyAPI** 的原生动态插件。

- 上游：Command Code 的 OpenAI 兼容 Provider API（`https://api.commandcode.ai/provider/v1`）
- 插件 ID：`commandcode-provider`（动态库文件名即 ID）
- 对外 provider key：`commandcode`（凭证记录与模型命名空间同名）
- 许可：MIT（原插件与本移植均为 MIT）

## 需求 1：支持 Command Code 的 Go 套餐

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
| 三协议客户端（OpenAI / Claude / Responses） | ✅ executor_input/output_formats 三种，统一转 chat-completions 上游 |
| reasoning 保真（reasoning → reasoning_content） | ✅ 非流式 + 每个 SSE 数据块都回填 |
| stop 序列被上游拒绝 | ✅ 翻译时统一丢弃 stop 字段 |
| 配额面板（套餐 / 5 小时 / 每周窗口） | ✅ 资源页 /v0/resource/plugins/commandcode-provider/quota（凭证来自 config 的 api-keys 池与 auths/ 记录，两者合并） |
| 网页登录（cmd login 授权流） | ❌ 未移植：key 从 api-keys、凭证文件或 ~/.commandcode/auth.json 读取 |
| 会话成本试算 / 模型成本分析 | ❌ 不移植：dsh 专属的会话历史面，CPA 有自己的 usage 统计 |
| web_search 提供方 | ❌ 不移植：dsh 的搜索工具面，CPA 无对应能力 |
| 图片尺寸裁剪 / ZDR 头 | ⚠️ 部分：zdr 仅作目录元数据保留，未加 x-cmd-zdr 请求头 |

## 修复记录

- **配额页不显示凭证（已修复）**：早期版本只从宿主的 `auths/` 记录生成凭证卡片，因此只用 `plugins.configs.commandcode-provider.api-keys` 配置密钥的用户会看到「没有找到 commandcode 凭证」。
  现在参考 [mczhoucn/commandcode-go-cliproxyapi](https://github.com/mczhoucn/commandcode-go-cliproxyapi) 的做法：卡片由 **config 的 api-keys 池** + `auths/` 记录**合并**生成；凭证 ID 用 key 的 SHA-256 摘要（`commandcode-key-<hash[:12]>` 风格），页面只显示摘要、不落明文；刷新某个账号时用摘要回查原始 key。请求同时带上管理端 `Authorization: Bearer <management-key>`。
  注意：改动在 `internal/plugin/models.go` 与 `internal/plugin/quota_page.go`，**必须重新编译动态库才生效**。

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

测试覆盖：能力声明、套餐过滤、三协议请求翻译、reasoning 回填、key 切换顺序、HTTP 状态可重试分类、未知方法信封。

## 目录结构

```
main.go                     CGO ABI 胶水（cliproxy_plugin_init / Call / Free / Shutdown）
internal/plugin/
  plugin.go                 方法分发、生命周期、流式 pump
  config.go                 配置解析与校验（plan-filter / api-keys / model-prefix ...）
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