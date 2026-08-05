# 节点组件本机控制面实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development for every task and superpowers:verification-before-completion before reporting completion.

**Goal:** 为 `agent.exe` 和 `helper.exe` 增加相互隔离、仅限本机的状态查询与受控关闭控制面，使后续托盘程序能够可信地认领组件、展示状态并复用既有 drain/清理逻辑。

**Architecture:** 新增独立的 `internal/nodectl` 有界帧协议和 Windows 命名管道传输；Agent 与删除 Helper 分别托管控制服务端，并把 `shutdown` 映射到各自根上下文的取消函数。控制命令不进入 Agent TCP 端口，也不复用 Helper 删除事务管道。状态由进程内部提供，Worker 仍完全由 Agent 管理。

**Tech Stack:** Go 1.22、`github.com/Microsoft/go-winio` 0.6.2、`github.com/vmihailenco/msgpack/v5` 5.4.1、`golang.org/x/sys` 0.28.0、Windows Named Pipe、Go `testing`。

**前置设计:** [媒体节点托盘管理程序设计](../specs/2026-08-02-node-tray-design.md)

**后续计划:** [节点托盘后端实施计划](2026-08-02-node-tray-backend.md)、[节点托盘 UI、构建与验收实施计划](2026-08-02-node-tray-ui-release.md)

---

## 全局约束

- 不在 Agent 现有 TCP 协议中加入 `status`、`shutdown`、`start` 或 `restart`。
- Agent 与 Helper 使用两个固定、不同的命名管道；Helper 控制管道不复用删除管道。
- 控制面只提供 `status` 与 `shutdown`。启动、重启由后续托盘监督器在本机完成。
- 单帧上限固定为 1 MiB，以容纳最多 1024 个 Worker 的有界只读快照；字符串字段和 Worker 数组均必须有长度上限；协议版本固定为 `1`。
- 响应只包含脱敏摘要，不返回 DSN、密码、环境变量或原始配置。
- `shutdown` 只触发现有受控退出路径；本计划不加入自动强杀。
- Worker 的启动、停止和重生仍由 Agent 的 Worker Pool 独占管理。
- 当前检出目录没有 `.git` 元数据。执行每个提交检查点前先运行 `git rev-parse --is-inside-work-tree`；若失败，在执行记录中写 `N/A_NO_GIT_METADATA`，不得伪造提交成功。

## 跨计划冻结接口

后续托盘后端只能依赖本计划暴露的 `internal/nodectl.Client` 和下列模型，不直接读取 Agent/Helper 私有状态：

```go
package nodectl

type Component string

const (
	ComponentAgent  Component = "agent"
	ComponentHelper Component = "delete-helper"
)

type Command string

const (
	CommandStatus   Command = "status"
	CommandShutdown Command = "shutdown"
)

type WorkerStatus struct {
	Index              int    `msgpack:"index"`
	PID                int    `msgpack:"pid"`
	Ready              bool   `msgpack:"ready"`
	CurrentTaskSummary string `msgpack:"current_task_summary"`
	LastErrorSummary   string `msgpack:"last_error_summary"`
}

type Request struct {
	Version   uint16  `msgpack:"version"`
	RequestID string  `msgpack:"request_id"`
	Command   Command `msgpack:"command"`
}

type Status struct {
	Component        Component `msgpack:"component"`
	MachineID        string    `msgpack:"machine_id"`
	PID              int       `msgpack:"pid"`
	StartedAtUnixMS  int64     `msgpack:"started_at_unix_ms"`
	ExecutablePath   string    `msgpack:"executable_path"`
	ConfigSHA256     string    `msgpack:"config_sha256"`
	Lifecycle        string    `msgpack:"lifecycle"`
	ServiceReady     bool      `msgpack:"service_ready"`
	Ready            bool      `msgpack:"ready"`
	WorkerExpected   int       `msgpack:"worker_expected"`
	WorkerReady      int       `msgpack:"worker_ready"`
	Workers          []WorkerStatus `msgpack:"workers"`
	SyncHealthy      bool      `msgpack:"sync_healthy"`
	SyncErrorSummary string    `msgpack:"sync_error_summary"`
	LastErrorSummary string    `msgpack:"last_error_summary"`
	ActiveRequests   int       `msgpack:"active_requests"`
}

type Response struct {
	Version      uint16  `msgpack:"version"`
	RequestID    string  `msgpack:"request_id"`
	OK           bool    `msgpack:"ok"`
	ErrorCode    string  `msgpack:"error_code"`
	ErrorSummary string  `msgpack:"error_summary"`
	Status       *Status `msgpack:"status,omitempty"`
}
```

