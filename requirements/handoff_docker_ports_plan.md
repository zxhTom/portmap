# Docker 与宿主机端口统一管理实现计划

执行者：Antigravity CLI，模型固定 gemini-3.6-flash-low。Codex 仅设计、评审、跑验收，不编写实现代码。本计划提供接口契约和测试输入输出，实现与测试代码均由 Antigravity 编写。

目标：在保留旧 SSH 行为的同时，统一管理宿主机到容器的 TCP 转发。

设计：[handoff_docker_ports.md](handoff_docker_ports.md)。文件白名单、范围和禁止事项完全沿用设计。工作目录为 /Users/zxhtom/temp/github/portmap，不自动提交。

## 全局约束

- 空 type 表示 ssh；既有 SSH 命令不改变参数语义。
- Docker 默认只在 127.0.0.1 监听，明确参数才开放其他地址。
- 不改变目标容器的端口发布配置，不重启或删除目标容器。
- 仅清理由本实例拥有的资源；临时网络、辅助容器具有实例和映射 labels。
- 真实集成测试仅操作明确创建的测试资源；设计中的“禁止修改真实容器或网络配置”指用户已有资源，测试临时资源例外。
- 保持最少注释，不写大段解释性代码注释。
- Go 依赖优先使用与当前 go.mod 版本兼容的官方 Docker 客户端，并核实其实际 API 后再实现。

## 任务 1：配置兼容和类型校验

文件：internal/config/config.go、internal/config/config_test.go、internal/manager/manager.go、internal/manager/manager_test.go。

契约：Mapping 增加 Type、Container、ContainerPort、BindHost，JSON 分别为 type、container、container_port、bind_host。Config 增加 Docker 配置，包含 host 和 helper_image；默认 host 为 unix:///var/run/docker.sock。实例 ID 单独持久化在数据目录，创建失败应阻止 daemon 初始化，不能使用空 ID 做清理。不可并行启动两个共享数据目录的 daemon；应通过数据目录实例锁防止竞争。

- [ ] 编写兼容测试：旧 JSON 没有 type，读取及保存后仍可按 SSH 使用；Docker 配置读取默认 host 和 bind_host；缺省 local_host 的 Docker 映射合法。
- [ ] 编写校验测试：ssh 要求 remote、local_host、local_port；docker 要求 container、container_port；两者 listen_port 必须在 1..65535；目标端口范围相同；未知类型拒绝；验证 bind_host 是合法 IP，不接受未解析主机名。
- [ ] 运行 go test ./internal/config ./internal/manager，确认新用例先失败，再由执行者实现默认值、条件校验和实例 ID 存储。
- [ ] 再运行同一命令，检查旧映射的配置结构及 SSH 参数语义未改变。

## 任务 2：统一生命周期与真实状态

文件：internal/forward/forward.go、internal/manager/manager.go、internal/manager/manager_test.go、internal/sshclient/client.go、internal/sshclient/client_test.go。

契约：forward 包定义 Status，字段 State 和 Error；State 为 stopped、starting、running、failed。Backend 提供 Start() error、Close() error、Status() Status。通过 manager 内部工厂构造 backend，测试可替换工厂，不需要真实 SSH 或 Docker。state.json 的 running 继续表示期望启用，不能用它代替实际健康状态。MappingInfo 增加 type、bind_host、container、container_port、error。

- [ ] 写 fake backend 测试：Start 失败返回 failed；恢复失败保留 running 期望但实际状态 failed；手动 stop 清除期望；Shutdown 保留期望；failed 状态允许再次 start。
- [ ] 写 SSH 测试：只有远程 listener 成功建立才显示 running；断开后转为 failed；重复 Close 不 panic；关闭与状态查询并发安全。
- [ ] 执行 go test ./internal/manager ./internal/sshclient，确认新增用例失败，再实现接口和最小 SSH 适配。
- [ ] 将慢速启动、镜像下载和清理移出全局管理锁，使用映射级生命周期串行化，防止一条映射阻塞所有查询或其他映射。测试阻塞的 fake Start 期间 ListMappings 和其他映射操作能完成。
- [ ] 执行 go test -race ./internal/manager ./internal/sshclient，确认状态与停止没有竞争。

## 任务 3：Docker API 与资源生命周期

文件：internal/dockerclient/client.go、internal/dockerclient/resources.go、internal/dockerclient/client_test.go、internal/dockerclient/resources_test.go、go.mod、go.sum。

契约：NewClient 接收配置及实例 ID；ListContainers 接收 context 并返回容器概要；CleanupOwned 接收 context，仅匹配当前实例 label。内部 Docker API 抽象必须支持 fake 或 HTTP 测试服务。容器概要包含 id、name、state、declared_ports、published_ports，发布端口包含 host_ip、host_port、container_port、protocol。

- [ ] 编写模拟 API 测试：socket 不可用；容器不存在或停止；镜像已存在不拉取；拉取流内 error 返回失败；inspect 返回空网络地址时失败。
- [ ] 编写每个创建阶段失败的回滚测试，验证仅删除本次创建的资源，包括网络已创建但目标尚未连接、目标连接后 helper 创建失败、helper 启动失败。
- [ ] 编写所有权测试：不同实例 label 不清理；同目标两条映射有不同网络；删除一条不影响另一条；清理重试对已不存在资源成功；同数据目录第二实例被锁拒绝。
- [ ] 执行 go test ./internal/dockerclient，确认新测试先失败，再实现客户端、唯一命名、labels、实例启动回收和回滚。
- [ ] Docker API 每个操作设置有界 context；退出清理使用独立超时 context，不能复用已取消启动 context。清理失败返回错误并保留可追踪资源信息，供重启回收。
- [ ] 运行 go test -race ./internal/dockerclient。

