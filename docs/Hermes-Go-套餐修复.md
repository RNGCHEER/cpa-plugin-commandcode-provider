# Command Code Go 套餐在 Hermes Agent 中不可用：修复记录

> 适用：EasyCLIProxyAPI / CLIProxyAPI 8.0.34 + `commandcode-provider` 0.1.2
> 日期：2026-10-20

## 1. 现象

Hermes Agent 选择 `commandcode/...` 的 Go 套餐模型后请求失败。实际日志里有两类错误：

```
Post "http://127.0.0.1:3050/v1/chat/completions": dial tcp 127.0.0.1:3050:
connectex: No connection could be made because the target machine actively refused it.
```

```
{"error":{"code":"model_price_error",
 "message":"Model commandcode/deepseek/deepseek-v4.1-flash has no configured price",
 "type":"cpa_key_billing_error"}}
```

## 2. 根因（四层叠加）

1. **运行中的核心仍是旧插件 0.1.0**
   - 最后一次内核启动是 `2026-10-19 21:41:02`，日志注册的是 `version=0.1.0`。
   - 新的 0.1.1 DLL 是 `22:32` 才放进去的，之后只发生了 `config file changed, reloading`。
   - CLIProxyAPI 的配置热重载**不会重新 dlopen 动态库**，所以 Go 网关代码从未生效。

2. **Hermes 的请求被路由到已停掉的 3050 Node 反代**
   - `cpa-core/config.yaml` 的根级 `api-keys.openai-compatibility` 有一个
     `name: commandcode-go`、`base-url: http://127.0.0.1:3050/v1` 的组。
   - 它的模型名没有 `commandcode/` 前缀，和插件的 `commandcode/` 命名空间冲突；
     插件模型未命中时请求落到这个组，3050 没启动 → 500。

3. **插件没有开启套餐过滤**
   - `plugins.configs.commandcode-provider` 只有 `enabled: true`，没有 `plan-filter`。
   - 插件发布整个目录（含 GOAT/Pro/Provider/Max 模型），Hermes 的模型列表被污染，
     选到非 Go 模型就会 403。

4. **`cpa-key-billing` 没有 Command Code 模型的价格**
   - Command Code 的模型不在 models.dev 里，billing 插件在 `request.intercept_before`
     阶段直接以 `503 model_price_error` 拒绝，请求根本到不了 commandcode 插件。
   - 这是本次 500 修好之后暴露出来的第二道拦截。

官方模型目录 `GET https://api.commandcode.ai/provider/v1/models?view=client` 无需鉴权，
每行都带 `minimumPlan`（`individual-go` / `individual-goat` / `individual-pro` /
`individual-provider` / `individual-max`）。因此套餐可见性应以目录为准，不写死静态表。

## 3. 已实施的修改

### 3.1 `cpa-core/config.yaml`

`plugins.configs.commandcode-provider`：

```yaml
commandcode-provider:
    enabled: true
    priority: 1
    plan-filter: go          # 只发布 Go 套餐可用的模型
    api-keys:
        - "<COMMANDCODE_API_KEY_1>"
        - "<COMMANDCODE_API_KEY_2>"
```

- 删除根级 `api-keys.openai-compatibility` 的 `commandcode-go` 组（整段 3050 路由）。
  现在核心启动日志为 `0 OpenAI-compat`，不会再出现 3050。
- 第二个 key 保留在 `api-keys` 池里，作为失败切换的备用凭证。

### 3.2 插件源码（v0.1.2）

- `internal/plugin/plans.go`：`PluginVersion` 改为 `0.1.2`，便于确认新库已加载。
- `internal/plugin/catalog.go`：
  - 当目录带 `minimumPlan` 时，`minimumPlan` 未知的行不再放进低套餐列表，
    防止 premium 模型泄漏；
  - 当目录退化为裸视图（完全没有 plan 元数据）时不做过滤，避免发布空列表。
- `internal/plugin/plugin_test.go`：新增
  `TestVisibleHonorsPlanFilter` 与 `TestVisibleKeepsRowsWithoutPlanMetadata`。
- 重新编译：
  ```bash
  export PATH=/usr/local/go/bin:$PATH
  export GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc
  go test ./internal/plugin/
  go build -trimpath -buildvcs=false -ldflags="-s -w" -buildmode=c-shared \
    -o commandcode-provider.dll .
  ```
- 覆盖到 `cpa-core/plugins/windows/amd64/commandcode-provider.dll` 后**完整重启**
  EasyCLIProxyAPI（退出 GUI + 结束 `cli-proxy-api.exe` + 重新打开）。

### 3.3 `cpa-key-billing` 自定义价格

从官方目录生成 58 个 `commandcode/...` 模型的 `pricing`，通过插件管理 API 写入：

