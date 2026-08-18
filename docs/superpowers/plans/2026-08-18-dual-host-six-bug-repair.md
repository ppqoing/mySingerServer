# 双主机六项缺陷修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复六项 `DH-*` 缺陷，并在正式构建和相同双主机真实媒体目录上完成复测。

**Architecture:** Worker 默认统一使用 VideoCore Session 管线；本地分析把 Roots 传至候选源；删除在 Helper 前写 SQLite journal；Manager 从 PostgreSQL 恢复有界历史并读取实时 Pool 状态。所有改动按缺陷逐项执行 RED-GREEN，避免无关防御代码和重构。

**Tech Stack:** Go 1.26.5、Windows cgo、VideoCore C ABI、SQLite、PostgreSQL 16、PowerShell、SSH。

**Spec:** `docs/superpowers/specs/2026-08-18-dual-host-six-bug-repair-design.md`

## Global Constraints

- 真实媒体 `I:\MiddleDir\11111111` 和 `D:\tmp\-------2-4` 只读，删除只使用隔离副本。
- 正式 Worker 参数保持 `CGO_ENABLED=1`、MinGW GCC、`-tags nodynamic`，不增加 `legacy_mediacore`。
- 当前主检出有用户未提交改动；仅编辑列出的文件与代码块，不清理、不重置、不宽泛暂存。
- `cmd/agent/main.go`、`internal/localanalysis/engine.go`、`internal/gui/tasks.go`、`internal/gui/httpapi.go` 已有用户改动，实施期间不提交这些混合文件；每项任务用聚焦 diff 和测试输出留证。
- 实现优先复用现有状态、表和接口，不增加无关校验层、兼容分支或大范围重构。
- Go 命令使用 `C:\tmp\go1.26.5\go\bin\go.exe`，`GOCACHE` 使用仓库外临时目录。

---

### Task 1: 阶段一图片走 VideoCore Session 管线

**Files:**
- Modify: `internal/wproc/run_test.go`
- Modify: `internal/wproc/run.go`

**Interfaces:**
- Consumes: `serve(net.Conn, int, Config, pipelineDeps) int`、`processMediaWithDeps`。
- Produces: Session 模式下阶段一图片不再调用旧 `processImageWithDeps`。

- [ ] **Step 1: 把旧路由测试改成失败回归**

将 `TestImageNoThumbnailServeUsesImagePipelineEvenWithSessionConfigured` 改为 `TestServeDispatchesPhase1ImageThroughSessionPipeline`。构造 `worker.Phase1` 图片任务和 `sessionPipelineFake`，通过 IPC 回答 SHA query，断言 `sessionFake.opens == 1`、`hashes == 1`、`rehashes == 1`、`closes == 1`，并断言旧 `decodeCalls == 0`。

- [ ] **Step 2: 运行 RED**

Run:

```powershell
$env:GOCACHE='C:\tmp\mysingerserver-six-bug-go-cache'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/wproc -run TestServeDispatchesPhase1ImageThroughSessionPipeline -count=1
```

Expected: FAIL，旧路由使 `decodeCalls=1` 且 `sessionFake.opens=0`。

- [ ] **Step 3: 删除生产特殊分流**

在 `internal/wproc/run.go` 删除：

```go
} else if useSessionPipeline && job.Phase == worker.Phase1 && job.Kind == worker.MediaImage {
    result, err = processImageWithDeps(cfg, &job, deps)
```

保留后续统一 `else if useSessionPipeline` 分支，使合法阶段一图片调用 `processMediaWithDeps`。

