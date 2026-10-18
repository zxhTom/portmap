# Docker 与宿主机端口统一管理设计

## 目标与范围

保留现有 SSH 反向映射，增加独立的 Docker TCP 映射：宿主机监听端口 → 运行中的容器端口。两种映射统一支持增删、列表、分组启停及期望运行状态持久化。旧配置省略类型时仍解释为 SSH；宿主机服务继续使用现有 local_host/local_port。此次不增加 UDP、容器创建、容器重建、Compose 修改、自动修改已有 Docker 发布端口或远程 Docker TLS 管理。

## 当前限制与参考原理

现有 Manager.entry 和 startMappingLocked 直接依赖 sshclient.Tunnel，Mapping 必须引用 SSH remote，无法表达本机 Docker 转发。

已通过 npm 安装 docker-port-forward@1.0.2 到 /private/tmp/portmap-docker-reference。参考源码位于 node_modules/docker-port-forward/index.js，仓库为 https://github.com/iammathew/docker-port-forward。

该工具创建临时网络，将目标与 socat 辅助容器连接到网络。宿主机接受 TCP 后，通过 Docker exec 的双向流运行 socat STDIO TCP-CONNECT，将流量转发至目标。此方法无需修改目标容器原始端口发布配置，也不依赖宿主机直接访问容器 IP。

不能直接复用原包作为常驻后端：固定网络名；每次启动无条件拉取镜像；缺少启动失败回滚；仅处理 SIGINT；未定义多映射共享目标时的所有权与恢复机制；npm bin 配置异常。实现应借鉴原理，以 Go 接入 Docker Engine API。

## 建议架构

- 新增转发生命周期接口，由 SSH Tunnel 与 Docker Forward 实现；接口至少包含启动、关闭及实际运行状态查询。Close 应幂等。
- Mapping 增加 type，值为 ssh 或 docker，空值等同 ssh。Docker 增加 container、container_port、bind_host；沿用 listen_port 表达宿主机监听端口，bind_host 默认 127.0.0.1。SSH 字段和语义保持兼容。
- CLI add 增加 --type、--container、--container-port、--bind-host。--remote 与 --local-port 从 Cobra 无条件必填改为按类型校验。Docker 使用示例：portmap add web --type docker --container nginx --container-port 80 --listen 8080 -g docker。
- 增加容器查询命令及 API，显示运行状态、容器标识、已发布端口和声明端口；声明端口不是可转发端口的限制。
- Docker socket 默认 unix:///var/run/docker.sock，可通过单独 daemon 配置覆盖 Unix socket 路径，以支持 Docker Desktop；不假定宿主机可直连容器地址。
- Docker 后端先验证容器运行状态和端口，再绑定宿主机监听地址。创建每条映射独有且带所有权 labels 的网络与 helper，名称使用实例标识及随机后缀，不能使用固定全局名。
- 通过 Docker exec 的非 TTY 双向流连接 socat，正确拆分 stdout/stderr 帧，只将 stdout 传回客户端。每个 TCP 会话独立 exec，不通过 shell 拼接命令。
- 缓存已存在 helper 镜像，缺失时才拉取；拉取结束须检查 Docker 返回的流内错误。helper 镜像应可配置，默认参考原项目的 socat 镜像；真实验收必须核对当前平台支持情况。
- 启动任一步失败必须逆序回滚本映射创建的资源。停止与删除先终止监听及会话，再清理 helper、目标在本映射网络的连接及网络。禁止删除目标容器、既有网络或非本实例资源。
- 启动时仅清理本数据目录实例拥有的残留资源，再尝试恢复期望运行的映射。不同数据目录、不同映射并发启动不得互相清理。使用持久化实例 ID 确定所有权。
- 将期望运行与实际状态分开。恢复失败不能显示 running，应返回 failed/error 信息并保留重启恢复意图。Docker daemon 不可达只影响 Docker 映射，SSH 与管理 API 仍须正常工作。

## 允许修改文件

- cmd/portmap/main.go：CLI 类型参数、条件校验、容器查询。
- internal/config/config.go：兼容配置字段、daemon Docker 配置、实例 ID 与状态结构。
- internal/manager/manager.go：统一生命周期及实际状态管理。
- internal/sshclient/client.go：仅为生命周期接口、幂等关闭与实际状态所需的最小适配。
- internal/api/api.go：Docker 参数与容器查询路由。
- internal/forward/*.go：统一生命周期定义，如确有必要。
- internal/dockerclient/*.go：Docker API、转发、资源所有权与清理。
- 上述目录中的 *_test.go：单元与协议模拟测试。
- go.mod、go.sum：必要的 Docker Go 客户端依赖。
- README.md、DEPLOY.md：配置、两类用法、Docker socket、资源行为及验收说明。

禁止修改参考包、其他项目、真实容器或网络配置，禁止提交凭据，禁止顺手重构无关代码，禁止自动提交。Codex 不写实现代码，由右侧 Antigravity CLI 使用 gemini-3.6-flash-low 完成。

## 测试用例

1. 旧 JSON 和旧 CLI SSH 命令保持兼容；无 Docker daemon 时 SSH 配置管理正常。
2. Docker 参数缺失、类型未知、端口超范围、容器停止或不存在、宿主机端口冲突，返回清楚的错误并不残留资源。
3. 模拟 Docker API，验证创建顺序、stdout/stderr 解帧、二进制双向流、客户端提前断开、helper 异常退出与启动失败回滚。
4. 两条映射指向同一容器，停止其中一条不影响另一条；两实例不互相清理资源。
5. 重复 Close、删除、分组停止安全；并发列表与启停无数据竞争。
6. 重启恢复成功显示 running；恢复失败显示实际失败而非 running；明确停止后不恢复。
7. 真实 Docker：创建未发布端口的 HTTP 测试容器，启动映射后从宿主机获取确定内容；停止后宿主机监听释放，目标仍运行，自建 helper 和网络全部清理；进程异常退出后重启验证残留回收。

## 验收命令

- go test ./...
- go test -race ./...
- go vet ./...
- go build -o /private/tmp/portmap-docker-acceptance ./cmd/portmap
- /private/tmp/portmap-docker-acceptance --help
- /private/tmp/portmap-docker-acceptance add --help
- Docker 集成测试使用显式启用的环境变量及独立数据目录，默认测试不得修改真实 Docker 资源。必须报告实际执行的命令及资源清理结果，不能将跳过写成通过。

## 执行状态

实现已由当前 herdr 窗口右侧 Antigravity 完成，Codex 已评审并独立验收，未提交。原指定 gemini-3.6-flash-low 在当前 CLI 不可用，用户同意继续后实际使用 gemini-3.8-flash-low。

验收通过：go test -count=1 -timeout 60s ./...、go test -count=1 -race -timeout 60s ./...、go vet ./...、构建、根命令与 add 帮助、git diff --check，以及独立临时数据目录的 CLI/API 冒烟测试（旧 SSH 命令、Docker 增删列表、400/405 错误、daemon 不可用状态隔离）。

真实 Docker 集成入口已执行，但因 daemon 不可达明确跳过，不能视为实机通过。集成测试覆盖双转发、停止后的独立资源断言及模拟孤儿网络/helper 回收；异常退出恢复目前为孤儿资源模拟，尚未实测完整进程崩溃重启。执行计划见 handoff_docker_ports_plan.md。
