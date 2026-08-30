# Dispatcher 同盘多在途身份 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 删除 `TaskFileDispatcher` 的“同一物理盘必须等待上一文件 SQLite ACK 才能交付下一文件”硬编码，使单个物理盘 lane 能按配置额度并发交付多个精确任务身份，同时保持权重、老化、Hash/Media 公平、TSV ACK 和取消所有权正确。

**Architecture:** 每个物理盘继续使用一个 `TaskDiskLane`、一个 TSV 和一个 Dispatcher owner；把 `lane -> 唯一 identity` 改为 `lane -> identity set`，并以冻结的 `per_disk_limit` 作为同 lane 未 ACK 身份窗口。普通队首占一个窗口位置，Hash→Media continuation 复用原身份；真实磁盘并发仍完全由同一个 `DiskReadScheduler` 的全局/逐盘 permit、配置权重和老化保护裁决。

**Tech Stack:** Rust 2024、Tokio 1.53、`BTreeMap/BTreeSet`、`TaskFileDispatcher`、`TransientTaskFileSet`、`DiskReadScheduler`、`WorkerPool`、rusqlite、PowerShell 7 Windows 验收脚本。

**Spec:** `docs/superpowers/specs/2026-08-30-multi-inflight-task-lane-dispatch-design.md`

## Global Constraints

- 不修改 SQLite schema、TSV 八列格式、Protobuf、Worker 计算协议或媒体算法。
- 不创建虚拟盘 lane；同一物理盘上的根仍合并为一份权重、一个硬上限和一个 TSV。
- `per_disk_limit`、`configured_weight` 和 `total_threads` 必须来自本轮冻结配置，不能把 `5:1`、`12` 或其他示例写死到产品代码。
- Dispatcher 只维护身份窗口和精确 ownership，不复制 `DiskReadScheduler` 的 deficit、老化或 Hash/Media 公平状态。
- 普通队首最多保留一个等待 permit 的 future；不能预领取整个 TSV，也不能建立无界 future/结果队列。
- continuation 使用同一 `TaskFileIdentity`，不新增 TSV 行，不重复占用身份窗口。
- 只有 SQLite ACK 成功后才能把对应行从 `P` 改为 `C/F`；允许乱序 ACK。
- 取消和任务级错误不写 `F`；未 ACK 行保持 `P`，运行目录在 owner 全部收束后删除。
- 新增或修改的方法、类型、字段和关键变量添加简洁中文注释。
- 不在本项中修复 Worker 原生崩溃、Everything、exporter 或验收进程清理等独立问题。
- 不部署、不替换、不清理 `I:\Tool`。
- Cargo 统一使用 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`；清空 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，设置 `CARGO_INCREMENTAL=0`、`CARGO_PROFILE_DEV_DEBUG=0`、`CARGO_PROFILE_TEST_DEBUG=0`。
- C 或 D 盘可用空间低于 10 GiB 时，先停止新的重型命令，只盘点并清理本项目明确可再生的 Cargo target/cache；保留源码、任务文件、运行证据和用户文件。
- 每个提交前运行定向测试、`cargo fmt --all -- --check` 和 `git diff --check`；只暂存本任务列出的文件。

---

## File Structure

- `crates/node-engine/src/task_dispatch.rs`：多身份窗口、精确 pending、continuation、ACK 和取消所有权的唯一实现位置。
- `crates/node-engine/tests/task_dispatch.rs`：Dispatcher 容量、乱序 ACK、continuation、失败和真实 scheduler 行为测试。
- `crates/node-engine/tests/transient_task_files.rs`：复用并补强任务文件多身份与乱序状态字节契约。
- `crates/node-engine/src/scan/task_file_base_coordinator.rs`：同 lane 多 Hash/Media 在首个 ACK 前推进的真实事件泵测试。
- `crates/node-engine/src/scan/task_file_base_stream.rs`：只在行为测试暴露实际 admission 问题时做最小调整；不重写事件泵。
- `AGENTS.md`：把“同 lane 一个未 ACK identity”改为按配置额度的精确身份窗口长期约束。
- `docs/superpowers/specs/2026-08-30-streaming-hash-media-pipeline-design.md`：澄清流式流水线允许同 lane 多身份，历史 ACK 语义仍保留。
- `docs/verification/2026-08-30-multi-inflight-task-lane-dispatch.md`：记录 RED/GREEN、回归、包和单次真实媒体证据。

---

### Task 1: 用行为测试替换错误的单身份契约

**Files:**
- Modify: `crates/node-engine/tests/task_dispatch.rs:355-393`
- Verify: `crates/node-engine/tests/transient_task_files.rs:163-220`

**Interfaces:**
- Consumes: `TaskDiskLane.per_disk_limit`、`TaskFileDispatcher::poll_next`、`mark_completed`。
- Proves: 同一 lane 的身份窗口按配置值工作，ACK 可乱序。

- [ ] **Step 1: 把旧测试改成配置窗口 RED**

删除测试名 `one_lane_waits_for_ack_before_delivering_next_identity`，新增：

```rust
#[test]
fn one_lane_dispatches_up_to_configured_limit_before_any_ack() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[22], LocalDiskKind::Ssd, 2, 2);
    let rows = [
        base_record("parallel-first.bin", None),
        base_record("parallel-second.bin", None),
        base_record("parallel-third.bin", None),
    ];
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher.append_batch(&task_lane, &rows).unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();

    let first = ready_task(poll_once(&mut dispatcher, &cancellation));
    let second = ready_task(poll_once(&mut dispatcher, &cancellation));
    assert_ne!(first.identity, second.identity);
    assert_eq!(provider.active_permits(), 2);
    assert!(poll_once(&mut dispatcher, &cancellation).is_pending());

    dispatcher.mark_completed(&second.identity).unwrap();
    drop(second);
    let third = ready_task(poll_once(&mut dispatcher, &cancellation));
    assert_eq!(third.record.item_id, rows[2].item_id);
    dispatcher.mark_completed(&first.identity).unwrap();
    dispatcher.mark_completed(&third.identity).unwrap();
    drop(first);
    drop(third);
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Ok(None))
    ));
    dispatcher.discard().unwrap();
}
```

测试辅助函数 `ready_task` 只解包 `Poll::Ready(Ok(Some(task)))`，失败信息必须包含实际 `Poll`。

- [ ] **Step 2: 增加乱序 ACK RED**

新增 `out_of_order_ack_releases_only_matching_same_lane_identity`：额度 3，先交付三项；先 ACK 第二项，读取 TSV 后只允许第二行首字节为 `C`，第一和第三行仍为 `P`；再分别把第一项标记 `F`、第三项标记 `C`，最终 `all_terminal`。

- [ ] **Step 3: 运行 RED**

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch one_lane_dispatches_up_to_configured_limit_before_any_ack --locked -- --test-threads=1`

