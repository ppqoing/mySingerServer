# MySingerServer 分层架构重构 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不改变现有用户操作语义的前提下，把 `gui.exe`、`agent.exe`、`worker.exe`、`helper.exe`、`nodetray.exe` 重构为边界清晰、可独立配置、通过 TCP + Protobuf 通信的分层模块，并按性能方案完成有界并发、游标分页、删除恢复和分析结果原子发布。

**Architecture:** 五个 EXE 只保留组合根；业务代码按 `domain → application → ports → infrastructure/delivery` 组织到 `internal/modules`。进程间链路逐条原子切换为回环 TCP + Protobuf；共享代码放入 `internal/shared`，它只是代码库，不承载业务或运行时服务。Agent 监听单一 Worker TCP 端点，Worker 携带实例 ID 主动回连注册；删除重启恢复以原始明确选择和当前事实重新计算，并使用新任务 ID 派发。

**Tech Stack:** Go 1.22、TCP、Protocol Buffers、SQLite、PostgreSQL/pgx、React 19、TypeScript 5.9、Vite 8、Vitest 4、TanStack React Virtual、Wails 2、PowerShell。

## Global Constraints

- 以以下四份文档为需求基线：
  - `docs/executable-target-layered-architecture.drawio`
  - `docs/superpowers/specs/2026-08-08-executable-target-layered-architecture-design.md`
  - `docs/superpowers/specs/2026-08-09-web-interaction-operation-design.md`
  - `docs/superpowers/specs/2026-08-09-performance-optimization-design.md`
- 保留五个既有 EXE 名称、启动职责和用户可见功能；`cmd/*` 最终只做参数解析、配置加载、依赖装配、启动和退出。
- `gui.exe`、`agent.exe`、`worker.exe`、`helper.exe`、`nodetray.exe` 各自读取自己的配置文件；任何进程不得依赖另一个进程把完整配置通过 IPC 注入。
- 浏览器只访问 GUI 的 HTTP + JSON API；GUI↔Agent、Agent↔Worker、Agent↔Helper、NodeTray↔Agent/Helper 统一为回环 TCP + Protobuf。
- Agent 只监听一个 Worker TCP 端点；每个 Worker 使用唯一 `instance_id` 主动回连并注册。不得恢复“每 Worker 一根命名管道”的结构。
- Agent 拥有 Worker 生命周期；NodeTray 只管理 Agent/Helper 进程。Worker 配置保存后，由 NodeTray 请求 Agent 排空并重建完整 Worker 池。
- 删除中断恢复不得重放旧任务：从原始明确选择和当前文件事实重新计算剩余项，创建新 `task_id`，再派发；不扩大选择、不改变删除模式、不复用旧 token/消息序号。
- 不处理跨分析版本任务协调。`analysis_run_id` 只用于分析 staging、原子发布、查询当前快照和旧结果清理。
- TCP 优先级只允许在完整帧边界调度；先采用单连接控制优先队列，只有性能验收实测控制延迟不达标时才拆分控制/数据连接。
- 不新增权限中心、审计中心、密钥中心等与本次目标无关的安全模块；保留现有必要的路径校验、单实例、进程归属和删除确认边界。
- `internal/shared` 只放 configuration、transport、logging、metrics、testkit 等复用代码；`testkit` 仅允许测试或开发工具导入，生产业务包不得依赖它。
- 使用绞杀式迁移：一条 EXE 链路的生产端和消费端在同一任务内切换；通过对应测试后立即删除该链路旧入口，禁止长期双写或双协议运行。
- 实施前使用 `superpowers:using-git-worktrees` 建立独立工作树。每个任务按 RED → GREEN → 定向测试 → 提交执行，保留可回退提交点。
- 审查上限：不创建独立的循环审查任务，不要求每项任务重复全仓验证。只保留任务内定向测试、Task 3 后阶段门 A、Task 7 后阶段门 B、Task 11 最终门；失败只修复当前门发现的问题并复跑该门。
- 当前生成计划的终端未发现可直接调用的 `go`。执行者必须先确认 `go version` 为 1.22 或更高；本计划中的测试命令是实施验收要求，不代表已在本次计划编写中运行。

---

## 目标目录与依赖规则

```text
cmd/
├── gui/main.go                 # Central 组合根
├── agent/main.go               # NodeAgent 组合根
├── worker/main.go              # MediaWorker 组合根
├── helper/main.go              # DeletionExecutor 组合根
└── nodetray/main.go            # NodeManagement 组合根

internal/modules/
├── central/                    # 节点、任务、查询、HTTP 用例编排
│   ├── domain/
│   ├── application/
│   ├── ports/
│   ├── infrastructure/
│   └── delivery/httpapi/
├── nodeagent/                  # 扫描、同步、Worker 调度、删除转发
├── mediaworker/                # 媒体计算流水线与本地缓存
├── analysis/                   # 第一阶段、第二阶段、分组发布
├── deletion/                   # 确认、操作、尝试、恢复和查询
└── nodemanagement/             # Agent/Helper 生命周期与配置应用

internal/shared/
├── configuration/              # JSON 读取、默认值、校验、规范化摘要
├── transport/tcpframe/         # 有界帧、超时、背压、帧边界优先级
├── logging/
├── metrics/
└── testkit/                    # 仅测试/开发工具

proto/
├── common/v1/common.proto
├── centralnode/v1/central_node.proto
├── worker/v1/worker.proto
├── deletion/v1/deletion.proto
└── control/v1/control.proto
```

依赖方向必须满足：

```text
delivery/infrastructure ──> application ──> domain
            │                    │
            └──────────────> ports <───────┘

cmd ──> modules/* + shared/*
modules/* ──> shared/*
shared/* -X-> modules/*
domain -X-> pgx / sqlite / net / http / wails
```

