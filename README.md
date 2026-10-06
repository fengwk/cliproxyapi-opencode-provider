# cliproxyapi-opencode-provider

CLIProxyAPI (CPA) 的 OpenCode Go 原生插件。独立仓库、独立动态库，不修改或 fork CPA。

支持 OpenAI Chat Completions、Anthropic Messages、OpenAI Responses 三种客户端协议，
流式和非流式均可使用；协议转换直接复用 CPA 的公开 `sdk/translator` 与 `builtin`。
支持通过 CPA 原生配额接口和插件页面查询 OpenCode Go 的滚动、周、月额度。

## 职责边界

```text
客户端 -> CPA 的会话识别、亲和、选 key、冷却、重试
       -> 插件的模型路由、协议转换、请求头与上游会话隔离 -> OpenCode Go
```

- CPA 管理全部凭据池、故障转移、冷却、会话识别和 LCP 回退。
- 插件不维护会话表、调度器或跨请求重试状态，仅管理当前流的生命周期。
- HTTP 与凭据持久化使用宿主回调，密钥保存在 CPA 的 `oauth.auth-dir`。
- 嵌入式中文管理页没有外部依赖，不在浏览器存储中保存密钥。

## 安装与使用

要求：支持原生插件的 CPA v8。真实宿主测试覆盖 v8.0.15 和 v8.0.16；
更高 v8 版本由 CI 每日验证，不保证未通过测试的版本或 v9 兼容。
当前 SDK 固定为 v8.0.16。

### 1. 安装插件

#### 从自定义插件源安装（推荐）

将下面的源合入现有 CPA 配置的 `plugins` 对象：

```yaml
plugins:
  enabled: true
  dir: "./plugins"
  store-sources:
    - "https://raw.githubusercontent.com/fengwk/cliproxyapi-opencode-provider/main/registry.json"
```

保存配置并让 CPA 重载，然后在 **插件商店** 中刷新、搜索 **OpenCode Go** 并点击安装。
商店会读取最新正式 GitHub Release，下载运行平台对应的 ZIP、验证 `checksums.txt`，
安装动态库并启用插件。自定义源不替换官方源，也不需要等待官方商店收录。
安装后在 **插件管理** 确认已注册、已生效；若安装响应要求重启，再重启 CPA。

仅添加 `plugins.configs` 配置不会下载安装文件，只有配置没有动态库时会显示“未注册”。
CPA 运行环境需要能访问 GitHub Raw、GitHub API 与 Release 下载地址。

#### 手动构建或安装

本地构建需要 Go 1.26+、C 编译器和 Make：

```bash
git clone https://github.com/fengwk/cliproxyapi-opencode-provider.git
cd cliproxyapi-opencode-provider
make build
```

产物：`dist/<goos>/<goarch>/cliproxyapi-opencode-provider.so`（macOS 为 `.dylib`）。
复制到 CPA 配置的 `<plugins.dir>/<goos>/<goarch>/`，不要将 C 共享库误当成 Go `plugin` 包。

CI 构建 Linux amd64、Linux arm64、macOS arm64 的原生库；
主分支 CI 的 `native-*` artifacts 可下载。正式版本的归档与校验和由 `v*` tag 的
release 工作流生成。应选择匹配操作系统、架构和 C 运行时的库；不支持直接将 glibc
Linux 构建用于 musl/Alpine。

### 2. 配置 CPA

参考 [config.example.yaml](config.example.yaml)，替换管理密钥与下游客户端密钥。
将以下片段合入现有 CPA v8 配置，而不是覆盖原配置：

```yaml
routing:
  strategy: round-robin
  session-affinity: true
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    cliproxyapi-opencode-provider:
      enabled: true
      base-url: "https://opencode.ai/zen/go/v1"
```

密钥导入 UI 需要启用 CPA 管理 API。示例默认仅允许本机管理；
远程使用必须配置 HTTPS 并显式开启 CPA 的远程管理。

### 3. 导入 OpenCode Go 密钥

