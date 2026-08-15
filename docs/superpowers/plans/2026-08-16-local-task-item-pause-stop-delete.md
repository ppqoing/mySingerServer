# 本地任务 Item 暂停停止删除 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 创建任务后立即在创建区下方显示紧凑任务 item，实时呈现业务阶段与阶段内进度，并提供真正的暂停、恢复、停止和删除操作。

**Architecture:** 以 SQLite 中的 `local_tasks` 作为唯一事实来源；Agent 通过带 `instance_id + revision` 的控制命令接受操作，任务服务负责串行化控制、驱动 Runner 排空并持久化稳定态。扫描与本地分析只上报业务阶段进度，不直接决定生命周期；NodeTray 使用单链自适应轮询合并快照，并以版本化命令避免旧请求覆盖新状态。删除严格限制在任务记录与其本地分析派生数据，保留全局文件索引、特征、缓存和中央数据。

**Tech Stack:** Go 1.23、modernc SQLite、MessagePack 本地协议、Wails v2.12、React 18、TypeScript、Vitest、Testing Library。

## Global Constraints

- 本计划的规范来源是 `docs/superpowers/specs/2026-08-16-local-task-item-pause-stop-delete-design.md`；若实现细节与该规格冲突，以规格为准并先修订规格。
- 当前检出包含用户未提交改动，且多个目标文件已有任务进度/取消相关修改。开始实现前必须使用 `superpowers:using-git-worktrees`，逐文件审阅 `git diff -- <精确路径>`，保留并整合现有改动；禁止 reset、checkout 覆盖、广泛清理或 `git add -A`。
- 本计划取代 `docs/superpowers/plans/2026-08-14-compute-scan-throughput-progress-cancel.md` 中尚未集成的本地任务进度/取消 UI 与生命周期部分；旧计划的吞吐、图片特征、缓存和发布内容不自动进入本次范围。
- 新增控制请求必须携带 `task_id`、`instance_id`、`expected_revision`。旧版 `cancel/retry` 载荷只作滚动兼容；一旦该 `task_id` 存在删除回执，旧载荷必须返回 `task_instance_required`，不得作用于重建后的实例。
- 暂停是“停止新枚举/派发 → 等待在途文件或配对安全落库 → `paused`”，恢复从文件/配对级持久检查点继续；不实现字节偏移续传，也不暂停 Worker 进程或操作系统线程。
- 删除运行中任务必须走 `deleting`，复用同一排空机制，再在单一 SQLite 事务中删除任务专属数据；不得删除全局文件索引、特征、缩略图/缓存、源文件、中央数据或已同步中央记录。
- `revision` 只在新实例、新运行尝试和生命周期/控制状态变化时递增；普通进度上报不递增。所有写操作同时校验 `machine_id + task_id + instance_id + revision`。
- UI 只允许一个 `setTimeout` 轮询链：存在活动/过渡态时 1 秒，全部稳定或出错时 5 秒；创建与控制响应合并后必须使旧请求失效，卸载后不得继续写状态。
- 对用户只返回安全错误码与安全摘要，数据库路径、媒体路径、SQL、堆栈和原始内部错误不得进入 Wails DTO。
- 每个任务只暂存该任务列出的文件。提交前运行 `git diff --cached --name-only`，若出现任务清单外文件则停止并修正暂存区。
- 当前会话已验证 Go 位于 `C:\tmp\go1.26.5\go\bin`，所以命令使用该绝对路径；若执行时路径已变化，先只读定位现有 Go，再替换命令路径，不修改系统 PATH、不在项目内安装工具。
- 不增加 Web/Manager/中央任务控制、WebSocket/Wails 事件推送、批量控制、优先级/新调度器、任务参数编辑、字节偏移恢复，也不顺带重构吞吐、缓存或其他非生命周期代码。
- GUI、Agent 重启、真实磁盘扫描和真实 SQLite 删除链路若未执行，只能报告 `PARTIAL` 或 `BLOCKED`，不能写成通过；未经单独授权不部署、不发布。

## 文件与职责映射

| Layer | Files | Responsibility |
|---|---|---|
| SQLite schema/migration | `internal/store/ddl.go`, `internal/store/db.go` | v4 schema、旧库安全重建、删除回执表、外键校验 |
| Task persistence | `internal/store/local_tasks.go`, new `internal/store/local_task_delete.go` | 版本化生命周期、阶段进度、恢复规则、删除事务 |
| Local wire protocol | `internal/proto/local.go`, `internal/proto/message.go` | 新操作、版本化控制 DTO、阶段快照、排空原因 |
| Task orchestration | `internal/localtask/service.go`, new `internal/localtask/control.go` | 控制门、运行尝试、异步排空、删除重试、启动恢复 |
| Production runner | `internal/agent/scan.go`, `cmd/agent/main.go`, `internal/localanalysis/engine.go` | 停止新工作、等待在途工作、上报 scan/stage1/2/3/finalizing |
| Agent local handler | `internal/agent/local_handler.go` | 鉴权后路由、兼容旧 cancel/retry、安全错误映射 |
| NodeTray Go bridge | `internal/nodetray/traymodel/model.go`, `internal/nodetray/app/service.go`, `nodetray/app.go` | 安全 DTO、Wails 方法、Agent 调用映射 |
| Frontend model/item | new `nodetray/frontend/src/pages/localTaskLifecycle.ts`, new `LocalTaskItem.tsx` | 状态/阶段文案、按钮矩阵、紧凑行、窄窗换行 |
| Frontend orchestration | `nodetray/frontend/src/pages/LocalTasksPage.tsx`, `nodetray/frontend/src/api/localAgent.ts` | 创建后插入、单链轮询、控制确认、竞态隔离 |
| Generated bindings | `nodetray/frontend/wailsjs/go/main/Backend.d.ts`, `Backend.js`, `nodetray/frontend/wailsjs/go/models.ts` | 与 Go/Wails 公共方法和 DTO 保持一致 |

## Task 1: 增加 v4 任务快照结构与迁移

**Files:**

- Modify: `internal/store/ddl.go`
- Modify: `internal/store/db.go`
- Modify: `internal/store/local_tasks.go`
- Modify: `internal/store/local_tasks_test.go`

**Produces:**

```go
type LocalTask struct {
	TaskID            string
	InstanceID        string
	Revision          int64
	MachineID         string
	Source            string
	Type              string
	Stage             int
	Status            string
	Phase             string
	EnvelopeDigest    string
	Envelope          []byte
	ProgressComplete  int64
	ProgressTotal     int64
	ProgressTotalKnown bool
	StatsJSON         string
	SafeErrorCode     *string
	SafeErrorMessage  *string
	CreatedAt         int64
	UpdatedAt         int64
	StartedAt         *int64
	CompletedAt       *int64
}
```

新库与迁移后的 `local_tasks` 表必须且只能接受以下状态：

```text
pending running waiting_recovery pausing paused stopping cancelled
succeeded failed deleting delete_failed
```

业务阶段为：

```text
waiting scan stage1 stage2 stage3 finalizing
```

- [ ] 新增会失败的新库结构与迁移测试。

创建包含一个运行中任务及其依赖行的旧版 v3 数据库夹具，通过 `store.Open` 打开后断言：

- schema 版本为 4；
- 旧记录获得非空 `instance_id`、`revision = 1`，并按 status/stage 映射最近 phase（`pending+stage0 => waiting`、其他 `stage0 => scan`、`stage1 => stage1`、`stage2 => stage3`、`stage3 => finalizing`）；
- 原进度、envelope、时间戳和全部外键依赖行都被保留；
- 所有新状态均可写入，未知状态被拒绝；
- `local_task_deletion_receipts` exists with primary key `(machine_id, task_id, instance_id)`;
- `PRAGMA foreign_key_check` returns no rows.
- 注入迁移失败时 `PRAGMA user_version` 仍为 3，修复失败点后再次打开可以完成迁移。

```go
func TestLocalTaskV3MigrationAddsVersionedLifecycleWithoutLosingDependents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-v3.db")
	seedLocalTaskV3Database(t, dbPath)
	db := openTestDBAt(t, dbPath)
	task, err := db.LoadLocalTask(context.Background(), "machine-1", "task-1")
	if err != nil { t.Fatal(err) }
	if task.InstanceID == "" { t.Fatal("missing migrated instance ID") }
	if task.Revision != 1 { t.Fatalf("revision=%d, want 1", task.Revision) }
	if task.Phase != "scan" { t.Fatalf("phase=%q, want scan", task.Phase) }
	requireForeignKeysValid(t, db)
}
```

- [ ] 运行聚焦 Store 测试，确认 v3 缺少版本/阶段列导致新断言失败。

运行：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store -run 'TestLocalTask(V3Migration|MigrationNewAndLegacy|Schema)'
```

预期：FAIL，证据为缺少 `instance_id`、`revision`、`phase`、`progress_total_known` 或删除回执结构。

- [ ] 实现 v4 新库结构和一次性重建表迁移。

在 `ddl.go` 中使用以下结构约束：

```sql
instance_id TEXT NOT NULL,
revision INTEGER NOT NULL DEFAULT 1 CHECK(revision > 0),
phase TEXT NOT NULL DEFAULT 'waiting'
  CHECK(phase IN ('waiting','scan','stage1','stage2','stage3','finalizing')),
