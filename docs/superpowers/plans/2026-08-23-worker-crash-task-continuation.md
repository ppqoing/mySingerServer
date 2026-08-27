# Worker 崩溃单文件隔离与完整路径日志 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 实施修复前使用 `superpowers:test-driven-development`，完成声明前使用 `superpowers:verification-before-completion`。

**Goal:** 修复 Worker 崩溃事件因阶段不一致而升级成任务级失败、阻塞整个基础计算任务的问题，并保证 Node 在 Worker 进入终态前持续持有文件完整路径，在崩溃日志和 SQLite 故障记录中输出该路径。

**Architecture:** Node 继续作为 Worker 进程监督和任务状态的唯一权威：运行映射保存 `task_id/item_id`、完整文件身份、PID/槽位和当前阶段。Worker 退出时，WorkerPool 先把路径所有权转移到 `WorkerEvent::Crashed`，基础计算 `active` 映射继续保留同一路径，直到 Node 完成崩溃落库后才释放；整个过程中不得出现无路径窗口。`stage` 只表示崩溃发生时的诊断阶段，不再参与文件归属硬校验；文件归属仍由运行中的 `item_id` 及持久化的机器、规范路径和文件大小确认。单 Worker 崩溃只把当前文件置为 `failed`，补建 Worker 后继续处理队列，任务级 `failed` 只保留给数据库、池关闭等基础设施错误。

**Tech Stack:** Rust 1.97.1、Tokio actor、rusqlite 事务、`tracing` 结构化日志、Windows Worker 子进程监督。

**Spec:** `docs/superpowers/plans/2026-08-23-worker-crash-task-continuation.md#需求与验收标准`

## Global Constraints

- 只修改 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup`；该工作树已有大量未提交和未跟踪改动，不得 reset、clean、checkout 覆盖或宽泛暂存。
- 本计划只修复 Node 的崩溃归属、状态落库、继续调度和日志完整性；不在同一变更中修复 FFmpeg `0xc0000374` 原生堆损坏。
- 不修改 Protobuf、SQLite schema、任务状态枚举和重试协议；`item_id` 已是当前任务项的稳定唯一键，本次不引入 attempt/generation 字段。
- `WorkerFileIdentity` 中机器、规范路径、显示路径、文件大小和物理盘身份保持冻结；`stage` 是 Node 更新的运行阶段，不得再作为不可变文件身份。
- Worker 崩溃必须产生一个文件级 `failed` 终态和一条 `worker_crash` 故障记录；不得因阶段文本不一致返回任务级基础设施错误。
- Node 日志必须包含完整 `display_path`，同时包含 `normalized_path`、`task_id`、`item_id`、`worker_pid`、`worker_exit_code`、`crash_stage` 和错误消息；不得只记录文件名。
- 路径从 dispatch 开始保存在 WorkerPool 运行映射和基础计算 `active` 映射中；进程退出时先复制进 `WorkerEvent::Crashed` 再移除池运行项，基础计算 `active` 项在崩溃事务提交后才释放。
- 如果 SQLite 事务、WorkerPool 通道或替代 Worker 启动失败，仍按任务级基础设施错误处理，不把真实基础设施故障伪装为文件失败。
- 新增方法、类型、字段和测试辅助对象必须有中文注释，说明职责、用法和关键实现逻辑。
- Cargo 输出固定使用 `C:\tmp\rust-v2-visual-fidelity-target`；运行 Rust 测试前清除继承的 `CC`/`CXX`，避免使用错误的外部编译器配置。
- 每个提交步骤只列出精确文件和建议消息；只有用户明确授权提交时才执行 `git commit`。

---

## 已确认故障链

1. `crates/node-engine/src/scan/base_compute.rs` 首次派发 `BeginBaseCompute` 时把运行身份阶段设为 `base_hash`。
2. Worker 返回 `BaseHashReady` 后，Node 把 SQLite `task_items.stage` 更新为 `base_compute`。
3. `crates/node-engine/src/worker/pool.rs::continue_base_slot` 向同一 Worker 会话发送 `ContinueBaseCompute` 时复用了原 `WorkIdentity`，运行映射里的阶段仍为 `base_hash`。
4. Worker 在续算期间退出后，`WorkerEvent::Crashed` 携带旧阶段；`NodeStore::fail_running_item_with_file_fault` 要求故障阶段与 SQLite 阶段严格相等，返回“Worker 崩溃事件身份与持久任务项不一致”。
5. 该存储错误向上传播为任务级错误，基础计算任务停止；崩溃文件没有形成完整的单文件故障闭环。

## 需求与验收标准

- Worker 在 `base_hash` 崩溃：当前文件失败，完整路径写入 Node 日志与 `file_faults`，后续文件继续。
- Worker 在 `base_compute` 崩溃：当前文件失败，故障阶段记录为 `base_compute`，后续文件继续。
- 同一任务中一个文件崩溃、另一个文件成功后，任务最终为 `completed`，统计为 `failed=1`、`succeeded=1`，不得变成任务级 `failed`。
- WorkerPool 在 `BaseHashReady → ContinueBaseCompute` 期间仍可查询到同一个完整文件路径；收到正常完成、取消或崩溃终态时先让终态事件取得同一路径，再移除运行项。
- `file_faults.display_path` 与任务项冻结的完整显示路径完全一致；`stage` 保存崩溃诊断阶段，但阶段不一致不能阻止事务提交。
- 机器 ID、规范路径、文件大小或任务项 `running` 状态不一致时仍必须拒绝崩溃写入，且任务项和故障表均无副作用。
- Node 结构化错误日志包含完整 Unicode/空格路径以及任务、进程、阶段和退出码字段。
- Worker 崩溃后同槽位补建成功，队列中剩余文件能够被继续派发。

## 文件职责映射

- Modify: `crates/node-store/src/tasks.rs` —— 把阶段从崩溃归属硬校验中移除，并在同一事务内完成单文件失败与故障写入。
- Modify: `crates/node-store/tests/task_recovery.rs` —— 固定“阶段是诊断字段、不可变文件身份仍需一致”的存储行为。
- Modify: `crates/node-engine/src/worker/pool.rs` —— 在 Continue 边界更新 Node 运行阶段，持续携带完整文件路径直到终态事件。
- Modify: `crates/node-engine/tests/worker_pipeline.rs` —— 验证两段式 Worker 会话的路径和阶段生命周期。
- Modify: `crates/node-engine/src/scan/base_compute.rs` —— 先输出完整路径日志并提交崩溃终态，再释放基础计算活动项；文件失败不终止任务。
- Modify: `crates/node-engine/Cargo.toml` —— 仅增加日志捕获测试使用的 `tracing-subscriber` 开发依赖。
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs` —— 验证一文件崩溃、后续文件成功、任务最终完成和故障路径完整。
- Modify: `apps/worker/tests/worker_pool.rs` —— 使用真实 `worker.exe` 验证续算阶段退出事件保留完整路径并补建 Worker。

