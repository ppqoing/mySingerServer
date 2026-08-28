### Task 7：基础计算改用瞬态 TSV，并原子收尾扫描清单

**目标：** 删除基础计算对 SQLite `tasks/task_items/task_stages` 的生产依赖。缓存完整命中项直接进入本轮已见/已解析清单；只有真正缺少计算数据的项才写入按物理盘拆分的 TSV，并由 Task 6 的唯一 dispatcher 调度。SQLite 事务提交成功后任务行才从 `P` 改为 `C`；单文件失败改为 `F`。全部生产、计算和 ACK 收束后，以一个 SQLite 事务提交当前扫描清单并返回当前进程快照。

本任务不实现任务恢复、`TaskCatalog`、分析结果、二筛任务、删除队列、分页、`.idx`、磁盘满清理、SSD 识别或真实媒体测试。Node 重启后的统一运行目录清理和进程内完成扫描目录放到 Task 8；本任务只负责当前扫描 run 的成功/失败/取消生命周期。

为避免同时修改 NodeStore 与大体量流水线，按三个顺序提交实施：

1. **Task 7A：** `NodeStore::finalize_scan_manifest` 的清单临时表和单事务。
2. **Task 7B：** 基础缓存分类、TSV 生产、dispatcher、Worker/SQLite ACK。
3. **Task 7C：** actor 当前进程快照、成功/失败/取消清理和生产回归。

## Task 7A：扫描清单单事务收尾

**Files:**

- Create: `crates/node-store/src/inventory.rs`
- Modify: `crates/node-store/src/lib.rs`
- Modify only when helper visibility is required: `crates/node-store/src/content.rs`
- Modify only when helper visibility is required: `crates/node-store/src/outbox.rs`
- Create: `crates/node-store/tests/inventory_finalize.rs`

**接口：**

```rust
/// 本轮已经得到完整内容键的扫描文件。
pub struct ResolvedScanFile {
    pub scanned: ScannedPath,
    pub content: ContentKey,
}

/// 当前扫描收尾所需的全部内存清单。
pub struct ScanFinalizeInput {
    pub roots: Vec<NormalizedPath>,
    pub seen_paths: Vec<NormalizedPath>,
    pub resolved_files: Vec<ResolvedScanFile>,
}

/// 扫描清单事务提交后的同步边界。
pub struct ScanFinalizeResult {
    pub outbox_high_seq: u64,
    pub library_revision: u64,
}
```

**固定语义：**

- `seen_paths` 包含所有成功枚举的路径，包括本轮读取或 Worker 失败的文件；它只防止旧活动位置被误失活。
- `resolved_files` 只包含结构完整的路径缓存命中、内容缓存命中或 Worker 结果已成功提交 SQLite 的文件；失败占位、默认值和空字段不能进入。
- 先在同一连接建立并清空 TEMP `seen_paths`/`resolved_files`，按最多 1,000 行批量写入；再开启一个正式事务执行内容/位置关系合并、按路径组件失活旧位置、file outbox、高水位读取和 `library_revision` 单次推进。
- `D:\A` 只影响其路径组件范围，不影响 `D:\AB`。重复根、路径和 resolved 行在入口规范化、排序、去重；同一路径对应不同 `ContentKey` 必须拒绝。
- 任一步失败整笔回滚：活动位置、outbox 和 revision 均不变化。取消、枚举失败或任务级错误由调用方不调用该接口，不在 Store 内伪造取消参数。

**TDD：**

1. 新接口缺失的编译 RED。
2. 真实 SQLite 覆盖完整命中/计算成功写关系与 outbox。
3. 覆盖 seen 但未 resolved 的旧活动路径不失活。
4. 覆盖 `D:\A`/`D:\AB`、根内未见位置失活、根外位置保持。
5. 注入事务中途失败，断言 outbox/revision/活动位置均回滚；成功一次只推进一次 revision。

```powershell
cargo test -p dedup-node-store --test inventory_finalize --locked -- --test-threads=1
cargo test -p dedup-node-store --test outbox --locked -- --test-threads=1
cargo test -p dedup-node-store --locked -- --test-threads=1
```

## Task 7B：缓存分类到 TSV 的唯一生产和调度链

**Files:**

- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/scan/engine.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/task_files.rs`
- Modify: `crates/node-engine/src/task_dispatch.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`
- Modify: `crates/node-engine/tests/scan_cache.rs`

**结果类型：**

```rust
/// 当前进程内可供后续分析读取的完成扫描快照。
pub struct CompletedScanSnapshot {
    pub task_id: TaskId,
    pub roots: Vec<NormalizedPath>,
    pub resolved_files: Vec<ResolvedScanFile>,
    pub outbox_high_seq: u64,
    pub library_revision: u64,
}

/// 基础计算完成结果。
pub struct ScanRunResult {
    pub summary: ScanSummary,
    pub completed: CompletedScanSnapshot,
}
```

实际入口可在不扩大依赖的前提下保留现有参数形状，但必须显式获得 `runtime_root`、冻结的 `PlannedScannedPath`、唯一 `DiskReadScheduler` 和取消令牌。

**固定生产链：**

1. 对枚举行按 Task 4 的批量缓存接口分类；查询前和运行全程不得创建或更新 SQLite 任务/任务项/阶段。
2. 完整路径缓存命中不创建 TSV 行，直接累计 cache hit、`seen_paths` 和 `resolved_files`。
3. 已知 MD5 的部分命中只写真实 `BASE_MISSING_*`；未命中路径只写 `TASK_NEEDS_MD5`。掩码只能使用 `TaskWorkMask` getter/构造器，不在 BaseCompute 重复解释位布局。
4. 每批分类后立即按冻结 lane `append_batch`；生产完成 `seal`。append/flush/文件损坏属于任务级错误，停止本轮且不收尾。
5. 真实工作只来自 `TaskFileDispatcher::next` 返回的记录与唯一 permit。删除 `remaining_rows`、`claim_next_item` 和 SQLite queued/running 就绪判断；读取器不得再次 acquire。
6. `TASK_NEEDS_MD5` 使用一个 `WorkerFileSession` 计算 MD5，按批查询内容缓存；内容完整命中只提交路径关系，仍缺字段时同一会话继续媒体计算，并只下发 `decision.missing_parts()`。
7. 持久化 actor ACK 成功后才 `mark_completed`；ACK 失败保持 `P` 并使任务级运行失败。单文件读取/Worker 崩溃只 `mark_failed`，其他 lane 继续。
8. 所有 TSV 行 `C/F`、dispatcher/Worker/persist ACK 收束后调用 7A；成功形成 `CompletedScanSnapshot`，然后由唯一 owner 删除当前 run 目录。

actor 的 `create_scan` 当前会在枚举和缓存查询前调用 `begin_scan_task`、`initialize_base_task_stages`；7B 必须同时改为只登记 `RuntimeTaskRegistry` 并启动内存 background job，否则“查询前零 task SQL 写入”不能成立。协议仍返回同一业务 Task ID，不新增持久目录或恢复层。

**同一行的 Hash→Media 续算：**

- `TASK_NEEDS_MD5` 行在 Hash permit 释放后仍保持原始 TSV 字节和 `P` 状态，不原位扩写 known MD5，也不追加第二行。
- MD5、内容缓存决策和后续缺失掩码保存在该任务身份的短生命周期内存上下文。
- dispatcher 提供受控的同身份 `MediaDecode` 再许可入口：先核对 run/lane/item/offset 与仍在途的原始 `P` 行，再向同一 provider/scheduler 申请一次 Media permit；不得通过 reader 二次申请，也不得另建 fairness 状态。
- 内容缓存已完整时不再申请 Media permit；只提交位置关系并在 ACK 后标 `C`。

**lane 并发修正：**

- “每 lane 至多一个 permit future”只限制等待 scheduler 的队首申请，不限制已经取得 permit 的任务数量。
- `TransientTaskFileSet` 必须把单个 `in_flight: Option<_>` 收敛为按完整 identity 管理的有界在途集合；permit 成功取走第一行后，dispatcher 可以继续观察同 lane 下一行，实际并发仍由 scheduler 的 `per_disk_limit` 和 global limit 控制。
- 同一 identity 只能在途一次，ACK/F 只移除精确 identity；`all_terminal`/`discard` 要求整个在途集合为空。

**必须删除的基础计算调用：**

- `reserve_scan_path`
- `queue_scan_item_for_read`
- `claim_next_item`
- `complete_item_guarded`
- `finalize_scan_task_from_items`
- 由 `remaining_rows` 推导 readiness 的分支

这些 API 可暂时留给尚未迁移的其他调用方，但基础计算生产调用必须归零；后续任务再删除无调用接口。

**TDD：**

- 1,000 个完整路径缓存命中：SQLite task SQL 写入 0、`.tasks.tsv` 0、cache_hits=1,000。
- 三项分类：完整命中无行；已知 MD5 缺联系表只含对应 bit；路径未命中 MD5 空且只有 `TASK_NEEDS_MD5`。
- persist gate：ACK 前磁盘首字节仍为 `P`，ACK 后为 `C`。
- Worker 崩溃：对应 lane 行为 `F`，另一物理盘行继续并成为 `C`。
- dispatcher permit 是唯一读取许可；Hash 后继续 Media 时按同一冻结 lane reacquire 一次，不通过 `ScheduledFileReader` 双重申请。
- SSD lane 额度 5、global 足够且至少 6 行时，可同时持有 5 个已派发任务；第 6 个必须等待 permit 释放，证明没有被 TSV owner 人为串行化。
- 现有 Hash/Media 3:1、跨盘配置权重、老化、Worker admission、credit 和 persist drain 行为保持。

```powershell
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --test scan_cache --locked -- --test-threads=1
cargo test -p dedup-node-engine --test base_compute_utilization --locked -- --test-threads=1
cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1
cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1
```

## Task 7C：actor 快照和当前 run 生命周期

**Files:**

- Modify: `crates/node-engine/src/actor.rs`（仅成功快照与当前 run 终态 plumbing；查询前 task SQL 移除已在 7B 完成）
- Modify only if current scan return plumbing requires it: `crates/node-engine/src/server.rs`
- Add or modify actor/runtime behavior tests under `crates/node-engine/tests/`

**固定语义：**

- `RuntimeTaskRegistry` 仍是运行中任务状态、统计和失败的唯一内存事实；不建立持久 `TaskCatalog`。
- actor 只接收成功 `ScanRunResult.completed`，供 Task 8 的进程内扫描目录使用；Node 重启后该内存为空，不恢复旧任务或旧 TSV。
- 成功：finalize 提交并生成快照后删除精确 `<runtime>/<run-id>`。
- 取消/任务级失败：先停止生产，取消 pending permit 和 Worker，等待 in-flight/ACK 所有权收束，再删除同一 run 目录；不把剩余 `P` 改 `F`，不调用 finalize。
- 单文件失败不是任务级失败：记录 `F` 后继续；扫描仍可成功收尾，seen 路径不被误失活。
- discard 失败明确使当前任务失败并保留精确目录供当前 owner 重试；禁止宽泛清理 `runtime` 根，禁止登记到磁盘满清理器。
- 不发布 Node analysis 结果给 Desktop，不新增恢复、历史或分页协议。

**TDD：**

- 成功扫描返回快照且 run 目录已删除。
- 取消/枚举失败/TSV append 失败均无 revision/outbox 推进，精确 run 目录最终删除。
- file failure 仍成功 finalize；失败路径仍 active，成功邻居进入 snapshot。
- actor/RuntimeTaskRegistry 只发布当前 Task ID 和终态，重启不恢复。

```powershell
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --test actor --locked -- --test-threads=1
cargo test -p dedup-node-engine --lib --locked -- --test-threads=1
cargo test -p dedup-node-store --locked -- --test-threads=1
cargo fmt --all -- --check
git diff --check
```

## 执行与审查边界

- 7A、7B、7C 严格顺序实施；同一生产文件不允许多个写代理并发修改。
- 每段先固定真实行为 RED，再做最小 GREEN；禁止 `read_source/contains/matches` 类源码检测测试。
- 方法、类型、字段和重要局部变量使用简洁中文注释说明用途、用法和所有权；不为未来恢复或兼容预留抽象。
- 每段完成后只做一次独立窄审查，修复 Critical/Important 后继续，避免重复扩大审查。
- 所有 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，关闭 incremental/debug info并清除 MinGW 环境变量。C 或 D 低于 10 GiB 时停止新的重型命令，只清理精确确认可再生的项目 target 后继续。
- 本任务不运行真实媒体、不打包、不部署、不触碰 `I:\Tool`。
