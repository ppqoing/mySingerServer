# 自适应磁盘 I/O、RGB 视频缩略图与完整视频元数据 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不新增读取进程、不传输媒体字节的前提下，让 Agent 按物理磁盘集中调度 Worker 的源媒体 read/seek，以完整扫描墙钟时间最短为目标自适应提高并发；同时把视频联系表改为真实 RGB24 彩色 JPEG、移除缩略图旁车/锁，并把容器与全部轨道元数据原子保存到 Compute/Manager。

**Architecture:** 保留现有 `Agent -> Worker IPC -> VideoCore` 边界。新增纯 Go `internal/diskio` 调度核心，Agent 负责物理磁盘身份、公平队列、AIMD 并发预算和生命周期取消；Worker 保留文件句柄与小型本地租约，并在 Go 读取及 VideoCore `WinFile::Read/Seek` 前消费配额。VideoCore 从同一个 `AVFormatContext` 同时产出既有灰度特征、受限 RGB tile 和容器/轨道快照；Worker 使用严格有界协议回传，Store 在同一事务中持久化并进入现有同步队列。

**Tech Stack:** Go 1.26、Windows Named Pipe/MessagePack、SQLite/modernc、PostgreSQL、C++17、FFmpeg libavformat/libavcodec/libswscale、libjpeg-turbo、React 19、TypeScript、Vitest/React Testing Library、PowerShell 发布脚本。

## Global Constraints

- 第一优化目标是同一完整扫描任务的墙钟时间最短；允许 CPU/磁盘利用率波动，不以平滑曲线或固定百分比作为成功条件。
- 不新增 `reader.exe`，不通过 IPC 传输媒体字节；Worker 保留源文件句柄，Agent 只传租约控制消息和统计。
- 同一物理磁盘的多个盘符/分区和多个任务必须共享一个调度预算；无法解析物理盘时进入受控卷桶，不能绕过调度。
- 顺序读本地窗口初始 4 MiB、可调 1–16 MiB；seek 必须单独申请令牌。队列、租约、消息体、轨道数和 tags 总字节都要有硬上限。
- 租约等待不消耗探测/解码超时；真实 read/seek、解码和算法执行仍受原超时约束，禁止靠延长超时掩盖问题。
- 暂停、停止、删除、关机优先于新租约；stale/实例不匹配报告只允许回收预算，不得更新当前任务进度。
- 缩略图可见画布固定 `AV_PIX_FMT_RGB24`、字节顺序 `R8G8B8`，TurboJPEG 固定 `TJPF_RGB + TJSAMP_420 + quality 90`；既有灰度特征与 golden 不变。
- 新缓存只使用 `thumbcache\<sha512前两位>\<sha512>.jpg`；不得创建版本目录、`.jpg.json`、`.jpg.lock` 或缩略图缓存元数据。
- 旧 `thumbcache\vc-grid-v1` 不读取、不迁移、不删除；用户以后手动清理，本计划没有清理授权。
- 视频元数据直接来自已打开的 `AVFormatContext`，不启动 `ffprobe.exe`、不重复读取源视频；不适用字段存 NULL，禁止用虚假零值表示未知。
- Worker IPC、VideoCore ABI、SQLite schema 按版本 fail closed；Agent、Worker、VideoCore DLL 必须作为同一发布闭包升级。
- 所有实现任务先取得确定性 RED，再做最小 GREEN；竞态测试用假时钟、通道和 barrier，不用 `Sleep` 判断时序。
- 不自动删除用户数据或旧缓存。正式基准前只检查 D 盘空间；不足时标记 BLOCKED，不执行清理。

## 文件结构与职责

- `internal/diskio/`：共享类型、纯调度策略、假时钟和按任务/物理盘统计。
- `internal/diskmap/`：Windows 卷到物理磁盘 extents 的解析与保守 fallback key。
- `internal/agent/scan.go`：有界派发、任务身份、持久化完成边界和调度快照上报。
- `internal/worker/`：租约 IPC、父进程 broker、Worker 崩溃回收和严格协议校验。
- `internal/wproc/`：Worker 本地租约客户端、Go 源文件读取包装器、纯 JPEG 缓存发布。
- `internal/wproc/videocore/`：VideoCore ABI 绑定、数字 governor handle、元数据拷贝和边界校验。
- `videocore/`：read/seek governor、可暂停 deadline、RGB tile/联系表、容器与全部轨道快照。
- `internal/store/`、`internal/syncer/`、`deploy/central.sql`：SQLite v5、PostgreSQL 表、原子替换和同步。
- `internal/proto/`、`cmd/agent/main.go`、`internal/nodetray/`、`nodetray/frontend/`：任务 I/O 指标契约、Wails DTO 和可展开任务详情。
- `scripts/benchmark-scan-io.ps1`：相同字段、相同输入、固定基线与自适应版本的生产墙钟验收。

---

### Task 1: 建立稳定物理磁盘键和调度配置

**Files:**
- Create: `internal/diskio/model.go`
- Create: `internal/diskio/model_test.go`
- Modify: `internal/diskmap/diskmap_windows.go:21-160`
- Modify: `internal/diskmap/diskmap_windows_test.go`
- Modify: `internal/config/agent.go:18-31,63-77,118-165,250-285`
- Modify: `internal/config/config_test.go`

**Interfaces:**

```go
package diskio

type DiskKey string

type SourceClass uint8

const (
	SourceSequential SourceClass = iota + 1
	SourceRandom
)

type Identity struct {
	Key       DiskKey
	Local     bool
	SSD       bool
	KnownSSD  bool
	Volume    string
	DiskNos   []uint32
}

type PolicyConfig struct {
	LeaseBytes            int64
	MinLeaseBytes         int64
	MaxLeaseBytes         int64
	HDDInitial            int
	SSDInitial            int
	MaxPerDisk            int
	HDDRandomMax          int
	Window                time.Duration
	IncreaseThreshold     float64
	DecreaseThreshold     float64
	MaxQueuedPerWorker    int
}
```

面向 JSON 的配置留在 `internal/config`，避免把 `time.Duration` 直接序列化为纳秒：

```go
type IOConfig struct {
	LeaseMB            int     `json:"lease_mb"`
	MinLeaseMB         int     `json:"min_lease_mb"`
	MaxLeaseMB         int     `json:"max_lease_mb"`
	HDDInitial         int     `json:"hdd_initial"`
	SSDInitial         int     `json:"ssd_initial"`
	MaxPerDisk         int     `json:"max_per_disk"`
	HDDRandomMax       int     `json:"hdd_random_max"`
	WindowMS           int     `json:"window_ms"`
	IncreaseThreshold  float64 `json:"increase_threshold"`
	DecreaseThreshold  float64 `json:"decrease_threshold"`
	MaxQueuedPerWorker int     `json:"max_queued_per_worker"`
}
```

- [ ] **Step 1: 写 Windows extents 与配置校验 RED**

在 `diskmap_windows_test.go` 用可注入 device-control seam 覆盖：两个盘符映射到同一 disk number 得到相同 key；跨盘卷的 extents 排序后得到稳定组合 key；IOCTL 失败退化为 `volume:<guid>`；UNC 得到 `network:<server/share>`。在 `config_test.go` 断言 4 MiB、1–16 MiB、HDD=2、SSD=4、总上限 24 的默认值，以及非法窗口/阈值/上限 fail closed。