---

### Task 1: 将崩溃阶段改为诊断信息并保持事务原子性

**Files:**
- Modify: `crates/node-store/tests/task_recovery.rs`
- Modify: `crates/node-store/src/tasks.rs:407-472`

**Interfaces:**
- Preserves: `NodeStore::fail_running_item_with_file_fault(&mut self, item_id: &str, fault: &FileFaultRecord, error: &str, now_ms: i64) -> Result<TaskEvent, StoreError>`。
- Preserves identity guard: `status == running`，且 `machine_id/normalized_path/file_size` 与持久任务项一致。
- Changes: `fault.stage` 只写入 `file_faults.stage`，不再与 `task_items.stage` 做相等判断。
- Produces: 故障记录使用任务项已经冻结的完整 `display_path`，避免错误事件覆盖权威路径。

- [ ] **Step 1: 写入阶段不同仍能落库的 RED 测试**

在 `crates/node-store/tests/task_recovery.rs` 增加：

```rust
#[test]
fn crashed_item_accepts_diagnostic_stage_and_keeps_full_display_path() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let root = NormalizedPath::new(r"I:\媒体库").unwrap();
    let normalized = NormalizedPath::new(r"I:\媒体库\歌手 A\现场\崩溃样本.mp4").unwrap();
    let display = DisplayPath::new(r"I:\媒体库\歌手 A\现场\崩溃样本.mp4").unwrap();
    let task = store.create_scan_task(&[root], 10).unwrap();
    let item_id = store
        .reserve_scan_path(
            task,
            &ScannedPath::new(normalized.clone(), display.clone(), 4096),
            11,
        )
        .unwrap()
        .unwrap();

    store
        .fail_running_item_with_file_fault(
            &item_id,
            &FileFaultRecord {
                machine_id: machine(),
                normalized_path: normalized,
                display_path: display.clone(),
                file_size: 4096,
                kind: FileFaultKind::WorkerCrash,
                stage: "base_compute".into(),
                windows_error_code: None,
                read_offset: None,
                read_size: None,
                worker_pid: Some(10528),
                worker_exit_code: Some(0xc000_0374_u32 as i32),
                first_seen_at_ms: 12,
                last_seen_at_ms: 12,
                occurrence_count: 1,
                message: "Worker 管道断开".into(),
            },
            "Worker 管道断开",
            12,
        )
        .unwrap();

    let fault = store.page_file_faults(None, 10).unwrap().items.remove(0);
    assert_eq!(fault.display_path.as_path(), display.as_path());
    assert_eq!(fault.stage, "base_compute");
    assert_eq!(fault.worker_pid, Some(10528));
    assert_eq!(fault.worker_exit_code, Some(0xc000_0374_u32 as i32));
}
```