progress_total_known INTEGER NOT NULL DEFAULT 0
  CHECK(progress_total_known IN (0,1)),
status TEXT NOT NULL
  CHECK(status IN (
    'pending','running','waiting_recovery','pausing','paused','stopping',
    'cancelled','succeeded','failed','deleting','delete_failed'
  ))
```

新增：

```sql
CREATE TABLE IF NOT EXISTS local_task_deletion_receipts (
  machine_id TEXT NOT NULL,
  task_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  deleted_at INTEGER NOT NULL,
  PRIMARY KEY(machine_id, task_id, instance_id)
);
```

设置 `localSchemaVersion = 4`。将 `migrateLocalTaskLifecycle` 实现为幂等的列/结构检查。对 v3，在事务外临时关闭外键，创建 `local_tasks_v4`，使用 `lower(hex(randomblob(16)))` 为每个旧任务生成实例 ID，并用 `CASE` 实现上述 status/stage 映射，然后复制数据、交换表、重建 `idx_local_tasks_machine_status`、提交、重新开启外键，再运行 `PRAGMA foreign_key_check`。重建或验证失败时返回错误并终止启动。

从 `Open` 按以下顺序调用迁移：

```go
if err := migrateVideoFeaturePresence(db); err != nil { return nil, err }
if err := migrateSyncQueueGeneration(db); err != nil { return nil, err }
if err := migrateLocalTaskEnvelope(db); err != nil { return nil, err }
if err := migrateLocalTaskLifecycle(db); err != nil { return nil, err }
```

从 `ddl` 字符串移除提前执行的 `PRAGMA user_version`；只有上述迁移和外键验证全部成功后，`Open` 才执行 `PRAGMA user_version = 4`。迁移失败不得把半迁移数据库标记为 v4。

更新任务查询/插入以读写新增列。只有真正插入新行时才用 `uuid.NewString()` 生成 `instance_id`；`CreateOrLoadLocalTask` 的幂等创建必须保留既有实例。

- [ ] 运行全部 Store 测试。

运行：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store
```

预期：PASS，覆盖新库结构、v3 迁移、重复打开幂等性和外键验证。

- [ ] 只提交结构与模型文件。

```powershell
git add -- internal/store/ddl.go internal/store/db.go internal/store/local_tasks.go internal/store/local_tasks_test.go
git diff --cached --name-only
git commit -m "feat: version local task snapshots"
```

预期暂存路径：严格等于上述四个路径。

## Task 2: 在 Store 中强制版本化生命周期与阶段进度转换

**Files:**

- Modify: `internal/store/local_tasks.go`
- Modify: `internal/store/local_tasks_test.go`

**Produces:**

```go
var ErrLocalTaskStale = errors.New("stale_task")
var ErrLocalTaskInstanceMismatch = errors.New("task_instance_mismatch")

type LocalTaskVersion struct {
	InstanceID string
	Revision   int64
}

type LocalTaskControl struct {
	TaskID          string
	InstanceID      string
	ExpectedRevision int64
}

type LocalTaskProgressUpdate struct {
	Phase             string
	Stage             int
	ProgressComplete  int64
	ProgressTotal     int64
	ProgressTotalKnown bool
	StatsJSON          string
}

func (d *DB) TransitionLocalTaskLifecycle(
	ctx context.Context,
	machineID string,
	control LocalTaskControl,
	toStatus string,
	safeCode *string,
	safeMessage *string,
) (LocalTask, error)

func (d *DB) UpdateLocalTaskProgress(
	ctx context.Context,
	machineID string,
	control LocalTaskControl,
	update LocalTaskProgressUpdate,
) (LocalTask, error)
```

- [ ] 新增会失败的转换、CAS、排序和进度测试。

覆盖每一条允许的转换边：

```text
pending -> running | pausing | stopping | deleting | waiting_recovery
running -> pausing | stopping | deleting | succeeded | failed | waiting_recovery
waiting_recovery -> running | pausing | stopping | deleting | failed
pausing -> paused | stopping | deleting | failed | waiting_recovery
paused -> pending | cancelled | deleting
stopping -> cancelled | deleting | failed | waiting_recovery
failed -> pending | deleting
cancelled -> pending | deleting
succeeded -> deleting
delete_failed -> deleting
```

另需断言：

- 稳定终态拒绝无关转换；
- 接受的生命周期转换恰好递增一次 revision；
- stale revision 返回 `ErrLocalTaskStale`，错误实例返回 `ErrLocalTaskInstanceMismatch`，且两者都不修改记录；
- 普通进度写入不改变 revision；
- 同一 phase 内 completed 单调，已知 total 只能保持或增加；
- `progress_total_known=false` 时 total 可随枚举发现量单调增加，并可在同一 phase 一次切换为 true；一旦为 true 不得退回 false；
- phase 前进可以重置 completed/total，phase 回退必须失败；
- 未知总数使用 `progress_total_known = false`，不得使用魔法数；
- 列表使用 `ORDER BY created_at DESC, task_id DESC`。

```go
func TestLocalTaskProgressAdvancesPhaseWithoutBumpingRevision(t *testing.T) {
	task := createVersionedTask(t)
	updated, err := db.UpdateLocalTaskProgress(ctx, "machine-1", store.LocalTaskControl{
		TaskID: task.TaskID, InstanceID: task.InstanceID, ExpectedRevision: task.Revision,
	}, store.LocalTaskProgressUpdate{
		Phase: "stage2", Stage: 2, ProgressComplete: 0,
		ProgressTotal: 12, ProgressTotalKnown: true, StatsJSON: "{}",
	})
	if err != nil { t.Fatal(err) }
	if updated.Revision != task.Revision {
		t.Fatalf("revision=%d, want %d", updated.Revision, task.Revision)
	}
}
```

- [ ] 运行测试并确认转换/排序断言失败。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store -run 'TestLocalTask(Lifecycle|Progress|List|Stale)'
```

预期：FAIL，因为当前 Store 既不校验实例/revision，也没有阶段变化模型。

- [ ] 为每次状态/进度变更实现单一事务。

生命周期转换必须在事务内读取行，先区分实例不匹配与 revision 过期、再校验状态图，最后使用 CAS 条件更新：

```sql
UPDATE local_tasks
SET status=?5,
    revision=revision+1,
    safe_error_code=?6,
    safe_error_message=?7,
    updated_at=?8,
    started_at=CASE WHEN ?5='running' THEN COALESCE(started_at,?8) ELSE started_at END,
    completed_at=CASE WHEN ?5 IN ('succeeded','failed','cancelled') THEN ?8 ELSE NULL END
WHERE machine_id=?1 AND task_id=?2 AND instance_id=?3 AND revision=?4;
```

进度更新使用相同 CAS 条件写入 `phase`、`stage`、计数、总数已知标志和统计，但不改变 `revision` 或 `status`。影响行数为零时返回 `ErrLocalTaskStale`。阶段顺序集中在一个 map 中：

```go
var localTaskPhaseOrder = map[string]int{
	"waiting": 0, "scan": 1, "stage1": 2,
	"stage2": 3, "stage3": 4, "finalizing": 5,
}
```

重构 `CancelLocalTask` 和 `RetryLocalTask` 以调用新转换 API。临时包装仅保留到下游包迁移完成，并在 Task 5 删除。恢复时让 `pending/running` 变为 `waiting_recovery`、`pausing` 变为 `paused`、`stopping` 变为 `cancelled`，每次只递增一次 revision；`paused` 和 `deleting` 保持不变，交由 Service 收敛。

- [ ] 运行聚焦和完整 Store 测试集。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store -run 'TestLocalTask'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store
```

预期：PASS。

- [ ] 提交生命周期 Store 变更。

```powershell
git add -- internal/store/local_tasks.go internal/store/local_tasks_test.go
git diff --cached --name-only
git commit -m "feat: enforce local task lifecycle versions"
```

## Task 3: 以事务和幂等方式删除任务专属分析数据

**Files:**

- Create: `internal/store/local_task_delete.go`
- Create: `internal/store/local_task_delete_test.go`
- Modify: `internal/store/local_tasks.go`

**Produces:**

```go
var ErrLocalTaskDeleteRetryable = errors.New("task_delete_retryable")

type LocalTaskDeleteResult struct {
	Deleted        bool
	AlreadyDeleted bool
	DeletedAt      int64
}

func (d *DB) HasLocalTaskDeletionReceipt(
	ctx context.Context, machineID, taskID string,
) (bool, error)

func (d *DB) LoadLocalTaskDeletionReceipt(
	ctx context.Context, machineID, taskID, instanceID string,
) (LocalTaskDeleteResult, error)

func (d *DB) DeleteLocalTaskData(
	ctx context.Context,
	machineID string,
	control LocalTaskControl,
) (LocalTaskDeleteResult, error)
```

- [ ] 新增会失败的删除边界夹具测试。

准备两个任务、两个本地分析 run、候选对/分组/成员行、当前分析指针、审核、本地删除批次/明细、outbox 事件、全局文件/特征/缓存行和无关中央/同步记录。删除其中一个任务并断言精确顺序与边界：

