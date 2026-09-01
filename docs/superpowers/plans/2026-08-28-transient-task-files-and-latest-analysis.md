# Rust V2 主功能收敛与瞬态任务文件实施计划

> **实现要求：** 代码任务使用 `superpowers:test-driven-development`，按任务保存真实 RED、最小 GREEN 和回归证据；完成实现后再使用 `superpowers:verification-before-completion`。不要用源码字符串匹配代替行为测试。

**目标：** 删除跨重启任务恢复、分析续算、删除历史、非核心诊断和复杂配置恢复链，只保留计算、同步、性能优化、多机器、去重和完成结果使用。SQLite 只承担长期事实、缓存和同步；计算与删除由瞬态 TSV 实际驱动；本地分析只保留最近一次成功结果。

**设计：** [`docs/superpowers/specs/2026-08-28-transient-task-files-and-latest-analysis-design.md`](../specs/2026-08-28-transient-task-files-and-latest-analysis-design.md)

**技术边界：** Rust 2024、Tokio actor、rusqlite/WAL、Protobuf、Slint、UTF-8 无 BOM TSV、Windows 原子文件替换、PowerShell 验收。

## 一、功能去留清单

### 保留功能

| 功能块 | 保留边界 |
|---|---|
| 文件扫描 | Everything 默认枚举、Walker 回退、多根一次提交、规范路径去重 |
| 物理盘调度 | 枚举前解析物理盘编号和类型；读取配置中的 SSD/HDD/unknown 每盘额度、全局额度、权重轮转和老化保护 |
| 缓存查询 | SQLite 每批最多 1,000 项真正批量 `SELECT`；可选 PostgreSQL 批量缓存；查询阶段不写任务或阶段 |
| 媒体计算 | 缺失字段掩码、按物理盘 TSV、WorkerPool、Hash/Media 联合调度、结果校验和 SQLite ACK |
| 当前运行容错 | Worker 崩溃只让当前项失败并补建 Worker；其他文件、Worker 和物理盘继续运行 |
| 本地长期数据 | `contents`、`files`、媒体特征、联系表、`file_faults`、`sync_outbox`、`sync_state` |
| 多机器同步 | outbox、PostgreSQL 事务、ACK、游标追赶、断线重放和 SnapshotRequired 全量快照 |
| 去重分析 | 精确重复、相似图片、相似视频、二筛、代表直连分组、本地分析和跨机器分析 |
| 结果与删除 | 最近一次 Node 本地结果、Desktop 自有跨机器结果、滑动窗口、预览、内存复核、瞬态删除队列 |
| 性能和配置 | Worker 数、总读取线程、各盘额度、阈值、节点端点、PostgreSQL 等必要配置；CPU、磁盘 I/O、队列和 Worker 遥测 |
| 发布验证 | crate 行为测试、Windows harness、正式包验证和一次双物理盘真实媒体全量测试 |

### 删除功能

| 删除项 | 简化后的行为 |
|---|---|
| SQLite 任务、任务项、阶段持久化与恢复 | 当前进程只用 `RuntimeTaskRegistry`；重启任务列表为空，用户重新扫描 |
| recovery Task/Stage、Runtime ID 映射 | Registry 直接使用业务 Task ID，不新增 `TaskCatalog` 或 `ScanSessionCatalog` |
| WorkerPool planned restart + SQLite requeue | 重启计算引擎直接取消当前任务并重建 Pool，不恢复旧项 |
| 本地分析运行、输入、候选、分组、复核持久化 | 运行态只在内存和 `.partial.tsv`；成功后原子发布最近一次结果 |
| 本地 `retry_phase2` | 失败或不完整后重新创建完整分析 |
| Desktop `resume`、`retry_unresolved` 和旧协调器续接 | 重启、任务消失或门禁失败即结束；再次操作创建新分析 |
| 删除批次、删除项、墓碑历史、RetryDeleteItems | 当前运行只顺序消费 `delete.tasks.tsv`；完成或下次启动删除队列 |
| PostgreSQL/SQLite 任务、分析运行和删除运行同步 | 只同步跨机器去重所需的内容、当前文件状态和媒体特征 |
| 独立数据库表诊断页 | 只保留连接/schema 是否可用的状态和普通日志 |
| 文件故障清理页与管理协议 | `file_faults` 只供计算判断、当前失败详情和日志使用，不提供历史管理页 |
| 配置双文件 journal、prepare/commit、CAS 补偿、自动重连验证 | 只原子替换现有配置文件；明确提示重启 Node 后生效 |
| 本方案新增的磁盘满自动清理、历史归档、JSON/JSONL、`.idx` | 空间不足使当前写入和任务失败，不主动删除任何缓存、日志、构建产物或媒体 |