该测试中持久任务项仍处于 `enumerated`，故障诊断阶段为 `base_compute`，用于精确复现当前硬校验失败。

- [ ] **Step 2: 运行 RED 并确认失败原因唯一**

```powershell
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-store --test task_recovery crashed_item_accepts_diagnostic_stage_and_keeps_full_display_path --locked -- --exact --test-threads=1
```

Expected: FAIL，错误为“Worker 崩溃事件身份与持久任务项不一致”；不得出现数据库 schema 或编译环境错误。

- [ ] **Step 3: 实现最小存储修复**

在 `fail_running_item_with_file_fault` 中不再查询和比较持久 `stage`，保留其余不可变身份检查，并用 SQLite 任务项的显示路径覆盖待写故障记录：

```rust
let identity_matches = status == "running"
    && fault.machine_id == persisted_machine
    && fault.normalized_path == persisted_normalized
    && fault.file_size == persisted_size
    && fault.windows_error_code.is_none();
if !identity_matches {
    return Err(StoreError::InvalidState(
        "Worker 崩溃事件身份与持久任务项不一致".into(),
    ));
}

let mut persisted_fault = fault.clone();
persisted_fault.display_path = persisted_display;
let event = complete_item_in_transaction(
    &transaction,
    item_id,
    TaskItemCompletion::Failed(error.to_owned()),
    now_ms,
)?;
upsert_file_fault_in_transaction(&transaction, &persisted_fault)?;
```

`complete_item_in_transaction` 和 `upsert_file_fault_in_transaction` 必须继续共享同一个事务；任何一步失败都不得留下半完成状态。

- [ ] **Step 4: 运行 GREEN 与身份拒绝回归**

```powershell
cargo test -p dedup-node-store --test task_recovery crashed_item_accepts_diagnostic_stage_and_keeps_full_display_path --locked -- --exact --test-threads=1
cargo test -p dedup-node-store --test task_recovery crashed_item_rejects_mismatched_fault_identity_without_side_effects --locked -- --exact --test-threads=1
cargo test -p dedup-node-store --test task_recovery crashed_item_is_failed_with_fault_and_never_requeued_after_reopen --locked -- --exact --test-threads=1
```

