# Compute 扫描吞吐、进度与停止 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 取消跨磁盘全局字节限流，修复默认 Worker 图片处理链，并为本地任务补齐可持久化进度和可真正停止扫描的按钮。

**Architecture:** Agent 扫描以调用方 `context.Context` 贯穿枚举、盘级队列和 Worker 提交边界，每块物理磁盘只受自己的流数约束；已经进入 Worker 的单项允许安全完成并落库。扫描进度通过现有 Agent 消息进入本地任务服务并持久化，NodeTray 对活跃任务轮询并调用现有 `local.task.cancel` 操作。

**Tech Stack:** Go 1.26、SQLite、MessagePack、React 19、TypeScript、Vitest、Wails 2、PowerShell 7、VideoCore C ABI。

## Global Constraints

- NodeTray 管理 Agent，Agent 管理 Worker；NodeTray 不直接启动、停止或强杀 Worker。
- 运行时不再按文件大小获取全局字节额度；并发仅由 HDD/SSD 每盘流数、Worker 数量和既有队列容量控制。
- `tuning.pending_bytes_mb` 继续允许旧配置解析，但不再参与调度；新默认配置不输出该字段。
- 停止任务保留已提交数据库结果、缓存和任务记录，最终状态为 `cancelled`，并允许后续重试。
- 已经进入 Worker 的单项允许正常完成或由既有超时机制结束；停止后不再提交新文件。
- 图片和视频的默认生产处理路径统一使用 VideoCore，不向 Compute 包重新加入 `mediacore.dll`。
- 更新 `D:\code\mySingerServer\publish\MySingerServer-Compute` 时只替换清单内静态文件，保留 `data`、SQLite WAL/SHM、日志、令牌、配置和缓存。
- 未执行的 GUI、真实磁盘、UAC、服务或长时间运行验收必须标记为 `PARTIAL` 或 `BLOCKED`。

## 文件结构与责任边界

| 文件 | 责任 |
|---|---|
| `internal/agent/scan.go`、`limiter.go` | 上下文传播、盘级派发、扫描观测，不再按文件字节限流 |
| `internal/worker/pool.go` | 可取消的 Worker 队列提交 |
| `internal/wproc/run.go` | 默认图片和视频统一分派至 VideoCore session pipeline |
| `internal/store/analysis.go` | 保存 SHA-512 生成前的合法终止错误 |
| `internal/config/agent.go`、`cmd/agent/thumb_cache*.go` | 旧配置兼容和缓存根启动初始化 |
| `internal/proto/message.go`、`internal/localtask/service.go`、`cmd/agent/main.go` | 扫描消息到本地任务进度的持久化闭环 |
| `internal/nodetray/traymodel/model.go`、`internal/nodetray/app/service.go`、`nodetray/app.go` | NodeTray 取消任务后端调用链 |
| `nodetray/frontend/src/api/localAgent.ts`、`pages/LocalTasksPage.tsx` | 任务轮询、进度条、阶段和停止按钮 |

---

### Task 1: 删除全局字节限流并让扫描可取消

**Files:**
- Modify: `internal/agent/scan.go`
- Modify: `internal/agent/scan_test.go`
- Modify: `internal/agent/limiter.go`
- Modify: `internal/agent/limiter_test.go`
- Modify: `internal/worker/pool.go`
- Modify: `internal/worker/pool_test.go`

**Interfaces:**
- Consumes: `ScanObserver`、`WorkerPool.Submit(*worker.JobMsg) error`、`HDDStreams/SSDStreams`。
- Produces: `(*ScanManager).PrepareContext(context.Context, proto.ScanTask, Sender) (proto.TaskAck, func())`、`(*worker.Pool).SubmitContext(context.Context, *worker.JobMsg) error`。

- [ ] **Step 1: 写失败测试**

在 Worker Pool 测试中填满 `jobs`，取消传给 `SubmitContext` 的上下文并断言返回 `context.Canceled`。在 Agent 测试中增加：

