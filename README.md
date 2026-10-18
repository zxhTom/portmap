# portmap

SSH 反向隧道管理器。在 A（内网机器）上运行，通过 SSH 连接 B（VPS），将 A 的端口映射到 B 的公网端口。

**B 不需要安装任何东西**，只要有 SSH 服务即可。和 `ssh -NRf` 一样简单。

## 架构

```
内网机器 A                           VPS B (有 SSH 即可)
┌─────────────┐          SSH         ┌──────────────┐
│ portmap     │─── 反向隧道 ───────→│ sshd          │
│ serve       │  A:9999 → B:19999   │ 监听 :19999   │
└─────────────┘                      └──────┬───────┘
                                            │
                                      外部 C 访问 :19999
```

portmap 做的事情：`ssh -NRf 19999:127.0.0.1:9999 user@vps`，但多了配置管理、分组、状态持久化和开机自启。

## 快速开始

```bash
# 编译
go build -o portmap ./cmd/portmap/

# 启动 daemon
./portmap serve

# 1. 添加远程 VPS
./portmap remote add myvps --host 1.2.3.4 --user root --auth-method=key --key=~/.ssh/id_rsa

# 如果使用密码认证
./portmap remote add myvps --host 1.2.3.4 --user root --auth-method=password --password=mypass

# 2. 添加映射：A 的 9999 → VPS 的 19999
./portmap add myapp -r myvps -l 19999 -p 9999 -g web

# 3. 启动
./portmap start myapp -g web

# 4. 查看
./portmap list
```

外部访问 `http://1.2.3.4:19999`，流量通过 SSH 隧道到达 A:9999。

## CLI 参考

```
PORTMAP_HOME    数据目录，默认 ~/.config/portmap
--data-dir      覆盖数据目录（全局参数）
--api-addr      API 地址，默认 127.0.0.1:9876（全局参数）
```

### serve

启动 daemon。

```bash
portmap serve [--log-file /var/log/portmap.log]
```

### remote add / delete

管理远程 VPS。

```bash
# 密钥认证
portmap remote add <名称> --host <IP> --user root --auth-method=key --key=~/.ssh/id_rsa

# 密码认证
portmap remote add <名称> --host <IP> --user root --auth-method=password --password=xxx

# 删除
portmap remote delete <名称>

参数:
  --host            VPS 地址（必填）
  --port            SSH 端口（默认 22）
  -u, --user        SSH 用户名（默认 root）
  --auth-method     认证方式：password 或 key（默认 key）
  --password        SSH 密码
  --key             SSH 私钥路径（默认 ~/.ssh/id_rsa）
  --passphrase      私钥密码
```

### add / delete

管理端口映射。

```bash
portmap add <名称> -r <remote> -l <VPS端口> -p <本地端口> [-g <分组>] [--local-host <目标IP>]

参数:
  -r, --remote      远程 VPS 名称（必填）
  -l, --listen      VPS 上监听的端口（必填）
  -p, --local-port  本地目标端口（必填）
  -g, --group       分组名称（默认 default）
  --local-host      本地目标 IP（默认 127.0.0.1）

# 删除
portmap delete <名称> -g <分组>
```

### list / start / stop

```bash
portmap list                           # 查看所有映射
portmap start <名称> -g <分组>         # 启动单条
portmap stop <名称> -g <分组>          # 停止单条
portmap group start <分组名>           # 启动整组
portmap group stop <分组名>            # 停止整组
```

## API

| 方法   | 路径                          | 说明         |
|--------|-------------------------------|-------------|
| POST   | /api/remotes                  | 添加 remote  |
| DELETE | /api/remotes/:name            | 删除 remote  |
| GET    | /api/mappings                 | 查看所有映射  |
| POST   | /api/mappings                 | 添加映射     |
| DELETE | /api/mappings/:name?group=g   | 删除映射     |
| POST   | /api/mappings/:name/start?group=g | 启动     |
| POST   | /api/mappings/:name/stop?group=g  | 停止     |
| POST   | /api/groups/:name/start       | 启动分组     |
| POST   | /api/groups/:name/stop        | 停止分组     |

