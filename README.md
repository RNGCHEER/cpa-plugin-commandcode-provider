# CommandCode Provider for CLIProxyAPI

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生动态插件，提供 **CommandCode** 模型通道，并自带一个「**密钥与套餐**」管理页面。

- 对齐上游 [pi-commandcode-provider v0.8.7](https://github.com/patlux/pi-commandcode-provider/releases/tag/v0.8.7)，能力快照来自 command-code@1.91.1。
- **v0.3.0 迁移**：推理仅走 Provider API；403 upgrade_required 直接返回，不再回退 /alpha/generate。已有密钥、认证文件和套餐筛选配置继续使用。
- Claude 模型走 Messages，目录支持 Responses 的其他模型优先走 Responses，其余走 Chat Completions。使用 CPA SDK 翻译请求与响应，保留图片和工具调用。
- Go 筛选参考[官方 Go 套餐页面](https://commandcode.ai/docs/plans/go)的 62 个模型（2026-11-23），仅补全实时目录中缺失的套餐信息，不增加已退役模型。套餐内可用模型不代表账号拥有 Provider API 权限。
- 支持 enable-zdr 与 CMD_ZDR=1 / COMMANDCODE_ZDR=1，默认关闭；环境变量开启优先。
- GLM-5.3 / Mistral Large 4 Responses 请求修复；GLM-5.3 明文推理修复只对该模型生效。

---

## 特性

| 能力 | 说明 |
| --- | --- |
| Provider API | 需要账号具备 API 权限；403 upgrade_required 直接返回 |
| 按凭证调度 | 宿主认证优先，配置密钥池作为故障切换 |
| **密钥管理页面** | 粘贴密钥 → 校验 → **写成宿主认证文件（auths/）**，CPA 才会真正调度 |
| **套餐挡位** | 从 `/alpha/billing/subscriptions` 读出套餐（Go / GOAT / Pro / Provider / Max），也可手动指定 |
| **按挡位发布模型** | 依据目录的 `minimumPlan` 决定客户端可见模型，写 `plan-filter`；也可勾成白名单 `visible-models` |
| 额度卡片 | 每个凭证显示套餐、剩余 credits、5 小时窗口、每周窗口，点击刷新 |
| 流式 / 工具调用 | OpenAI chat-completions、Responses 与 Claude 路由的请求都会按模型路由到对应的 Provider API 协议 |

---

## 安装

### 插件商店

EasyCLIProxyAPI / CLIProxyAPI 管理面板 → 插件商店 → 搜索 `commandcode-provider`。

### 手动安装

从 [Releases](https://github.com/RNGCHEER/cpa-plugin-commandcode-provider/releases) 下载对应平台压缩包，解压到插件目录（压缩包内就是库文件本身）：

```
plugins/
  windows/amd64/commandcode-provider.dll
  linux/amd64/commandcode-provider.so
  darwin/arm64/commandcode-provider.dylib
  ...
```

`config.yaml` 至少需要：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    commandcode-provider:
      enabled: true
```

重启核心，面板插件菜单里会出现「**CommandCode 密钥与套餐**」。

---

## 快速开始

打开页面：

```
http(s)://<你的 CPA 地址>/v0/resource/plugins/commandcode-provider/quota
```

（页面用浏览器 localStorage 里已登录的管理密钥调宿主接口，所以要在同一域名/端口、先登录管理面板。）

1. **粘贴密钥**：`user_...`（也可以用官方 CLI 的 `~/.commandcode/auth.json` 里的 `apiKey`）。
2. **点「识别套餐」**：插件用该 key 请求 `/alpha/billing/subscriptions`、`/alpha/billing/credits`、`/alpha/whoami`，回显套餐、账号、剩余额度，并把挡位自动选好。
3. **调挡位 / 勾模型**：改挡位会按目录 `minimumPlan` 自动勾选；手动勾选后保存会写成白名单。
4. **点「导入密钥并保存设置」**：
   - 校验通过 → 调宿主 `host.auth.save` 写成 `auths/commandcode-key-<sha256>.json`（**这一步才让请求能真正路由**）；
   - 同时把 `plan-filter` / `visible-models` 写进 `config.yaml`；
   - 若该 key 之前还留在 `api-keys` 里，会自动摘掉，避免重复卡片。
5. **删除密钥**：管理面板「认证文件」里删对应的 `commandcode-key-*.json`。

> ⚠️ 只把密钥写进 `plugins.configs.commandcode-provider.api-keys`、不加认证文件时，页面能看到额度卡片，但请求会返回
> `503 auth_not_found: no auth available (providers=commandcode …)` —— CPA 的调度器只认认证记录。v0.2.0 的页面已经自动处理这一步。

---

## 配置项

`plugins.configs.commandcode-provider` 下：

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 是否启用（宿主也会看这个值） |
| `base-url` | `https://api.commandcode.ai/provider/v1` | Provider API 基址 |
| `enable-zdr` | `false` | 发送 x-cmd-zdr: 1；环境变量 CMD_ZDR=1 或 COMMANDCODE_ZDR=1 优先开启 |
| `endpoint-overrides` | 空 | 模型 id → auto / responses / chat-completions；Claude 仅允许 auto，Responses 必须由目录声明 |
| `catalog-url` | `{base-url}/models` | 模型目录地址 |
| `api-keys` | 空 | 密钥池；支持 `${ENV_VAR}`；**优先使用认证记录，这里是 failover** |
| `plan-filter` | 空 | `go` / `goat` / `pro` / `provider` / `max`，按挡位过滤可见模型 |
| `visible-models` | 空 | 白名单（目录 id，不带前缀）；非空时只发布这些 |
| `exclude-hidden` | `true` | 过滤目录里的 `hidden` 模型 |
| `request-timeout` | `5m` | 单次上游请求超时 |
| `allow-http` | `false` | 允许 http:// 上游（仅本机测试） |
| `model-prefix` | `{enabled: true, value: commandcode}` | 客户端模型 id 前缀 |

---

## 套餐挡位与可见模型

插件按目录里每个模型的 `minimumPlan` 判定所需挡位，低档账号看不到高档模型。

| 挡位 | `plan-filter` | 发布模型数（目录 102 行，已剔除 hidden） |
| --- | --- | --- |
| 不过滤 | `""` | 94 |
| Go | `go` | 58 |
| GOAT | `goat` | 69 |
| Pro | `pro` | 84 |
| Provider | `provider` | 92 |
| Max | `max` | 94 |

> 数字是 2026-10-20 实测（`/v1/models` 里 `commandcode/*` 的条数），目录随上游更新会变。
> `plan-filter` / `visible-models` 是**插件级（全局）**设置，不按账号独立。

---

## 管理接口

| 路由 | 说明 |
| --- | --- |
| `GET /v0/resource/plugins/commandcode-provider/quota` | 页面（HTML 本身不需要管理密钥；页面内的接口调用需要） |
| `POST /v0/management/plugins/commandcode-provider/quota-usage` | 凭证卡片 / 额度窗口（不传参返回全部，传 `auth_index` 或 `key_id` 刷新单个） |
| `POST /v0/management/plugins/commandcode-provider/inspect-key` | 只校验不落盘：`{"api_key": "..."}` → 套餐 id / 挡位 / 额度 / 该挡位可用模型 |
| `POST /v0/management/plugins/commandcode-provider/catalog` | 模型目录（含 `minimum_plan`）+ 当前 `plan-filter` / `visible-models` / 已配置凭证（脱敏） |
| `POST /v0/management/plugins/commandcode-provider/import-key` | 校验 + 调 `host.auth.save` 写认证文件 |

写插件配置走宿主自带的 `PATCH /v0/management/plugins/commandcode-provider/config`（浅合并，保留注释）。

---

## 常见问题

**页面提示「读取插件配置失败（HTTP 401）」**
没登录管理面板，或页面和面板不在同一个域名/端口（页面要用 localStorage 的 `cli-proxy-auth`）。

**请求 503 `auth_not_found: no auth available (providers=commandcode …)`**
没有 commandcode 的认证记录。用页面「导入密钥并保存设置」，或在 `auths/` 放 `{"type":"commandcode","api_key":"user_..."}`。

**请求 503 `model_price_error: Model … has no configured price`**
装了 `cpa-key-billing` 之类的计费插件，且该模型没配价。计费插件按**客户端可见模型名（带 `commandcode/` 前缀）**查自定义价，查不到就拒。要么在计费面板补价，要么关掉计费插件。

**`/v1/messages`（Claude 客户端）异常**
v0.3.0 使用 CPA SDK 翻译请求、非流式响应和流式事件，支持 Claude、Responses 和 Chat Completions 客户端。

**插件商店点「更新」后新界面消失**
商店会装回官方版本，覆盖本地构建。

---

## 从源码构建

需要 Go（见 `go.mod`）与 C 工具链（c-shared 必须 CGO）。

```bash
CGO_ENABLED=1 go build -buildmode=c-shared -trimpath \
  -ldflags "-s -w -X github.com/RNGCHEER/cpa-plugin-commandcode-provider/internal/plugin.PluginVersion=0.3.0" \
  -o commandcode-provider.so .
```

Windows 用 MinGW 时设置 `CC` 指向 `gcc.exe`，产物为 `.dll`。单元测试：`go test ./...`。

---

## 发布流程（GitHub Actions）

`.github/workflows/build.yml`：

- **PR / push**：`gofmt` 校验、`go vet`、`go test`，并在 5 个平台（linux amd64/arm64、darwin amd64/arm64、windows amd64）交叉编译。
- **push 到 `main`**：读 `internal/plugin/plans.go` 里的 `PluginVersion`，若对应 tag `v<version>` 不存在就**自动打标签**，并在同一次运行里发布 Release。
- **推 `v*` 标签**：直接构建 + 发布（同一套产物）。
- **手动**：Actions → Build → Run workflow。

产物按插件商店约定命名，压缩包内就是库文件本身：

```
commandcode-provider_<version>_<goos>_<goarch>.zip
commandcode-provider_<version>_<goos>_<goarch>.zip.sha256
checksums.txt
```

**发新版只需两步**：改 `internal/plugin/plans.go` 里的 `PluginVersion` → push 到 `main`。

---

## 文档

- [V0.2.MD](V0.2.MD)：v0.2 的实现说明（设计取舍、数据流、接口契约、模型过滤算法、踩坑记录）
- [docs/适配说明.md](docs/适配说明.md)：v0.2 历史网关实现记录；v0.3 不再使用私有网关

## 致谢

- [patlux/pi-commandcode-provider](https://github.com/patlux/pi-commandcode-provider)
- [Mars-Sea/dsh-commandcode-provider](https://github.com/Mars-Sea/dsh-commandcode-provider)
- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) · [issue #3955](https://github.com/router-for-me/CLIProxyAPI/issues/3955)

## License

MIT

社区: [Linux.do](https://linux.do) — 一个友好的中文技术社区。