## Task 1：建立有界控制协议模型和帧编码

**Files:**

- Create: `internal/nodectl/message.go`
- Create: `internal/nodectl/frame.go`
- Test: `internal/nodectl/message_test.go`
- Test: `internal/nodectl/frame_test.go`

### Step 1：先写失败的协议校验测试

在 `message_test.go` 写表驱动测试，至少覆盖：版本不是 1、空或超过 64 字符的 `request_id`、未知命令、合法 `status`、合法 `shutdown`；在 `Status` 测试中覆盖路径 1024 字符上限、错误摘要 512 字符上限、负 PID/计数、最多 1024 个 Worker、Worker 索引唯一且与汇总一致，以及 Agent/Helper 组件枚举。

测试调用以下尚未实现的接口：

```go
const ProtocolVersion uint16 = 1

func (r Request) Validate() error
func (s Status) Validate() error
func (r Response) Validate() error
func SanitizeSummary(value string) string
```

`SanitizeSummary` 的断言必须证明：去除 CR/LF/NUL、截断到 512 个 Unicode 字符，并把含 userinfo 的连接 URI 整体替换为 `[REDACTED_URI]`，测试不得写入真实 DSN 或凭据。

### Step 2：运行 RED

Run: `go test ./internal/nodectl -run 'Test(Request|Status|Response|Sanitize)' -count=1`

Expected: FAIL，提示 `Request`、`Validate` 或 `SanitizeSummary` 未定义。

### Step 3：实现最小模型与严格校验

在 `message.go` 放入“跨计划冻结接口”的类型定义，并实现：

- 所有字符串先验证 UTF-8；
- `RequestID` 长度 1–64，命令只允许两个枚举；
- `MachineID` 长度 1–128；`ExecutablePath` 长度 1–1024；
- `ConfigSHA256` 只能为空或 64 个小写十六进制字符；
- `Lifecycle` 只允许 `starting`、`running`、`stopping`、`failed`；进程内部已启动后不返回 `stopped`；
- PID、Worker 计数和 ActiveRequests 不得为负；Helper 的 Worker 计数必须为 0；
- Agent 的 `Workers` 长度必须等于 `WorkerExpected`，索引唯一且在范围内；每个任务摘要最多 96 个 UTF-8 字节、错误摘要最多 192 个 UTF-8 字节，且不得包含媒体文件路径，保证 1024 项快照仍低于 1 MiB 帧上限；
- Helper 的 `Workers` 必须为空、同步字段必须为空/false；Agent 的 `SyncErrorSummary` 必须脱敏；
- 失败响应不能携带 `Status`，成功响应不能携带错误码或错误摘要；
- `SanitizeSummary` 先清除控制字符，再用 URI/键值模式做保守脱敏，最后按 rune 截断。

### Step 4：先写失败的帧边界测试

`frame_test.go` 使用 `net.Pipe()` 覆盖：往返编码、包含 1024 个 Worker 的合法状态、声明长度为 0、声明长度超过 1 MiB、截断帧、未知附加字段、响应校验失败。测试接口固定为：

```go
const MaxFrameSize = 1024 * 1024

func WriteFrame(w io.Writer, value any) error
func ReadFrame(r io.Reader, value any) error
```

### Step 5：运行第二个 RED

Run: `go test ./internal/nodectl -run TestFrame -count=1`

Expected: FAIL，提示 `WriteFrame` 或 `ReadFrame` 未定义。

### Step 6：实现四字节大端长度前缀和 MessagePack

`frame.go` 必须：

- 使用 `binary.BigEndian`；
- 编码到内存后再检查上限，禁止先写长度后才发现超限；
- 使用 `io.ReadFull`；
- 拒绝 0 长度与超限长度；
- 解码后调用值类型的 `Validate`，使用小型私有接口 `interface{ Validate() error }`；
- 不修改 `internal/proto`，避免控制面和业务协议共享上限或演进节奏。

### Step 7：运行 GREEN 与包级竞态检查

Run: `go test ./internal/nodectl -count=1`

Expected: PASS。

Run: `go test -race ./internal/nodectl -count=1`

Expected: PASS；若当前 Windows Go 工具链无法运行 race detector，记录原始错误并在有 CGO race 工具链的验收机补跑，不可把未运行写成 PASS。