- [ ] **Step 2: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/diskio ./internal/diskmap ./internal/config -run 'Test(DiskIdentity|IOPolicy)'
```

Expected: `internal/diskio` 不存在，且现有 `diskmap.Info` 只能返回单个 `DeviceNumber`，新测试不能编译。

- [ ] **Step 3: 实现模型、extents 和配置**

在 `AgentConfig` 增加 `IO IOConfig \`json:"io"\``，由 `IOConfig.Policy(workerCount)` 转成 `diskio.PolicyConfig`。`diskmap.Resolve` 使用 `IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS`，复制并排序全部 disk number 后生成：单盘 `physical:<n>`、多盘 `physical-set:<n,n,...>`、fallback `volume:<guid>`。保留现有 `DeviceNumber/PartitionNumber` 字段以免破坏调用方，新增 `Identity diskio.Identity`。

- [ ] **Step 4: 运行 GREEN 与现有配置回归**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/diskio ./internal/diskmap ./internal/config
```

- [ ] **Step 5: 提交 Task 1**

```powershell
git add -- internal/diskio/model.go internal/diskio/model_test.go internal/diskmap/diskmap_windows.go internal/diskmap/diskmap_windows_test.go internal/config/agent.go internal/config/config_test.go
git commit -m "feat: model physical disk IO identity"
```

---

### Task 2: 实现确定性的自适应磁盘调度核心

**Files:**
- Create: `internal/diskio/controller.go`
- Create: `internal/diskio/controller_test.go`
- Create: `internal/diskio/fake_clock_test.go`

**Interfaces:**

```go
type Request struct {
	RequestID  uint64
	TaskID     string
	InstanceID string
	WorkerID   int
	Disk       DiskKey
	Class      SourceClass
	WantBytes  int64
	WantSeek   bool
}

type Grant struct {
	LeaseID    uint64
	Generation uint64
	Bytes      int64
	Seeks      uint32
}

type Report struct {
	LeaseID, Generation uint64
	TaskID, InstanceID string
	WorkerID int
	Disk DiskKey
	Bytes int64
	Seeks uint32
	ReadTime, WaitTime time.Duration
	Completed, Cancelled bool
}

type Snapshot struct {
	Concurrency, BusyWorkers, IOWaitWorkers int
	EffectiveBytesPerSecond float64
	LeaseWait time.Duration
	SequentialBytes int64
	SeekCount int64
}

type Controller interface {
	Acquire(context.Context, Request) (Grant, error)
	Report(Report)
	CancelTask(taskID, instanceID string)
	ReclaimWorker(workerID int)
	Snapshot(taskID, instanceID string) Snapshot
}
```

- [ ] **Step 1: 写 AIMD、平台探测和硬上限 RED**

用 fake clock 构造至少 2 秒且有足够字节的观测窗口。断言吞吐提升 `>=5%` 时并发 `+1`；下降 `>8%`、P95 激增或 seek 拥塞时至少降低 1、最多降低 `max(1,current/4)`；平台期只探测 `n+1`；Worker 全忙不增，持续队列加空闲 Worker 快速增；HDD 随机并发不超过 8，总并发不超过 `min(workerCount,24,config)`。

- [ ] **Step 2: 写公平性、取消和 stale RED**

覆盖两任务各获最低份额、任务多于槽位时 round-robin、等待年龄防饥饿、暂停任务取消等待申请、旧 generation/instance 报告只回收不改 Snapshot、Worker 崩溃归还未使用预算、每 Worker 最多一个有效小窗口。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/diskio -run 'TestController'
```

Expected: 缺少 `NewController/Acquire/Report`。

- [ ] **Step 4: 实现单 owner-loop 控制器**

所有磁盘状态由一个 goroutine 拥有；调用方通过有界命令通道进入。每个 `DiskKey` 保存 active leases、按 `(taskID,instanceID)` 分组的 FIFO、观测窗口和探测状态。`Acquire` 只返回 1–16 MiB 或单 seek 窗口，取消通过 request context 和 `CancelTask` 双门完成。策略计算集中到纯函数：

```go
func nextLimit(current int, sample WindowSample, cfg PolicyConfig) int
func chooseTask(now time.Time, queues map[TaskIdentity]*taskQueue) TaskIdentity
```

- [ ] **Step 5: 运行 GREEN、race 和泄漏检查**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/diskio
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/diskio
```

- [ ] **Step 6: 提交 Task 2**

```powershell
git add -- internal/diskio/controller.go internal/diskio/controller_test.go internal/diskio/fake_clock_test.go
git commit -m "feat: schedule adaptive disk IO leases"
```

---

### Task 3: 扩展 Worker IPC 并接入 Agent broker

**Files:**
- Modify: `internal/worker/messages.go:53-70,158-315`
- Modify: `internal/worker/messages_test.go`
- Modify: `internal/worker/ipc_test.go`
- Modify: `internal/worker/pool.go`
- Modify: `internal/worker/supervisor.go:520-585`
- Modify: `internal/worker/pool_test.go`

**Protocol additions:**

```go
const (
	MsgIOLeaseAcquire = "io_lease_acquire"
	MsgIOLeaseGrant   = "io_lease_grant"
	MsgIOLeaseReport  = "io_lease_report"
	MsgIOLeaseCancel  = "io_lease_cancel"
	IPCCompatibilityVersion = 2
)

type IOLeaseAcquireMsg struct {
	JobID int64 `msgpack:"job_id"`
	RequestID uint64 `msgpack:"request_id"`
	TaskID string `msgpack:"task_id"`
	InstanceID string `msgpack:"instance_id"`
	DiskKey string `msgpack:"disk_key"`
	Class uint8 `msgpack:"class"`
	WantBytes int64 `msgpack:"want_bytes"`
	WantSeek bool `msgpack:"want_seek"`
}
```

`JobMsg` 同步增加 `ScanInstanceID` 与 `DiskKey`。`worker.Config` 注入 `IOBroker diskio.Controller`，不把 broker 序列化到子进程环境。

- [ ] **Step 1: 写四类消息 round-trip/边界 RED**

覆盖空 task/instance/disk、`WantBytes` 超 16 MiB、未知 class、grant 超请求、report generation 不匹配和 max-frame 拒绝；旧 IPC=1 Ready 必须被父进程 fail closed。

- [ ] **Step 2: 写 Supervisor 生命周期 RED**

fake Worker 发送 acquire，断言父进程用当前 `JobMsg` 的 task/instance/disk 覆盖不可信字段再调用 broker；Worker 退出调用 `ReclaimWorker`；任务取消返回 `MsgIOLeaseCancel`；旧实例 report 只触发回收。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/worker -run 'Test(IOLease|PoolIOLease|WorkerCompatibility)'
```

- [ ] **Step 4: 实现严格 broker 桥**

`readLoop` 新增 acquire/report 分支；acquire 使用 `worker.pool.ctx` 与当前 job 的取消 context，grant 写回前再次检查 `current` 指针与 job identity。任何消息校验失败都沿既有 protocol hard-fail；broker unavailable 不得下发无限制 grant。

