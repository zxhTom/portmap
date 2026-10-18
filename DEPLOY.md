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
