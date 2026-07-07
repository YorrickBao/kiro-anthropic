# kiro-anthropic

[![CI](https://github.com/YorrickBao/kiro-anthropic/actions/workflows/ci.yml/badge.svg)](https://github.com/YorrickBao/kiro-anthropic/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/YorrickBao/kiro-anthropic?sort=semver)](https://github.com/YorrickBao/kiro-anthropic/releases)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![Go Report Card](https://goreportcard.com/badge/github.com/YorrickBao/kiro-anthropic)](https://goreportcard.com/report/github.com/YorrickBao/kiro-anthropic)
[![License: MIT](https://img.shields.io/github/license/YorrickBao/kiro-anthropic)](LICENSE)
[![Conventional Commits](https://img.shields.io/badge/Conventional%20Commits-1.0.0-yellow)](https://www.conventionalcommits.org/en/v1.0.0/)

把 **Kiro**（Amazon Q Developer / CodeWhisperer）账号代理成 **Anthropic Messages API** 的本地服务。任何兼容 Anthropic 协议的客户端（Claude Code、各类 SDK 等）都能直接指向本服务，用上 Kiro 里的 Claude（以及 DeepSeek / GLM / MiniMax / Qwen 等）模型。

零第三方依赖，单个静态二进制。默认监听 `127.0.0.1:17890`。

> 非官方工具。请在遵守 Kiro / AWS 服务条款的前提下使用。

---

## 功能特性

- **Anthropic Messages API** `POST /v1/messages`，支持**流式（SSE）**与**非流式**。
- **工具调用 / 函数调用**：完整的 `tools` → `tool_use` → `tool_result` 多轮闭环。
- **图片输入**：base64 的 `png` / `jpeg` / `gif` / `webp`（已实测可正常识图）。
- **系统提示词**：`system` 原生透传到 Kiro 的 `systemPrompt`。
- **推理 effort**：读取请求里的 `output_config.effort` / `reasoning_effort`，**未指定时默认顶格**，并按每个模型的档位自动 clamp。
- **扩展思考（extended thinking）**：模型的思考过程通过 Anthropic 原生的 `thinking` / `redacted_thinking` 内容块透传（流式下发 `thinking_delta` + `signature_delta`）。多轮对话时思考块连同 `signature` 原样回传给后端；若后端判定签名失效（`THINKING_SIGNATURE_INVALID`），自动剥离推理内容并重试一次。请求侧 `thinking: {type:"disabled"}` 会关闭思考块并把 effort 降到最低档。
- **最大输出 tokens**：尊重调用方的 `max_tokens`，按模型的 `[min, max]` 范围 clamp，**未指定时默认顶格**。
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
| `version` | 打印版本 |
| `help` | 帮助 |

### `serve` 参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--host` | `127.0.0.1` | 监听地址 |
| `--port` | `17890` | 监听端口 |
| `--proxy` | `http://127.0.0.1:7890` | 出站代理；优先级：本参数 > `http(s)_proxy` 环境变量 > 内置默认；`none` 表示直连 |
| `--token-file` | `~/.aws/sso/cache/kiro-auth-token.json` | Kiro 令牌文件路径 |
| `--profile-arn` | 自动解析 | 显式指定 CodeWhisperer profileArn |
| `--api-key` | 空（开放） | 设置后客户端须用 `x-api-key` 或 `Authorization: Bearer` 携带 |
| `--agent-mode` | `vibe` | Kiro agent 模式 |
| `--region` | 取自令牌 | 覆盖区域 |

### 环境变量

- `HTTPS_PROXY` / `HTTP_PROXY` / `https_proxy` / `http_proxy`：出站代理（被 `--proxy` 覆盖）。
- `KIRO_DEBUG=1`：把发往 Kiro 的完整请求体打到 stderr，便于排查（不含密钥）。
- `KIRO_DEBUG_STREAM=1`：把 Kiro 返回的每一帧事件（`:event-type` 与原始 payload）打到 stderr，便于排查思考/工具流。

---

## API 端点

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
- 尊重调用方的 `max_tokens`，并 clamp 到模型的 `[min, max]`（例如 opus-4.8 为 `[1024, 128000]`）。
- **默认**：请求未指定时按模型上限下发。
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
- 支持 user 消息中的 base64 图片：`png` / `jpeg` / `gif` / `webp`。
- Anthropic 的 `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"..."}}` 会映射为 Kiro 的 `images:[{"format":"png","source":{"bytes":"..."}}]`。

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

## 限制

- **网络搜索不支持**：Anthropic 的 `web_search` 是服务端工具，而 Kiro 的 web 搜索是客户端工具、runtime 无对应服务端接口，无法直接映射（客户端会报 “web search not supported”）。
- **图片仅 base64**：`source.type: "url"` 的图片会被跳过（留 `[unsupported image omitted]` 提示）；`tool_result` 里的图片不转发。
- **采样参数**：`temperature` / `top_p` / `top_k` 不透传（Opus 4.7+ 本身也不支持）。
- **usage 为估算**：返回的 `input_tokens` / `output_tokens` 是基于字符数的粗略估算，非精确计费值。

---

## 目录结构

```
main.go          CLI 入口、参数、代理解析、启动
httpclient.go    出站 HTTP 客户端（代理感知）
token.go         令牌加载/刷新（SSO-OIDC）、profileArn 解析
eventstream.go   AWS vnd.amazon.eventstream 解码器
kiro.go          Kiro 运行时客户端、请求/响应类型、模型列表
anthropic.go     Anthropic 类型、模型映射、请求翻译、SSE 组装
server.go        HTTP 服务与各端点
build.sh         跨平台构建脚本
```
