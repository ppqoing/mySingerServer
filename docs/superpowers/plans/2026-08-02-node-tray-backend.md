# 节点托盘后端实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development for every task and superpowers:verification-before-completion before reporting completion.

**Goal:** 实现可被 Wails 前端调用的节点托盘后端，包括交互式表单模型、严格配置校验与原子保存、组件认领和五态监督、登录启动、Helper 一次性 UAC 动作及最高权限计划任务。

**Architecture:** 将与 UI 无关的逻辑放入 `internal/nodetray`，以纯 Go 接口隔离配置存储、进程、控制管道、注册表、计划任务和提权边界。普通托盘进程永不持有管理员权限；受保护写入和 Helper 计划任务操作由同一可执行文件的受限 one-shot elevated 模式完成。Agent/Helper 状态只通过前一计划冻结的 `internal/nodectl.Client` 认领。

**Tech Stack:** Go 1.22、Windows API、`golang.org/x/sys/windows` 0.28.0、Task Scheduler 2.0 COM、Windows Registry、`internal/nodectl`、Go `testing`。

**前置计划:** [节点组件本机控制面实施计划](2026-08-02-node-control-plane.md)

**前置设计:** [媒体节点托盘管理程序设计](../specs/2026-08-02-node-tray-design.md)

**后续计划:** [节点托盘 UI、构建与验收实施计划](2026-08-02-node-tray-ui-release.md)

---

## 全局约束

- 本计划不创建原始 JSON 编辑器；所有写入必须来自有类型表单 DTO。
- 不能用进程名认领 Agent/Helper；必须校验 PID、创建时间、规范化可执行路径、控制握手和配置指纹。
- 生命周期统一为 `stopped`、`starting`、`running`、`stopping`、`failed`；`degraded` 只作为健康标记。
- 自动启动只发生在当前部署账号登录后；Agent 不变为服务，Helper 自动模式使用固定计划任务。
- Helper 手动启动和受保护变更必须经 UAC；UAC 取消不改变旧配置和旧任务。
- 自动模式只负责登录后的首次启动，不无限自动重启。异常退出后进入 `failed` 并等待人工动作。
- 托盘退出默认不停止组件；本计划的 Supervisor 不把组件放入 kill-on-close Job Object。
- 所有错误 DTO 仅含稳定错误码和脱敏摘要；不得把 DSN、密码或完整环境放入事件/日志。
- 当前目录没有 `.git` 元数据。每个提交步骤仅在 `git rev-parse --is-inside-work-tree` 成功时执行，否则记录 `N/A_NO_GIT_METADATA`。

## 冻结后端模型

以下模型由第三份计划的 Wails 绑定直接生成 TypeScript 类型，不得用 `map[string]any` 替代：

```go
package traymodel

type Lifecycle string

const (
	Stopped  Lifecycle = "stopped"
	Starting Lifecycle = "starting"
	Running  Lifecycle = "running"
	Stopping Lifecycle = "stopping"
	Failed   Lifecycle = "failed"
)

type StartMode string

const (
	StartManual    StartMode = "manual"
	StartAutomatic StartMode = "automatic"
)

type NotificationLevel string

const (
	NotifyImportant NotificationLevel = "important"
	NotifyAll       NotificationLevel = "all"
)

type LocationKind string

const (
	AgentLogs    LocationKind = "agent-logs"
	HelperLogs   LocationKind = "helper-logs"
	AgentBackup  LocationKind = "agent-backup"
	HelperBackup LocationKind = "helper-backup"
)

type ComponentState struct {
	Lifecycle       Lifecycle `json:"lifecycle"`
	Healthy         bool      `json:"healthy"`
	Ready           bool      `json:"ready"`
	PID             int       `json:"pid"`
	StartedAtUnixMS int64     `json:"startedAtUnixMs"`
	UptimeSeconds   int64     `json:"uptimeSeconds"`
	WorkerReady     int       `json:"workerReady"`
	WorkerExpected  int       `json:"workerExpected"`
	ActiveRequests  int       `json:"activeRequests"`
	ErrorCode       string    `json:"errorCode"`
	ErrorSummary    string    `json:"errorSummary"`
	NeedsAttention  bool      `json:"needsAttention"`
}

type WorkerState struct {
	Index              int    `json:"index"`
	PID                int    `json:"pid"`
	Ready              bool   `json:"ready"`
	CurrentTaskSummary string `json:"currentTaskSummary"`
	LastErrorSummary   string `json:"lastErrorSummary"`
}

type Overview struct {
	MachineID       string         `json:"machineId"`
	Agent           ComponentState `json:"agent"`
	Workers         []WorkerState  `json:"workers"`
	Helper          ComponentState `json:"helper"`
	AgentStartMode  StartMode      `json:"agentStartMode"`
	HelperStartMode StartMode      `json:"helperStartMode"`
	HelperEnabled   bool           `json:"helperEnabled"`
	HelperTaskDrift bool           `json:"helperTaskDrift"`
	LoginStartDrift bool           `json:"loginStartDrift"`
}

type TraySettings struct {
	LoginStartTray         bool              `json:"loginStartTray"`
	AgentStartMode         StartMode         `json:"agentStartMode"`
	HelperEnabled          bool              `json:"helperEnabled"`
	HelperStartMode        StartMode         `json:"helperStartMode"`
	CloseToTray            bool              `json:"closeToTray"`
	RefreshIntervalSeconds int               `json:"refreshIntervalSeconds"`
	NotificationLevel      NotificationLevel `json:"notificationLevel"`
}

type OperationResult struct {
	OK           bool   `json:"ok"`
	ErrorCode    string `json:"errorCode"`
	ErrorSummary string `json:"errorSummary"`
	UACCancelled bool   `json:"uacCancelled"`
}
```