Expected: 旧实现第二次 `poll_once` 为 Pending，测试失败，精确证明 lane 级 ACK 屏障。

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch out_of_order_ack_releases_only_matching_same_lane_identity --locked -- --test-threads=1`

Expected: 同样在交付第二身份前失败；不是编译器或测试夹具失败。

- [ ] **Step 4: 保留任务文件层既有契约**

Run: `cargo test -p dedup-node-engine --features test-hooks --test transient_task_files one_lane_tracks_multiple_inflight_and_allows_out_of_order_ack --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test transient_task_files refill_continues_while_another_identity_is_inflight --locked -- --test-threads=1`

Expected: 两项在产品修改前已经 PASS，证明无需重写 TSV owner。

---

### Task 2: 把 Dispatcher 改为精确身份集合和有界窗口

**Files:**
- Modify: `crates/node-engine/src/task_dispatch.rs:168-400`
- Test: `crates/node-engine/tests/task_dispatch.rs`

**Interfaces:**
- Replaces: `pending: BTreeMap<String, PendingPermit<_>>`。
- Replaces: `in_flight_lanes: BTreeMap<String, TaskFileIdentity>`。
- Produces: 精确身份 pending、lane 身份集合和窗口辅助函数。

- [ ] **Step 1: 修改状态字段**

```rust
/// 等待 scheduler 的精确任务请求；身份键用于失败和取消时逐项收束。
pending: BTreeMap<TaskFileIdentity, PendingPermit<Provider::Permit>>,
/// 每个 lane 已交付但尚未 SQLite ACK 的精确身份集合。
in_flight_by_lane: BTreeMap<String, BTreeSet<TaskFileIdentity>>,
```

更新结构体和模块注释，删除“每个 lane 同时最多一个请求/唯一身份”的描述。

- [ ] **Step 2: 增加三个私有辅助函数**

```rust
/// 返回 lane 已占用的不同任务身份数，普通 pending 也预占一个窗口位置。
fn lane_identity_count(&self, lane_key: &str) -> usize;

