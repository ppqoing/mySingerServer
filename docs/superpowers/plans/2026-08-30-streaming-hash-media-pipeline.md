# Hash 与 Media 单项查询流式流水线 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 每个 Hash 完成后立即执行一次 ContentKey 单项缓存查询，并在其他 Hash 与 Media 仍在运行时继续调度，删除 Hash→Media 整批屏障。

**Architecture:** 一个 `task_file` 事件泵串行拥有 dispatcher、任务上下文和状态迁移，同时观察有界 Hash future、远端单项查询、Worker 事件与 SQLite ACK。扫描前路径缓存仍按最多 1000 项批量查询；只有 Hash 后的 ContentKey 查询改为每次一个键，所有 `P -> C/F` 仍由 SQLite ACK 驱动。

**Tech Stack:** Rust 2024、Tokio 1.53、rusqlite、Protobuf Worker 协议、`TaskFileDispatcher`、`DiskReadScheduler`、`WorkerPool`、`BaseStoreActor`。

**Spec:** `docs/superpowers/specs/2026-08-30-streaming-hash-media-pipeline-design.md`

## Global Constraints

- 不修改 SQLite schema、TSV 八列格式、Worker Protobuf、媒体算法或物理盘权重配置。
- 扫描前 `lookup_base_cache_by_paths` 保持批量查询；Hash 后只调用 `lookup_base_cache_by_key`。
- 每个远端 ContentKey 请求也只包含一个键；远端命中导入 SQLite 后允许单项回查一次取得本地 `content_id`。
- Hash permit 在读取 future 完成时释放；Media permit 只在 `BaseSourceReadComplete` 后释放。
- Hash/远端查询在途不超过 `hash_capacity`，Media 活动不超过 `worker_capacity`。
- 取消不写 `F`；只有 SQLite ACK 成功后才把 TSV 行从 `P` 改为 `C/F`。
- 新增方法、类型、字段和关键变量添加简洁中文注释。
- 不启动真实媒体、不打包、不部署、不触碰 `I:\Tool`。
- Cargo 使用 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`；清除 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，设置 `CARGO_INCREMENTAL=0`、`CARGO_PROFILE_DEV_DEBUG=0`、`CARGO_PROFILE_TEST_DEBUG=0`。
- C 或 D 盘可用空间低于 10 GiB 时停止重型命令，只清理项目内可再生 Cargo target。

---

## File Structure

- `crates/node-store/src/content.rs`：单 ContentKey 完整基础缓存查询。
- `crates/node-engine/src/scan/base_persistence.rs`：经 SQLite actor 暴露单项查询及测试观测。
- `crates/node-engine/src/scan/task_file_base_compute.rs`：可逐项 join 的 Hash 运行态和单项缓存判定。
- `crates/node-engine/src/scan/task_file_media_compute.rs`：可逐 Worker 事件推进的 Media 运行态。
- `crates/node-engine/src/scan/task_file_media_persistence.rs`：可逐条投递、逐 ACK 应用的持久化运行态。
- `crates/node-engine/src/scan/task_file_base_stream.rs`：唯一 Hash/远端/Media/ACK 事件泵。
- `crates/node-engine/src/scan/task_file_base_coordinator.rs`：公共入口、结果包装和行为测试。
- `crates/node-engine/src/scan/task_file_scan_run.rs`、`crates/node-engine/src/actor.rs`：远端缓存按值进入并在流结束后收回。
- `AGENTS.md`、`docs/verification/2026-08-30-streaming-hash-media-pipeline.md`：长期约束与验证证据。

---

### Task 1: 增加 ContentKey 单项缓存查询入口

**Files:**
- Modify: `crates/node-store/src/content.rs:85-105`
- Modify: `crates/node-store/tests/content_cache.rs:300-335`
- Modify: `crates/node-engine/src/scan/base_persistence.rs:560-590`

**Interfaces:**
- Consumes: `ContentKey`、`BaseCacheRecord`、`NodeStore::lookup_key_cache_batch`。
- Produces: `NodeStore::lookup_base_cache_by_key(&ContentKey) -> Result<Option<BaseCacheRecord>, StoreError>`。
- Produces: `BaseStoreHandle::lookup_base_cache_by_key(&ContentKey) -> Result<Option<BaseCacheRecord>, StoreError>`。

- [ ] **Step 1: 写单项查询 RED 测试**

在 `content_cache.rs` 新增真实 SQLite 测试：

```rust
#[test]
fn lookup_base_cache_by_key_returns_one_complete_record() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"C:\single.bin", 31),
            [0x31; 16],
            MediaKind::Other,
        )
        .unwrap();
    store.mark_base_complete(content.id).unwrap();
    assert_eq!(
        store.lookup_base_cache_by_key(&content.key).unwrap().unwrap().content_key,
        content.key
    );
    assert!(store
        .lookup_base_cache_by_key(&ContentKey::new([0xFF; 16], 999))
        .unwrap()
        .is_none());
}
```

- [ ] **Step 2: 运行测试确认 RED**

Run: `cargo test -p dedup-node-store --test content_cache lookup_base_cache_by_key_returns_one_complete_record --locked -- --test-threads=1`

Expected: 编译失败，缺少 `lookup_base_cache_by_key`。

- [ ] **Step 3: 实现 NodeStore 单项入口**

```rust
/// 加载一个内容键对应的完整基础缓存；不等待或合并其他内容键。
pub fn lookup_base_cache_by_key(
    &self,
    key: &ContentKey,
) -> Result<Option<BaseCacheRecord>, StoreError> {
    let mut records = self.lookup_key_cache_batch(std::slice::from_ref(key))?;
    records
        .pop()
        .ok_or_else(|| StoreError::InvalidState("单项基础缓存查询没有返回对应位置".into()))
}
```

复用既有图片、视频和帧表装载，不复制 SQL，也不经过批次容量计算。

- [ ] **Step 4: 实现 BaseStoreHandle 单项入口**

```rust
/// 经 SQLite 单写 actor 查询一个 ContentKey，不等待组成批次。
pub(crate) fn lookup_base_cache_by_key(
    &self,
    key: &ContentKey,
) -> Result<Option<BaseCacheRecord>, StoreError> {
    #[cfg(test)]
    self.key_lookup_batches.lock().expect("查询观测锁不应中毒").push(1);
    let key = *key;
    self.call(move |store| store.lookup_base_cache_by_key(&key))
}
```

- [ ] **Step 5: 运行定向和 NodeStore 全量测试**

Run: `cargo test -p dedup-node-store --test content_cache lookup_base_cache_by_key_returns_one_complete_record --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-store --locked -- --test-threads=1`

Expected: 全部 PASS。

- [ ] **Step 6: 检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "feat: add single content cache lookup"`