格式化时允许把 `TraySettings` 字段对齐，但 JSON 名称不得改变。

## Task 1：建立托盘设置、表单 DTO 与双向映射

**Files:**

- Create: `internal/nodetray/traymodel/model.go`
- Create: `internal/nodetray/traymodel/model_test.go`
- Create: `internal/nodetray/config/forms.go`
- Create: `internal/nodetray/config/forms_test.go`
- Modify: `internal/config/agent.go`
- Test: `internal/config/config_test.go`
- Modify: `internal/helper/config.go`
- Test: `internal/helper/config_test.go`

### Step 1：写模型枚举失败测试

测试 `StartMode.Validate()`、`NotificationLevel.Validate()`、`TraySettings.Validate()` 和生命周期枚举，至少拒绝未知启动模式、未知通知级别、Helper 禁用但自动启动、1–3 秒以外的可见窗口刷新间隔。`CloseToTray=false` 表示点击关闭按钮时先显示退出对话框，不能直接静默退出。接口：

```go
func (m StartMode) Validate() error
func (n NotificationLevel) Validate() error
func (s TraySettings) Validate() error
```

### Step 2：运行 RED 并实现最小模型

Run: `go test ./internal/nodetray/traymodel -count=1`

Expected: FAIL 后实现，再运行得到 PASS。

### Step 3：写 Agent/Helper 表单映射失败测试

表单 DTO 必须把秘密拆成独立字段而不是单个 DSN 文本框：

```go
type AgentForm struct {
	MachineID     string            `json:"machineId"`
	ListenHost    string            `json:"listenHost"`
	ListenPort    int               `json:"listenPort"`
	DataDir       string            `json:"dataDir"`
	Database      DatabaseForm      `json:"database"`
	UseEverything bool              `json:"useEverything"`
	Scan          ScanForm          `json:"scan"`
	Sync          SyncForm          `json:"sync"`
	Proto         ProtoForm         `json:"proto"`
	Worker        WorkerForm        `json:"worker"`
	Pipeline      PipelineForm      `json:"pipeline"`
	Thumb         ThumbForm         `json:"thumb"`
	IPC           IPCForm           `json:"ipc"`
	Delete        AgentDeleteForm   `json:"delete"`
	Tuning        TuningForm        `json:"tuning"`
}

type DatabaseForm struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Database       string `json:"database"`
	User           string `json:"user"`
	Password       string `json:"password"`
	PasswordStored bool   `json:"passwordStored"`
	ReplacePassword bool  `json:"replacePassword"`
	SSLMode        string `json:"sslMode"`
}

type ScanForm struct {
	HDDReadBlockMB     int      `json:"hddReadBlockMb"`
	HDDStreams         int      `json:"hddStreamsPerDisk"`
	SSDStreams         int      `json:"ssdStreamsPerDisk"`
	ImageMemResidentMB int      `json:"imageMemResidentMb"`
	ImageTimeoutS      int      `json:"imageTimeoutS"`
	VideoTimeoutS      int      `json:"videoTimeoutS"`
	ImageExts          []string `json:"imageExts"`
	VideoExts          []string `json:"videoExts"`
}

type SyncForm struct {
	IntervalS   int `json:"intervalS"`
	TriggerRows int `json:"triggerRows"`
	UpsertBatch int `json:"upsertBatch"`
}

type ProtoForm struct { HeartbeatS int `json:"heartbeatS"` }

type WorkerForm struct {
	Count          int    `json:"count"`
	ExePath        string `json:"exePath"`
	ImageTimeoutS  int    `json:"imageTimeoutS"`
	VideoTimeoutS  int    `json:"videoTimeoutS"`
	ImageMemoryMB  int    `json:"imageMemoryMb"`
	RespawnDelayMS int    `json:"respawnDelayMs"`
}

type PipelineForm struct { ReadChunkKB int `json:"readChunkKb"` }

type ThumbForm struct {
	CacheDir       string `json:"cacheDir"`
	TileMaxSide    int    `json:"tileMaxSide"`
	ProbeTimeoutS  int    `json:"probeTimeoutS"`
	NativeTimeoutS int    `json:"nativeTimeoutS"`
	FrameTimeoutS  int    `json:"frameTimeoutS"`
}

type IPCForm struct { MaxFrameMB int `json:"maxFrameMb"` }

type AgentDeleteForm struct {
	PipeName           string `json:"pipeName"`
	MaxEntriesPerFrame int    `json:"maxEntriesPerFrame"`
	DialTimeoutMS      int    `json:"dialTimeoutMs"`
	HelloTimeoutS     int    `json:"helloTimeoutS"`
	ReportTimeoutS    int    `json:"reportTimeoutS"`
}

type TuningForm struct {
	StatsEnabled   bool   `json:"statsEnabled"`
	StatsIntervalS int    `json:"statsIntervalS"`
	StatsHistoryS  int    `json:"statsHistoryS"`
	PendingBytesMB int    `json:"pendingBytesMb"`
	StatsLogMB     int    `json:"statsLogMb"`
	PprofAddr      string `json:"pprofAddr"`
}

type HelperForm struct {
	PipeName             string   `json:"pipeName"`
	AllowedRoots         []string `json:"allowedRoots"`
	DeniedRoots          []string `json:"deniedRoots"`
	DefaultMode          string   `json:"defaultMode"`
	AllowHardDelete      bool     `json:"allowHardDelete"`
	RecycleDirName       string   `json:"recycleDirName"`
	MaxEntriesPerFrame   int      `json:"maxEntriesPerFrame"`
	FrameReadTimeoutSec  int      `json:"frameReadTimeoutSec"`
	FrameWriteTimeoutSec int      `json:"frameWriteTimeoutSec"`
	LogDir               string   `json:"logDir"`
}

func AgentToForm(cfg *config.AgentConfig) (AgentForm, error)
func AgentFromForm(form AgentForm, base *config.AgentConfig) (*config.AgentConfig, error)
func HelperToForm(cfg helper.Config) HelperForm
func HelperFromForm(form HelperForm) (helper.Config, error)
```