### 不得误删的正确性机制

- outbox 重放、PostgreSQL 提交后 ACK、同步 cursor 和 SnapshotRequired 是同步主功能，不属于任务恢复。
- Worker 当前运行内的崩溃隔离和替换是计算主功能，不属于跨重启任务恢复。
- SQLite/TSV 的提交 ACK、分析结果原子替换、删除前路径/大小/MD5 复核是结果正确性边界。
- PostgreSQL schema 连接校验保留；只删除面向用户的逐表诊断和统计页面。

## 二、全局实现约束

- 保留 SQLite schema 3 和现有物理表，避免现有媒体缓存失效；旧运行态表启动时清空，产品路径不再读写。等以后明确升级不兼容 schema 时再物理删表。
- 不兼容旧 Go/C++、旧 Rust 协议或旧运行态数据，不增加迁移层。
- 任务文件、删除队列和本地分析结果只用 UTF-8 无 BOM TSV，不用 JSON/JSONL，不生成 `.idx`。
- 方法、类型、字段和关键状态变量添加简洁中文注释，说明用途、所有权和状态变化；保持单 actor/单写者和小接口。
- 不新增后台清理服务。启动只精确删除 `data/node/runtime` 和未完成 `.partial.tsv`。
- Node 本地分析结果与 Desktop 跨机器分析结果分属各自所有者；Desktop 不读取 Node 本地分析结果。
- 普通同步不会使当前进程扫描快照过期。只有成功扫描收尾或成功删除推进 library revision 后，旧快照不能开始新分析。
- 真实媒体 `H:\pik\00000000000` 和 `I:\tmp` 只读；真实全量测试只跑一次，任务终态即结束，1,800 秒只是超时上限。
- 不触碰或部署到 `I:\Tool`。保留当前工作树所有无关修改，每次只暂存本任务文件，禁止 `git add -A`、`git clean` 和破坏性 reset。

## 三、核心接口

不新增统一 Catalog。Node actor 直接拥有 Registry 和一个很小的完成扫描映射：

```rust
/// Node actor 当前进程内的运行状态；不跨重启恢复。
struct EngineState {
    runtime_tasks: RuntimeTaskRegistry,
    completed_scans: BTreeMap<TaskId, CompletedScanSnapshot>,
}

/// 当前进程内一次成功扫描可供分析使用的冻结输入。
struct CompletedScanSnapshot {
    task_id: TaskId,
    roots: Vec<NormalizedPath>,
    library_revision: u64,
    outbox_high_seq: u64,
    manifest_path: PathBuf,
    manifest_sha256: [u8; 32],
    total_files: u64,
    succeeded_files: u64,
    failed_files: u64,
}

/// 一个计算运行中按物理盘拆分的实际任务源。
struct TransientTaskFileSet {
    run_id: TaskId,
    lanes: BTreeMap<PhysicalDiskId, TaskFileLane>,
}

/// 当前删除运行的顺序队列；任务终态后删除。
struct DeleteTaskFile {
    run_id: TaskId,
    path: PathBuf,
}
```

计算任务行固定为：