```go
func TestScanCancellationStopsNewDiskSubmissionsButDrainsStartedWork(t *testing.T)
func TestOversizedFileOnOneDiskDoesNotBlockAnotherDisk(t *testing.T)
```

第二个测试以两个 `diskNo`、每盘一个流和可控 Hasher 运行；阻塞磁盘 0 的超大文件后，必须观察到磁盘 2 已经开始处理。

- [ ] **Step 2: 运行测试确认失败**

```powershell
go test -p=1 -count=1 ./internal/worker ./internal/agent -run 'Test(PoolSubmitContext|ScanCancellation|OversizedFile)'
```

Expected: FAIL，缺少新接口或磁盘 2 仍被全局额度阻塞。

- [ ] **Step 3: 实现 Worker 可取消提交**

在 `internal/worker/pool.go` 实现：

```go
func (p *Pool) SubmitContext(ctx context.Context, job *JobMsg) error
```

`Submit` 调用 `SubmitContext(context.Background(), job)` 保持兼容。队列满时等待必须能被 `ctx.Done()` 和 Pool 关闭唤醒，且不能持有会阻止关闭的锁。

- [ ] **Step 4: 删除字节信号量并保留观测器**

从 `ScanManager` 删除 `limiter`；从 `limiter.go` 删除 `byteLimiter/newByteLimiter` 和 weighted semaphore。将观测包装收敛为：

```go
func runObservedWork(observer ScanObserver, diskNo, bytes int64, work func() (time.Duration, time.Duration)) {
    started := time.Now()
    if observer != nil { observer.Begin(diskNo, bytes) }
    read, decode := work()
    if observer != nil { observer.End(diskNo, bytes, time.Since(started), read, decode) }
}
```

删除证明“大文件占满容量”的测试，保留 Begin/End 成对记录测试。

- [ ] **Step 5: 贯穿扫描上下文**

保留远端兼容入口：

```go
func (m *ScanManager) Prepare(task proto.ScanTask, sender Sender) (proto.TaskAck, func()) {
    return m.PrepareContext(context.Background(), task, sender)
}
```

新增 `PrepareContext` 并将上下文保存到 `ScanState`。枚举回调、待处理查询、分盘 jobs 投递和 Worker 队列等待均检查同一上下文。已成功提交 Worker 或已开始普通哈希的项继续完成；result writer 排空这些结果并落库后才结束。

- [ ] **Step 6: 运行定向测试并提交**

```powershell
go test -p=1 -count=1 ./internal/worker ./internal/agent -run 'Test(PoolSubmitContext|ScanCancellation|OversizedFile|RunObservedWork)'
git add -- internal/agent/scan.go internal/agent/scan_test.go internal/agent/limiter.go internal/agent/limiter_test.go internal/worker/pool.go internal/worker/pool_test.go
git commit -m "fix: cancel scans without cross-disk byte limits"
```

Expected: PASS；提交只包含列出的六个文件。

---

### Task 2: 修复默认 Worker 图片链与 SHA 前置错误

**Files:**
- Modify: `internal/wproc/run.go`
- Modify: `internal/wproc/run_test.go`
- Modify: `internal/store/analysis.go`
- Modify: `internal/store/analysis_test.go`

**Interfaces:**
- Consumes: `processMediaWithDeps`、`AnalysisResult.Errors`。
- Produces: 默认 Phase1 Image 使用 session pipeline；`validAnalysisPreSHAFailure(AnalysisResult) bool`。

- [ ] **Step 1: 写分派与持久化失败测试**

`run_test.go` 用 session fake 执行 `Phase1 + MediaImage`，断言 session open/hash/analyze 被调用且 legacy `pipelineDeps.open` 未调用。

`analysis_test.go` 枚举图片后调用：

