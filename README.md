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