- [ ] **Step 5: 运行 GREEN 与 Worker 全包**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/worker
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/worker
```

- [ ] **Step 6: 提交 Task 3**

```powershell
git add -- internal/worker/messages.go internal/worker/messages_test.go internal/worker/ipc_test.go internal/worker/pool.go internal/worker/supervisor.go internal/worker/pool_test.go
git commit -m "feat: broker worker disk IO leases"
```

---

### Task 4: 实现 Worker 本地租约窗口并覆盖 Go 源文件读取

**Files:**
- Create: `internal/wproc/io_lease.go`
- Create: `internal/wproc/io_lease_test.go`
- Modify: `internal/wproc/ipc.go`
- Modify: `internal/wproc/run.go:45-190`
- Modify: `internal/wproc/run_test.go`
- Modify: `internal/wproc/pipeline.go:150-185`
- Modify: `internal/wproc/pipeline_phase2.go:120-150`
- Modify: `internal/wproc/pipeline_session.go:260-295`
- Modify: `internal/wproc/image_preview.go:330-370`

**Worker API:**

```go
type IOLeaseClient interface {
	BeforeRead(ctx context.Context, want int) (leaseID uint64, granted int, err error)
	AfterRead(leaseID uint64, bytes int, elapsed time.Duration, err error)
	BeforeSeek(ctx context.Context) (leaseID uint64, err error)
	AfterSeek(leaseID uint64, elapsed time.Duration, err error)
}

type governedFile struct {
	file *os.File
	lease IOLeaseClient
}
```

- [ ] **Step 1: 写本地扣减、补充和取消 RED**

断言 4 MiB grant 支持多个 64 KiB read 而只跨 IPC 一次；余额不足才补充；每次 seek 单独申请；generation cancel 立即唤醒；pipe EOF 返回基础设施错误；read/seek 在没有 grant 时绝不调用底层文件 seam。

- [ ] **Step 2: 写生产管线读取覆盖 RED**

对 SHA、最终 rehash、Phase 1/2 源读取和 preview 源读取注入 recording file，断言每个底层 `Read` 前都有 grant；缓存 JPEG、临时 JPEG、日志读取不经过 source governor。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc -run 'Test(IOLeaseClient|GovernedSource|ServeIOLease)'
```

- [ ] **Step 4: 统一同步 IPC pump**

将只识别 SHA reply 的 `pumpSHAReply` 收敛为 job 内串行 `workerRPC`；它允许 SHA 和 lease 两种请求/回复，但拒绝任何乱序、重复 request ID 或意外消息。媒体处理仍每 Worker 一次只跑一个 job，不引入第二个并发 pipe reader。

- [ ] **Step 5: 包装所有 Go 源读取**

通过 `openSource(job) (io.ReadSeekCloser,error)` 注入 `governedFile`。完成后报告实际字节、耗时、错误类别；上下文取消优先返回 `context.Canceled`，不改写为媒体损坏。

- [ ] **Step 6: 运行 GREEN 与 race**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/wproc
```

- [ ] **Step 7: 提交 Task 4**

```powershell
git add -- internal/wproc/io_lease.go internal/wproc/io_lease_test.go internal/wproc/ipc.go internal/wproc/run.go internal/wproc/run_test.go internal/wproc/pipeline.go internal/wproc/pipeline_phase2.go internal/wproc/pipeline_session.go internal/wproc/image_preview.go
git commit -m "feat: govern worker source file reads"
```

---

### Task 5: 把租约 governor 接到 VideoCore read/seek 并排除等待超时

**Files:**
- Modify: `videocore/include/videocore/videocore.h`
- Modify: `videocore/src/deadline.h`
- Modify: `videocore/src/deadline.cpp`
- Modify: `videocore/src/win_file.h`
- Modify: `videocore/src/win_file.cpp`
- Modify: `videocore/src/avio_bridge.h`
- Modify: `videocore/src/avio_bridge.cpp`
- Modify: `videocore/src/media_session.h`
- Modify: `videocore/src/media_session.cpp`
- Modify: `videocore/src/api.cpp`
- Modify: `videocore/CMakeLists.txt`
- Create: `videocore/tests/test_win_file.cpp`
- Create: `videocore/tests/test_deadline.cpp`
- Modify: `internal/worker/messages.go:62-66`
- Modify: `internal/worker/messages_test.go`
- Create: `internal/wproc/videocore/io_governor.go`
- Create: `internal/wproc/videocore/io_governor_test.go`
- Modify: `internal/wproc/videocore/bindings.go`
- Modify: `internal/wproc/videocore/bindings_test.go`
- Modify: `internal/wproc/videocore/media.go`
- Modify: `internal/wproc/videocore/media_test.go`

**ABI v2:**

```c
#define VC_ABI_VERSION 2u
#define VC_VERSION_STRING "2.0.0"

typedef int32_t (VC_CALL *vc_io_acquire_fn)(
    uintptr_t context, uint32_t operation, uint64_t requested_bytes,
    uint64_t* lease_id, uint64_t* granted_bytes,
    vc_error* err);
typedef void (VC_CALL *vc_io_report_fn)(
    uintptr_t context, uint64_t lease_id, uint64_t actual_bytes,
    uint64_t elapsed_ns, int32_t status);

typedef struct vc_io_governor {
    uint32_t struct_size;
    uint32_t abi_version;
    uintptr_t context;
    vc_io_acquire_fn acquire;
    vc_io_report_fn report;
} vc_io_governor;
```

`vc_media_open_options` 尾部增加 `const vc_io_governor* io_governor`。Go 使用 `runtime/cgo.Handle` 的数值作为 `uintptr_t`，绝不把 Go 指针存入 C/C++。

- [ ] **Step 1: 写 native 授权次序与 deadline RED**

用 `VC_WIN_FILE_TESTING` hook 断言 acquire 在 `before_read/before_seek` 之前；拒绝 grant 时 `ReadFile/SetFilePointerEx` 调用数不变。假时钟让 acquire 等待 5 秒、实际 I/O 10 ms，1 秒 operation timeout 仍成功；实际 I/O 推进 2 秒则 `VC_ERR_TIMEOUT`。

- [ ] **Step 2: 写 Go handle 生命周期 RED**

覆盖 open 成功/失败、close、cancel、panic recovery 和重复 close；断言 handle 只释放一次，callback 返回的取消/基础设施错误不泄漏原始 pipe 文本。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' --build videocore\build --config Release --target test_vc_win_file test_vc_deadline
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' --test-dir videocore\build -C Release -R 'videocore_(win_file|deadline)' --output-on-failure
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc/videocore -run TestIOGovernor
```

- [ ] **Step 4: 实现 mutable deadline**

`Deadline` 增加 `Extend(std::chrono::nanoseconds)`；`WinFile::Read/Seek` 改接收 `Deadline*`，C++ 在 governor callback 前后用同一 steady clock 测量真实等待并延后同一个 operation deadline，再做 `CheckInterrupt` 和实际 I/O。AVIO opaque、hash 和分析路径传同一 mutable deadline，避免只延长局部副本，也不信任 callback 自报的等待值。

- [ ] **Step 5: 实现 governor 与报告**

每次 `ReadFile/SetFilePointerEx` 前 acquire，结束后 report 实际字节和真实 I/O 时间；缓存/临时文件 Open 时不设置 governor。ABI 结构大小、版本、空函数指针全部严格校验。同步把 Worker 的 `VideoCoreABIVersion` 改为 2、`VideoCoreVersion` 改为 `2.0.0`，旧 DLL/Worker Ready 必须 fail closed。

- [ ] **Step 6: 运行 GREEN、全部 native gate 与 CGO**

