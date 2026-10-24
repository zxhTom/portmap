# portmap

SSH 反向隧道管理器。在 A（内网机器）上运行，通过 SSH 连接 B（VPS），将 A 的端口映射到 B 的公网端口。

**B 可以只运行 SSH**，不需要安装 portmap 即可转发端口。安装可选的 `portmap server` 后，B 还可查看并禁用经过它的映射。

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

## 配置变更和故障自动恢复

`portmap serve` 每隔 5 秒检查 `config.json` 和映射的实际状态：

- 有效配置变更会自动应用，仅重建发生变化的映射；修改 SSH remote 或 Docker 连接配置也会重建受影响的映射。
- 配置文件暂时缺失、JSON 格式错误、端口无效、名称重复或引用不存在的 remote 时，保留上次有效配置并记录错误。建议通过临时文件加原子替换的方式保存配置。
- 已执行 `start` 的映射会持续尝试恢复，包含首次启动失败、开机时网络不可用等情况。重试会等待当前连接/清理操作结束，因此实际恢复时间还取决于网络和 Docker API 超时。
- SSH 连接中断后会重新建立完整的 SSH 连接；每 10 秒发送保活请求，5 秒无响应则关闭失效连接并等待恢复。
- Docker 目标/helper 停止、同名目标容器重建、专用转发网络地址变化时，关闭旧转发，再按配置创建新资源。容器重建要使用稳定的容器名称；固定的容器 ID 不会跟随新容器。
- 新增映射默认停止，需要执行一次 `portmap start`。手动停止的映射不会被自动启动；配置变更会保留映射的启停意图。
- 自动恢复重建的是映射，不会重启你的业务容器，也不需要重启整个 portmap 服务。

Linux 开机自启仍需安装 `deploy/portmap.service` 并执行 `systemctl enable --now portmap`。CLI 和服务必须使用相同的数据目录；例如服务使用 `/etc/portmap` 时，CLI 也应指定 `--data-dir /etc/portmap`。

## Multipass 虚拟机端口映射

在运行 Multipass 的宿主机上运行 portmap，通过实例名定位虚拟机，无需固定 IP，也无需在虚拟机内安装 portmap。

```bash
# 列出实例、运行状态及 IPv4 地址
portmap multipass list

# 虚拟机 shell-test-vm:22 → 宿主机 127.0.0.1:2222
portmap add vm-ssh --type=multipass \
  --instance=shell-test-vm --instance-port=22 -l 2222
portmap start vm-ssh
ssh -p 2222 ubuntu@127.0.0.1

# 虚拟机 shell-test-vm:22 → 已配置 VPS myvps:1922
portmap add vm-public --type=multipass \
  --instance=shell-test-vm --instance-port=22 -r myvps -l 1922
portmap start vm-public
```

- 不填写 `--remote` 时，在宿主机监听 TCP 端口；默认 `--bind-host=127.0.0.1`，需要局域网访问可显式设为 `0.0.0.0` 并配置防火墙。
- 填写 `--remote` 时，在对应 VPS 上建立 SSH 反向监听；需要先配置 remote，并满足 SSH 转发、GatewayPorts 和防火墙条件。
- 使用 `multipass info --format json` 读取运行实例的第一个有效非回环 IPv4 地址。宿主机必须能直接访问该地址；虚拟机内服务应监听实例网卡地址或 `0.0.0.0`，仅监听虚拟机内 `127.0.0.1` 的服务不能直接转发。
- 实例停止、删除、重建或 IP 改变后，旧映射会失效并按运行意图重试；恢复同名实例后会重新获取 IP。手动停止的映射保持停止。程序不会自动启动、停止或修改虚拟机。
- 支持 TCP，不支持 UDP。默认优先从 PATH 查找 `multipass`，Linux 下也会尝试 `/snap/bin/multipass`。其他路径可设置 `PORTMAP_MULTIPASS_BINARY`；服务用户必须有权限访问 Multipass daemon。
- 启停状态、配置热加载和开机恢复与其他映射相同。使用 systemd 时 CLI 也应指定相同的 `--data-dir /etc/portmap`。

配置中的映射示例：

```json
{
  "name": "vm-ssh",
  "type": "multipass",
  "instance": "shell-test-vm",
  "instance_port": 22,
  "listen_port": 2222,
  "bind_host": "127.0.0.1"
}
```

API：`GET /api/multipass/instances` 查看实例；`POST /api/mappings` 使用上述字段添加映射，其他启停 API 不变。公网反向映射额外设置 `remote`。

## 映射用途描述

添加映射时使用 `--description`，`portmap list` 会显示 DESCRIPTION 列：

