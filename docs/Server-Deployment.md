# 在服务器上常驻运行（EC2 等）

本页讲如何把 `kiro-anthropic` 作为常驻服务跑在 Linux 服务器（如 AWS EC2）上，并保证安全。

---

## 1. 安全模型（先读）

服务有两个监听端口，安全前提不同：

| 端口 | 默认 | 绑定 | 鉴权 | 说明 |
|---|---|---|---|---|
| API (`--port`) | 17890 | `127.0.0.1` | `--api-key`（默认无） | Anthropic 兼容接口，消耗账号池额度 |
| Admin (`--admin-port`) | 27890 | **始终 `127.0.0.1`** | **无** | 账号管理台（登录/删除/导入/刷新） |

两条铁律：

1. **Admin 端口无鉴权，且强制只绑回环**。它按真实 TCP 对端 IP 拒绝非回环连接，公网服务器上**只能经 SSH 隧道**访问。不要试图暴露它。
2. **API 绑非回环（如 `0.0.0.0`）时必须设 `--api-key`**，否则服务拒绝启动。这是代码层面的强制校验，防止账号池额度被公网白嫖。

---

## 2. 两种部署形态

### 方案 A：全回环 + SSH 隧道（自用，最安全，推荐）

服务全绑 `127.0.0.1`，云安全组**只开 22 端口**。

服务器：

```bash
kiro-anthropic serve --proxy none
```

工作站（转发 API + admin 两个端口）：

```bash
ssh -L 17890:localhost:17890 -L 27890:localhost:27890 <user>@<server>
```

然后本地浏览器打开 `http://localhost:27890` 登录/管理；把客户端指向 `http://localhost:17890`。

> 首次登录也走这条隧道：浏览器完成 IdC 授权后回跳到 `localhost:27890/oauth/callback`，隧道保证回调落到服务器上同一进程。

### 方案 B：API 对可信来源开放 + 强制 api-key（团队共享）

```bash
kiro-anthropic serve --host 0.0.0.0 --api-key "$(openssl rand -hex 32)" --proxy none
```

- 安全组**只放 17890 给可信来源 IP 段**，绝不 `0.0.0.0/0`。
- 强烈建议前置 TLS 反代（Caddy/Nginx + Let's Encrypt），否则 api-key 明文过网。
- Admin（27890）仍只走 SSH 隧道。
- 客户端携带 `x-api-key: <key>` 或 `Authorization: Bearer <key>`。

---

## 3. 一键安装脚本

仓库根目录的 `install.sh`（Linux + systemd）：下载最新 release → 校验 SHA-256 → 装到 `/usr/local/bin` → 建专用系统用户 → 写 systemd unit → 启动。

```bash
curl -fsSL https://raw.githubusercontent.com/YorrickBao/kiro-anthropic/main/install.sh -o install.sh

# 默认绑 127.0.0.1（配合 SSH 隧道）
sudo bash install.sh

# 对外开放（强制 key）
sudo bash install.sh --host 0.0.0.0 --api-key <key>

# 指定版本
sudo bash install.sh --version v0.4.3

# 卸载
sudo bash install.sh --uninstall
```

可用选项：`--host`、`--port`、`--admin-port`、`--proxy`、`--api-key`、`--version`、`--uninstall`、`--help`。

### 脚本涉及的文件（审计 / 手动清理）

| 路径 | 用途 | `--uninstall` 是否删除 |
|---|---|---|
| `/usr/local/bin/kiro-anthropic` | 二进制 | ✅ |
| `/etc/systemd/system/kiro-anthropic.service` | systemd unit | ✅ |
| 系统用户/组 `kiro` | 运行服务的非特权账户 | ❌（保留） |
| `/var/lib/kiro-anthropic/` | 服务 HOME | ❌（保留） |
| `/var/lib/kiro-anthropic/.kiro-anthropic/accounts.json` | 账号池（**含长期凭据**） | ❌（保留） |

`--uninstall` 刻意保留数据目录以免误删凭据；结尾会打印彻底清除命令：

```bash
sudo userdel kiro
sudo rm -rf /var/lib/kiro-anthropic
```

---

## 4. 手写 systemd unit（不用脚本时）

`/etc/systemd/system/kiro-anthropic.service`：

```ini
[Unit]
Description=kiro-anthropic
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=kiro
Environment=HOME=/var/lib/kiro-anthropic
ExecStart=/usr/local/bin/kiro-anthropic serve --proxy none
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/kiro-anthropic

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd --system --home-dir /var/lib/kiro-anthropic --create-home --shell /usr/sbin/nologin kiro
sudo systemctl daemon-reload
sudo systemctl enable --now kiro-anthropic
```

> `--proxy none`：EC2 直连 AWS/kiro.dev 通常没问题。默认代理是 `127.0.0.1:7890`（本机不存在），必须显式关掉，否则所有出站请求失败。

---

## 5. 首次上账号

Admin 登录是浏览器 OAuth 流程，无头服务器有两条路：

1. **SSH 隧道 + 本地浏览器**（方案 A 的隧道）在 `http://localhost:27890` 完成 IdC 登录。**最顺**。
2. 在本地机器先 `serve` 登录好，把 `~/.kiro-anthropic/accounts.json` 拷到服务器 `/var/lib/kiro-anthropic/.kiro-anthropic/`（注意属主 `chown kiro:kiro`）。

账号入池后，后台每 60s 自动刷新令牌保活。

---

## 6. 运维

```bash
systemctl status kiro-anthropic       # 状态
journalctl -u kiro-anthropic -f       # 实时日志
systemctl restart kiro-anthropic      # 重启

# 升级：自更新子命令（或重跑 install.sh）
sudo -u kiro /usr/local/bin/kiro-anthropic upgrade --check
sudo bash install.sh                  # 拉最新 release 重装
```

> 想要**零中断升级**（新旧实例并存、切流量、排空长流后再退旧实例），见 [蓝绿切换升级](./blue-green.md)。

### 故障排查

- **启动即退出，日志报 `--api-key is required`**：你把 `--host` 绑成了非回环地址却没给 key。要么改回 `127.0.0.1`（配合隧道），要么加 `--api-key`。
- **出站请求全失败/超时**：多半是默认代理 `127.0.0.1:7890` 不存在。加 `--proxy none` 直连。
- **API 返回 503**：账号池为空，去 admin 页登录/导入账号。
- **admin 页打不开**：确认走的是 SSH 隧道（`-L 27890:localhost:27890`）并访问 `localhost:27890`，直接访问服务器公网 IP 会被拒。