## 任务 4：Docker TCP 转发

文件：internal/dockerclient/forward.go、internal/dockerclient/forward_test.go、internal/manager/manager.go。

契约：Docker Forward 实现 forward.Backend。Start 同步完成目标校验、本机 bind、helper 可用及网络目标地址解析；任何失败不得返回 running。每条宿主机连接创建独立 socat exec，非 TTY，stdin/stdout 连接启用，stderr 不进入业务流。命令以数组传入，不能使用 shell 拼接。

- [ ] 写协议测试：包含零字节和任意字节的 TCP 数据往返一致；交错 Docker stdout/stderr 帧只输出 stdout；截断帧导致当前会话退出；对端提前断开与 Close 能停止两个复制方向。
- [ ] 写生命周期测试：端口占用不残留 helper/network；多个并发连接互不影响；helper 异常退出或目标停止转为 failed，监听被释放；重复 Close 安全；进程停止时所有会话关闭。
- [ ] 执行 go test ./internal/dockerclient，确认失败，再实现本机 listener、exec hijack、解帧、会话跟踪、状态监测与清理。
- [ ] 失败路径取消监测任务，明确停止等待任务退出；避免监测协程调用 Close 等待自己导致死锁。
- [ ] 执行 go test -race ./internal/dockerclient ./internal/manager。

## 任务 5：CLI、API 与说明

文件：cmd/portmap/main.go、cmd/portmap/main_test.go、internal/api/api.go、internal/api/api_test.go、README.md、DEPLOY.md。

契约：add 增加 --type（默认 ssh）、--container、--container-port、--bind-host；保留 --listen/-l。serve 增加 --docker-host、--docker-helper-image，优先级为显式 flag 高于 config.json 高于默认值。新增 docker list 命令，调用 GET /api/docker/containers。API 的 Docker 添加映射不要求 SSH 字段；映射 list 显示类型和正确目标。所有 start/stop 及分组动作仅接受 POST。

- [ ] 写 API 测试：旧 SSH 请求兼容；Docker 添加无需 remote；非法类型/端口返回 400；Docker daemon 不可用返回明确错误，不影响已有 API；GET start/stop 返回 405。
- [ ] 写 CLI 参数测试：原命令保持兼容；Docker 示例可通过参数校验；容器列表中包含声明端口和发布端口，未声明端口仍允许显式配置。
- [ ] 执行 go test ./cmd/portmap ./internal/api，确认新测试先失败，再实现命令和 API。
- [ ] 更新 README 和 DEPLOY，给出两种映射示例、Docker Desktop socket 示例、host 监听地址默认值、镜像与网络行为、失败状态、恢复行为、退出清理及当前协议限制。
- [ ] 执行 go test ./... 与 go vet ./...。

## 任务 6：集成验收与交付

文件：internal/dockerclient/integration_test.go（显式开关）、README.md 或 DEPLOY.md 中的验收说明。

- [ ] 添加 PORTMAP_DOCKER_INTEGRATION=1 开关，默认跳过；使用独立临时数据目录、唯一资源前缀，且只清理测试创建的资源。
- [ ] 测试创建未发布端口的 HTTP 服务目标，确认内容可经宿主机转发；同目标创建两映射，停一条另一条可用；最终目标仍运行且本实例 helper/network 均被回收。
- [ ] 测试异常退出后的重启残留回收和状态恢复；错误状态不能显示 running。
- [ ] 若 daemon 不可用，报告跳过原因，不自行安装或重启 Docker，不伪称集成通过。优先完成全部模拟 API 与协议测试。
- [ ] 执行设计中的 go test ./...、go test -race ./...、go vet ./...、构建与两条 help 命令。可用时额外运行 PORTMAP_DOCKER_INTEGRATION=1 go test ./internal/dockerclient -run Integration -v。
- [ ] Antigravity 报告改动文件、命令结果、跳过项与遗留问题。Codex 检查 git diff --stat、git diff --check、完整 diff，亲自执行上述验收。发现问题返回 Antigravity 修正，Codex 不直接修改实现。

## 评审重点

- 两个 daemon 共用数据目录：实例锁拒绝第二个，防止回收活跃资源（任务 1、3）。
- helper 拉取返回 HTTP 成功但流内 error：启动失败且无残留（任务 3）。
- Docker stream 的 stderr 帧包含非文本：业务 TCP 数据保持原样（任务 4）。
- 监测与 Close 并发：不死锁、不泄漏会话、重复 Close 安全（任务 4）。
- 目标容器停止后重启：实际状态失败，显式 start 能重新 inspect 并建立新转发（任务 2、4）。

## 审阅与执行

设计已确认。计划完成且已自检覆盖范围、字段一致性和失败用例。计划审阅确认后，按用户指定方式在当前 herdr 窗口右侧启动 Antigravity；当前其他窗口已有 agy-dev，不能误用。新实例应使用不冲突的名称并固定指定模型。