---

### Task 2: 提取可逐事件推进的 Hash、Media 和持久化运行态

**Files:**
- Modify: `crates/node-engine/src/scan/task_file_base_compute.rs:100-620`
- Modify: `crates/node-engine/src/scan/task_file_media_compute.rs:25-340`
- Modify: `crates/node-engine/src/scan/task_file_media_persistence.rs:65-730`

**Interfaces:**
- Consumes: Task 1 的 `BaseStoreHandle::lookup_base_cache_by_key`。
- Produces: `TaskFileHashRuntime`、`TaskFileMediaRuntime<P>`、`TaskFilePersistRuntime`。

- [ ] **Step 1: 写 Hash 运行态 RED 测试**

```rust
#[tokio::test]
async fn hash_runtime_returns_one_ready_item_without_draining_window() {
    let gate = Arc::new(Notify::new());
    let mut runtime = TaskFileHashRuntime::new(2);
    let (first, second) = two_dispatched_hash_tasks();
    let reader = GatedHashReader::new([0x41; 16], Arc::clone(&gate));
    runtime.spawn(first, reader.clone(), ReadCancellationToken::new()).unwrap();
    runtime.spawn(second, reader, ReadCancellationToken::new()).unwrap();
    let first = timeout(Duration::from_secs(1), runtime.join_one()).await.unwrap().unwrap();
    assert_eq!(first.result.unwrap(), [0x41; 16]);
    assert_eq!(runtime.active_len(), 1);
    gate.notify_waiters();
    runtime.cancel_and_join().await;
}
```