```go
_, err := db.SaveAnalysis(ctx, AnalysisResult{
    MachineID: "m", Path: path, Kind: MediaImage, Size: 10, MTime: 20,
    RequestedFields: proto.FieldSHA512 | proto.FieldPDQ256,
    Errors: []FieldError{{Field: proto.FieldSHA512 | proto.FieldPDQ256, Stage: "open", Msg: "sharing violation"}},
})
```

断言无错误、`files.sha512 IS NULL`、文件失败原因仍为原始 open 错误。带非 SHA 成功载荷的空 SHA 结果仍必须拒绝。

- [ ] **Step 2: 运行测试确认失败**

```powershell
go test -p=1 -count=1 ./internal/wproc ./internal/store -run 'Test(DefaultSessionPipelineRoutesPhase1Image|SaveAnalysisPreSHA)'
```

Expected: FAIL，图片进入旧路径且 `SaveAnalysis` 报 SHA 长度错误。

- [ ] **Step 3: 统一默认分派**

删除 `useSessionPipeline && Phase1 && MediaImage` 的特殊分支。session pipeline 启用时，Phase1/Phase2 图片和视频在阶段、类型校验后统一调用 `processMediaWithDeps`；显式 legacy 测试注入路径保持可用。

- [ ] **Step 4: 保存严格白名单内的前置失败**

`SaveAnalysis` 在访问 SHA 键表前按以下规则分支：

```go
switch {
case len(result.SHA512) == 64:
    sha, err = encodeSHA512(result.SHA512)
case validAnalysisPreSHAFailure(result):
    // 只更新 files.status/error/missing_mask，不写 SHA 或特征表。
default:
    return CommittedState{}, fmt.Errorf("store: SHA-512 must be exactly 64 bytes, got %d", len(result.SHA512))
}
```

白名单要求没有成功字段和媒体载荷、至少一个错误、每个错误覆盖 SHA 字段，且阶段仅为 `stat/open/read/hash/stale`。

- [ ] **Step 5: 运行测试并提交**

```powershell
go test -p=1 -count=1 ./internal/wproc ./internal/store -run 'Test(DefaultSessionPipelineRoutesPhase1Image|SaveAnalysisPreSHA|SaveAnalysisRejects)'
git add -- internal/wproc/run.go internal/wproc/run_test.go internal/store/analysis.go internal/store/analysis_test.go
git commit -m "fix: route phase one images through videocore"
```

Expected: PASS；Compute 构建仍不依赖 `mediacore.dll`。

---

### Task 3: 初始化安全缓存根并废弃新配置中的字节额度

