# 部署指南（Linux）

## 环境要求

- Linux 服务器，Go 1.25+（编译机器）或预编译的二进制文件
- Systemd（大多数现代 Linux 发行版）

## 1. 编译

在编译机器上执行（可以是目标服务器，也可以是架构相同的其他机器）：

```bash
git clone <仓库地址> portmap
cd portmap
CGO_ENABLED=0 go build -ldflags="-s -w" -o portmap ./cmd/portmap/
```

生成静态链接的 `portmap` 二进制文件（约 6 MB）。

如果交叉编译，设置 `GOOS` 和 `GOARCH`：
```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o portmap ./cmd/portmap/
```

## 2. 安装二进制

```bash
sudo cp portmap /usr/local/bin/portmap
sudo chmod +x /usr/local/bin/portmap
```

## 3. 创建目录

```bash
sudo mkdir -p /etc/portmap
sudo mkdir -p /var/log/portmap
sudo chown nobody:nogroup /var/log/portmap
```

## 4. 安装 systemd 服务

```bash
sudo cp deploy/portmap.service /etc/systemd/system/portmap.service
sudo systemctl daemon-reload
```

检查服务文件内容：
```bash
cat /etc/systemd/system/portmap.service
```

预期内容：
```ini
[Unit]
Description=Portmap - 端口映射管理器
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/portmap serve --log-file=/var/log/portmap/portmap.log
Restart=always
RestartSec=5
User=nobody
Group=nogroup
Environment=PORTMAP_HOME=/etc/portmap

[Install]
WantedBy=multi-user.target
```

### 安全说明

服务默认以 `nobody:nogroup` 用户运行，该用户需要：
- `$PORTMAP_HOME` 目录的读写权限（配置和状态文件）
- 日志文件路径的写入权限
- 绑定所配置端口的权限

绑定 1024 以下端口需要 `CAP_NET_BIND_SERVICE` 或 root 权限。如需使用低端口，在 service 文件中添加：
```ini
[Service]
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
```

或改为 root 用户（不推荐）：
```ini
User=root
```

## 5. 配置 portmap

编辑 `/etc/portmap/config.json`：

```json
{
  "bind_addr": "0.0.0.0",
  "port_range": {
    "start": 20000,
    "end": 30000
  },
  "groups": []
}
```

也可以在服务启动后通过 CLI 交互式添加映射（见下文）。

## 6. 启动服务

```bash
sudo systemctl enable portmap
sudo systemctl start portmap
sudo systemctl status portmap
```

查看日志：
```bash
sudo journalctl -u portmap -f
# 或
sudo tail -f /var/log/portmap/portmap.log
```

## 7. 添加端口映射

Daemon 启动后，使用 CLI 管理映射：

```bash
# 指向 daemon 的 API
export PORTMAP_HOME=/etc/portmap

# 添加映射：公网端口 20000 → 内网 10.0.0.5:8080
sudo -E portmap add my-api --target-host 10.0.0.5 --target-port 8080 -g web

# 启动
sudo -E portmap start my-api -g web

# 查看
sudo -E portmap list
```

由于 daemon 以 `nobody` 身份运行，CLI 也需要对 `$PORTMAP_HOME` 有读写权限，或纯通过 API 通信。使用 `sudo -E` 可以确保权限。

如果希望管理员用户直接操作，可以改变数据目录权限：
```bash
sudo chown -R youruser:youruser /etc/portmap
```

然后修改 service 文件中的 `User` 为对应用户，或继续使用 `sudo`。

## 8. 防火墙

确保映射的端口在防火墙中开放：

```bash
# ufw
sudo ufw allow 20000:30000/tcp

# iptables
sudo iptables -A INPUT -p tcp --dport 20000:30000 -j ACCEPT

# firewalld
sudo firewall-cmd --permanent --add-port=20000-30000/tcp
sudo firewall-cmd --reload
```

## 9. 验证

从外部机器测试：

```bash
# 测试映射端口是否可达
curl http://<服务器公网IP>:20000

# 或检查端口是否开放
nc -zv <服务器公网IP> 20000
```

## 10. 常用管理命令

```bash
# 查看所有映射
sudo -E portmap list

# 添加指定端口的映射
sudo -E portmap add grafana --target-host 10.0.0.10 --target-port 3000 -p 23000 -g infra

# 更新目标
sudo -E portmap update grafana -g infra --target-port 3001

# 启动/停止单条映射
sudo -E portmap start grafana -g infra
sudo -E portmap stop grafana -g infra

# 启动/停止整组
sudo -E portmap group start web
sudo -E portmap group stop web

# 删除映射（会先停止）
sudo -E portmap delete grafana -g infra
```

## 升级

```bash
# 停止服务
sudo systemctl stop portmap

# 替换二进制
sudo cp portmap /usr/local/bin/portmap

# 启动服务
sudo systemctl start portmap

# 映射状态从 state.json 自动恢复
```

## 故障排查

### 服务启动失败

```bash
sudo journalctl -u portmap -n 50 --no-pager
```

常见原因：
- **端口被占用**：检查 `ss -tlnp | grep <端口>`
- **数据目录权限不足**：`sudo chown nobody:nogroup /etc/portmap`
- **绑定地址已被使用**：其他进程或残留的 portmap 实例占用了端口

### CLI 无法连接 daemon