### Step 8：提交检查点

Run: `git add internal/nodectl && git commit -m "feat: add bounded node control protocol"`

Expected: Git 工作树中提交成功；当前无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 2：实现 Windows 命名管道传输和限制性 ACL

**Files:**

- Create: `internal/nodectl/pipe_windows.go`
- Create: `internal/nodectl/pipe_stub.go`
- Create: `internal/nodectl/pipe_windows_test.go`
- Test: `internal/nodectl/pipe_stub_test.go`

### Step 1：写管道名称和跨平台桩测试

冻结接口：

```go
func AgentPipeName() string
func HelperPipeName() string
func Listen(name string) (net.Listener, error)
func Dial(ctx context.Context, name string) (net.Conn, error)
```

名称必须精确为：

```text
\\.\pipe\mysingerserver-agent-control-v1
\\.\pipe\mysingerserver-helper-control-v1
```

非 Windows 桩的 `Listen` 和 `Dial` 返回包含 `nodectl named pipes require windows` 的错误，以便 Linux 静态构建失败原因明确。

### Step 2：运行 RED

Run: `go test ./internal/nodectl -run 'Test(AgentPipeName|HelperPipeName|PipeStub)' -count=1`

Expected: FAIL，接口尚未实现。

### Step 3：实现构建标签和固定名称

`pipe_windows.go` 使用 `//go:build windows`，`pipe_stub.go` 使用 `//go:build !windows`。不得允许调用方传入任意用户输入生成管道名；`Listen` 虽接受名称，但只允许两个冻结名称。

### Step 4：写 Windows ACL 测试

`pipe_windows_test.go` 创建监听器后读取安全描述符并断言：

- 当前登录用户 SID 可读写；
- `BUILTIN\Administrators` 和 `SYSTEM` 可读写；
- `NETWORK` 明确拒绝或未被授予；
- `Everyone` 未被授予；
- Agent 与 Helper 管道不能同时绑定第二个监听器。

复用 `internal/helper/pipe_windows.go` 已验证过的 SID 获取思路，但不要从 Helper 包反向依赖控制包。

### Step 5：运行 Windows RED

Run: `go test ./internal/nodectl -run TestPipeACL -count=1`

Expected: FAIL，监听或安全描述符尚未满足断言。

### Step 6：用 go-winio 实现限制性监听和上下文拨号

安全描述符只授予当前用户、BA、SY；使用 `winio.ListenPipe` 和 `winio.DialPipeContext`。监听必须拒绝非冻结名称。拨号超时由调用方上下文决定，不写固定无限等待。

### Step 7：运行 GREEN

Run: `go test ./internal/nodectl -run 'TestPipe' -count=1`

Expected: PASS。

Run: `go test ./internal/nodectl -count=1`

Expected: PASS。

### Step 8：提交检查点

Run: `git add internal/nodectl && git commit -m "feat: secure local component control pipes"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 3：实现通用控制服务端与客户端

**Files:**

- Create: `internal/nodectl/server.go`
- Create: `internal/nodectl/client.go`
- Test: `internal/nodectl/server_test.go`
- Test: `internal/nodectl/client_test.go`

### Step 1：写端到端失败测试

冻结接口：

```go
type StatusProvider interface {
	ControlStatus() Status
}

type ShutdownFunc func()

func Serve(ctx context.Context, ln net.Listener, provider StatusProvider, shutdown ShutdownFunc) error

type Client struct {
	// 私有字段
}