**Files:**
- Create: `cmd/agent/thumb_cache.go`
- Create: `cmd/agent/thumb_cache_windows.go`
- Create: `cmd/agent/thumb_cache_other.go`
- Create: `cmd/agent/thumb_cache_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`
- Modify: `internal/config/agent.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: 绝对化后的 `cfg.Thumb.CacheDir`。
- Produces: `prepareThumbCacheRoot(string) error`；兼容解析但不默认输出的 `PendingBytesMB`。

- [ ] **Step 1: 写失败测试**

覆盖不存在的多级目录被创建、普通文件路径被拒绝、symlink 被拒绝、Windows `FILE_ATTRIBUTE_REPARSE_POINT` 被拒绝。配置测试断言旧值 1024 仍能 Load/Validate，而 `json.Marshal(DefaultAgent())` 不包含 `pending_bytes_mb`。

- [ ] **Step 2: 运行测试确认失败**

```powershell
go test -p=1 -count=1 ./cmd/agent ./internal/config -run 'Test(PrepareThumbCache|AgentConfigPendingBytes)'
```

Expected: FAIL，目录准备函数不存在且默认 JSON 仍输出字段。

- [ ] **Step 3: 实现缓存根准备并接入启动**

`prepareThumbCacheRoot` 使用 `filepath.Abs/Clean`、`os.MkdirAll`、`os.Lstat`，要求最终对象为普通目录且不是 symlink。Windows helper 使用 `windows.GetFileAttributes` 拒绝重解析点，非 Windows helper 返回 nil。

`runWithDependencies` 在 `store.Open` 和 `worker.NewPool` 之前执行：

```go
if err := prepareThumbCacheRoot(cfg.Thumb.CacheDir); err != nil {
    return fmt.Errorf("prepare thumb cache root: %w", err)
}
```

- [ ] **Step 4: 退出默认配置和调度校验**

字段标签改为 `json:"pending_bytes_mb,omitempty"`，`DefaultAgent()` 设为 0；校验允许 0 或兼容范围 `1..16384`。扫描代码不得读取该字段。

- [ ] **Step 5: 运行测试并提交**

```powershell
go test -p=1 -count=1 ./cmd/agent ./internal/config -run 'Test(PrepareThumbCache|AgentConfigPendingBytes|AgentDefaults)'
git add -- cmd/agent/thumb_cache.go cmd/agent/thumb_cache_windows.go cmd/agent/thumb_cache_other.go cmd/agent/thumb_cache_test.go cmd/agent/main.go cmd/agent/main_test.go internal/config/agent.go internal/config/config_test.go
git commit -m "fix: prepare compute thumbnail cache root"
```

Expected: PASS；旧 `agent.json` 无需修改即可启动。

---

### Task 4: 持久化扫描进度并让取消等待真实收尾

**Files:**
- Modify: `internal/proto/message.go`
- Modify: `internal/proto/message_test.go`
- Modify: `internal/agent/scan.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`
- Modify: `internal/localtask/service.go`
- Modify: `internal/localtask/service_test.go`

**Interfaces:**
- Consumes: Task 1 的 `PrepareContext`、`MsgTaskProgress/MsgTaskDone`。
- Produces: `localtask.ProgressUpdate`、`func(ProgressUpdate) error`、持久化 progress/stats。

- [ ] **Step 1: 定义进度契约并写失败测试**

定义：

```go
type ProgressUpdate struct {
    Stage int
    HasProgress bool
    ProgressComplete int64
    ProgressTotal int64
    StatsJSON string
}
type TaskRunner interface {
    Run(context.Context, CreateRequest, int, func(ProgressUpdate) error) error
}
```

`HasProgress=true` 表示本次携带完整进度快照；`HasProgress=false` 只推进阶段并保留数据库中的进度和统计。fake runner 报告扫描进度和阶段后，断言任务表保存最后完成数、总数、阶段和 JSON。另用可控 runner 在收到取消后写最后进度再返回，断言 Cancel 等待、最终状态 `cancelled`、进度保留且 Retry 可用。

- [ ] **Step 2: 运行测试确认失败**

```powershell
go test -p=1 -count=1 ./internal/localtask ./cmd/agent -run 'Test(LocalTaskProgress|CancelWaitsForScanCleanup|LocalTaskRunnerForwardsProgress)'
```

Expected: FAIL，当前 runner 回调只有 stage，扫描消息也未持久化。

- [ ] **Step 3: 扩充进度消息**

为 `proto.TaskProgress` 追加：

```go
Failed    int64 `msgpack:"failed,omitempty"`
ElapsedMS int64 `msgpack:"elapsed_ms,omitempty"`
```

`progressLoop` 和 total 即时消息填充 Done/Total/Failed/ElapsedMS/Speed；TaskDone 保持最终统计权威。

- [ ] **Step 4: 连接 Agent runner 与持久化回调**

`localScanRunner` 使用 `PrepareContext`。`agentLocalTaskRunner.Run` 将 progress 消息转成 `ProgressUpdate{Stage:0, HasProgress:true}`，把速度、失败数、时长编码为现有 NodeTray JSON 字段；TaskDone 先报告最终统计再结束 terminal。分析 checkpoint 使用 `ProgressUpdate{Stage:stage, HasProgress:false}`，由 service 保留当前数值。

上下文取消后等待 ScanManager terminal，再返回 `context.Canceled`，确保已开始的工作完成并落库。

- [ ] **Step 5: 调整 taskService 取消时序**

`taskService.run` 的报告回调从最新 `current` 补齐未改变字段并调用 `TransitionLocalTask`。活跃任务 Cancel 只触发 `attempt.cancel()` 并等待 attempt 完成，不提前设 superseded 或伪造终态；run 负责写 `cancelled/task_cancelled`。无活跃尝试的 pending/recovery 任务仍可直接 store cancel，重复取消幂等。

- [ ] **Step 6: 运行测试并提交**

```powershell
go test -p=1 -count=1 ./internal/proto ./internal/localtask ./cmd/agent ./internal/agent -run 'Test(TaskProgress|LocalTaskProgress|Cancel|LocalTaskRunner)'
git add -- internal/proto/message.go internal/proto/message_test.go internal/agent/scan.go cmd/agent/main.go cmd/agent/main_test.go internal/localtask/service.go internal/localtask/service_test.go
git commit -m "feat: persist local scan progress and cancellation"
```

Expected: PASS；刷新后进度可从 SQLite 恢复。

---

### Task 5: 暴露 NodeTray 停止任务 API

**Files:**
- Modify: `internal/nodetray/traymodel/model.go`
- Modify: `internal/nodetray/traymodel/model_test.go`
- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`
- Modify: `nodetray/app.go`
- Modify: `nodetray/app_test.go`
- Modify: `nodetray/frontend/src/api/localAgent.ts`
- Modify: `nodetray/frontend/src/api/localAgent.test.ts`