```powershell
& .\scripts\build.ps1 -VideoCoreOnly
& .\scripts\test-cgo.ps1 -Packages '.\internal\wproc\...' -DllDir '.\videocore\build\Release'
```

- [ ] **Step 7: 提交 Task 5**

```powershell
git add -- videocore/CMakeLists.txt videocore/include/videocore/videocore.h videocore/src/deadline.h videocore/src/deadline.cpp videocore/src/win_file.h videocore/src/win_file.cpp videocore/src/avio_bridge.h videocore/src/avio_bridge.cpp videocore/src/media_session.h videocore/src/media_session.cpp videocore/src/api.cpp videocore/tests/test_win_file.cpp videocore/tests/test_deadline.cpp internal/worker/messages.go internal/worker/messages_test.go internal/wproc/videocore/io_governor.go internal/wproc/videocore/io_governor_test.go internal/wproc/videocore/bindings.go internal/wproc/videocore/bindings_test.go internal/wproc/videocore/media.go internal/wproc/videocore/media_test.go
git commit -m "feat: govern VideoCore source IO"
```

---

### Task 6: 把扫描派发改为有界流水线并接通生命周期

**Files:**
- Modify: `internal/agent/scan.go:26-45,560-860,1160-1240`
- Modify: `internal/agent/scan_test.go`
- Create: `internal/agent/scan_io_drain_test.go`
- Modify: `internal/agent/limiter.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`

**Behavior:**

`DiskResolver` 改为返回 `diskio.Identity`。`processDiskBatch` 的固定 goroutine 数只控制有界提交，不再把 HDD 图片硬编码为 1；每个 `JobMsg` 携带 exact `taskID + instanceID + DiskKey`，真正 read 并发由 `diskio.Controller` 决定。

- [ ] **Step 1: 写 HDD 多任务在飞与有界队列 RED**

用 24 Worker fake pool 和 barrier 证明 HDD 图片阶段在首个结果返回前可以提交多个 job；队列数不超过 `MaxQueuedPerWorker*workerCount`；完成进度只在 Store durable result 后增加，已提交/等待 lease 不算完成。

- [ ] **Step 2: 写 pause/stop/delete/shutdown RED**

在等待 lease、已获小窗口、结果持久化三个边界分别触发 Drain/Abort。断言未派发项仍未完成，等待申请立即取消，当前窗口最多完成一次，stale replacement 结果不更新新实例，Shutdown 等待 broker/reporter 后再 `TaskDone`。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/agent ./cmd/agent -run 'Test(ScanBoundedPipeline|ScanLeaseDrain|AgentRunnerDiskIdentity)'
```

- [ ] **Step 4: 实现有界派发**

每块磁盘创建固定容量 `pendingJobs`，producer 只负责精确枚举顺序；dispatcher 在 Worker capacity 可用时提交，result writer 持久化后更新 Done。保留图片/其他/视频分类顺序，但删除 `images, 1` 的特殊并发值。`Controller.CancelTask` 在 state reason 设置后、停止新 dispatch 前调用。

- [ ] **Step 5: 接入 main**

Agent 启动时创建单个进程级 controller，并注入 ScanManager 与 Worker Pool；关闭顺序为停止新任务、取消租约、排空 Worker、停 controller。配置日志输出物理 key、初始/上限，不记录路径或 token。

- [ ] **Step 6: 运行 GREEN、Agent/Worker race**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/agent ./cmd/agent
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/agent ./internal/worker
```

- [ ] **Step 7: 提交 Task 6**

```powershell
git add -- internal/agent/scan.go internal/agent/scan_test.go internal/agent/scan_io_drain_test.go internal/agent/limiter.go cmd/agent/main.go cmd/agent/main_test.go
git commit -m "feat: pipeline adaptive scan dispatch"
```

---

### Task 7: 定义视频元数据契约并迁移 SQLite/PostgreSQL

**Files:**
- Create: `internal/proto/video_metadata.go`
- Create: `internal/proto/video_metadata_test.go`
- Modify: `internal/proto/message.go:52-65,240-280,366-430`
- Modify: `internal/proto/message_test.go`
- Modify: `internal/store/ddl.go:1-75`
- Modify: `internal/store/db.go:10-60`
- Create: `internal/store/video_metadata.go`
- Create: `internal/store/video_metadata_test.go`
- Modify: `internal/store/content.go`
- Modify: `internal/store/content_test.go`
- Modify: `internal/store/mask.go`
- Modify: `internal/store/mask_test.go`
- Modify: `internal/store/features.go`
- Modify: `internal/store/features_test.go`
- Modify: `internal/store/analysis.go:17-45,120-270`
- Modify: `internal/store/analysis_test.go`
- Modify: `internal/store/syncq.go`
- Modify: `internal/store/syncq_test.go`
- Modify: `internal/syncer/syncer.go`
- Modify: `internal/syncer/phase1_sync_test.go`
- Modify: `deploy/central.sql`

**Domain contract:**

```go
const FieldVideoMetadata uint32 = 1 << 10

type VideoContainerMetadata struct {
	FormatName, FormatLongName string
	StartTimeUS, DurationUS, BitRate, FileSize *int64
	ProbeScore *int32
	TagsJSON string
	PrimaryVideoStream *int32
	DecoderName string
}

type VideoStreamMetadata struct {
	Index int32
	MediaType string
	CodecID int32
	CodecName, CodecLongName, CodecTag string
	Profile string
	Level *int32
	TimeBase string
	StartTimeUS, DurationUS, BitRate, FrameCount *int64
	Disposition uint32
	Language, Title, TagsJSON string
	PixelFormat string
	BitDepth, Width, Height *int32
	SAR, DAR, AvgFrameRate, RealFrameRate string
	Rotation *int32
	ColorRange, ColorSpace, ColorTransfer, ColorPrimaries, ChromaLocation, FieldOrder string
	SampleFormat, ChannelLayout string
	SampleRate, Channels, AudioBitDepth *int32
}
```

- [ ] **Step 1: 写契约限制 RED**

测试 canonical tags JSON、最多 256 tracks、单 tags 64 KiB、总 metadata 1 MiB、stream index 唯一、合法 media type、N/A 用 nil。新增字段位后，所有 phase/mask round-trip 必须保持旧 bit 值不变。

- [ ] **Step 2: 写 SQLite v4 -> v5 RED**

真实 SQLite 建 v4 fixture，Open 后断言 `user_version=5`，旧 `video_features/video_frames` 字节不变，并存在 `video_containers`、`video_streams`。用 trigger 在第 N 个 stream 写入失败，断言 container、全部 streams、files mask、sync queue 全事务回滚。

表定义要包含外键和 NULL 语义：