映射测试必须证明除测试专用 `worker.crash_injection` 外，现有 Agent 配置的全部支持字段都能由结构化表单往返；高级字段可以折叠，但不能只能靠改 JSON 修改。`worker.crash_injection` 不进入生产界面，转换时从 `base` 原样保留。`AgentToForm` 永远把 `Password` 置空，只用 `PasswordStored=true` 告知界面已有秘密；`ReplacePassword=false` 时 `AgentFromForm` 从受保护的 `base` 保留旧密码，只有显式勾选替换时才读取 `Password`，此时空值表示明确清除。测试必须证明 GetAgentForm/事件/错误不会回传现有密码。

### Step 4：抽取纯校验入口

为现有配置包新增：

```go
func ValidateAgent(cfg *AgentConfig, executable string, cpuCount int) (*AgentConfig, error)
func ValidateConfig(cfg Config, executable string) (Config, error)
```

分别位于 `internal/config` 和 `internal/helper`。现有 `LoadAgent`/`LoadConfig` 先严格解码，再调用纯校验函数，确保 CLI 和托盘共享同一规则。禁止复制一份略有差异的 UI 校验。

### Step 5：运行 RED

Run: `go test ./internal/nodetray/config ./internal/config ./internal/helper -run 'Test.*(Form|Validate)' -count=1`

Expected: FAIL，纯校验或表单映射尚未实现。

### Step 6：实现映射、DSN 构造和脱敏错误

使用 `net/url` 正确编码用户名/密码，输出 PostgreSQL DSN 时对 query 参数排序，避免配置指纹抖动。校验错误以字段路径返回：

```go
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
```

`Message` 不包含密码和完整 DSN。Helper `allowed_roots` 继续调用现有系统目录拒绝和规范化逻辑。

### Step 7：运行 GREEN

Run: `go test ./internal/nodetray/config ./internal/config ./internal/helper -count=1`

Expected: PASS。

### Step 8：提交检查点

Run: `git add internal/nodetray/traymodel internal/nodetray/config internal/config internal/helper && git commit -m "feat: add interactive node configuration models"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 2：实现严格 JSON、限制性 ACL、原子保存和单份可用备份

**Files:**

- Create: `internal/nodetray/config/store.go`
- Create: `internal/nodetray/config/store_windows.go`
- Create: `internal/nodetray/config/store_stub.go`
- Test: `internal/nodetray/config/store_test.go`
- Test: `internal/nodetray/config/store_windows_test.go`

### Step 1：写失败的存储合同测试

冻结接口：

```go
type Paths struct {
	TraySettings string
	AgentConfig  string
	HelperConfig string
}

type Store struct {
	// 私有字段
}

func NewStore(paths Paths) (*Store, error)
func (s *Store) LoadTraySettings() (traymodel.TraySettings, error)
func (s *Store) SaveTraySettings(value traymodel.TraySettings) error
func (s *Store) LoadAgentForm() (AgentForm, error)
func (s *Store) SaveAgentForm(value AgentForm) (configSHA256 string, err error)
func (s *Store) LoadHelperForm() (HelperForm, error)
func (s *Store) PrepareHelperWrite(value HelperForm) (PreparedWrite, error)
func (s *Store) RestoreAgentBackup() error
func (s *Store) RestoreHelperBackup() (PreparedWrite, error)