**Interfaces:**
- Consumes: `LocalOperationTaskCancel`、`proto.LocalTaskIDRequest`。
- Produces: Wails `CancelLocalTask(taskId string)`、前端 `cancelLocalTask(taskId: string)`。

- [ ] **Step 1: 写三层失败测试**

Service 测试断言取消调用使用 `local.task.cancel` 且 payload TaskID 正确；远端错误只返回安全摘要。Wails Backend 测试断言未启动返回 `backend_not_started`，启动后转发。TypeScript 测试断言传入原始 task ID，后端缺失返回 `backend_unavailable`。

- [ ] **Step 2: 运行测试确认失败**

```powershell
go test -p=1 -count=1 ./internal/nodetray/... ./nodetray -run 'Test.*CancelLocalTask'
npm --prefix nodetray/frontend test -- --run src/api/localAgent.test.ts
```

Expected: FAIL，取消方法尚未暴露。

- [ ] **Step 3: 实现 DTO、服务和 Wails 方法**

```go
type LocalTaskIDRequest struct { TaskID string `json:"taskId"` }
```

`Service.CancelLocalTask` 调用 `localCall(ctx, proto.LocalOperationTaskCancel, proto.LocalTaskIDRequest{TaskID: request.TaskID}, &proto.LocalTaskCancelResponse{})`，成功返回 `OperationResult{OK:true}`。Backend 接收字符串并构造 DTO；前端调用 `call('CancelLocalTask', fallback, taskId)`。

- [ ] **Step 4: 运行测试并提交**

```powershell
go test -p=1 -count=1 ./internal/nodetray/... ./nodetray -run 'Test.*CancelLocalTask'
npm --prefix nodetray/frontend test -- --run src/api/localAgent.test.ts
git add -- internal/nodetray/traymodel/model.go internal/nodetray/traymodel/model_test.go internal/nodetray/app/service.go internal/nodetray/app/service_test.go nodetray/app.go nodetray/app_test.go nodetray/frontend/src/api/localAgent.ts nodetray/frontend/src/api/localAgent.test.ts
git commit -m "feat: expose local task cancellation in nodetray"
```

Expected: PASS。

---