```sql
CREATE TABLE video_containers (
  sha512 TEXT PRIMARY KEY,
  format_name TEXT NOT NULL,
  format_long_name TEXT,
  start_time_us INTEGER,
  duration_us INTEGER,
  bit_rate INTEGER,
  file_size INTEGER,
  probe_score INTEGER,
  tags_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(tags_json)),
  primary_video_stream INTEGER,
  decoder_name TEXT
);
CREATE TABLE video_streams (
  sha512 TEXT NOT NULL REFERENCES video_containers(sha512) ON DELETE CASCADE,
  stream_index INTEGER NOT NULL CHECK (stream_index >= 0),
  media_type TEXT NOT NULL CHECK (media_type IN ('video','audio','subtitle','data','attachment')),
  codec_id INTEGER NOT NULL,
  codec_name TEXT NOT NULL,
  codec_long_name TEXT,
  codec_tag TEXT,
  profile TEXT,
  level INTEGER,
  time_base TEXT,
  start_time_us INTEGER,
  duration_us INTEGER,
  bit_rate INTEGER,
  frame_count INTEGER,
  disposition INTEGER NOT NULL DEFAULT 0,
  language TEXT,
  title TEXT,
  tags_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(tags_json)),
  pixel_format TEXT,
  bit_depth INTEGER,
  width INTEGER,
  height INTEGER,
  sar TEXT,
  dar TEXT,
  avg_frame_rate TEXT,
  real_frame_rate TEXT,
  rotation INTEGER,
  color_range TEXT,
  color_space TEXT,
  color_transfer TEXT,
  color_primaries TEXT,
  chroma_location TEXT,
  field_order TEXT,
  sample_format TEXT,
  sample_rate INTEGER,
  channels INTEGER,
  channel_layout TEXT,
  audio_bit_depth INTEGER,
  PRIMARY KEY (sha512, stream_index)
);
```

- [ ] **Step 3: 写同步 round-trip RED**

`PendingSyncBatch` 增加两张表并保持公平轮转；loader 对同 SHA 返回一个 container 和完整有序 streams；PostgreSQL `ON CONFLICT` 事务中先 upsert container、再 exact replace streams；ack generation 精确匹配。Compute -> fake remote -> Manager 比较每一批准字段和 NULL。

- [ ] **Step 4: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/proto ./internal/store ./internal/syncer -run 'Test(VideoMetadata|SchemaV5|SyncVideoMetadata)'
```

- [ ] **Step 5: 实现 v5、原子保存和同步**

`SaveAnalysis` 在 `FieldsDone&FieldVideoMetadata != 0` 时调用 transaction-local `replaceVideoMetadata`；先 upsert container，再 delete+insert sorted streams，最后 enqueue 两张表。把 `FieldVideoMetadata` 加入视频 `contentFieldMask`、`phaseOneFieldsMask` 和 `RequiredStageOneMask`，`MissingPhase1/LookupContent` 只有在 container 存在且完整 stream 集合通过校验时才清该 bit。普通 SHA 复用由 missing mask 决定；force rescan 请求该 bit 并替换整集合。元数据成功而缩略图失败时只清缩略图 bit，metadata bit 保持完成。

- [ ] **Step 6: 运行 GREEN 与全 Store/Syncer**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/proto ./internal/store ./internal/syncer
```

- [ ] **Step 7: 提交 Task 7**

```powershell
git add -- internal/proto/video_metadata.go internal/proto/video_metadata_test.go internal/proto/message.go internal/proto/message_test.go internal/store/ddl.go internal/store/db.go internal/store/video_metadata.go internal/store/video_metadata_test.go internal/store/content.go internal/store/content_test.go internal/store/mask.go internal/store/mask_test.go internal/store/features.go internal/store/features_test.go internal/store/analysis.go internal/store/analysis_test.go internal/store/syncq.go internal/store/syncq_test.go internal/syncer/syncer.go internal/syncer/phase1_sync_test.go deploy/central.sql
git commit -m "feat: persist complete video metadata"
```

---

### Task 8: 从 VideoCore 的已打开容器提取全部轨道并回传

**Files:**
- Modify: `videocore/include/videocore/videocore.h`
- Modify: `videocore/src/media_session.h`
- Modify: `videocore/src/media_session.cpp`
- Modify: `videocore/src/video_analysis.h`
- Modify: `videocore/src/video_analysis.cpp`
- Modify: `videocore/src/api.cpp`
- Modify: `videocore/tests/test_media_session.cpp`
- Modify: `videocore/tests/test_video_analysis.cpp`
- Modify: `internal/wproc/videocore/bindings.go`
- Modify: `internal/wproc/videocore/bindings_test.go`
- Modify: `internal/wproc/videocore/media.go`
- Modify: `internal/wproc/videocore/media_test.go`
- Modify: `internal/worker/messages.go`
- Modify: `internal/worker/messages_test.go`
- Modify: `internal/worker/deduper.go`
- Modify: `internal/worker/pool.go`
- Modify: `internal/worker/pool_test.go`
- Modify: `internal/wproc/pipeline_session.go`
- Modify: `internal/wproc/pipeline_session_test.go`

**C ABI:**

```c
#define VC_MAX_STREAMS 256u
#define VC_CONTAINER_HAS_START_TIME       (1ull << 0)
#define VC_CONTAINER_HAS_DURATION         (1ull << 1)
#define VC_CONTAINER_HAS_BIT_RATE         (1ull << 2)
#define VC_CONTAINER_HAS_FILE_SIZE        (1ull << 3)
#define VC_CONTAINER_HAS_PROBE_SCORE      (1ull << 4)
#define VC_CONTAINER_HAS_PRIMARY_VIDEO    (1ull << 5)

typedef struct vc_video_container_info {
    uint32_t struct_size;
    uint32_t abi_version;
    uint64_t present_mask;
    int64_t start_time_us;
    int64_t duration_us;
    int64_t bit_rate;
    int64_t file_size;
    int32_t probe_score;
    int32_t primary_video_stream;
    char format_name_utf8[128];
    char format_long_name_utf8[256];
    char decoder_name_utf8[128];
} vc_video_container_info;

#define VC_STREAM_HAS_LEVEL               (1ull << 0)
#define VC_STREAM_HAS_START_TIME          (1ull << 1)
#define VC_STREAM_HAS_DURATION            (1ull << 2)
#define VC_STREAM_HAS_BIT_RATE            (1ull << 3)
#define VC_STREAM_HAS_FRAME_COUNT         (1ull << 4)
#define VC_STREAM_HAS_BIT_DEPTH           (1ull << 5)
#define VC_STREAM_HAS_WIDTH               (1ull << 6)
#define VC_STREAM_HAS_HEIGHT              (1ull << 7)
#define VC_STREAM_HAS_ROTATION            (1ull << 8)
#define VC_STREAM_HAS_SAMPLE_RATE         (1ull << 9)
#define VC_STREAM_HAS_CHANNELS            (1ull << 10)
#define VC_STREAM_HAS_AUDIO_BIT_DEPTH     (1ull << 11)

typedef struct vc_video_stream_info {
    uint32_t struct_size;
    uint32_t abi_version;
    uint64_t present_mask;
    uint32_t stream_index;
    uint32_t media_type;
    int32_t codec_id;
    int32_t level;
    int64_t start_time_us;
    int64_t duration_us;
    int64_t bit_rate;
    int64_t frame_count;
    uint32_t disposition;
    int32_t bit_depth;
    int32_t width;
    int32_t height;
    int32_t rotation;
    int32_t sample_rate;
    int32_t channels;
    int32_t audio_bit_depth;
    char codec_name_utf8[128];
    char codec_long_name_utf8[256];
    char codec_tag_utf8[32];
    char profile_utf8[128];
    char time_base_utf8[32];
    char language_utf8[64];
    char title_utf8[256];
    char pixel_format_utf8[64];
    char sar_utf8[32];
    char dar_utf8[32];
    char avg_frame_rate_utf8[32];
    char real_frame_rate_utf8[32];
    char color_range_utf8[32];
    char color_space_utf8[32];
    char color_transfer_utf8[32];
    char color_primaries_utf8[32];
    char chroma_location_utf8[32];
    char field_order_utf8[32];
    char sample_format_utf8[64];
    char channel_layout_utf8[128];
} vc_video_stream_info;

VC_API int32_t VC_CALL vc_media_container_info(vc_media_session*, vc_video_container_info*, vc_error*);
VC_API uint32_t VC_CALL vc_media_stream_count(vc_media_session*);
VC_API int32_t VC_CALL vc_media_stream_info(vc_media_session*, uint32_t ordinal, vc_video_stream_info*, vc_error*);
VC_API int32_t VC_CALL vc_media_metadata_json(vc_media_session*, int32_t stream_index, char* dst, uint32_t capacity, uint32_t* required, vc_error*);
```