```text
状态\t任务项ID\t工作类型\t规范路径\t显示路径\t文件大小\t已知MD5\t缺失字段掩码
```

删除任务行固定为：

```text
状态\t删除项ID\t模式\t机器ID\t规范路径\t显示路径\t文件大小\tMD5
```

完成扫描成功清单固定为：

```text
S\t规范路径\t显示路径\t文件大小\tMD5\t媒体类型
```

计算任务文件和删除任务文件使用 `P/C/F`。计算行在 SQLite 结果提交 ACK 后改 `C`；删除行在文件系统成功且 `files.active/outbox/revision` 提交 ACK 后改 `C`。失败行改 `F` 并继续其他行。成功清单只有 `S` 行和尾记录。

## 四、实施任务

### Task 1：切断 SQLite 运行态持久化与恢复

**Files:**

- Modify: `crates/node-store/src/tasks.rs`
- Modify: `crates/node-store/src/analysis.rs`
- Modify: `crates/node-store/src/review.rs`
- Modify: `crates/node-store/src/delete.rs`
- Modify: `crates/node-store/src/open.rs`
- Modify: `crates/node-store/src/lib.rs`
- Replace tests: `crates/node-store/tests/task_recovery.rs`
- Create: `crates/node-store/tests/runtime_state_boundary.rs`

- [ ] 写 RED：预置旧任务、分析、复核和删除行，重新打开 NodeStore 后断言运行态全部清空；同时断言内容、文件、特征、故障、outbox 和 cursor 完全不变。
- [ ] 实现一个启动事务 `clear_transient_runtime_state`，只清理旧运行态表。
- [ ] 删除产品可见的 `recover_running_items`、`recover_active_computation_tasks`、`requeue_planned_items`、分析恢复和删除重试接口；schema/测试夹具需要时可直接 SQL 预置旧行。
- [ ] 保留内容、文件、特征、故障、outbox、ACK 和快照接口，不能为了删恢复破坏同步。
- [ ] 运行：

  ```powershell
  $env:CARGO_TARGET_DIR='C:\tmp\rust-v2-core-scope-target'
  cargo test -p dedup-node-store --locked -- --test-threads=1
  ```

  Expected: NodeStore 全量通过；恢复测试被“启动清空且长期事实不变”行为测试替代。

### Task 2：让 RuntimeTaskRegistry 成为唯一任务事实

**Files:**

- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/node-engine/src/server.rs`
- Modify: `crates/desktop-core/src/runtime_tasks.rs`
- Modify: `crates/desktop-core/src/app.rs`
- Replace: `crates/desktop-core/tests/runtime_tasks_e2e.rs`
- Modify: `crates/node-engine/tests/worker_pipeline.rs`

- [ ] 写 RED：SQLite 预置 running 任务后启动 Node，首次任务列表必须为空，不能出现 `recovery` 类型或新 Runtime ID。
- [ ] 写 RED：Node 返回空完整任务列表时 Desktop 必须替换并清空该节点旧任务，而不是保留 stale 行。
- [ ] 写真实 MainWindow 事件交错测试：`RuntimeTasksChanged` 与普通 `ViewChanged` 无论先后到达，任务中心、总览最近任务和 running_count 都只能显示同一 Registry 快照，连接 PostgreSQL 或刷新其他页面不得覆盖任务状态。
- [ ] Registry 直接以业务 Task ID 保存类型、状态、统计、阶段、Worker、失败和终态 outbox 高水位；删除 Recovery task kind/stage 和业务 ID 到 Runtime ID 映射。
- [ ] `EngineState` 只增加私有 `completed_scans: BTreeMap<TaskId, CompletedScanSnapshot>`，值只含扫描根、revision、统计、outbox 高水位和成功清单文件身份，不复制全量对象，也不建立 `TaskCatalog`/`ScanSessionCatalog` 类。
- [ ] 计算引擎重启改为：取消当前任务、等待当前 Worker 收束、销毁并重建 Pool；删除 prepare/requeue/restart 三阶段协议。
- [ ] 运行：

  ```powershell
  cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1
  cargo test -p dedup-desktop-core --test runtime_tasks_e2e --locked -- --test-threads=1
  ```

### Task 3：删除非核心协议、诊断界面和配置恢复链

**Files:**

- Modify: `proto/node.proto`
- Modify: `crates/protocol/src/lib.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/server.rs`
- Simplify: `crates/node-engine/src/config_repository.rs`
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-core/src/view_state.rs`
- Modify: `crates/desktop-ui/src/bindings.rs`
- Modify: `crates/desktop-ui/src/models.rs`
- Modify: `crates/desktop-ui/ui/pages/settings-workspace.slint`
- Modify: `crates/central-store/src/schema.rs`
- Modify related protocol/UI/config tests

