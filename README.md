# kiro-anthropic

[![CI](https://github.com/YorrickBao/kiro-anthropic/actions/workflows/ci.yml/badge.svg)](https://github.com/YorrickBao/kiro-anthropic/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/YorrickBao/kiro-anthropic?sort=semver)](https://github.com/YorrickBao/kiro-anthropic/releases)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/github/license/YorrickBao/kiro-anthropic)](LICENSE)
[![Conventional Commits](https://img.shields.io/badge/Conventional%20Commits-1.0.0-yellow)](https://www.conventionalcommits.org/en/v1.0.0/)
[![Sponsor](https://img.shields.io/badge/sponsor-%E7%88%B1%E5%8F%91%E7%94%B5-ff69b4)](https://www.ifdian.net/item/db69bdce79e911f19e2f52540025c377)

把 **Kiro**（Amazon Q Developer / CodeWhisperer）账号代理成 **Anthropic Messages API** 的本地服务。任何兼容 Anthropic 协议的客户端（Claude Code、各类 SDK 等）都能直接指向本服务，用上 Kiro 里的 Claude（以及 DeepSeek / GLM / MiniMax / Qwen 等）模型。

单个静态二进制，内置 `upgrade` 自更新。默认监听 `127.0.0.1:17890`。

> 非官方工具。请在遵守 Kiro / AWS 服务条款的前提下使用。

---

## 功能特性

- **Anthropic Messages API** `POST /v1/messages`，支持**流式（SSE）**与**非流式**。
- **工具调用 / 函数调用**：完整的 `tools` → `tool_use` → `tool_result` 多轮闭环。
- **图片输入**：`png` / `jpeg` / `gif` / `webp`（已实测可正常识图）。支持 base64，也支持远程 `url`（下载后内联为 base64，带 SSRF 防护、15s 超时与 10MB 上限）。
- **系统提示词**：`system` 原生透传到 Kiro 的 `systemPrompt`。
- **推理 effort**：读取请求里的 `output_config.effort` / `reasoning_effort`，**未指定时默认顶格**，并按每个模型的档位自动 clamp。
- **扩展思考（extended thinking）**：模型的思考过程通过 Anthropic 原生的 `thinking` / `redacted_thinking` 内容块透传（流式下发 `thinking_delta` + `signature_delta`）。多轮对话时思考块连同 `signature` 原样回传给后端；若后端判定签名失效（`THINKING_SIGNATURE_INVALID`），自动剥离推理内容并重试一次。请求侧 `thinking: {type:"disabled"}` 会关闭思考块并把 effort 降到最低档。
- **最大输出 tokens**：按模型 schema 把调用方的 `max_tokens` 下发给 Kiro。**注意：实测 Kiro 后端不强制执行该上限**，实际输出长度由模型/effort 决定，`stop_reason` 基本不会是 `max_tokens`。该字段仅为协议兼容而发送。
- **模型能力发现** `GET /v1/models`：返回 `max_input_tokens`、`max_tokens`、`capabilities.effort`（上下文窗口、effort 档位）。
- **令牌自动刷新**：读取 Kiro 的登录缓存，快过期时用 SSO-OIDC 自动续期并写回。
- **profileArn 自动解析**：企业/IdC 账号走 `ListAvailableProfiles`，免费/社交登录用内置固定 ARN。
- **出站代理**：所有到 AWS / Kiro 的请求都走代理，默认 `http://127.0.0.1:7890`，可覆盖或关闭。
- **可选本地鉴权**：`--api-key` 要求客户端携带 key。

---

## 工作原理

```
Anthropic 客户端 ──/v1/messages──►  kiro-anthropic  ──GenerateAssistantResponse──►  runtime.<region>.kiro.dev
   (Claude Code)                    (本地 :17890)      (AWS 事件流, 走出站代理)
```

- 鉴权：读取 `~/.aws/sso/cache/kiro-auth-token.json` 及其客户端注册文件；过期时调用 SSO-OIDC `CreateToken` 刷新。
- 推理：向 `https://runtime.<region>.kiro.dev` 发送 `AmazonCodeWhispererStreamingService.GenerateAssistantResponse`，解析其 `vnd.amazon.eventstream` 响应，再翻译回 Anthropic 的 SSE / JSON。
- 模型与 profile：通过 `https://management.<region>.kiro.dev` 的 `ListAvailableModels` / `ListAvailableProfiles` 获取。

---

## 前置条件

1. 本机已安装 **Kiro** 桌面端并**完成登录**（本工具复用它的令牌缓存 `~/.aws/sso/cache/`）。
2. 能访问 AWS / `*.kiro.dev`（国内一般需要代理，见下文）。
3. 仅自行构建时需要 **Go 1.26+**。

---

## 构建

```bash
# 本机构建
go build -o kiro-anthropic .

# 跨平台打包（产物 + 校验和输出到 dist/）
./build.sh
./build.sh --help          # 查看用法、平台矩阵、环境变量
VERSION=1.0.0 ./build.sh   # 打版本号进二进制
```

默认平台矩阵：`darwin/amd64`、`darwin/arm64`、`linux/amd64`、`linux/arm64`、`linux/386`、`windows/amd64`、`windows/arm64`。

---

## 快速开始

```bash
# 启动（默认 :17890，出站默认走 http://127.0.0.1:7890）
./kiro-anthropic serve

# 查看登录/令牌/profileArn 状态
./kiro-anthropic status

# 列出账号可用模型（含上下文窗口与 effort 档位）
./kiro-anthropic models

# 检查并升级到 GitHub 最新 release（见下文“升级”一节）
./kiro-anthropic upgrade --check
./kiro-anthropic upgrade            # 交互确认后替换当前二进制
```

调用（注意：本机若设了 `http_proxy`，`curl` 访问本地服务要加 `--noproxy '*'`）：

```bash
curl --noproxy '*' http://127.0.0.1:17890/v1/messages \
  -H 'content-type: application/json' \
  -d '{
    "model": "claude-opus-4.8",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "用一句话介绍你自己"}]
  }'
```

---

## 命令

| 命令 | 说明 |
|---|---|
| `serve` | 启动 Anthropic 兼容服务（默认端口 17890） |
| `status` | 显示当前令牌、区域、过期时间、profileArn、出站代理 |
| `models` | 列出账号可用模型 ID，含上下文窗口与 effort 档位 |
| `upgrade` | 从 GitHub Release 下载并原地替换当前二进制 |
| `version` | 打印版本 |
| `help` | 帮助 |

### `serve` 参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--host` | `127.0.0.1` | 监听地址 |
| `--port` | `17890` | 监听端口（被占用时自动 +1 重试） |
| `--admin-port` | `27890` | 管理页端口，**仅限本机访问**（被占用时自动 +1 重试） |
| `--proxy` | `http://127.0.0.1:7890` | 出站代理；优先级：本参数 > `http(s)_proxy` 环境变量 > 内置默认；`none` 表示直连 |
| `--token-file` | `~/.aws/sso/cache/kiro-auth-token.json` | Kiro 令牌文件路径 |
| `--accounts-file` | `~/.kiro-anthropic/accounts.json` | 管理页自助登录的多账号凭据存储路径 |
| `--profile-arn` | 自动解析 | 显式指定 CodeWhisperer profileArn |
| `--api-key` | 空（开放） | 设置后客户端须用 `x-api-key` 或 `Authorization: Bearer` 携带 |
| `--agent-mode` | `vibe` | Kiro agent 模式 |
| `--region` | 取自令牌 | 覆盖 **SSO 区域**（用于 OIDC 令牌刷新 `oidc.<region>.amazonaws.com`） |
| `--api-region` | 取自 `--region` | 覆盖 **Kiro API 区域**（`runtime` / `management.<region>.kiro.dev`）。仅当 Q/Kiro API 与你的 IdC 不在同一区域时才需要设置（如 IdC 在 `us-east-1`，API 在 `eu-central-1`） |
| `--log` | `false` | 开启请求访问日志（输出到 stdout/当前窗口）；默认关闭 |
| `--log-file` | 空 | 把访问日志写到指定文件（隐含 `--log`）；特殊值 `stdout`/`stderr`/`none` |

访问日志为结构化单行（Go `slog`），字段含 `method` / `path` / `status` / `duration` / `bytes`（`/v1/messages` 另加 `model` / `mode`）。级别按结果分：`5xx → ERROR`、`4xx 或请求中途出错 → WARN`、其余 `INFO`；出错时附带 `error=<原因>`。

### 管理页（`--admin-port`）

`serve` 会在 API 端口之外，额外起一个**只监听 `127.0.0.1`** 的管理端口（默认 `27890`），提供账号状态页面，并支持自助登录企业账号：

```
http://127.0.0.1:27890/            # 管理页（HTML，每 10s 自动刷新）
http://127.0.0.1:27890/api/status.json     # 页面数据源（账号 / 额度 / 模型）
http://127.0.0.1:27890/api/accounts.json   # 已登录的多账号列表（脱敏）
http://127.0.0.1:27890/health      # 健康检查
```

页面展示：

- **账号**：provider、authMethod、区域、profileArn、令牌过期倒计时、账号邮箱、打码后的 access token、出站代理、是否需要 api-key。
- **多账号登录（企业 IdC）**：填入 Identity Center 的 **Start URL** 与 **Region**（可加备注）即可发起登录，见下节。已登录账号以列表展示（备注 / provider / region / profileArn / 过期状态 / 打码 token），可逐个删除。
- **额度**：账号订阅级别、剩余 / 已用 / 总额度（credits，带进度条）、重置日期、试用额度（若在生效期）、超额封顶 / 单价 / 状态，以及完整原始 `getUsageLimits` 数据。额度来自与模型列表同源的控制面 `management.<region>.kiro.dev/getUsageLimits`，服务端缓存 60s。
- **模型**：账号可用模型的 ID、名称、最大输入 / 输出 tokens、effort 档位（与 `/v1/models` 同源）。

安全说明：

- 管理端口**强制绑定 `127.0.0.1`**，且每个请求都经两道校验——按真实 TCP 对端 IP 限定回环（不信任 `X-Forwarded-For`），并校验 `Host` 头必须是 `localhost` / 回环地址（防 DNS 重绑定与跨站请求）。
- access token 只以打码形式展示，不渲染完整值。多账号存储里的 `refreshToken` / `clientSecret` **不会**通过任何 API 返回。
- **端口自增**：`--port` 与 `--admin-port` 若被占用，会自动 +1 逐个重试直到找到空闲端口（上限 65535）。启动横幅会打印两个端口的**实际**监听地址——若发生自增，请以横幅为准。

#### 多账号登录与凭据存储

管理页可直接登录**企业 IdC（IAM Identity Center）账号**，凭据由本服务自行保存，不依赖 Kiro 桌面端：

1. 在「多账号登录」填入 IdC 的 Start URL（形如 `https://your-org.awsapps.com/start`）与 Region，点「开始登录」。
2. 服务向 `oidc.<region>.amazonaws.com` 注册一个公共客户端，浏览器新窗口打开 AWS 授权页（**authorization_code + PKCE** 流程）。
3. 你在浏览器批准后，AWS 回跳到管理页的 `/oauth/callback`，服务用授权码换取 `accessToken` / `refreshToken`，自动解析 `profileArn`，并写入多账号存储。

- **存储位置**：默认 `~/.kiro-anthropic/accounts.json`（可用 `--accounts-file` 覆盖）。目录 `0700`、文件 `0600`、原子写。文件含长期凭据（`refreshToken` / `clientSecret`），请妥善保管。
- **自动续期**：`serve` 期间后台每 60s 扫描存储，对已过期或距过期不足 5 分钟的账号，用其自带的 `clientId` / `clientSecret` / `refreshToken` 经 SSO-OIDC 自动刷新并写回；失败仅记日志、下轮重试。
- **回跳与远程部署**：回调地址取自管理页请求的 Host（受回环校验保证是本机）。若服务部署在远程主机，请通过 SSH 端口转发访问管理页，使浏览器的 `localhost` 与服务端回环一致：

  ```bash
  ssh -L 27890:localhost:27890 <server>
  # 然后在本地浏览器打开 http://localhost:27890
  ```

- **导入本机现有凭据**：管理页的「导入本机凭据」按钮会读取 `--token-file`（默认 `~/.aws/sso/cache/kiro-auth-token.json`）及其客户端注册文件（`<clientIdHash>.json`，找不到时扫描同目录），把当前 Kiro 桌面端已登录的账号一键纳入多账号存储；同一凭据（clientId + refreshToken）已存在时不重复导入。
#### 多账号请求分发

`/v1/messages` 会在已存账号间做**轮询（round-robin）**负载均衡：

- **回退规则**：账号存储为空时，`/v1/messages` 走 Kiro 桌面端缓存的单账号（`--token-file`），**行为与之前完全一致**；一旦存储里有账号，请求即改为在这些账号间轮询。管理页、`/v1/models`、额度展示始终基于 `--token-file` 的主账号，不受影响。
- **流前故障转移**：某账号请求失败（鉴权失效、配额/限流、上游 5xx、网络错误）时，自动切换到下一个账号重试；请求本身的问题（如 400 参数错误）则立即返回，不会白白消耗其他账号。**注意**：故障转移只发生在流开始前——一旦 SSE 开始下发数据（响应头已发出），中途的上游错误只能如实透传，无法再切换账号。
- **轻量冷却**：失败的账号会被短暂冷却（默认 60s）跳过；全部账号都在冷却时，选用最快恢复的那个。成功一次即清除冷却。
- **保活**：无论账号是否正被分发使用，后台都会按前述规则自动刷新其令牌。
- 选号策略刻意保持简单（纯轮询 + 冷却），不做会话粘性 / 加权 / 单账号并发限制。

### 环境变量

- `HTTPS_PROXY` / `HTTP_PROXY` / `https_proxy` / `http_proxy`：出站代理（被 `--proxy` 覆盖）。
- `KIRO_DEBUG=1`：把发往 Kiro 的完整请求体打到 stderr，便于排查（不含密钥）。
- `KIRO_DEBUG_STREAM=1`：把 Kiro 返回的每一帧事件（`:event-type` 与原始 payload）打到 stderr，便于排查思考/工具流。

---

## 升级

`upgrade` 子命令从 GitHub Releases 拉取匹配当前平台（`GOOS`/`GOARCH`）的归档，校验 `checksums.txt` 的 SHA-256，解出二进制后**原地替换**正在运行的可执行文件（Windows 上先把旧 exe 重命名为 `.old`，下次启动清理）。

```bash
./kiro-anthropic upgrade --check            # 只检查是否有新版，不下载不替换
./kiro-anthropic upgrade                    # 交互确认后替换
./kiro-anthropic upgrade -y                 # 跳过确认
./kiro-anthropic upgrade --version v0.2.0   # 安装指定 tag（可降级/回滚）
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--proxy` | 同 `serve` | GitHub API / 下载的出站代理；国内访问 GitHub 一般需要 |
| `--check` | `false` | 仅检查，不下载不安装 |
| `-y`, `--yes` | `false` | 跳过确认提示 |
| `--version` | latest | 指定 release tag（如 `v0.2.0`） |
| `--allow-unverified` | `false` | 即使缺少 `checksums.txt` 或未列出该资产也继续安装（不推荐） |

说明：
- 出站代理语义与 `serve` 完全一致：`--proxy` > `http(s)_proxy` 环境变量 > 内置默认 `http://127.0.0.1:7890`。GitHub 访问不通时按需设置 `--proxy none` 或可用代理。
- 若可执行文件位于需要 root 的目录（如 `/usr/local/bin`），替换会失败，请用 `sudo` 重试或手动替换。
- 可选设置 `GITHUB_TOKEN` 环境变量以提高 API 限流额度（未鉴权时 GitHub 有 60 次/小时限制）。
- 版本比较走语义化版本（semver）。`dev` / 本地 `go build` 产物无法判定高低，会提示"开发版"并要求确认。

---

## API 端点

以下端点在 **API 端口**（`--port`，默认 17890）上。管理页端点（账号/额度/模型）在独立的**管理端口**上，见上文「管理页」一节。

### `POST /v1/messages`
Anthropic Messages API。支持 `stream: true`（SSE：`message_start` / `content_block_start` / `content_block_delta` / `content_block_stop` / `message_delta` / `message_stop`；`content_block_delta` 涵盖 `text_delta` / `thinking_delta` / `signature_delta` / `input_json_delta`）与非流式聚合响应。支持 `system`、`messages`、`tools`、`tool_result`、`image`、`thinking`、`output_config.effort` / `reasoning_effort`，以及历史消息中的 `thinking` / `redacted_thinking` 块回传。

### `GET /v1/models`
返回账号可用模型，每项包含：`id`、`type`、`display_name`、`created_at`、`max_input_tokens`、`max_tokens`、`capabilities.effort`（`supported` 及 `low/medium/high/xhigh/max`）。

### `GET /health`
健康检查。

---

## 模型与能力

用 `./kiro-anthropic models` 查看你账号实际可用的模型。示例（取决于账号/套餐）：

| 模型 | 上下文（输入） | 最大输出 | effort |
|---|---|---|---|
| `claude-opus-4.8` | 1M | 128K | low/medium/high/xhigh/max |
| `claude-sonnet-5` | 1M | 64K | low/medium/high/xhigh/max |
| `claude-opus-4.6` | 1M | 64K | low/medium/high/max |
| `claude-sonnet-4.5` | 200K | 64K | 不支持 |
| `claude-haiku-4.5` | 200K | 64K | 不支持 |
| `auto` | 1M | 64K | 不支持 |

> 还可能包含 `deepseek-3.2`、`glm-5`、`minimax-m2.x`、`qwen3-coder-next` 等。

**模型名映射**：可直接用上面的 ID；也接受常见 Anthropic 名称（含 `opus` / `sonnet` / `haiku` 关键字会自动映射；未知名默认 `auto`）。

### effort（推理强度）
- 来源：请求体的 `output_config.effort`（Anthropic 原生字段）优先，其次 `reasoning_effort`（OpenAI 风格别名）。
- 档位：`low` / `medium` / `high` / `xhigh` / `max`（具体可用档位随模型而定）。
- **默认**：请求未指定时按该模型的**最高档**下发；请求指定了就用请求值，并 clamp 到该模型的可用档位。
- 不支持 effort 的模型（如 `claude-sonnet-4.5`、`auto`）不会下发该字段。

### 最大输出 tokens
- 调用方的 `max_tokens` 会按模型 schema（`[min, max]`，例如 opus-4.8 为 `[1024, 128000]`）clamp 后下发到 Kiro 的 `additionalModelRequestFields`。
- **不强制执行**：实测 Kiro 后端忽略该上限——即便下发 `max_tokens=1024`，模型仍会按需输出到自然结束（`stop_reason=end_turn`）。因此不要依赖 `max_tokens` 截断输出，`stop_reason` 也基本不会是 `max_tokens`。
- 不支持该字段的模型不下发。

### 扩展思考（extended thinking / reasoning）
- Kiro 后端以 `reasoningContentEvent` 流式下发思考内容（先 `text` 分片，末尾一帧 `signature`）。本服务将其翻译为 Anthropic 的思考内容块：
  - 流式：`content_block_start {type:"thinking"}` → 若干 `thinking_delta` → 一个 `signature_delta` → `content_block_stop`，然后才是正文 `text` 块。
  - 非流式：`content` 数组里包含 `{"type":"thinking","thinking":"...","signature":"..."}`。
  - 被后端加密的推理映射为 `redacted_thinking` 块（`data` 字段）。
- **默认开启**：只要模型产出思考内容就透传（思考块始终排在正文之前）。请求带 `thinking: {"type":"disabled"}` 时不下发思考块，并把 effort 降到该模型最低档；`thinking: {"type":"enabled", "budget_tokens": N}` 按开启处理（`budget_tokens` 会被接收但不映射为 effort 档位，Kiro 用的是 effort 而非 token 预算）。
- **多轮回传**：客户端把上一轮的 `thinking` / `redacted_thinking` 块（含 `signature`）放进 `messages` 历史时，会原样回传给后端（`assistantResponseMessage.reasoningContent`），以保持思考链连续。若后端返回 `THINKING_SIGNATURE_INVALID`（签名失效，常见于上下文压缩后），会自动剥离历史里的推理内容并重试一次（仅对流开始前的 400 校验错误生效）。
  - **每个 assistant 轮次仅回传首个思考块**：Kiro 的 `reasoningContent` 是单成员 union，一轮只能携带一个 `reasoningText` 或 `redactedContent`。若某个 assistant 消息里有多个思考块（如交错思考 + 多次工具调用），只回传第一个，其余思考块会被丢弃（其正文 `text` 仍保留）。

### 图片
- 支持 user 消息中的图片：`png` / `jpeg` / `gif` / `webp`。
- Anthropic 的 `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"..."}}` 会映射为 Kiro 的 `images:[{"format":"png","source":{"bytes":"..."}}]`。
- **远程 URL 图片**：`{"type":"image","source":{"type":"url","url":"https://..."}}` 会在翻译前被下载并内联成 base64（Kiro 只接受内联字节）。下载走与其他出站请求相同的代理，并有多重护栏：仅允许 `http`/`https`、拒绝解析到环回/内网/链路本地地址的主机（SSRF 防护）、单张 15s 超时、10MB 上限、按响应 `Content-Type` 校验为上述四种图片类型之一。任一护栏不通过时该图片被跳过（下游留 `[unsupported image omitted]` 提示），不会中断整个请求。

---

## 与 Claude Code 集成

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:17890
export ANTHROPIC_API_KEY=dummy                     # 服务默认开放，但 Claude Code 需要有个 key
export ANTHROPIC_MODEL=claude-opus-4.8             # 主模型
export ANTHROPIC_SMALL_FAST_MODEL=claude-haiku-4.5 # 后台/小任务模型
export no_proxy=127.0.0.1,localhost                # 关键：让本地请求绕开代理
export NO_PROXY=127.0.0.1,localhost
```

设置了 `--api-key` 时，把 `ANTHROPIC_API_KEY` 设成同一个值即可。

---

## 代理说明

- 只有**出站**（本工具 → AWS / Kiro）走代理；本地监听（客户端 → `:17890`）不经过代理。
- 优先级：`--proxy` > `http(s)_proxy` 环境变量 > 内置默认 `http://127.0.0.1:7890`。
- 直连：`--proxy none`。
- 因此本机若全局设了 `http_proxy`，用 `curl` / 客户端访问本地服务时记得设 `no_proxy=127.0.0.1,localhost` 或 `curl --noproxy '*'`，否则本地请求会被塞进代理。

---

## 区域说明

本工具区分两个区域，默认相等、绝大多数账号无需关心：

- **SSO 区域**（`--region`）：用于 OIDC 令牌刷新 `oidc.<region>.amazonaws.com`。默认取自令牌里的 `region`。
- **API 区域**（`--api-region`）：用于 Kiro 的 `runtime.<region>.kiro.dev`（推理）与 `management.<region>.kiro.dev`（模型/profile/额度）。默认跟随 `--region`。

只有**企业 IdC 账号的 Identity Center 与 Q/Kiro API 不在同一区域**时才需要分别设置——例如 IdC 在 `us-east-1`，而 API 只在 `eu-central-1` 提供：

```bash
./kiro-anthropic serve --region us-east-1 --api-region eu-central-1
```

此时 `status` 会额外打印一行 `api region`。不设 `--api-region` 时行为与旧版完全一致。

---

## 限制

- **网络搜索不支持**：Anthropic 的 `web_search` 是服务端工具，而 Kiro 的 web 搜索是客户端工具、runtime 无对应服务端接口，无法直接映射（客户端会报 “web search not supported”）。
- **图片**：支持 base64 与远程 `url`（URL 会下载后内联，受 SSRF/超时/大小/类型护栏约束，不通过则跳过并留 `[unsupported image omitted]` 提示）；`tool_result` 里的图片不转发。
- **采样参数**：`temperature` / `top_p` / `top_k` 不透传（Opus 4.7+ 本身也不支持）。
- **usage 为估算**：返回的 `input_tokens` / `output_tokens` 是基于字符数的粗略估算（非精确计费值）。Kiro 后端只提供上下文占用百分比与 credit 计费，不提供真实 token 计数，故无法返回精确值。
- **stop_reason**：取自 Kiro 结束帧（`metadataEvent.stopReason`）的权威值（`END_TURN`/`TOOL_USE`/`MAX_TOKENS` 等），映射为 Anthropic 的 `end_turn`/`tool_use`/`max_tokens`。由于后端不强制 `max_tokens`，`max_tokens` 实际极少出现。
- **工具调用标记泄漏**：个别模型（实测 `deepseek-3.2`）偶尔把工具调用的**起始标记**（`<｜DSML｜function_calls`、`<function_calls>`）当普通文本混进正文尾部，而真正的工具调用仍以结构化事件正常返回。本服务会自动剥离正文末尾这类残留标记（含跨帧拆分的情况），工具调用不受影响。仅做尾部标记剥离，不解析泄漏的 XML、不合成工具调用，因此不会误伤正文里合法出现的这类文本。

---

## 目录结构

```
main.go          CLI 入口（cobra）、参数、代理解析、启动
httpclient.go    出站 HTTP 客户端（代理感知）
token.go         令牌加载/刷新（SSO-OIDC）、profileArn 解析
eventstream.go   AWS vnd.amazon.eventstream 解码（基于 smithy-go）
kiro.go          Kiro 运行时客户端、请求/响应类型、模型列表、账号额度（getUsageLimits）
anthropic.go     Anthropic 类型、模型映射、请求翻译、SSE 组装
server.go        HTTP 服务与各端点、结构化访问日志（slog）
admin.go         管理页（仅本机）：路由、回环 + Host 校验、账号/额度/模型聚合 JSON、登录/账号端点
admin.html       管理页前端（内嵌，go:embed）
login.go         企业 IdC 登录（authorization_code + PKCE）、账号令牌刷新
accounts.go      多账号凭据存储（JSON 持久化、原子写、脱敏视图）
refresher.go     后台定期刷新已存账号的令牌
listen.go        监听辅助：端口被占用时自动 +1 重试
upgrade.go       从 GitHub Release 自更新（minio/selfupdate + semver）
build.sh         跨平台构建脚本
```

---

## 😸 觉得好用？

那就赏我一杯咖啡吧 ☕

你的支持 = 我的多巴胺 = 更多奇怪的想法变成代码。

<table>
  <tr>
    <td align="center">
      <img src=".github/sponsor-qr.png" width="200" alt="爱发电打赏二维码" /><br/>
      <sub>微信 / 支付宝 扫码打赏</sub>
    </td>
    <td align="center" valign="middle">
      <a href="https://www.ifdian.net/item/db69bdce79e911f19e2f52540025c377"><strong>爱发电 打赏页 →</strong></a>
    </td>
  </tr>
</table>