- [ ] **Step 4: 运行 GREEN 和包回归**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/wproc -run 'TestServeDispatchesPhase1ImageThroughSessionPipeline|TestServeDispatchesPhase2ThroughSessionPipeline|TestServeDispatchesImagePreview' -count=1
```

Expected: PASS。

- [ ] **Step 5: 检查聚焦 diff**

```powershell
git diff --check -- internal/wproc/run.go internal/wproc/run_test.go
git status --short -- internal/wproc/run.go internal/wproc/run_test.go
```

Expected: 仅两个文件包含本任务改动，staged 仍为空。

### Task 2: 修复 contact-sheet 临时路径 cgo 所有权

**Files:**
- Modify: `internal/wproc/videocore/bindings.go`
- Create: `internal/wproc/videocore/bindings_cgo_windows_test.go`

**Interfaces:**
- Consumes: `utf16Path(string) ([]uint16, error)`、`cgoBridge.analyze`。
- Produces: `allocUTF16Path([]uint16) (*C.uint16_t, func(), error)`，调用期路径完全位于 C 内存。

- [ ] **Step 1: 写 Windows+cgo 失败测试**

新测试使用非空 Unicode 路径分配 native buffer，读取每个 UTF-16 code unit 与源切片比较，并在释放前确认指针非空。测试随后通过真实 `cgoBridge.analyze` 的现有 VideoCore 测试入口运行带 `TempJPEGPath` 的请求，原实现应触发 cgo 指针检查。

- [ ] **Step 2: 运行 RED**

使用与正式 Worker 相同的 `CGO_ENABLED=1`、`CC`、`-tags nodynamic` 和已构建 VideoCore import library 运行目标测试。Expected: FAIL 于 `Go pointer to unpinned Go pointer` 或缺少 C 分配函数；不能接受因测试装配错误失败。

- [ ] **Step 3: 最小实现 C 分配与释放**

在 cgo preamble 增加 `#include <stdlib.h>`。`analyze` 中将 Go slice 替换为：

```go
temporaryUnits, err := utf16Path(request.TempJPEGPath)
temporaryPath, releasePath, err := allocUTF16Path(temporaryUnits)
if err != nil { return AnalysisResult{}, err }
defer releasePath()
nativeRequest.temporary_jpeg_path = temporaryPath
nativeRequest.temporary_jpeg_path_units = C.uint32_t(len(temporaryUnits))
```

`allocUTF16Path` 仅负责 `C.malloc`、`C.memcpy` 和 `C.free`；空输入返回 nil 指针和空释放函数。删除 `runtime.KeepAlive(temporaryPath)`，保留 `runtime` 包给其他既有调用使用时才继续导入。

- [ ] **Step 4: 运行 GREEN 和真实 MP4 直达测试**

Expected: Unicode 路径测试 PASS；对隔离 MP4 设置非空 `TempJPEGPath` 后分析返回而不 panic，输出 JPEG 存在且 Worker 仍响应下一任务。

- [ ] **Step 5: 运行 VideoCore 包回归**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/wproc/videocore -count=1
```

Expected: PASS；若 DLL 装配缺失，记录环境失败并改用正式构建阶段验证，不能把未运行标为 PASS。

### Task 3: Roots 端到端绑定本地分析

**Files:**
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`
- Modify: `internal/localanalysis/engine.go`
- Modify: `internal/localanalysis/engine_test.go`
- Modify: `internal/localanalysis/stage1.go`
- Modify: `internal/localanalysis/stage1_test.go`
- Create: `internal/localanalysis/root_source.go`
- Create: `internal/localanalysis/root_source_test.go`

**Interfaces:**
- Produces: `RunWithProgressForRoots(context.Context, string, []string, func(int) error) error`。
- Produces: `StageOne.RunForRoots(context.Context, string, string, []string) (firstscreen.Result, error)`。

- [ ] **Step 1: 恢复 Roots 行为测试并运行 RED**

从已验证提交 `2c5cdecf`、`3b4ad25d`、`85bde546` 读取测试合同，手工适配当前文件。覆盖大小写、目录边界、跨盘、空值、相对路径、`..`、盘符根，以及 `request.Roots` 复制。运行：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/localanalysis ./cmd/agent -run 'Roots|ScanThenAnalysis' -count=1
```

Expected: FAIL，因为当前接口只接受 TaskID。

- [ ] **Step 2: 实现 Roots 候选源**

新增 `rootScopedCandidateSource`，用 `filepath.Clean`、`filepath.IsAbs`、`filepath.VolumeName` 和 `filepath.Rel` 过滤 `StreamActiveFiles`。只保留一套 `validateTaskRoots`，不在多个层重复实现路径算法。

- [ ] **Step 3: 接通 Engine、StageOne 与 Agent**

`localAnalysisRunner` 改为 `RunWithProgressForRoots`；`agentLocalTaskRunner.Run` 调用前执行 `roots := append([]string(nil), request.Roots...)`。Engine 在 `BeginLocalAnalysis` 之前验证 Roots，再把作用域源传给 StageOne。

- [ ] **Step 4: 运行 GREEN、普通与 race 回归**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/localanalysis ./cmd/agent -count=1
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race ./internal/localanalysis ./cmd/agent -run 'Roots|ScanThenAnalysis' -count=1
```

Expected: PASS。