1. 定位该任务的 `run_id`；
2. 删除其 `local_current_analysis` 指针；
3. 将匹配的 `local_delete_batches.run_id` 置为 `NULL`，同时保留 `local_delete_batches` 和 `local_delete_items`；
4. 删除匹配的 `local_reviews`；
5. 删除匹配的 `local_dup_members`；
6. 删除匹配的 `local_dup_groups`；
7. 删除匹配的 `local_pair_scores`；
8. 删除未发送/重试中的 `local_outbox` 分析事件，其 `entity_key` 必须等于 run ID 或以 `runID + ':'` 开头；
9. 删除 `local_analysis_runs`；
10. 插入删除回执；
11. 删除 `local_tasks`。

断言另一任务、全局文件/特征/缓存、本地文件删除明细以及中央/同步数据逐字节不变；即使存在更旧分析 run，`local_current_analysis` 也保持空缺，不自动回退。

```go
func TestDeleteLocalTaskDataRemovesOnlyTaskOwnedAnalysis(t *testing.T) {
	fixture := seedTwoTaskDeletionFixture(t)
	result, err := fixture.DB.DeleteLocalTaskData(ctx, "machine-1", fixture.ControlA)
	if err != nil { t.Fatal(err) }
	if !result.Deleted { t.Fatal("task was not deleted") }
	assertTaskAAnalysisAbsent(t, fixture.DB)
	assertTaskBAndGlobalDataUnchanged(t, fixture.DB, fixture.Before)
}
```

- [ ] 新增会失败的幂等、任务 ID 复用和回滚测试。

断言：

- 对同一 `(machine, task, instance)` 重复删除时由回执返回成功；
- 后续使用同一 `task_id` 创建任务会获得新 `instance_id`，且不匹配旧回执；
- 同名新实例存在时重复发送旧实例删除请求，仍只命中旧回执并返回 `AlreadyDeleted`，新实例及其数据不变；
- 错误实例返回 `ErrLocalTaskInstanceMismatch`，错误 revision 返回 `ErrLocalTaskStale`；
- 当前状态不是 `deleting` 时返回 `ErrLocalTaskTransition`，不得绕过 Service 控制接受阶段直接删除；
- 已确认 `ack_at IS NOT NULL` 的分析 outbox 事件保留，且删除过程不新增中央撤回事件；
- 表驱动的 `BEFORE DELETE`、`BEFORE UPDATE` 和 `BEFORE INSERT` trigger 在十一个有序步骤逐一注入失败；每个用例都回滚之前的删除/更新和回执插入；
- SQLite 主错误码 `SQLITE_BUSY`、`SQLITE_LOCKED` 会包装为 `ErrLocalTaskDeleteRetryable`，约束/损坏错误不会。

- [ ] 运行删除测试，确认实现在加入前会失败。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store -run 'TestDeleteLocalTask|TestLocalTaskDeletionReceipt'
```

预期：FAIL，因为删除 API 和回执行为尚不存在。

- [ ] 实现删除事务与错误分类器。

outbox 清理必须精确匹配实体，不得使用宽泛的 `%runID%` 条件：

```sql
DELETE FROM local_outbox
WHERE ack_at IS NULL
  AND topic LIKE 'local_analysis.%'
  AND (
    entity_key=?1 OR
    substr(entity_key,1,length(?1)+1)=?1 || ':'
  );
```

使用 `errors.As(err, *sqlite.Error)` 和主错误码（`Code() & 0xff`）识别 `SQLITE_BUSY`/`SQLITE_LOCKED`，并用 `ErrLocalTaskDeleteRetryable` 包装。事务第一步始终查询精确 `(machine_id, task_id, instance_id)` 回执；命中即返回 `AlreadyDeleted`，即使同一 task ID 已存在新实例也不得读取或修改新实例。未命中回执时再读取并校验当前任务实例、revision 和 `deleting` 状态；任务行不存在则返回 `sql.ErrNoRows`。

- [ ] 运行 Store 删除测试与完整 Store 测试集。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store -run 'TestDeleteLocalTask|TestLocalTaskDeletionReceipt'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store
```

预期：PASS。

- [ ] 只提交删除 Store 文件。

```powershell
git add -- internal/store/local_task_delete.go internal/store/local_task_delete_test.go internal/store/local_tasks.go
git diff --cached --name-only
git commit -m "feat: delete local task analysis atomically"
```

## Task 4: 扩展本地协议的版本化控制与安全快照

**Files:**

- Modify: `internal/proto/local.go`
- Modify: `internal/proto/local_test.go`
- Modify: `internal/proto/message.go`
- Modify: `internal/proto/message_test.go`

**Produces:**

```go
const (
	LocalOperationTaskPause  = "local.task.pause"
	LocalOperationTaskResume = "local.task.resume"
	LocalOperationTaskDelete = "local.task.delete"
)

type LocalTaskControlRequest struct {
	TaskID           string `msgpack:"task_id"`
	InstanceID       string `msgpack:"instance_id"`
	ExpectedRevision int64  `msgpack:"expected_revision"`
}

type LocalTaskControlResponse struct {
	Task    *LocalTask `msgpack:"task,omitempty"`
	Deleted bool       `msgpack:"deleted,omitempty"`
}

type TaskDrainReason string

const (
	TaskDrainPause           TaskDrainReason = "pause"
	TaskDrainStop            TaskDrainReason = "stop"
	TaskDrainDelete          TaskDrainReason = "delete"
	TaskDrainProcessShutdown TaskDrainReason = "process_shutdown"
)
```

为 `proto.LocalTask` 增加 `InstanceID`、`Revision`、`Phase`、`ProgressTotalKnown`、`StartedAt` 和 `CompletedAt`。为 `TaskProgress` 增加 `TotalKnown`、`Failed` 和 `ElapsedMS`。线上保留 `TaskDone.Reason`，但只允许排空原因常量以及表示自然完成的空值。

- [ ] 新增会失败的 MessagePack 往返与校验测试。

覆盖 `IsLocalOperation` 中三个新操作、非空且无首尾空格的 ID 与正 revision 严格校验、完整快照往返、用于 cancel/retry 兼容的旧 `LocalTaskIDRequest` 解码，以及未知排空原因拒绝。

```go
func TestLocalTaskControlPayloadRoundTrip(t *testing.T) {
	want := LocalTaskControlRequest{
		TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7,
	}
	payload, err := EncodeLocalPayload(want)
	if err != nil { t.Fatal(err) }
	var got LocalTaskControlRequest
	if err := DecodeLocalPayload(payload, &got); err != nil { t.Fatal(err) }
	if got != want { t.Fatalf("got %#v, want %#v", got, want) }
	if err := got.Validate(); err != nil { t.Fatal(err) }
}
```

- [ ] 运行协议测试，确认缺少常量/字段时编译失败。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/proto -run 'TestLocalTask|TestTaskProgress|TestTaskDone'
```

预期：新契约实现前在编译阶段 FAIL。

- [ ] 实现 DTO、校验和允许列表，不新增传输消息类型。

`LocalTaskControlRequest.Validate` must require:

```go
if request.TaskID == "" || strings.TrimSpace(request.TaskID) != request.TaskID ||
	request.InstanceID == "" || strings.TrimSpace(request.InstanceID) != request.InstanceID ||
	request.ExpectedRevision <= 0 {
	return fmt.Errorf("invalid_task_control")
}
```

保留 `LocalOperationTaskCancel` 和 `LocalOperationTaskRetry`；其 Handler 同时接受新请求与旧任务 ID 请求。暂停/继续/删除只接受版本化请求。

- [ ] 运行完整协议测试集。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/proto
```

预期：PASS。

- [ ] 只提交协议文件。

```powershell
git add -- internal/proto/local.go internal/proto/local_test.go internal/proto/message.go internal/proto/message_test.go
git diff --cached --name-only
git commit -m "feat: add versioned local task controls"
```

## Task 5: 构建异步任务控制状态机

**Files:**

- Modify: `internal/localtask/service.go`
- Modify: `internal/localtask/service_test.go`
- Create: `internal/localtask/control.go`
- Create: `internal/localtask/control_test.go`

**Produces:**