/// 判断 lane 是否已有一个普通队首 permit 请求。
fn has_pending_ordinary(&self, lane_key: &str) -> bool;

/// 从 lane 集合释放一个精确身份，并在集合为空时删除 lane 键。
fn release_in_flight_identity(&mut self, identity: &TaskFileIdentity) -> io::Result<()>;
```

`lane_identity_count` 计算 `in_flight_by_lane[lane].len()`，再加上不属于该集合的普通 pending 身份；continuation 不增加计数。

- [ ] **Step 3: 改造 ACK 和快照入口**

`mark_completed`、`mark_failed` 和 `abandon_in_flight` 都调用 `release_in_flight_identity`。`in_flight_identities()` 按 lane 键和身份自然顺序 flatten 后返回全部身份，不丢失同盘兄弟项。`discard()` 只在 pending、continuations、全部 lane 身份集合均空时允许执行。

- [ ] **Step 4: 改造 `has_admitted_work`**

规则固定为：

- admission 允许的 exact pending 存在，返回 true；
- Media continuation 存在且允许 Media，返回 true；
- 普通队首类别被允许，并且 `lane_identity_count < lane.per_disk_limit`，返回 true；
- lane 窗口已满不报告 admission Blocked，只等待其 Hash/Media/ACK 事件释放窗口。

- [ ] **Step 5: 运行 Task 1 GREEN**

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch one_lane_dispatches_up_to_configured_limit_before_any_ack --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch out_of_order_ack_releases_only_matching_same_lane_identity --locked -- --test-threads=1`

Expected: 两项 PASS；额度 2 时第三项确实等待，乱序 ACK 只改对应状态字节。

- [ ] **Step 6: 增加 HDD=1 回归并运行**

新增 `hdd_lane_with_limit_one_remains_serial`，使用 `lane(&[23], LocalDiskKind::Hdd, 1, 1)`，证明首项未 ACK 时第二项仍 Pending。

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch hdd_lane_with_limit_one_remains_serial --locked -- --test-threads=1`

Expected: PASS。

- [ ] **Step 7: 格式检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "fix: allow configured same-lane task window"`

---

### Task 3: 让普通队首和 continuation 保持精确 ownership

**Files:**
- Modify: `crates/node-engine/src/task_dispatch.rs:250-665`
- Modify: `crates/node-engine/tests/task_dispatch.rs:487-680`

**Interfaces:**
- Consumes: Task 2 的身份集合和窗口辅助函数。
- Preserves: `request_media_continuation`、`ensure_identity_not_waiting`、`TaskDispatchAdmission`。

- [ ] **Step 1: 写 continuation 窗口 RED**

新增 `same_lane_continuation_reuses_identity_window_slot`：lane 额度 2，先交付 Hash 身份 A 和普通 Media 身份 B；登记 A 的 Media continuation 后，即使两个身份窗口已满，A continuation 仍可取得 permit；第三个普通身份 C 必须等待 B 或 A 的终态 ACK。

断言：

- continuation 的 `identity == A`；
- TSV 行数没有增加；
- A 只在最终 Media ACK 后迁移一次；
- `in_flight_identities()` 只有 A、B，不出现第三个派生身份。

- [ ] **Step 2: 写跨身份清理 RED**

新增 `same_lane_pending_request_does_not_block_other_identity_abandon`：同 lane A、B 已在途，B 有等待请求或 continuation；取消并丢弃 pending 后，`abandon_in_flight(A)` 必须成功且 B 仍可单独收束。

- [ ] **Step 3: 改造 `request_media_continuation`**

按完整身份查重。若同 lane 有普通 pending，只撤下该普通 future，保持普通 TSV 行为 `P`；不得删除其他 identity 的 continuation。登记顺序仍由 `BTreeMap<TaskFileIdentity, _>` 保持稳定。

- [ ] **Step 4: 改造 `start_lane_requests`**

实现顺序：