```bash
# 检查 daemon 是否在运行
sudo systemctl status portmap

# 检查 API 是否在监听
ss -tlnp | grep 9876

# 确认 PORTMAP_HOME 与 daemon 配置一致
echo $PORTMAP_HOME
```

### 重启后映射状态丢失

确保 `nobody` 对 `$PORTMAP_HOME/state.json` 有写权限：
```bash
sudo chown nobody:nogroup /etc/portmap/state.json
sudo chmod 600 /etc/portmap/state.json
```

### 绑定低端口（<1024）

在 service 文件中添加：
```ini
[Service]
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
```

然后：
```bash
sudo systemctl daemon-reload
sudo systemctl restart portmap
```

---

# Relay 隧道部署（内网穿透）

适用于 **ABC 三机场景**：A（内网，无公网 IP）通过隧道连接到 B（VPS，有公网 IP），C（外部客户端）通过 B 访问 A。

## 架构

```
内网机器 A                   VPS B（公网）             外部客户端 C
┌──────────────┐   隧道      ┌──────────────┐        ┌──────────┐
│ portmap      │──出站连接──→│ portmap serve │        │          │
│ connect      │            │  --relay     │←─TCP──│ 客户端   │
│              │            │  :20000      │        │          │
│ 目标:8080    │            └──────────────┘        └──────────┘
└──────────────┘
```

- **A** 主动向 B 发起出站隧道连接（仅需 A→B 方向可达）
- **B** 监听公网端口，通过隧道将流量转发到 A
- **C** 像访问普通服务一样连接 B 的公网端口

## VPS（B）部署

与标准部署相同，但加上 `--relay` 参数：

```bash
# 编译并安装（同标准部署步骤）
sudo cp portmap /usr/local/bin/portmap
sudo mkdir -p /etc/portmap /var/log/portmap
sudo chown nobody:nogroup /var/log/portmap
```

### 带 relay 的 service 文件

`/etc/systemd/system/portmap.service`：

```ini
[Unit]
Description=Portmap Relay 服务器
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/portmap serve --relay --log-file=/var/log/portmap/portmap.log
Restart=always
RestartSec=5
User=nobody
Group=nogroup
Environment=PORTMAP_HOME=/etc/portmap

# 如需绑定 1024 以下端口（可选）
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

启用并启动：
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now portmap
```

Relay 服务器默认监听 7000 端口（可在 `config.json` 的 `relay.listen_addr` 中配置）。确保内网机器 A 能访问此端口：

```bash
sudo ufw allow 7000/tcp
```

### 配置 Relay 认证（可选）

编辑 `/etc/portmap/config.json` 添加认证用户：

```json
{
  "relay": {
    "enabled": true,
    "listen_addr": "0.0.0.0:7000",
    "users": [
      {"user": "admin", "password": "your_password"}
    ]
  }
}
```

重启使配置生效：
```bash
sudo systemctl restart portmap
```

客户端连接时需提供用户名密码：
```bash
./portmap connect --relay-addr <vps公网IP>:7000 -u admin -P your_password
```

不配置 `users` 则无需认证。

### 在 VPS 上添加映射

```bash
sudo -E portmap add my-app --target-host <任意IP> --target-port 8080 -g infra
sudo -E portmap start my-app -g infra
```

注意：此处的 `--target-host` 和 `--target-port` 仅用于列表展示。实际目标由机器 A 上的隧道客户端决定。

## 内网机器（A）部署

内网机器只需要 `portmap` 二进制文件，运行 `portmap connect` 即可。

### 方式一：命令行启动（手动）

```bash
# 无需认证
./portmap connect --relay-addr <vps公网IP>:7000

# 需要认证
./portmap connect --relay-addr <vps公网IP>:7000 -u admin -P your_password
```

此方式从服务端 config.json 读取映射配置并建立隧道。

### 方式二：tunnel.json 配置（显式指定）

创建 `~/.config/portmap/tunnel.json`：

```json
{
  "relay_addr": "vps公网IP:7000",
  "user": "admin",
  "password": "your_password",
  "tunnels": [
    {
      "mapping_id": "infra/my-app",
      "target_host": "127.0.0.1",
      "target_port": 8080
    }
  ]
}
```

然后运行：
```bash
./portmap connect
```

### 方式三：systemd 服务实现开机自启

`/etc/systemd/system/portmap-tunnel.service`：

```ini
[Unit]
Description=Portmap 隧道客户端
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/portmap connect --relay-addr <vps公网IP>:7000 -u admin -P your_password
Restart=always
RestartSec=5
User=nobody
Group=nogroup
Environment=PORTMAP_HOME=/etc/portmap

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now portmap-tunnel
```

隧道客户端在连接断开后会自动重连。

## 验证

在 VPS（B）上：
```bash
sudo -E portmap list
```

应显示：
```
GROUP  NAME     STATUS     LOCAL_PORT  TARGET
infra  my-app   connected  20000       127.0.0.1:8080
```

状态 `connected` 表示隧道已激活。

从外部客户端（C）测试：
```bash
curl http://<vps公网IP>:20000
```

## 防火墙总览

| 机器      | 端口     | 方向    | 用途               |
|----------|---------|---------|-------------------|
| VPS B    | 20000+  | 入站    | 对外映射端口        |
| VPS B    | 7000    | 入站    | A 发起的 relay 隧道 |
| VPS B    | 9876    | 本地    | 管理 API           |
| 机器 A   | 任意    | 出站    | 连接到 B:7000      |