Expected: 3 个测试 PASS；阶段不同可以提交，规范路径/大小不一致仍被拒绝，崩溃终态重启后不会复活。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/node-store/src/tasks.rs crates/node-store/tests/task_recovery.rs
git commit -m "fix: treat worker crash stage as diagnostic"
```

---

### Task 2: WorkerPool 在续算边界同步阶段并保留完整路径

**Files:**
- Modify: `crates/node-engine/src/worker/pool.rs:125-140, 690-830, 1364-1401`
- Modify: `crates/node-engine/tests/worker_pipeline.rs`

**Interfaces:**
- Preserves: `WorkerFileIdentity` 现有字段和所有外部 dispatch 方法签名。
- Produces helper: `update_work_stage(work: &mut WorkIdentity, stage: &str)`，只更新 Node 保存的当前阶段，不改变文件路径身份。
- Changes: 成功接受 `ContinueBaseCompute` 后，真实池 `PoolState.running`、发送给 slot 的 `WorkIdentity` 和可控测试池 `active/PoolState.running` 同步变为 `base_compute`。
- Preserves lifecycle: `display_path` 从 dispatch 到 `Completed/Crashed/Cancelled` 一直存在；`Crashed` 事件取得路径所有权后运行映射才移除，NodeEngine 的 `active` 项继续保留到崩溃事务提交。

- [ ] **Step 1: 写入路径与阶段生命周期 RED 测试**

在 `crates/node-engine/tests/worker_pipeline.rs` 增加一个使用可控池的测试。请求必须使用完整 Unicode/空格路径，并在 Continue 后、Crash 前检查运行映射：

```rust
#[tokio::test]
async fn base_continue_updates_retained_crash_context_until_terminal_event() {
    let (mut pool, _started, control) = WorkerPool::controlled_batch_for_test(1);
    let full_path = r"I:\媒体库\歌手 A\现场\崩溃样本.mp4";
    let identity = WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x71; 32]),
        normalized_path: dedup_core::NormalizedPath::new(full_path).unwrap(),
        display_path: DisplayPath::new(full_path).unwrap(),
        file_size: 4096,
        stage: "base_hash".into(),
        physical_disk_id: "disk-7".into(),
    };
    pool.dispatch_runtime(base_begin_request("task-base", "item-base", full_path), identity)
        .await
        .unwrap();
    assert!(matches!(pool.next_event().await, Some(WorkerEvent::Started { .. })));

    control
        .base_hash_ready("task-base".into(), "item-base".into(), [7; 16])
        .await;
    assert!(matches!(pool.next_event().await, Some(WorkerEvent::BaseHashReady { .. })));
    pool.continue_base_compute(
        "item-base",
        proto::ContinueBaseCompute {
            task_id: "task-base".into(),
            item_id: "item-base".into(),
            media_kind: proto::MediaKind::MediaVideo as i32,
            missing_parts: BASE_MISSING_PROBE,
        },
    )
    .await
    .unwrap();

    let running = control.running_files();
    assert_eq!(running.len(), 1);
    assert_eq!(running[0].2.display_path.as_path(), Path::new(full_path));
    assert_eq!(running[0].2.stage, "base_compute");

    control
        .crash("task-base".into(), "item-base".into(), "worker exited".into())
        .await;
    let Some(WorkerEvent::Crashed { identity, .. }) = pool.next_event().await else {
        panic!("续算 Worker 崩溃必须返回文件级事件");
    };
    assert_eq!(identity.display_path.as_path(), Path::new(full_path));
    assert_eq!(identity.stage, "base_compute");
    assert!(control.running_files().is_empty());
}
```

同时增加测试辅助函数，字段必须与 `BeginBaseCompute` 当前 V4 协议一致：

```rust
/// 构造只用于 WorkerPool 状态测试的基础计算首段请求。
fn base_begin_request(task_id: &str, item_id: &str, path: &str) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::BeginBaseCompute(
            proto::BeginBaseCompute {
                task_id: task_id.into(),
                item_id: item_id.into(),
                machine_id: "71".repeat(32),
                normalized_path: path.into(),
                display_path: path.into(),
                file_size: 4096,
                physical_disk_id: "disk-7".into(),
                block_size_bytes: 64 * 1024,
                block_timeout_ms: 3_000,
                block_retries: 2,
            },
        )),
    }
}
```

测试导入同时调整为：

```rust
use dedup_protocol::{BASE_MISSING_PROBE, proto::{self, worker_envelope}};
```

再增加首段崩溃用例，固定 `base_hash` 期间也保留完整路径：

```rust
#[tokio::test]
async fn base_hash_crash_keeps_dispatch_path_until_terminal_event() {
    let (mut pool, _started, control) = WorkerPool::controlled_batch_for_test(1);
    let full_path = r"I:\媒体库\歌手 B\首段崩溃.mp4";
    let identity = WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x72; 32]),
        normalized_path: dedup_core::NormalizedPath::new(full_path).unwrap(),
        display_path: DisplayPath::new(full_path).unwrap(),
        file_size: 4096,
        stage: "base_hash".into(),
        physical_disk_id: "disk-8".into(),
    };
    pool.dispatch_runtime(base_begin_request("task-hash", "item-hash", full_path), identity)
        .await
        .unwrap();
    assert!(matches!(pool.next_event().await, Some(WorkerEvent::Started { .. })));

    control
        .crash("task-hash".into(), "item-hash".into(), "worker exited".into())
        .await;
    let Some(WorkerEvent::Crashed { identity, .. }) = pool.next_event().await else {
        panic!("MD5 阶段崩溃必须返回文件级事件");
    };
    assert_eq!(identity.display_path.as_path(), Path::new(full_path));
    assert_eq!(identity.stage, "base_hash");
    assert!(control.running_files().is_empty());
}
```

- [ ] **Step 2: 运行 RED**

```powershell
cargo test -p dedup-node-engine --test worker_pipeline base_continue_updates_retained_crash_context_until_terminal_event --locked -- --exact --test-threads=1
```

Expected: FAIL；Continue 后 `running[0].2.stage` 仍为 `base_hash`，崩溃事件也携带旧阶段。

- [ ] **Step 3: 在真实池和可控池共用阶段更新逻辑**

在 `WorkIdentity` 附近增加：

```rust
/// 更新 Node 保存的 Worker 当前处理阶段；文件路径及大小身份保持冻结。
fn update_work_stage(work: &mut WorkIdentity, stage: &str) {
    if let Some(identity) = work.file_identity.as_mut() {
        identity.stage = stage.to_owned();
    }
}
```

真实 `continue_base_slot` 必须先校验 task/item/awaiting 状态，再更新发送副本和 `PoolState.running`：

```rust
let mut work = work;
update_work_stage(&mut work, "base_compute");
slot.commands
    .send(SlotCommand::ContinueBaseCompute {
        work: work.clone(),
        envelope: proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ContinueBaseCompute(command)),
        },
    })
    .map_err(|_| WorkerPoolError::Closed)?;