启动 CPA 后打开：

```text
http://127.0.0.1:8317/v0/resource/plugins/cliproxyapi-opencode-provider/ui
```

输入 CPA **管理密钥**，再逐行粘贴 OpenCode Go 的 API keys。支持批量导入、去重、
非敏感状态列表和删除。管理密钥、下游访问 CPA 的 API key、OpenCode 上游 API key
是三种不同凭据，不要混用。密钥文件权限由 CPA 设置为 `0600`；请备份并保护 auth 目录。

### 4. 发起请求

模型 ID 使用固定命名空间 `opencode-go/`，例如 `opencode-go/glm-5.2`、
`opencode-go/minimax-m2.7`、`opencode-go/gpt-5.6-luna`。
实际可用性、额度与缓存策略仍由 OpenCode Go 决定。

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -H "x-opencode-session: example-conversation-1" \
  -d '{"model":"opencode-go/glm-5.2","messages":[{"role":"user","content":"Hello"}]}'
```

同一会话应复用同一客户端会话信号。已有 `X-Session-Affinity` 时优先保留该信号；
否则插件仅对自己的模型将显式 `x-opencode-session` 适配为 CPA 的亲和头。
没有显式信号时，仍由 CPA 自行识别会话，插件不再计算请求内容哈希。

### 5. 查看 OpenCode Go 配额

在同一个插件页面加载密钥列表，点击对应密钥的 **查看配额**，即可看到滚动、周、月
窗口的剩余比例、已用比例和重置时间（UTC）。查询按点击触发；插件不轮询、不重试、
不缓存配额，也不支持重置。列表刷新、导入或删除时会清除旧查询结果。

这是上游账户的额度，不是 CPA 本机累计 token 统计。多个密钥可能属于同一订阅、共享
额度，不能把它们的剩余量相加。上游目前提供百分比而非金额或绝对 token 余额；窗口
周期和重置时间以上游响应为准，插件不硬编码滚动窗口时长或推算余额。

插件注册 CPA 原生 `QuotaProvider`，让 `opencode-go` 凭据带有 `supports_quota: true`
和 `quota_provider: opencode-go`。支持通用插件配额的管理前端可以直接展示。**官方管理
前端的“配额管理”页目前仍只适配内置提供商，安装本插件不会自动增加该页面的卡片**；
在前端完成适配前，请使用插件页或下面的原生 API，无需修改 CPA 核心。

```bash
# 使用 CPA 管理密钥；返回文件名、标签和查询所需的 auth_index，不返回上游密钥。
curl http://127.0.0.1:8317/v0/management/plugins/cliproxyapi-opencode-provider/keys \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"

# AUTH_INDEX 取自上述已托管密钥列表。
curl http://127.0.0.1:8317/v0/management/plugins/cliproxyapi-opencode-provider/quota \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"auth_index\":\"$AUTH_INDEX\"}"
```

同样可以使用 `POST /v0/management/quota/fetch`，或
`GET /v8/management/plugins/cliproxyapi-opencode-provider/quota?auth_index=...`。
发现接口为 `GET /v0/management/quota/providers`；以上接口均由 CPA 管理认证保护。
每次查询通过宿主 HTTP 回调向配置的 `base-url` 追加 `/usage`，使用所选凭据进行一次
只读 GET。网络错误、上游拒绝或格式错误会返回失败，不会显示虚构的剩余额度，
也不会改变凭据的冷却或路由状态。

## 模型路由与缓存

| 模型前缀 | 原生上游协议 |
| --- | --- |
| `minimax`, `qwen` | Anthropic Messages |
| `gpt`, `grok`, `muse-spark` | OpenAI Responses |
| `glm`, `kimi`, `deepseek`, `longcat`, `mimo`, `hy`, `space-bunny` | OpenAI Chat Completions |

嵌入模型快照并尝试通过宿主发现模型；发现失败时使用快照。
未知模型族不会猜测协议，可以在插件配置中显式补充或覆盖：

```yaml
plugins:
  configs:
    cliproxyapi-opencode-provider:
      models:
        - id: "custom-model"
          protocol: "openai" # openai | claude | openai-response