- [ ] 用 descriptor/真实 UI 行为写 RED，删除 `RetryDeleteItems`、恢复任务字段、文件故障列表/清理管理命令和自动配置重启确认链；不要用读取 `.proto` 文本断言。
- [ ] 保留任务查询/取消、扫描、本地分析、结果窗口、预览、创建删除、同步、快照和必要配置读写协议。
- [ ] 配置保存只允许改现有 `config.toml` 的业务字段：校验后写临时文件并原子替换；不改 bootstrap 路径，不写 journal，不启动替代进程。响应明确“保存成功，重启 Node 后生效”。
- [ ] 删除逐表数据库诊断和文件故障清理 UI；保留节点/PG 连接是否可用、schema 不匹配错误和当前任务失败详情。
- [ ] 更新协议版本；Rust V2 组件成套升级，不提供旧协议兼容。
- [ ] 运行：

  ```powershell
  cargo test -p dedup-protocol --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test config_repository --locked -- --test-threads=1
  cargo test -p dedup-desktop-core --locked -- --test-threads=1
  cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1
  cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
  ```

### Task 4：完成批量缓存分类和枚举前物理盘计划

**Files:**

- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-engine/src/scan/cache_resolver.rs`
- Modify: `crates/node-engine/src/scan/enumerator.rs`
- Modify: `crates/node-engine/src/scan/everything.rs`
- Modify: `crates/node-engine/src/io/scheduler.rs`
- Add focused node-store/node-engine tests

- [ ] 写 RED：1,000 条路径的查询 SQL 数量与项目数无关，查询前后没有 task/stage `INSERT/UPDATE`。
- [ ] 统一完整性判定和缺失掩码：空值、非法长度、失败占位和缺视频槽为缺失；合法全零特征仍命中。
- [ ] 在获取文件列表前把每个扫描根解析为物理盘编号和介质类型；枚举出的路径只按已冻结根映射分 lane，不重复查询物理盘。
- [ ] 一个扫描任务接收多个根；Everything 默认，启动/IPC/完整枚举失败时整次回退 Walker。
- [ ] SQLite 查询只读；插入或更新只在 Worker 结果、扫描收尾和删除成功时执行。
- [ ] 运行：

  ```powershell
  cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test scan_cache --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1
  ```

### Task 5：实现按物理盘 TSV 的真实计算调度

**Files:**

- Create: `crates/node-engine/src/task_file.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Modify: `crates/node-engine/src/io/scheduler.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Add: `crates/node-engine/tests/task_file.rs`

- [ ] 写 RED：完整缓存命中不生成行；两个物理盘的缺失项进入两个文件；dispatcher 必须真实从文件顺序取得 `P` 行。
- [ ] 每 lane 只保留有限预读和一个队首许可请求；公平性只由现有 `DiskReadScheduler` 决定，不复制权重状态。
- [ ] Hash/Media 使用同一联合选择 epoch；额度读取配置，保持全局不足时加权轮转和 HDD 老化保护。
- [ ] Worker 只计算掩码要求的字段。NodeStore 合并结果时不以缺失值覆盖有效缓存；事务 ACK 后原位 `P -> C`，文件级失败 `P -> F`。
- [ ] 基础计算和二筛共用同一任务文件组件；删除所有 SQLite claim/complete/finalize task item 调用。
- [ ] 运行：

  ```powershell
  cargo test -p dedup-node-engine --test task_file --locked -- --test-threads=1
  cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1
  ```

### Task 6：用一次事务收尾扫描并保存当前进程快照

**Files:**

- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-store/src/outbox.rs`
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Add: `crates/node-store/tests/inventory_finalize.rs`
- Add: `crates/node-engine/tests/completed_scan_snapshot.rs`

