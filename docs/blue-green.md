# 蓝绿切换升级（零中断）

本页讲如何用 nginx 反向代理做 `kiro-anthropic` 的蓝绿部署：升级时新旧两个实例并存，把新流量切到新实例，等旧实例把在途请求（含长时间的流式对话）跑完再退出，全程不中断、不切断已有连接。

适用于想「无损升级」的常驻部署。只需要单实例、能接受几百毫秒中断的，直接 `systemctl restart` 或重跑 `install.sh` 即可，不必用本方案。

---

## 1. 核心原则

**切换点是 `nginx -s reload`，不是 kill 旧进程。**

nginx 优雅 reload 时，老 worker 保留已建立的连接直到请求结束，新 worker 用新配置路由新请求。任何时刻 nginx 都只把**新连接**发给正在正常 `accept` 的实例，所以不存在「连接被拒」的窗口。

如果反过来先 kill 旧实例：`kiro-anthropic` 收到 SIGTERM 会立刻关闭监听端口，此刻 nginx 再发新连接就会 `connection refused`。而 `/v1/messages` 是 **POST（非幂等）**，nginx 默认不会把它重试到另一个后端，失败会直接返回给客户端。所以顺序必须是「先 reload 改路由，后 kill」。

> 前置条件：本方案依赖「排空到连接归零才退出」的关闭行为（收到 SIGTERM 后等所有在途请求/流结束再退，不再有固定超时硬切）。旧实例带这个行为才能无损排空长流。

---

## 2. 两个实例（blue / green）

| 角色 | API 端口 | 说明 |
|---|---|---|
| blue | 17890 | 平时活跃 |
| green | 17891 | 平时不启动（或待命），升级时启用 |

两个实例**共享同一个 accounts 文件**（同一 `HOME` 下的 `accounts.json`），即同一个账号池，这是期望行为。切换的重叠窗口里两个进程都会跑令牌刷新，可能有一次冗余刷新；写入是「临时文件 + 原子 rename」，不会损坏文件，可接受。

Admin 端口（默认 27890，强制只绑回环、无鉴权）**不要反代出去**。两个实例同时跑时，green 的 admin 端口若与 blue 冲突会自动 +1（27891），无需干预。反代只暴露 API 端口。

---

## 3. nginx 配置

```nginx
# /etc/nginx/conf.d/kiro.conf

upstream kiro_backend {
    # 带 down 的实例不接新流量。平时 blue 活跃，green 待命。
    server 127.0.0.1:17890;            # blue  (active)
    server 127.0.0.1:17891 down;       # green (standby)

    keepalive 16;                      # 复用到后端的长连接
}

server {
    listen 8080;
    server_name _;

    # 服务端最大读 32MB 请求体（内联图片），放宽上限。
    client_max_body_size 32m;

    location / {
        proxy_pass http://kiro_backend;

        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection        "";   # 配合 upstream keepalive

        # --- SSE 流式的关键 ---
        proxy_buffering     off;    # 否则 nginx 攒着不吐，流式变伪流式
        proxy_cache         off;
        proxy_read_timeout  3600s;  # 一次对话流可能持续几分钟，按最长估
        proxy_send_timeout  3600s;

        # /v1/messages 是 POST（非幂等）。保持默认不重试非幂等请求：
        # 干净切换下不会把同一次对话重复发给两个实例（避免重复生成/计费）。
        # 不要加 non_idempotent。
        proxy_next_upstream error timeout;
    }
}
```

---

## 4. 切换流程

```bash
# 1. 启动 green(新版本),监听 17891
kiro-anthropic serve --port 17891 --proxy none &

#    等它 ready 再继续
curl -sf http://127.0.0.1:17891/health >/dev/null && echo green-ready

# 2. 翻转 down 标记:blue 标 down、green 放开(改配置里那两行)
sed -i \
  -e 's|server 127.0.0.1:17890;|server 127.0.0.1:17890 down;|' \
  -e 's|server 127.0.0.1:17891 down;|server 127.0.0.1:17891;|' \
  /etc/nginx/conf.d/kiro.conf

# 3. 校验并优雅 reload:老 worker 把 blue 上的在途请求跑完,
#    新 worker 只把新请求发给 green。此刻起没有新流量进 blue。
nginx -t && nginx -s reload

# 4. 等 blue 上的存量流排空后发 SIGTERM,它会等自己那些流结束再干净退出。
#    建议先观察一段时间(确认 green 无异常)再执行这步。
kill -TERM "$(pgrep -f 'kiro-anthropic serve --port 17890')"
```

上面的 `sed` 仅为示例，生产建议把两行 `server` 维护成显式配置或用配置管理工具改。

---

## 5. 回滚

只要 blue 进程还活着，回滚就是把 down 标记翻回来再 reload，零风险：

```bash
sed -i \
  -e 's|server 127.0.0.1:17890 down;|server 127.0.0.1:17890;|' \
  -e 's|server 127.0.0.1:17891;|server 127.0.0.1:17891 down;|' \
  /etc/nginx/conf.d/kiro.conf
nginx -t && nginx -s reload
```

所以第 4 步的 kill **不要急**，确认 green 稳定后再做。

---

## 6. 配合 systemd（可选）

用模板单元跑两个实例，`%i` 作为 API 端口：

```ini
# /etc/systemd/system/kiro-anthropic@.service
[Unit]
Description=kiro-anthropic (port %i)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=kiro
Environment=HOME=/var/lib/kiro-anthropic
ExecStart=/usr/local/bin/kiro-anthropic serve --port %i --proxy none
Restart=on-failure
RestartSec=3
# 给排空留足时间:SIGTERM 后等在途流结束才退,别让 systemd 过早 SIGKILL。
TimeoutStopSec=3700
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/kiro-anthropic

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl start kiro-anthropic@17891     # 起 green
# nginx reload 切流量到 17891(见第 4 步)
sudo systemctl stop  kiro-anthropic@17890     # 排空后停 blue
```

`TimeoutStopSec` 必须大于一次对话流的最长时长，否则 systemd 会在超时后 `SIGKILL` 强杀，把排空中的流切断，失去无损的意义。

---

## 7. 注意事项小结

- 切换点在 **`nginx -s reload`**，不在 kill；顺序是「先 reload，后停旧实例」。
- SSE 必须 `proxy_buffering off` + 长 `proxy_read_timeout`。
- POST 保持默认不重试非幂等请求，**不要**加 `proxy_next_upstream ... non_idempotent`。
- 两实例共享 accounts 文件，重叠窗口有一次冗余令牌刷新，可接受，不损坏文件。
- systemd 场景下 `TimeoutStopSec` 要大于最长流时长。
- Admin 端口不反代；green 的 admin 端口冲突会自动 +1。