```go
type DrainReason string

const (
	DrainPause           DrainReason = "pause"
	DrainStop            DrainReason = "stop"
	DrainDelete          DrainReason = "delete"
	DrainProcessShutdown DrainReason = "process_shutdown"
)

var ErrDrainRequested = errors.New("local_task_drain_requested")

type ControlRequest = proto.LocalTaskControlRequest
type ControlResult = proto.LocalTaskControlResponse

type RunControl struct {
	Context context.Context
	Drain   <-chan struct{}
	Reason  func() DrainReason
}

type ProgressUpdate struct {
	Phase              string
	Stage              int
	ProgressComplete   int64
	ProgressTotal      int64
	ProgressTotalKnown bool
	StatsJSON          string
}

type TaskRunner interface {
	Run(RunControl, CreateRequest, Task, func(ProgressUpdate) error) error
}

type TaskStore interface {
	CreateOrLoadLocalTask(context.Context, store.LocalTaskCreate) (store.LocalTask, error)
	LoadLocalTask(context.Context, string, string) (store.LocalTask, error)
	ListLocalTasks(context.Context, string, int, int) ([]store.LocalTask, error)
	RecoverLocalTasks(context.Context, string) ([]store.LocalTask, error)
	TransitionLocalTaskLifecycle(context.Context, string, store.LocalTaskControl, string, *string, *string) (store.LocalTask, error)
	UpdateLocalTaskProgress(context.Context, string, store.LocalTaskControl, store.LocalTaskProgressUpdate) (store.LocalTask, error)
	HasLocalTaskDeletionReceipt(context.Context, string, string) (bool, error)
	LoadLocalTaskDeletionReceipt(context.Context, string, string, string) (store.LocalTaskDeleteResult, error)
	DeleteLocalTaskData(context.Context, string, store.LocalTaskControl) (store.LocalTaskDeleteResult, error)
}

type Service interface {
	Create(context.Context, CreateRequest) (Task, error)
	List(context.Context, ListRequest) (Page[Task], error)
	Pause(context.Context, ControlRequest) (Task, error)
	ResumeTask(context.Context, ControlRequest) (Task, error)
	Cancel(context.Context, ControlRequest) (Task, error)
	Delete(context.Context, ControlRequest) (ControlResult, error)
	Retry(context.Context, ControlRequest) (Task, error)
	LegacyCancel(context.Context, string) (Task, error)
	LegacyRetry(context.Context, string) (Task, error)
}

type RecoverableService interface {
	Service
	PrepareRecovery(context.Context) error
	ResumeRecoveredTasks(context.Context) error
	Shutdown(context.Context) error
}
```

`ControlRequest` 对应协议身份/版本元组。`ControlResult` 包含已接受的过渡快照，或在重复删除已完成时包含 `Deleted=true`。

- [ ] 新增会失败的控制接受时序与最终状态测试。

使用显式暴露 `started`、`inFlightReleased` 和 `returned` channel 的阻塞假 Runner。断言：

- 暂停立即返回 `pausing` 快照，不等待假 Runner 完成；
- 暂停中不会启动第二次运行；
- 释放在途工作后进入 `paused` 并保留进度；
- 继续返回同一实例的 `pending`，随后仅通过一次新尝试进入 `running`；
- 停止返回 `stopping`，排空后进入 `cancelled`；
- 删除返回 `deleting`，排空后只调用一次 `DeleteLocalTaskData`，列表随后不再返回该任务；
- 删除稳定态任务跳过 Runner 排空；
- 暂停后停止会升级意图，删除会覆盖两者；
- 非幂等控制使用旧 expected revision 时返回 `ErrLocalTaskStale`，且不通知当前尝试；
- 在 `pausing/paused` 重复暂停、在 `stopping/cancelled` 重复停止、在 `deleting` 重复删除，均返回当前同实例快照且不再次递增 revision。

```go
func TestPauseAcceptsImmediatelyThenPersistsPausedAfterDrain(t *testing.T) {
	fixture := newBlockingServiceFixture(t)
	task := fixture.createAndWaitStarted()
	accepted, err := fixture.Service.Pause(ctx, ControlRequest{
		TaskID: task.TaskID, InstanceID: task.InstanceID,
		ExpectedRevision: task.Revision,
	})
	if err != nil { t.Fatal(err) }
	if accepted.Status != "pausing" { t.Fatalf("status=%q", accepted.Status) }
	close(fixture.Runner.InFlightReleased)
	fixture.requireStatusEventually("paused")
}
```

- [ ] 新增会失败的 active map 清理与控制竞态测试。

保留现有取消/重试竞态覆盖，并扩展暂停/删除场景：

- 旧尝试的延迟清理不能删除更新的 `active[taskID]` 条目；
- 旧 revision 的迟到进度回调收到 `ErrLocalTaskStale`，不能覆盖恢复后的尝试；
- 两个并发暂停命令只有一个胜出，另一个得到 stale 结果；
- 删除等待精确运行尝试的 `done` channel，而非只等待 `ctx.Done()`；
- 没有更强用户意图时，进程结束持久化为 `waiting_recovery` 而非 `cancelled`。

- [ ] 运行聚焦测试，确认现有 Service API 无法满足要求。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/localtask -run 'Test(LocalTask)?(Pause|Resume|Cancel|Delete|Control|Attempt|ProcessShutdown)'
```

预期：在编译阶段或同步取消行为断言处 FAIL。

- [ ] 实现每任务一个控制 gate 和版本感知的 `taskAttempt`。

同一任务 ID 的创建、控制和收尾操作共用一个 gate。可变运行尝试身份放在互斥锁后：

```go
type taskAttempt struct {
	mu       sync.RWMutex
	taskID   string
	instance string
	revision int64
	reason   DrainReason
	drain    chan struct{}
	drainOnce sync.Once
	hardCancel context.CancelFunc
	done     chan struct{}
}

func (a *taskAttempt) version() store.LocalTaskControl {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return store.LocalTaskControl{
		TaskID: a.taskID,
		InstanceID: a.instance,
		ExpectedRevision: a.revision,
	}
}
```

控制接受顺序固定为：

1. 获取任务 gate；
2. 读取并校验期望身份/revision；
3. 持久化 `pausing`、`stopping` 或 `deleting` 并递增 revision；
4. 更新内存运行尝试的 revision 和意图；
5. 关闭运行尝试的 drain channel，但不取消当前在途 Worker context；
6. 返回新快照；
7. 后台运行尝试等待 Runner 排空，持久化 `paused`/`cancelled` 或开始删除。

如果 pending 或 waiting-recovery 任务没有活动尝试，仍先持久化并返回过渡快照，再排入后台收尾，立即收敛为 `paused`/`cancelled` 或开始删除。不得只为完成空闲控制而虚构 Runner。

控制意图优先级为 `delete > stop > pause > process_shutdown`。进度回调读取运行尝试的最新 revision，调用 `UpdateLocalTaskProgress`，且绝不修改 status。

增加运行尝试级进度合并器。阶段变化时立即持久化，同阶段最多每秒一次；进入排空或自然终态前必须刷新最新快照。非 stale 的进度写入错误通过注入的 `logf` 记录，保留为待写进度并在下一 tick 重试，不把任务直接标为失败。`ErrLocalTaskStale` 只终止已被替代的 reporter，防止旧尝试继续写入。

同步更新 `taskFromStore`，完整映射 `instance_id`、`revision`、`phase`、`progress_total_known`、创建/更新时间及可空开始/完成时间；envelope 只用于还原 roots/mode/rescan/extensions，不进入错误摘要。

- [ ] 实现继续、重试、启动恢复与删除收敛。

启动收敛规则固定为：

```text
pending/running/waiting_recovery -> launch one recovered attempt
pausing                          -> paused
paused                           -> remain paused; do not launch
stopping                         -> cancelled
deleting                         -> resume deletion reconciliation
delete_failed                    -> remain visible for explicit delete retry
terminal                         -> no action
```

`ResumeTask` only accepts `paused`; it persists `pending`, returns that accepted snapshot, then the background attempt persists `running`. `Retry` only accepts `failed` or `cancelled`; it keeps the same instance, increments revision, preserves the durable phase/checkpoint, clears safe errors, and starts one attempt. `LegacyCancel` and `LegacyRetry` first reject any task ID that has a deletion receipt, then resolve the current instance/revision under the task gate before delegating to the versioned path.

`Delete` 在读取当前任务前先调用 `LoadLocalTaskDeletionReceipt` 查询精确实例；命中时直接返回 `Deleted=true`。这样同名新任务存在时，迟到的旧实例删除请求只命中旧回执。未命中时才验证当前实例/revision、写入 `deleting` 并启动排空/删除。

`Shutdown(ctx)` marks the service as closing, upgrades every active attempt with no stronger intent to `process_shutdown`, closes each drain channel and waits for the exact `done` channels. It persists those attempts as `waiting_recovery`. Only if the caller's bounded shutdown context expires may it invoke `hardCancel`; it then returns the context error so runtime acceptance cannot misreport the drain as successful.

Runner 完成结果依据已存意图解释，而不是依据 `context.Canceled`：没有已接受控制时自然 `nil` 变为 `succeeded`；已接受 `pause/stop/delete/process_shutdown` 时，即使最后一个在途单元自然完成，也分别进入 `paused/cancelled/删除收敛/waiting_recovery`；其他错误以安全错误码进入 `failed`。排空期间接受的更强意图始终覆盖较早原因。

遇到 `ErrLocalTaskDeleteRetryable` 时按 `1s, 2s, 4s, 8s, 16s, 30s` 重试；第六次自动重试后仍失败则持久化 `delete_failed`，安全错误码为 `delete_retry_exhausted`。通过 `ServiceOption` 注入退避函数，使测试使用零时长 channel 而不真实休眠。永久错误立即持久化为 `delete_failed`，错误码为 `delete_failed`。

将旧启动方法从 `Resume` 改名为 `ResumeRecoveredTasks`，避免与用户继续命令混淆。所有 Service 调用点改用版本化方法后，删除临时旧 Store 转换包装。

- [ ] 运行 Service 测试，再运行该包的 race 检测。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/localtask
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/localtask
```

预期：两者均 PASS。若当前 Windows Go 工具链无法构建 `-race`，记录精确工具链错误并标记 `BLOCKED`；非 race 测试集仍必须通过。