- [ ] **Step 1: 写 native H.264/HEVC/多轨道 RED**

夹具至少含 video+audio+subtitle+attachment/data；断言 stream index 和 codec 来自 `codecpar`，主视频 stream 与实际 decoder 一致，容器/stream tags canonical，N/A flags 不伪造零。测试 hook 断言只创建一个 `AVFormatContext`、没有外部进程、没有第二次源文件遍历。

- [ ] **Step 2: 写协议恶意输入 RED**

`JobResultMsg` 和 `SHAReplyMsg` 增加 `VideoContainer *proto.VideoContainerMetadata` 与 `VideoStreams []proto.VideoStreamMetadata`；Worker 同步增加 `MaskVideoMetadata = 1<<10` 并纳入 `fieldMaskFull`，旧 bit 值保持不变。拒绝 257 tracks、重复 index、未知 media type、超总字节、metadata payload 存在但 bit 未完成；允许 metadata 成功加 contact-sheet field error。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' --build videocore\build --config Release --target test_vc_session test_vc_video_analysis
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' --test-dir videocore\build -C Release -R 'videocore_(session|video_analysis)' --output-on-failure
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/worker ./internal/wproc/videocore ./internal/wproc -run 'Test.*VideoMetadata'
```

- [ ] **Step 4: 在 probe 完成后冻结快照**

VideoCore 只从已打开且完成 stream info 的 `AVFormatContext` 拷贝固定字段。字符串用固定 UTF-8 容量和 required-length 两段式 API；tags 排序、长度限制后生成 JSON。`AV_NOPTS_VALUE`、0/0 rational 和不可用 codec 字段设置 present flag=false。

- [ ] **Step 5: 接入 Worker/Store**

Worker 请求 `FieldVideoMetadata` 时读取快照并回传；不因 RGB/某一帧失败丢弃 metadata。`Deduper.Ask` 从 `store.ContentState` 复用完整集合，in-flight singleflight 也深拷贝 metadata；`worker.Pool` 转换 `JobResultMsg -> store.AnalysisResult` 时深拷贝并执行 Task 7 的校验/事务，未提交 bit 的 metadata 在结果返回前清空。

- [ ] **Step 6: 运行 GREEN、native 18/18 与 Go 包**

```powershell
& .\scripts\build.ps1 -VideoCoreOnly
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/worker ./internal/wproc/videocore ./internal/wproc ./internal/agent ./internal/store
```

- [ ] **Step 7: 提交 Task 8**

```powershell
git add -- videocore/include/videocore/videocore.h videocore/src/media_session.h videocore/src/media_session.cpp videocore/src/video_analysis.h videocore/src/video_analysis.cpp videocore/src/api.cpp videocore/tests/test_media_session.cpp videocore/tests/test_video_analysis.cpp internal/wproc/videocore/bindings.go internal/wproc/videocore/bindings_test.go internal/wproc/videocore/media.go internal/wproc/videocore/media_test.go internal/worker/messages.go internal/worker/messages_test.go internal/worker/deduper.go internal/worker/pool.go internal/worker/pool_test.go internal/wproc/pipeline_session.go internal/wproc/pipeline_session_test.go
git commit -m "feat: extract all video stream metadata"
```

---

### Task 9: 生成受限 RGB24 联系表且保持灰度特征不变

**Files:**
- Create: `videocore/src/native_algorithms/rgb_image.h`
- Modify: `videocore/src/contact_sheet.h`
- Modify: `videocore/src/contact_sheet.cpp`
- Modify: `videocore/src/video_analysis.cpp:400-570,1420-1590`
- Modify: `videocore/tests/test_contact_sheet.cpp`
- Modify: `videocore/tests/test_video_analysis.cpp`
- Verify only: `videocore/tests/test_video_legacy_golden.ps1`

**Native model:**

```cpp
struct RgbImage {
    int32_t width = 0;
    int32_t height = 0;
    int32_t stride = 0;
    std::vector<uint8_t> pixels;
};

struct ContactSheetResult {
    videocore::native::GrayImage feature_canvas;
    videocore::native::RgbImage rgb_canvas;
    videocore::native::ImageFeatures features;
};
```

- [ ] **Step 1: 写彩色输出 RED**

构造红/绿/蓝/肤色块 fixture，解码选中真实 video stream 后断言 RGB 内存顺序；读取 JPEG SOF 断言 components=3，并解码抽样验证不是 `R==G==B`。固定断言 `TJPF_RGB/TJSAMP_420/quality=90`，占位 tile 是中性深色 RGB。

- [ ] **Step 2: 写内存与 golden RED**

测试 4K/8K frame 只留下 `tile_max_side` 以内 RGB tile，六帧总 RGB bytes 有硬上限；既有六帧 PDQ/pHash/Sobel 和联系表灰度 PDQ golden 字节不变；每 tile 转换前后都检查 cancel/deadline。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe' --build videocore\build --config Release --target test_vc_contact_sheet test_vc_video_analysis
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' --test-dir videocore\build -C Release -R 'videocore_(contact_sheet|video_analysis|video_legacy_golden)' --output-on-failure
```

- [ ] **Step 4: 从同一 AVFrame 产生灰度特征和 RGB tile**

保留现有 `FrameToGray` 的输出和调用顺序；新增 `FrameToRgbTile`，依据 frame 优先、stream fallback 的 range/colorspace/transfer/primaries 配置 `sws_setColorspaceDetails`，输出 `AV_PIX_FMT_RGB24`，完成旋转/SAR 后直接缩到 tile 上限。没有声明时使用 FFmpeg 已批准 fallback，不猜测虚假 metadata。

- [ ] **Step 5: 双画布合成与彩色 JPEG**

灰度 canvas 只计算既有特征；RGB canvas 只写用户可见 JPEG。预算计算按 `width*height*3` 和 `tjBufSize(...,TJSAMP_420)`，TurboJPEG 调用：

```cpp
tjCompress2(handle, rgb.pixels.data(), rgb.width, rgb.stride, rgb.height,
            TJPF_RGB, &jpeg, &jpeg_size, TJSAMP_420,
            90, TJFLAG_NOREALLOC);
```

- [ ] **Step 6: 运行 GREEN、18/18 与真实 H.264/HEVC fixture**

```powershell
& .\scripts\build.ps1 -VideoCoreOnly
```

Expected: VideoCore CTest 18/18；旧特征 golden 未更新，彩色 fixture 通过。

- [ ] **Step 7: 提交 Task 9**

```powershell
git add -- videocore/src/native_algorithms/rgb_image.h videocore/src/contact_sheet.h videocore/src/contact_sheet.cpp videocore/src/video_analysis.cpp videocore/tests/test_contact_sheet.cpp videocore/tests/test_video_analysis.cpp
git commit -m "feat: encode RGB video contact sheets"
```