```bash
portmap add vm-ssh --type=multipass --instance=shell-test-vm \
  --instance-port=22 -l 2222 --description "shell 安装验证虚拟机的 SSH 入口"

# 修改已有映射描述，-g 指定原分组
portmap describe tom-ssh "tom 虚拟机 SSH 管理入口，公网 1922" -g multipass

# 清空描述
portmap describe tom-ssh "" -g multipass
```

配置使用可选的 `description` 字段。修改描述不会重启或中断映射；直接编辑配置中的描述也支持自动加载。API 使用 `PUT /api/mappings/<name>/description?group=<group>`，请求体为 `{"description":"用途说明"}`。旧配置不含此字段时仍可正常使用。

## 迭代更新已部署的 portmap

`upgrade` 针对 Linux systemd 部署，保留现有配置、描述、映射及每条映射的运行/停止意图。更新会短暂断开当前连接，原端口会自动恢复，客户端重新连接即可。

```bash
# 生成新版本，也可以使用可信来源提供的本机架构二进制文件
go build -ldflags '-X main.buildVersion=2026.10.18' -o /tmp/portmap.new ./cmd/portmap

# 本机和 ds2 当前均部署为用户服务，在各自机器上执行：
~/.local/bin/portmap upgrade --binary /tmp/portmap.new

# 可选：校验下载文件的 SHA-256
~/.local/bin/portmap upgrade --binary /tmp/portmap.new --sha256 <预期SHA256>

# 查看版本与更新结果
~/.local/bin/portmap version
~/.local/bin/portmap list
```

流程：验证候选程序与配置版本 → 确认服务进程和数据目录 → 备份旧程序、config.json、state.json → 校验并原子替换程序 → 重启原服务 → 核对配置、描述、启停状态及原先正常运行的映射。相同程序已安装且正在运行时，跳过更新和重启。新版启动或恢复检查失败时，自动还原旧程序和配置快照并验证恢复。

备份保存在数据目录的 `updates/backup-*`，目录仅当前用户可访问，配置备份权限为 0600。每次备份含 `metadata.json`（版本文件校验值、服务名和更新时间）。更新失败时保留失败配置供排查，不在日志输出密码。

- 默认操作用户级 `portmap.service`。系统级部署使用 `sudo portmap --data-dir /etc/portmap upgrade --system --binary /tmp/portmap.new`。
- 使用其他服务名或 API 地址时，指定 `--service`、`--api-addr`；数据目录通过 `--data-dir` 或 `PORTMAP_HOME` 指定。
- 默认更新当前命令所在的程序。首次从不支持 upgrade 的旧版更新，可运行新程序：`/tmp/portmap.new upgrade --binary /tmp/portmap.new --install-path /实际安装位置/portmap`。
- 映射恢复等待时间默认 60 秒，可用 `--timeout 120s` 调整。原先已离线的映射保留重试意图，不会阻塞更新。
- 更新期间避免同时修改配置或启停映射；恢复检查发现变化会回退到更新前快照。该命令不修改已有 systemd 单元及开机启用设置。

维护者可运行 `python3 tests/upgrade_systemd.py /绝对路径/portmap` 做真实 systemd 更新/回退验收。测试创建独立的临时服务，验证成功更新、重复更新、校验失败和故障回退，结束时清理测试服务；不会操作正式的 portmap.service。

## 隧道定期检测与作废

服务约每 30 秒检查一次已启动映射的 TCP 入口和目标端口，检测超时为 8 秒；多条映射顺序检查，网络故障时实际间隔可能延长。配置仍每 5 秒加载。SSH 映射探测公网中转端口和本地目标；Multipass 探测虚拟机目标及入口；Docker 通过辅助容器探测目标端口。检测不会发送业务请求，只验证 TCP 可达性，不代表应用业务健康。

检测失败关闭旧隧道并尝试重建，每轮失败只计一次；重建成功不会立即清零，下一轮检测成功才清零。连续失败**超过 10 次（第 11 次）**，标记“作废”并停止自动重试，保留映射配置。状态及失败次数写入 state.json，服务重启和更新后保留；主动停止的映射不检测、不重试。

`portmap list` 显示“待检测 / 正常 / 重试中 / 作废 / 已停止”及连续失败次数。API 列表增加 `health`、`consecutive_failures`、`checked_at`；作废时 `status` 为 `invalid`，`error` 包含最近失败原因。修复目标服务或网络后手动启动，清除作废状态和计数：

```bash
portmap start tom-ssh -g multipass
portmap list
```

## 有效期与续期