type PreparedWrite struct {
	TargetPath    string
	CanonicalJSON []byte
	SHA256        string
}
```

测试覆盖：严格拒绝未知 JSON 字段和尾随值、写入临时文件后 fsync、同目录原子 replace、保存前保留一份 `.last-good`、复读/复验失败时正式文件不变、备份恢复也先验证、并发保存被文件锁串行化。

### Step 2：运行 RED

Run: `go test ./internal/nodetray/config -run 'TestStore' -count=1`

Expected: FAIL，Store 尚未实现。

### Step 3：实现普通用户可写存储

托盘设置写当前用户配置目录。Agent 配置目标默认由部署路径注入，不硬编码真实机器目录；创建或修复 ACL 时仅授予当前部署用户、Administrators、SYSTEM。Helper 配置的普通进程只生成 `PreparedWrite`，不得直接落盘到受保护目标。

规范 JSON 使用两空格缩进和结尾换行；SHA-256 对实际将写入的字节计算。日志仅记录目标 basename、SHA-256 和结果，不记录 JSON 内容。

### Step 4：写 Windows ACL 失败测试并实现

测试读取 DACL，确认 Agent 配置不向 Everyone/NETWORK 开放，Helper 正式配置不向普通用户授予写权限。临时测试目录显式创建并只清理自己的 GUID 子目录。

Run: `go test ./internal/nodetray/config -run 'Test.*ACL' -count=1`

Expected: RED 后实现，再运行 PASS。

### Step 5：运行 GREEN 和故障注入

Run: `go test ./internal/nodetray/config -count=1`

Expected: PASS。

Run: `go test ./internal/nodetray/config -run 'TestStore.*Failure' -count=20`

Expected: PASS，故障注入后正式文件或 last-good 至少有一个可验证版本。

### Step 6：提交检查点

Run: `git add internal/nodetray/config && git commit -m "feat: save node configuration atomically"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 3：实现可信进程身份、重新认领和监督状态机

**Files:**

- Create: `internal/nodetray/process/identity.go`
- Create: `internal/nodetray/process/identity_windows.go`
- Create: `internal/nodetray/process/identity_stub.go`
- Create: `internal/nodetray/process/helper_windows.go`
- Create: `internal/nodetray/process/helper_stub.go`
- Test: `internal/nodetray/process/identity_test.go`
- Test: `internal/nodetray/process/helper_windows_test.go`
- Create: `internal/nodetray/supervisor/supervisor.go`
- Create: `internal/nodetray/supervisor/component.go`
- Test: `internal/nodetray/supervisor/supervisor_test.go`
- Test: `internal/nodetray/supervisor/component_test.go`

### Step 1：写进程身份失败测试

冻结接口：

```go
type Identity struct {
	PID             int
	StartedAtUnixMS int64
	ExecutablePath  string
}

type Inspector interface {
	Inspect(pid int) (Identity, error)
	Wait(ctx context.Context, identity Identity) (exitCode int, err error)
}

func SameProcess(expected, actual Identity) bool
```

`SameProcess` 要求 PID、创建时间和经 Windows 最终路径解析后的可执行文件全部一致，路径比较使用 Windows ordinal ignore-case。PID 复用、符号链接/短路径混淆、拒绝访问均不得判为同一实例。

`helper_windows_test.go` 通过可注入 ShellExecute backend 断言：手动 Helper 启动只使用 `runas`、固定规范化 `helper.exe`、固定 `--config <绝对路径>`，不传环境、密码或媒体目录清单；设置 `SEE_MASK_NOCLOSEPROCESS` 取得进程句柄并构造 Identity；Windows `ERROR_CANCELLED` 映射为类型化 `ErrUACCancelled`，且不改变 Supervisor 旧状态。

### Step 2：运行 RED 并实现 Windows Inspector

Run: `go test ./internal/nodetray/process -count=1`

Expected: FAIL 后实现，再运行 PASS。

实现使用最小进程权限打开句柄，查询创建时间和最终映像路径；Wait 优先等待句柄事件，禁止每 100 ms 轮询全进程表。Helper 手动模式通过 `ShellExecuteExW` 的 `runas` 启动；Agent 使用普通 Launcher，Helper 自动模式只请求运行固定计划任务。

### Step 3：写监督状态机失败测试

冻结依赖和方法：

```go
type Launcher interface {
	Start(ctx context.Context, executable string, args []string, env []string) (process.Identity, error)
}

type ElevatedHelperLauncher interface {
	StartHelper(ctx context.Context, helperExecutable string, helperConfig string) (process.Identity, error)
}

type Terminator interface {
	Terminate(identity process.Identity, exitCode uint32) error
}

type Controller interface {
	Status(ctx context.Context) (nodectl.Status, error)
	Shutdown(ctx context.Context) error
}

type Spec struct {
	Component      nodectl.Component
	ExecutablePath string
	ConfigPath     string
	ExpectedSHA256 string
	ReadyTimeout   time.Duration
	StopTimeout    time.Duration
}

func New(spec Spec, launcher Launcher, inspector process.Inspector, controller Controller, terminator Terminator) *Supervisor
func (s *Supervisor) Start(ctx context.Context) traymodel.OperationResult
func (s *Supervisor) Stop(ctx context.Context) traymodel.OperationResult
func (s *Supervisor) Restart(ctx context.Context) traymodel.OperationResult
func (s *Supervisor) ForceStopClaimed(ctx context.Context) traymodel.OperationResult
func (s *Supervisor) Refresh(ctx context.Context) traymodel.ComponentState
func (s *Supervisor) Adopt(ctx context.Context, candidate process.Identity) traymodel.ComponentState
func (s *Supervisor) Subscribe(buffer int) (<-chan traymodel.ComponentState, func())
```

