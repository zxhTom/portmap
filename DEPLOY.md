# 部署指南（Linux）

## 原理

portmap 运行在 **内网机器 A** 上，通过 SSH 连接到 VPS（B），在 B 上建立反向端口监听。

**B（VPS）不需要安装 portmap**，只需要开启 SSH 服务。B 上需要配置：
```bash
# /etc/ssh/sshd_config
GatewayPorts yes        # 允许 0.0.0.0 监听而不是 127.0.0.1
```

然后 `systemctl restart sshd`。

---

## 1. 编译

```bash
git clone git@github.com:zxhTom/portmap.git
cd portmap
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o portmap ./cmd/portmap/
```

生成静态链接的 `portmap` 二进制文件。

## 2. 安装（内网机器 A 上）

```bash
sudo cp portmap /usr/local/bin/portmap
sudo chmod +x /usr/local/bin/portmap
sudo mkdir -p /etc/portmap /var/log/portmap
```

## 3. 配置 VPS 连接

```bash
export PORTMAP_HOME=/etc/portmap

# 启动 daemon
portmap serve --log-file=/var/log/portmap/portmap.log &

# 添加 VPS（密钥认证）
portmap remote add myvps \
  --host 你的VPS公网IP \
  --user root \
  --auth-method=key \
  --key ~/.ssh/id_rsa

# 或密码认证
portmap remote add myvps \
  --host 你的VPS公网IP \
  --user root \
  --auth-method=password \
  --password 你的密码
```

## 4. 添加映射

```bash
# 将本地 9999 映射到 VPS 的 19999
portmap add myapp -r myvps -l 19999 -p 9999 -g web

# 启动
portmap start myapp -g web

# 查看
portmap list
```

## 5. 配置开机自启

创建 systemd 服务 `/etc/systemd/system/portmap.service`：

```ini
[Unit]
Description=Portmap SSH Tunnel Manager
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/portmap serve --log-file=/var/log/portmap/portmap.log
Restart=always
RestartSec=5
Environment=PORTMAP_HOME=/etc/portmap

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now portmap
```

**daemon 重启后会自动恢复所有映射的上次运行状态**——之前 running 的会自动重建 SSH 隧道。

## 6. 验证

从外部机器访问 VPS 的公网端口：

```bash
curl http://VPS公网IP:19999
```

## 常用命令

```bash
# 添加新 VPS
portmap remote add vps2 --host 另一台VPS的IP --user admin --auth-method=key

# 删除 VPS
portmap remote delete vps2

# 添加映射（不同 VPS）
portmap add another -r vps2 -l 8080 -p 3000 -g infra

# 批量管理整组
portmap group start web
portmap group stop web

# 删除映射
portmap delete myapp -g web

# 查看所有映射状态
portmap list
```

## 故障排查

```bash
# 查看 daemon 日志
journalctl -u portmap -f
tail -f /var/log/portmap/portmap.log

# 检查映射状态
portmap list

# 测试 SSH 连通性
ssh root@VPS的IP echo ok

# 确保 VPS 上 sshd_config 配置了 GatewayPorts yes
# 否则监听在 127.0.0.1，外部无法访问
```

## 多台内网机器共享一个 VPS

每台内网机器可以独立运行 portmap，只要映射到 VPS 的不同端口即可：

```
机器 A: portmap → VPS:19999 → A:9999
机器 D: portmap → VPS:20080 → D:8080
```

## 防火墙

内网机器 A 只需要能 **出站** 访问 VPS 的 SSH 端口（默认 22），不需要开放任何入站端口。

---

## 7. Docker 映射部署说明

除了 SSH 隧道外，若需将本地未发布端口的 Docker 容器映射到本地端口：

```bash
# 启动 daemon 并指定 Docker 选项（可选）
portmap serve --docker-host="unix:///var/run/docker.sock"

# 添加容器转发
portmap add db-forward --type=docker --container=my-db --container-port=5432 -l 15432 --bind-host=127.0.0.1
portmap start db-forward
```

- **macOS / Docker Desktop 用户**：Docker Socket 路径通常为 `unix://$HOME/.docker/run/docker.sock`，可在启动 `portmap serve` 时通过 `--docker-host` 指定。
- **孤儿资源清理与自愈**：portmap 启动时自动通过 `portmap.managed` 及实例 ID 标签执行有界清理并报告失败，随后才按上次记录恢复运行状态；停止映射时会主动回滚已创建的关联资源。
- **协议说明**：Docker 转发仅支持 TCP 协议。

## 8. 自动加载配置与故障恢复

推荐直接安装仓库随附的服务模板（替代前文的简化示例）：

```bash
sudo install -m 644 deploy/portmap.service /etc/systemd/system/portmap.service
sudo systemctl daemon-reload
sudo systemctl enable --now portmap
# 已运行旧版本时，替换二进制后需要重启一次以加载新代码：
# sudo systemctl restart portmap
sudo systemctl is-enabled portmap
sudo systemctl status portmap
```

模板等待网络就绪，自动创建配置/日志目录，并在进程退出后每隔 5 秒重启。各映射的连接失败由程序内部恢复，无需等待进程退出。