func NewClient(dial func(context.Context) (net.Conn, error)) *Client
func (c *Client) Status(ctx context.Context) (Status, error)
func (c *Client) Shutdown(ctx context.Context) error
```

使用内存 listener 或 `net.Pipe` 测试：

- `Status` 返回 provider 快照且 request ID 对应；
- `Shutdown` 先返回成功响应，再且仅调用一次回调；
- 多个并发状态请求互不串帧；
- 未知命令返回 `unsupported_command`；
- 坏帧只关闭该连接，不终止 listener；
- 上下文取消后 `Serve` 返回 `context.Canceled` 或 nil，不泄漏 goroutine；
- 客户端拒绝 request ID 不匹配的响应。

### Step 2：运行 RED

Run: `go test ./internal/nodectl -run 'Test(Server|Client)' -count=1`

Expected: FAIL，服务端/客户端接口未定义。

### Step 3：实现一次请求一次连接的客户端

客户端每次调用生成 128-bit 随机 request ID，建立连接、设置由 context 推导的 deadline、写一帧、读一帧并关闭。不得缓存长连接，避免组件重启后保留失效句柄。

### Step 4：实现有界并发服务端

`Serve` 为每个连接启动处理 goroutine，但用容量 16 的信号量限制并发；连接只处理一条请求。`shutdown` 使用 `sync.Once`，成功响应写完后调用。监听 accept 错误只有在 context 取消或 listener 关闭时可视为正常结束。

所有返回给客户端的错误使用稳定码：`invalid_request`、`unsupported_command`、`status_unavailable`、`internal_error`，摘要必须经过 `SanitizeSummary`。

### Step 5：运行 GREEN 和泄漏测试

Run: `go test ./internal/nodectl -run 'Test(Server|Client)' -count=1`

Expected: PASS。

Run: `go test ./internal/nodectl -count=50`

Expected: PASS，重复运行不出现偶发死锁。

### Step 6：提交检查点

Run: `git add internal/nodectl && git commit -m "feat: serve local status and shutdown commands"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 4：把 Agent 状态和受控关闭接入控制面

**Files:**

- Create: `internal/agentcontrol/provider.go`
- Create: `internal/agentcontrol/service.go`
- Create: `internal/agentcontrol/singleinstance_windows.go`
- Create: `internal/agentcontrol/singleinstance_stub.go`
- Test: `internal/agentcontrol/provider_test.go`
- Test: `internal/agentcontrol/service_test.go`
- Modify: `internal/worker/pool.go`
- Modify: `cmd/agent/main.go`
- Test: `cmd/agent/main_test.go`

### Step 1：为 Worker Pool 写只读状态测试

为 `internal/worker` 增加不会暴露可变内部结构的快照：

```go
type RuntimeSnapshot struct {
	Expected         int
	Ready            int
	LastErrorSummary string
	Workers          []RuntimeWorkerStatus
}

type RuntimeWorkerStatus struct {
	Index              int
	PID                int
	Ready              bool
	CurrentTaskSummary string
	LastErrorSummary   string
}

func (p *Pool) RuntimeSnapshot() RuntimeSnapshot
```

测试覆盖 worker ready、启动失败、重生中的计数一致性和并发读取。每个 Worker 快照在持有该 worker 的互斥锁时复制 Index/PID/Ready/当前任务类别和最近错误；当前任务摘要只包含阶段和内部任务 ID，不含输入媒体路径。Worker 包不依赖控制协议包；`agentcontrol.Provider` 在映射为 `nodectl.WorkerStatus` 时统一调用 `SanitizeSummary`，测试证明外部状态不包含 worker 环境、媒体路径或数据库连接信息。

### Step 2：运行 RED 并实现快照

Run: `go test ./internal/worker -run TestRuntimeSnapshot -count=1`

Expected: FAIL 后实现最小线程安全快照，再运行同一命令得到 PASS。

### Step 3：写 Agent Provider 失败测试

冻结构造接口：

```go
type Inputs struct {
	MachineID       string
	ExecutablePath  string
	ConfigSHA256    string
	StartedAt       time.Time
	ListenerReady   func() bool
	Workers         interface{ RuntimeSnapshot() worker.RuntimeSnapshot }
	SyncHealth      func() SyncHealth
}

type SyncHealth struct {
	Healthy      bool
	ErrorSummary string
}

func NewProvider(inputs Inputs) *Provider
func (p *Provider) ControlStatus() nodectl.Status
```

断言 `ServiceReady` 直接反映业务 listener，`Ready` 仅在 listener 已就绪且 `WorkerReady == WorkerExpected` 时为真；Worker 未齐时生命周期可保持 `starting`，但进程及每个 Worker 的有界只读快照仍可读。同步失败只令 `SyncHealthy=false` 并产生脱敏摘要，不把已运行 Agent 误判为 stopped。配置指纹使用加载成功后的配置规范化 JSON 做 SHA-256，不读取 UI 草稿。

### Step 4：运行 RED 并实现 Provider

Run: `go test ./internal/agentcontrol -run TestProvider -count=1`

Expected: FAIL 后实现，再运行得到 PASS。

### Step 5：写 Agent 控制服务集成失败测试

`service.go` 冻结接口：

```go
type Service struct {
	// 私有字段
}

func New(provider nodectl.StatusProvider, shutdown nodectl.ShutdownFunc) *Service
func (s *Service) Run(ctx context.Context) error
```