locked.running.insert(slot_id, work);
locked.awaiting_continue.remove(&slot_id);
```

可控池接受 Continue 后也调用 `update_work_stage`，并把更新后的 `WorkIdentity` 同时写回 `active` 和 `actor_state.running`。这保证可控集成测试与真实 actor 使用相同状态语义。

更新 `WorkerFileIdentity`、`WorkIdentity` 和 `running_files` 的中文注释，明确只有 `stage` 可变，完整路径在终态前保留。

- [ ] **Step 4: 运行 GREEN 与现有调度竞态门禁**

```powershell
cargo test -p dedup-node-engine --test worker_pipeline base_continue_updates_retained_crash_context_until_terminal_event --locked -- --exact --test-threads=1
cargo test -p dedup-node-engine --test worker_pipeline base_hash_crash_keeps_dispatch_path_until_terminal_event --locked -- --exact --test-threads=1
cargo test -p dedup-node-engine --test worker_pipeline crash_fault_fails_file_a_once_while_file_b_completes_and_slot_is_replaced --locked -- --exact --test-threads=1
cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1
```

Expected: PASS；Continue 后路径不变、阶段更新，Crash 后映射释放；原扫描崩溃隔离、取消和补位测试不回归。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/node-engine/src/worker/pool.rs crates/node-engine/tests/worker_pipeline.rs
git commit -m "fix: retain worker file context through base continuation"
```

---

### Task 3: Node 记录完整崩溃路径并把崩溃限制为单文件失败