测试覆盖完整转换矩阵：

- stopped → starting → running；
- starting 超时且进程仍在 → failed/needs attention，不自动结束；
- running → stopping → stopped；
- stopping 超时 → failed，不自动强杀；
- `ForceStopClaimed` 只有在此前握手认领的 PID、创建时间和最终路径仍完全一致时才调用 Terminator；
- PID 已复用、路径漂移或未认领实例的强制停止返回 `identity_mismatch`，绝不按名称结束；
- failed → 手动 Start → starting；
- `Restart` 严格先完成 Stop 才 Start；
- 同时点击 Start 只创建一次进程；
- 异常退出只通知一次，不自动重启；
- Agent 只有 `WorkerReady == WorkerExpected` 才 Ready；
- Helper 只有 Ready 且配置指纹相符才 Running；
- 控制握手路径/PID/创建时间/配置指纹任一不符都显示 `unclaimed_instance`，不发送 shutdown。

### Step 4：运行 RED

Run: `go test ./internal/nodetray/supervisor -count=1`

Expected: FAIL，Supervisor 尚未实现。

### Step 5：实现串行命令和事件驱动等待

每个 Supervisor 使用一个命令 goroutine 串行修改状态；外部方法通过 request/reply channel 进入。进程退出用 `Inspector.Wait`；状态握手在 starting 阶段采用带指数上限的 100/200/400/800/1000 ms 探测，超过 ReadyTimeout 后停止探测，不形成永久轮询。`ForceStopClaimed` 是停止超时后的独立显式动作，不被 `Stop`、`Restart` 或 `ExitTray` 隐式调用。

错误码稳定为 `invalid_config`、`already_running`、`unclaimed_instance`、`start_failed`、`ready_timeout`、`shutdown_failed`、`stop_timeout`、`unexpected_exit`。摘要必须经过 `nodectl.SanitizeSummary`。

### Step 6：运行 GREEN 和竞态检查

Run: `go test ./internal/nodetray/supervisor -count=1`

Expected: PASS。

Run: `go test -race ./internal/nodetray/supervisor -count=20`

Expected: PASS；无法启用 race 时按原始错误记录为未运行。

### Step 7：提交检查点

Run: `git add internal/nodetray/process internal/nodetray/supervisor && git commit -m "feat: supervise and adopt local node components"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 4：实现托盘和 Agent 当前用户单实例、登录启动

**Files:**

- Create: `internal/nodetray/windows/singleinstance/mutex_windows.go`
- Create: `internal/nodetray/windows/singleinstance/activate_windows.go`
- Create: `internal/nodetray/windows/singleinstance/singleinstance_stub.go`
- Test: `internal/nodetray/windows/singleinstance/singleinstance_windows_test.go`
- Create: `internal/nodetray/windows/loginstart/loginstart_windows.go`
- Create: `internal/nodetray/windows/loginstart/loginstart_stub.go`
- Test: `internal/nodetray/windows/loginstart/loginstart_windows_test.go`

### Step 1：写单实例和激活失败测试

接口：

```go
type Lease interface { Close() error }

func AcquireTray(userSID string) (Lease, error)
func AcquireAgent(machineID string) (Lease, error)
func ListenActivation(ctx context.Context, show func()) error
func SignalExisting(ctx context.Context) error
```

托盘互斥体按当前用户 SID 隔离；Agent 互斥体按规范化 machine ID 隔离。第二托盘实例必须通过激活管道通知已有实例显示窗口后退出，不得创建 Supervisor。

### Step 2：运行 RED 并实现

Run: `go test ./internal/nodetray/windows/singleinstance -count=1`

Expected: FAIL 后实现，再运行 PASS。

### Step 3：写 HKCU 登录启动失败测试

接口：

```go
type Service interface {
	Enabled() (bool, string, error)
	Enable(executable string) error
	Disable() error
}
```

使用 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 的固定值名 `MySingerServerNodeTray`。命令行只允许已规范化的当前 `nodetray.exe` 绝对路径和 `--background`；路径漂移返回当前注册值供 UI 提示，不扫描磁盘。

### Step 4：运行 RED 并实现

Run: `go test ./internal/nodetray/windows/loginstart -count=1`

Expected: FAIL 后实现，再运行 PASS。测试必须在独立临时注册表测试键或可注入 registry backend 上运行，不能修改真实 Run 键。

### Step 5：提交检查点

Run: `git add internal/nodetray/windows/singleinstance internal/nodetray/windows/loginstart && git commit -m "feat: add tray single instance and login start"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 5：实现固定 Helper 计划任务