## 配置文件

`$PORTMAP_HOME/config.json`（默认 `~/.config/portmap/config.json`）：

```json
{
  "remotes": [
    {
      "name": "myvps",
      "host": "1.2.3.4",
      "port": 22,
      "user": "root",
      "auth": {
        "method": "key",
        "key_path": "~/.ssh/id_rsa"
      }
    },
    {
      "name": "another",
      "host": "5.6.7.8",
      "port": 2222,
      "user": "admin",
      "auth": {
        "method": "password",
        "password": "secret"
      }
    }
  ],
  "groups": [
    {
      "name": "web",
      "mappings": [
        {
          "name": "myapp",
          "remote": "myvps",
          "listen_port": 19999,
          "local_host": "127.0.0.1",
          "local_port": 9999
        },
        {
          "name": "ssh-tunnel",
          "remote": "another",
          "listen_port": 20022,
          "local_host": "10.0.0.5",
          "local_port": 22
        }
      ]
    }
  ]
}
```

### state.json（自动管理）

```json
{
  "mappings": {
    "web/myapp": {
      "running": true,
      "started_at": "2026-08-07T22:00:00+08:00"
    }
  }
}
```

daemon 重启后自动恢复上次状态。

## Docker 容器端口转发

除了传统的 SSH 反向隧道，portmap 支持将本机或远程未发布端口（未暴露到宿主机端口）的 Docker 容器直接转发到宿主机本地监听。

### 两类映射命令对比

1. **SSH 映射**（默认 `type=ssh`）：
   ```bash
   portmap add myapp --type=ssh -r myvps -l 19999 -p 9999 -g web
   ```
2. **Docker 映射**（`type=docker`）：
   ```bash
   portmap add web-proxy --type=docker --container=my-nginx --container-port=80 -l 8080 --bind-host=127.0.0.1
   ```
   可使用 `portmap docker list` 发现本机运行中的 Docker 容器与其已声明/已发布的端口信息。

### 守护进程参数与 Docker Socket

启动 `portmap serve` 时，可通过参数显式覆盖 Docker 守护进程连接与 Helper 镜像：
```bash
portmap serve --docker-host="unix:///Users/user/.docker/run/docker.sock" --docker-helper-image="ghcr.io/iammathew/docker-port-forward/sidecar:latest"
```
- **Helper 镜像要求**：默认使用 `ghcr.io/iammathew/docker-port-forward/sidecar:latest`；若使用自定义镜像，镜像内需提供 `socat` 可执行文件，并兼容 `sleep infinity` 的默认入口调用（例如避免镜像固定 `ENTRYPOINT=socat` 导致追加 `Cmd ["sleep", "infinity"]` 无法作为后台驻留容器启动）。
- **macOS / Docker Desktop**：如遇默认 socket 不可达，通常位于 `unix://$HOME/.docker/run/docker.sock`。
- **默认监听地址**：`--bind-host` 默认安全绑定至 `127.0.0.1`，避免非预期暴露到公网。

### 资源所有权、恢复机制与限制

- **标签隔离**：portmap 为由其创建的 bridge 网络和 helper 容器附加 `portmap.managed=true` 及唯一的 `portmap.instance_id` 实例标签，绝不误触宿主机上的其他容器与网络。
- **恢复与清理**：`portmap serve` 启动时首先在有界超时内尝试清理当前实例残留的孤儿 helper 容器与辅助网络并报告失败，随后才按上次记录恢复运行状态；停止映射时会主动回滚已创建的关联资源。
- **协议限制**：当前 Docker 端口转发基于底层 TCP 流，仅支持 **TCP** 协议，不支持 UDP。