映射默认长期有效。可选配置字段 `expires_at` 使用带时区的 RFC3339 时间，例如 `2026-11-01T18:00:00+08:00`。服务在约每 5 秒的配置检查中关闭已到期隧道（其他检测阻塞时可能延迟），标记“作废 / 已到期”，保留配置并停止自动重建；启动时也不会恢复过期隧道。

```bash
# 已有隧道：从现在起有效 7 天（不是在旧期限上累加）
portmap renew tom-ssh -g multipass --for 7d

# 指定到期时刻，必须包含时区
portmap renew tom-ssh -g multipass --until '2026-11-01T18:00:00+08:00'

# 取消期限，改为长期有效
portmap renew tom-ssh -g multipass --clear

# 创建时设置期限，也可创建后使用 renew
portmap add demo -r relay-vps -l 3022 -p 22 --expires-at '2026-11-01T18:00:00+08:00'
portmap list
```

`renew` 也可写为 `expiry`。`--for` 支持 `30m`、`24h` 或整数天 `7d`。续期已作废且原本启用的隧道时，会清零失败计数并尝试恢复；手动停止的隧道保持停止。正常运行的隧道修改期限不会重建。期限保存在配置中，服务重启和升级后仍生效，不会重置为新的时长。

API：`PUT /api/mappings/<name>/expiry?group=<group>`，请求体 `{"expires_at":"2026-11-01T18:00:00+08:00"}`；空字符串表示长期有效。列表返回 `expires_at` 和 `invalid_reason`（`expired` 为到期作废，`connection_failures` 为连续连接失败），CLI 显示到期时间和作废原因。

## Web 管理控制台

控制台与静态资源内嵌于单个 Go 程序，无需额外的 Node 服务或前端构建。`portmap serve` 默认在 `127.0.0.1:9877` 启动网页；命令行 API 仍为 `127.0.0.1:9876`。支持中文桌面/移动布局、真实状态统计、搜索与类型筛选、创建/删除和启停隧道、描述编辑、有效期续期、中转节点管理、当前浏览器会话操作记录，以及管理密码修改。浏览器数据约每 20 秒刷新，后台检测周期不变。

**首次登录：** 访问 `http://127.0.0.1:9877`，账号 `admin`。首次启动时每个实例独立生成随机密码，保存在数据目录的 `web-initial-password`，例如：

```bash
cat ~/.config/portmap/web-initial-password
```

登录后点击左下角账号修改密码。密码以 bcrypt 哈希保存在 `web-auth.json`，文件权限 0600；修改密码后删除初始密码文件并注销所有会话。不要把这些文件提交到 Git。已有隧道和 SSH 认证配置不会用作网页登录凭据。更新保留认证文件，服务重启后需要重新登录。

**访问 ds2 上的网页：** 在浏览器所在电脑保持以下 SSH 命令运行，当前 ds2 已启用 HTTPS，转发后打开 `https://127.0.0.1:9878`（自签名证书需信任）。在 ds2 上查看其独立初始密码。

```bash
ssh -N -L 127.0.0.1:9878:127.0.0.1:9877 -p 1822 zxhtom@47.121.182.177
```

默认不公开新的公网端口。需要公网直接访问时配置 HTTPS 证书，并仅将 Web 监听器对外开放，CLI API 始终应保留在回环地址：

```bash
portmap serve --web-addr 0.0.0.0:9877 --web-cert /path/fullchain.pem --web-key /path/privkey.pem
# 不启动网页（命令行与隧道照常工作）
portmap serve --web-addr=
```

对外 Web 监听强制使用 HTTPS；默认建议通过 SSH 转发访问。会话保存在服务内存中，12 小时有效；HttpOnly、SameSite=Strict Cookie，HTTPS 时附加 Secure。所有管理写操作校验同源 Origin 和会话 CSRF Token，登录失败 5 次后对来源地址限流 15 分钟。中转节点接口不会返回密码、私钥路径或口令；网页不暴露内部进程健康接口。

忘记管理密码时，可从标准输入重设并重启用户服务。密码不应出现在命令参数或 shell 历史中：

```bash
read -rs WEB_NEW_PASSWORD
printf '%s' "$WEB_NEW_PASSWORD" | portmap web-password --password-stdin
unset WEB_NEW_PASSWORD
systemctl --user restart portmap
```

页面操作记录只在当前浏览器会话内保留，不能替代持久化审计日志。Webhook、通知和跨实例集中管理暂未提供。

开发验收：`go test -race ./...` 包含登录隔离、CSRF/同源校验、限流、会话失效、密码持久化和敏感字段过滤。安装 Playwright 后可运行 `node tests/web_browser.mjs /绝对路径/portmap`；测试仅启动临时实例，覆盖真实登录和 CRUD、续期、密码修改及桌面/移动截图。通过 `PORTMAP_PLAYWRIGHT_MODULE`、`PORTMAP_CHROMIUM` 和 `PORTMAP_SCREENSHOTS` 可指定已有测试工具和截图目录。