**Files:**

- Create: `internal/nodetray/windows/task/model.go`
- Create: `internal/nodetray/windows/task/task_windows.go`
- Create: `internal/nodetray/windows/task/task_stub.go`
- Test: `internal/nodetray/windows/task/model_test.go`
- Test: `internal/nodetray/windows/task/task_windows_test.go`

### Step 1：写固定任务定义测试

冻结常量和接口：

```go
const TaskPath = `\MySingerServer\DeleteHelper`

type Definition struct {
	HelperExecutable string
	HelperConfig     string
	UserSID          string
}

type Status struct {
	Installed bool
	Running   bool
	LastResult uint32
}

type Service interface {
	Inspect(ctx context.Context) (Status, error)
	Install(ctx context.Context, definition Definition) error
	Remove(ctx context.Context) error
	Run(ctx context.Context) error
	Stop(ctx context.Context) error
}
```

测试断言生成的 Task Scheduler 定义：同一部署用户、InteractiveToken、RunLevelHighest、LogonTrigger、固定任务路径、固定 Helper/config 参数、不保存密码、不允许任意 action/working directory。

### Step 2：运行 RED 并实现纯定义校验

Run: `go test ./internal/nodetray/windows/task -run TestDefinition -count=1`

Expected: FAIL 后实现，再运行 PASS。

### Step 3：写 COM 适配器契约测试

通过小型 COM backend 接口注入 fake，覆盖安装替换、检查、运行、停止、删除、任务不存在和访问拒绝。普通进程只允许 Inspect/Run；Install/Remove/Stop 若需要提升，由下一任务的 one-shot 模式调用。

### Step 4：实现 Task Scheduler 2.0 适配

使用 Windows COM API，不调用拼接字符串的 `schtasks.exe`。所有路径先取最终规范路径；Task 注册内容不包含凭据。Stop 只停止固定任务实例，不按进程名结束 Helper。

### Step 5：运行 GREEN

Run: `go test ./internal/nodetray/windows/task -count=1`

Expected: PASS；需要真实 Task Scheduler 的测试只在显式集成标签下运行，默认单元测试不改变系统任务。

### Step 6：提交检查点

Run: `git add internal/nodetray/windows/task && git commit -m "feat: manage fixed elevated helper task"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 6：实现一次性 UAC 动作协议和受限提权进程

**Files:**

- Create: `internal/nodetray/windows/elevation/message.go`
- Create: `internal/nodetray/windows/elevation/client_windows.go`
- Create: `internal/nodetray/windows/elevation/server_windows.go`
- Create: `internal/nodetray/windows/elevation/elevation_stub.go`
- Test: `internal/nodetray/windows/elevation/message_test.go`
- Test: `internal/nodetray/windows/elevation/elevation_windows_test.go`
- Create: `internal/nodetray/elevated/actions.go`
- Test: `internal/nodetray/elevated/actions_test.go`

### Step 1：写动作白名单失败测试

只允许以下动作：

```go
type Action string

const (
	ActionWriteHelperConfig Action = "write_helper_config"
	ActionInstallHelperTask Action = "install_helper_task"
	ActionRemoveHelperTask  Action = "remove_helper_task"
)

type Request struct {
	Version uint16 `msgpack:"version"`
	Nonce   string `msgpack:"nonce"`
	Action  Action `msgpack:"action"`
	Payload []byte `msgpack:"payload"`
}

type Response struct {
	Version      uint16 `msgpack:"version"`
	Nonce        string `msgpack:"nonce"`
	OK           bool   `msgpack:"ok"`
	ErrorCode    string `msgpack:"error_code"`
	ErrorSummary string `msgpack:"error_summary"`
}
```

协议版本 1，Payload 上限 256 KiB，Nonce 为 32 字节随机值的 64 位小写十六进制表示。测试拒绝未知动作、路径字段、命令行字段、过大 payload 和 nonce 重放。

### Step 2：运行 RED 并实现消息校验

Run: `go test ./internal/nodetray/windows/elevation -run TestMessage -count=1`

Expected: FAIL 后实现，再运行 PASS。

### Step 3：写 one-shot 握手失败测试

普通进程创建随机命名管道并设置当前用户 + Administrators + SYSTEM ACL，然后通过 `ShellExecuteEx` 的 `runas` 启动同一可执行文件：

```text
nodetray.exe --elevated-once --pipe \\.\pipe\mysingerserver-elevate-<nonce> --nonce <nonce>
```

提升进程必须连接管道、验证服务端进程路径与签名/同一映像、接收一条动作、返回一条响应并退出。命令行不携带配置 JSON、密码或任意目标路径。

测试覆盖 UAC 取消映射为 `UACCancelled=true`、连接超时、nonce 不符、第二条请求拒绝、父进程退出以及响应脱敏。

### Step 4：实现动作执行器

`internal/nodetray/elevated/actions.go` 冻结接口：

```go
type Executor struct {
	HelperConfigPath string
	TaskService      task.Service
}

