# 节点硬件机器唯一 ID 与 GUI 动态发现实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 让 Agent、Helper 和 NodeTray 根据 CPU ID、主板 ID 与 Windows MachineGuid 计算同一个 `node-<64位SHA-256>` 机器唯一 ID，移除可编辑的专属 `machine_id`，并让 GUI 只按地址连接、在 Hello 后动态采用 Agent 身份。

**架构：** 新增 `internal/machineid` 共享包，纯函数负责规范化和摘要，Windows 采集器负责 WMI/注册表；三个节点进程调用同一入口。Agent 的运行时 ID 与磁盘配置分离，GUI Pool 按地址维护连接、按 Hello ID 维护动态身份索引，两个前端分别提供只读节点身份和地址配置。

**技术栈：** Go 1.26.5、Go `testing`、`crypto/sha256`、`github.com/go-ole/go-ole`、`golang.org/x/sys/windows/registry`、Windows WMI/COM、React 19、TypeScript 5.9、Vitest、Vite、Wails 2.12.0

## Global Constraints

- 来源固定为 `Win32_Processor.ProcessorId`、`Win32_BaseBoard.SerialNumber`、`HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`。
- 规范化固定为：去首尾 Unicode 空白和 NUL、转大写、过滤已确认占位值；多条 WMI 结果去重、字典序排序后以 `|` 连接。
- 摘要文本固定使用 LF 且包含结尾 LF；输出固定为 `node-` 加完整 64 位小写 SHA-256。
- 一项或两项不可用时继续并记录来源警告；三项全部不可用时阻止对应组件启动。
- Agent、Helper、NodeTray 使用同一生成器；NodeTray 只读展示，不允许编辑或覆盖。
- GUI endpoint 只包含 `addr`；旧 Agent/GUI 配置中的 `machine_id` 可加载但被忽略，新编码结果必须移除。
- 重复身份按“首个有效在线连接占用”处理；后续连接冲突且不可调度，占用者断线后才允许重新认领。
- 这是个人项目：只执行直接相关的 TDD 和一次最终集中验证，不增加额外安全审查。
- 不启动、停止或重启当前真实进程；真实硬件和多机验收记录为 `NOT_RUN_MANUAL`。
- 当前 checkout 无 Git 元数据，不初始化 Git、不创建提交；版本状态为 `N/A_NO_GIT_METADATA`。
- Go 测试统一使用 `-count=1`。执行前定义：

```powershell
$go = 'C:\Users\Administrator\AppData\Local\Temp\go1.26.5-portable\go\bin\go.exe'
$pnpm = 'C:\Users\Administrator\.cache\codex-runtimes\codex-primary-runtime\dependencies\bin\fallback\pnpm.cmd'
```

## 文件映射

- 新增 `internal/machineid/machineid.go`、`current_windows.go`、`current_other.go`、`machineid_test.go`。
- 修改 `go.mod`、`go.sum`。
- 修改 `internal/config/agent.go`、`internal/config/config_test.go`、`cmd/agent/main.go`、`cmd/agent/main_test.go`。
- 修改 `cmd/helper/main.go`、`cmd/helper/main_test.go`。
- 修改 `internal/nodetray/production/adapters.go`、`internal/nodetray/app/service.go`、`nodetray/composition*.go` 及对应测试。
- 修改 `internal/nodetray/config/forms.go`、Store 测试、NodeTray Agent/Overview 页面和生成的 Wails model。
- 修改 `internal/config/gui.go`、GUI 配置服务/API 测试。
- 修改 `internal/gui/pool.go`、`pool_test.go`、`httpapi_test.go`。
- 修改中央 Web API contracts、设置页、Agent 状态页、扫描页、分组筛选及测试。
- 更新 `internal/gui/webui_dist/`、README、部署示例、`bin/*.json` 和四个 Windows 可执行文件。

---

### Task 1: 建立共享机器身份生成器

**Files:**

- Create: `internal/machineid/machineid.go`
- Create: `internal/machineid/current_windows.go`
- Create: `internal/machineid/current_other.go`
- Create: `internal/machineid/machineid_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**

```go
type Source interface {
	ProcessorIDs() ([]string, error)
	BaseBoardSerialNumbers() ([]string, error)
	MachineGUID() (string, error)
}