- [ ] 只提交编排文件。

```powershell
git add -- internal/localtask/service.go internal/localtask/service_test.go internal/localtask/control.go internal/localtask/control_test.go
git diff --cached --name-only
git commit -m "feat: orchestrate local task lifecycle controls"
```

## Task 6: 让扫描和分析安全排空并上报业务阶段进度

**Files:**

- Modify: `internal/agent/scan.go`
- Modify: `internal/agent/scan_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`
- Modify: `internal/localanalysis/engine.go`
- Modify: `internal/localanalysis/engine_test.go`
- Modify: `internal/store/local_analysis.go`
- Modify: `internal/store/local_analysis_test.go`

**Produces:**

```go
type AnalysisProgress struct {
	Phase              string
	Complete           int64
	Total              int64
	TotalKnown         bool
	CheckpointStage    int
}

var ErrDrainRequested = errors.New("local_analysis_drain_requested")

func (e *Engine) RunWithProgress(
	ctx context.Context,
	taskID string,
	drain <-chan struct{},
	report func(AnalysisProgress) error,
) error

func (m *ScanManager) Drain(
	taskID string,
	reason proto.TaskDrainReason,
) (bool, *proto.TaskStats)

func (m *ScanManager) Abort(taskID string) bool
```

- [ ] 新增会失败的扫描排空测试。

构造包含阻塞中在途特征任务和排队路径的扫描夹具。调用 `Drain(taskID, pause)` 后：

- 不再枚举或派发新路径；
- 已运行的 Item 可以完成，且结果正常持久化/发送；
- 枚举回调停止后，已收集但未满批的记录仍使用未取消的工作 context 刷入 SQLite；
- 已注册但尚未派发的媒体 route 全部释放，不泄漏路由；
- 只有结果循环和批发送器结束后才发送 `MsgTaskDone`；
- `TaskDone.Reason == "pause"`；
- 第二次 drain 调用幂等并返回最新统计；
- `stop`、`delete` 和 `process_shutdown` 保留各自原因；
- 自然完成的 reason 为空。
- `scan_only` 自然扫描完成后上报 `finalizing`，刷新最终统计/检查点后以 `1/1` 结束；`scan_then_analysis` 则从 scan 前进到 stage1。

```go
func TestScanDrainStopsDispatchAndWaitsForInFlightResult(t *testing.T) {
	fixture := newBlockedScanFixture(t)
	fixture.start()
	accepted, _ := fixture.Manager.Drain(fixture.TaskID, proto.TaskDrainPause)
	if !accepted { t.Fatal("drain was not accepted") }
	fixture.requireNoFurtherDispatch()
	fixture.releaseInFlight()
	done := fixture.waitDone()
	if done.Reason != "pause" { t.Fatalf("reason=%q", done.Reason) }
}
```

- [ ] 新增会失败的本地分析检查点/进度测试。

为 Store 增加限定到任务 run 的读取器：

```go
func (d *DB) ListLocalPairScoresForRun(
	ctx context.Context, runID string,
) ([]LocalPairScore, error)
```

断言：

- `stage1` reports indeterminate while candidate discovery runs, then `1/1` when that attempt's stage-one result is available;
- `stage2` total equals candidate-pair count and increments only after `SaveLocalPairScore` succeeds;
- `stage3` total equals stage-two yes-pairs and increments only after stage-three JSON is durable;
- `finalizing` is indeterminate until groups, completion and publication are durable, then reports `1/1`;
- 完成 N 个二筛候选对后暂停并继续时使用同一 `run_id`，加载已保存候选对、跳过对应 Worker 调用，从 N 继续且不产生重复行；
- 完成 N 个三筛候选对后暂停也会跳过已有三筛 JSON；
- 持久保存前的排空绝不推进进度。

- [ ] 新增会失败的生产适配器测试。

适配器必须把扫描进度/统计和分析进度转换成 `localtask.ProgressUpdate`，并在控制排空后等待扫描终态，而不是立即返回 `ctx.Err()`。

```go
func TestAgentLocalTaskRunnerWaitsForScanDrainBeforeReturning(t *testing.T) {
	runner, scan := newRunnerWithBlockedScan(t)
	control := newDrainingRunControl(localtask.DrainPause)
	done := make(chan error, 1)
	go func() { done <- runner.Run(control, request, snapshot, report) }()
	assertNotClosed(t, done)
	scan.finish(proto.TaskDrainPause)
	err := <-done
	if !errors.Is(err, localtask.ErrDrainRequested) { t.Fatalf("error=%v", err) }
}
```

- [ ] 运行聚焦测试，确认排空/进度行为尚缺失。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/agent ./internal/localanalysis ./internal/store ./cmd/agent -run 'Test(ScanDrain|LocalAnalysis.*Progress|AgentLocalTaskRunner)'
```

预期：排空原因、持久候选对读取和阶段回调实现前 FAIL。

- [ ] 在 `ScanManager` 中用带原因的排空替换布尔取消。

每次扫描同时保留“硬停止 context”和“停止新工作 channel”。`Drain` 在 `dispatchMu` 临界区内保存最强原因并关闭停止新工作 channel，但不取消数据库/Worker context；该方法返回后必须保证不会再发生新派发。枚举器观察 channel 后用专用 sentinel 结束枚举，再使用未取消 context 刷新尾批。派发器关闭 jobs 前释放所有未派发 work 的 `cancelRoute`；已经交给 worker 的 work 完成正常终端路径。worker/result/batch goroutine 通过现有 wait group 排空，只有全部等待结束后收尾器才发送 `TaskDone`。`Abort` 仅在有界硬停止 context 到期时取消工作 context。`Cancel(taskID)` 仅临时包装为 `Drain(taskID, TaskDrainStop)`，待所有生产调用和测试迁移后删除。

进度上报必须携带：

```go
proto.TaskProgress{
	TaskID: taskID,
	Done: stats.Done,
	Total: stats.Total,
	TotalKnown: enumerationComplete,
	Speed: currentSpeed,
	Failed: stats.Failed + stats.ScanErrors,
	ElapsedMS: stats.ElapsedMS,
}
```

scan 阶段统一按“本次根目录中已枚举文件”为总单元：枚举期间每秒上报 `Done=0`、`Total=已发现数`、`TotalKnown=false`；枚举完成后令 `cached=enumerated-pendingWork`，先上报 `Done=cached`、`Total=enumerated`、`TotalKnown=true`，随后每个持久化完成/失败的 work 递增 Done。恢复时以持久快照为下限：`total=max(snapshot.Total, enumerated)`、`complete=max(snapshot.Complete, cached+attemptDone)`；自然结束时把旧快照中已消失的剩余项计入 skipped，使 `complete=total`。这样同 phase 计数不回退，也不把动态发现数误当成最终百分比。

- [ ] 为分析引擎加入持久阶段上报和候选对级恢复。

启动时把既有候选对评分加载到 `map[pairKey]LocalPairScore`。每个二筛候选对复用 `Stage2JSON` 非 nil 的行，每个三筛候选对复用 `Stage3JSON` 非 nil 的行。不得只凭计数推断完成。只有对应行持久化成功后才上报进度。继续使用 `BeginLocalAnalysis(machineID, taskID)`，使暂停任务重新打开同一个 building run。

在一筛前后、每个二筛候选对前、每个三筛候选对前和安全收尾前检查 drain channel。一个候选对一旦开始，就使用 `RunControl.Context` 完成两次 Worker 调用和持久保存；暂停/停止/删除不得取消这个 context。在处理单元之间观察到排空信号时返回 `localanalysis.ErrDrainRequested`。一筛视为一个在途单元：若期间收到排空信号，让其事务结果完成，再在二筛前返回。

使用以下阶段边界：

```go
report(AnalysisProgress{Phase: "stage1", TotalKnown: false, CheckpointStage: 1})
report(AnalysisProgress{Phase: "stage1", Complete: 1, Total: 1, TotalKnown: true, CheckpointStage: 1})
report(AnalysisProgress{Phase: "stage2", Complete: persistedStage2, Total: int64(len(candidates)), TotalKnown: true, CheckpointStage: 2})
report(AnalysisProgress{Phase: "stage3", Complete: persistedStage3, Total: int64(len(stage2Passed)), TotalKnown: true, CheckpointStage: 3})
report(AnalysisProgress{Phase: "finalizing", TotalKnown: false, CheckpointStage: 3})
report(AnalysisProgress{Phase: "finalizing", Complete: 1, Total: 1, TotalKnown: true, CheckpointStage: 3})
```

- [ ] 更新 `agentLocalTaskRunner` 以遵守 `RunControl`。

扫描恢复时，回调使用 `max(snapshot.ProgressComplete, message.Done)`，防止重新枚举把进度写回较小值。`control.Drain` 关闭时调用 `ScanManager.Drain(taskID, mappedReason)`，等待 `MsgTaskDone` 后才返回 `localtask.ErrDrainRequested`。只有 `control.Context.Done()` 的有界硬停止路径才调用 `ScanManager.Abort`。分析恢复时，抑制排序低于持久快照阶段的准备性回调；第一次同阶段回调使用 `max(snapshot.ProgressComplete, callback.Complete)`。当前或更高阶段的回调转发给 Service reporter；将 `localanalysis.ErrDrainRequested` 映射为 `localtask.ErrDrainRequested`，并保留其他安全内部错误。

在 `cmd/agent/main.go` 中把 `taskService.Shutdown` 接为 `runService` 的第一个排空回调，位于二阶段管理器与 fair pool 停止之前，超时使用现有 Worker 超时加清理宽限。该顺序让本地任务在共享执行池关闭前完成在途 Worker 作业。

同时把 `localTaskLifecycle.Resume` 及 `prepareLocalTaskLifecycle` 内的调用改为 `ResumeRecoveredTasks`，并在生命周期接口中加入 `Shutdown(context.Context) error`；对应 `cmd/agent/main_test.go` 断言启动顺序仍为“同步准备恢复 → listener ready → 异步恢复”，停止顺序为“任务排空 → phase2 排空 → fair pool 关闭”。

- [ ] 运行受影响测试与竞态敏感包。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store ./internal/localanalysis ./internal/agent ./cmd/agent
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/localtask ./internal/agent
```