### Task 4: Helper 前持久化删除 journal

**Files:**
- Modify: `internal/localdelete/service.go`
- Modify: `internal/localdelete/service_test.go`
- Modify: `internal/store/files.go`
- Modify: `internal/store/local_review.go`
- Modify: `internal/store/firstscreen.go`
- Modify: `internal/store/local_review_test.go`
- Modify: `internal/store/firstscreen_test.go`

**Interfaces:**
- Produces: `BeginDeletionBatch(context.Context, string, CommittedDeletion, string) error`。
- Changes: `CommitDeletionResults` 只完成已存在的 batch/items，不再首次插入 journal。

- [ ] **Step 1: 写 service 顺序失败测试**

给 `fakeDeleteStore` 增加 `begun bool`、`beginErr error`、`commitErr error`。Helper 检查 `begun`；断言 begin 失败时 Helper 调用为 0，正常时调用顺序为 begin → helper → commit，commit 失败时 begin 仍为 true。

- [ ] **Step 2: 写 SQLite RED 测试**

新增测试：`BeginDeletionBatch` 后 batch=`running`、item=`pending`；给完成事务注入 outbox trigger 失败后，batch/item 仍保持原 journal；成功完成后 item=`deleted`、file=`deleted`、outbox 有一条事件。

- [ ] **Step 3: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/localdelete ./internal/store -run 'DeletionBatch|DeleteExecutePersistsIntent' -count=1
```

Expected: FAIL，因为 Store 没有 begin 方法且当前 Helper 先执行。

- [ ] **Step 4: 实现两个简洁事务边界**

`BeginDeletionBatch` 复用当前 `CommitDeletionResults` 中对 current analysis、review 和文件快照的校验，然后插入 batch 和 pending items。`CommitDeletionResults` 加载既有 pending items，逐项核对 `file_id/path/sha` 后更新结果；只对确定成功项更新 files 和 outbox。

`localdelete.Execute` 在 Helper 前调用 begin：

```go
if err := service.store.BeginDeletionBatch(ctx, prepared.batchID, current, prepared.digest); err != nil {
    return DeleteBatch{}, err
}
reports, helperErr := service.helper.Execute(ctx, task)
```

- [ ] **Step 5: 隔离未收口项目**

在 `StreamActiveFiles` 和 `LoadCommittedDeletion` 的文件查询加入同一 `NOT EXISTS` 条件，排除 `local_delete_items.result IN ('pending','uncertain')`。不添加新文件状态或后台恢复器。

- [ ] **Step 6: 运行 GREEN、完整包回归与 race**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/localdelete ./internal/store -count=1
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race ./internal/localdelete -count=1
```

Expected: PASS。

### Task 5: 恢复活动任务和最近 200 条终态历史

**Files:**
- Modify: `internal/gui/tasks.go`
- Modify: `internal/gui/postgres_integration_test.go`

**Interfaces:**
- Consumes: `scan_tasks.target`、`scan_tasks.stats_json`。
- Produces: Restore 后 `/api/tasks` 含全部活动扫描任务和最近 200 条终态扫描任务。

- [ ] **Step 1: 写 PostgreSQL RED 测试**

插入活动 scan、终态 scan、`target.type=phase2` 三类记录；终态写入字面量 `stats_json`。断言 Restore 后 List 包含活动和终态统计、排除 phase2，PendingScans 只包含活动。再插入 201 条终态，断言只恢复最新 200 条。

- [ ] **Step 2: 运行 RED**

```powershell
$env:DEDUP_TEST_PG_DSN='postgres://dedup:dedup@127.0.0.1:5432/dedup?sslmode=disable'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/gui -run 'TestTaskRegistryRestores.*IntegrationEnabled' -count=1
```

Expected: FAIL，当前终态不恢复。

- [ ] **Step 3: 用 CTE 恢复两类任务**

查询结构：

```sql
WITH active AS (
  SELECT ... FROM scan_tasks WHERE status IN ('sent','acked','running') AND scan target
), terminal AS (
  SELECT ... FROM scan_tasks WHERE status IN ('done','failed') AND scan target
  ORDER BY updated_at DESC,id DESC LIMIT 200
)
SELECT ... FROM active UNION ALL SELECT ... FROM terminal;
```

同时读取 `stats_json`，非空时 `json.Unmarshal` 为 `proto.TaskStats` 并调用 `applyTaskStats`。不改变 `List` 和 `PendingScans` 的职责。

