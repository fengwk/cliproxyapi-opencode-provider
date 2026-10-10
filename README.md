# OpenCode Provider

CLIProxyAPI (CPA) 的原生插件，目前支持 OpenCode Go。独立仓库、独立动态库，不修改或 fork CPA。
插件展示名为 **OpenCode Provider**，稳定 ID 为 `cliproxyapi-opencode-provider`。

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

要求：支持原生插件的 CPA v8。真实宿主测试覆盖 v8.0.15 和 v8.0.23；
更高 v8 版本由 CI 每日验证，不保证未通过测试的版本或 v9 兼容。
SDK 精确版本锁定在 `go.mod`，由自动维护链路在验证兼容后升级。

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

保存配置并让 CPA 重载，然后在 **插件商店** 中刷新、搜索 **OpenCode Provider** 并点击安装。
商店会读取最新正式 GitHub Release，下载运行平台对应的 ZIP、验证 `checksums.txt`，
安装动态库并启用插件。自定义源不替换官方源，也不需要等待官方商店收录。
安装后在 **插件管理** 确认已注册、已生效；若安装响应要求重启，再重启 CPA。

仅添加 `plugins.configs` 配置不会下载安装文件，只有配置没有动态库时会显示“未注册”。
CPA 运行环境需要能访问 GitHub Raw、GitHub API 与 Release 下载地址。

共享出口的匿名 GitHub API 额度用尽时，安装可能返回 429；这不是插件加载错误。
可为 CPA 进程设置环境变量 `CPA_PLUGIN_GITHUB_TOKEN`，再将下面的可选认证规则合入
`plugins` 对象。使用仅需读取公开 Release 元数据的 GitHub token，不要使用 OpenCode
密钥，也不要把 token 明文写入 YAML：

```yaml
plugins:
  store-auth:
    - match: "https://api.github.com/repos/fengwk/cliproxyapi-opencode-provider"
      apply-to: ["metadata"]
      type: "github-token"
      token-env: "CPA_PLUGIN_GITHUB_TOKEN"
```

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

启动 CPA 后，在管理面板左侧「插件」分组点击 **OpenCode Provider**，即可打开密钥管理页。
它不是「插件管理」里的基础配置抽屉。也可以直接打开：

```text
http://127.0.0.1:8317/v0/resource/plugins/cliproxyapi-opencode-provider/ui
```

插件页只允许同源管理面板 iframe 嵌入。若管理前端与 CPA 服务使用不同源，
请直接打开上面的 CPA 资源地址；不要移除 CSP 或允许任意站点嵌入密钥页。

输入 CPA **管理密钥**，再逐行粘贴 OpenCode Go 的 API keys。支持批量导入、去重、
非敏感状态列表和删除。管理密钥、下游访问 CPA 的 API key、OpenCode 上游 API key
是三种不同凭据，不要混用。密钥文件权限由 CPA 设置为 `0600`；请备份并保护 auth 目录。

凭据列表读取失败时不会执行导入；插件内的并发导入串行去重，不覆盖已有文件。
请求期间输入框暂时禁用；只有经过结果校验的完整成功才清空密钥，失败或结果未知时保留输入。

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
和 `quota_provider: opencode-go`。**官方管理面板 v1.26.1 起**会通过
`GET /v8/management/plugins` 发现该插件，并直接渲染通用的 `/quota` 插件卡片，无需修改
CPA 核心。v1.26.0 及更早版本仍请求已下线的 `/v8/management/quota/fetch`，刷新会返回
404；这是面板版本过旧，升级管理面板即可，并非 CPA 版本过旧。在升级面板前，可继续使用
插件页或下面的原生 API。

```bash
# 使用 CPA 管理密钥；返回文件名、标签和查询所需的 auth_index，不返回上游密钥。
curl http://127.0.0.1:8317/v0/management/plugins/cliproxyapi-opencode-provider/keys \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"

# AUTH_INDEX 取自上述已托管密钥列表。
# 官方管理面板 v1.26.1 的主路径：仅 auth_index，不带 provider。
curl http://127.0.0.1:8317/v8/management/plugins/cliproxyapi-opencode-provider/quota \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"auth_index\":\"$AUTH_INDEX\"}"
```

旧版脚本仍可使用 `POST /v0/management/quota/fetch`、
`POST /v0/management/plugins/cliproxyapi-opencode-provider/quota` 或
`GET /v8/management/plugins/cliproxyapi-opencode-provider/quota?auth_index=...`。
发现接口为 `GET /v0/management/quota/providers`；以上接口均由 CPA 管理认证保护。
每次查询通过宿主 HTTP 回调向配置的 `base-url` 追加 `/usage`，使用所选凭据进行一次
只读 GET。网络错误、上游拒绝或格式错误会返回失败，不会显示虚构的剩余额度，
也不会改变凭据的冷却或路由状态。

## 模型路由与缓存