测试使用可注入 listener 工厂证明 `shutdown` 取消 Agent 根上下文，而且 drain 回调和 `pool.Close()` 各执行一次。再写 Windows 单实例测试，第二次获取同 machine ID 的互斥体必须返回类型化 `ErrAlreadyRunning`。

### Step 6：运行 RED

Run: `go test ./internal/agentcontrol ./cmd/agent -run 'Test(Control|SingleInstance|Shutdown)' -count=1`

Expected: FAIL，服务未接入。

### Step 7：重构 `cmd/agent/main.go` 为单一根上下文

启动顺序固定为：加载并校验配置 → 取得单实例锁 → 启动 Worker Pool → 启动业务 listener → 启动控制 listener。控制 `shutdown` 只调用根 `cancel()`；现有 `defer pool.Close()`、业务 listener 关闭与 drain 顺序保持原语义。

Agent 启动失败时释放单实例锁。控制服务失败应触发根取消并使 Agent 以非零退出，而不是留下业务 listener 孤儿。

### Step 8：运行 GREEN

Run: `go test ./internal/agentcontrol ./cmd/agent ./internal/worker -count=1`

Expected: PASS。

Run: `go test ./internal/agentcontrol ./internal/worker -count=20`

Expected: PASS。

### Step 9：提交检查点

Run: `git add internal/agentcontrol internal/worker cmd/agent && git commit -m "feat: expose agent local control status"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 5：把删除 Helper 的独立状态和受控关闭接入控制面

**Files:**

- Create: `internal/helpercontrol/provider.go`
- Create: `internal/helpercontrol/service.go`
- Test: `internal/helpercontrol/provider_test.go`
- Test: `internal/helpercontrol/service_test.go`
- Modify: `internal/helper/server.go`
- Test: `internal/helper/server_test.go`
- Modify: `cmd/helper/main.go`
- Test: `cmd/helper/main_test.go`

### Step 1：为删除服务写活动请求快照测试

在 `internal/helper/server.go` 暴露：

```go
func (s *Server) ActiveRequests() int
func (s *Server) Listening() bool
```

测试必须覆盖连接建立、事务执行、完成、取消和服务关闭过程，计数永不为负。不得用“当前只有一个连接”作为不加同步的理由。

### Step 2：运行 RED 并实现原子快照

Run: `go test ./internal/helper -run 'Test(ActiveRequests|Listening)' -count=1`

Expected: FAIL 后实现，再运行得到 PASS。

### Step 3：写 Helper Provider 和独立管道失败测试

冻结接口：

```go
type Inputs struct {
	MachineID      string
	ExecutablePath string
	ConfigSHA256   string
	StartedAt      time.Time
	DeleteService  interface {
		ActiveRequests() int
		Listening() bool
	}
}

func NewProvider(inputs Inputs) *Provider
func (p *Provider) ControlStatus() nodectl.Status
```

断言 Helper `Ready` 只在删除服务管道正在监听时为真，Worker 计数恒为 0，`ActiveRequests` 与删除服务一致。控制 listener 必须绑定 `HelperPipeName()`，现有删除 listener 名称保持不变。

### Step 4：运行 RED

Run: `go test ./internal/helpercontrol -count=1`

Expected: FAIL，Provider/Service 尚未实现。

### Step 5：实现 Helper 控制服务并共享根取消

`internal/helpercontrol/service.go` 使用与 Agent 相同的 `nodectl.Serve`。`cmd/helper/main.go` 用同一个根 context 驱动删除服务和控制服务；`shutdown` 停止接受新删除请求，并沿用既有等待当前事务完成/超时的退出路径。

保留现有删除协议对既有 `proto.MsgShutdown` 的兼容行为，但托盘程序和新测试只通过独立控制管道发起生命周期关闭。记录兼容注释，防止未来误把两条管道合并。

### Step 6：运行 GREEN 和删除协议回归

Run: `go test ./internal/helpercontrol ./internal/helper ./cmd/helper -count=1`

Expected: PASS。

Run: `go test ./internal/helper -run 'Test.*Delete' -count=1`

Expected: PASS，既有删除事务协议未回归。

### Step 7：提交检查点

Run: `git add internal/helpercontrol internal/helper cmd/helper && git commit -m "feat: add separate helper control service"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 6：完成控制面集成、兼容性和安全验收

**Files:**

- Create: `tests/windows/Test-NodeControlPlane.ps1`
- Create: `docs/deployment/node-control-plane.md`
- Modify: `README.md`
- Modify: `scripts/build.ps1`