**Files:**
- Modify: `crates/node-engine/Cargo.toml`
- Modify: `crates/node-engine/src/scan/base_compute.rs:680-835`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`

**Interfaces:**
- Produces internal helper: `log_worker_crash(task_id: &str, item_id: &str, identity: &WorkerFileIdentity, worker_pid: Option<u32>, worker_exit_code: Option<i32>, message: &str)`，统一输出完整结构化崩溃日志。
- Preserves: `WorkerEvent::Crashed` 由 Node 消费，`fail_running_item_with_file_fault` 同一事务写任务项和故障。
- Changes lifecycle: `active.get(item_id)` 读取并记录路径；SQLite 提交成功后才 `active.remove(item_id)` 释放路径和磁盘许可。
- Produces outcome: 当前项 `failed`、`summary.file_failures += 1`、`summary.skipped_incomplete += 1`，事件循环继续等待/派发其他文件。

- [ ] **Step 1: 写入基础计算继续执行 RED 集成测试**

在 `crates/node-engine/tests/base_compute_pipeline.rs` 增加 `worker_crash_after_continue_fails_one_file_and_task_completes`。固定使用一个 Worker 槽位和两个文件：

```rust
#[tokio::test]
async fn worker_crash_after_continue_fails_one_file_and_task_completes() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("安装目录");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x72; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("媒体 库");
    let root = DisplayPath::new(&media_root).unwrap();
    let options = ScanOptions::new(vec![root]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let first_path = media_root.join("崩溃文件.mp4");
    let second_path = media_root.join("正常文件.bin");
    let rows = vec![
        ScannedPath::new(
            NormalizedPath::new(&first_path).unwrap(),
            DisplayPath::new(&first_path).unwrap(),
            4096,
        ),
        ScannedPath::new(
            NormalizedPath::new(&second_path).unwrap(),
            DisplayPath::new(&second_path).unwrap(),
            4096,
        ),
    ];
    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (reader, _) = ScheduledFileReader::controlled_for_test(&config, 1, NeverRead, |_| {
        (vec![1], LocalDiskKind::Hdd)
    })
    .unwrap();
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "基础计算")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    let mut remote = DisabledRemoteFeatureCache;
    let task_text = task_id.as_uuid().to_string();

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        &mut remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let first_item = started.recv().await.unwrap().1;
        let crashed_path = controller
            .running_files()
            .into_iter()
            .find(|(_, item_id, _)| item_id == &first_item)
            .expect("Node 必须在 Worker 结束前保留当前文件路径")
            .2
            .display_path;
        controller
            .base_hash_ready(task_text.clone(), first_item.clone(), [1; 16])
            .await;
        wait_for_continue(&controller, &first_item).await;
        controller
            .crash(task_text.clone(), first_item, "Worker 管道断开".into())
            .await;

        let second_item = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("补建槽位必须继续派发后续文件")
            .unwrap()
            .1;
        controller
            .base_hash_ready(task_text.clone(), second_item.clone(), [2; 16])
            .await;
        wait_for_continue(&controller, &second_item).await;
        controller
            .complete_base(task_text, second_item, [2; 16], other_output())
            .await;
        crashed_path
    };

    let (summary, crashed_path) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.file_failures, 1);
    assert_eq!(store.task_snapshot(task_id).unwrap().status, TaskStatus::Completed);
    let items = store.task_items(task_id).unwrap();
    assert_eq!(items.iter().filter(|item| item.status == TaskItemStatus::Failed).count(), 1);
    assert_eq!(items.iter().filter(|item| item.status == TaskItemStatus::Succeeded).count(), 1);
    let fault = store.page_file_faults(None, 10).unwrap().items.remove(0);
    assert_eq!(fault.display_path, crashed_path);
    assert_eq!(fault.stage, "base_compute");
    assert_eq!(fault.kind, dedup_node_store::FileFaultKind::WorkerCrash);
    let failures = registry.details(reporter.id()).await.unwrap().failures;
    assert_eq!(
        failures[0].display_path.as_str(),
        crashed_path.as_path().to_string_lossy().as_ref()
    );
}
```

- [ ] **Step 2: 运行 RED 并确认任务被阶段校验阻塞**

```powershell
cargo test -p dedup-node-engine --test base_compute_pipeline worker_crash_after_continue_fails_one_file_and_task_completes --locked -- --exact --test-threads=1
```

Expected: FAIL；修复前 `run_existing` 返回“Worker 崩溃事件身份与持久任务项不一致”，第二个文件无法形成成功终态。

- [ ] **Step 3: 增加结构化日志并调整活动路径释放顺序**

在 `base_compute.rs` 增加一个小型日志函数，集中固定字段名：

```rust
/// 记录 Node 观察到的 Worker 崩溃及完整文件上下文，供现场日志直接定位文件。
fn log_worker_crash(
    task_id: &str,
    item_id: &str,
    identity: &WorkerFileIdentity,
    worker_pid: Option<u32>,
    worker_exit_code: Option<i32>,
    message: &str,
) {
    tracing::error!(
        task_id = %task_id,
        item_id = %item_id,
        file_path = %identity.display_path.as_path().display(),
        normalized_path = %identity.normalized_path.as_str(),
        crash_stage = %identity.stage,
        worker_pid = ?worker_pid,
        worker_exit_code = ?worker_exit_code,
        error = %message,
        "Worker 计算文件时崩溃，当前文件已标记失败并继续任务"
    );
}
```

`WorkerEvent::Crashed` 分支按以下顺序执行：

1. 使用事件身份立即调用 `log_worker_crash`，此时路径仍由事件和 `active` 映射共同持有。
2. 构造 `FileFaultRecord`，调用 `fail_running_item_with_file_fault` 原子提交单文件失败与故障。
3. 提交成功后调用 `active.remove(&item_id)`，释放文件路径、磁盘许可和运行时槽位详情。
4. 更新 `summary`、运行时失败列表和阶段进度，然后返回 `Ok(())` 继续事件循环。

如果 `active` 意外缺少该项，仍使用 `WorkerEvent::Crashed.identity.display_path` 写日志和故障；记录一条 invariant warning，但不得静默丢弃已确认的 Worker 崩溃。只有 SQLite 事务失败才向上传播任务级错误。

- [ ] **Step 4: 自动验证完整日志字段**

在 `crates/node-engine/Cargo.toml` 增加：

```toml
[dev-dependencies]
tempfile.workspace = true
tracing-subscriber.workspace = true
```

在 `base_compute.rs` 的 `#[cfg(test)]` 模块中使用内存 `MakeWriter` 调用 `log_worker_crash`，断言输出同时包含：

