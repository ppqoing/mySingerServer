# SDD ledger — plan: D:/code/mySingerServer/docs/superpowers/plans/2026-08-28-transient-task-files-and-latest-analysis.md

Workspace: `D:/code/mySingerServer/.worktrees/core-scope-transient-runtime`
Branch: `codex/core-scope-transient-runtime`
Merge base: `d097aed91da1176303618e64164d1ba35bdce112`
Spec: `D:/code/mySingerServer/docs/superpowers/specs/2026-08-28-transient-task-files-and-latest-analysis-design.md`

## Preflight self-consistency

| Task | Tests against implementation | Files and outputs | Result |
|---|---|---|---|
| 1 | Runtime-state boundary tests exercise real SQLite reopen and retained long-term facts | NodeStore runtime tables/APIs | API deletion is coupled to Task 2; ruling below |
| 2 | Real Node startup, WorkerPool restart and Desktop list replacement | Registry, actor, pool, Desktop task model | Consistent |
| 3 | Descriptor, config repository and real UI behavior tests | Protocol, actor/server, config, settings UI | Consistent; protocol removals must precede Task 9 final protocol shape |
| 4 | SQL trace/cache fixtures and disk scheduler behavior | Cache resolver, enumerators, scheduler | Consistent |
| 5 | Real TSV, BaseCompute and disk scheduler tests | Shared task file for base and stage2 | Consistent; Task 6 consumes sealed output |
| 6 | Real scan finalization/outbox/snapshot behavior | Success manifest and completed-scan metadata | Consistent |
| 7 | Real local analysis/result/window UI behavior | Latest result TSV and in-memory run state | Consistent |
| 8 | Real cross-analysis and sync tests | Desktop coordinator, central facts, snapshot | Consistent; must preserve sync recovery mechanisms |
| 9 | Real delete queue/filesystem/store behavior | Delete TSV, current file fact, protocol/UI | Consistent |
| 10 | Contract, harness and one real-media acceptance | Telemetry/report/evidence | Consistent; no repeated A/B |
| 11 | Final review, actual-state AGENTS update and package verifier | Docs/evidence/package | Consistent; AGENTS must not be updated early |

## Shared-file and interface scan

| Tasks | Producer → consumer | Finding |
|---|---|---|
| 1 → 2 | NodeStore runtime boundary → Node actor startup | Removing recovery APIs in Task 1 would break NodeEngine before Task 2 |
| 2 → 3 | Actor/server task identity → simplified protocol and config | Task 3 must build on direct business Task ID |
| 2 → 7 | `RuntimeTaskRegistry` → completed scan catalog | Task 2 does not prebuild a catalog; the base TSV finalizer creates the single process-local owner together with its first valid snapshot |
| 3 → 9 | Protocol pruning → final delete queue protocol | Task 3 removes retry/history messages; Task 9 keeps one-shot create/query behavior |
| 4 → 5 | Cache missing masks and disk lanes → TSV producer/dispatcher | Exact mask and lane types must remain single-source |
| 5 → 6 | Base persistence ACK and sealed task files → scan finalizer | Finalize only after all C/F and ACK drain |
| 5 → 7 | Shared stage2 task file → local analysis phase2 | Task 7 cannot recreate SQLite task persistence |
| 6 → 7 | Success manifest metadata → local analysis input freeze | Analysis validates revision, manifest hash and current active facts |
| 7 → 8 | Local result ownership → Desktop cross-result ownership | No Node local result crosses into Desktop coordinator |
| 8 → 9 | Current-file active sync → successful deletion outbox | No tombstone history; inactive file snapshot remains authoritative |
| 9 → 10 | Delete/task/result telemetry → acceptance report | Acceptance observes final shape only |
| 10 → 11 | Verification evidence → AGENTS and package | Design book records only verified implementation |

## Rulings

- Task 1: Ruling: retain call-compatible NodeStore recovery APIs temporarily but stop startup recovery and add boundary tests; Task 2 removes all callers and then deletes the APIs — every intermediate commit remains compilable — cost if wrong: Task 1 alone does not yet satisfy the final no-recovery API surface.
- Task 1 plan-gap review: the first brief omitted `library_revision`; Task 1 is not complete until schema 3 initialization/strict parsing, `NodeStore::library_revision()` and the transaction-only bump helper are implemented and independently verified.
- Preflight: Ruling: the approved plan/spec in the dirty main checkout are the binding inputs for task briefs; copy their final versions into this feature branch during Task 11 before updating AGENTS — this protects unrelated main changes — cost if wrong: losing the main checkout would require reconstructing the approved documents from the SDD artifacts.

## Baseline

- Build environment root cause: inherited `CC/CXX` pointed to MinGW while Rust target was MSVC, producing `___chkstk_ms/__isnan` link failures in bundled SQLite. Cleared those variables for Cargo commands and rebuilt only this plan's target cache.
- `cargo test -p dedup-node-store --locked -- --test-threads=1`: 43 passed, 0 failed (baseline `d097aed9`).

## Read-only dependency maps