1. 删除所有 admission 已禁止的 pending future；
2. 每个 lane 先找尚未 pending 的最小 continuation；
3. 没有可派发 continuation 时，只在窗口未满且没有普通 pending 时观察一个队首；
4. 以精确 identity 插入 `pending`；
5. 不在这里复制 scheduler 权重或一次循环批量领取多个 TSV 行。

- [ ] **Step 5: 改造 `poll_lane_requests`**

普通请求 permit 成功后调用 `take_lane_exact`，再把身份插入该 lane 的 `BTreeSet`；若窗口计数异常超限，释放 permit 并返回明确基础设施错误。continuation permit 成功后必须验证该身份已存在于对应 lane 集合，只移除其 continuation 意图，不插入新身份。

permit future 失败时只删除该 identity 的 pending；普通行仍为 `P`，continuation 意图保留供重试。

- [ ] **Step 6: 运行 continuation 和 admission 回归**

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch same_lane_continuation_reuses_identity_window_slot --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch media_continuation_is_prioritized_over_the_next_same_lane_head --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch failed_media_continuation_keeps_intent_for_retry_and_cancel_keeps_pending_status --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch hash_only_admission_cancellation_keeps_pending_row_as_p --locked -- --test-threads=1`

Expected: 全部 PASS。

- [ ] **Step 7: 检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "fix: preserve exact continuation ownership"`

---

### Task 4: 闭合多身份失败、取消和 discard

**Files:**
- Modify: `crates/node-engine/src/task_dispatch.rs:280-350`
- Modify: `crates/node-engine/tests/task_dispatch.rs:630-930`
- Verify: `crates/node-engine/src/scan/task_file_base_stream.rs:510-570`

**Interfaces:**
- Consumes: `cancel_pending_permit_requests`、`in_flight_identities`、`abandon_in_flight`、`discard`。
- Proves: 多身份取消不串项、不泄漏、不误写终态。

- [ ] **Step 1: 写多身份取消 RED**

新增 `cancellation_abandons_all_same_lane_identities_and_preserves_pending_rows`：同 lane 额度 3，交付三身份，其中一个有 continuation；触发取消，先调用 `cancel_pending_permit_requests`，再按 `in_flight_identities` 逐项 abandon，最后 `discard`。取消前读取 TSV，取消收束前后所有未 ACK 行都必须为 `P`。

- [ ] **Step 2: 写单 identity permit 失败 RED**

新增 `same_lane_permit_failure_keeps_other_inflight_identities_intact`：A 已交付，B 的 permit future 返回错误；错误后 A 仍在 `in_flight_identities`，B 行仍为 `P`，下一次请求可按 B 精确重试。

- [ ] **Step 3: 精确化取消检查**

`abandon_in_flight(identity)` 只拒绝“同一个 identity 仍有 pending future”的情况，不检查整个 lane。`cancel_pending_permit_requests` 一次丢弃全部 future；`in_flight_identities` 返回稳定、无重复快照供 cleanup 使用。

- [ ] **Step 4: 核对事件泵 cleanup 顺序**

确认 `cleanup_stream` 仍按 Hash/remote → Worker/Media → persist → dispatcher identity 的顺序收束。只有行为测试失败时才做最小修复；不得用 `clear()` 跳过 permit/Worker join。

- [ ] **Step 5: 运行错误和取消回归**

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch cancellation_abandons_all_same_lane_identities_and_preserves_pending_rows --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch same_lane_permit_failure_keeps_other_inflight_identities_intact --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch abandon_after_cancellation_allows_exact_run_discard --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch discard_rejects_an_unacknowledged_dispatched_task --locked -- --test-threads=1`

Expected: 全部 PASS。

- [ ] **Step 6: 检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "fix: close multi-identity dispatcher cleanup"`

---

### Task 5: 用真实 DiskReadScheduler 验证配置额度和双盘公平

**Files:**
- Modify: `crates/node-engine/tests/task_dispatch.rs:395-455`
- Verify: `crates/node-engine/tests/disk_scheduler.rs`

**Interfaces:**
- Consumes: `SchedulerTaskLanePermitProvider`、`DiskReadConfig`、`TaskDiskLane`。
- Proves: Dispatcher 能把一个 lane 的多个 Ready 文件暴露给真实 scheduler，不伪造权重。