模型目录来自两处合并：每个凭据的宿主发现，以及显式配置的 `manual-models`。插件
使用该凭据、经宿主 HTTP 回调请求一次 `GET <base-url>/models`，把成功响应里的官方
模型按其原始顺序、`created` 和 `owned_by` 发布到 `opencode-go/` 命名空间；
`manual-models` 只追加官方响应中不存在的 native id。插件不内置、不嵌入任何静态模型
清单，也**不会**回退到固定列表或上一次结果。

发现失败（回调失败、上游非 2xx、响应格式非法）时，若未配置
`manual-models`，仍以脱敏的失败 envelope 返回，**不会**被当作“空目录成功”；只有
上游返回合法的空 `data: []` 时才是真正的空目录。若已配置 `manual-models`，失败或
空目录都会回退为仅发布这些显式模型。两条路径都必须先解析出非空凭据，缺少凭据始终
返回错误。手动注册不验证密钥在上游是否有效，推理仍必须通过上游认证。

模型发现参与 CPA 的注册与路由，而不只是界面展示；CPA 的 `GET /v1/models` 读取其
注册表，不会为每次推理重新发现。插件只在宿主触发发现时调用一次 `/models`，
**不会**在每次推理时抓取；`/models` 失败也不等同于推理失败，协议转换、上游会话
与请求头适配照常进行。宿主按凭据的增删改触发发现；发现失败时宿主**可能**保留上一次
成功注册的目录（插件侧不新增任何缓存）。实际的刷新调度与保留行为以宿主版本为准。

发布时插件添加固定前缀 `opencode-go/`，上游实际收到的模型 id 是去掉该前缀的
native id，因此上游永远看不到 `opencode-go/` 前缀。

模型目录列出上游返回的全部官方模型（包括未知族），但执行时按下列前缀映射到上游
协议；未匹配任何前缀且未配置覆盖的模型 fail-closed，不会被猜测协议：

| 模型前缀 | 原生上游协议 |
| --- | --- |
| `minimax`, `qwen` | Anthropic Messages |
| `gpt`, `grok`, `muse-spark` | OpenAI Responses |
| `glm`, `kimi`, `deepseek`, `longcat`, `mimo`, `hy`, `space-bunny` | OpenAI Chat Completions |

`models` 配置**只**用于指定或覆盖上述协议路由，**不会**把未出现在官方响应或
`manual-models` 中的模型加入发布目录：

```yaml
plugins:
  configs:
    cliproxyapi-opencode-provider:
      models:
        - id: "custom-model"
          protocol: "openai" # openai | claude | openai-response
```

`manual-models` 用于显式注册模型 ID：官方目录之外都会以 `opencode-go/` 前缀发布，
即使 `/models` 失败也可用。填原始 ID 或 `opencode-go/` 全名（归一化时剥离一次前缀），
已知模型族可省略 `protocol`，未知族必须显式指定；最多 100 条、每条 ID 不超过 200
UTF-8 字节。官方目录已有的 ID 保留其 `created`/`owned_by` 元数据。手动注册只影响
CPA 目录与路由，不保证上游实际支持或额度充足。插件页的「手动模型」可编辑草稿、
校验（`POST /validate`）后保存（浅 PATCH 配置并回读确认），清空并保存可取消全部手动注册：

```yaml
plugins:
  configs:
    cliproxyapi-opencode-provider:
      manual-models:
        - id: "glm-5.2"           # 已知族，自动选择协议
        - id: "novel-model"
          protocol: "claude"      # openai | claude | openai-response
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
bash scripts/test-compatibility-report.sh # 兼容性失败上报与恢复
bash scripts/test-package-plugin.sh    # 真实 ZIP 布局与校验和
python3 scripts/test_dependency_policy.py # Go / 官方 Actions 纯版本升级策略
python3 scripts/test_auto_release.py      # 自动发版、竞态与恢复
python3 scripts/test_publish_release.py   # 不可变产物与草稿恢复
CPA_BINARY=/absolute/path/to/cpa make integration
make package                          # 需要 zip，输出 dist/pkg/
```

集成测试加载真正的 CPA 二进制与原生插件，使用本地 mock 上游和假密钥，覆盖：

- 三种客户端协议 x 三种上游协议 x 流式/非流式，共 18 条路径。
- 文本、推理、tool calls、usage 与 cached tokens。
- 多 key 429 故障转移、会话粘性、无显式信号的 CPA 会话回退。
- 管理 API 认证、资源安全头、密钥导入/列表/删除与重启持久化。
- 原生配额发现、凭据索引、三窗口映射、多 key 查询隔离、错误脱敏与只读重置拒绝。
- 真实宿主上发现失败时显式 `manual-models` 的认证校验、浅配置补丁、目录注册与撤销。
- SSE 分片和截断，禁止虚假的成功结束。

维护流程：

1. 每日兼容性 CI 使用**未改动插件 SDK**测试最新稳定 CPA v8 宿主；定时与手动触发都执行
   最新宿主检查。失败运行保持红色，并建立去重的、由自动化自有的 GitHub issue，仅包含
   运行链接，不公开日志或凭据；下一次通过后自动关闭该 issue。
