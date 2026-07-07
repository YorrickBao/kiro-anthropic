# AGENTS.md

给在本仓库工作的 AI agent 的项目指令。请在开始编码前阅读。

## 项目简介

`kiro-anthropic` 是一个本地 HTTP 服务，把 Kiro（Amazon Q Developer / CodeWhisperer）账号通过 **Anthropic Messages API** 协议暴露出来，使任何 Anthropic 兼容客户端都能调用 Kiro 的 Claude 模型。纯 Go 项目（见 `go.mod`，无 Node 依赖）。

主要源码：`main.go`（入口/CLI）、`server.go`（HTTP 服务）、`anthropic.go`（Anthropic 协议）、`kiro.go`（Kiro 后端）、`token.go`（鉴权/令牌）、`eventstream.go`（流式）、`httpclient.go`、`util.go`，各文件有对应的 `*_test.go`。

## 构建与测试

```bash
go test ./...                 # 跑全部测试（提交前必做）
go build -o kiro-anthropic .  # 本地单平台构建
./build.sh                    # 跨平台构建 + 打包 + 校验和（发版用）
VERSION=1.2.3 ./build.sh      # 指定版本号构建
```

- 修改代码后至少跑一次 `go test ./...`；`build.sh` 默认也会先跑测试作为门禁。
- 遵循标准 Go 风格：`gofmt`/`go vet` 干净；导出符号写 doc comment（与现有代码一致，英文注释）。
- 文本文件统一 LF 换行（见 `.gitattributes`）。

## 版本号

不要在代码里手改版本号。`main.go` 的 `version` 只是兜底默认值，真实版本由 `build.sh` 在编译期通过 `-ldflags -X main.version=...` 注入，来源是 git tag。**版本的唯一真实来源是 git tag。**

## 发版流程

打一个 `v` 开头的 tag 并推送即可，其余自动完成：

```bash
git tag v1.2.3
git push origin v1.2.3
```

GitHub Actions（`.github/workflows/release.yml`）会：跑 `build.sh` → 用 git-cliff（`cliff.toml`）按 commit 类型生成分类 Release Notes → 发布到 GitHub Releases。也可在 Actions 页面手动触发（workflow_dispatch）。

## 提交规范（重要）

本仓库使用 **Conventional Commits**。Release Notes 由 git-cliff 依据 commit 前缀自动分组，所以每条 commit 必须规范，否则不会出现在 changelog 里。

格式：

```
<type>(<scope>): <subject>

[可选正文，解释 why]

[可选页脚，如 BREAKING CHANGE / Closes #123]
```

- **type**：`feat` `fix` `perf` `refactor` `docs` `test` `build` `ci` `chore` `style` `revert`
- **scope**（可选）：按模块，如 `anthropic` `kiro` `server` `token` `eventstream` `ci`
- **subject**：祈使句、现在时、小写开头、结尾不加句号，尽量 ≤ 50 字符
- **破坏性变更**：type 后加 `!`（如 `feat!:`），或在页脚写 `BREAKING CHANGE: 说明`
- 一次 commit 只做一件事，粒度别太大

示例：

```
feat(anthropic): support extended thinking end to end
fix(server): map upstream rate limit to 429 instead of 500
docs(readme): document proxy configuration
chore(ci): add release workflow
feat(kiro)!: rename --token flag to --credentials
```

避免：`update code`、`fixed bug`、`WIP`、以及缺少 type 前缀的标题。
