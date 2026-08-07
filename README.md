# portmap

TCP 端口映射管理器，支持 Relay 隧道实现内网穿透。可部署在公网服务器上直接转发流量，也可通过 relay 模式将 NAT 后的内网服务暴露到公网。

## 架构

### 直连模式
服务器能直接访问目标 — 简单 TCP 代理：

```
客户端 → [portmap:20000] → 目标:8080
```

### Relay 模式（内网穿透）
服务器无法直接访问目标（目标在内网 NAT 后）。由内网机器主动出站建立隧道：

```
                     ┌─ portmap serve (VPS B) ─┐
外部 C ──→ :20000                              │
                     │      relay 隧道 (A → B)  │
                     │                          │
                     └─ portmap connect (A) ────┘
                                    │
                              目标:8080 (A 或其局域网内)
```

- **`portmap serve --relay`** 运行在公网 VPS（B）上
- **`portmap connect`** 运行在内网机器（A）上，主动连接 VPS relay
- 外部客户端（C）访问 VPS 公网端口，流量通过隧道到达 A

## 快速开始

```bash
# 编译
go build -o portmap ./cmd/portmap/

# --- 直连模式 ---
./portmap serve
./portmap add web --target-host 10.0.0.5 --target-port 8080 -g my-group
./portmap start web -g my-group

# --- Relay 模式（VPS 侧） ---
./portmap serve --relay
./portmap add myapp --target-host <内网IP> --target-port 8080 -g infra
./portmap start myapp -g infra

# --- Relay 模式（内网机器，连接 VPS） ---
./portmap connect --relay-addr <vps公网IP>:7000
```

## 核心概念

### 映射（Mapping）

一条映射定义了一个 TCP 监听，将收到的所有连接转发到目标。

| 字段         | 必填 | 默认   | 说明                          |
|-------------|------|--------|------------------------------|
| name         | 是   | —      | 组内唯一标识                   |
| target_host  | 是   | —      | 转发的目标 IP                  |
| target_port  | 是   | —      | 转发的目标端口                  |
| local_port   | 否   | 自动   | portmap 监听端口，0=自动分配     |

### 分组（Group）

分组将相关映射归类，支持批量启停和共享端口范围。

| 字段        | 默认   | 说明                            |
|------------|--------|--------------------------------|
| name        | —      | 分组标识                        |
| port_range  | nil    | 自动分配端口范围（覆盖全局设置）   |

### 状态持久化

每条映射的运行/停止状态写入 `state.json`。daemon 重启后自动恢复上次状态 — 运行的继续运行，停止的保持停止。

## CLI 参考

所有命令通过 HTTP API 与 daemon 通信。

```
PORTMAP_HOME    数据目录，默认 ~/.config/portmap
--data-dir      覆盖数据目录（全局参数）
--api-addr      Daemon API 地址，默认 127.0.0.1:9876（全局参数）
```

### serve

启动 daemon。

```bash
portmap serve [flags]

参数:
  --bind-addr string   端口监听地址（默认从 config 读取：0.0.0.0）
  --log-file string    日志文件路径（默认仅 stderr）
  --relay              启用 relay 服务器，接受隧道客户端连接（默认端口 7000）
```

### connect

以隧道客户端身份连接 portmap relay 服务器。用于内网机器主动连接 VPS。

```bash
portmap connect [flags]

参数:
  --relay-addr string   Relay 服务器地址（例如 vps公网IP:7000）
  --token string        Relay 认证令牌（当服务器需要时）

# 使用 --relay-addr 时，从服务端 config.json 读取映射信息
# 不使用 --relay-addr 时，从数据目录的 tunnel.json 读取配置
```

### add

添加端口映射。添加后默认为停止状态。

```bash
portmap add <名称> [flags]

参数:
  -g, --group string        分组名称（默认 "default"）
  -p, --port int            本地监听端口（0=自动分配，默认 0）
      --target-host string  目标主机（必填）
      --target-port int     目标端口（必填）
```

示例：
```bash
portmap add grafana -g infra --target-host 10.0.0.10 --target-port 3000
portmap add ssh-relay -g infra --target-host 10.0.0.11 --target-port 22 -p 2222
```

### update

更新已有映射。仅修改指定字段。如果映射正在运行且目标或端口变化，会自动重启。