预期：PASS；唯一允许的例外是明确记录的 `-race` 工具链 `BLOCKED`。

- [ ] 提交生产排空/进度链路。

```powershell
git add -- internal/agent/scan.go internal/agent/scan_test.go cmd/agent/main.go cmd/agent/main_test.go internal/localanalysis/engine.go internal/localanalysis/engine_test.go internal/store/local_analysis.go internal/store/local_analysis_test.go
git diff --cached --name-only
git commit -m "feat: drain local tasks at durable checkpoints"
```

## Task 7: 通过 Agent 本地 Handler 路由控制操作

**Files:**

- Modify: `internal/agent/local_handler.go`
- Modify: `internal/agent/local_handler_test.go`

- [ ] 新增会失败的暂停、继续、停止、删除及旧版兼容 Handler 测试。

断言通过认证的本地请求：

- 所有控制都解码版本化请求；
- 使用期望元组恰好调用一次 Service 方法；
- 返回已接受的任务快照；
- 已存在删除回执时返回 `{deleted:true}`；
- stale 请求映射为 `stale_task`，错误实例映射为 `task_instance_mismatch`，非法状态转换映射为 `invalid_task_state`，未知任务映射为 `task_not_found`，Service 不可用映射为 `local_task_unavailable`，永久删除失败映射为 `task_delete_failed`，其他控制失败映射为 `task_control_failed`；
- 仅在不存在删除回执时对 cancel/retry 接受旧 `LocalTaskIDRequest`；
- 任务 ID 复用后的旧控制返回 `task_instance_required`；
- 绝不暴露假 Service 的原始错误文本。

```go
func TestLocalTaskPauseRoutesVersionedControl(t *testing.T) {
	service := &fakeLocalTaskService{pauseTask: taskSnapshot("pausing", 8)}
	response := handleLocalTaskRequest(t, service, proto.LocalOperationTaskPause,
		proto.LocalTaskControlRequest{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7})
	if !response.OK { t.Fatalf("response=%#v", response) }
	if service.pauseRequest.ExpectedRevision != 7 { t.Fatalf("request=%#v", service.pauseRequest) }
}
```

- [ ] 运行 Handler 测试，确认 Service 接口与分支尚不完整。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/agent -run 'TestLocalTask.*(Pause|Resume|Cancel|Delete|Legacy|Safe)'
```

预期：新接口与分支接通前 FAIL。

- [ ] 扩展 `LocalTaskService` 与 Handler 路由。

使用不同方法，不把所有操作压缩成无类型字符串：

```go
type LocalTaskService interface {
	Create(context.Context, localtask.CreateRequest) (localtask.Task, error)
	List(context.Context, localtask.ListRequest) (localtask.Page[localtask.Task], error)
	Pause(context.Context, localtask.ControlRequest) (localtask.Task, error)
	ResumeTask(context.Context, localtask.ControlRequest) (localtask.Task, error)
	Cancel(context.Context, localtask.ControlRequest) (localtask.Task, error)
	Delete(context.Context, localtask.ControlRequest) (localtask.ControlResult, error)
	Retry(context.Context, localtask.ControlRequest) (localtask.Task, error)
	LegacyCancel(context.Context, string) (localtask.Task, error)
	LegacyRetry(context.Context, string) (localtask.Task, error)
}
```

共享解码器先检查 MessagePack map 的键。只要存在 `instance_id` 或 `expected_revision` 任一键，就要求完整版本化结构，绝不回退。仅对 cancel/retry，严格只含旧 `task_id` 结构的载荷调用 `LegacyCancel`/`LegacyRetry`；这些 Service 方法在任务 gate 内执行删除回执和当前版本检查。暂停/继续/删除拒绝旧载荷。

- [ ] 运行 Agent Handler 与 localtask 测试集。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/agent ./internal/localtask
```

预期：PASS。

- [ ] 提交 Agent 本地控制桥接。

```powershell
git add -- internal/agent/local_handler.go internal/agent/local_handler_test.go
git diff --cached --name-only
git commit -m "feat: route local task lifecycle controls"
```

## Task 8: 通过 NodeTray 暴露安全控制并重新生成 Wails 绑定

**Files:**