### Task 6: 添加任务进度条、轮询和停止按钮

**Files:**
- Modify: `nodetray/frontend/src/api/localAgent.ts`
- Modify: `nodetray/frontend/src/pages/LocalTasksPage.tsx`
- Modify: `nodetray/frontend/src/pages/LocalTasksPage.test.tsx`
- Modify: `nodetray/frontend/src/app.css`

**Interfaces:**
- Consumes: `cancelLocalTask`、`progressComplete/progressTotal/speed/failures/duration`。
- Produces: 活跃任务 1 秒轮询、阶段化进度和停止中状态。

- [ ] **Step 1: 写进度、轮询和停止失败测试**

使用 fake timers 覆盖：total 为 0 时无 value 的不确定 `<progress>`；total 大于 0 时 value/max 和 `40/100`；活跃状态每 1000 ms 再 list；全部终态后停止；卸载清除 timer；创建成功立即刷新。

停止测试覆盖：running 显示按钮；点击只调用 cancel 一次；Promise 未完成时禁用并显示“正在停止”；成功后刷新；终态不显示按钮；失败后恢复并显示安全摘要。

- [ ] **Step 2: 运行测试确认失败**

```powershell
npm --prefix nodetray/frontend test -- --run src/pages/LocalTasksPage.test.tsx
```

Expected: FAIL，当前页面只加载一次任务 ID 和状态。

- [ ] **Step 3: 完整化类型与依赖**

```ts
export type LocalTask = {
  taskId: string; source: string; mode: string; stage: number; status: string
  progressComplete: number; progressTotal: number
  speed?: string; failures?: number; duration?: string; syncStatus?: string
  errorCode?: string; errorSummary?: string
}
```

`LocalTasksAPI` 增加 `cancel(taskId)`，默认绑定 Task 5 的 API。

- [ ] **Step 4: 实现任务卡片和轮询**

活跃集合为 `pending/running/waiting_recovery`。阶段标签为 0“枚举与扫描”、1“扫描完成”、2“二筛”、3“三筛完成”。单个 effect 完成首次 load 和后续 timeout；每次响应按最新 tasks 决定是否安排下一次，cleanup 忽略过期 Promise 并清除 timer。

卡片显示状态、阶段、progress、完成数、失败数、速度、时长。`progressTotal <= 0` 不传 value。

- [ ] **Step 5: 实现停止中状态和样式**

用 `Set<string>` 保存 stopping IDs；请求完成前禁用按钮，成功或失败后移除并刷新，重复点击无第二个请求。在 `app.css` 增加 `.local-task-card`、`__meta`、`__progress`、`__actions`，只复用现有变量和按钮风格。

- [ ] **Step 6: 运行测试、构建并提交**

```powershell
npm --prefix nodetray/frontend test -- --run src/pages/LocalTasksPage.test.tsx src/api/localAgent.test.ts
npm --prefix nodetray/frontend run build
git add -- nodetray/frontend/src/api/localAgent.ts nodetray/frontend/src/pages/LocalTasksPage.tsx nodetray/frontend/src/pages/LocalTasksPage.test.tsx nodetray/frontend/src/app.css
git commit -m "feat: show and stop local task progress"
```

Expected: PASS，TypeScript 构建无错误。

---

### Task 7: 回归、构建、发布与运行验收

**Files:**
- Verify: 全部修改文件
- Generate: `artifacts/compute-scan-progress-stage/`
- Generate: `publish/MySingerServer-compute-win-x64-20260814-compute-scan-progress.zip`
- Update static files only: `publish/MySingerServer-Compute/`

**Interfaces:**
- Consumes: Tasks 1–6 的完整任务闭环。
- Produces: 测试证据、Compute ZIP/sidecar、目标运行目录静态更新和 SHA-256。

- [ ] **Step 1: 格式化并运行相关全量测试**