---

### Task 10: 收敛为无旁车、无锁的纯 JPEG 缓存

**Files:**
- Modify: `internal/wproc/contact_sheet_cache.go`
- Modify: `internal/wproc/contact_sheet_cache_test.go`
- Delete: `internal/wproc/contact_sheet_lock_windows.go`
- Delete: `internal/wproc/contact_sheet_lock_other.go`
- Modify: `internal/wproc/contact_sheet_reparse_windows.go`
- Modify: `internal/wproc/contact_sheet_reparse_other.go`
- Modify: `internal/wproc/contact_sheet_reparse_windows_test.go`
- Modify: `internal/wproc/thumbcache.go`
- Modify: `internal/wproc/thumbcache_test.go`
- Modify: `internal/wproc/pipeline_session.go`
- Modify: `internal/wproc/pipeline_session_test.go`
- Modify: `internal/wproc/pipeline_video.go`
- Modify: `internal/wproc/pipeline_video_test.go`

**Final path:**

```go
func contactSheetFinalPath(root string, sha [64]byte) string {
	encoded := hex.EncodeToString(sha[:])
	return filepath.Join(root, encoded[:2], encoded+".jpg")
}
```

- [ ] **Step 1: 写目录闭包 RED**

常规生成、并发同 SHA、强制重算、损坏 JPEG、DB 缺指纹重算和取消场景结束后递归枚举 temp root：允许最终 `xx/<sha>.jpg` 和运行中的唯一 `.jpg.tmp-<pid>-<job>-<nonce>`；任何 `vc-grid-v1` 新目录、`.json`、`.lock`、半写 JPEG 都失败。测试只清自己创建的 temp，不触碰真实旧目录。

- [ ] **Step 2: 写并发原子发布 RED**

两个 Worker 对同 SHA 写不同但合法 RGB JPEG，barrier 控制交错；读取者只能看到旧完整文件或新完整文件，最终文件可解析、components=3、非零。Windows `MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH)` 失败时保留先前完整 JPEG并删除自己的 temp。

- [ ] **Step 3: 写复用/强制/修复 RED**

普通扫描复用可解析三分量 JPEG；灰度 JPEG、零字节、截断 JPEG 重新生成；force 永远覆盖同路径；JPEG 有效但 Store 缺 thumb PDQ/尺寸时只读 JPEG 重算，不打开源视频。启动只清理过期 `.jpg.tmp-*`，不删最终 JPEG。

- [ ] **Step 4: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc -run 'Test(ContactSheetCache|ThumbnailCache|SessionPipeline.*Contact)'
```

- [ ] **Step 5: 删除 sidecar/lock 生产依赖**

移除 `ContactSheetMeta`、JSON encode/decode、publish lock 和 `contactSheetPipeline` 版本目录。Agent 进程内现有 SHA singleflight 保留；跨 Worker 只依赖唯一 temp、fsync、三分量校验和原子 replace。`thumbcache.go` 与 session/video 两条生产可达路径统一调用同一个纯 JPEG helper，不能留下另一路生成 `.json`。

- [ ] **Step 6: 运行 GREEN、全 WProc 与 race**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/wproc
```

- [ ] **Step 7: 提交 Task 10**

```powershell
git add -- internal/wproc/contact_sheet_cache.go internal/wproc/contact_sheet_cache_test.go internal/wproc/contact_sheet_lock_windows.go internal/wproc/contact_sheet_lock_other.go internal/wproc/contact_sheet_reparse_windows.go internal/wproc/contact_sheet_reparse_other.go internal/wproc/contact_sheet_reparse_windows_test.go internal/wproc/thumbcache.go internal/wproc/thumbcache_test.go internal/wproc/pipeline_session.go internal/wproc/pipeline_session_test.go internal/wproc/pipeline_video.go internal/wproc/pipeline_video_test.go
git commit -m "feat: publish sidecar-free RGB thumbnails"
```

---

### Task 11: 把 I/O 指标送到任务快照和可展开详情

**Files:**
- Modify: `internal/proto/message.go:366-435`
- Modify: `internal/proto/local.go:130-155`
- Modify: `internal/proto/local_test.go`
- Modify: `cmd/agent/main.go:972-1020`
- Modify: `cmd/agent/main_test.go`
- Modify: `internal/nodetray/traymodel/model.go:156-196`
- Modify: `internal/nodetray/traymodel/model_test.go`
- Modify: `internal/nodetray/app/service.go:220-260,322-345`
- Modify: `internal/nodetray/app/service_test.go:299-330`
- Modify: `nodetray/frontend/wailsjs/go/models.ts`
- Modify: `nodetray/frontend/src/pages/LocalTaskItem.tsx`
- Modify: `nodetray/frontend/src/pages/LocalTaskItem.test.tsx`
- Modify: `nodetray/frontend/src/app.css`

**Stats v2:**

```go
const LocalTaskDisplayStatsVersion = 2

type TaskIOStats struct {
	DiskConcurrency int `json:"disk_concurrency" msgpack:"disk_concurrency"`
	EffectiveReadBPS float64 `json:"effective_read_bps" msgpack:"effective_read_bps"`
	LeaseWaitMS int64 `json:"lease_wait_ms" msgpack:"lease_wait_ms"`
	SequentialBytes int64 `json:"sequential_bytes" msgpack:"sequential_bytes"`
	SeekCount int64 `json:"seek_count" msgpack:"seek_count"`
	BusyWorkers int `json:"busy_workers" msgpack:"busy_workers"`
	IOWaitWorkers int `json:"io_wait_workers" msgpack:"io_wait_workers"`
}
```

- [ ] **Step 1: 写 stats v1 -> v2 与单调合并 RED**

解码 v1 时保留原 speed/failures/duration，新 I/O 字段为零；v2 progress/final 使用 max/累计规则避免迟到快照回退；replacement instance 仍由既有 revision/generation 门阻挡。display failures 继续只用 `Failed`，不能重新叠加 `ScanErrors`。

- [ ] **Step 2: 写任务卡详情 RED**

任务卡主行仍完整显示阶段、进度、速度、失败摘要和耗时；`<details>` 展开后显示磁盘并发、有效读取速度、租约等待、顺序字节、seek、忙 Worker/I/O 等待 Worker。窄屏换行不截断；0/未知显示 `—`，不显示 `NaN/Infinity`。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/proto ./cmd/agent ./internal/nodetray/app -run 'Test.*LocalTask.*IO'
npm --prefix nodetray/frontend test -- --run src/pages/LocalTaskItem.test.tsx
```

- [ ] **Step 4: 接通 Snapshot -> StatsJSON -> Wails DTO**

Scan progress loop 从 controller 读取 exact task/instance snapshot，填入 `TaskProgress.IO`；runner 的 display stats 只接受同实例、不低 revision 的更新。NodeTray 后端映射为数值 DTO，前端负责单位格式化。更新 `models.ts` 后验证生成物无仅含 tab 的空行。

- [ ] **Step 5: 实现详情布局**

主行不因详情指标扩列；详情使用响应式 grid 和 `overflow-wrap:anywhere`。按钮锁、确认、polling、删除 exact revision 逻辑不变。

- [ ] **Step 6: 运行 GREEN、全前端与 build**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/proto ./cmd/agent ./internal/nodetray/app
npm --prefix nodetray/frontend test -- --run
npm --prefix nodetray/frontend run lint
npm --prefix nodetray/frontend run build
```