程序每隔 5 秒重新读取配置，验证通过后重建变更的映射；无效或暂时缺失的配置不会中断上次有效映射。首次连接失败、SSH 断网和 Docker 转发资源异常都会持续重试。SSH 具备保活超时检测；Docker 使用容器名称配置时可跟随同名容器重建。程序只重建转发资源，不会重启业务容器。

新映射仍需手动启动一次，停止的映射不会被自动拉起。始终使用服务的数据目录操作，例如：

```bash
sudo portmap --data-dir /etc/portmap list
sudo portmap --data-dir /etc/portmap start myapp -g web
```

服务默认以 root 运行，SSH 私钥建议填写绝对路径，确保服务用户可读。公网 SSH 映射还需要 VPS 允许 TCP 转发、配置 GatewayPorts，并放行相应防火墙端口。

## 9. Multipass 映射部署

portmap 应运行在可以访问 Multipass daemon 和实例 IPv4 地址的宿主机上。服务用户需要已有的 Multipass 访问权限；portmap 不会修改权限或启停实例。

```bash
sudo portmap --data-dir /etc/portmap multipass list
sudo portmap --data-dir /etc/portmap add vm-ssh --type=multipass \
  --instance=shell-test-vm --instance-port=22 -l 2222
sudo portmap --data-dir /etc/portmap start vm-ssh
```

上述示例仅绑定宿主机 `127.0.0.1:2222`。需要直接暴露在宿主机所有网卡时，添加 `--bind-host=0.0.0.0`。需要映射到 VPS 时，添加 `-r myvps`，`-l` 改为 VPS 上的目标监听端口。

程序默认自动查找 Multipass CLI；如果服务的 PATH 中不存在 CLI 且 `/snap/bin/multipass` 也不可用，可以通过 systemd drop-in 设置实际绝对路径：

```ini
[Service]
Environment=PORTMAP_MULTIPASS_BINARY=/实际路径/multipass
```

虚拟机内目标服务需要监听实例网卡或所有网卡。多网卡实例会使用第一个有效非回环 IPv4 地址，请确认宿主机可达。实例 IP 变化后程序自动重建映射；实例离线时记录失败并等待重试，不会操作实例电源。

## 10. 更新已部署版本

在对应宿主机上准备新版二进制，再由已安装程序执行：

```bash
# 当前本机和 ds2 的用户服务部署
~/.local/bin/portmap upgrade --binary /tmp/portmap.new

# 系统级部署
sudo /usr/local/bin/portmap --data-dir /etc/portmap upgrade \
  --system --binary /tmp/portmap.new
```

更新允许短暂重连，原端口、用途说明、配置和启停意图保留；原先正常运行的映射必须通过恢复检查。候选文件会先校验可执行性、配置版本及可选 SHA-256，再原子替换。更新失败会自动还原备份并检查旧服务恢复。旧程序和配置/状态快照位于数据目录的 `updates/backup-*`，不要在更新期间同时编辑配置或启停映射。

旧版尚无 `upgrade` 命令时，用新版程序引导首次更新：

```bash
/tmp/portmap.new --data-dir ~/.config/portmap upgrade \
  --binary /tmp/portmap.new --install-path ~/.local/bin/portmap
```

此命令保持现有服务单元及开机启用状态。自定义服务通过 `--service` 指定，API 地址通过 `--api-addr` 指定。恢复时间较长可使用 `--timeout 120s`。重复安装同一正在运行的程序不会重启服务。

### Web 控制台

新版服务默认附带 `127.0.0.1:9877` 管理页面。首次账号 `admin`，独立随机密码见 `$PORTMAP_HOME/web-initial-password`（0600）；登录后可修改，哈希保存在 `web-auth.json`。不要复用 SSH/sudo 密码，不要将管理凭据提交 Git。升级不替换这些文件；重启会注销浏览器登录会话。

本机浏览器打开 `http://127.0.0.1:9877`。访问 ds2：在客户端运行 `ssh -N -L 127.0.0.1:9878:127.0.0.1:9877 -p 1822 zxhtom@47.121.182.177`，当前 ds2 已启用 HTTPS，应打开 `https://127.0.0.1:9878` 并信任其自签名证书，使用 ds2 自己的网页密码。

可通过 `serve --web-addr=` 禁用网页。公开监听必须同时提供 `--web-cert` 和 `--web-key`，避免通过公网 HTTP 发送登录密码；独立的 CLI API（默认 9876）仅供可信本地访问，不应对公网开放。网页启动失败会记录错误并保留现有隧道服务，部署后应独立确认网页可访问。

公网 Web 入口使用 portmap 的 SSH 反向隧道，将 VPS 上随机分配的可用五位数端口转发至 ds2 的 `127.0.0.1:9877`（HTTPS）。公网端口以 `portmap list` 中 `management` 分组的实际配置为准，不再固定为 9877；创建时省略 `--listen` 即自动探测分配并保存。

### 集中域名路由（当前实际部署）