```
PUT /v0/management/plugins/cpa-key-billing/prices
Authorization: Bearer <management-key>
{
  "model_id": "commandcode/deepseek/deepseek-v4.1-flash",
  "input_per_1m": 0.15,
  "output_per_1m": 0.60,
  "cache_read_per_1m": 0.003
}
```

映射规则：

| 官方目录字段 | billing 字段 |
| --- | --- |
| `pricing.inputCost` | `input_per_1m` |
| `pricing.outputCost` | `output_per_1m` |
| `pricing.cacheReadCost` | `cache_read_per_1m` |
| `pricing.cacheWriteCost` | `cache_write_per_1m` |

免费模型（目录无 `pricing`）按 0 写入；`cache_write` 缺失时留空，由插件按输入价继承。
以后官方目录新增模型时，需要补一条自定义价格，否则 billing 会以
`model_price_error` 拒绝。

### 3.4 Hermes `config.yaml`

路径：`%LOCALAPPDATA%\hermes\config.yaml`

- `model.default` → `commandcode/deepseek/deepseek-v4.1-flash`
- `model.provider` → `cpa-gui`
- 迁移到 Hermes 的 `providers:` 标准写法（`custom_providers` 已是 retired list store，
  `hermes doctor` 会提示缺少 `providers:` twin）：`providers.cpa-gui` 使用
  `api` / `api_key` / `transport` / `default_model` / `models`。
- `models` 只包含 58 个 `commandcode/...` 的当前 Go 模型；删除所有无前缀 ID、
  非 Go 模型和已下线 ID。
- 删除旧的 `providers.ccproxy`（3050 Node 反代）和 `custom_providers` 列表。

示例：

```yaml
model:
  default: commandcode/deepseek/deepseek-v4.1-flash
  provider: cpa-gui
  base_url: ''

providers:
  cpa-gui:
    name: cpa-gui
    api: "http://127.0.0.1:8317/v1"
    api_key: "<CPA_CLIENT_KEY>"
    transport: chat_completions
    default_model: "commandcode/deepseek/deepseek-v4.1-flash"
    discover_models: false
    models:
      - "commandcode/deepseek/deepseek-v4.1-flash"
      # ... 其余 57 个 Go 模型
```

> `hermes doctor` 仍会提示 `model.default` 带 vendor 前缀（因为 provider 不是
> openrouter）。这是启发式警告；实测 Hermes 会把 `commandcode/...` 原样发给 CPA，
> 不要为了消警告去掉前缀。

修改后重启 Hermes / 新开会话，让模型选择器重新读配置。

## 4. 验证结果（本机实测）

| 项目 | 结果 |
| --- | --- |
| 插件注册版本 | `version=0.1.2` |
| `/v1/models` 中 `commandcode/` 数量 | 58（官方 Go 口径） |
| 非 Go 模型泄漏 | 无（`muse-spark-*-contributor` 是 Go 档） |
| 非流式请求 | 200，正文 `OK` |
| 流式请求 | SSE chunk + `finish_reason: stop` + `data: [DONE]` |
| 系统提示 + tools + reasoning_effort | 200，`prompt_tokens=293`（客户端系统提示生效） |
| 工具调用 | `finish_reason: tool_calls`，`get_time` 参数正确 |
| 工具结果回放 | 200，模型正确使用工具结果 |
| 免费 Go 模型 | `minimax/minimax-m3.1-flash:free` 200 |
| 核心日志 | `commandcode: plan has no provider API access, using the CLI gateway` |
| 3050 / model_price_error | 不再出现 |

## 5. 切换套餐

只改 `cpa-core/config.yaml` 的 `plan-filter`，然后完整重启核心：

| 套餐 | 配置 |
| --- | --- |
| Go | `plan-filter: go` |
| GOAT | `plan-filter: goat` |
| Pro | `plan-filter: pro` |
| Provider | `plan-filter: provider` |
| Max | `plan-filter: max` |

不设置则发布整个目录。模型可见性始终由官方目录的 `minimumPlan` 决定。

## 6. 回滚

本次修改前的备份（时间戳 `20261020-085955` / `090445`）：

- `cpa-core/config.yaml.bak-hermes-go-20261020-085955`
- `cpa-core/plugins/windows/amd64/commandcode-provider.dll.bak-hermes-go-20261020-085955`
- `%LOCALAPPDATA%\hermes\config.yaml.bak-clean-20261020-090445`
- `%LOCALAPPDATA%\hermes\config.yaml.bak-providers-20261020-091042`

回滚后同样需要完整重启核心和 Hermes。

## 7. 相关文件

- `cpa-core/config.yaml`
- `plugin-src/commandcode-provider/internal/plugin/{plans.go,catalog.go,plugin_test.go}`
- `plugin-src/commandcode-provider/commandcode-provider.dll`
- `cpa-core/plugins/windows/amd64/commandcode-provider.dll`
- `%LOCALAPPDATA%\hermes\config.yaml`
- `cpa-core/plugins/cpa-key-billing-state-v1.db`（自定义价格持久化）