- Task 2: `.superpowers/sdd/2026-08-28-transient-task-files-and-latest-analysis/task-2-map.md` — direct TaskId registry, empty-list replacement, no recovery task, simple WorkerPool rebuild.
- Task 3: `.superpowers/sdd/2026-08-28-transient-task-files-and-latest-analysis/task-3-map.md` — remove retry/fault-management/restart-confirmation chains while retaining schema availability and necessary config.

## Review ledger

- Task 1 initial review: Critical — `deletion_tombstones` was incorrectly retained across startup.
- Task 1 fix round 1: commit `61f05792`; scoped re-review Approved. Startup now clears tombstones in the same transaction and the real SQLite boundary test classifies them as transient.
- Task 1 plan-gap round 2: commit `ff8d2da0`; implemented strict `library_revision` initialization/read/bump boundary. Review found only missing empty-string coverage.
- Task 1 fix round 3: commit `b434de67`; empty-string SQLite metadata case covered; scoped re-review PASS.
- Task 1 final controller verification: `cargo test -p dedup-node-store --locked -- --test-threads=1` passed (4 unit + 40 integration, 0 failed); `cargo fmt --all -- --check`, range `git diff --check`, and clean worktree passed. Accepted range: `d097aed9..b434de67`.
- Plan consistency review: current SDD sequencing is usable only after restoring omitted formal-plan boundaries. Adopt the 11-step sequence: SQLite boundary+revision; runtime single fact; protocol/config/UI slimming; cache completeness; pre-enumeration lane freeze; TSV substrate; base TSV+finalize+snapshot; stage2 TSV+catalog; latest analysis TSV+in-memory analysis; reader/wire/Slint window; delete zero-history+acceptance/docs.
- Task 2 accepted range: `b434de67..35094db6`. Runtime registry now uses business IDs only; Node startup publishes no recovered task; Desktop replaces a node's full task list; planned requeue/recovery APIs are removed; Worker restart cancels the active job, joins every slot driver with a bounded deadline, releases the old Job, then rebuilds from preserved production config.
- Task 2 review rounds closed: stale Desktop rows, detached driver ownership, unbounded cancel, and late retired-slot events were each reproduced by behavior tests and fixed. Final independent review Approved with no Critical/Important/Minor findings.
- Task 2 final controller verification on `35094db6`: NodeEngine lib 64/64, serial `base_compute_pipeline` 59/59, real worker fixture 3/3, Desktop controller 7/7, Desktop runtime e2e 1/1, NodeStore 44/44, UI bindings 16/16, `cargo fmt --all -- --check`, `git diff --check`, and clean worktree all passed. The report retains the non-gating default-parallel timing failures instead of claiming that run passed.
- Task 3 protocol ruling: keep `PROTOCOL_VERSION=5`. Replace only the old restart-coupled config request/response names with ordinary save semantics in the existing config payload slots; remove retry/fault-management payloads; do not allocate result-window tag 46 until the later window task.
- Task 3 accepted range: `35094db6..196575c3`. V5 keeps tags 39/40 for ordinary `SaveNodeConfig/NodeConfigSaved`; retry/file-fault management and restart/reconnect confirmation chains are removed while runtime failures, NodeStore file faults, central schema validation/sync/cross-machine analysis, resolved Node paths and tray compute-engine restart remain. Independent review found one machine-identity race; `196575c3` added the real A-to-B same-index reconnect gate and scoped re-review Approved.
- Task 3 final controller verification: protocol 21 tests, config repository 8/8, Node actor 7/7, server 3/3, Desktop config controller 4/4 and e2e 2/2, Node lifecycle 2/2, UI bindings 15/15, offscreen layout 16/16, window contract 21/21, central store 3 passed with one environment-gated PostgreSQL test ignored, formatting and diff checks all passed.
- Task 4 brief: `.superpowers/sdd/2026-08-28-transient-task-files-and-latest-analysis/task-4-brief.md`. It binds one structural completeness classifier, fixed-count 1,000-row batch SELECTs, legal zero-valued hits, per-slot video stage2 masks, decodable MD5-derived contact sheets and no failure placeholders.
- Task 4 acceptance: `CacheCompleteness` now classifies structural base/stage2 gaps; path/key batches preserve order, duplicates and size while using three traced SELECTs per 1,000-item batch; video masks, local MD5-derived contact-sheet validation, remote thin adaptation, no-placeholder/field-preservation writes and phase2 missing-slot consumption are covered by behavior tests. Final Task 4 regression: NodeStore 50 tests, content cache 17/17, base pipeline 59/59, local analysis 8/8, worker pipeline 23/23, formatting and diff checks passed.
- Task 4 follow-up acceptance: review指出的中心二筛视频槽位重复下发、畸形 stage1 被消费、partial stage1 outbox 降级均以真实行为测试先 RED 后 GREEN；最终 content cache 18/18、local analysis 10/10、base pipeline 59/59、worker pipeline 23/23、node-engine lib 64/64、central-store 测试通过，`cargo fmt --all -- --check` 与 `git diff --check` 通过。phase2 现在只请求成功槽位缺失掩码与中心成功槽位的交集，NodeStore outbox 在事务内编码合并后有效值；仅为既有 actor Worker 屏障夹具补充 `base_complete` 前置，不放宽生产门禁。
- Task 4 follow-up 2 acceptance: 空交集行为先以旧实现 `calls=1` 的真实 RED 固化，随后只重发中心请求且本机已有的完整视频槽位，零 Worker 并提交成功任务；公开二筛重发统一经过 `CacheCompleteness` 门禁。图片/视频槽位 Quality 101..255 作为缺失输入保留既有合法值和 outbox，合法 0 不受影响；负视频 duration 不再转换为巨大 `u64`，快照负槽位时间明确报错。最终 content_cache 22/22、NodeStore 55/55、base 59/59、local 11/11、worker 23/23、central_cache 1/1、node-engine lib 64/64、central-store 3 通过且 1 忽略，格式和 diff 检查通过；C=18.06--18.17 GiB、D=10.31 GiB，未触及停止线。
- Task 4 follow-up 3 acceptance: 快照混合有效/负 duration、负 frame time 的真实 RED 证明旧实现会发出缺失 payload 或阻断整页；GREEN 改为在读取边界跳过两类负值，邻居分页和游标稳定完成。远端完整六槽导入 RED 观察到未请求槽污染及 preexisting 全量重复；GREEN 在远端调用前冻结本地字段，仅持久化 `expected ∩ missing`，并只选择性重发冻结槽。新增真实 outbox 类型/槽位/计数测试覆盖请求 `[0]` 与预存 `[0]`+导入 `[1]`，均 Worker=0、无 2..5。验证：快照 1/1、远端 2/2、content cache 22/22、NodeStore 全量、local 11/11、base 59/59、worker 23/23、node-engine lib 66/66、central-store 3 通过且 1 忽略，fmt/diff 通过；C 约 18.2 GiB、D 约 10.3 GiB，未触及停止线。
- Task 4 follow-up 4 acceptance: 中心增量/快照特征 UPSERT 以旧值保护可空字段，`decoded=false` 不降级既有 true，非法 Quality 先按缺失处理而合法 `0` 保留；空二筛载荷可初次落库并由完整载荷补齐，`base_complete` 只单调变为 true。新增 PostgreSQL 表级行为夹具和 phase2 正式 payload 解码断言；本机缺少 `DEDUP_TEST_POSTGRES_URL`，中心集成测试保持 1 项 ignored（未伪报 PASS）。验证：phase2 2/2、content cache 22/22、NodeStore 全量、local 11/11、base 59/59、worker 23/23、node-engine lib 66/66、central-store 3 通过/2 ignored，fmt/diff 通过；C=17.95 GiB、D=10.31 GiB，未触及停止线。
- Task 4 follow-up 5 acceptance: 中心 PostgreSQL 行为测试改用 UUID 派生唯一 MachineId/ContentKey/LocationKey；异步 case 以 Result 传递核心错误，独立清理守卫无论 case 成功/失败均清除位置、墓碑、特征、内容、cursor 与节点后再返回原错误。连续两次 content_upsert 均明确 1 ignored（缺少 DEDUP_TEST_POSTGRES_URL），central-store 全量 3 通过/2 ignored，fmt/diff 通过；C=17.79 GiB、D=10.23 GiB，未触及停止线。
- Task 4 final independent review: `4f4fa227` 获得 Approved。控制 Agent 在当前提交独立重跑 content cache 22/22、NodeStore 55/55、local analysis 11/11、base pipeline 59/59、worker pipeline 23/23、NodeEngine lib 66/66；central-store 3 passed、2 个 PostgreSQL 环境测试明确 ignored；`cargo fmt --all -- --check` 与 `git diff --check` 通过。Task 4 正式闭环。
- Task 5 brief: `.superpowers/sdd/2026-08-28-transient-task-files-and-latest-analysis/task-5-brief.md`。边界固定为首次枚举前一次解析全部根，枚举行携带冻结物理盘 lane，Hash/Media 读取期不再解析；本提交不创建 TSV、不改变唯一 scheduler，也不提前实现加权 dispatcher。
- Task 5 accepted range: `a976ba05..77304c1e`。独立审查确认 actor 真实执行“全部根解析→枚举→精确 planned lane”，解析失败不调用 enumerator；读取期系统解析旁路已删除，Hash/Media 只消费同一规范路径的冻结 lane；混合 HDD/SSD/Unknown lane 的保守最小额度由唯一 scheduler 对每个底层物理盘实际执行，未提前实现 TSV 或跨盘加权 dispatcher。
- Task 5 final controller verification: storage_device 5/5、scan_roots 11/11、enumerators 4/4、disk_scheduler 27/27、base_compute_pipeline 59/59、NodeEngine lib 66/66、`cargo fmt --all -- --check` 与 `git diff --check` 全部通过。审查仅指出报告残留旧接口描述，已修正为当前“读取期旧接口已删除”的事实。
- Task 6 brief: `.superpowers/sdd/2026-08-28-transient-task-files-and-latest-analysis/task-6-brief.md`。Task 6A 先实现 TSV 单所有者、固定字节、可见边界、有限预读和 `P/C/F`；独立审查通过后，Task 6B 再把每 lane 单队首、配置权重和老化接入唯一 `DiskReadScheduler`。本任务不提前迁移 BaseCompute/actor。
- Task 6A accepted range: `4cf8181b..c36e5ef3`，并补充 `prefetched_len` 的活动状态门禁。固定八列 UTF-8 TSV、全局重复 item 拒绝、flush 后发布、有限预读、精确 identity take、SQLite ACK 后单字节 `P→C`、单文件失败 `P→F`、poison fail-closed、无漏唤醒 publication 和 owner-only discard 已闭环；独立审查 Approved。控制 Agent 当前定向 `transient_task_files` 22/22、fmt/diff 通过；Task 6B 尚未开始。
- Task 5 实施：新增 `ScanDiskPlan`，在生产枚举前一次解析、排序、去重并合并全部根的物理盘 lane；枚举失败路径稳定返回 `SCAN_ROOT_STORAGE_RESOLVE_FAILED` 且不调用枚举器。Hash、Stage1 和 Media 读取许可均消费冻结 lane，不再使用读取期位置解析或可变路径缓存；为保持既有测试边界，物理盘身份改由只读 `physical_disk_id` 提供，旧 `take_physical_disk_id` 保留兼容但不再作为事实来源。真实行为测试先 RED 后 GREEN；存储 5/5、scan_roots 9/9、enumerators 4/4、disk_scheduler 27/27、base_compute_pipeline 59/59、node-engine lib 66/66、fmt/diff-check 全通过。Task 5 尚未提交，待独立审查后合入。
- Task 5 follow-up：按审查反馈补齐混合类型 lane 的冻结最小逐盘额度、复合盘 permit 阻塞/释放行为、枚举器真实顺序边界和逐行精确 lane 所有权。删除生产 `LaneSource::System`、普通 `ScheduledFileReader::new`、读取期存储解析及 `take_physical_disk_id` 事实；生产构造只接收 `PlannedScannedPath` 精确映射。验证：scan_roots 11/11、storage_device 5/5、enumerators 4/4、disk_scheduler 27/27、base_compute_pipeline 59/59、node-engine lib 66/66，fmt/diff-check 通过。待提交 follow-up commit。
- Task 6A 实施：新增固定 TSV 的 `TransientTaskFileSet`，严格校验 UUID v7、八列 UTF-8 无 BOM/LF、路径与掩码，按物理盘 lane 创建全新 run 目录；append/flush 后发布、seal、有限预读、队首领取、完整身份重读和 `P→C/F` 原位状态均由单一文件所有者封装。行为测试首个模块缺失 RED 已实际触发；最终 `transient_task_files` 11/11、node-engine lib 66/66、fmt/diff-check 通过。新增 `task-6a-report.md`；未实现 Task 6B dispatcher/权重，未改 SQLite、协议、actor、BaseCompute、pipeline、UI。
- Task 6A 审查修复：损坏 P 行预读失败不再推进 cursor；append、seal 和状态 `sync_data` 失败会毒化整个 run 并 fail-closed；`peek_lane` 返回拥有型 `TaskLaneHead`，`take_lane` 只接受精确队首身份；同一物理盘的类型、盘号、权重和额度冻结；新增 missed-wakeup-safe publication `Notify`，以及只由集合所有者执行、校验 runtime 直接子目录的 `discard`。先以旧行为固定 RED，修复后 `transient_task_files` 18/18、node-engine lib 66/66、fmt/diff-check 通过；仍未实现 Task 6B 或接入生产计算路径。
- Task 6A 第二轮审查修复：失败 append 通过可 take 的 `BufWriter` 拆分并丢弃缓冲后再逐项 truncate/seek 回滚；lane 打开、clone 或内部一致性 IO 错误毒化 run 并唤醒等待者；discard 删除失败进入 `cleanup_pending`，保留精确路径并允许同 owner 重试，只有成功删除才清理索引；物理盘号入口严格要求 canonical 排序去重。新增行为测试后 `transient_task_files` 22/22、node-engine lib 66/66、fmt/diff-check 通过；仍未实现 Task 6B 或接入生产计算路径。
- Task 6B scheduler follow-up：加权外层先选可运行物理盘 lane，再执行复合盘/T=1/Hash-媒体规则；legacy 入口以权重 1 参加同一轮转，响应发送成功后才提交 deficit/cursor。清理仅重置无加权等待项的 lane deficit，保留 legacy 游标避免饥饿；冻结额度溢出和同 key 权重冲突直接返回配置错误；老化直通计为一次成功并清零该 lane deficit。新增 weighted/T=1/复合、legacy、公平状态失败/取消、重现、老化和错误边界行为测试；`disk_scheduler` 39/39、node-engine lib 66/66、定向 rustfmt/diff-check 通过。详见 `task-6b-scheduler-report.md`；全仓 fmt 仅受另一 agent 未跟踪的 `task_dispatch.rs` 格式差异影响。
- Task 6B 第二轮 scheduler 修复：冻结权重与 deficit 分离，活动 permit 归零前保留同 key 配置；`weighted_mode` 只由当前开放 weighted waiter 决定，全部离开后恢复 legacy 选择；FIFO 清理移除任意位置已取消项并忽略其权重。新增三项复审行为测试均先 RED 后 GREEN；`disk_scheduler` 42/42、node-engine lib 66/66、定向 rustfmt/diff-check 通过。详见 `task-6b-scheduler-report.md`，未改 `task_dispatch.rs`。
- Task 6B scheduler 最终释放顺序修复：permit Drop 先释放全部磁盘/全局计数，再解除 lane 权重冻结并唤醒 actor，消除活动读取尚未完全释放时接受冲突权重的原子窗口；控制 Agent 独立验证 `disk_scheduler` 42/42、node-engine lib 66/66、scheduler rustfmt 与 diff-check 通过。
- Task 6B Dispatcher 实施：新增 `task_dispatch.rs`，由单一 `TaskFileDispatcher` 拥有瞬态任务文件；每 lane 至多一个队首 permit future，只有许可成功后才按完整 identity take，失败/取消保留 `P`；未 seal 空 lane 使用 publication epoch/Notify 等待，sealed 且全部 C/F 后返回 `None`。`SchedulerTaskLanePermitProvider` 仅转换冻结 lane 后调用唯一 `DiskReadScheduler::acquire_lane`，不复制权重、active 或老化状态。预备模块缺失 RED 已实际触发；Dispatcher 10/10、Task6A 22/22、scheduler 42/42 通过；测试临时目录改为拥有型 harness，无 `mem::forget` 泄漏。详见 `task-6b-report.md`；未接入 actor/BaseCompute/SQLite/协议/UI。
- Task 6B Dispatcher 复审修复：`TransientTaskFileSet::discard` 现在拒绝任何 lane 存在未 ACK 的 `in_flight`；新增 `take_lane_exact` 在弹出队首和设置在途前复核完整任务记录，记录变化时保持 `P`、不设置在途并由 Dispatcher 自动释放 permit；清理了重复的 Pending 分支。两项缺陷先以旧实现真实 RED 固化，修复后 task_dispatch 12/12、Task6A 22/22、scheduler 42/42、scan_roots 11/11、base_compute_pipeline 59/59、node-engine lib 66/66 通过，未接入 actor/BaseCompute/SQLite/协议/UI。
- Task 7 brief: `.superpowers/sdd/2026-08-28-transient-task-files-and-latest-analysis/task-7-brief.md`。按 7A 扫描清单单事务、7B 基础缓存到 TSV/dispatcher/ACK、7C actor 当前进程快照与精确 run 清理顺序实施；明确不增加任务恢复、TaskCatalog、分析/删除/分页/磁盘满清理，也不在基础计算保留第二套 SQLite task 事实。
- Task 7A 实施：新增 `NodeStore::finalize_scan_manifest` 及扫描收尾清单类型；TEMP 清单清空/装载、位置关系、组件根失活、file outbox、高水位和 `library_revision` 在一个显式业务事务内完成，完全相同的活动关系不重复写 file outbox。行为测试先 RED 后 GREEN；inventory_finalize 8/8、outbox 6/6、NodeStore 全量 56/56、fmt/diff-check 通过。详见 `task-7a-report.md`；尚未接入 BaseCompute/actor，也未运行真实媒体或部署。
- Task 7A follow-up：stale 位置改为 SQL 根组件过滤并按规范路径游标每批最多 1000 行，resolved 改为 TEMP JOIN `contents/files` 分批读取，复用 prepared 更新和 outbox，避免整机 Vec 与逐项 SELECT；`%` 根名不会误匹配 `D:\AB`。旧实现的 2,001 根外行和 1,001 resolved N+1 RED 已实际触发；修复后 inventory 单元 2/2、inventory_finalize 8/8、outbox 6/6、NodeStore 全量 56/56、定向 rustfmt/diff-check 通过。详见 `task-7a-report.md`；未接入 BaseCompute/actor。
- Task 7B0：`TransientTaskFileSet` 将单个 `in_flight` 改为完整 identity 集合，支持同 lane 多项领取、乱序 ACK、在途期间继续 refill，并由真实 `DiskReadScheduler` 控制 per-disk/global 并发；dispatcher 增加同一 TSV 行 Hash→Media 续算、同 lane 单等待 future、续算优先、失败重试、取消保持 `P` 和精确 abandon。先以缺失接口真实 RED，后验证 transient_task_files 25/25、task_dispatch 18/18、disk_scheduler 42/42、node-engine lib 66/66、fmt/diff-check 全通过。详见 `task-7b0-report.md`；未接入 BaseCompute/actor，也未运行真实媒体或部署。
- Task 7B1：新增 `HashPermitReader` 外部许可 Hash 读取边界，完整 MD5 期间只持有 dispatcher 交付的 `ScheduledReadPermit`；`ScheduledFileReader` 按冻结 `TaskDiskLane` 实现 `TaskLanePermitProvider`，复用唯一 scheduler、权重/逐盘额度和 waiting/active/IO telemetry；裸 `DiskReadPermit` provider 与旧 `PipelineFileReader` API 保留。focused 行为测试 6/6、旧 `scan_runtime_details` ScheduledFileReader 1/1、`task_dispatch` 18/18、BaseCompute ScheduledReader 回归 2/2、fmt/diff-check 通过。详见 `task-7b1-report.md`；尚未接入 BaseCompute/actor，也未运行真实媒体或部署。
- Task 7B1 follow-up：取消等待测试先保存 identity，再取消并清空 waiting，随后释放首个 Hash permit，断言对应物理盘 `waiting=0/active=0/granted=1/released=1`，最后才写入 `F`；TSV 字节保持原样。仅修改 focused 测试与报告，待单独 follow-up 提交。
- Task 7B2A：新增 `NodeStore::commit_scan_stage1_taskless`，在单一 Immediate 事务内合并内容、一筛、联系表与 outbox，不读取或写入 `tasks/task_items/task_stages`；旧 guarded API 保持兼容。`taskless_stage1` 4/4、NodeStore 56/56、fmt/diff-check 通过，独立审查 Approved。详见 `task-7b2a-report.md`。
- Task 7B2B：`BasePersistIdentity` 增加完整 `TaskFileIdentity` 分支，TaskFile identity 在队列 Full/Closed、Store actor 执行与 ACK 间原样传递；旧 `TaskItemIdentity` 构造保持兼容。`base_persistence` 5/5、NodeEngine lib 69/69、base pipeline 59/59、fmt/diff-check 通过，独立审查 Approved。详见 `task-7b2b-report.md`。
- Task 7B2C：新增 `BaseTaskProducer`，以最多 1,000 项的批次分类完整命中、部分命中与路径未命中；只有真实缺失项按冻结物理盘 lane 写入 TSV，完整命中不创建任务文件。所有任务行在首个 lane 发布前复用任务文件规则完整预校验，追加或 seal 失败仍保留精确 run owner 并可 `discard`。focused 11/11、task_dispatch 18/18、transient task files 25/25、NodeEngine lib 66/66、fmt/diff-check 通过，窄复审 Approved。详见 `task-7b2c-report.md`。
- Task 7B2D1：Hash 后的 Media 续算允许同一 TSV identity 在内存中派生 `known_md5` 和真实基础缺失位，同时重新核对 run、lane、offset、length、item、路径、大小、工作类型及原始 `P` 行；派生 mask 必须非空、无 `needs_md5` 且无未知位。TSV 原字节不改、不追加第二行，最终 ACK 只把同一状态字节改为 `C`。task_dispatch 19/19、transient task files 25/25、NodeEngine lib 69/69、fmt/diff-check 通过，窄复审 Approved。详见 `task-7b2d1-report.md`。
- Task 7D3A：新增完整瞬态扫描运行器，串联批量缓存、TSV 生产、Hash/Media 协调、taskless ACK、writer join、扫描清单事务、最终 outbox 发布和精确 run 清理；成功只返回当前进程 `CompletedScanSnapshot`，取消/失败不提交清单且不增加 TaskCatalog/恢复。窄审查发现的末端取消漏检与 writer join 失败目录泄漏已行为修复；模块 8/8、NodeEngine lib 120/120、inventory finalize 8/8、fmt/diff-check 通过。详见 `task-7d3a-report.md`；actor 生产切换留给 D3B。
- Task 7D3B：提交 `dddaac0a`。Node actor 的 `CreateScan` 已切到瞬态扫描运行器和 `data/runtime`，不写 `tasks/task_items/task_stages`；取消、关机和失败不走旧任务表。成功路径先安装唯一 `latest_completed_scan`，再发布 RuntimeTask Completed，保证完成事件观察到同一快照。actor 12/12、NodeEngine lib(test-hooks) 122/122、base pipeline 59/59、Desktop controller 9/9、fmt/diff-check 通过；既有 `cross_phase2` 高水位失败明确留给 Stage2 迁移。
- Task 8A1：提交 `affbbec6`。协议 V5 新增 tag 29 的 `Stage2SourceReadComplete`；WorkerPool 将其作为身份校验后的非终态事件，保留 slot/CPU，直到 `Stage2Result` 才释放。protocol wire 7/7、WorkerPool 定向 3/3、fmt/diff-check 通过；真实 Worker 发送点留给 Task 8A2。
- Task 8B1：提交 `529023d1`。新增 `NodeStore::commit_stage2_taskless`，图片/视频二筛与 outbox 在同一事务提交，不写任务表；非法结果整体回滚，结构完整的合法全零特征仍有效。taskless Stage2 6/6、NodeStore 全量 74/74、fmt/diff-check 通过；任务文件执行与 actor/分析接入留给后续 Task 8 子项。
- Task 8A2：提交 `3d2882b4`。Worker 在二筛源文件或联系表读取结束后发布 `Stage2SourceReadComplete`，后续计算只消费拥有型内存帧；协议进程 RED 为旧实现等待事件超时，GREEN 输出 `WORKER_PROTOCOL_PROCESS_PASS`。详见 `task-8a2-report.md`。
- Task 8C/8B2：提交 `3449b313` 与 `f711cbdc`。二筛先冻结真实缺失选择，再由按物理盘 TSV、唯一读取许可、Stage2 SourceReadComplete 和 taskless SQLite ACK 执行；完整命中零 Worker，视频只计算缺失槽位。随后 `d8fe877b` 修复完整视频命中重发，`83a92f94` 保留精确选择和联系表回退边界。
- Task 8E1：提交 `48277497` 与 `58dbd61f`。RuntimeTask 终态携带真实 outbox 高水位，并提供当前进程完成扫描快照查询；重启不恢复旧 ID。
- Task 8E2：提交 `c547896`。外部 Stage2 batch 改为瞬态内存工作集合，不再创建或推进 `tasks/task_items/task_stages`；本地/远端批量缓存、重发和 RuntimeTask 阶段保留，生产 TSV/唯一 scheduler/actor 接线留给 E3。
- Task 8E3 ruling：先拆成 E3A Phase2 生产编排、再做 E3B Actor 接线，禁止两个实现 Agent 并行修改 `phase2.rs/actor.rs` 共享边界；理由是单一 `NodeStore`、WorkerPool 和取消所有权必须串行定型；若判断错误，代价是多一次集成提交，但不会产生双调度器或双写者。Superpowers `sdd-workspace/task-brief` 在当前 Git Bash 缺少 `basename/dirname`，使用同一 SDD 目录内的等价窄 brief `task-7e3a-brief.md` 继续，保留原 Task 7 为权威。
- Task 8E3A：提交 `4e4895c8`。Phase2 在第一次本地/远端缓存查询前冻结全部来源 lane，完整命中零 TSV/Worker，Compute 只进入唯一 `ScheduledFileReader`、Stage2 task-file runner 与 taskless SQLite ACK；返回恢复后的 Store、内容统计和真实 outbox 高水位。定向 Phase2 通过、NodeEngine lib 135/135、fmt/diff 通过。
- Task 8E3A fix round 1/5：独立审查发现 runtime 目录先于 `BaseStoreActor::finish` 被删除；提交 `8a8f9e27` 以真实 join/discard 顺序 RED 修复成功、runner 失败、writer/highwater 失败路径。复审确认 1 项 addressed、0 项 open；Phase2 9/9、NodeEngine lib 137/137、fmt/diff 通过。
- Task 8E3A complete (commits `c5478969..8a8f9e27`, review clean)。
- Task 8E3B：提交 `1b95a0df`。外部 Stage2 background job 使用瞬态 runtime、唯一读取调度器与 task-file runner；RuntimeTask 终态延后到 Pool、SQLite actor、TSV 与真实 outbox 高水位收束后发布，旧任务表保持空。Actor 16/16、NodeEngine lib 141/141、Desktop cross phase2 3/3、fmt/diff 通过。
- Task 8E3B fix round 1/5：独立审查发现成功扫描与 Restart/Shutdown 交错时可能先发布 Completed 却丢失 `latest_completed_scan`；提交 `7d6188af` 将扫描快照和终态合并为只能消费一次的 `BackgroundOutcome`，正常完成与停止路径均先安装快照再发布终态。定向交错 RED→GREEN，Actor 17/17、NodeEngine lib 142/142、fmt/diff 通过；定向复审确认 1 项 addressed、0 项 open。
- Task 8E3B complete (commits `4072b530..7d6188af`, review clean)。
- Task 9：提交 `ff220933`。新增 Windows 同目录 `MoveFileExW(REPLACE_EXISTING|WRITE_THROUGH)` 原子替换，以及固定 H/M/F 的 UTF-8/LF 最近分析结果 writer/校验器；旧结果在 partial 写入、显式 discard、格式失败和替换失败期间保持不变。原子文件 4/4、结果文件 3/3、Windows 23 通过/1 ignored、NodeEngine test-hooks lib 142/142、fmt/diff 通过。
- Task 9 fix round 1/5：独立审查发现未显式 discard 的 writer 会遗留 partial，且发布元数据缺少直达 `run_id/library_revision/group_count`；提交 `8d946cf3` 增加未完成 writer 的 best-effort Drop 精确清理、唯一组计数及锁定旧 result 的 writer 层失败证据。结果文件 6/6、原子文件 4/4、Windows 23 通过/1 ignored、NodeEngine test-hooks lib 142/142、fmt/diff 通过；定向复审确认 2 项 addressed、0 项 open。
- Task 9 complete (commits `6b6731dd..8d946cf3`, review clean)。
- Task 10A：提交 `235148d0`。本地分析输入、候选、分组和成员已迁入 `dedup-node-engine::analysis::model`，纯算法保留 `DisplayPath`、代表直连和二筛证据；旧 SQLite 分析状态机仅在读写边界做显式兼容转换。`local_analysis` 11/11、代表分组 3/3、NodeEngine test-hooks lib 142/142、fmt/diff 通过。独立审查无 Critical/Important；其 Minor“大小写未覆盖”经源码复核不成立：`NormalizedPath` 会把组件转大写，而测试断言保留 `Root` 混合大小写，已能证明结果不是从规范路径重建。
- Task 10B1：提交 `ee1708f4`。当前扫描唯一 TaskId/revision 门禁、一次批量基础缓存查询、排序去重的内存输入/一筛候选与最近结果 TSV 发布已落地；新入口不读取旧 `tasks` 表作生产门禁，也不写旧分析/任务表。旧 queued/running 行行为测试先 RED 后 GREEN；控制 Agent 验证 transient 4/4、local_analysis 11/11、representative_grouping 3/3、NodeEngine test-hooks lib 146/146、fmt/diff 通过。独立审查 Spec Compliance PASS、Task Quality PASS，无 Critical/Important/Minor。
- Task 10B1 complete (commits `7a063e95..ee1708f4`, review clean)。
- Task 10B2：提交 `3ec6868`。二筛缺失项按唯一 ContentKey 和结构完整性批量准备，合法全零特征保持缓存命中；最终候选判定一次批量读取全部唯一内容后在内存完成，删除了逐候选 SQLite 查询。控制 Agent 验证新增行为 2/2、local_analysis 11/11、representative_grouping 3/3、NodeEngine test-hooks lib 148/148、fmt/diff 通过；独立聚焦审查无 Critical/Important/Minor。
- Task 10B2 complete (commits `ee1708f4..3ec6868`, review clean)。
- Task 10C：提交 `70d2886`。Node actor 已改用当前进程 `latest_completed_scan` 与内存分析状态；缓存命中直接发布最近结果 TSV，确有二筛缺失时才进入瞬态任务文件、唯一读取调度器、Worker 与 taskless SQLite ACK。取消在最终发布前再次核对，不覆盖旧结果；Stage2 运行目录删除首次失败时由同一 production owner 重试，未清理完成不发布成功。旧 queued/running 任务行不参与新入口门禁，旧任务/分析表保持不变。控制 Agent 当前验证 NodeEngine test-hooks lib 153/153、local_analysis 11/11、representative_grouping 3/3、NodeStore analysis_state 6/6、fmt/diff-check 全通过；详见 `task-10c-report.md`。
- Task 10C complete (commits `3ec6868..70d2886`, focused review findings closed)。
- Task 11/12 Ruling: Node 保留最近本地结果的只读窗口协议，但 Desktop 不实现或调用 Node 本地 analysis 窗口；Task11 只落 Node reader/actor/wire，Task12 只把 PostgreSQL 中心结果改成 UI 滑动窗口并删除旧本地分支 — 直接用户约束“desktop 不需要 node 的 analysis 结果”晚于旧计划，且这样不会复制两套结果事实 — 若判断错误，代价是未来 Node 本地管理界面需要另加一个协议客户端。
- Task 11 Ruling: `ReadLocalResultWindow.group_kind` 使用新 message 的 field 12，保留草案 fields 1..11，不让客户端读取全量后过滤 — 原计划接口要求 `Groups(GroupKind)`，但 proto 草案漏字段 — 若判断错误，代价仅是尚未发布的新 tag 46 消息字段布局需要在发布前调整。
- Task 11：提交 `cfadb360`。最近成功 TSV 通过顺序 H/M/F 校验建立进程内组摘要和成员 `u64` 偏移，窗口 seek 不创建 `.idx`；V5 新增 tag 46、显式 `group_kind` 和 `GroupMember.display_path=12`，Desktop 未接入 Node 本地结果。
- Task 11 review：发现两项阻塞问题：损坏结果被降为 Internal/NotFound；reader 按固定路径重开导致替换到 actor 安装之间读错文件且安装失败会丢旧磁盘结果。
- Task 11 fix round 1/5：提交 `78170400`。`InvalidResult=7` 与启动 Invalid 状态已解决第一项；第二项仍因原子替换后 `bind_prepared` 重新打开路径而未闭环，scoped re-review 为 1 addressed、1 open。
- Task 11 fix round 2/5：提交 `7a237c39`。替换前完整验证产生并持有稳定文件句柄，Windows 原子替换直接移动同一文件身份，替换后不再执行 fallible open/bind；scoped re-review 为 1 addressed、0 open，无新 Critical/Important。
- Task 11 controller verification：analysis_result_window 7/7、analysis_result_file 6/6、dedup-protocol 24 项、dedup-windows atomic_file 5/5、NodeEngine test-hooks lib 155/155、`cargo fmt --all -- --check`、`git diff --check` 全通过；C/D 可用空间 22.82/16.05 GiB。
- Task 11 complete (commits `dcf9c37..7a237c39`, review clean)。