func (e *Executor) Execute(ctx context.Context, request elevation.Request) elevation.Response
```

`write_helper_config` 的 Payload 只能反序列化为 Task 2 的 `PreparedWrite`，且 TargetPath 必须与构造时固定 HelperConfigPath 完全一致；再次调用 Helper 纯校验，执行 ACL + 原子替换 + last-good。计划任务动作只使用构造时固定路径和已校验 Definition。

### Step 5：运行 GREEN

Run: `go test ./internal/nodetray/windows/elevation ./internal/nodetray/elevated -count=1`

Expected: PASS。

Run: `go test ./internal/nodetray/windows/elevation ./internal/nodetray/elevated -count=20`

Expected: PASS，不出现 nonce 复用或 goroutine 泄漏。

### Step 6：提交检查点

Run: `git add internal/nodetray/windows/elevation internal/nodetray/elevated && git commit -m "feat: restrict one-shot elevated tray actions"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 7：组合后端服务并冻结 Wails 调用边界

**Files:**

- Create: `internal/nodetray/app/service.go`
- Create: `internal/nodetray/app/events.go`
- Create: `internal/nodetray/app/redaction.go`
- Test: `internal/nodetray/app/service_test.go`
- Test: `internal/nodetray/app/redaction_test.go`
- Create: `internal/nodetray/bootstrap/bootstrap.go`
- Test: `internal/nodetray/bootstrap/bootstrap_test.go`

### Step 1：写应用服务契约失败测试

对 UI 暴露的方法冻结为：

```go
type Service struct {
	// 只持有接口依赖
}

func (s *Service) GetOverview(ctx context.Context) (traymodel.Overview, error)
func (s *Service) GetAgentForm(ctx context.Context) (config.AgentForm, error)
func (s *Service) ValidateAgent(ctx context.Context, value config.AgentForm) []config.FieldError
func (s *Service) SaveAgent(ctx context.Context, value config.AgentForm) traymodel.OperationResult
func (s *Service) SaveAndRestartAgent(ctx context.Context, value config.AgentForm) traymodel.OperationResult
func (s *Service) StartAgent(ctx context.Context) traymodel.OperationResult
func (s *Service) StopAgent(ctx context.Context) traymodel.OperationResult
func (s *Service) RestartAgent(ctx context.Context) traymodel.OperationResult
func (s *Service) ForceStopAgent(ctx context.Context) traymodel.OperationResult
func (s *Service) GetHelperForm(ctx context.Context) (config.HelperForm, error)
func (s *Service) ValidateHelper(ctx context.Context, value config.HelperForm) []config.FieldError
func (s *Service) SaveHelper(ctx context.Context, value config.HelperForm) traymodel.OperationResult
func (s *Service) StartHelper(ctx context.Context) traymodel.OperationResult
func (s *Service) StopHelper(ctx context.Context) traymodel.OperationResult
func (s *Service) RestartHelper(ctx context.Context) traymodel.OperationResult
func (s *Service) ForceStopHelper(ctx context.Context) traymodel.OperationResult
func (s *Service) GetTraySettings(ctx context.Context) (traymodel.TraySettings, error)
func (s *Service) SaveTraySettings(ctx context.Context, value traymodel.TraySettings) traymodel.OperationResult
func (s *Service) OpenLocation(ctx context.Context, kind traymodel.LocationKind) traymodel.OperationResult
func (s *Service) ExitTray(ctx context.Context, stopComponents bool) traymodel.OperationResult
```

`Overview` 含 Agent、Worker 汇总、Helper、计划任务和登录启动漂移状态，不含秘密。测试证明：保存未验证表单不会落盘；保存并重启在保存成功后才停止；Helper 配置保存正确发起一次 one-shot UAC；Helper 手动启动使用固定 `runas` Launcher；UAC 取消保留旧配置和旧状态；Helper 自动启动只运行固定任务；`ExitTray(false)` 不调用任何 Supervisor Stop；强制停止方法只转发到对应 Supervisor 的 `ForceStopClaimed`，且必须由 UI 二次确认后单独调用。`OpenLocation` 只接受四个固定枚举并由后端解析目录，未知枚举或不在固定配置/日志根下的路径一律拒绝，前端不能借此打开任意路径。

### Step 2：运行 RED

Run: `go test ./internal/nodetray/app -count=1`

Expected: FAIL，应用服务尚未实现。

### Step 3：实现串联规则与事件总线

事件类型只允许 `component-state`、`operation-progress`、`attention-required`、`settings-changed`。事件 payload 使用冻结 DTO，容量满时合并同组件旧状态，不能阻塞 Supervisor。任何发送到 UI/通知的文本先过 `redaction.go`。

### Step 4：写 Bootstrap 失败测试并实现