```bash
portmap update <名称> [flags]

参数:
  -g, --group string        分组名称（默认 "default"）
  -p, --port int            新本地端口（0=保持当前端口不变）
      --target-host string  新目标主机
      --target-port int     新目标端口
```

### delete

删除映射。如果正在运行会先停止。

```bash
portmap delete <名称> -g <分组>
```

### list

查看所有映射及其状态。

```bash
portmap list
```

输出列：GROUP, NAME, STATUS, LOCAL_PORT, TARGET, STARTED

状态说明：
- `running` — 直连代理正在监听
- `waiting` — Relay 模式：已启动但尚未有隧道客户端连接
- `connected` — Relay 模式：隧道客户端已连接，正在转发
- `stopped` — 已停止

### start / stop

启动或停止单条映射。

```bash
portmap start <名称> -g <分组>
portmap stop <名称> -g <分组>
```

### group start / group stop

启动或停止分组内的所有映射。

```bash
portmap group start <分组名>
portmap group stop <分组名>
```

## API

Daemon 在 `127.0.0.1:9876` 上提供 REST API：

| 方法     | 路径                            | 说明            |
|---------|---------------------------------|-----------------|
| GET      | /api/mappings                   | 查看所有映射     |
| POST     | /api/mappings                   | 添加映射        |
| PUT      | /api/mappings/:name             | 更新映射        |
| DELETE   | /api/mappings/:name?group=g     | 删除映射        |
| POST     | /api/mappings/:name/start?group=g | 启动映射      |
| POST     | /api/mappings/:name/stop?group=g  | 停止映射      |
| POST     | /api/groups/:name/start         | 启动分组        |
| POST     | /api/groups/:name/stop          | 停止分组        |

### 添加映射请求

```json
{
  "group": "infra",
  "name": "grafana",
  "local_port": 0,
  "target_host": "10.0.0.10",
  "target_port": 3000
}
```

### 查看映射响应

```json
[
  {
    "group": "infra",
    "name": "grafana",
    "local_port": 20000,
    "target_host": "10.0.0.10",
    "target_port": 3000,
    "status": "running",
    "started_at": "2026-08-07T16:00:00+08:00"
  }
]
```

## 配置文件

配置文件位于 `$PORTMAP_HOME`（默认 `~/.config/portmap/`）。

### config.json

```json
{
  "bind_addr": "0.0.0.0",
  "port_range": {
    "start": 20000,
    "end": 30000
  },
  "relay": {
    "enabled": true,
    "listen_addr": "0.0.0.0:7000",
    "token": ""
  },
  "groups": [
    {
      "name": "infra",
      "port_range": {"start": 20000, "end": 20099},
      "mappings": [
        {
          "name": "grafana",
          "local_port": 20000,
          "target_host": "10.0.0.10",
          "target_port": 3000
        }
      ]
    },
    {
      "name": "web",
      "mappings": [
        {
          "name": "api",
          "local_port": 0,
          "target_host": "10.0.0.20",
          "target_port": 8080
        }
      ]
    }
  ]
}
```

- `bind_addr`：监听地址。`0.0.0.0` 接受所有网络接口的连接，`127.0.0.1` 仅本地。
- `port_range`：全局自动分配端口范围。
- `relay.enabled`：是否启用 relay 服务器，接受隧道客户端连接。
- `relay.listen_addr`：relay 服务器监听地址（默认 `0.0.0.0:7000`）。
- `relay.token`：可选的隧道客户端认证令牌。
- `groups[].port_range`：分组级别的端口范围，设置后覆盖全局设置。
- `mappings[].local_port`：`0` 表示从对应端口范围自动分配。

### tunnel.json（客户端配置，供 `portmap connect` 使用）

使用 `connect --relay-addr` 时自动生成，也可手动创建：

```json
{
  "relay_addr": "vps公网IP:7000",
  "token": "",
  "tunnels": [
    {
      "mapping_id": "web/my-service",
      "target_host": "127.0.0.1",
      "target_port": 8080
    },
    {
      "mapping_id": "infra/ssh",
      "target_host": "10.0.0.5",
      "target_port": 22
    }
  ]
}
```

### state.json（daemon 管理，请勿手动编辑）

```json
{
  "mappings": {
    "infra/grafana": {
      "running": true,
      "local_port": 20000,
      "started_at": "2026-08-07T16:00:00+08:00"
    }
  }
}
```