- [ ] **Step 1: 把第六项额度测试改为单 Dispatcher 同 lane**

新增 `scheduler_grants_five_same_lane_identities_and_holds_sixth`：

- `hdd_threads_per_disk=5`、`total_threads=5`；
- 一个 dispatcher、一个 PhysicalDisk27 lane、六行；
- 前五个 `next` 均取得真实 permit，首个 ACK 前第六个 future 必须等待；
- 释放任意一个 permit 并 ACK 对应身份后，第六个取得许可。

该测试替代“六个独立 dispatcher 才能占满一块盘”的绕行夹具。

- [ ] **Step 2: 增加 SSD/HDD 配置窗口测试**

新增 `ssd_and_hdd_lanes_expose_configured_five_to_one_window`：SSD lane 额度/权重 5，HDD lane 额度/权重 1，全局 6，各自至少六行。持有前六个 permit，按 physical disk 统计必须为 SSD 5、HDD 1；数字来自测试配置，而不是产品常量。

- [ ] **Step 3: 增加全局不足与老化回归**

复用现有真实 scheduler 测试验证 `total_threads=3` 时累计选择保持配置权重、HDD 不超过自己的额度且不饥饿。Dispatcher 测试只证明 Ready 暴露；不在 Dispatcher 新写 deficit 算法。

Run: `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1`

- [ ] **Step 4: 运行真实 scheduler 定向测试**

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch scheduler_grants_five_same_lane_identities_and_holds_sixth --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch ssd_and_hdd_lanes_expose_configured_five_to_one_window --locked -- --test-threads=1`

Expected: 全部 PASS；测试结束前显式 drop permit、逐 identity ACK/abandon 并 discard。

- [ ] **Step 5: 检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "test: verify configured multi-lane dispatch limits"`

---

### Task 6: 验证真实流式流水线不再等待首个 ACK

**Files:**
- Modify: `crates/node-engine/src/scan/task_file_base_coordinator.rs:725-1850`
- Conditional Modify: `crates/node-engine/src/scan/task_file_base_stream.rs:108-310`

**Interfaces:**
- Consumes: `TaskFileBaseCoordinatorOptions`、`BasePersistTestController`、controlled `WorkerPool`。
- Proves: 产品事件泵在同 lane 内真正使用多身份窗口，而不是 Dispatcher 单元测试假并发。

- [ ] **Step 1: 写同 lane 多 Hash RED**

新增 `same_lane_hashes_continue_while_first_sqlite_ack_is_blocked`：同一 PhysicalDisk lane 额度 3，三项都需要 MD5，`hash_capacity=3`。用 `BasePersistTestController` 暂停第一条 SQLite 持久化，断言在释放 ACK 前三个 Hash reader 都已进入或完成源读取。

旧 Dispatcher 必须在第二个 reader 进入前失败；不能用 sleep 后读取计数作为唯一同步，使用 `Notify`/原子计数加 `timeout`。

- [ ] **Step 2: 写同 lane 多 Media RED**

新增 `same_lane_starts_multiple_media_workers_before_first_sqlite_ack`：同一 lane 额度 4、四个已知 MD5 且确实缺 Media 字段的项，`worker_capacity=4`，controlled WorkerPool 有四槽。暂不发送第一个 Worker 终态，断言四个不同 item 都收到 `Started`；随后按乱序终态完成并验证四条 SQLite ACK 分别把 TSV 改为 `C`。

- [ ] **Step 3: 运行 RED**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib same_lane_hashes_continue_while_first_sqlite_ack_is_blocked --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib same_lane_starts_multiple_media_workers_before_first_sqlite_ack --locked -- --test-threads=1`

Expected: 旧实现因首身份未 ACK 而超时/断言失败。

- [ ] **Step 4: 只在需要时最小调整事件泵**

预期 Task 2-4 后不需要改事件泵。若测试仍失败，只允许修复以下边界：

- `has_admitted_work` 对窗口空位的判断；
- 每次 task 返回后重新计算 `allow_hash/allow_media`；
- ACK 分支不得成为全局 dispatch gate。

不得改回批量 Hash join、不得等待整批 Media、不得引入第二个事件泵。

- [ ] **Step 5: 运行 GREEN 和既有流式回归**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib same_lane_hashes_continue_while_first_sqlite_ack_is_blocked --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib same_lane_starts_multiple_media_workers_before_first_sqlite_ack --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib first_hashed_media_miss_enters_worker_before_later_hash_finishes --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib active_media_does_not_block_later_hash_on_another_lane --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib cancellation_returns_pending_owner_without_acknowledging_rows --locked -- --test-threads=1`