任务依赖：`1 → 2 → 3 → 4 → 5 → 6 → 7 → {8, 9} → 10 → 11`。Task 8 和 Task 9 可以在同一工作树中按顺序执行，但不要并行修改共享 PostgreSQL 查询和 schema 文件。

## 配置文件归属

| EXE | 默认配置 | 进程自读内容 | 其他进程可做的事 |
|---|---|---|---|
| `gui.exe` | `gui.json` | HTTP、PostgreSQL、分析、分页参数 | NodeTray 不编辑 |
| `agent.exe` | `agent.json` | GUI 端点、本地库、扫描、同步、Worker 池、Helper 端点 | NodeTray 原子保存并重启 Agent |
| `worker.exe` | `worker.json` | 流水线、缓存、FFmpeg/VideoCore、单 Worker 限额 | NodeTray 保存；Agent 重建 Worker 池 |
| `helper.exe` | `helper.json` | TCP 监听、允许根目录、删除超时、日志 | NodeTray 原子保存并重启 Helper |
| `nodetray.exe` | `%LocalAppData%/MySingerServer/nodetray.json` | 托盘偏好、组件路径、轮询间隔、启动行为 | 仅 NodeTray 自己读取和保存 |

所有相对路径以对应配置文件所在目录解析；命令行 `--config` 只覆盖配置文件位置，不覆盖业务字段。

---

### Task 1：建立共享配置内核和模块边界骨架

**Files:**
- Create: `internal/shared/configuration/loader.go`
- Create: `internal/shared/configuration/path.go`
- Create: `internal/shared/configuration/canonical.go`
- Create: `internal/shared/configuration/loader_test.go`
- Create: `internal/shared/configuration/path_test.go`
- Create: `internal/modules/central/doc.go`
- Create: `internal/modules/nodeagent/doc.go`
- Create: `internal/modules/mediaworker/doc.go`
- Create: `internal/modules/analysis/doc.go`
- Create: `internal/modules/deletion/doc.go`
- Create: `internal/modules/nodemanagement/doc.go`
- Create: `internal/shared/testkit/import_guard_test.go`

- [ ] **Step 1：写共享配置契约的失败测试**

覆盖：默认值先于 JSON 合并、未知字段拒绝、校验后返回规范值、相对路径以配置文件目录解析、同一有效配置产生稳定 SHA-256、无效 JSON 不返回半成品。

```go
type Snapshot[T any] struct {
    Path          string
    Value         T
    CanonicalJSON []byte
    SHA256        string
}

func LoadJSON[T any](
    path string,
    defaults func() T,
    validate func(*T) error,
) (Snapshot[T], error)
```

运行：

```powershell
go test -count=1 ./internal/shared/configuration -run 'TestLoadJSON|TestResolvePath'
```

预期：因包和函数尚不存在而失败。

- [ ] **Step 2：实现配置读取、路径解析和规范摘要**

`LoadJSON` 必须使用 `json.Decoder.DisallowUnknownFields()`；加载完成后调用模块自己的校验函数。`ResolvePath(configPath, value)` 对绝对路径只执行 `filepath.Clean`，对相对路径使用配置文件目录拼接。规范 JSON 和 SHA-256 只用于配置变更判断，不引入签名或权限系统。

- [ ] **Step 3：建立六个业务模块包说明和共享库导入守卫**

每个 `doc.go` 写明模块目的、拥有的数据、公开用例、不得承担的职责。`import_guard_test.go` 扫描 `internal/shared` 的 Go import，拒绝 `dedup/internal/modules/`；同时拒绝非 `_test.go` 文件导入 `dedup/internal/shared/testkit`。

- [ ] **Step 4：运行 Task 1 定向测试并提交**

```powershell
go test -count=1 ./internal/shared/...
git add internal/shared internal/modules
git commit -m "refactor: establish shared configuration and module boundaries"
```

---

### Task 2：建立 Protobuf 合约和共享 TCP 帧传输

**Files:**
- Create: `proto/common/v1/common.proto`
- Create: `proto/centralnode/v1/central_node.proto`
- Create: `proto/worker/v1/worker.proto`
- Create: `proto/deletion/v1/deletion.proto`
- Create: `proto/control/v1/control.proto`
- Create: `internal/gen/common/v1/common.pb.go`
- Create: `internal/gen/centralnode/v1/central_node.pb.go`
- Create: `internal/gen/worker/v1/worker.pb.go`
- Create: `internal/gen/deletion/v1/deletion.pb.go`
- Create: `internal/gen/control/v1/control.pb.go`
- Create: `internal/shared/transport/tcpframe/conn.go`
- Create: `internal/shared/transport/tcpframe/writer.go`
- Create: `internal/shared/transport/tcpframe/conn_test.go`
- Create: `internal/shared/transport/tcpframe/writer_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `scripts/build.ps1`

- [ ] **Step 1：先写帧协议和背压失败测试**

固定帧格式为 `4-byte big-endian length + protobuf payload`。测试必须覆盖：16 MiB 硬上限、部分读写、读写 deadline、连接关闭唤醒、消息数和字节数双上限、控制帧只在业务帧结束后抢占。

```go
type Priority uint8

const (
    PriorityBusiness Priority = iota
    PriorityControl
)

type Options struct {
    MaxFrameBytes   int
    TargetFrameBytes int
    QueueMessages   int
    QueueBytes      int64
    ReadTimeout     time.Duration
    WriteTimeout    time.Duration
}