type Result struct {
	ID              string
	CPUAvailable    bool
	BoardAvailable  bool
	SystemAvailable bool
	Warnings        []string
}

func Resolve(source Source) (Result, error)
func Current() (Result, error)
func Valid(value string) bool
```

- [ ] **Step 1: 写入黄金摘要、排序、占位过滤和容错测试**

```go
func TestResolveBuildsVersionedStableHardwareIdentity(t *testing.T) {
	got, err := Resolve(fakeSource{
		cpus: []string{" bfebfbff000a0671 ", "BFEBFBFF000A0671"},
		boards: []string{"BOARD-001"},
		system: "00112233-4455-6677-8899-aabbccddeeff",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "node-5af06a5f3367adf7667600b1d18ff5d042d15c51fe531dbbfd348a5e4d7a0ced"
	if got.ID != want || !got.CPUAvailable || !got.BoardAvailable || !got.SystemAvailable {
		t.Fatalf("Result = %#v, want %q and all sources", got, want)
	}
}

func TestResolveSortsAndFiltersPlaceholders(t *testing.T) {
	left, _ := Resolve(fakeSource{
		cpus: []string{"CPU-B", "UNKNOWN", "CPU-A"},
		boards: []string{"TO BE FILLED BY O.E.M.", "BOARD-Z"},
		system: "SYSTEM-X",
	})
	right, _ := Resolve(fakeSource{
		cpus: []string{"cpu-a", "cpu-b"},
		boards: []string{" board-z "},
		system: "system-x",
	})
	if left.ID != right.ID {
		t.Fatalf("enumeration order changed ID: %q != %q", left.ID, right.ID)
	}
}

func TestResolveUsesRemainingSourcesAndRejectsNoSources(t *testing.T) {
	got, err := Resolve(fakeSource{
		cpuErr: errors.New("cpu failed"),
		boards: []string{"DEFAULT STRING"},
		system: "SYSTEM-ONLY",
	})
	if err != nil || got.ID == "" || got.CPUAvailable || got.BoardAvailable ||
		!got.SystemAvailable || len(got.Warnings) != 2 {
		t.Fatalf("partial Result = %#v err=%v", got, err)
	}
	if _, err := Resolve(fakeSource{
		cpuErr: errors.New("cpu"),
		boardErr: errors.New("board"),
		systemErr: errors.New("system"),
	}); err == nil {
		t.Fatal("Resolve accepted three unavailable sources")
	}
}
```

- [ ] **Step 2: 运行测试并确认因实现不存在而失败**

```powershell
& $go test -count=1 ./internal/machineid
```

Expected: FAIL，包或符号尚不存在。

- [ ] **Step 3: 实现纯生成合同**

`machineid.go` 使用以下固定文本：

```go
const canonicalPrefix = "mysingerserver-machine-id:v1\n"

var machineIDPattern = regexp.MustCompile("^node-[0-9a-f]{64}$")

func Valid(value string) bool { return machineIDPattern.MatchString(value) }

func Resolve(source Source) (Result, error) {
	if source == nil {
		return Result{}, errors.New("machine identity unavailable: source is nil")
	}
	cpus, cpuErr := source.ProcessorIDs()
	boards, boardErr := source.BaseBoardSerialNumbers()
	system, systemErr := source.MachineGUID()
	cpu := normalizeMany(cpus)
	board := normalizeMany(boards)
	systemValues := normalizeMany([]string{system})
	result := Result{
		CPUAvailable: len(cpu) > 0,
		BoardAvailable: len(board) > 0,
		SystemAvailable: len(systemValues) > 0,
	}
	result.Warnings = sourceWarnings(cpuErr, boardErr, systemErr, result)
	if !result.CPUAvailable && !result.BoardAvailable && !result.SystemAvailable {
		return Result{}, errors.New("machine identity unavailable: no valid CPU, board, or system ID")
	}
	systemValue := ""
	if len(systemValues) != 0 {
		systemValue = systemValues[0]
	}
	canonical := canonicalPrefix +
		"cpu=" + strings.Join(cpu, "|") + "\n" +
		"board=" + strings.Join(board, "|") + "\n" +
		"system=" + systemValue + "\n"
	digest := sha256.Sum256([]byte(canonical))
	result.ID = "node-" + hex.EncodeToString(digest[:])
	return result, nil
}
```

`normalizeMany` 必须 trim、转大写、过滤设计文档中的占位值和仅由 `0`/`-`/空白组成的值，再去重排序。警告只能包含来源名和失败类别，不能包含原始 ID。

- [ ] **Step 4: 实现 Windows 采集器和非 Windows stub**

`current_windows.go` 使用 `runtime.LockOSThread`、`ole.CoInitializeEx`、`WbemScripting.SWbemLocator`、`ConnectServer`、`ExecQuery` 读取两个 WMI 属性；所有 COM 对象和 VARIANT 必须 `Release/Clear`。MachineGuid 使用 64 位注册表视图：

```go
key, err := registry.OpenKey(
	registry.LOCAL_MACHINE,
	"SOFTWARE\\Microsoft\\Cryptography",
	registry.QUERY_VALUE|registry.WOW64_64KEY,
)
if err != nil {
	return "", err
}
defer key.Close()
value, _, err := key.GetStringValue("MachineGuid")
return value, err
```

`Current()` 调用 `Resolve(windowsSource{})`；非 Windows 返回 `machine identity unavailable: unsupported platform`。把 `github.com/go-ole/go-ole v1.3.0` 提升为直接依赖。

- [ ] **Step 5: 整理模块并验证**

```powershell
& $go mod tidy
& $go test -count=1 ./internal/machineid
```

Expected: PASS，不引入 PowerShell、wmic 或新 WMI 包。

---

### Task 2: 迁移 Agent 配置并在启动早期注入机器 ID

**Files:**

- Modify: `internal/config/agent.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`

**Interfaces:**

- Consumes: `machineid.Current() (machineid.Result, error)`。
- Produces: `AgentConfig.MachineID string` 为 `json:"-"` 运行时字段，配置指纹不包含它。

- [ ] **Step 1: 写入新旧配置兼容测试**

```go
func TestLoadAgentAcceptsMissingAndIgnoresLegacyMachineID(t *testing.T) {
	without := validAgentJSONWithoutMachineID(t)
	cfg := loadAgentFixture(t, without)
	if cfg.MachineID != "" {
		t.Fatalf("runtime MachineID = %q before injection", cfg.MachineID)
	}
	withLegacy := addTopLevelJSONField(t, without, "machine_id", "legacy-manual-id")
	legacy := loadAgentFixture(t, withLegacy)
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.MachineID != "" || bytes.Contains(encoded, []byte("\"machine_id\"")) {
		t.Fatalf("legacy ID survived: cfg=%#v json=%s", legacy, encoded)
	}
}
```

主程序测试注入 `node-` 加 64 个 `a`，验证控制身份、单实例、Worker 配置和 Agent Server 都收到该值；provider 返回错误时，数据库、日志、Worker 和监听器工厂均未调用。

- [ ] **Step 2: 运行测试并确认旧必填规则失败**

```powershell
& $go test -count=1 ./internal/config ./cmd/agent
```

Expected: FAIL，缺少 `machine_id` 仍被拒绝或运行时 ID 未注入。

- [ ] **Step 3: 分离序列化配置和运行时 ID**

将 `MachineID` 改为 `json:"-"`，从 `ValidateAgent` 删除人工 ID 校验。实现严格 `UnmarshalJSON`：使用 AgentConfig alias、`DisallowUnknownFields`，显式接收但丢弃顶层 `machine_id json.RawMessage`，并拒绝尾随 JSON；其他未知字段仍失败。这样 `LoadAgent` 和 NodeTray 的严格 loader 都兼容旧文件，重新 marshal 会自动剥离旧字段。

- [ ] **Step 4: 在下游初始化前解析身份**

增加可测试入口：

```go
type machineIdentityProvider func() (machineid.Result, error)

func runWithDependencies(
	configPath string,
	openDeleteLogger deleteLoggerFactory,
	resolveIdentity machineIdentityProvider,
) error
```

`LoadAgent` 后立即解析身份，失败包装为 `resolve Agent machine identity`；成功后 `cfg.MachineID = identity.ID`，再执行 executable 校验、单实例、配置指纹、数据库、Worker、同步和 Server 初始化。日志创建后逐条记录来源警告，不记录原始 ID。

- [ ] **Step 5: 运行 Agent 回归测试**

```powershell
& $go test -count=1 ./internal/config ./internal/agent ./cmd/agent
```

Expected: PASS。原有 Server 测试可继续直接设置运行时 `cfg.MachineID`。

---

### Task 3: 让 Helper 与 NodeTray 后端共享同一身份

**Files:**

- Modify: `cmd/helper/main.go`
- Modify: `cmd/helper/main_test.go`
- Modify: `internal/nodetray/production/adapters.go`
- Modify: `internal/nodetray/production/adapters_test.go`
- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`
- Modify: `nodetray/composition.go`
- Modify: `nodetray/composition_test.go`
- Modify: `nodetray/composition_windows.go`
- Modify: `nodetray/composition_windows_test.go`

**Interfaces:**

```go
func NewAgentController(dialer Dialer, machineID string) (*FixedController, error)
func NewHelperController(dialer Dialer, machineID string) (*FixedController, error)
```

`trayapp.Dependencies` 增加只读 `MachineID string`；保存流程删除 `MachineIDUpdater` 依赖。

- [ ] **Step 1: 写入 Helper 和 NodeTray 统一身份测试**

Helper provider 返回：

```go
machineid.Result{
	ID: "node-" + strings.Repeat("b", 64),
	CPUAvailable: true,
	Warnings: []string{"board source unavailable"},
}
```

断言 Helper 控制状态使用该值而非 hostname；provider 失败时控制管道和删除服务不启动。NodeTray 测试断言两个 controller 接受同一个注入 ID、拒绝不同上报 ID，Overview 返回依赖 ID，SaveAgent 不调用身份更新器。

- [ ] **Step 2: 运行测试并确认仍依赖 hostname/配置值**

```powershell
& $go test -count=1 ./cmd/helper ./internal/nodetray/production ./internal/nodetray/app ./nodetray
```

Expected: FAIL。

- [ ] **Step 3: 修改 Helper 启动依赖**

把 `dependencies.machineID func() (string, error)` 改为 `identity machineIdentityProvider`，生产值为 `machineid.Current`。`runWith` 在创建锁和监听器前解析身份，用 `result.ID` 校验并创建 Helper control provider；日志创建后记录警告。

- [ ] **Step 4: 固定 NodeTray 两个控制器的身份**

两个 controller 构造器只接受已计算的 machineID，先调用现有 `validMachineID`，再保存固定值。删除 previousMachineID 兼容窗口和可变身份更新逻辑。`GetOverview` 使用 `s.machineID`；`saveAgentLocked` 保存配置并更新摘要后直接刷新状态，不再更新身份。

- [ ] **Step 5: Windows composition 只计算一次**

`composeWindowsProductionBackend` 调用一次 `machineid.Current()`，失败返回 `production composition: machine identity unavailable`。通过现有 NodeTray 启动日志逐条记录 `result.Warnings`，只记录来源名与失败类别，不记录任何原始硬件值；把 `result.ID` 同时传给：

```go
agentController := func(context.Context) (supervisor.Controller, error) {
	return production.NewAgentController(native.Dialer, native.MachineID)
}
helperController := func(context.Context) (supervisor.Controller, error) {
	return production.NewHelperController(native.Dialer, native.MachineID)
}
```

并通过 `productionCompositionInputs.MachineID` 注入 App Service。测试 ID 全部改为完整 `node-<64hex>`。

- [ ] **Step 6: 运行后端回归测试**

```powershell
& $go test -count=1 ./cmd/helper ./internal/nodetray/... ./nodetray
```

Expected: PASS。

---

### Task 4: 移除 NodeTray 可编辑 ID 并只读展示

**Files:**

- Modify: `internal/nodetray/config/forms.go`
- Modify: `internal/nodetray/config/forms_test.go`
- Modify: `internal/nodetray/config/store_test.go`
- Modify: `nodetray/frontend/src/pages/AgentPage.tsx`
- Modify: `nodetray/frontend/src/pages/AgentPage.test.tsx`
- Modify: `nodetray/frontend/src/pages/OverviewPage.tsx`
- Modify: `nodetray/frontend/src/pages/OverviewPage.test.tsx`
- Regenerate: `nodetray/frontend/wailsjs/go/models.ts`

**Interfaces:**

- Produces: `config.AgentForm` 不再含 `MachineID/machineId`。
- Consumes: `traymodel.Overview.MachineID` 作为只读值。

- [ ] **Step 1: 写入表单和页面失败测试**

Go 测试断言 `AgentToForm/AgentFromForm/SaveAgentForm` 的 JSON 和磁盘规范配置均不含 `machine_id`，旧文件加载后保存会剥离旧字段。React 测试断言：

```tsx
expect(screen.queryByLabelText('机器 ID')).not.toBeInTheDocument()
expect(screen.getByText('node-' + 'a'.repeat(64))).toBeInTheDocument()
```

dirty/save 测试改为编辑“监听地址”或“数据目录”，保存请求不得含 `machineId`。

- [ ] **Step 2: 运行测试并确认旧字段仍存在**

```powershell
& $go test -count=1 ./internal/nodetray/config
Push-Location .\nodetray\frontend
& $pnpm test -- src/pages/AgentPage.test.tsx src/pages/OverviewPage.test.tsx
Pop-Location
```

Expected: FAIL。

- [ ] **Step 3: 修改 Go 表单和前端**

从 `AgentForm`、`AgentToForm`、`AgentFromForm`、NodeTray `emptyForm` 和常用设置 fieldset 删除机器 ID。`AgentFromForm` 生成的新配置保持运行时 MachineID 为空。在 Overview Agent 摘要增加：

```tsx
<div>
  <dt>机器 ID</dt>
  <dd className="mono" title={overview.machineId}>{overview.machineId || '—'}</dd>
</div>
```

- [ ] **Step 4: 重新生成 Wails model**

```powershell
Push-Location .\nodetray
& $go run github.com/wailsapp/wails/v2/cmd/wails@v2.12.0 generate module
Pop-Location
```

Expected: 生成的 `config.AgentForm` 无 `machineId`。

- [ ] **Step 5: 运行 NodeTray 门禁**

```powershell
& $go test -count=1 ./internal/nodetray/... ./nodetray
Push-Location .\nodetray\frontend
& $pnpm test
& $pnpm run lint
& $pnpm run build
Pop-Location
```

Expected: 全部 PASS。

---

### Task 5: 把 GUI endpoint 配置迁移为仅地址

**Files:**

- Modify: `internal/config/gui.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/gui/config_service_test.go`
- Modify: `internal/gui/config_http_test.go`
- Modify: `cmd/gui/main_test.go`

**Interfaces:**

```go
type AgentEndpoint struct {
	Addr string `json:"addr"`
}
```

`LoadGUI` 继续宽松读取旧 endpoint 的 `machine_id`；严格 Web PUT 对旧字段返回 unknown field。

- [ ] **Step 1: 写入地址唯一和单向迁移测试**

```go
func TestLoadGUIIgnoresLegacyMachineIDAndNewEncodingRemovesIt(t *testing.T) {
	cfg := loadGUIFixture(t, "gui.json", []byte(
		"{\"listen_addr\":\"127.0.0.1:18080\"," +
			"\"pg_dsn\":\"postgres://fixture.invalid/dedup\"," +
			"\"heartbeat_s\":15," +
			"\"agents\":[{\"machine_id\":\"machine-a\",\"addr\":\"127.0.0.1:9101\"}]}",
	))
	if len(cfg.Agents) != 1 || cfg.Agents[0].Addr != "127.0.0.1:9101" {
		t.Fatalf("Agents = %#v", cfg.Agents)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("\"machine_id\"")) {
		t.Fatalf("new GUI encoding retained legacy ID: %s", encoded)
	}
}
```

另测两个相同地址在 `agents[1].addr` 返回 `duplicate`；配置 API GET 只返回 addr；PUT endpoint 含 `machine_id` 时被严格拒绝。

- [ ] **Step 2: 运行配置/API 测试并确认旧模型失败**

```powershell
& $go test -count=1 ./internal/config ./internal/gui ./cmd/gui
```

Expected: FAIL，旧验证仍要求 `agents[i].machine_id`。

- [ ] **Step 3: 删除 endpoint MachineID 并校验地址唯一**

`ValidateGUI` 使用大小写无关的地址 key：

```go
seen := make(map[string]bool, len(cfg.Agents))
for index, endpoint := range cfg.Agents {
	field := fmt.Sprintf("agents[%d].addr", index)
	key := strings.ToLower(endpoint.Addr)
	switch {
	case endpoint.Addr == "":
		validation.add(field, "required", "Agent 地址不能为空")
	case !validGUIHostPort(endpoint.Addr):
		validation.add(field, "invalid_address", "地址必须是 host:port")
	case seen[key]:
		validation.add(field, "duplicate", "Agent 地址不能重复")
	default:
		seen[key] = true
	}
}
```

`LoadGUI` 当前使用普通 `json.Unmarshal`，因此旧 machine_id 会自然忽略；Web 配置 API 的 `DisallowUnknownFields` 会拒绝新 PUT 中的旧字段。更新所有配置 fixtures，但数据库任务与协议里的运行时 machine_id 不变。

- [ ] **Step 4: 运行配置回归测试**

```powershell
& $go test -count=1 ./internal/config ./internal/gui ./cmd/gui
```

Expected: PASS。

---

### Task 6: 将 GUI Pool 改为地址连接和动态身份占用

**Files:**

- Modify: `internal/gui/pool.go`
- Modify: `internal/gui/pool_test.go`
- Modify: `internal/gui/httpapi_test.go`

**Interfaces:**

```go
type IdentityState string

const (
	IdentityPending  IdentityState = "pending"
	IdentityClaimed  IdentityState = "claimed"
	IdentityConflict IdentityState = "conflict"
)

type AgentStatus struct {
	MachineID     string        `json:"machine_id"`
	Addr          string        `json:"addr"`
	Online        bool          `json:"online"`
	IdentityState IdentityState `json:"identity_state"`
	LastErr       string        `json:"last_err,omitempty"`
}
```

Pool 内部使用 `byAddr map[string]*AgentConn`、`byMachineID map[string]*AgentConn` 和独立身份 mutex。

- [ ] **Step 1: 替换旧 mismatch 测试并加入冲突测试**

```go
const machineA = "node-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const machineB = "node-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
```

测试必须覆盖：

- endpoint 只有地址，Hello machineA 后消息回调和 onConnect 收到 machineA；
- Hello machineB 不再与配置值比较；
- 空值、`machine-a`、长度错误和大写摘要被拒绝；
- 两个连接 claim machineA 时首个成功，第二个为 conflict 且 `Send` 仍指向首个；
- 首个 release 后第二个可再次 claim；
- 旧连接延迟 release 不会删除已指向新连接的映射；
- Hello 前 pending、机器 ID 为空、不可调度。

- [ ] **Step 2: 运行 Pool 测试并确认旧索引失败**

```powershell
& $go test -count=1 ./internal/gui -run 'Test(AgentConn|Pool)'
```

Expected: FAIL，仍出现旧 mismatch 或 `pool.conns[machineID]` 假设。

- [ ] **Step 3: 实现身份 claim/release**

```go
func (pool *Pool) claimIdentity(conn *AgentConn, machineID string) error {
	if !machineid.Valid(machineID) {
		return fmt.Errorf("invalid agent machine_id %q", machineID)
	}
	pool.identityMu.Lock()
	defer pool.identityMu.Unlock()
	if existing := pool.byMachineID[machineID]; existing != nil && existing != conn {
		return fmt.Errorf("identity conflict: machine_id %s already connected", machineID)
	}
	pool.byMachineID[machineID] = conn
	return nil
}

func (pool *Pool) releaseIdentity(conn *AgentConn, machineID string) {
	pool.identityMu.Lock()
	if pool.byMachineID[machineID] == conn {
		delete(pool.byMachineID, machineID)
	}
	pool.identityMu.Unlock()
}
```

`AgentConn.runOnce` 在协议版本验证后记录 Hello ID、claim、defer release，再 setOnline。冲突时记录 `IdentityConflict` 并返回；成功后日志、回调和消息分发全部使用 Hello ID。`setOffline` 清理连接但保留当前进程内最后识别 ID。

- [ ] **Step 4: 改造查询和状态**

`NewPool` 按 Addr 建立 `byAddr`；`Start/Status` 遍历地址索引。`Send/IsOnline` 在身份 mutex 下从 `byMachineID` 取连接。状态按在线优先、机器 ID、地址稳定排序；待识别行由前端使用地址作为 key。

- [ ] **Step 5: 运行 Pool 和 HTTP 回归**

```powershell
& $go test -count=1 ./internal/gui ./cmd/gui
rg -n "machine_id mismatch: config=" .\internal\gui
```

Expected: 测试 PASS，rg 无匹配。

---

### Task 7: 更新中央 Web 配置与动态身份状态

**Files:**

- Modify: `webui/src/api/contracts.ts`
- Modify: `webui/src/api/appApi.ts`
- Modify: `webui/src/api/appApi.test.ts`
- Modify: `webui/src/features/settings/GUISettingsPage.tsx`
- Modify: `webui/src/features/settings/GUISettingsPage.test.tsx`
- Modify: `webui/src/features/agents/AgentsPage.tsx`
- Modify: `webui/src/features/agents/AgentsPage.test.tsx`
- Modify: `webui/src/features/scans/ScansPage.tsx`
- Modify: `webui/src/features/scans/ScansPage.test.tsx`
- Modify: `webui/src/features/groups/GroupFilters.tsx`
- Modify: `webui/src/features/groups/GroupsPage.test.tsx`
- Regenerate: `internal/gui/webui_dist/`

**Interfaces:**

```ts
export type AgentIdentityState = "pending" | "claimed" | "conflict";

export interface AgentStatus {
  machineId: string;
  addr: string;
  online: boolean;
  identityState: AgentIdentityState;
  lastErr?: string;
}

export interface GUIAgentConfig {
  addr: string;
}
```

- [ ] **Step 1: 写入 API 映射和页面失败测试**

`appApi.test.ts` 固定 pending 映射和保存体：

```ts
expect(await api.listAgents()).toEqual([{
  machineId: "",
  addr: "192.168.1.10:9101",
  online: false,
  identityState: "pending"
}]);

expect(savedBody.agents).toEqual([
  { addr: "192.168.1.10:9101" },
  { addr: "192.168.1.11:9101" }
]);
```

页面测试固定：设置页没有机器标识输入框；pending 显示“待识别”；conflict 显示“身份冲突”；多个 pending 行 key 不冲突；conflict 与 claimed 上报同一 ID 时扫描和分组筛选只出现一个机器选项。

- [ ] **Step 2: 运行定向 Web 测试并确认旧合同失败**

```powershell
Push-Location .\webui
& $pnpm test -- src/api/appApi.test.ts src/features/settings/GUISettingsPage.test.tsx src/features/agents/AgentsPage.test.tsx src/features/scans/ScansPage.test.tsx src/features/groups/GroupsPage.test.tsx
Pop-Location
```

Expected: FAIL。

- [ ] **Step 3: 修改 API 类型和转换**

`agents()` 映射并严格枚举 `identity_state`；`guiConfig()` 的 agents 只读取 addr；`guiConfigInput()` 只发送：

```ts
agents: value.agents.map(agent => ({ addr: agent.addr }))
```

不得从 `last_err` 文本推断身份状态。

- [ ] **Step 4: 修改设置页和状态页**

`GUISettingsPage` 的 `updateAgent` 只接受 `"addr"`，新增行为 `{ addr: "" }`，删除机器标识控件及错误绑定。`AgentsPage` 以 addr 为行 key，机器列显示 `agent.machineId || "待识别"`，状态标签：

```ts
function statusLabel(agent: AgentStatus): string {
  if (agent.identityState === "conflict") return "身份冲突";
  if (agent.identityState === "pending") return "待识别";
  return agent.online ? "在线" : "离线";
}
```

- [ ] **Step 5: 去重业务选择项**

扫描页和分组筛选只对非空 machineId 建立按 ID 去重的列表；同 ID 多行时优先保留 `online && identityState === "claimed"` 的状态。在线删除判断继续使用 online 集合，因此 conflict/pending 不会被视为可用机器。

- [ ] **Step 6: 运行完整 Web 门禁并更新嵌入资源**

```powershell
Push-Location .\webui
& $pnpm test
& $pnpm run lint
& $pnpm run build
Pop-Location
.\scripts\build-web.ps1 -VerifyEmbedded
```

Expected: 全部 PASS。若脚本仅因当前环境没有 npm 被阻塞，前三项完成后把嵌入门禁记录为 `BLOCKED_NPM_NOT_FOUND`，不得报告为 PASS。

---

### Task 8: 更新配置说明、集中验证并编译

**Files:**

- Modify: `README.md`
- Modify: `deploy/README-节点部署.md`
- Modify: `deploy/agent.example.json`
- Modify: `deploy/gui.example.json`
- Modify: `bin/agent.json`
- Modify: `bin/gui.json`
- Build: `bin/agent.exe`
- Build: `bin/helper.exe`
- Build: `bin/gui.exe`
- Build: `artifacts/stage/nodetray.exe`

**Interfaces:**

- 文档和示例只公开地址配置与自动生成 ID，不再要求两端人工对齐。

- [ ] **Step 1: 更新文档和 JSON 示例**

Agent 示例删除 `"machine_id": "media-pc-1"`。GUI 示例改为：

```json
"agents": [
  { "addr": "192.168.1.101:9101" },
  { "addr": "192.168.1.102:9101" }
]
```

README 说明 ID 为 `node-<sha256>`，由三个来源自动计算；故障排查改为核对 Agent 地址、监听地址、状态页上报 ID 和防火墙。NodeTray 说明机器 ID 在概览只读显示。

- [ ] **Step 2: 扫描残留的人工配置语义**

```powershell
rg -n 'machine_id.*必须|machine_id.*一致|机器 ID.*输入|机器标识' README.md deploy bin nodetray\frontend\src webui\src internal\nodetray\config
```

Expected: 没有人工填写或两端一致的现行说明；数据库、任务和协议中的运行时 machine_id 保留。

- [ ] **Step 3: 执行最终 Go 集中验证**

```powershell
& $go test -count=1 ./internal/machineid ./internal/config ./internal/agent ./cmd/agent ./cmd/helper ./internal/gui ./cmd/gui ./internal/nodetray/... ./nodetray
```

Expected: PASS。

- [ ] **Step 4: 执行两个前端集中验证**

```powershell
Push-Location .\webui
& $pnpm test
& $pnpm run lint
& $pnpm run build
Pop-Location
Push-Location .\nodetray\frontend
& $pnpm test
& $pnpm run lint
& $pnpm run build
Pop-Location
```

Expected: 全部 PASS。

- [ ] **Step 5: 编译 Agent、隐藏窗口 Helper 和 GUI**

```powershell
& $go build -trimpath -o .\bin\agent.exe .\cmd\agent
& $go build -trimpath -ldflags '-H=windowsgui' -o .\bin\helper.exe .\cmd\helper
& $go build -trimpath -o .\bin\gui.exe .\cmd\gui
```

Expected: 三个命令退出码 0；Helper 保持 Windows GUI 子系统，不显示 cmd 窗口。

- [ ] **Step 6: 编译 NodeTray**

```powershell
.\scripts\build-nodetray.ps1 -Go $go -OutDir .\artifacts\stage
```

Expected: `artifacts\stage\nodetray.exe` 通过 x64、manifest 和 WebView2 检查。若仅因 npm 缺失而阻塞，记录 `BLOCKED_NPM_NOT_FOUND`，不以未校验产物替代 PASS。

- [ ] **Step 7: 记录产物证据和手工验收边界**

```powershell
$artifacts = @(
  '.\bin\agent.exe',
  '.\bin\helper.exe',
  '.\bin\gui.exe',
  '.\artifacts\stage\nodetray.exe'
)
Get-Item -LiteralPath $artifacts | Select-Object FullName,Length,LastWriteTime
Get-FileHash -LiteralPath $artifacts -Algorithm SHA256 | Select-Object Path,Hash
```

最终报告分别列出测试、构建、嵌入资源和 NodeTray 供应链状态。真实硬件读取、真实 Agent/Helper/NodeTray 握手和重复身份多 endpoint 认领均标记 `NOT_RUN_MANUAL`，不得启动现有进程代替用户验收。