```

CPA 的逻辑会话 `S` 保持不变；插件在选定凭据后计算上游会话：

```text
U = hex(SHA256(JSON(["opencode-go-session-v1", auth.ID, S])))
```

发送 `x-opencode-session: U` 和非默认 `User-Agent`；Chat/Responses 的
`prompt_cache_key` 同样使用 `U`。同一 auth 与会话组合稳定，换 key 后隔离上游会话。
这保证路由/缓存标识的一致性，**不保证实际缓存命中率或计费优惠**。

上游 401、429、5xx 通过标准 HTTP 状态交给 CPA 处理；插件不自行冷却或重试。
流式事件按完整 SSE 帧处理，不把缺失原生结束事件的 EOF 伪装成成功。
`count_tokens`、OAuth 登录和通用 `http_request` 不在本插件支持范围内。

## 验证与自动维护

```bash
make test                             # vet、Go 单测、Node 22+ UI 测试
make race                             # Go race detector
bash scripts/test-automerge.sh         # 自动合并安全边界
bash scripts/test-package-plugin.sh    # 真实 ZIP 布局与校验和
CPA_BINARY=/absolute/path/to/cpa make integration
make package                          # 需要 zip，输出 dist/pkg/
```

集成测试加载真正的 CPA 二进制与原生插件，使用本地 mock 上游和假密钥，覆盖：

- 三种客户端协议 x 三种上游协议 x 流式/非流式，共 18 条路径。
- 文本、推理、tool calls、usage 与 cached tokens。
- 多 key 429 故障转移、会话粘性、无显式信号的 CPA 会话回退。
- 管理 API 认证、资源安全头、密钥导入/列表/删除与重启持久化。
- 原生配额发现、凭据索引、三窗口映射、多 key 查询隔离、错误脱敏与只读重置拒绝。
- SSE 分片和截断，禁止虚假的成功结束。

维护流程：

1. 每日兼容性 CI 使用**未改动插件 SDK**测试最新稳定 CPA v8 宿主。
2. Dependabot 每日更新 Go 依赖，所有 PR 跑单测、原生构建及最低/最新宿主测试。
3. 只有 Dependabot 的纯 `go.mod`/`go.sum` 版本更新、直接依赖集合不变且完整 CI 通过时，
   才对**精确已验证提交**自动 squash 合并；越界、过期 CI 或失败都不合并。
4. 合并后显式触发主分支 CI，重新生成原生 artifacts；不依赖仓库的 Allow auto-merge 设置，
   不自动发布 tag、部署或覆盖正在运行的 CPA。
5. GitHub Actions 更新每周提 PR，工作流修改与 CPA v9 迁移需要人工确认。

发布流程：将已验证提交打上 `vX.Y.Z` tag 并推送，release 工作流自动构建三个平台、
打包并发布正式 GitHub Release。当前不会在依赖更新合并后自动打 tag 或发版；
CI artifacts 不是正式 Release。`registry.json` 不固定版本，新 Release 无需修改插件源。
发布不等于部署，已安装插件需在 CPA 插件商店中主动更新，不会后台覆盖运行中的动态库。

宿主升级不会更新已经编译进插件的 translator。宿主自己的池化/亲和逻辑可独立升级；
需要新的 SDK 转换逻辑时安装重新构建的插件，而不是手改转换器。
启用仓库 GitHub Actions 与 Dependabot 后，这些检查和依赖更新会自动运行。

## 安全与许可证

静态资源公开且不含密钥，密钥管理接口由 CPA 管理认证保护。UI 禁止非本机明文 HTTP
操作、避免 HTML 注入、不回显服务端错误文本，也不把密钥放进 URL、日志或浏览器存储。
不要使用已泄露的 API key；本仓库与测试不包含真实 OpenCode 凭据。

MIT；SDK 与 C ABI 参考实现的归属见 [NOTICE](NOTICE)。