当前统一后缀为 `port.toms.chat`。1822 Web 控制台提供统一设置；47 的 Nginx 与 relay 也已切换。权威 DNS 指向 47.121.182.177。正式证书待提供，当前 HTTPS 使用有效期至 2026-11-18 的临时自签名通配证书，浏览器会提示不受信任。

- 47：`/home/zxhtom/.local/bin/portmap server serve`，系统服务 `/etc/systemd/system/portmap-server.service`，运行用户 `zxhtom`，开机启用。原 `portmap-relay.service` 已停用。
- 47 数据与私有控制 socket：`/home/zxhtom/.config/portmap-relay/`，目录 0700、注册表及 socket 0600。控制通过 SSH 执行固定的 `portmap relay request` 命令，不发布公网控制端口。
- 47 Nginx：`/etc/nginx/conf.d/portmap-domains.conf`，只接管 `*.port.toms.chat`，反代到 `127.0.0.1:9881`。原有站点配置保留。
- sshd 已设置 `GatewayPorts clientspecified` 并校验后 reload；普通 TCP 映射仍显式请求 `0.0.0.0`，域名映射请求 `127.0.0.1`。无需重启 sshd 或断开原有 SSH 会话。
- 1822 的 `management/portmap-web-public` 已绑定 `portmap`，入口 `https://portmap.port.toms.chat`；内部中转端口 `18610` 仅本机可连，目标仍为 1822 的 HTTPS `127.0.0.1:9877`。
- 正式通配符证书待用户提供；临时证书在 `/etc/nginx/cert/port-toms-chat-temp/`。日后只替换本域名 Nginx 配置中的证书路径，不覆盖其他站点的证书文件。
- 域名注册没有业务数量上限；所有实例共同遵守同一份集中、原子更新的归属表。已有绑定停止或过期仍保留归属，显式解除或删除后释放。
- 更新 VPS 服务端程序时，先验证候选版本与校验值，原子替换程序，再 `sudo systemctl restart portmap-server`。注册表保留，客户端约 20 秒心跳恢复活跃路由。客户端更新仍使用 `portmap upgrade`，保留域名配置并校验能力。

此次基础配置备份位于 47 的 `/root/portmap-domains-backup-20261018-151959/`。回退前应先在客户端解除域名模式或停止域名隧道，避免在恢复旧的 GatewayPorts 策略后公开本应私有的中转端口。

2026-10-19 后缀切换备份位于 47 的 `/home/zxhtom/.config/portmap-relay/migration-20261019-095629/`，包含迁移前的域名注册表、Nginx 配置和 relay 服务单元。1822 的客户端配置通过 portmap 的更新备份保留。

### VPS 服务端模式

同一个二进制可运行 `portmap server serve`。它继承原 `relay serve` 的域名路由及数据目录，并增加只查看、禁用、重新启用隧道的 VPS 控制台。`relay serve` 仍可运行旧版域名路由；同一台 VPS 上不要同时启动两个模式，它们共用 `127.0.0.1:9881` 和私有控制 socket。

安装新二进制到 VPS 的 `/home/zxhtom/.local/bin/portmap` 后，用 `deploy/portmap-server.service` 替换运行中的 `portmap-relay.service`；保留原 `~/.config/portmap-relay` 目录及 Nginx 配置。客户端也需更新到含此功能的版本，并重启原 `portmap serve` 服务，无需修改已有远端或映射的创建命令。没有运行服务端的 VPS 继续按原来的 SSH 方式转发。2026-10-19 已在 47 及本机、1822 客户端部署；服务端当前自动收到两台客户端共 6 条在线 VPS 映射。旧二进制及 relay 服务文件备份于 47 的 `~/.config/portmap-relay/server-mode-backup-20261019/`，两台客户端的旧二进制各自保留为 `~/.local/bin/portmap.before-server-20261019`。

服务端控制台默认只监听 `127.0.0.1:9890`；公网通过独立的 Nginx 配置 `deploy/portmap-server.nginx.conf` 提供 `https://server.port.toms.chat`，HTTP 自动跳转到 HTTPS。公网 IP 的 `:9890` 不开放。从管理电脑也可通过 `ssh -N -L 127.0.0.1:9890:127.0.0.1:9890 -p 22 zxhtom@47.121.182.177` 访问 `http://127.0.0.1:9890`。初始账号为 `admin`，随机密码保存在 VPS 的 `~/.config/portmap-relay/server-web-initial-password`（0600），密码哈希保存在 `server-web-auth.json`。目前仍使用临时自签名通配符证书，浏览器会提示不受信任；换上正式证书时同步更新服务端 Nginx 配置中的证书路径。

客户端每 15 秒通过已配置的 SSH 账号执行 `portmap server request`，经 VPS 上 0600 的 Unix socket 上报该 VPS 对应的全部映射（包含已停止和失败的映射）。服务端超过 45 秒没有收到上报会显示离线；禁用策略持久化到 `disabled-tunnels.json`，客户端收到后关闭现有隧道，并在再次启动前查询策略。重新启用后客户端会按照原来的启停意图自动恢复。服务端不能代替客户端创建映射，也无法识别没有更新的 portmap 客户端或其他软件直接建立的 SSH 隧道；这些连接不会出现在清单中，也不受此禁用策略管理。