- Modify: `internal/nodetray/traymodel/model.go`
- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`
- Modify: `nodetray/app.go`
- Modify: `nodetray/app_test.go`
- Modify: `nodetray/frontend/wailsjs/go/main/Backend.d.ts`
- Modify: `nodetray/frontend/wailsjs/go/main/Backend.js`
- Modify: `nodetray/frontend/wailsjs/go/models.ts`

**Produces:**

```go
type LocalTaskControl struct {
	TaskID           string `json:"taskId"`
	InstanceID       string `json:"instanceId"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

type LocalTask struct {
	TaskID             string   `json:"taskId"`
	InstanceID         string   `json:"instanceId"`
	Revision           int64    `json:"revision"`
	Source             string   `json:"source"`
	Mode               string   `json:"mode"`
	Stage              int      `json:"stage"`
	Status             string   `json:"status"`
	Phase              string   `json:"phase"`
	Roots              []string `json:"roots"`
	ProgressComplete   int64    `json:"progressComplete"`
	ProgressTotal      int64    `json:"progressTotal"`
	ProgressTotalKnown bool     `json:"progressTotalKnown"`
	Speed              string   `json:"speed"`
	Failures           int64    `json:"failures"`
	Duration           string   `json:"duration"`
	SyncStatus         string   `json:"syncStatus"`
	ErrorCode          string   `json:"errorCode"`
	ErrorSummary       string   `json:"errorSummary"`
	CreatedAt          int64    `json:"createdAt"`
	UpdatedAt          int64    `json:"updatedAt"`
	StartedAt          int64    `json:"startedAt"`
	CompletedAt        int64    `json:"completedAt"`
}

type LocalTaskResult struct {
	OK           bool      `json:"ok"`
	Task         LocalTask `json:"task"`
	Deleted      bool      `json:"deleted"`
	ErrorCode    string    `json:"errorCode"`
	ErrorSummary string    `json:"errorSummary"`
}
```

Backend 方法：

```go
func (b *Backend) PauseLocalTask(value traymodel.LocalTaskControl) traymodel.LocalTaskResult
func (b *Backend) ResumeLocalTask(value traymodel.LocalTaskControl) traymodel.LocalTaskResult
func (b *Backend) CancelLocalTask(value traymodel.LocalTaskControl) traymodel.LocalTaskResult
func (b *Backend) DeleteLocalTask(value traymodel.LocalTaskControl) traymodel.LocalTaskResult
func (b *Backend) RetryLocalTask(value traymodel.LocalTaskControl) traymodel.LocalTaskResult
```

对已完成的幂等删除，返回 `LocalTaskResult.OK=true`、`Deleted=true`，任务值为空。

- [ ] 新增会失败的 Service 与 Backend 测试。

扩展 `fakeLocalAgentGateway` 以捕获操作和解码后的载荷。对五个方法断言：

- 使用三个身份字段调用预期 Agent 操作；
- 映射返回任务快照中的 phase、revision、总数已知标志和时间戳；
- 保留安全错误码/摘要，但丢弃所有原始 gateway 错误；
- 以 `Deleted=true` 表示删除完成；
- revision 为零时在 gateway 调用前拒绝。

```go
func TestPauseLocalTaskForwardsVersionedControl(t *testing.T) {
	gateway := &fakeLocalAgentGateway{response: encodedTaskControlResponse(t, "pausing", 8)}
	service := newTestService(gateway)
	result := service.PauseLocalTask(ctx, traymodel.LocalTaskControl{
		TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7,
	})
	if !result.OK { t.Fatalf("result=%#v", result) }
	if gateway.operation != proto.LocalOperationTaskPause { t.Fatalf("operation=%q", gateway.operation) }
	if result.Task.Revision != 8 { t.Fatalf("revision=%d", result.Task.Revision) }
}
```

- [ ] 运行聚焦 NodeTray Go 测试，确认方法尚缺失。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/nodetray/app ./nodetray -run 'Test.*LocalTask'
```

预期：DTO 与方法实现前在编译阶段 FAIL。

- [ ] 实现共享控制调用器与公开 Backend 包装方法。

共享调用器必须校验 DTO、编码 `proto.LocalTaskControlRequest`、调用精确操作、解码 `LocalTaskControlResponse`，并且只通过 `traymodel` 传递安全字段：

```go
func (s *Service) controlLocalTask(
	ctx context.Context,
	operation string,
	request traymodel.LocalTaskControl,
) traymodel.LocalTaskResult
```

删除授权 token 以及全部文件系统/数据库内部信息留在服务端。任务删除不得复用无关的文件删除确认流程。

- [ ] 生成绑定前运行 Go 测试。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/nodetray/... ./nodetray
```

预期：PASS。

- [ ] 使用仓库锁定的 Wails 版本重新生成绑定。

在 `nodetray` 目录运行：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' run github.com/wailsapp/wails/v2/cmd/wails@v2.12.0 generate module
```

随后验证五个方法名和三个控制字段：

```powershell
rg -n "PauseLocalTask|ResumeLocalTask|CancelLocalTask|DeleteLocalTask|RetryLocalTask|expectedRevision|instanceId" frontend/wailsjs/go/main frontend/wailsjs/go/models.ts
```

预期：每个方法都出现在 JS 与声明输出中；模型包含 `taskId`、`instanceId` 和 `expectedRevision`。

- [ ] 重跑 Go 测试并提交桥接与生成产物。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/nodetray/... ./nodetray
git add -- internal/nodetray/traymodel/model.go internal/nodetray/app/service.go internal/nodetray/app/service_test.go nodetray/app.go nodetray/app_test.go nodetray/frontend/wailsjs/go/main/Backend.d.ts nodetray/frontend/wailsjs/go/main/Backend.js nodetray/frontend/wailsjs/go/models.ts
git diff --cached --name-only
git commit -m "feat: expose local task controls in nodetray"
```

## Task 9: 渲染紧凑任务 Item 与精确操作矩阵

**Files:**

- Create: `nodetray/frontend/src/pages/localTaskLifecycle.ts`
- Create: `nodetray/frontend/src/pages/localTaskLifecycle.test.ts`
- Create: `nodetray/frontend/src/pages/LocalTaskItem.tsx`
- Create: `nodetray/frontend/src/pages/LocalTaskItem.test.tsx`
- Modify: `nodetray/frontend/src/app.css`
- Modify: `nodetray/frontend/src/api/localAgent.ts`

**Produces:**

```ts
export type LocalTaskStatus =
  | "pending" | "running" | "waiting_recovery" | "pausing" | "paused"
  | "stopping" | "cancelled" | "succeeded" | "failed"
  | "deleting" | "delete_failed";

export type LocalTaskPhase =
  | "waiting" | "scan" | "stage1" | "stage2" | "stage3" | "finalizing";

export interface LocalTask {
  taskId: string;
  instanceId: string;
  revision: number;
  source: string;
  mode: string;
  stage: number;
  status: LocalTaskStatus;
  phase: LocalTaskPhase;
  roots: string[];
  progressComplete: number;
  progressTotal: number;
  progressTotalKnown: boolean;
  speed: string;
  failures: number;
  duration: string;
  syncStatus: string;
  errorCode?: string;
  errorSummary?: string;
  createdAt: number;
  updatedAt: number;
  startedAt: number;
  completedAt: number;
}

export type LocalTaskOperation = "pause" | "resume" | "cancel" | "delete" | "retry";

export interface LocalTaskControl {
  taskId: string;
  instanceId: string;
  expectedRevision: number;
}

export interface LocalTaskResult {
  ok: boolean;
  task?: LocalTask;
  deleted?: boolean;
  errorCode?: string;
  errorSummary?: string;
}
```

- [ ] 新增会失败的纯状态/操作测试。

编码已批准规格中的精确矩阵：

```ts
const actionsByStatus: Record<LocalTaskStatus, readonly LocalTaskOperation[]> = {
  pending: ["pause", "cancel", "delete"],
  running: ["pause", "cancel", "delete"],
  waiting_recovery: ["pause", "cancel", "delete"],
  pausing: [],
  paused: ["resume", "cancel", "delete"],
  stopping: [],
  cancelled: ["retry", "delete"],
  succeeded: ["delete"],
  failed: ["retry", "delete"],
  deleting: [],
  delete_failed: ["delete"],
};

const statusLabel: Record<LocalTaskStatus, string> = {
  pending: "等待中", running: "运行中", waiting_recovery: "等待恢复",
  pausing: "正在暂停", paused: "已暂停", stopping: "正在停止",
  cancelled: "已停止", succeeded: "已完成", failed: "失败",
  deleting: "正在删除", delete_failed: "删除失败",
};

const phaseLabel: Record<LocalTaskPhase, string> = {
  waiting: "等待", scan: "枚举与扫描", stage1: "一筛",
  stage2: "二筛", stage3: "三筛", finalizing: "安全收尾",
};
```

断言每个状态/阶段的中文文案、活动态分类、已知/未知总数进度展示，以及未来未知值会安全显示为“未知状态/未知阶段”且无可用操作。
`delete_failed` 的 `delete` 操作按钮文案必须为“重试删除”，其他可删除状态的文案为“删除”。

- [ ] 新增会失败的 `LocalTaskItem` 组件测试。

断言：

- 行内顺序为 `模式/创建时间 | 状态 · 阶段 | 进度 | 完成/总数 | 速度 · 失败 · 耗时 | 操作`；
- 完整任务 ID 可通过 `title` 查看，可见 ID 使用次要样式；
- 未知总数渲染不确定进度条和 `完成数 / --`；
- 已知总数渲染 `value`、`max` 和精确计数；
- 已知总数为 0 时显示 `0 / 0`，原生 `<progress>` 使用安全的正 `max`，不得产生无效属性或 NaN；
- 过渡状态显示“正在暂停/正在停止/正在删除”且控制按钮禁用；
- Item 级安全错误保留在该行；
- 回调接收操作和当前 `instanceId/revision`，绝不只传任务 ID。
- React 列表 key 使用 `instanceId`；同名新实例替换旧 `taskId` 行，迟到旧实例响应不能把旧行插回。

```tsx
render(<LocalTaskItem task={runningTask} locked={false} onAction={onAction} />);
await user.click(screen.getByRole("button", { name: "暂停" }));
expect(onAction).toHaveBeenCalledWith("pause", {
  taskId: runningTask.taskId,
  instanceId: runningTask.instanceId,
  expectedRevision: runningTask.revision,
});
```

- [ ] 运行聚焦前端测试，确认模块缺失时失败。

```powershell
npm --prefix nodetray/frontend test -- --run src/pages/localTaskLifecycle.test.ts src/pages/LocalTaskItem.test.tsx
```

预期：生命周期助手和任务 Item 尚不存在，因此 FAIL。

- [ ] 实现纯映射助手、任务 Item 组件与 API 表面。

在 `localAgent.ts` 中把每个包装函数直接映射到生成的 Wails 方法：

```ts
export const pauseLocalTask = (control: LocalTaskControl) => Backend.PauseLocalTask(control);
export const resumeLocalTask = (control: LocalTaskControl) => Backend.ResumeLocalTask(control);
export const cancelLocalTask = (control: LocalTaskControl) => Backend.CancelLocalTask(control);
export const deleteLocalTask = (control: LocalTaskControl) => Backend.DeleteLocalTask(control);
export const retryLocalTask = (control: LocalTaskControl) => Backend.RetryLocalTask(control);
```

总数已知时使用原生 `<progress>`，未知时使用具备 `role="progressbar"` 的无障碍 CSS 不确定进度条。CSS 在桌面宽度保持单行，在 NodeTray 现有窄窗断点以下切换为明确的两行网格，不得增加横向滚动。

- [ ] 运行新文件的聚焦测试与 lint。

```powershell
npm --prefix nodetray/frontend test -- --run src/pages/localTaskLifecycle.test.ts src/pages/LocalTaskItem.test.tsx
npm --prefix nodetray/frontend run lint
```

预期：PASS。

- [ ] 只提交任务 Item/模型文件。

```powershell
git add -- nodetray/frontend/src/pages/localTaskLifecycle.ts nodetray/frontend/src/pages/localTaskLifecycle.test.ts nodetray/frontend/src/pages/LocalTaskItem.tsx nodetray/frontend/src/pages/LocalTaskItem.test.tsx nodetray/frontend/src/app.css nodetray/frontend/src/api/localAgent.ts
git diff --cached --name-only
git commit -m "feat: render local task lifecycle items"
```

## Task 10: 增加创建插入、自适应轮询、确认与旧请求防护

**Files:**

- Modify: `nodetray/frontend/src/pages/LocalTasksPage.tsx`
- Modify: `nodetray/frontend/src/pages/LocalTasksPage.test.tsx`
- Modify: `nodetray/frontend/src/api/localAgent.test.ts`

**Produces:**

```ts
type RequestGeneration = number;

interface OperationLock {
  apiGeneration: RequestGeneration;
  instanceId: string;
  revision: number;
  operation: LocalTaskOperation;
}

const ACTIVE_POLL_MS = 1_000;
const IDLE_POLL_MS = 5_000;

export type LocalTasksAPI = {
  choose: (currentPath: string) => Promise<PathSelectionResult>;
  create: (request: LocalTaskCreate) => Promise<LocalTaskResult>;
  list: () => Promise<LocalTaskPage>;
  pause: (control: LocalTaskControl) => Promise<LocalTaskResult>;
  resume: (control: LocalTaskControl) => Promise<LocalTaskResult>;
  cancel: (control: LocalTaskControl) => Promise<LocalTaskResult>;
  delete: (control: LocalTaskControl) => Promise<LocalTaskResult>;
  retry: (control: LocalTaskControl) => Promise<LocalTaskResult>;
};
```

- [ ] 使用假定时器和延迟 Promise 重写/新增会失败的页面测试。

覆盖：

1. 初始加载按最新创建优先渲染任务；
2. 创建成功立即在表单下方 upsert 返回快照，并触发一次即时刷新；
3. 存在活动/过渡任务时恰好安排一次 1 秒刷新；
4. 全部为稳定任务时恰好安排一次 5 秒刷新；
5. 列表返回 `ok:false` 或 Promise rejection 时都保留最后可信任务、显示“状态可能已过期”，并在 5 秒后重试；
6. 成功恢复后清除过期标记；
7. 卸载时清除定时器并忽略迟到列表响应；
8. 切换 API generation 后忽略旧列表/控制响应及旧错误；
9. 同一 API generation 中较旧列表请求迟到时不能覆盖较新的列表快照；
10. 旧实例/revision 的迟到控制响应不能覆盖新 Item 或解锁新按钮；
11. `stale_task` 或 `task_instance_mismatch` 安全响应保留 Item，只释放匹配锁并立即刷新；
12. 暂停/继续无需确认直接提交；
13. 停止确认说明已完成结果会保留；
14. 删除确认列出四项边界：删除本机任务/分析、保留全局索引/特征/缓存、保留文件删除审计、不撤回中央数据；
15. 逐 Item 操作错误保留在对应 Item，且不清空列表。

```tsx
it("keeps one adaptive polling chain", async () => {
  vi.useFakeTimers();
  listLocalTasks.mockResolvedValueOnce(pageWith(runningTask));
  render(<LocalTasksPage api={api({ list: listLocalTasks })} />);
  await screen.findByText("运行中");
  expect(vi.getTimerCount()).toBe(1);
  await vi.advanceTimersByTimeAsync(999);
  expect(listLocalTasks).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(1);
  expect(listLocalTasks).toHaveBeenCalledTimes(2);
});
```

- [ ] 运行页面/API 测试，确认现有 2 秒 interval 行为不符合要求。

```powershell
npm --prefix nodetray/frontend test -- --run src/pages/LocalTasksPage.test.tsx src/api/localAgent.test.ts
```

预期：在轮询时序、控制、确认和旧响应断言处 FAIL。

- [ ] 实现快照合并与单一自调度轮询链。

在当前 API generation 内使用单调递增的请求序号。绝不调用 `setInterval`：

```ts
const scheduleNext = (delay: number) => {
  window.clearTimeout(timerRef.current);
  timerRef.current = window.setTimeout(() => void poll(), delay);
};

const acceptSnapshot = (incoming: LocalTask) => {
  setTasks(current => upsertNewestFirst(current, incoming));
};
```

注入的 `api` 对象身份变化时递增 API generation。每个 await 边界都将捕获的 API generation 和请求序号与当前 ref 比较。只有 `instanceId` 相同且 revision 不小于当前值，或响应是当前创建请求真正返回的新实例时，才允许替换 Item。列表刷新对删除后消失具有权威性，但请求失败时不得清空列表。

创建成功后：

1. 立即 upsert 响应快照；
2. 只在成功后重置表单；
3. 立即执行一次列表刷新；
4. 由该刷新选择下一次 1 秒/5 秒延迟。

- [ ] 使用 `ConfirmDialog` 实现逐项操作与确认。

点击时从任务快照构造控制请求。按钮锁键为 `apiGeneration + instanceId + revision + operation`。暂停/继续直接调用；停止和删除打开不同确认内容。成功后合并返回快照，或在 `Deleted=true` 时删除精确实例，然后刷新。收到 `stale_task` 或 `task_instance_mismatch` 时保留 Item 并立即刷新。只有拥有当前锁的 Promise 才能释放它。

- [ ] 运行聚焦测试、完整前端测试、lint 与构建。

```powershell
npm --prefix nodetray/frontend test -- --run src/pages/LocalTasksPage.test.tsx src/pages/LocalTaskItem.test.tsx src/pages/localTaskLifecycle.test.ts src/api/localAgent.test.ts
npm --prefix nodetray/frontend test
npm --prefix nodetray/frontend run lint
npm --prefix nodetray/frontend run build
```

预期：全部 PASS。

- [ ] 只提交页面编排测试/代码。

```powershell
git add -- nodetray/frontend/src/pages/LocalTasksPage.tsx nodetray/frontend/src/pages/LocalTasksPage.test.tsx nodetray/frontend/src/api/localAgent.test.ts
git diff --cached --name-only
git commit -m "feat: control and poll local task items"
```

## Task 11: 执行集成验证与真实运行验收

**Files:**

- 只有测试失败暴露范围内缺陷时才修改代码；该修复与精确测试文件一起暂存。
- 本任务不创建发布产物，也不执行部署。

- [ ] 运行格式化并确认未产生意外文件。

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w internal/store/ddl.go internal/store/db.go internal/store/local_tasks.go internal/store/local_task_delete.go internal/proto/local.go internal/proto/message.go internal/localtask/service.go internal/localtask/control.go internal/agent/scan.go internal/agent/local_handler.go cmd/agent/main.go internal/localanalysis/engine.go internal/store/local_analysis.go internal/nodetray/traymodel/model.go internal/nodetray/app/service.go nodetray/app.go
git status --short
```

预期：只出现已知用户改动和本功能精确文件。若格式化触及脏文件中的无关 hunk，暂存前只隔离本功能 hunk。

- [ ] 运行聚焦 Go 验证门。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/store ./internal/proto ./internal/localtask ./internal/agent ./internal/localanalysis ./cmd/agent ./internal/nodetray/... ./nodetray
```

预期：PASS。

- [ ] 运行完整 Go 测试集和相关 race 测试集。

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./...
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/localtask ./internal/agent
```

预期：PASS。race 构建的工具链限制必须记录为 `BLOCKED`，不得静默跳过。

- [ ] 运行完整前端验证门。

```powershell
npm --prefix nodetray/frontend test
npm --prefix nodetray/frontend run lint
npm --prefix nodetray/frontend run build
```

预期：PASS。

- [ ] 通过仓库标准脚本构建 NodeTray。

```powershell
pwsh -NoProfile -File .\scripts\build-nodetray.ps1
```

预期：脚本退出码为 0，并在脚本报告的路径生成 NodeTray 可执行文件。记录路径与 SHA-256；该产物只是构建结果，不代表获得部署权限。

- [ ] 环境允许时执行真实本机运行验收。

使用一次性测试数据库和足够大的一次性媒体目录。验证并记录以下证据：

- 创建响应立即插入任务 Item；
- scan 阶段在枚举总数未知前显示不确定进度，随后显示已知计数；
- 在枚举、队列等待、Worker 在途处理、二筛和三筛中暂停时停止新派发、让在途结果落库并进入 `paused`；
- Agent 重启后暂停任务仍保持暂停；
- 继续保持 `instance_id`、推进 revision、复用同一分析 run，并跳过已持久化文件/候选对结果；
- 停止进入 `cancelled` 并保留已完成本地结果；
- 删除活动任务时可见经过 `deleting`，只删除任务及其本地分析行，清空当前指针，保留全局索引/特征/缓存和文件删除审计，且不发出中央撤回；
- 复用任务 ID 时获得新实例，旧删除命令无害；
- 列表失败时保留可见 Item，并通过 5 秒轮询恢复；
- 窄窗布局换为两行且无横向滚动。

捕获控制前后任务快照、Agent 日志、相关 SQLite 行数和 UI 截图。任何未执行的 GUI、重启、磁盘、数据库或中央同步检查都要附原因标为 `PARTIAL`/`BLOCKED`。

- [ ] 检查最终 diff，并且仅在存在验证驱动修复时提交。

```powershell
git diff --check
git status --short
git diff --cached --name-only
```

若验证发现需要范围内修复，只暂存该实现文件及其精确回归测试，然后提交：

```powershell
git commit -m "fix: close local task lifecycle regressions"
```

若无需修复，不创建空提交。

## 完成标准

- 新任务创建后立即出现在表单下方，并能通过空闲轮询持续发现。
- 每个任务快照具有稳定实例身份、单调生命周期 revision、业务阶段和显式总数已知语义。
- 暂停、继续、停止和删除遵守已批准状态/操作矩阵及异步接受行为。
- 扫描与分析停止新工作、排空在途工作，并从持久文件/候选对边界继续。
- 删除具备事务性、幂等性和任务实例安全性，且严格限制在任务专属本地分析数据。
- 旧请求、旧实例、旧 revision 和迟到前端 Promise 不能修改或在视觉上覆盖更新状态。
- 聚焦与完整 Go/前端测试、lint 和构建通过；race 与真实运行边界如实报告。