```rust
assert!(log.contains(r"I:\媒体库\歌手 A\现场\崩溃样本.mp4"));
assert!(log.contains("task_id=task-log"));
assert!(log.contains("item_id=item-log"));
assert!(log.contains("crash_stage=base_compute"));
assert!(log.contains("worker_pid=Some(10528)"));
assert!(log.contains("worker_exit_code=Some(-1073740940)"));
```

测试 writer 类型、缓冲区字段和 `Write/MakeWriter` 实现均添加中文注释；使用 `tracing::subscriber::with_default` 隔离 subscriber，不修改其他并行测试的全局日志状态。

- [ ] **Step 5: 运行 GREEN 与单文件失败回归**

```powershell
cargo test -p dedup-node-engine worker_crash_log_contains_full_path_and_process_context --locked -- --test-threads=1
cargo test -p dedup-node-engine --test base_compute_pipeline worker_crash_after_continue_fails_one_file_and_task_completes --locked -- --exact --test-threads=1
cargo test -p dedup-node-engine --test base_compute_pipeline invalid_single_file_result_is_logged_and_later_file_continues --locked -- --exact --test-threads=1
```

Expected: PASS；日志保留完整路径，崩溃文件失败，后续文件成功，父任务完成。

- [ ] **Step 6: 准备精确提交**

```powershell
git add -- crates/node-engine/Cargo.toml crates/node-engine/src/scan/base_compute.rs crates/node-engine/tests/base_compute_pipeline.rs
git commit -m "fix: isolate worker crashes and log full file paths"
```

---

### Task 4: 真实 Worker 退出、补建和最终验证

**Files:**
- Modify: `apps/worker/tests/worker_pool.rs`
- Verify only: `I:\Tool\mySingerServer-rust-v2-win-x64\runtime\ffmpeg`

**Interfaces:**
- Consumes: Task 2 的 Continue 阶段同步和 `WorkerEvent::Crashed.identity`。
- Produces acceptance evidence: 真实 `worker.exe` 在续算期间被终止后，崩溃事件仍含完整路径和 `base_compute`，同槽位补建新 PID。

- [ ] **Step 1: 增加真实进程 RED/GREEN 测试**

在 `apps/worker/tests/worker_pool.rs` 增加导入：

```rust
use dedup_core::{DisplayPath, MachineId, NormalizedPath};
use dedup_node_engine::worker::WorkerFileIdentity;
```

然后增加完整真实进程用例：

```rust
#[tokio::test]
async fn real_base_continue_crash_keeps_full_path_and_replaces_worker() {
    let Some(runtime) = runtime_fixture() else {
        return;
    };
    let media_root = runtime.path().join("媒体 库");
    std::fs::create_dir_all(&media_root).unwrap();
    let source = media_root.join("崩溃文件.jpg");
    std::fs::copy(media_fixture("image.jpg"), &source).unwrap();
    let worker = runtime.path().join("worker.exe");
    let config = WorkerPoolConfig::new(WorkerLaunch::new(worker), 1)
        .with_result_read_delay(Duration::from_millis(500));
    let mut pool = WorkerPool::start(config).await.unwrap();
    let crash_pid = pool.worker_process_ids()[0];
    let identity = WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x73; 32]),
        normalized_path: NormalizedPath::new(&source).unwrap(),
        display_path: DisplayPath::new(&source).unwrap(),
        file_size: std::fs::metadata(&source).unwrap().len(),
        stage: "base_hash".into(),
        physical_disk_id: "disk-real".into(),
    };
    pool.dispatch_runtime(base_begin_request(&source), identity)
        .await
        .unwrap();
    assert!(matches!(next_event(&mut pool).await, WorkerEvent::Started { .. }));

    let WorkerEvent::BaseHashReady { md5, .. } = next_event(&mut pool).await else {
        panic!("真实 Worker 必须先返回 MD5");
    };
    assert_eq!(md5.len(), 16);
    pool.continue_base_compute(
        "item-base",
        proto::ContinueBaseCompute {
            task_id: "task-base".into(),
            item_id: "item-base".into(),
            media_kind: proto::MediaKind::MediaImage as i32,
            missing_parts: BASE_MISSING_PROBE | BASE_MISSING_STAGE1,
        },
    )
    .await
    .unwrap();
    pool.terminate_worker_for_test(crash_pid).await.unwrap();

    let WorkerEvent::Crashed {
        identity,
        process_id,
        ..
    } = next_event(&mut pool).await else {
        panic!("续算期间退出必须产生文件级崩溃事件");
    };
    assert_eq!(identity.display_path.as_path(), source.as_path());
    assert_eq!(identity.stage, "base_compute");
    assert_eq!(process_id, Some(crash_pid));
    wait_for_replacement(&pool, crash_pid).await;
}
```