- [ ] **Step 2: 运行并确认 RED**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib hash_runtime_returns_one_ready_item_without_draining_window --locked -- --test-threads=1`

Expected: 缺少 `TaskFileHashRuntime`。

- [ ] **Step 3: 实现 Hash 运行态**

```rust
pub(super) struct TaskFileHashRuntime {
    reads: JoinSet<HashReadOutcome>,
    unclaimed_rows: usize,
    next_sequence: usize,
}

impl TaskFileHashRuntime {
    pub(super) fn new(remaining_hash_rows: usize) -> Self;
    pub(super) fn can_dispatch(&self, hash_capacity: usize) -> bool;
    pub(super) fn spawn<P, H>(&mut self, task: DispatchedTask<P>, reader: H, cancellation: ReadCancellationToken) -> Result<(), String>
    where P: Send + 'static, H: HashPermitReader<Permit = P>;
    pub(super) async fn join_one(&mut self) -> Result<HashReadOutcome, String>;
    pub(super) fn active_len(&self) -> usize;
    pub(super) fn is_finished(&self) -> bool;
    pub(super) async fn cancel_and_join(&mut self);
}
```

`join_one` 只取一个完成项；删除生产路径中的全量 `outcomes Vec`。

测试模块同时新增 `two_dispatched_hash_tasks() -> (DispatchedTask<TestPermit>, DispatchedTask<TestPermit>)`，使用 `TaskFileIdentity::new`、两个 `TaskFileRecord` 和不同显示路径构造确定身份；`GatedHashReader` 按第一个路径立即返回、第二个路径等待 `Notify`，并在 cancellation 时返回 `ReadFailure::Cancelled`。

- [ ] **Step 4: 写并实现 Media 逐事件运行态**

先写 `media_runtime_handles_started_source_complete_and_terminal_separately`，依次注入 Started、SourceReadComplete、Completed，证明每次调用只消费一个事件。

```rust
pub(super) struct TaskFileMediaRuntime<P: TaskLanePermitProvider> {
    active: BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
    settled: BTreeSet<TaskFileIdentity>,
}

impl<P: TaskLanePermitProvider> TaskFileMediaRuntime<P> {
    pub(super) fn new() -> Self;
    pub(super) fn has_capacity(&self, worker_capacity: usize) -> bool;
    pub(super) fn has_active(&self) -> bool;
    pub(super) async fn dispatch(&mut self, pending: &mut TaskFileBaseComputePending<P>, task: DispatchedTask<P::Permit>, worker_pool: &mut WorkerPool, store: &BaseStoreHandle, read_config: &DiskReadConfig, cancellation: &ReadCancellationToken) -> Result<(), String>;
    pub(super) async fn handle_event(&mut self, event: WorkerEvent, reporter: Option<&RuntimeTaskReporter>) -> Result<Option<TaskFileMediaTerminal>, String>;
    pub(super) async fn cancel_and_drain(&mut self, worker_pool: &WorkerPool, cancellation: &ReadCancellationToken);
}
```

终态类型固定为：

```rust
pub(super) enum TaskFileMediaTerminal {
    Completed(TaskFileMediaCompleted),
    Failed(TaskFileMediaFailure),
}
```

- [ ] **Step 5: 写并实现持久化逐 ACK 运行态**

先写 `persist_runtime_applies_only_acknowledged_identity`：两个身份入队，只应用第一条 ACK 时第一行 `C`、第二行仍 `P`。

```rust
pub(super) struct TaskFilePersistRuntime {
    queue: VecDeque<PendingTaskFilePersist>,
    in_flight: BTreeMap<TaskFileIdentity, TaskFilePersistAction>,
}

impl TaskFilePersistRuntime {
    pub(super) fn new() -> Self;
    pub(super) fn enqueue_hash_complete(&mut self, identity: TaskFileIdentity, scanned: ScannedPath, md5: [u8; 16], media_kind: MediaKind, content_key: ContentKey);
    pub(super) fn enqueue_hash_failure(&mut self, identity: TaskFileIdentity, scanned: ScannedPath, error: ReadFailure);
    pub(super) fn enqueue_media_terminal(&mut self, terminal: TaskFileMediaTerminal, options: &TaskFileMediaPersistenceOptions) -> Result<(), String>;
    pub(super) fn try_submit(&mut self, store: &BaseStoreHandle) -> Result<(), String>;
    pub(super) fn has_in_flight(&self) -> bool;
    pub(super) fn is_empty(&self) -> bool;
    pub(super) fn apply_ack<P: TaskLanePermitProvider>(&mut self, pending: &mut TaskFileBaseComputePending<P>, ack: BasePersistAck) -> Result<(), String>;
    pub(super) fn drop_unacknowledged(&mut self);
}
```

队列消息和 ACK 动作固定为：

```rust
struct PendingTaskFilePersist {
    identity: TaskFileIdentity,
    message: BasePersistMessage,
    action: TaskFilePersistAction,
}