构建后只恢复本轮生成的 `nodetray/frontend/dist` 差异，不覆盖用户改动。

- [ ] **Step 7: 提交 Task 11**

```powershell
git add -- internal/proto/message.go internal/proto/local.go internal/proto/local_test.go cmd/agent/main.go cmd/agent/main_test.go internal/nodetray/traymodel/model.go internal/nodetray/traymodel/model_test.go internal/nodetray/app/service.go internal/nodetray/app/service_test.go nodetray/frontend/wailsjs/go/models.ts nodetray/frontend/src/pages/LocalTaskItem.tsx nodetray/frontend/src/pages/LocalTaskItem.test.tsx nodetray/frontend/src/app.css
git commit -m "feat: show local task disk IO details"
```

---

### Task 12: 完成跨层正确性、性能基准和发布闭包

**Files:**
- Create: `scripts/benchmark-scan-io.ps1`
- Create: `scripts/benchmark-scan-io.test.ps1`
- Modify: `scripts/build.ps1`（仅当 ABI/schema gate 需要显式检查时）
- Modify: `scripts/package-node-release.ps1`（仅当 manifest 缺少新 ABI/schema 字段时）
- Modify: `scripts/package-manager-release.ps1`（仅当 manifest 缺少新 schema 字段时）
- Create: `docs/superpowers/reports/2026-08-17-adaptive-disk-io-rgb-video-metadata-validation.md`

- [ ] **Step 1: 写 benchmark 脚本契约 RED**

Pester/纯 PowerShell 测试断言脚本要求：固定输入 `I:\MiddleDir\11111111`、相同字段掩码和 Worker 数；记录 build SHA、配置、每盘轨迹、任务开始/结束、结果集合摘要；D 盘空间不足立即 BLOCKED；不删除数据/旧 cache；基线和自适应任一失败都不得输出性能 PASS。

- [ ] **Step 2: 运行全量静态/单元/race gate**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./...
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/diskio ./internal/agent ./internal/worker ./internal/wproc
npm --prefix nodetray/frontend test -- --run
npm --prefix nodetray/frontend run lint
npm --prefix nodetray/frontend run build
& .\scripts\build.ps1 -VideoCoreOnly
```

Expected: Go、race、前端、VideoCore CTest 18/18 全部 PASS；若 native 工具链或权限失败，记录环境阻断，不能称为功能 PASS。

- [ ] **Step 3: 运行真实媒体正确性抽检**

用 fresh DLL/Worker 对 I 盘至少抽检 H.264、HEVC 和一个多轨道容器：缩略图为三分量彩色 JPEG；DB 主视频 stream/decoder 与 FFmpeg 实际选择一致；全部轨道行数/索引一致；缓存目录没有新 `.json/.lock/vc-grid-v1`。对只读源文件记录 SHA，测试不得修改源媒体。

- [ ] **Step 4: 跑固定基线与自适应全量扫描**

```powershell
& .\scripts\benchmark-scan-io.ps1 -Root 'I:\MiddleDir\11111111' -Mode Baseline -OutputDir '.\artifacts\benchmarks\io-baseline'
& .\scripts\benchmark-scan-io.ps1 -Root 'I:\MiddleDir\11111111' -Mode Adaptive -OutputDir '.\artifacts\benchmarks\io-adaptive'
```

比较完整文件集合、SHA、图片特征、六帧特征和失败集合，必须完全一致；自适应墙钟不得回退超过 3%，目标至少缩短 20%。`<20%` 但未回退只标“正确性 PASS、性能目标未达成”，不调阈值伪造结果。

- [ ] **Step 5: 验证生命周期与资源波动**

基准中途各执行一次暂停/恢复和停止，确认等待 lease 取消、in-flight 小窗口排空、进度不超前。记录 CPU/磁盘波动但不据此失败；以墙钟和结果一致性判定。任务详情需显示 controller 轨迹对应的并发/速度/等待。

- [ ] **Step 6: fresh 全产品 build/package**

```powershell
$stage = '.\artifacts\stage\adaptive-disk-io-rgb-video-metadata-20260817'
$revision = (git rev-parse HEAD).Trim()
& .\scripts\build.ps1 -Go 'C:\tmp\go1.26.5\go\bin\go.exe' -StageDir $stage
& .\scripts\package-node-release.ps1 `
  -StageDir $stage -OutputDir '.\artifacts\releases' `
  -ReleaseId '20260817-adaptive-disk-io-rgb-video-metadata' `
  -BuildDate '2026-08-17' -SourceRevision $revision
& .\scripts\package-manager-release.ps1 `
  -StageDir $stage -OutputDir '.\artifacts\releases' `
  -ReleaseId '20260817-adaptive-disk-io-rgb-video-metadata' `
  -BuildDate '2026-08-17' -SourceRevision $revision
```

独立展开 ZIP 并验证 Agent/Worker/VideoCore ABI=2、SQLite schema=5、依赖闭包、manifest、SHA-256；部署只允许目录 move 保留现有 `data` 与 `local-control.token` ACL，不复制/重建 token。没有用户再次明确授权时只产包，不替换现场目录、不启动真实任务。

- [ ] **Step 7: 写中文验收报告**

报告必须分别列出：代码/单元、native、race、前端、真实媒体、全量性能、包内容、部署/GUI 运行边界。任何未执行项写 `PARTIAL/BLOCKED`，附命令、exit code、artifact 路径和 SHA-256。

- [ ] **Step 8: 提交 Task 12**

```powershell
git add -- scripts/benchmark-scan-io.ps1 scripts/benchmark-scan-io.test.ps1 scripts/build.ps1 scripts/package-node-release.ps1 scripts/package-manager-release.ps1 docs/superpowers/reports/2026-08-17-adaptive-disk-io-rgb-video-metadata-validation.md
git diff --cached -- scripts/build.ps1 scripts/package-node-release.ps1 scripts/package-manager-release.ps1
git commit -m "test: validate adaptive media scan throughput"
```

若 build/package 脚本未实际修改，不得把它们加入提交。ZIP、stage、benchmark 原始产物不提交 Git。

---

## 最终完成定义

- [ ] 固定基线与自适应扫描结果集合、SHA、特征和失败集合一致。
- [ ] 自适应版本墙钟回退不超过 3%，并记录是否达到至少 20% 缩短目标。
- [ ] 同物理磁盘共享预算，多任务公平、取消、崩溃和 stale 身份均有确定性回归。
- [ ] 每个生产源 read/seek 都在有效租约内；缓存/日志/DB 不进入源盘调度。
- [ ] 视频 JPEG 为 RGB 三分量，既有灰度特征 golden 未变化。
- [ ] 新 `thumbcache` 只产生分片 `.jpg` 和短暂唯一 temp，不产生版本目录、`.json`、`.lock`。
- [ ] 旧 `thumbcache\vc-grid-v1` 未读取、未迁移、未删除。
- [ ] SQLite/PostgreSQL 保存容器及全部轨道，NULL/限制/原子替换/同步均通过。
- [ ] 任务卡主信息完整，I/O 指标在详情中可读，迟到快照不能回退当前实例。
- [ ] Go、race、前端、VideoCore CTest、真实媒体、包闭包均有 fresh 证据；未运行的运行时边界明确标 `PARTIAL/BLOCKED`。