- [ ] **Step 2: 使用部署目录的真实 FFmpeg DLL 运行进程测试**

```powershell
$env:DEDUP_FFMPEG_TEST_SOURCE_DIR='I:\Tool\mySingerServer-rust-v2-win-x64\runtime\ffmpeg'
cargo test -p worker --test worker_pool real_base_continue_crash_keeps_full_path_and_replaces_worker --locked -- --exact --test-threads=1
```

Expected: PASS；事件路径完整、阶段为 `base_compute`、PID 对应被终止进程、替代 Worker 成功 Ready。

- [ ] **Step 3: 运行聚焦验证门禁**

```powershell
cargo fmt --all -- --check
cargo test -p dedup-node-store --test task_recovery --locked -- --test-threads=1
cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p worker --test worker_pool real_base_continue_crash_keeps_full_path_and_replaces_worker --locked -- --exact --test-threads=1
cargo test -p worker --test worker_pool base_session_keeps_slot_busy_until_continue_result --locked -- --exact --test-threads=1
cargo clippy -p dedup-node-store -p dedup-node-engine -p worker --tests --locked -- -D warnings
```

Expected: 全部 PASS；若真实 Worker 测试因 `DEDUP_FFMPEG_TEST_SOURCE_DIR` 缺失而跳过，不得把跳过报告为运行时验收通过，必须补齐 DLL 路径后重跑。

- [ ] **Step 4: 核对精确差异与验收矩阵**

```powershell
git diff -- crates/node-store/src/tasks.rs crates/node-store/tests/task_recovery.rs crates/node-engine/Cargo.toml crates/node-engine/src/worker/pool.rs crates/node-engine/src/scan/base_compute.rs crates/node-engine/tests/worker_pipeline.rs crates/node-engine/tests/base_compute_pipeline.rs apps/worker/tests/worker_pool.rs
git status --short -- docs/superpowers/plans/2026-08-23-worker-crash-task-continuation.md crates/node-store/src/tasks.rs crates/node-store/tests/task_recovery.rs crates/node-engine/Cargo.toml crates/node-engine/src/worker/pool.rs crates/node-engine/src/scan/base_compute.rs crates/node-engine/tests/worker_pipeline.rs crates/node-engine/tests/base_compute_pipeline.rs apps/worker/tests/worker_pool.rs
```

逐项确认：

- `base_hash` 和 `base_compute` 崩溃都只失败当前文件。
- 两文件测试最终任务为 `completed`，且成功/失败各一项。
- Node 日志和 `file_faults` 均保存完整路径。
- immutable identity 不一致仍被拒绝且无事务副作用。
- Worker 新 PID 补建，剩余文件继续调度。
- 未修改 Protobuf、SQLite schema、任务状态枚举及 FFmpeg 解码实现。

- [ ] **Step 5: 准备最终精确提交**

```powershell
git add -- apps/worker/tests/worker_pool.rs docs/superpowers/plans/2026-08-23-worker-crash-task-continuation.md
git commit -m "test: verify worker crash path and recovery"
```

## 完成定义

只有以下条件同时成立才能声明本问题修复完成：

1. 存储、WorkerPool、基础计算和真实 Worker 四组聚焦测试均为当前代码的新鲜 PASS。
2. 测试明确覆盖 `BaseHashReady` 之后的 Worker 崩溃，而不只是 dispatch 前或普通一筛请求崩溃。
3. Node 日志断言包含完整路径和全部结构化上下文字段。
4. SQLite 故障记录使用完整持久显示路径，阶段为实际崩溃阶段。
5. 同一任务剩余文件继续计算并最终 `completed`；单文件失败计数正确。
6. 工作树中用户原有改动保持不变，只审阅和暂存本计划列出的精确文件。