### Step 1：先写失败的 Windows 集成脚本断言

脚本参数必须显式接收临时 Agent 配置、临时 Helper 配置和已构建二进制路径，不得默认使用真实媒体目录。测试根目录由 `New-Item` 创建在 `C:\tmp\mysingerserver-node-control-<guid>`；脚本退出时只清理该已解析临时根。

脚本依次验证：

1. Agent 控制 `status` 返回当前 PID、路径、配置指纹和 Worker 汇总；
2. Agent 第二实例被单实例锁拒绝；
3. Agent `shutdown` 触发受控退出且 Worker 子进程树清理；
4. Agent TCP 端口不接受本机控制命令；
5. Helper 删除管道与控制管道同时存在且名称不同；
6. Helper 有活动删除事务时 `status.active_requests == 1`；
7. Helper `shutdown` 不截断已接受事务，退出后不再接受新事务；
8. 日志和 JSON 证据不含 DSN、密码和测试秘密标记。

### Step 2：运行脚本的静态 RED

Run: `pwsh -NoProfile -File tests/windows/Test-NodeControlPlane.ps1 -WhatIf`

Expected: 在实现完整参数和 `-WhatIf` 静态检查前 FAIL；完成脚本骨架后 PASS，且不启动进程。

### Step 3：把控制面包纳入构建和文档

`scripts/build.ps1` 无需增加新二进制，但必须让 Agent/Helper 的普通构建覆盖新包；文档说明固定管道、ACL、仅本机边界、协议版本和托盘以外客户端不得依赖未冻结字段。

README 只增加节点控制面的开发说明和后续托盘计划链接，不宣称托盘已交付。

### Step 4：运行静态和单元总门禁

Run: `go fmt ./internal/nodectl ./internal/agentcontrol ./internal/helpercontrol ./internal/helper ./internal/worker ./cmd/agent ./cmd/helper`

Expected: 命令成功，随后工作树只出现预期格式化差异。

Run: `go vet ./internal/nodectl ./internal/agentcontrol ./internal/helpercontrol ./internal/helper ./internal/worker ./cmd/agent ./cmd/helper`

Expected: PASS。

Run: `go test ./internal/nodectl ./internal/agentcontrol ./internal/helpercontrol ./internal/helper ./internal/worker ./cmd/agent ./cmd/helper -count=1`

Expected: PASS。

Run: `pwsh -NoProfile -File tests/windows/Test-NodeControlPlane.ps1 -WhatIf`

Expected: PASS，未启动或停止任何真实进程。

### Step 5：在明确授权的测试机运行动态验收

Run: `pwsh -NoProfile -File tests/windows/Test-NodeControlPlane.ps1 -AgentExe artifacts/stage/agent.exe -WorkerExe artifacts/stage/worker.exe -HelperExe artifacts/stage/helper.exe -VideoCoreDll artifacts/stage/videocore.dll -TestRoot C:\tmp\mysingerserver-node-control-acceptance`

Expected: PASS，并在测试根生成脱敏 JSON 结果。若未获得运行进程、UAC 或 PostgreSQL 测试权限，状态必须记录为 `BLOCKED_NOT_RUN_DYNAMIC`，不可降级为 PASS。

### Step 6：凭据和远程控制回归扫描

Run: `rg -n "postgres(ql)?://|password\s*[:=]|MsgShutdown|CommandShutdown|mysingerserver-.*-control" internal cmd tests/windows docs/deployment`

Expected: 每个命中均人工归类；文档和测试不得出现真实凭据，`CommandShutdown` 只出现在本机控制包/适配层，Agent TCP 分派器不存在生命周期分支。

### Step 7：最终提交检查点

Run: `git add internal cmd tests/windows docs/deployment README.md scripts/build.ps1 && git commit -m "test: verify local node component control plane"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## 完成定义

- `internal/nodectl` 的模型、边界、ACL、客户端和服务端测试全部通过。
- Agent 控制状态能准确汇总 listener 与 Worker Ready 数，受控关闭复用既有 drain。
- Helper 控制状态与删除事务管道完全分离，受控关闭不破坏已接受事务。
- Agent TCP 没有生命周期控制入口，Worker 仍不可被托盘直接操作。
- 动态验收有明确 PASS 证据，或如实记录 `BLOCKED_NOT_RUN_DYNAMIC` 及缺失权限。
- 所有外显错误和证据完成凭据扫描与脱敏检查。