- [ ] **Step 4: 运行 GREEN**

运行目标 PostgreSQL 测试和 `go test ./internal/gui -count=1`。Expected: PASS。

### Task 6: runtime/status 使用 Pool 实时状态

**Files:**
- Modify: `internal/gui/runtime_host.go`
- Modify: `internal/gui/runtime_host_test.go`

**Interfaces:**
- Consumes: `API.pool.Status()`。
- Produces: 单次请求固定 API 快照；有 Pool 时 runtime status 与 agents 共享状态来源。

- [ ] **Step 1: 写 RED 测试**

创建 Pool 和一个 AgentConn 测试状态，将其置为 claimed/online，安装到 RuntimeHost；断言 `/api/runtime/status` 返回同一 machine ID、`online=true`、`identity_state=claimed`。保留现有数据库不可用离线快照测试。

- [ ] **Step 2: 运行 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/gui -run 'TestRuntimeHost.*Agent' -count=1
```

Expected: FAIL，当前返回构造期 pending/offline。

- [ ] **Step 3: 传入请求级 API 快照**

把 `ServeHTTP` 已读取的 `api` 传给 `handleRuntimeStatus`。复制 RuntimeHost 状态后，仅在 `api != nil && api.pool != nil` 时用 `api.pool.Status()` 替换 Agents；不新增缓存或观察者。

- [ ] **Step 4: 运行 GREEN 和 RuntimeHost 回归**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test ./internal/gui -run 'TestRuntimeHost' -count=1
```

Expected: PASS。

### Task 7: 聚焦回归、正式构建和本机黑盒

**Files:**
- Verify: `scripts/build.ps1`
- Verify: `internal/wproc`、`cmd/agent`、`internal/localanalysis`、`internal/localdelete`、`internal/store`、`internal/gui`

- [ ] **Step 1: 格式化并检查白名单 diff**

对本计划修改的 Go 文件运行 `gofmt -w`，然后运行 `git diff --check`。确认 staged 为空，确认没有真实媒体文件进入 Git 状态。

- [ ] **Step 2: 运行六包回归**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc ./cmd/agent ./internal/localanalysis ./internal/localdelete ./internal/store ./internal/gui
```

- [ ] **Step 3: 运行可执行的全仓回归**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./...
```

记录既有失败与本轮回归；不得用聚焦 PASS 替代全仓结果。

- [ ] **Step 4: 正式构建**

按项目标准依赖路径运行 `scripts/build.ps1`。记录 Worker、Agent、GUI、VideoCore 的路径和 SHA-256；若 VideoCore 正式门禁失败，状态记为 PARTIAL 并继续对已生成的可验证运行件做非发布验收。

- [ ] **Step 5: 本机黑盒**

用隔离图片、MP4 和删除副本验证阶段一、contact sheet、Roots、预览、删除 journal 与重启任务历史。删除故障注入完成后核对 SQLite 和文件系统。

### Task 8: 双主机真实媒体复测并更新 Bug 文档

**Files:**
- Modify: `docs/details/2026-08-14-bug-investigation.md`

- [ ] **Step 1: 部署同哈希运行件**

本机和 SSH 别名 `codex-192-168-1-6` 使用相同 Worker/Agent/VideoCore。只替换测试运行目录中的运行件，不覆盖远程用户 Everything、配置、数据或日志。

- [ ] **Step 2: 扫描两个真实根**

本机根 `I:\MiddleDir\11111111`，远程根 `D:\tmp\-------2-4`。运行到图片和视频均有非零成功样本后取消并等待终态。

- [ ] **Step 3: 核对动态门槛**

检查无 MediaCore stub 错误、无 cgo panic、无系统性 Worker 重生；记录 files_done、files_failed、decode_calls、crashes，以及 PostgreSQL 中有效 SHA、image_features、video_features 数量。

- [ ] **Step 4: 安全验收分析、预览和删除**

真实根只做 Roots 隔离分析和预览；删除仅对隔离副本执行，验证 journal 先于物理删除以及成功收口。

- [ ] **Step 5: 更新 Bug 文档**

在最新双主机章节追加修复结果表，按 P0/P1/P2 写明每项 `PASS`、`PARTIAL` 或 `BLOCKED`、测试命令、任务 ID、哈希、计数和残余边界。

- [ ] **Step 6: 最终验证**

重新运行 `git diff --check`、聚焦回归和文档路径检查，确认未修改真实媒体，确认 staged 中没有用户文件。