Expected: 全部 PASS。

- [ ] **Step 6: 检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "test: prove same-lane streaming concurrency"`

---

### Task 7: 全量回归并更新长期契约

**Files:**
- Modify: `AGENTS.md:115-125`
- Modify: `docs/superpowers/specs/2026-08-30-streaming-hash-media-pipeline-design.md:35-95`
- Add: `docs/verification/2026-08-30-multi-inflight-task-lane-dispatch.md`
- Do not Modify: `docs/verification/2026-08-30-streaming-hash-media-pipeline.md`

- [ ] **Step 1: 更新 `AGENTS.md`**

把旧句：

```text
同一 lane 在 SQLite ACK 前只交付一个任务身份
```

替换为：

```text
同一 lane 可在 SQLite ACK 前按冻结的 per_disk_limit 交付多个精确任务身份；
每个身份仍只由自己的 ACK 迁移 P→C/F，允许乱序 ACK；Media continuation 复用原身份。
```

同时写明全局 permit、Hash/Media slot、Worker 和持久化背压继续生效。

- [ ] **Step 2: 更新流式设计说明**

在调度/所有权章节澄清：同 lane continuation 的“同身份”只表示不新增 TSV 行，不表示整个 lane 在 ACK 前串行。引用本计划的设计补充，不回写历史验证结论。

- [ ] **Step 3: 运行 NodeEngine 全量回归**

Run: `cargo test -p dedup-node-engine --features test-hooks --test task_dispatch --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test transient_task_files --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --locked -- --test-threads=1`

Expected: 全部 PASS；不得用过滤后的局部测试替代包全量。

- [ ] **Step 4: 运行跨 crate 协议和 UI 稳定回归**

Run: `cargo test -p worker --test worker_protocol_process --locked -- --test-threads=1`

Run: `cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1`

Run: `cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1`

Expected: 全部 PASS；本项没有协议/UI 变更，但需防止工作树并行改动破坏验收客户端。

- [ ] **Step 5: 写验证记录**

`docs/verification/2026-08-30-multi-inflight-task-lane-dispatch.md` 必须记录：

- 当前 revision 和 source tree SHA；
- 两个真实 RED 的命令与精确失败；
- Task 1-6 的 GREEN 与全量计数；
- lane 额度、全局额度、Hash/Media 容量和实际峰值；
- 取消/失败/乱序 ACK 的状态字节证据；
- 未执行或失败的命令，不能只记录成功项。

- [ ] **Step 6: 格式、差异和提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Run: `git status --short`

Commit: `git commit -m "docs: define multi-inflight dispatcher contract"`

---

### Task 8: 构建候选并执行一次双物理盘真实媒体门禁

**Files:**
- Verify: `scripts/build-release.ps1`
- Verify: `scripts/verify-release.ps1`
- Verify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
- Modify: `docs/verification/2026-08-30-multi-inflight-task-lane-dispatch.md`

**Run boundary:** 只生成候选包、外部测试工具和 `C:\tmp` 证据；不部署。

- [ ] **Step 1: 构建并验证正式候选包**

Run: `pwsh -NoProfile -File scripts\build-release.ps1 -CargoTargetDir C:\tmp\rust-v2-core-scope-target-task7b2d2c1`

Run: `pwsh -NoProfile -File scripts\verify-release.ps1 -Package dist-rust-v2\mySingerServer-rust-v2-win-x64.zip`

Expected: `RUST_V2_RELEASE_BUILD_PASS` 和 `PACKAGE_PASS`；正式 ZIP 仍只允许 desktop/node/worker/Everything 四个顶层 EXE。

- [ ] **Step 2: 单独构建验收客户端和结果导出器**

Run: `cargo build -p dedup-desktop-core --example runtime_acceptance --release --locked --target x86_64-pc-windows-msvc`

Run: `cargo build -p dedup-node-store --example export_scan_result_summary --release --locked --target x86_64-pc-windows-msvc`

记录两个 EXE 的绝对路径和 SHA-256，不把它们放进正式 ZIP。

- [ ] **Step 3: 执行唯一一轮真实媒体运行**

Run:

```powershell
pwsh -NoProfile -Command "& 'tests\windows\Measure-RustV2RuntimeAcceptance.ps1' `
  -MediaRoots @('H:\pik\00000000000','I:\tmp') `
  -DurationSeconds 10800 -SampleSeconds 2 `
  -CargoTargetDir 'C:\tmp\rust-v2-core-scope-target-task7b2d2c1' `
  -ReleaseRoot 'D:\code\mySingerServer\.worktrees\core-scope-transient-runtime\dist-rust-v2\staging' `
  -AcceptanceClientPath 'C:\tmp\rust-v2-core-scope-target-task7b2d2c1\x86_64-pc-windows-msvc\release\examples\runtime_acceptance.exe' `
  -ResultExporterPath 'C:\tmp\rust-v2-core-scope-target-task7b2d2c1\x86_64-pc-windows-msvc\release\examples\export_scan_result_summary.exe' `
  -EvidenceRoot 'C:\tmp\rust-v2-multi-inflight-single-run\evidence' `
  -ReportPath 'C:\tmp\rust-v2-multi-inflight-single-run\evidence\report.md' `
  -Enumerator everything -WorkerCount 20 `
  -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 -UnknownThreadsPerDisk 1 `
  -TotalReadThreads 12 -ReservedCores 1 `
  -SingleRun -CompleteWhenTaskTerminal -RequireDistinctPhysicalDisks"
```