type Conn interface {
    ReadFrame(context.Context) ([]byte, error)
    WriteFrame(context.Context, []byte, Priority) error
    Close() error
}
```

运行：

```powershell
go test -count=1 ./internal/shared/transport/tcpframe
```

预期：因实现不存在而失败。

- [ ] **Step 2：定义四条链路的 oneof Envelope**

公共字段只包括 `message_id`、`sent_at_unix_ms`、`correlation_id` 和稳定错误码。各链路 Envelope 独立，禁止建立包含所有业务消息的全局万能 Envelope。`worker.proto` 包含 Register/Ready/Job/SHAQuery/SHAReply/Result/Shutdown；`central_node.proto` 包含 Hello/Heartbeat/Task/Ack/Progress/Result；`deletion.proto` 包含 DeleteTask/DeleteProgress/DeleteResult/Shutdown；`control.proto` 包含 Status/Shutdown/ApplyWorkerConfig。

- [ ] **Step 3：生成 Go 代码并把生成命令固定进构建脚本**

在 `scripts/build.ps1` 增加显式 protobuf 生成/一致性检查步骤；提交生成的 `.pb.go`，发布构建不得依赖目标机存在 `protoc`。不在本任务删除 MessagePack，因为生产链路尚未切换。

- [ ] **Step 4：实现共享 TCP 帧层并验证 Protobuf 往返**

写队列满时返回稳定的 `ErrBackpressure`，不得无界阻塞或无界追加。TargetFrameBytes 是业务分片目标，不允许传输层拆开一个 Protobuf 消息；业务层必须在编码前拆分批次。

- [ ] **Step 5：运行 Task 2 定向测试并提交**

```powershell
go test -count=1 ./internal/shared/transport/tcpframe ./internal/gen/...
git add proto internal/gen internal/shared/transport go.mod go.sum scripts/build.ps1
git commit -m "refactor: add protobuf tcp transport foundation"
```

---

### Task 3：把 Agent↔Worker 切换为单端点主动注册

**Files:**
- Create: `internal/modules/mediaworker/config/config.go`
- Create: `internal/modules/mediaworker/config/config_test.go`
- Create: `internal/modules/mediaworker/application/runtime.go`
- Create: `internal/modules/mediaworker/infrastructure/agentclient/client.go`
- Create: `internal/modules/nodeagent/application/workerpool/pool.go`
- Create: `internal/modules/nodeagent/application/workerpool/registry.go`
- Create: `internal/modules/nodeagent/application/workerpool/pool_test.go`
- Create: `internal/modules/nodeagent/infrastructure/workertcp/listener.go`
- Create: `internal/modules/nodeagent/infrastructure/workertcp/listener_test.go`
- Modify: `cmd/worker/main.go`
- Modify: `cmd/agent/main.go`
- Modify: `internal/config/agent.go`
- Modify: `internal/config/config_test.go`
- Move: `internal/wproc/*` → `internal/modules/mediaworker/infrastructure/pipeline/`
- Move: `internal/worker/deduper.go` → `internal/modules/nodeagent/application/workerpool/deduper.go`
- Delete after cutover: `internal/worker/ipc.go`
- Delete after cutover: `internal/worker/messages.go`

- [ ] **Step 1：写 Worker 独立配置失败测试**

把当前 `WPROC_*` 环境变量映射成 `worker.json` 字段；环境变量不再是生产配置入口。保留测试注入点，但生产 `cmd/worker` 必须先读取自己的配置。

```go
type Config struct {
    SchemaVersion int            `json:"schemaVersion"`
    Pipeline      PipelineConfig `json:"pipeline"`
    Cache         CacheConfig    `json:"cache"`
    Limits        LimitsConfig   `json:"limits"`
}

func Load(path string) (configuration.Snapshot[Config], error)
```

运行：

```powershell
go test -count=1 ./internal/modules/mediaworker/config
```

预期：因新加载器不存在而失败。

- [ ] **Step 2：写单监听器和实例注册失败测试**

测试：两个 Worker 用不同 `instance_id` 并发注册到同一监听地址；未知 ID、重复 ID、PID/slot 不匹配和超时注册被拒绝；连接断开只回收对应实例；Worker 意外退出后 Agent 使用新 `instance_id` 拉起替代实例。

```go
type ExpectedInstance struct {
    InstanceID string
    Slot       int
    PID        int
}

type Registration struct {
    InstanceID string
    Slot       int
    PID        int
    Conn       tcpframe.Conn
}

type Registry interface {
    Expect(ExpectedInstance) error
    Register(context.Context, *workerv1.Register, tcpframe.Conn) (Registration, error)
    Remove(instanceID string)
}
```

- [ ] **Step 3：实现 Agent 单一 Worker listener 和主动回连参数**

Agent 启动时绑定 `worker.listenAddr`（默认回环地址端口 0），取得实际端点后启动 Worker：

```text
worker.exe --config <worker.json> --agent-endpoint <ip:port> \
  --instance-id <uuid> --worker-index <n>
```

Worker 连接成功后第一帧必须是 Register，注册成功后再发送 Ready。Agent 维护 `instance_id → slot/PID/session` 映射；不把 TCP 源端口当身份。

- [ ] **Step 4：迁移媒体流水线并切断旧命名管道**

将 `wproc.Run(pipe,index)` 改为 `mediaworker.Run(ctx, configPath, endpoint, instanceID, index)`；现有 FFmpeg、VideoCore、MediaCore、缩略图和 phase2 行为原样迁移。完成同一提交的生产切换后删除 Worker 命名管道 dial/listen 和 MessagePack 消息类型。

- [ ] **Step 5：运行 Worker 链路测试**

```powershell
go test -count=1 ./internal/modules/mediaworker/... ./internal/modules/nodeagent/application/workerpool ./internal/modules/nodeagent/infrastructure/workertcp ./cmd/worker ./cmd/agent
```

预期：配置、双实例注册、任务往返、退出回收和流水线既有测试全部通过。

- [ ] **Step 6：执行阶段门 A（仅一次）并提交**

```powershell
go test -count=1 ./internal/modules/... ./internal/shared/... ./cmd/worker ./cmd/agent
git add cmd internal go.mod go.sum
git commit -m "refactor: register workers over one agent tcp endpoint"
```

阶段门 A 只证明共享基础和 Worker 链路闭合；不提前审查 GUI、Helper 或 Web。

---

### Task 4：抽取 NodeAgent 扫描应用层并落实有界执行

**Files:**
- Create: `internal/modules/nodeagent/domain/scan.go`
- Create: `internal/modules/nodeagent/application/scan/service.go`
- Create: `internal/modules/nodeagent/application/scan/limiter.go`
- Create: `internal/modules/nodeagent/ports/localstore.go`
- Create: `internal/modules/nodeagent/ports/enumerator.go`
- Create: `internal/modules/nodeagent/ports/central.go`
- Create: `internal/modules/nodeagent/infrastructure/sqlite/store.go`
- Create: `internal/modules/nodeagent/infrastructure/sqlite/schema.go`
- Create: `internal/modules/nodeagent/infrastructure/enumerator/adapter.go`
- Create: `internal/modules/nodeagent/application/scan/service_test.go`
- Move: `internal/agent/scan.go` → `internal/modules/nodeagent/application/scan/legacy_scan.go`
- Move: `internal/agent/limiter.go` → `internal/modules/nodeagent/application/scan/legacy_limiter.go`
- Modify: `cmd/agent/main.go`
- Modify: `internal/store/ddl.go`

- [ ] **Step 1：写扫描批量落库和有界调度失败测试**

测试 100 万条枚举输入时应用层只保留配置的批次和窗口；任务排队量不得超过 `pendingJobs`；上下文取消后停止继续枚举；SQLite 写入按批次事务提交；重启后从 generation 的数据库状态继续调度而不重做已完成媒体计算。

```go
type Limits struct {
    EnumerateBatch int
    PendingJobs    int
    WorkerInflight int
    SyncBatch      int
}

type LocalStore interface {
    BeginGeneration(context.Context, Scan) (GenerationID, error)
    UpsertEnumerated(context.Context, GenerationID, []FileCandidate) error
    NextPending(context.Context, GenerationID, PendingCursor, int) ([]PendingFile, PendingCursor, error)
    Complete(context.Context, GenerationID, FileResult) error
}
```

- [ ] **Step 2：实现 domain/application/ports，不让应用层依赖 sqlite/pgx/net**

`scan.Service` 只通过端口调用枚举器、本地仓储、Worker 池和 Central 客户端。原 `internal/enum`、`internal/store`、`internal/syncer` 先作为 infrastructure adapter 接入；业务状态和并发窗口进入 application。

- [ ] **Step 3：把去重缓存改为容量 + TTL 双限制**

缓存键和命中语义保持现状，但实现必须在插入时执行过期和容量淘汰；指标暴露当前条目数、命中率、淘汰次数，不记录文件路径标签。

- [ ] **Step 4：运行 Task 4 定向测试并提交**

```powershell
go test -count=1 ./internal/modules/nodeagent/... ./internal/enum ./internal/store ./internal/syncer ./cmd/agent
git add cmd/agent internal/modules/nodeagent internal/enum internal/store internal/syncer
git commit -m "refactor: extract bounded node agent scan application"
```

---

### Task 5：迁移 GUI↔Agent TCP + Protobuf 并持久化出站任务

**Files:**
- Create: `internal/modules/central/domain/node.go`
- Create: `internal/modules/central/domain/task.go`
- Create: `internal/modules/central/application/nodes/service.go`
- Create: `internal/modules/central/application/tasks/service.go`
- Create: `internal/modules/central/ports/nodes.go`
- Create: `internal/modules/central/ports/tasks.go`
- Create: `internal/modules/central/infrastructure/postgres/node_repository.go`
- Create: `internal/modules/central/infrastructure/postgres/task_repository.go`
- Create: `internal/modules/central/infrastructure/agenttcp/server.go`
- Create: `internal/modules/central/infrastructure/agenttcp/server_test.go`
- Create: `internal/modules/nodeagent/infrastructure/centraltcp/client.go`
- Create: `internal/modules/nodeagent/infrastructure/centraltcp/client_test.go`
- Modify: `cmd/gui/main.go`
- Modify: `cmd/agent/main.go`
- Modify: `internal/store/ddl.go`

- [ ] **Step 1：写握手、心跳、任务恢复失败测试**

覆盖 `machine_id` 握手、重复连接替换、断线退避重连、心跳超时、任务 ack/progress/result、Agent SQLite 未同步队列恢复、GUI PostgreSQL 待派发任务恢复。每个链路只恢复自己拥有的状态，消息 ID 只做同一任务内幂等。

- [ ] **Step 2：定义 Central 与 NodeAgent 端口**

```go
type NodeSessionPort interface {
    SendTask(context.Context, MachineID, TaskEnvelope) error
    Disconnect(MachineID, error)
}

type Outbox interface {
    Enqueue(context.Context, OutboundMessage) error
    Next(context.Context, int) ([]OutboundMessage, error)
    MarkAcknowledged(context.Context, MessageID) error
}
```

GUI 的 application 不直接持有 TCP 连接；Agent 的 scan/sync application 不直接编码 Protobuf。

- [ ] **Step 3：实现双方 TCP adapter 和持久化恢复**

GUI 监听配置中的 Agent 端点；Agent 主动连接 GUI。连接恢复后先握手，再按本地 outbox 顺序发送未确认消息。批量文件同步按 `TargetFrameBytes` 在业务层拆分，单帧超限在编码前被拒绝。

- [ ] **Step 4：在同一任务切换生产组合根并删除该链路旧入口**

更新 `cmd/gui` 和 `cmd/agent` 只装配新 adapter。移除 GUI↔Agent 旧 MessagePack 连接使用点；保留 `internal/proto` 中仍被 Helper/其他链路使用的类型，待相应任务切换后再删。

- [ ] **Step 5：运行 Task 5 定向测试并提交**

```powershell
go test -count=1 ./internal/modules/central/... ./internal/modules/nodeagent/... ./cmd/gui ./cmd/agent
git add cmd internal/modules internal/store
git commit -m "refactor: migrate central node link to protobuf tcp"
```

---

### Task 6：迁移 Agent↔Helper 删除执行链路

**Files:**
- Create: `internal/modules/deletion/domain/task.go`
- Create: `internal/modules/deletion/ports/executor.go`
- Create: `internal/modules/deletion/infrastructure/helpertcp/client.go`
- Create: `internal/modules/deletion/infrastructure/helpertcp/client_test.go`
- Create: `internal/modules/deletion/infrastructure/helperserver/server.go`
- Create: `internal/modules/deletion/infrastructure/helperserver/server_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/helper/main.go`
- Modify: `internal/helper/config.go`
- Delete after cutover: `internal/helper/pipe_windows.go`
- Delete after cutover: `internal/agent/delete/forwarder.go`

- [ ] **Step 1：写 Helper 独立配置和 TCP 重连失败测试**

Helper 配置新增显式回环 `listenAddr`，仍由 Helper 自己读取 `helper.json`。测试 Helper 重启时 Agent 只重新连接并恢复“尚未取得明确终态”的新派发；不得由 transport 自动重放旧 DeleteTask。

- [ ] **Step 2：实现删除 Protobuf adapter**

```go
type Executor interface {
    Execute(context.Context, DeleteAttempt) (<-chan Progress, error)
    Shutdown(context.Context) error
}

type DeleteAttempt struct {
    TaskID      string
    OperationID string
    Mode        DeleteMode
    Items       []DeleteItem
}
```

Helper server 只校验配置允许根目录并执行传入项；不查询 PostgreSQL、不决定选择范围、不创建恢复任务。

- [ ] **Step 3：切换 Agent/Helper 组合根并移除命名管道**

Agent 通过 `helpertcp.Client` 发送新任务，Helper 用共享 `tcpframe` 监听。完成生产切换后删除 Helper 业务命名管道 listener 和旧 MessagePack forwarder；保留 NodeTray 控制链路到 Task 10。

- [ ] **Step 4：运行 Task 6 定向测试并提交**

```powershell
go test -count=1 ./internal/modules/deletion/... ./internal/helper ./cmd/helper ./cmd/agent
git add cmd internal/modules/deletion internal/helper internal/agent
git commit -m "refactor: execute deletion over protobuf tcp"
```

---

### Task 7：持久化删除操作并实现重算后重新派发

**Files:**
- Create: `internal/modules/deletion/domain/operation.go`
- Create: `internal/modules/deletion/application/service.go`
- Create: `internal/modules/deletion/application/recovery.go`
- Create: `internal/modules/deletion/application/service_test.go`
- Create: `internal/modules/deletion/ports/repository.go`
- Create: `internal/modules/deletion/infrastructure/postgres/schema.go`
- Create: `internal/modules/deletion/infrastructure/postgres/repository.go`
- Create: `internal/modules/deletion/infrastructure/postgres/repository_integration_test.go`
- Create: `internal/modules/central/delivery/httpapi/delete_handlers.go`
- Create: `internal/modules/central/delivery/httpapi/delete_handlers_test.go`
- Modify: `cmd/gui/main.go`
- Modify: `webui/src/api/contracts.ts`
- Modify: `webui/src/api/appApi.ts`
- Modify: `webui/src/features/deletion/DeleteDialog.tsx`
- Modify: `webui/src/features/deletion/DeleteStatusPanel.tsx`
- Delete after cutover: in-memory task ownership in `internal/gui/delete.go`

- [ ] **Step 1：先写删除状态机和恢复失败测试**

状态机固定为：

```text
operation: prepared -> running -> completed | partial | failed | interrupted
attempt:   queued -> dispatched -> running -> completed | failed | interrupted
```

测试必须证明：确认时持久化原始明确选择；启动扫描到 `running/interrupted` 操作；恢复时只从原始选择中计算当前仍存在、仍属于目标节点且未完成的项；每次恢复创建新的 `task_id` 和递增 `attempt_no`；旧任务迟到结果不能覆盖新尝试；空剩余集直接完成操作。

- [ ] **Step 2：建立 PostgreSQL 表和仓储端口**

```sql
CREATE TABLE IF NOT EXISTS delete_operations (
    operation_id uuid PRIMARY KEY,
    mode text NOT NULL,
    status text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS delete_operation_items (
    operation_id uuid NOT NULL REFERENCES delete_operations(operation_id),
    file_id bigint NOT NULL,
    machine_id text NOT NULL,
    original_path text NOT NULL,
    state text NOT NULL,
    PRIMARY KEY (operation_id, file_id)
);

CREATE TABLE IF NOT EXISTS delete_attempts (
    task_id uuid PRIMARY KEY,
    operation_id uuid NOT NULL REFERENCES delete_operations(operation_id),
    attempt_no integer NOT NULL,
    status text NOT NULL,
    created_at timestamptz NOT NULL,
    finished_at timestamptz,
    UNIQUE (operation_id, attempt_no)
);
```

Repository 的 `RecalculateRemaining` 必须 join 当前 `files` 事实，但禁止重新运行分组搜索后扩大选择。

- [ ] **Step 3：实现 GUI 删除用例和启动恢复器**

`POST /api/delete/confirmations` 返回确认摘要；`POST /api/delete/operations` 返回 `operationId + taskId`；`GET /api/delete/operations/:id` 返回操作聚合状态和 attempts；`GET /api/delete/tasks` 返回可恢复历史。GUI 启动完成数据库连接后运行一次 recovery scan，不做固定高频轮询。

- [ ] **Step 4：适配 Web 删除操作逻辑**

弹窗仍执行“选择 → 预检查 → 二次确认 → 提交”；提交后以 `operationId` 跟踪，不再把单次 `taskId` 当长期操作身份。刷新页面后从查询 API 恢复进行中状态。

- [ ] **Step 5：运行 Task 7 定向测试**

```powershell
go test -count=1 ./internal/modules/deletion/... ./internal/modules/central/... ./cmd/gui
npm test --prefix webui -- --run src/features/deletion src/api/appApi.test.ts
```

- [ ] **Step 6：执行阶段门 B（仅一次）并提交**

```powershell
go test -count=1 ./internal/modules/central/... ./internal/modules/nodeagent/... ./internal/modules/deletion/... ./cmd/gui ./cmd/agent ./cmd/helper
npm test --prefix webui -- --run src/features/deletion src/api/appApi.test.ts
git add cmd internal webui
git commit -m "refactor: persist and recover deletion operations"
```

阶段门 B 只确认三条核心进程链路和删除一致性；不重复阶段门 A 的底层压力测试。

---

### Task 8：把分组查询与 Web 列表切换为游标分页

**Files:**
- Create: `internal/modules/central/domain/group.go`
- Create: `internal/modules/central/application/groups/query.go`
- Create: `internal/modules/central/ports/groups.go`
- Create: `internal/modules/central/infrastructure/postgres/group_repository.go`
- Create: `internal/modules/central/infrastructure/postgres/group_repository_integration_test.go`
- Create: `internal/modules/central/delivery/httpapi/group_handlers.go`
- Create: `internal/modules/central/delivery/httpapi/group_handlers_test.go`
- Modify: `webui/src/api/contracts.ts`
- Modify: `webui/src/api/appApi.ts`
- Replace: `webui/src/hooks/usePagedGroups.ts` → `webui/src/hooks/useCursorGroups.ts`
- Modify: `webui/src/hooks/hooks.test.tsx`
- Modify: `webui/src/features/groups/GroupsPage.tsx`
- Modify: `webui/src/features/groups/GroupsPage.test.tsx`
- Delete after cutover: numeric page query path in `internal/gui/groups.go`

- [ ] **Step 1：写稳定游标和查询计划失败测试**

游标包含排序键、稳定 ID、筛选摘要和当前发布的 `analysis_run_id`；它只用于查询快照失效，不参与任务协调。测试前后翻页无重复/遗漏、筛选或排序变化使旧游标失效、发布切换返回 `cursor_expired`、`size` 被限制在配置上限内。

```go
type GroupCursor struct {
    Sort          string `json:"s"`
    Primary       int64  `json:"p"`
    GroupID       int64  `json:"g"`
    FilterHash    string `json:"f"`
    AnalysisRunID string `json:"r"`
}

type GroupPage struct {
    Items      []GroupSummary `json:"items"`
    NextCursor string         `json:"nextCursor,omitempty"`
    HasMore    bool           `json:"hasMore"`
}
```

- [ ] **Step 2：实现 keyset SQL 和轻量摘要查询**

列表请求只查分组摘要；成员和缩略图按展开/进入详情后懒加载。排序条件必须使用稳定 tie-breaker `group_id`，禁止深页 `OFFSET`。旧 `page` 参数只保留一个发布周期的兼容 adapter，内部转换受最大页数限制且 Web 不再调用。

- [ ] **Step 3：把 React 状态改为游标链**

`useCursorGroups` 保存 `currentCursor`、`nextCursor`、返回栈和有限页缓存；查询/筛选/排序变化时原子清空选择与游标。继续使用 TanStack Virtual，只渲染可视区；详情、预览、删除弹窗和 Esc/返回逻辑沿用 Web 设计文档。

- [ ] **Step 4：运行 Task 8 定向测试并提交**

```powershell
go test -count=1 ./internal/modules/central/... ./cmd/gui -run 'TestGroup|TestCursor'
npm test --prefix webui -- --run src/hooks src/features/groups src/api/appApi.test.ts
npm run build --prefix webui
git add internal/modules/central cmd/gui webui
git commit -m "perf: use cursor pagination for group browsing"
```

---

### Task 9：落实分析与数据库性能方案

**Files:**
- Create: `internal/modules/analysis/domain/run.go`
- Create: `internal/modules/analysis/application/runner.go`
- Create: `internal/modules/analysis/application/runner_test.go`
- Create: `internal/modules/analysis/ports/repository.go`
- Create: `internal/modules/analysis/infrastructure/postgres/schema.go`
- Create: `internal/modules/analysis/infrastructure/postgres/repository.go`
- Create: `internal/modules/analysis/infrastructure/postgres/repository_integration_test.go`
- Move: `internal/firstscreen/*` → `internal/modules/analysis/infrastructure/firstscreen/`
- Move: `internal/phase2/*` → `internal/modules/analysis/infrastructure/phase2/`
- Modify: `cmd/gui/main.go`
- Modify: `internal/config/gui.go`
- Modify: `cmd/benchscreen/main.go`
- Modify: `cmd/perfreport/main.go`

- [ ] **Step 1：写有界分析和原子发布失败测试**

测试读取页、候选批次、并发计算、批量写入均受配置限制；取消后停止产生新批次；失败 run 不改变 current 指针；成功 run 在单事务内切换 current；旧 run 仅在无查询引用且超过保留时间后清理。

```go
type RunRepository interface {
    Begin(context.Context) (RunID, error)
    WriteGroupBatch(context.Context, RunID, []Group) error
    Publish(context.Context, RunID) error
    Current(context.Context) (RunID, error)
    Cleanup(context.Context, time.Time, int) (int64, error)
}

type Limits struct {
    ReadPage       int
    CandidateBatch int
    ComputeWorkers int
    WriteBatch     int
    MaxInflight    int
}
```

- [ ] **Step 2：添加 staging/current schema 与查询索引**

分析结果表增加 `analysis_run_id`；建立 current-run 指针表；按分组列表排序组合建立索引；文本搜索沿用 PostgreSQL `pg_trgm`（若现有部署已启用）并为未启用环境提供明确启动错误，不回退到全表模糊扫描。列表摘要字段预聚合写入 run 结果。

- [ ] **Step 3：迁移第一阶段和第二阶段到 analysis 模块**

保留算法和阈值语义，重构批次边界和仓储调用。`analysis_run_id` 不进入 Agent/Worker 任务身份判断；Worker phase2 结果按当前分析运行上下文由 GUI 的 adapter 绑定，旧运行失败只丢弃该 staging run。

- [ ] **Step 4：补充基准命令的预算断言**

`benchscreen` 和 `perfreport` 输出至少包含吞吐、P95 批次耗时、最大在途项、数据库批次数、进程内存峰值。门槛从性能设计文档读取并集中到测试配置，禁止硬编码成测试机器专属绝对数值。

- [ ] **Step 5：运行 Task 9 定向测试并提交**

```powershell
go test -count=1 ./internal/modules/analysis/... ./cmd/gui ./cmd/benchscreen ./cmd/perfreport
go test -count=1 ./internal/modules/analysis/... -run 'TestBounded|TestPublish|TestCleanup'
git add internal/modules/analysis internal/config cmd
git commit -m "perf: bound analysis and publish results atomically"
```

---

### Task 10：完成 NodeTray 配置闭环和控制链路迁移

**Files:**
- Create: `internal/modules/nodemanagement/domain/component.go`
- Create: `internal/modules/nodemanagement/application/service.go`
- Create: `internal/modules/nodemanagement/application/service_test.go`
- Create: `internal/modules/nodemanagement/ports/control.go`
- Create: `internal/modules/nodemanagement/infrastructure/agenttcp/client.go`
- Create: `internal/modules/nodemanagement/infrastructure/helpertcp/client.go`
- Create: `internal/modules/nodemanagement/infrastructure/jsonconfig/nodetray.go`
- Create: `internal/modules/nodemanagement/infrastructure/jsonconfig/worker.go`
- Create: `internal/modules/nodeagent/delivery/controltcp/server.go`
- Create: `internal/modules/deletion/delivery/controltcp/server.go`
- Modify: `internal/nodetray/config/store.go`
- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/supervisor/supervisor.go`
- Modify: `nodetray/composition.go`
- Modify: `nodetray/frontend/src/bindings/backend.ts`
- Create: `nodetray/frontend/src/pages/WorkerPage.tsx`
- Create: `nodetray/frontend/src/pages/WorkerPage.test.tsx`
- Modify: `nodetray/frontend/src/pages/SettingsPage.tsx`
- Modify: `nodetray/frontend/src/pages/OverviewPage.tsx`
- Delete after cutover: production named-pipe control usage in `internal/nodectl`
- Delete after cutover: production named-pipe providers in `internal/agentcontrol` and `internal/helpercontrol`

- [ ] **Step 1：写组件级配置状态失败测试**

```go
type ConfigTarget string

const (
    AgentConfig ConfigTarget = "agent"
    WorkerConfig ConfigTarget = "worker"
    HelperConfig ConfigTarget = "helper"
    TrayConfig ConfigTarget = "nodetray"
)

type ConfigState struct {
    Target          ConfigTarget
    DiskSHA256      string
    RuntimeSHA256   string
    NeedsRestart    bool
    ApplyInProgress bool
}
```

测试保存 Agent/Helper 配置只标记对应组件；保存 Worker 配置标记 `workerPool`；Agent 报告所有新 Worker 的有效摘要与磁盘摘要一致后才清除 `NeedsRestart`；失败时保留标记并显示稳定错误。

- [ ] **Step 2：让 NodeTray 独立读取 nodetray.json 并扩展 Worker 配置 Store**

NodeTray 启动最先加载自己的配置，再解析 Agent/Worker/Helper 文件位置。Store 保存使用现有规范 JSON + 原子替换策略；Worker 表单只暴露性能设计中允许用户调整的线程、缓存、帧批次和工具路径字段。

- [ ] **Step 3：实现 ApplyWorkerConfig 控制用例**

顺序固定：

```text
NodeTray 原子保存 worker.json
→ 标记 needsRestart(workerPool)
→ TCP 请求 Agent ApplyWorkerConfig(expected_sha256)
→ Agent 停止接收新任务并等待在途任务到达超时边界
→ Agent 关闭完整 Worker 池
→ Agent 重新读取自己的 agent.json 以取得池参数
→ Agent 启动新 Worker；每个 Worker 独立读取 worker.json 并注册
→ Agent 汇总有效摘要并回复 NodeTray
→ 摘要一致才清除 needsRestart(workerPool)
```

NodeTray 不直接启动、停止或强杀 Worker。

- [ ] **Step 4：迁移 Agent/Helper 控制面到 TCP + Protobuf**

状态、优雅关闭和 Worker 配置应用使用 `control.proto`；Agent/Helper 各自监听独立回环控制端点。保留已有单实例和可信进程兜底，但删除 NodeTray 生产代码对命名管道控制 provider 的依赖。

- [ ] **Step 5：实现 Worker 配置页面和状态反馈**

新增 Worker 页面，沿用 Agent/Helper 页的脏表单、保存、放弃和错误展示。Overview 显示 Worker 池数量、运行状态、磁盘/运行时配置摘要是否一致；不新增独立“安全中心”页面。

- [ ] **Step 6：运行 Task 10 定向测试并提交**

```powershell
go test -count=1 ./internal/modules/nodemanagement/... ./internal/modules/nodeagent/delivery/controltcp ./internal/modules/deletion/delivery/controltcp ./internal/nodetray/... ./nodetray ./cmd/agent ./cmd/helper
npm test --prefix nodetray/frontend -- --run src/pages/WorkerPage.test.tsx src/pages/SettingsPage.test.tsx src/pages/OverviewPage.test.tsx
npm run build --prefix nodetray/frontend
git add internal nodetray cmd/agent cmd/helper
git commit -m "refactor: apply component configs through node tray"
```

---

### Task 11：收口旧包、依赖、文档和最终性能验收

**Files:**
- Create: `internal/shared/logging/logging.go`
- Create: `internal/shared/metrics/metrics.go`
- Create: `internal/shared/metrics/metrics_test.go`
- Create: `internal/shared/testkit/tcp.go`
- Modify: `cmd/gui/main.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/worker/main.go`
- Modify: `cmd/helper/main.go`
- Modify: `nodetray/main.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `scripts/build.ps1`
- Modify: `scripts/package-node-release.ps1`
- Modify: `README.md`
- Modify: `docs/current-project-architecture.md`
- Modify: `docs/superpowers/specs/2026-08-08-executable-target-layered-architecture-design.md`
- Delete if no imports remain: `internal/proto/`
- Delete if no imports remain: `internal/nodectl/`
- Delete or reduce to compatibility-free adapters: `internal/agent/`, `internal/gui/`, `internal/worker/`, `internal/wproc/`, `internal/firstscreen/`, `internal/phase2/`

- [ ] **Step 1：写最终 import 和旧协议静态守卫**

测试扫描生产 Go 文件，保证：`cmd` 不含业务 SQL/状态机；domain 不导入 `net/http/pgx/sqlite/wails`；shared 不导入 modules；生产代码不导入 MessagePack 或 go-winio；不存在 `--pipe` Worker 参数；五个 EXE 都有 `--config` 或固定且可测试的自有配置解析路径。

- [ ] **Step 2：合并共享日志、指标和测试工具**

指标只提供进程内接口和现有日志输出适配，不新增监控服务。统一指标名至少包括 TCP 队列字节、控制帧延迟、Worker 在途任务、扫描 pending、分析批次、游标查询耗时、删除操作状态。文件路径、token 和完整配置不得作为指标标签。

- [ ] **Step 3：删除旧实现和不再使用的依赖**

先运行：

```powershell
rg -n 'internal/(proto|nodectl|agent|gui|worker|wproc|firstscreen|phase2)' --glob '*.go'
rg -n 'msgpack|go-winio|named pipe|--pipe' --glob '*.go' --glob 'go.mod'
```

逐项确认无生产 import 后删除旧目录或空壳；若基准工具仍需旧包，先改为导入 `internal/modules`。执行 `go mod tidy`，只移除已无引用的 MessagePack/go-winio 依赖。

- [ ] **Step 4：更新项目启动描述和构建打包**

`docs/current-project-architecture.md` 写入每个模块的设计目的、实现方法、配置归属、公开端口和禁止职责，使 Codex 启动时能读取当前事实。`README.md` 和示例配置列出五个配置文件。构建脚本生成并校验五个 EXE、五份示例配置、Web 静态资源和 Protobuf 生成一致性。

- [ ] **Step 5：执行唯一最终门**

```powershell
go test -count=1 ./...
go vet ./...
npm test --prefix webui
npm run build --prefix webui
npm test --prefix nodetray/frontend
npm run build --prefix nodetray/frontend
pwsh -NoProfile -File .\scripts\build.ps1
```

随后只运行性能方案已有的基准入口，不新增第二套性能评审流程：

```powershell
go test -count=1 ./internal/modules/analysis/... -run 'TestBounded|TestPublish'
go test -count=1 ./internal/modules/central/... -run 'TestCursor|TestGroupQuery'
go test -count=1 ./internal/modules/nodeagent/... -run 'TestBounded|TestBackpressure'
```

通过标准：所有命令退出码为 0；没有无界队列/深页 OFFSET；控制帧延迟达到性能文档目标。若控制延迟不达标，才创建一个后续实现任务拆分控制/数据连接，不在本计划内预先增加复杂度。

- [ ] **Step 6：提交最终收口**

```powershell
git add cmd internal proto webui nodetray scripts README.md docs go.mod go.sum
git commit -m "refactor: complete layered executable architecture"
```

---

## 最终验收矩阵

| 场景 | 预期结果 | 验证位置 |
|---|---|---|
| 启动五个 EXE | 各自读取自己的配置，错误指向对应文件和字段 | `cmd/*`、NodeTray composition tests |
| 多 Worker 注册 | 单 Agent 端点接收多个唯一实例，错误实例被拒绝 | WorkerPool/TCP integration tests |
| GUI-Agent 断线 | 双方按各自持久化状态恢复，不丢待确认任务 | Central/NodeAgent integration tests |
| Helper 重启 | transport 不重放旧任务；删除应用重算后派发新 task ID | Deletion recovery tests |
| 删除页面刷新 | 通过 operation ID 恢复状态和 attempts | Web deletion tests |
| 百万级分组浏览 | 游标分页、稳定排序、虚拟列表，无深页 OFFSET | Group query/Web tests |
| 分析失败 | current 结果不变，失败 staging 可清理 | Analysis publish tests |
| Worker 配置应用 | NodeTray 保存后由 Agent 排空并重建完整池 | NodeManagement tests |
| 退出 NodeTray | Helper → Agent 优雅退出，可信进程兜底保持既有顺序 | Existing lifecycle tests |
| 构建发布 | 五个 EXE、Web 资源、配置示例和清单齐全 | `scripts/build.ps1` |

## 非目标

- 不引入微服务注册中心、消息队列、服务网格或远程配置中心。
- 不引入额外安全、权限、审计模块。
- 不同时支持 MessagePack 和 Protobuf 的长期兼容运行。
- 不处理跨分析版本任务迁移或回放。
- 不改变现有媒体算法、删除模式、用户确认语义和 NodeTray 可信退出边界。
- 不为“审查计划”再生成审查计划；实施过程以本文件的三个门为上限。