`bootstrap` 负责：解析固定部署路径 → 读取托盘设置 → 获取托盘单实例 → 初始化两个 nodectl 客户端 → 尝试重新认领 → 根据启动模式启动 Agent 或请求运行 Helper 任务 → 启动状态刷新。窗口可见时按 1–3 秒设置刷新，隐藏且稳定时最多每 10 秒恢复检查；进程句柄和事件优先。Helper 自动模式不直接 `runas`，手动模式不创建计划任务。

测试覆盖矩阵：登录启动开关与 Agent/Helper 自动/手动组合共 8 种；任何组件失败不阻止窗口/托盘启动；异常退出不触发自动重启。

### Step 5：运行 GREEN

Run: `go test ./internal/nodetray/app ./internal/nodetray/bootstrap -count=1`

Expected: PASS。

Run: `go test ./internal/nodetray/... -count=1`

Expected: PASS。

### Step 6：运行后端静态门禁

Run: `go fmt ./internal/nodetray/...`

Expected: 成功，仅格式化预期文件。

Run: `go vet ./internal/nodetray/...`

Expected: PASS。

Run: `rg -n "postgres(ql)?://|PGPassword|pgPassword|password|ShellExecute|schtasks|taskkill|TerminateProcess" internal/nodetray`

Expected: 每个命中都人工归类；秘密只存在表单输入/受保护配置转换路径，日志/事件/错误 DTO 无秘密；不存在 `schtasks`、`taskkill`；`TerminateProcess` 只能位于进程身份再次验证后的 `ForceStopClaimed` 适配器，普通 Stop/Restart/ExitTray 路径不得调用。

### Step 7：最终提交检查点

Run: `git add internal/nodetray && git commit -m "feat: compose node tray backend services"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## Task 8：后端动态验收骨架和权限边界

**Files:**

- Create: `tests/windows/Test-NodeTrayBackend.ps1`
- Create: `docs/deployment/node-tray-security.md`

### Step 1：编写无副作用预检

脚本必须支持 `-WhatIf`，只检查：测试二进制路径、临时目录范围、当前用户 SID、Task Scheduler 可用性、WebView2 以外的后端前置条件。不得在 `-WhatIf` 下写真实 Run 键、创建计划任务或触发 UAC。

Run: `pwsh -NoProfile -File tests/windows/Test-NodeTrayBackend.ps1 -WhatIf`

Expected: PASS 并打印将使用的临时根和固定任务路径，不修改系统状态。

### Step 2：添加授权后动态场景

脚本在显式 `-AllowUAC -AllowTaskScheduler -AllowHKCUStartup` 三个开关下分别测试：

1. Agent 认领、启动、受控停止、停止超时不强杀；
2. Helper 手动 UAC 取消/同意；
3. Helper 自动任务安装、登录触发定义、运行、停止和删除；
4. 托盘当前用户 Run 值启用/禁用；
5. 临时配置 ACL、原子替换、last-good 恢复；
6. 退出后 Agent/Helper 默认保持运行；
7. “停止组件后退出”受控等待，超时要求明确选择。

每项在执行前保存原系统状态，执行后仅恢复脚本自己修改的固定测试值/测试任务。不得枚举或清理其他任务。

### Step 3：运行单元与预检门禁

Run: `go test ./internal/nodetray/... -count=1`

Expected: PASS。

Run: `pwsh -NoProfile -File tests/windows/Test-NodeTrayBackend.ps1 -WhatIf`

Expected: PASS。

### Step 4：在明确授权的 Windows 验收机会话运行动态测试

Run: `pwsh -NoProfile -File tests/windows/Test-NodeTrayBackend.ps1 -NodeTrayExe artifacts/stage/nodetray.exe -AgentExe artifacts/stage/agent.exe -HelperExe artifacts/stage/helper.exe -TestRoot C:\tmp\mysingerserver-node-tray-backend -AllowUAC -AllowTaskScheduler -AllowHKCUStartup`

Expected: PASS 并生成脱敏证据；若缺少交互式桌面、UAC、任务计划程序或注册表授权，逐项记录 `BLOCKED_NOT_RUN_DYNAMIC`，不得把预检通过写成动态 PASS。

### Step 5：提交检查点

Run: `git add tests/windows/Test-NodeTrayBackend.ps1 docs/deployment/node-tray-security.md && git commit -m "test: define node tray backend acceptance"`

Expected: Git 工作树中提交成功；无 Git 元数据环境记录 `N/A_NO_GIT_METADATA`。

## 完成定义

- 表单与现有 Agent/Helper 配置共用同一纯校验实现，未暴露字段可无损保留。
- 配置写入具备严格 JSON、限制性 ACL、原子替换、复读验证和一份 last-good。
- Supervisor 的五态转换、可信认领、超时不强杀和不无限重启均有测试。
- 托盘/Agent 单实例、HKCU 登录启动、固定 Helper 计划任务均通过可注入后端测试。
- 提权命令只有三个白名单动作，nonce 一次有效，命令行和日志不携带秘密。
- Wails 将调用的 Go 方法和 DTO 已冻结并有无 UI 单元测试。
- 动态 Windows/UAC/计划任务验收已 PASS，或如实标记 `BLOCKED_NOT_RUN_DYNAMIC`。