## 中转端口占用检查与随机分配

通过 CLI/Web/API 新建 SSH 中转映射（包括带 `remote` 的 Multipass 映射）时，会先认证到中转站，实际尝试绑定端口。指定端口已经被本实例其他映射使用时，会提示映射名称；中转站有其他进程占用或禁止 SSH 转发时，会明确提示端口不可用。认证或主机指纹验证失败时，不会把端口当作可用，也不会保存映射。

```bash
# 不指定 --listen：使用加密随机数在 10000–65535 中选择并检查一个五位数端口
portmap add web-console --remote relay-vps --local-port 9877 --description 'HTTPS 管理后台'

# 显式指定：检查中转端口，冲突则拒绝创建
portmap add fixed-ssh --remote relay-vps --listen 1922 --local-port 22

# 查看最终保存的端口
portmap list
```

API 创建时省略 `listen_port` 或传 `0`，响应中会返回分配后的实际 `listen_port`。Web 的中转端口输入留空即可随机分配；Docker 和不使用中转站的 Multipass 本地转发仍需显式指定监听端口。随机仅发生在创建时，端口保存到配置，重启和更新不会重新选号。

同一中转地址和 SSH 端口的不同节点名称共享本实例的端口冲突检查，已停止或作废但尚未删除的配置仍保留其端口。配置热加载也拒绝重复中转端口。创建时的检查是短时探测，不会在隧道停止期间替其他进程长期保留系统端口；正式启动仍由系统实际绑定决定，若其他程序抢先占用会启动失败并提示，绝不会抢占或关闭对方服务。

浏览器验收现在使用真实 SSH 测试中转站检查端口绑定，先额外构建辅助程序：`go build -o /tmp/portmap.test-relay ./tests/relay`，并把主程序构建为 `/tmp/portmap` 后再运行 `node tests/web_browser.mjs /tmp/portmap`。也可用 `PORTMAP_TEST_RELAY` 指定辅助程序路径。

## 域名模式

新安装默认后缀为 `port.toms.chat`，可以在 Web 控制台的“域名后缀设置”中统一修改当前实例的所有中转节点。设置会核对已有域名隧道使用的中转站后缀，并短暂重连。公网生效还要求 DNS 通配记录、Nginx 入口与 HTTPS 证书同时指向并覆盖同一后缀。

域名模式适用于 **HTTP/HTTPS Web 服务**。公网入口为 `https://<前缀>.port.toms.chat`，无需再附带中转端口。SSH、数据库等普通 TCP 协议不会携带 HTTP Host，继续使用原有端口模式，不能凭一个 A 记录在同一端口区分目标。

当前管理后台：`https://portmap.port.toms.chat`。在 Web 的“新建隧道”中填写域名前缀及后端协议；已有隧道可点名称打开详情，再点“绑定域名”。支持多个前缀，用逗号或空格分隔，没有人为设置域名数量上限。前缀须符合 DNS 单标签规则（字母、数字、内部横线，最长 63 字符）。

```bash
# 新建 HTTP 服务，同时自动选择可用的五位数内部中转端口
portmap add app --remote relay-vps --local-port 8080 --domain app
portmap start app

# 多个域名指向同一条隧道
portmap domains app --prefix app,api --upstream-scheme http

# 当前 1822 管理后台的实际绑定
portmap domains portmap-web-public -g management --prefix portmap --upstream-scheme https

# 主动解除域名模式，恢复公网 IP+端口访问（会短暂重连）
portmap domains app --clear
portmap list
```

配置使用 `domain_prefixes: ["app", "api"]` 和 `upstream_scheme: "http"`（或 `https`）。根级 `domain_zone` 为统一后缀，未设置时默认 `port.toms.chat`；单个中转节点的 `domain_zone` 仅在未设置统一后缀时生效。Web 和 `GET/PUT /api/settings/domain-zone` 可读写统一后缀。API 创建支持上述字段；修改使用 `PUT /api/mappings/<name>/domains?group=<group>`，请求体 `{"domain_prefixes":["app"],"upstream_scheme":"http"}`。列表返回域名前缀、完整 `domains` 及后端协议。

访问路径：浏览器 → 47 的 Nginx 443 → 本机 `portmap relay` 路由服务 → 中转机 `127.0.0.1:<随机端口>` → SSH 加密隧道 → 1822 的目标服务。Nginx 仅新增 `*.port.toms.chat` 命名空间，既有站点继续保留。一个通配符入口配合动态查表，不需要每新增域名都手工写 Nginx 配置。