- [ ] 写 RED：只有枚举完成、缓存批次完成、任务文件封闭、全部行 C/F 且 SQLite ACK 排空后才能 finalize。
- [ ] 为缓存完整命中和 SQLite ACK 成功项顺序生成 `scan-success.tsv`；F 不进入。文件封闭后校验行数和 SHA-256，写入失败不得 finalize。
- [ ] 收尾事务批量写当前文件事实、按路径组件失活本轮未见位置、写 file outbox、推进 library revision 并返回真实 highwater。
- [ ] 任务级失败、取消或枚举失败绝不 finalize；单文件 F 不阻塞其他项和成功收尾。
- [ ] SQLite 收尾提交后才把扫描根、revision、统计、highwater、成功清单路径和 SHA 写入 `EngineState.completed_scans`；同步不修改该映射。新扫描收尾或删除推进 revision 后，旧快照不能用于新分析。
- [ ] 运行：

  ```powershell
  cargo test -p dedup-node-store --test inventory_finalize --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test completed_scan_snapshot --locked -- --test-threads=1
  cargo test -p dedup-node-store --test outbox --locked -- --test-threads=1
  ```

### Task 7：本地分析只发布最近一次结果并使用滑动窗口

**Files:**

- Modify: `crates/node-engine/src/analysis/mod.rs`
- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Create: `crates/node-engine/src/analysis/result_file.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/desktop-core/src/results.rs`
- Modify: `crates/desktop-ui/src/bindings.rs`
- Modify: `crates/desktop-ui/src/models.rs`
- Modify result/review Slint components and tests

- [ ] 写 RED：本地分析不能读取旧 SQLite task/analysis run；只能选择当前 `completed_scans` 且 revision 一致的 Task ID，并校验成功清单后连接 SQLite 当前 active 文件、排序去重并冻结输入。
- [ ] 候选、二筛待办、分组和复核只在当前运行内存；删除 `retry_phase2`。失败/取消/Incomplete 删除 partial，不覆盖旧成功结果。
- [ ] 以固定 TSV 写 `latest-analysis.partial.tsv`，flush、`sync_all`、关闭后原子替换 `latest-analysis.result.tsv`；任意时刻只保留最近一次成功结果。
- [ ] 结果读取器不分页、不建 `.idx`；顺序校验 TSV 和 footer SHA，内存只缓存行偏移及当前可见前后窗口。
- [ ] UI 删除上一页/下一页/加载更多，只按滚动位置请求窗口。结果 revision 旧时仍可只读查看，但复核和删除禁用。
- [ ] 运行本地分析、结果文件、bindings、window 和 offscreen layout 行为测试。

### Task 8：简化跨机器分析并保护必要同步

**Files:**