任务到达终态立即结束；10800 秒只是硬上限。不得追加第二轮、六轮 A/B 或 A-3。

- [ ] **Step 4: 应用真实运行门禁**

PASS 必须同时满足：

- 任务终态为 `completed`；若 `cancelled/failed`，本轮直接 FAIL/INCONCLUSIVE，不自动重跑；
- `H:\pik\00000000000` 与 `I:\tmp` 映射到不同物理盘，媒体前后 manifest 完全一致；
- 两盘存在重叠读取；持续 Ready 的目标盘 `disk_reads.active.peak` 不再被 Dispatcher 固定为 1；
- Worker 非空闲峰值大于 2，并保留 CPU、逐盘吞吐、队列等待和 Worker 崩溃完整路径；
- `grant/release`、任务终态、SQLite ACK 和 TSV/运行目录清理守恒；
- 结果导出文件存在、SHA-256 已记录，缺失或不完整只可标记 INCONCLUSIVE；
- Node 未意外退出，正式包与源 revision/SHA 绑定一致。

权重只统计两个 lane 同时 Ready 且可运行的窗口；不得用磁盘字节吞吐比例代替任务选择权重。

- [ ] **Step 5: 更新验证记录**

把真实 evidence root、包 SHA、EXE SHA、物理盘映射、任务终态、逐盘 active 峰值、Worker 峰值、CPU/IO、崩溃路径和最终结论追加到本次验证文档。明确说明没有部署到 `I:\Tool`。

---

### Task 9: 最终审查与交付

**Review model:** `gpt-5.6-sol`，reasoning effort `max`。

- [ ] **Step 1: 请求只读代码审查**

审查范围固定为本计划产生的提交，重点检查：

- lane 窗口是否真的来自 `per_disk_limit`；
- 普通 pending 是否被重复计数或越界；
- continuation 是否错误增加身份数；
- permit 完成/失败、乱序 ACK、取消时是否释放错误身份；
- 是否复制了 scheduler 权重/老化状态；
- 是否出现无界队列或新的整批屏障。

- [ ] **Step 2: 修复 Important/Critical 问题**

每个成立的问题先补旧实现可失败的行为测试，再做最小修复；不因审查意见扩大到 Worker 崩溃、恢复或 UI 功能。

- [ ] **Step 3: 重跑受影响定向测试和 Task 7 全量**

Expected: 所有测试 PASS，`cargo fmt --all -- --check` 和 `git diff --check` PASS。

- [ ] **Step 4: 交付边界**

最终报告列出提交、测试、包、真实运行结论和剩余独立问题。只有用户另行要求时才合并、推送或部署；本计划默认停在“候选已验证、生产未替换”。
