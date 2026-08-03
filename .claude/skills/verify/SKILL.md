---
name: verify
description: Drive the Anthropic API against an isolated fake Kiro upstream
---

# Runtime verification

Use the public HTTP surface; do not call selector methods directly.

1. Build the server to a temp path: `go build -o /tmp/kiro-anthropic-verify .`.
2. Create a temp `accounts.json` with placeholder credentials, distinct `profileArn` values, future token expiries, and `--no-import-local`.
3. Route `--proxy` to an isolated HTTP CONNECT proxy that performs TLS interception with its own test CA, then serves the fake Kiro responses inside each tunnel:
   - `GET /getUsageLimits` with per-`profileArn` Base or Overage usage.
   - `ListAvailableModels` with `gpt-5.6-sol`.
   - `GenerateAssistantResponse` with a valid AWS event-stream content frame.
   Log only account labels derived from `profileArn`; never log Authorization values.
4. On macOS, Go may ignore `SSL_CERT_FILE`. Build a temp-only `-overlay` for `httpclient.go` whose `tls.Config.RootCAs` contains the fake upstream CA; do not alter production source or the system keychain.
5. Launch on isolated ports, wait for both Usage warmups, then POST a synchronous request to `/v1/messages` and capture both the API response and fake-upstream request order.
6. Verify Base preference, Base-failure fallback to Overage, and pre-send fencing by blocking Base model lookup, toggling policy through `POST /api/accounts/overage`, returning Base-zero active-overage Usage, then confirming no Base runtime request occurs before the Overage runtime request.

Always stop the server and fake upstream after capture. Run CI checks separately; they are not runtime evidence.