- Modify: `crates/desktop-core/src/analysis/mod.rs`
- Modify: `crates/desktop-core/src/analysis/task.rs`
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-core/src/sync.rs`
- Modify: `crates/central-store/src/analysis.rs`
- Modify: `crates/central-store/src/cross_analysis.rs`
- Modify: `crates/central-store/src/stages.rs`
- Modify: `crates/central-store/src/content.rs`
- Modify: `crates/node-store/src/snapshot.rs`
- Modify related central/cross-analysis tests

- [ ] 写 RED：重启 Desktop、Node 任务消失或旧运行未完成时，协调器不能 `resume`；Retry UI 创建新 run ID 并从 collecting 开始，或直接移除按钮。
- [ ] 删除 `resume`、`retry_unresolved` 和未完成运行恢复。运行过程可用内存状态，只有完整最终结果提交 PostgreSQL 后才对 UI 可见。
- [ ] Desktop 跨机器分析只读取 PostgreSQL 已同步的内容、当前文件和媒体特征；不得请求或读取 Node 本地分析结果。
- [ ] 同步 outbox 只包含内容、当前文件 active 状态和媒体特征。删除通过 file active=false 同步，不写/同步删除墓碑历史。
- [ ] 保留 PG 提交后 ACK、1,000 项增量、断线重放、cursor 追赶和 SnapshotRequired；快照必须包含 inactive 文件当前事实。
- [ ] 运行：

  ```powershell
  cargo test -p dedup-central-store --locked -- --test-threads=1
  cargo test -p dedup-desktop-core --test cross_analysis --locked -- --test-threads=1
  cargo test -p dedup-desktop-core --test sync_batches --locked -- --test-threads=1
  cargo test -p dedup-desktop-core --test sync_snapshot --locked -- --test-threads=1
  cargo test -p dedup-node-store --test outbox --locked -- --test-threads=1
  ```

### Task 9：实现瞬态删除队列并移除删除历史

**Files:**

- Create: `crates/node-engine/src/delete_task_file.rs`
- Modify: `crates/node-engine/src/delete.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-store/src/outbox.rs`
- Modify: `proto/node.proto`
- Modify node/desktop/UI deletion tests

- [ ] 写 RED：创建删除后生成一份固定 TSV，执行器一次只处理一行；SQLite `delete_batches/delete_items/deletion_tombstones/review_marks` 不新增产品行。
- [ ] 创建队列前核对最新分析 ID、revision 和每组至少一个 Keep；复核决定只在当前进程内存。
- [ ] 每行执行前重新验证 active LocationKey、实际大小和流式 MD5；验证失败标 F 并继续。
- [ ] 文件系统成功后事务更新 `files.active=0`、file outbox 和 revision；ACK 后标 C。提交失败标 F、任务明确失败并要求重新扫描，不伪装完成。
- [ ] 终态删除 queue；Node 启动删除遗留 queue。移除 RetryDeleteItems 和跨重启续删。
- [ ] 运行删除、回收站、outbox、协议和 UI 行为测试。

### Task 10：回归、遥测和一次真实媒体全量验收

**Files:**

- Modify: `crates/desktop-core/examples/runtime_acceptance.rs`
- Modify: `crates/desktop-core/tests/runtime_acceptance_contract.rs`
- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
- Modify related Windows harness/report tests
- Create: `docs/verification/2026-08-28-core-scope-and-transient-runtime.md`

- [ ] 更新验收客户端：任务终态即最终化，1,800 秒仅为超时；记录缓存批量吞吐、每盘 TSV P/C/F、Worker、CPU、物理盘 I/O、SQLite 运行态写入数和结果 SHA。
- [ ] 运行 node-store、node-engine、protocol、central-store、desktop-core、desktop-ui 全量测试，以及 Windows harness/report/package 测试、`cargo fmt --all -- --check`、`git diff --check`。
- [ ] 使用 Worker 20、总读取线程 12、Everything 和两个只读媒体根运行一次全量测试：

  ```powershell
  pwsh -NoProfile -File tests\windows\Measure-RustV2RuntimeAcceptance.ps1 `
    -MediaRoot @('H:\pik\00000000000','I:\tmp') `
    -DurationSeconds 1800 -SampleSeconds 2 `
    -WorkerCount 20 -TotalReadThreads 12 `
    -Enumerator everything -CompleteWhenTaskTerminal `
    -ReleaseRoot '<candidate-release-root>' `
    -AcceptanceClientPath '<runtime-acceptance-exe>'
  ```

- [ ] 门禁：任务到终态即结束；两个物理盘 ready 区间存在设计允许的并发；额度来自配置且无 HDD 饥饿；缓存命中不入 TSV；SQLite 旧任务/分析/删除表产品写入为 0；同步和最终结果正确。
- [ ] 不进行六轮 A/B、A-3 或重复真实全量测试；小型固定 fixture 验证第二次缓存命中即可。

### Task 11：最终审查、更新设计书并构建候选包

**Files:**

- Modify last: `AGENTS.md`
- Finalize: `docs/verification/2026-08-28-core-scope-and-transient-runtime.md`

- [ ] 使用 `gpt-5.6-sol`、`max` 做一次只读最终审查，只检查五个边界：旧恢复入口是否仍可达、TSV 是否为实际调度源、同步正确性是否保留、Desktop 是否依赖 Node 本地分析结果、删除是否还能写历史或恢复。
- [ ] Important 以上且有真实行为证据的问题按 TDD 修复并复跑受影响门禁；不扩大到新功能。
- [ ] 所有实现与验证通过后，最后更新 `AGENTS.md`：删除旧任务/分析/删除恢复和非核心诊断描述，写入已落地的数据所有权、TSV 格式、重启语义、跨机器边界、删除队列和验证命令。不得提前把计划当成已实现事实。
- [ ] 构建并验证候选正式包：

  ```powershell
  pwsh -NoProfile -File scripts\build-release.ps1 -CargoTargetDir C:\tmp\rust-v2-core-scope-release-target
  pwsh -NoProfile -File scripts\verify-release.ps1 -Package dist-rust-v2\mySingerServer-rust-v2-win-x64.zip
  ```

- [ ] 记录包 SHA-256 和 manifest；正式包仍只包含 desktop/node/worker/Everything 四个顶层 EXE。不部署、不替换 `I:\Tool`。

## 五、最终闭环清单

- [ ] 1,000 项基础缓存查询是固定数量批量 `SELECT`，查询阶段任务/阶段写入为 0。
- [ ] 完整缓存命中不生成任务行，部分命中只计算真实缺失字段。
- [ ] 枚举前确定物理盘编号和类型；每盘独立 TSV 是实际调度来源，额度和权重读取配置。
- [ ] SQLite ACK 前计算行保持 P，成功后 C，文件失败 F；Worker 崩溃不阻塞其他项。
- [ ] 扫描只在完整成功边界提交当前文件、outbox、revision 和完成快照。
- [ ] 完成扫描快照只引用已校验成功清单；缓存命中和成功计算项在内，文件级失败和旧 SQLite 位置不在。
- [ ] Node 重启后任务列表、完成扫描映射、未完成分析和删除队列为空；长期缓存仍保留。
- [ ] `RuntimeTaskRegistry` 是唯一任务事实；没有 TaskCatalog、ScanSessionCatalog、Recovery task 或 Runtime ID 映射。
- [ ] 任务中心和总览最近任务始终来自同一 Registry 快照，普通视图或数据库连接事件不会造成状态来回切换。
- [ ] 本地分析只保留最近一次成功 TSV；不分页、不用 JSON、不建 `.idx`。
- [ ] Desktop 跨机器分析只使用同步后的 PostgreSQL 事实，不读取 Node 本地分析结果，不恢复旧协调器。
- [ ] 删除使用顺序 TSV；不保存批次/项/墓碑历史，不续删；成功更新当前文件事实和 outbox。
- [ ] 数据库表诊断页、文件故障清理页、复杂配置恢复链和旧重试入口已删除。
- [ ] outbox/ACK/cursor/snapshot、当前运行 Worker 崩溃隔离和安全删除复核仍通过行为测试。
- [ ] 未加入磁盘满自动清理；空间不足时当前任务明确失败。
- [ ] 一次双物理盘真实媒体全量验收完成，无六轮重复跑测。
- [ ] 最终审查、全量回归、格式检查和包验证通过后，`AGENTS.md` 已按实际落地结果更新。