enum TaskFilePersistAction {
    HashComplete { scanned: ScannedPath, content_key: ContentKey },
    HashFailed,
    MediaComplete {
        scanned: ScannedPath,
        content_key: ContentKey,
        media_kind: MediaKind,
        worker_slot: Option<u32>,
    },
    MediaFailed { display_path: String, worker_slot: Option<u32> },
}
```

`try_submit` 遇到 Full 时保留消息并返回，不在内部等待 ACK。

- [ ] **Step 6: 保持旧包装器测试 GREEN**

让现有 Hash/Media/persistence 完整阶段包装器暂时循环调用新运行态，Task 3 切换生产入口前不改变外部测试契约。

- [ ] **Step 7: 运行三个模块测试**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib task_file_base_compute --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib task_file_media_compute --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib task_file_media_persistence --locked -- --test-threads=1`

Expected: 全部 PASS。

- [ ] **Step 8: 检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "refactor: expose incremental base pipeline state"`

---

### Task 3: 实现统一 Hash/远端/Media/ACK 事件泵

**Files:**
- Create: `crates/node-engine/src/scan/task_file_base_stream.rs`
- Modify: `crates/node-engine/src/scan/mod.rs:10-25`
- Modify: `crates/node-engine/src/scan/task_file_base_coordinator.rs:190-320,430-760`
- Modify: `crates/node-engine/src/scan/task_file_scan_run.rs:80-375`
- Modify: `crates/node-engine/src/actor.rs:2970-3015`

**Interfaces:**
- Consumes: Task 1-2 的单项查询和三个增量运行态。
- Produces: `run_task_file_base_stream`，唯一生产协调循环。
- Produces: 按值进入并在结束后收回的 `R: RemoteFeatureCache`。

- [ ] **Step 1: 写真实协调器 RED 测试**

新增 `first_hashed_media_miss_enters_worker_before_later_hash_finishes`：两个未知 MD5 文件位于不同 lane，`hash_capacity=2`、`worker_capacity=1`。第一个 Hash 立即完成，第二个先通知 entered 再等待 gate。

```rust
later_hash_entered.notified().await;
let first_worker = timeout(Duration::from_secs(1), started.recv())
    .await
    .expect("首个 Hash 缺失项不得等待后续 Hash")
    .unwrap();
assert_eq!(observer.lookup_key_batch_sizes_for_test(), vec![1]);
later_hash_gate.notify_waiters();
timeout(Duration::from_secs(1), async {
    loop {
        if observer.lookup_key_batch_sizes_for_test() == vec![1, 1] {
            break;
        }
        tokio::task::yield_now().await;
    }
})
.await
.expect("第二个 Hash 应在首个 Media 未完成时执行单项查询");
// 此前没有向 controlled Worker 发送 Completed/Crashed，first_worker 仍然 active。
assert!(!first_worker.1.is_empty());
```

最后完成两个 Worker，断言两行均 `C`、resolved 两项、查询观测严格 `[1, 1]`。

- [ ] **Step 2: 运行并确认旧实现 RED**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib first_hashed_media_miss_enters_worker_before_later_hash_finishes --locked -- --test-threads=1`

Expected: 首个 Worker 在第二个 Hash gate 释放前未 Started，超时失败。

- [ ] **Step 3: 调整远端缓存所有权**

把 `run_task_file_scan*` 的 `remote: &mut R` 改为 `remote: R`，内部使用 `Arc<R>`。actor 按值传入 `NodeRemoteFeatureCache`。全部远端 future 结束后：

```rust
let mut remote = Arc::try_unwrap(remote)
    .map_err(|_| ScanError::Stage1("基础流结束时仍有远端缓存查询 owner".into()))?;
publish_final_outbox(&mut store, &mut remote, &mut remote_available, &mut warning).await;
```