约束和生命周期：

- 域名由 47 上的中转服务集中登记，持久保存在 `~/.config/portmap-relay/domains.json`。同一个完整域名只能属于一个隧道身份；不同 portmap 实例并发申请也会拒绝重复，服务重启不会遗忘归属。
- 域名模式请求 SSH 仅监听中转站 `127.0.0.1`；中转服务检查实际监听地址，拒绝公开监听的后端。sshd 使用 `GatewayPorts clientspecified`，原有显式 `0.0.0.0` TCP 映射仍公开监听。旧公网 `18610` 已不再可直连管理后台，内部仍沿用该端口。
- 未绑定域名返回 404，不回落到任意隧道；已绑定但未启动或租约失效返回 503。HTTP 只对已绑定域名跳转 HTTPS，HTTPS 拒绝 TLS SNI 与 Host 不一致的请求。
- 停止或到期会停用转发并保留域名归属，便于续用；删除隧道或解除绑定会释放域名。隧道约每 20 秒续租，90 秒无续租则停止转发。中转重启时先禁用转发，随后由客户端心跳自动恢复，不会直接信任旧的活跃状态。
- HTTPS 后端通过源机器读取目标证书、经 SSH 传递 SHA-256 指纹，中转端验证该指纹；不会全局跳过上游证书校验。心跳重新检查目标证书以支持后端证书更换。
- 控制请求走已认证 SSH，再调用中转机私有 Unix socket（0600），没有公开的域名管理 API。当前部署统一使用 SSH 用户 `zxhtom`。
- 现有配置、描述、有效期和启停意图保留。升级会拒绝不支持域名模式的候选版本，以免把域名专用端口退回公开监听。

47 上服务为系统级 `portmap-server.service`，程序仍以 `zxhtom` 用户运行；HTTP 路由仅监听 `127.0.0.1:9881`，由 Nginx 对外提供 80/443。参考 [服务端单元](deploy/portmap-server.service) 与 [Nginx 配置](deploy/portmap-domains.nginx.conf)。两台客户端仍是各自的用户级 `portmap.service`。

47 当前使用临时自签名 `*.port.toms.chat` 证书，浏览器会提示不受信任；有效期至 2026-11-18。提供正式证书后，修改 `/etc/nginx/conf.d/portmap-domains.conf` 和 `/etc/nginx/conf.d/portmap-server.conf` 中的证书路径，并执行 `sudo nginx -t && sudo systemctl reload nginx`；**不要覆盖其他站点正在共用的证书文件**。

# VPS 服务端

同一个 `portmap` 二进制支持 `portmap server serve` 模式，在 VPS 上提供域名中转和独立控制台。当前服务端页面为 `https://server.port.toms.chat`，客户端页面为 `https://portmap.port.toms.chat`。客户端照常添加和启动映射；有服务端时会通过 SSH 自动上报，VPS 控制台可查看、禁用和重新启用映射。未安装服务端的 VPS 仍可使用普通 SSH 端口转发。部署方式、凭据位置和可见范围见 [DEPLOY.md](DEPLOY.md#vps-服务端模式)。

### VPS 域名后缀与服务端恢复

客户端的“中转服务器 → 管理后缀”和服务端的“此 VPS 的域名后缀”维护同一台 VPS 的允许列表与主后缀。新建隧道选定 VPS 后默认使用主后缀，也可选择其他绑定后缀；已有隧道保存自己的选择，不随主后缀修改。新增后缀仍需准备该 VPS 的 DNS、Nginx 与证书。使用中的后缀不可删除。

CLI 新建或编辑域名隧道可用 `--domain-zone <suffix>` 指定后缀；省略时，新隧道使用主后缀，编辑已有隧道保留原后缀。旧单后缀配置兼容。

服务端每 15 秒探测启用隧道的 VPS 入口；异常时通过客户端每 15 秒的 SSH 上报响应发送恢复请求。也可点击“请求恢复”。客户端核对本地启停意图、有效期、作废和禁用策略后重建，回传结果，服务端再次探测确认。指令一分钟有效、按隧道去重，自动尝试最多 5 次，退避从 30 秒增加到 8 分钟。达到上限后排查原因，再手动请求。

这依赖客户端守护进程运行并且仍能主动连接 VPS 的 SSH 控制通道；客户端进程退出、整机断网或 SSH 完全不可达时，服务端无法远程启动该进程。停止、过期、作废和被禁用的隧道不会被自动拉起。TCP 入口可达只表示隧道入口存在，不代表业务响应正常。