```powershell
gofmt -w internal/agent/scan.go internal/agent/scan_test.go internal/agent/limiter.go internal/agent/limiter_test.go internal/worker/pool.go internal/worker/pool_test.go internal/wproc/run.go internal/wproc/run_test.go internal/store/analysis.go internal/store/analysis_test.go internal/config/agent.go internal/config/config_test.go cmd/agent/main.go cmd/agent/main_test.go cmd/agent/thumb_cache.go cmd/agent/thumb_cache_windows.go cmd/agent/thumb_cache_other.go cmd/agent/thumb_cache_test.go internal/proto/message.go internal/proto/message_test.go internal/localtask/service.go internal/localtask/service_test.go internal/nodetray/traymodel/model.go internal/nodetray/traymodel/model_test.go internal/nodetray/app/service.go internal/nodetray/app/service_test.go nodetray/app.go nodetray/app_test.go
go test -p=1 -count=1 ./internal/agent ./internal/worker ./internal/wproc ./internal/store ./internal/config ./internal/proto ./internal/localtask ./internal/nodetray/... ./cmd/agent ./nodetray
npm --prefix nodetray/frontend test
npm --prefix nodetray/frontend run build
```

Expected: 所有命令退出码 0。

- [ ] **Step 2: 运行仓库全量 Go 回归**

```powershell
go test -p=1 -count=1 ./...
```

Expected: PASS。外部 PostgreSQL、GUI、UAC 或设备依赖无法提供时记录具体错误并标为 `BLOCKED`。

- [ ] **Step 3: 构建 stage 并验证不含 legacy DLL**

```powershell
& '.\scripts\build.ps1' -StageDir '.\artifacts\compute-scan-progress-stage'
```

Expected: stage 包含 agent/worker/nodetray/videocore/FFmpeg/manifest，不含 `mediacore.dll`。

- [ ] **Step 4: 验证并生成 Compute 包**

```powershell
& '.\scripts\test-package-node-release.ps1'
& '.\scripts\test-package-portable-release.ps1'
$revision = git rev-parse HEAD
& '.\scripts\package-node-release.ps1' -StageDir '.\artifacts\compute-scan-progress-stage' -OutputDir '.\publish' -ReleaseId '20260814-compute-scan-progress' -BuildDate '2026-08-14' -SourceRevision $revision
```

Expected: ZIP、sidecar 和包内 manifest 通过，包内无数据库、日志、令牌和 `mediacore.dll`。

- [ ] **Step 5: 白名单更新目标运行目录**

停止 Compute 并确认 agent/worker/nodetray/helper 无进程。解压 ZIP 到临时候选目录，只复制包内静态项，明确排除：

```text
data/**
*.db
*.db-wal
*.db-shm
logs/**
thumbcache/**
*.token
agent.json
helper.json
tray.json
```

复制前后记录目标 data 文件数、数据库大小和配置 SHA-256，并断言不变。不得删除目标目录后整体替换。

- [ ] **Step 6: 运行本地任务验收**

启动目标 `Start-Compute.ps1`，用磁盘 0 与磁盘 2 的测试根创建任务，验证页面进度、两盘独立推进、停止后无新派发、最终取消、已完成结果保留和重试。日志不得再出现旧版 mediacore 不可用、空 SHA 二次错误或缓存根不存在。

若当前权限不能读取 Windows 性能计数器，用 Agent 盘级 active/pending 与完成数作为并行证据，并将“性能监视器 100% 活跃时间”标为 `BLOCKED`。

- [ ] **Step 7: 记录产物和工作区状态**

```powershell
Get-FileHash -Algorithm SHA256 '.\publish\MySingerServer-compute-win-x64-20260814-compute-scan-progress.zip'
git status --short
git log --oneline -8
```

报告绝对路径、SHA-256、源提交、自动化测试、运行验收和所有 `PARTIAL/BLOCKED` 边界；不提交 artifacts、publish 运行数据或用户既有未跟踪文件。