- [ ] **Step 4: 实现单键远端运行态**

每个 remote future 只拥有一个 `ContentKey` 和一个 Hash 上下文，只调用 `lookup_contents(&[key])`。数量/键不匹配触发 local-only 降级；远端记录更完整时导入 SQLite，再调用 `lookup_base_cache_by_key` 取得 `content_id`。

```rust
struct RemoteLookupInput {
    hashed: HashedTask,
    local: Option<BaseCacheRecord>,
}

struct RemoteLookupOutput {
    input: RemoteLookupInput,
    result: Result<Option<BaseCacheRecord>, RemoteCacheError>,
}
```

- [ ] **Step 5: 实现统一事件循环**

```rust
pub(super) async fn run_task_file_base_stream<P, H, R>(
    pending: TaskFileBaseComputePending<P>,
    reader: H,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    options: &TaskFileBaseCoordinatorOptions,
    cancellation: ReadCancellationToken,
    remote: Arc<R>,
    remote_available: &mut bool,
    warning: &mut Option<String>,
    reporter: Option<&RuntimeTaskReporter>,
) -> Result<TaskFileBaseComputePending<P>, TaskFileBaseStreamError<P>>
where P: TaskLanePermitProvider, H: HashPermitReader<Permit = P::Permit>, R: RemoteFeatureCache;
```

错误必须携带唯一 pending owner：

```rust
pub(super) struct TaskFileBaseStreamError<P: TaskLanePermitProvider> {
    message: String,
    pending: TaskFileBaseComputePending<P>,
}

impl<P: TaskLanePermitProvider> TaskFileBaseStreamError<P> {
    pub(super) fn into_pending(self) -> TaskFileBaseComputePending<P> {
        self.pending
    }
}
```

`tokio::select!` 同时包含 ACK、Worker event、remote join、Hash join、dispatcher joint admission 和 10 ms 取消检查。每个分支只处理一个事件后返回循环顶部。Hash join 分支立即调用 `lookup_base_cache_by_key`，随后只进入 remote future、Media continuation、成功 persist 或失败 persist之一。

- [ ] **Step 6: 切换协调器生产入口**

`run_task_file_base_coordinator_inner` 删除顺序执行完整 Media/Hash/persist pass 的循环，改为调用一次流式事件泵；成功后复用 `finish_coordinator` 的 Drained 检查和 summary 构造。

- [ ] **Step 7: 运行主测试确认 GREEN**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib first_hashed_media_miss_enters_worker_before_later_hash_finishes --locked -- --test-threads=1`

Expected: PASS；第二次查询发生时第一个 Worker 仍 active。

- [ ] **Step 8: 运行协调器和 scan run 回归**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib task_file_base_coordinator --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib task_file_scan_run --locked -- --test-threads=1`

Expected: 全部 PASS。

- [ ] **Step 9: 检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "feat: stream hash results into media workers"`

---

### Task 4: 关闭取消、错误和资源守恒边界

**Files:**
- Modify: `crates/node-engine/src/scan/task_file_base_stream.rs`
- Modify: `crates/node-engine/src/scan/task_file_base_coordinator.rs`
- Modify: `crates/node-engine/src/scan/task_file_base_compute.rs`
- Modify: `crates/node-engine/src/scan/task_file_media_compute.rs`
- Modify: `crates/node-engine/src/scan/task_file_media_persistence.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`

**Interfaces:**
- Consumes: Task 3 的统一事件泵。
- Produces: Hash/远端/Media/ACK 统一 cleanup 和容量守恒证据。

- [ ] **Step 1: 写并发取消 RED 测试**

新增 `cancellation_drains_hash_remote_media_and_preserves_unacked_rows`：Media A active、Hash B gated、远端 C gated 时取消；断言两秒内返回 cancelled、Worker idle、所有未 ACK 行仍 `P`、dispatcher 可 `discard()`。

- [ ] **Step 2: 运行确认 RED**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib cancellation_drains_hash_remote_media_and_preserves_unacked_rows --locked -- --test-threads=1`

Expected: cleanup 未收束全部新 owner，失败或超时。

- [ ] **Step 3: 实现统一 cleanup**