2. Dependabot 每日更新 Go 依赖，每周更新官方 GitHub Actions。所有 PR 跑单测、race、
   管理页测试、自动化安全策略、三个平台原生构建及最低/最新真实宿主测试。
3. Go 更新必须保持模块路径、直接依赖集合及 CPA v8 不变；Actions 更新只能改变现有
   白名单官方 action 的版本引用，不得改变命令、权限、工作流结构或其他内容。
   完整 CI 通过后，只对**精确已验证提交**自动 squash 合并。
4. 合并后显式派发主分支完整 CI，不依赖仓库 Allow auto-merge 设置或被
   `GITHUB_TOKEN` 抑制的 push 事件。
5. 主分支 CI 通过后，控制器核对自上一正式版本以来的每个主线提交均来自已合并的
   同仓库 Dependabot PR 且满足安全策略，然后自动递增 patch 版本、创建绑定该提交的
   tag，并显式派发三平台 release 工作流。例如 `v0.1.2 -> v0.1.3`，无需人工打 tag。
6. 每 6 小时自动协调未完成发布：缺少主分支 CI 时仅对合格更新重新派发 CI，
   有活动任务时等待，派发或构建失败时重试同一 tag，绝不移动 tag、越过待发布版本，
   或覆盖已经公开的 Release。发布前核对校验和、归档布局、平台与源码提交；
   所有资源上传完整后才将草稿公开。

普通功能、配置、权限修改与 CPA v9 迁移不会被当作依赖更新自动发布；失败保持旧正式
版本可用，不伪装成成功。持续失败或越界更新仍需要诊断；自动化不能保证外部 API、
权限、托管 runner 或未来破坏性变更永不需要人工介入。协调器异常与每日兼容性检查失败
都会建立去重的、由自动化自有的 GitHub issue，仅包含运行链接，不公开日志或凭据。

人工功能发版仍可对已验证提交推送 `vX.Y.Z` tag；CI artifacts 不是正式 Release。
`registry.json` 不固定版本，新 Release 无需修改插件源。
发布不等于部署，已安装插件需在 CPA 插件商店中主动更新，不会后台覆盖运行中的动态库。
也可手动运行 `auto-release` 协调器；人工 tag 的失败发布可从主分支重试，例如
`gh workflow run release.yml --ref main -f tag=v0.1.2`，产物仍构建自该 tag。仅自动化自有、
尚未公开的草稿允许恢复；人工草稿不动，已公开资源不覆盖。

宿主升级不会更新已经编译进插件的 translator。宿主自己的池化/亲和逻辑可独立升级；
需要新的 SDK 转换逻辑时安装重新构建的插件，而不是手改转换器。
启用仓库 GitHub Actions 与 Dependabot 后，这些检查和依赖更新会自动运行。

## 界面主题

内嵌管理页复用 CPA 管理中心的主题令牌，与其保持一致的排版、圆角与配色（暖灰浅色为
默认、纯白与深色可选），页面无外部字体、无内联样式，兼容 CSP。

- 内嵌于同源页面（例如 CPA 管理中心的资源页 iframe）时，读取父页面根元素上的
  `data-theme`（`dark` / `white` / 无属性即暖灰浅色），并把父页面白名单设计令牌的
  计算值复制到本页面；父页面切换主题时会实时同步。
- 无父页面或父页面跨域时，回退到系统配色：系统深色使用深色主题，系统浅色使用纯白主题。
- 仅读取父页面根元素的主题属性与白名单设计令牌（`--bg-*`、`--text-*`、`--border-*`、
  `--primary-*`、语义色、圆角与阴影），绝不读取父页面的任何认证或凭据状态。

浏览器视觉校验脚本位于 `internal/web/browser/`：

```bash
# Playwright 为临时外部依赖，不写入本仓库
npm i playwright
NODE_PATH="$(npm root)" SCREENSHOT_DIR=/tmp/opencode-theme-shots \
  node internal/web/browser/theme-check.cjs
```

同目录的 `models.cjs` 复用同一临时 Playwright 依赖、本地路由 mock、假凭据与生产 CSP，
覆盖手动模型编辑器：校验失败不发写入、浅保存保留其它配置、有界回读确认、删除并以空
列表撤销注册、390px 布局；不连接真实 CPA 或上游：

```bash
CHROMIUM_PATH=/path/to/chromium NODE_PATH="$(npm root)" \
  node internal/web/browser/models.cjs /tmp/opencode-model-shots
```

`make test` 另行运行 `node --test internal/web/ui.test.cjs`，其中已覆盖主题桥接的纯函数、
内嵌/回退逻辑，以及手动模型的归一化规则与端点前缀。

## 安全与许可证

静态资源公开且不含密钥，密钥管理接口由 CPA 管理认证保护。UI 禁止非本机明文 HTTP
操作、避免 HTML 注入、不回显服务端错误文本，也不把密钥放进 URL、日志或浏览器存储。
不要使用已泄露的 API key；本仓库与测试不包含真实 OpenCode 凭据。

MIT；SDK 与 C ABI 参考实现的归属见 [NOTICE](NOTICE)。