```rust
cancellation.cancel();
hash.cancel_and_join().await;
remote_reads.abort_all();
while remote_reads.join_next().await.is_some() {}
media.cancel_and_drain(worker_pool, &cancellation).await;
persist.drop_unacknowledged();
abandon_all_in_flight(&mut pending)?;
```

新增 `abandon_all_in_flight<P>(&mut TaskFileBaseComputePending<P>) -> Result<(), String>`：收集 `pending.contexts.keys().cloned()`，逐个调用 `dispatcher.abandon_in_flight`；只用于所有 permit、远端 future 和 Worker 已经清空后的任务级收尾。

保留第一个权威错误；cleanup 错误只追加诊断。未 ACK 行不得调用 `mark_failed`。

- [ ] **Step 4: 增加互不阻断测试**

新增：

- `hash_failure_is_persisted_while_active_media_completes`：最终一行 `F`、一行 `C`。
- `worker_crash_is_persisted_while_hash_stream_continues`：Media 崩溃后下一个 Hash 仍单项查询并启动 Worker。

两项均断言 `summary.file_failures == 1`、查询观测值全部为 1。

- [ ] **Step 5: 增加真实 scheduler 上限测试**

在 `base_compute_pipeline.rs` 用 `ScheduledFileReader` 构造同盘 Hash/Media 竞争，断言 Hash active、Media active、每盘 active、全局 active 均不超过配置；并证明 Media permit 仅在 `BaseSourceReadComplete` 后释放。

- [ ] **Step 6: 删除生产不可达的整批屏障代码**

删除 transient 生产路径的全量 `outcomes Vec` 和整轮 drain；保留旧 `BaseComputeEngine`、分析或其他合法调用者所需的批量 API。用 `rg` 确认生产协调器不再调用旧完整 pass。

- [ ] **Step 7: 运行定向和 NodeEngine 全量回归**

Run: `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1`

Expected: 全部 PASS。

- [ ] **Step 8: 检查并提交**

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Commit: `git commit -m "fix: close streaming pipeline ownership gates"`

---

### Task 5: 全量验证、基准和长期架构文档

**Files:**
- Modify: `AGENTS.md`
- Create: `docs/verification/2026-08-30-streaming-hash-media-pipeline.md`

**Interfaces:**
- Consumes: Task 1-4 的最终实现和原始测试输出。
- Produces: 当前长期架构说明和可复核验证报告。

- [ ] **Step 1: 更新 AGENTS.md**

记录：Hash future 每完成一项立即单项查询 SQLite；可选远端也每次一个键；缺失项立即登记同身份 Media continuation；同一事件泵继续处理其他 Hash、Worker 和 ACK。明确扫描前路径缓存仍批量查询。

- [ ] **Step 2: 运行固定基准三轮**

Run: `cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked`

记录三轮 `elapsed_ms`、吞吐、`persisted_completed`、基准 EXE 路径和 SHA256。相对本分支已记录的 115.946 ms 中位数退化超过 15% 时停止并定位。

- [ ] **Step 3: 运行最终验证**

Run: `cargo test -p dedup-node-store --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --features test-hooks --test transient_task_files --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1`

Run: `cargo test -p dedup-node-engine --test scan_runtime_details --locked -- --test-threads=1`

Run: `cargo fmt --all -- --check`

Run: `git diff --check`

Expected: 全部 PASS。

- [ ] **Step 4: 生成中文验证报告**

报告必须列出旧实现 RED、每个 GREEN 命令、所有 ContentKey 查询调用大小为 1、第二个 Hash 查询时第一个 Media 仍 active、取消后零 owner、基准三轮，以及未执行真实媒体/打包/部署。

- [ ] **Step 5: 提交文档**

Stage: `git add AGENTS.md docs/verification/2026-08-30-streaming-hash-media-pipeline.md`

Commit: `git commit -m "docs: record streaming base pipeline verification"`

- [ ] **Step 6: 最终审查门禁**

使用 `gpt-5.6-sol`、`max` 只读审查最终提交范围，重点检查：是否仍有阶段 drain；本地/远端是否严格单键；dispatcher、Hash permit、Media permit、Worker slot、ACK 是否单一 owner；取消是否泄漏；路径缓存批量查询和物理盘权重是否误改。

发现 Important 及以上问题时按 RED→GREEN 修复并重新执行 Task 5 全部门禁；无问题后才报告完成。
