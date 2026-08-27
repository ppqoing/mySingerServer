# 三类计算任务与 Worker 文件会话实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 本项目按用户要求不使用子代理。

**Goal:** 将现有扫描与判重流水线重构为基础计算、重复文件清单生成、二次特征计算三类持久任务，由 Worker 使用可续算文件会话完成 MD5、缩略图和媒体特征计算，并支持 SQLite-only 单机及 Node 直连可选 PostgreSQL 的多机模式。

**Architecture:** Node 负责枚举、缓存判定、物理磁盘许可、SQLite 单写和 PostgreSQL 降级；Worker 负责所有文件内容读取与计算。基础计算通过 `BeginBaseCompute → BaseHashReady → ContinueBaseCompute → BaseComputeResult` 保持 Worker 槽位和文件句柄，Node 在 MD5 返回后只要求缺失计算。Desktop 只编排重复文件清单任务，多机数据保存在 PostgreSQL，单机分析数据保存在目标 Node SQLite。

**Tech Stack:** Rust 1.97.1、Tokio、Prost/Protobuf V4、rusqlite、tokio-postgres NoTls、Windows Overlapped I/O、FFmpeg 8.0.1 自定义 AVIO、Slint 1.17.1。

**Spec:** `docs/superpowers/specs/2026-08-22-three-task-compute-pipeline-design.md`

## Global Constraints

- 只修改 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup`，保留现有未提交改动；不得 reset、clean、checkout 覆盖或宽泛暂存。
- 不使用子代理；按本计划在当前会话内执行并在任务边界检查差异。
- 方法、类型、变量和公开接口使用中文注释说明职责、用法和实现逻辑；业务 crate 继续满足 `#![warn(missing_docs)]`。
- PostgreSQL 为 Node 可选能力；关闭时不得连接，异常时降级为 SQLite-only，不能让基础或二次计算任务失败。
- 缩略图固定为 `<cache>/contact-sheets/<md5 前两位>/<完整 md5>.jpg`，只在所属 Node 本地保存。
- 单块读取默认超时 3 秒、重试 2 次；任务进度每 2 秒合并发布，阶段终态立即发布。
- SQLite `PRAGMA user_version` 升级为 3；PostgreSQL 使用新的中心 schema 标识；旧库只拒绝，不迁移。
- Cargo 输出固定使用 `C:\tmp\rust-v2-visual-fidelity-target`。
- 只运行每个任务列出的相关测试；全部实现完成后才执行真实媒体半小时验收，不运行 workspace 全量测试。
- 每个提交步骤只列出精确文件和建议消息；实际执行期间只有用户明确授权提交时才运行 `git commit`。

---

### Task 1: 冻结 V4 协议与 Node PostgreSQL 配置

**Files:**
- Modify: `proto/node.proto`
- Modify: `crates/protocol/src/lib.rs`
- Modify: `crates/protocol/src/convert.rs`
- Modify: `crates/core/src/config.rs`
- Modify: `crates/core/tests/node_config.rs`
- Modify: `crates/protocol/tests/node_config_wire.rs`
- Create: `crates/protocol/tests/worker_base_compute_wire.rs`

**Interfaces:**
- Produces: `NodePostgresConfig`、`NodeConfig.postgres`、协议版本 `4`。
- Produces Worker messages: `BeginBaseCompute`、`BaseHashReady`、`ContinueBaseCompute`、`BaseComputeResult`。
- Produces bit values: `BASE_MISSING_PROBE = 1`、`BASE_MISSING_STAGE1 = 2`、`BASE_MISSING_CONTACT_SHEET = 4`。
- Extends `ComputeStage2`: `contact_sheet_path = 5`、`generate_contact_sheet_if_missing = 6`；图片保持空路径，视频明确携带本地目标 JPG。

- [ ] **Step 1: 写配置与协议 RED**

```rust
#[test]
fn node_postgres_defaults_disabled_and_roundtrips_password() {
    let mut config = NodeConfig::default();
    config.postgres.enabled = true;
    config.postgres.host = "192.168.1.8".into();
    config.postgres.database = "media".into();
    config.postgres.username = "dedup".into();
    config.postgres.password = "secret".into();
    assert_eq!(NodeConfig::from_toml(&config.to_toml().unwrap()).unwrap(), config);
}

#[test]
fn worker_base_compute_messages_preserve_read_limits_and_missing_mask() {
    let encoded = WorkerEnvelope { payload: Some(Payload::BeginBaseCompute(BeginBaseCompute {
        block_size_bytes: 4 * 1024 * 1024,
        block_timeout_ms: 3_000,
        block_retries: 2,
        ..fixture_begin()
    }))}.encode_to_vec();
    assert_eq!(WorkerEnvelope::decode(encoded.as_slice()).unwrap(), fixture_envelope());
}
```

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-core --test node_config node_postgres --locked -- --test-threads=1
cargo test -p dedup-protocol --test worker_base_compute_wire --locked -- --test-threads=1
```

Expected: FAIL；`NodePostgresConfig` 和四种 Worker 消息尚不存在。

- [ ] **Step 3: 实现最小 V4 契约**

```rust
/// Node 可选的中心 PostgreSQL 基础连接参数。
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(default)]
pub struct NodePostgresConfig {
    pub enabled: bool,
    pub host: String,
    pub port: u16,
    pub database: String,
    pub username: String,
    pub password: String,
    pub connect_timeout_seconds: u64,
}
```

`NodeConfigValue` 使用字段 `19` 保存 `NodePostgresConfigValue`。Worker oneof 使用请求字段 `13/14` 和响应字段 `25/26`；原字段号不复用。`BeginBaseCompute` 必须携带完整文件身份及读取块大小、超时毫秒、重试次数。`ContinueBaseCompute` 携带媒体类型提示和 `missing_parts`。`ComputeStage2` 在 V4 中一次增加本地联系表路径和缺失时生成开关，后续任务不得再次修改协议形状。

- [ ] **Step 4: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-core --test node_config node_postgres --locked -- --test-threads=1
cargo test -p dedup-protocol --test node_config_wire --locked -- --test-threads=1
cargo test -p dedup-protocol --test worker_base_compute_wire --locked -- --test-threads=1
```

Expected: PASS；用户名和密码经过 TOML、领域配置和 Protobuf 往返后保持不变。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- proto/node.proto crates/protocol/src/lib.rs crates/protocol/src/convert.rs crates/core/src/config.rs crates/core/tests/node_config.rs crates/protocol/tests/node_config_wire.rs crates/protocol/tests/worker_base_compute_wire.rs
git commit -m "feat: define v4 compute pipeline protocol"
```

### Task 2: 提取共享 PostgreSQL 存储边界

**Files:**
- Modify: `Cargo.toml`
- Create: `crates/central-store/Cargo.toml`
- Create: `crates/central-store/src/lib.rs`
- Move: `crates/desktop-core/src/central/*.rs` → `crates/central-store/src/`
- Modify: `crates/desktop-core/Cargo.toml`
- Modify: `crates/desktop-core/src/lib.rs`
- Modify: `crates/desktop-core/src/central/mod.rs`
- Modify: `crates/node-engine/Cargo.toml`
- Create: `crates/central-store/tests/public_contract.rs`

**Interfaces:**
- Produces crate: `dedup-central-store`。
- Preserves: `dedup_desktop_core::central::*` 通过 re-export 保持现有调用点可编译。
- Produces concrete owner: `CentralStore`，后续由 Desktop 和 Node 共同复用，不允许 Node 依赖 `dedup-desktop-core`。

- [ ] **Step 1: 写共享 crate RED**

```rust
use dedup_central_store::{CentralStore, CentralStoreError};

#[test]
fn central_store_is_available_without_desktop_core() {
    fn accepts(_: Option<CentralStore>, _: Option<CentralStoreError>) {}
    accepts(None, None);
}
```

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-central-store --test public_contract --locked -- --test-threads=1
```

Expected: FAIL；workspace 中尚无 `dedup-central-store`。

- [ ] **Step 3: 原样移动中心存储实现并建立 re-export**

```rust
// crates/desktop-core/src/central/mod.rs
pub use dedup_central_store::*;
```

只移动 PostgreSQL 连接、schema、内容、同步、分析和删除持久化；Desktop 的编排状态机仍留在 `dedup-desktop-core`。移动时保留现有公开名称和 SQL，不顺带修改行为。

- [ ] **Step 4: 运行 GREEN 与现有中心行为门禁**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-central-store --test public_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test central_schema --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test central_store --locked -- --test-threads=1
```

Expected: PASS；Desktop 公共导入路径不变，Node 可以直接依赖共享 PostgreSQL crate。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- Cargo.toml crates/central-store crates/desktop-core/Cargo.toml crates/desktop-core/src/lib.rs crates/desktop-core/src/central crates/node-engine/Cargo.toml
git commit -m "refactor: share central postgres store"
```

### Task 3: 持久化任务阶段与二次派发状态

**Files:**
- Modify: `crates/node-store/src/schema.sql`
- Modify: `crates/node-store/src/open.rs`
- Create: `crates/node-store/src/stages.rs`
- Modify: `crates/node-store/src/faults.rs`
- Modify: `crates/node-store/src/lib.rs`
- Create: `crates/node-store/tests/task_stages.rs`
- Modify: `crates/node-store/tests/file_faults.rs`
- Modify: `deploy/central-v2.sql`
- Modify: `crates/central-store/src/schema.rs`
- Create: `crates/central-store/src/stages.rs`
- Modify: `crates/central-store/src/lib.rs`
- Create: `crates/central-store/tests/task_stages.rs`

**Interfaces:**
- Produces: `PersistentStageState`、`TaskStageWrite`、`TaskStageSnapshot`。
- Produces Node APIs: `save_task_stage`、`task_stages`、`save_analysis_stage`、`analysis_stages`。
- Produces central APIs: `save_analysis_stage`、`upsert_stage2_dispatch`、`stage2_dispatches`。
- Extends `FileFaultRecord`: 读取块偏移/大小、Worker PID/退出码、首次/最近发生时间和重复次数；仍不保存任务 ID 或任务项 ID。

- [ ] **Step 1: 写 schema 与恢复 RED**

```rust
#[test]
fn task_stage_keeps_its_own_start_time_and_counts_after_reopen() {
    let task = store.create_task("base_compute", &items(), 1_000).unwrap();
    store.save_task_stage(task, running("enumerate_files", 1_100, 0, None)).unwrap();
    store.save_task_stage(task, completed("enumerate_files", 1_100, 1_300, 10, 10)).unwrap();
    drop(store);
    let stages = reopen().task_stages(task).unwrap();
    assert_eq!(stages[0].started_at_ms, Some(1_100));
    assert_eq!(stages[0].finished_at_ms, Some(1_300));
}
```

中心测试固定同一 `(analysis_run_id, machine_id, md5, file_size)` 重复派发只更新状态，不新增第二行。

故障测试连续写入同一 `(machine_id, normalized_path, fault_kind)`，断言首次时间保持、最近时间更新、重复次数递增，并分别保留读取块偏移/大小或 Worker PID/退出码。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-store --test task_stages --locked -- --test-threads=1
cargo test -p dedup-central-store --test task_stages --locked -- --test-threads=1
```

Expected: FAIL；阶段表和 API 不存在。

- [ ] **Step 3: 实现新建 schema**

```sql
CREATE TABLE task_stages (
    task_id TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
    stage_id TEXT NOT NULL,
    state TEXT NOT NULL,
    completed INTEGER NOT NULL DEFAULT 0,
    total INTEGER,
    failed INTEGER NOT NULL DEFAULT 0,
    skipped INTEGER NOT NULL DEFAULT 0,
    started_at_ms INTEGER,
    finished_at_ms INTEGER,
    warning_text TEXT,
    PRIMARY KEY(task_id, stage_id)
) STRICT;
```

SQLite 同时增加同字段的 `analysis_run_stages`，`PRAGMA user_version=3`。PostgreSQL 增加 `analysis_run_stages` 与 `analysis_stage2_dispatches`，`schema_metadata.schema_id` 改为 `mysingerserver-rust-v2-central-schema-3`。旧 SQLite/中心 schema 只返回不兼容错误。

`file_faults` 同时增加以下可空诊断列，数据库唯一键保持 `(machine_id, normalized_path, fault_kind)`：

```sql
read_offset INTEGER,
read_size INTEGER,
worker_pid INTEGER,
worker_exit_code INTEGER,
first_seen_at_ms INTEGER NOT NULL,
last_seen_at_ms INTEGER NOT NULL,
occurrence_count INTEGER NOT NULL DEFAULT 1
```

- [ ] **Step 4: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-store --test task_stages --locked -- --test-threads=1
cargo test -p dedup-node-store --test file_faults --locked -- --test-threads=1
cargo test -p dedup-node-store --test open --locked -- --test-threads=1
cargo test -p dedup-central-store --test task_stages --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test central_schema --locked -- --test-threads=1
```

Expected: PASS；空库初始化 schema 3，旧库拒绝且没有迁移 SQL。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/node-store/src/schema.sql crates/node-store/src/open.rs crates/node-store/src/stages.rs crates/node-store/src/faults.rs crates/node-store/src/lib.rs crates/node-store/tests/task_stages.rs crates/node-store/tests/file_faults.rs deploy/central-v2.sql crates/central-store/src/schema.rs crates/central-store/src/stages.rs crates/central-store/src/lib.rs crates/central-store/tests/task_stages.rs
git commit -m "feat: persist compute task stages"
```

### Task 4: 建立可复用 Worker 文件会话与 FFmpeg 自定义 AVIO

**Files:**
- Modify: `crates/windows/src/overlapped_reader.rs`
- Modify: `crates/windows/src/lib.rs`
- Create: `crates/windows/tests/reusable_file.rs`
- Modify: `crates/media-ffmpeg/src/ffi.rs`
- Modify: `crates/media-ffmpeg/src/loader.rs`
- Modify: `crates/media-ffmpeg/src/decode.rs`
- Modify: `crates/media-ffmpeg/src/lib.rs`
- Create: `crates/media-ffmpeg/tests/custom_io.rs`
- Create: `crates/node-engine/src/worker/file_session.rs`
- Modify: `crates/node-engine/src/worker/mod.rs`

**Interfaces:**
- Produces Windows type: `ReusableOverlappedFile::open`、`read_at`、`len`。
- Produces media trait: `SeekableMediaSource::read`、`seek`、`len`。
- Produces Worker type: `WorkerFileSession::open`、`compute_md5`、`media_source`。
- Produces FFmpeg APIs: `Ffmpeg::probe_source`、`Ffmpeg::decode_frame_from_source`。

- [ ] **Step 1: 写“只打开一次”与随机读取 RED**

```rust
#[test]
fn one_worker_file_session_reuses_one_open_for_md5_and_media_reads() {
    let opener = CountingFileOpener::new(fixture_bytes());
    let mut session = WorkerFileSession::open_with(opener.clone(), path(), limits()).unwrap();
    assert_eq!(session.compute_md5(&cancel()).unwrap(), expected_md5());
    session.media_source().seek(SeekFrom::Start(4)).unwrap();
    assert_eq!(session.media_source().read(&mut [0_u8; 8]).unwrap(), 8);
    assert_eq!(opener.open_count(), 1);
}
```

FFmpeg 测试从自定义内存 source 探测图片和 12 秒视频，禁止传入文件路径。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-windows --test reusable_file --locked -- --test-threads=1
cargo test -p dedup-media-ffmpeg --test custom_io --locked -- --test-threads=1
```

Expected: FAIL；复用句柄和自定义 AVIO API 不存在。

- [ ] **Step 3: 实现句柄与 AVIO 生命周期**

```rust
/// Worker 内一次打开、支持超时重试随机读取的文件会话。
pub struct WorkerFileSession {
    file: ReusableOverlappedFile,
    cursor: u64,
    limits: WorkerReadLimits,
}
```

`ReusableOverlappedFile` 持有一个 Windows `OwnedHandle` 并复用事件对象。FFmpeg 函数表增加 `avformat_alloc_context`、`avio_alloc_context`、`avio_context_free`、`av_malloc`、`av_free`；`DecoderSession` 的 Drop 顺序固定为解码资源、format、AVIO buffer、AVIO context、Rust source。AVIO read/seek callback 不得 panic 穿过 FFI。

- [ ] **Step 4: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-windows --test reusable_file --locked -- --test-threads=1
cargo test -p dedup-media-ffmpeg --test custom_io --locked -- --test-threads=1
cargo test -p dedup-media-ffmpeg --test decode_windows --locked -- --test-threads=1
```

Expected: PASS；路径 API 仍可用，但基础 Worker 流程使用自定义 source。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/windows/src/overlapped_reader.rs crates/windows/src/lib.rs crates/windows/tests/reusable_file.rs crates/media-ffmpeg/src/ffi.rs crates/media-ffmpeg/src/loader.rs crates/media-ffmpeg/src/decode.rs crates/media-ffmpeg/src/lib.rs crates/media-ffmpeg/tests/custom_io.rs crates/node-engine/src/worker/file_session.rs crates/node-engine/src/worker/mod.rs
git commit -m "feat: reuse worker file handles for ffmpeg"
```

### Task 5: 实现 Worker 两步基础计算状态机

**Files:**
- Modify: `crates/node-engine/src/worker/pipeline.rs`
- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/node-engine/src/worker/process.rs`
- Modify: `apps/worker/src/main.rs`
- Create: `crates/node-engine/tests/worker_base_session.rs`
- Modify: `apps/worker/tests/worker_pool.rs`

**Interfaces:**
- Produces: `BaseMissingParts`、`BaseHashOutput`、`BaseComputeOutput`。
- Produces: `WorkerRequestHandler::handle`，持有至多一个 `WorkerFileSession`。
- Produces pool API: `continue_base_compute(item_id, ContinueBaseCompute)`。
- Produces event: `WorkerEvent::BaseHashReady`；该事件不得把 Worker 槽位改为空闲。

- [ ] **Step 1: 写续算和槽位占用 RED**

```rust
#[tokio::test]
async fn hash_ready_keeps_slot_and_continue_finishes_the_same_session() {
    pool.dispatch_scan(begin("item-a"), cancel(), true, identity()).await.unwrap();
    let hash = expect_hash_ready(pool.next_event().await);
    assert_eq!(pool.available_slots(), 0);
    pool.continue_base_compute("item-a", continue_with(BASE_MISSING_STAGE1)).await.unwrap();
    let result = expect_base_result(pool.next_event().await);
    assert_eq!(result.md5, hash.md5);
    assert_eq!(pool.available_slots(), 1);
    assert_eq!(opener.open_count(), 1);
}
```

另测错误 item ID、重复 Continue、取消、Worker 崩溃都会关闭会话且只发一个终态。
Worker 崩溃事件必须返回 PID 和可选退出码；测试同时断言故障记录不含任务 ID、任务项 ID。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test worker_base_session --locked -- --test-threads=1
cargo test -p worker --test worker_pool base_session --locked -- --test-threads=1
```

Expected: FAIL；池把所有响应都视为终态，也没有续算命令。

- [ ] **Step 3: 实现最小状态机**

```rust
enum WorkerRequestState {
    Idle,
    AwaitingContinue { task_id: String, item_id: String, session: WorkerFileSession, md5: [u8; 16] },
}
```

`BaseHashReady` 只更新运行详情为“缓存判定”，不从 `active` 移除槽位。`ContinueBaseCompute` 只能定向发送给持有该 item 的 slot。掩码为零时返回只含 MD5 的 `BaseComputeResult`；非零时通过同一 session 完成所需探测、一筛和缩略图。

- [ ] **Step 4: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test worker_base_session --locked -- --test-threads=1
cargo test -p worker --test worker_pool base_session --locked -- --test-threads=1
cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1
```

Expected: PASS；同一 item 从 HashReady 到最终结果始终占用同一 PID/slot。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/node-engine/src/worker/pipeline.rs crates/node-engine/src/worker/pool.rs crates/node-engine/src/worker/process.rs apps/worker/src/main.rs crates/node-engine/tests/worker_base_session.rs apps/worker/tests/worker_pool.rs
git commit -m "feat: continue base computation in one worker session"
```

### Task 6: 实现 Node 基础计算任务、双层缓存与持续调度

**Files:**
- Create: `crates/node-engine/src/central_cache.rs`
- Create: `crates/node-engine/src/base_compute.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Modify: `crates/node-engine/src/scan/engine.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/io/scheduler.rs`
- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-store/src/features.rs`
- Modify: `crates/node-store/src/outbox.rs`
- Create: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/scan_cache.rs`
- Modify: `crates/node-engine/tests/scan_parallelism.rs`

**Interfaces:**
- Produces cache trait: `RemoteFeatureCache::{lookup_paths, lookup_contents, publish_outbox}`。
- Produces engine: `BaseComputeEngine::run_existing`。
- Produces decision: `BaseComputeDecision { media_kind, missing_parts, cached_content_id, contact_sheet }`。
- Replaces production use of `SystemMd5` and `PipelineFileReader` with Worker file sessions plus `DiskReadPermit`。

- [ ] **Step 1: 写缓存优先级与无波次屏障 RED**

```rust
#[tokio::test]
async fn local_then_postgres_then_worker_only_computes_missing_parts() {
    let result = run_fixture(local_partial(), postgres_stage1(), local_contact_sheet()).await;
    assert_eq!(result.worker_begins, 1);
    assert_eq!(result.continue_masks, vec![0]);
    assert_eq!(result.local_imports, 1);
}

#[tokio::test]
async fn completed_worker_is_replaced_without_waiting_for_the_whole_wave() {
    let control = ControlledBaseWorkers::new(4);
    start_base_task(control.clone()).await;
    control.finish("item-2").await;
    assert_eq!(control.next_started().await, "item-5");
}
```

另测 PostgreSQL 超时后发送本地缺失掩码，任务保持成功并记录 warning。
另测读取超时故障保存最后失败块的偏移和大小；Worker 崩溃故障保存机器 ID、路径、PID、退出码和时间计数，但不保存任务 ID/任务项 ID，也不启用熔断。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --test scan_parallelism continuous_refill --locked -- --test-threads=1
```

Expected: FAIL；Node 仍在本地计算 MD5，并通过 `process_batch().await` 等待整波 Worker。

- [ ] **Step 3: 实现三阶段基础计算和缓存导入**

```rust
pub struct BaseComputeDecision {
    pub media_kind: Option<MediaKind>,
    pub missing_parts: BaseMissingParts,
    pub cached_content_id: Option<ContentId>,
    pub contact_sheet: Option<ContactSheetCacheEntry>,
}
```

枚举完成后批量冻结总数；路径缓存按 SQLite→PostgreSQL 查询。未完整命中项取得物理磁盘许可后派发 `BeginBaseCompute`。收到 MD5 后按 SQLite→PostgreSQL 查询内容缓存、校验本地 JPG，并立即向同一 slot 发送缺失掩码。结果先写 SQLite，再写 outbox；PostgreSQL 导入和同步都不得直接修改任务项终态。

- [ ] **Step 4: 实现持续补位与 round-robin**

```rust
struct ActiveBaseItem {
    item_id: String,
    permit: DiskReadPermit,
    state: BaseItemState,
}
```

主循环同时 select Worker 事件、取消和可用磁盘项；单个终态返回后立即补充下一项。WorkerPool 空闲集合改为 `VecDeque<slot>` 循环归还，不使用 `BTreeSet::pop_first()`。磁盘许可持续到最终 `BaseComputeResult` 或失败终态。

- [ ] **Step 5: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --test scan_cache --locked -- --test-threads=1
cargo test -p dedup-node-engine --test scan_parallelism --locked -- --test-threads=1
```

Expected: PASS；Node 不打开文件计算 MD5，Worker 持续补位且不同物理盘并行。

- [ ] **Step 6: 准备精确提交**

```powershell
git add -- crates/node-engine/src/central_cache.rs crates/node-engine/src/base_compute.rs crates/node-engine/src/lib.rs crates/node-engine/src/scan/engine.rs crates/node-engine/src/scan/pipeline.rs crates/node-engine/src/scan/mod.rs crates/node-engine/src/io/scheduler.rs crates/node-engine/src/worker/pool.rs crates/node-store/src/content.rs crates/node-store/src/features.rs crates/node-store/src/outbox.rs crates/node-engine/tests/base_compute_pipeline.rs crates/node-engine/tests/scan_cache.rs crates/node-engine/tests/scan_parallelism.rs
git commit -m "feat: run cached base computation through workers"
```

### Task 7: 实现缩略图复用的二次特征任务

**Files:**
- Modify: `crates/media/src/contact_sheet.rs`
- Modify: `crates/media/src/lib.rs`
- Modify: `crates/media/tests/video_features.rs`
- Modify: `crates/node-engine/src/contact_sheet_cache.rs`
- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Modify: `crates/node-engine/src/worker/pipeline.rs`
- Create: `crates/node-engine/tests/stage2_thumbnail_cache.rs`
- Modify: `crates/desktop-core/tests/cross_phase2.rs`

**Interfaces:**
- Produces: `decode_contact_sheet(jpeg) -> [GrayImage; 6]`。
- Produces: `Stage2Source::{ImageFile, VideoContactSheet, VideoFallback}`。
- Produces: `Stage2CacheResolver`，查询 SQLite→PostgreSQL 并导入完整结果。

- [ ] **Step 1: 写缩略图一致性 RED**

```rust
#[test]
fn reused_and_regenerated_video_thumbnail_produce_the_same_stage2() {
    let first = compute_with_video_fallback(video_fixture());
    let second = compute_with_existing_thumbnail(first.jpeg.clone());
    assert_eq!(first.stage2, second.stage2);
    assert_eq!(second.video_open_count, 0);
}
```

另测损坏 JPG 触发原视频回退，原视频不可读只失败当前 item；图片始终读取原图片，不错误使用视频联系表。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test stage2_thumbnail_cache --locked -- --test-threads=1
cargo test -p dedup-media --test video_features contact_sheet_stage2 --locked -- --test-threads=1
```

Expected: FAIL；当前视频二筛仍重新打开原视频并抽帧。

- [ ] **Step 3: 实现固定 JPEG 语义**

```rust
pub enum Stage2Source {
    ImageFile(DisplayPath),
    VideoContactSheet(PathBuf),
    VideoFallback { video: DisplayPath, target: PathBuf },
}
```

视频二次特征始终从 JPEG 字节解码后的六格计算。回退时先生成固定 JPG、原子发布，再从该 JPG 字节计算，确保首次与复用结果一致。有效性检查包含可解码、固定画布尺寸和六格完整性。

- [ ] **Step 4: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test stage2_thumbnail_cache --locked -- --test-threads=1
cargo test -p dedup-media --test video_features --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test cross_phase2 --locked -- --test-threads=1
```

Expected: PASS；已有缩略图不打开原视频，回退生成结果与后续复用一致。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/media/src/contact_sheet.rs crates/media/src/lib.rs crates/media/tests/video_features.rs crates/node-engine/src/contact_sheet_cache.rs crates/node-engine/src/analysis/phase2.rs crates/node-engine/src/worker/pipeline.rs crates/node-engine/tests/stage2_thumbnail_cache.rs crates/desktop-core/tests/cross_phase2.rs
git commit -m "feat: compute video stage2 from cached thumbnails"
```

### Task 8: 持久化并恢复重复文件清单生成任务

**Files:**
- Create: `crates/desktop-core/src/analysis/task.rs`
- Modify: `crates/desktop-core/src/analysis/mod.rs`
- Modify: `crates/desktop-core/src/analysis/dispatch.rs`
- Modify: `crates/desktop-core/src/analysis/finalize.rs`
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/central-store/src/analysis.rs`
- Modify: `crates/central-store/src/cross_analysis.rs`
- Modify: `crates/node-engine/src/analysis/mod.rs`
- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Create: `crates/desktop-core/tests/duplicate_list_tasks.rs`
- Modify: `crates/desktop-core/tests/cross_analysis.rs`

**Interfaces:**
- Produces: `DuplicateListCoordinator::run`、`resume`。
- Produces stages: `build_candidates`、`dispatch_stage2`、`final_compare`。
- Multi-machine persistence: PostgreSQL `analysis_runs`、`analysis_run_stages`、`analysis_stage2_dispatches`。
- Single-machine persistence: selected Node SQLite `analysis_runs`、`analysis_run_stages`。

- [ ] **Step 1: 写三阶段和重连 RED**

```rust
#[tokio::test]
async fn resumed_duplicate_list_task_does_not_redispatch_completed_stage2_content() {
    let run = coordinator.start(inputs()).await.unwrap();
    node.complete_stage2(content_a()).await;
    drop(coordinator);
    let resumed = DuplicateListCoordinator::resume(store(), sessions()).await.unwrap();
    resumed.run(run).await.unwrap();
    assert_eq!(node.dispatch_count(content_a()), 1);
    assert_eq!(stages(run), ["build_candidates", "dispatch_stage2", "final_compare"]);
}
```

另测单机 SQLite-only 路径不访问 PostgreSQL，多机缺失二次结果保持 Incomplete 而非零分。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-desktop-core --test duplicate_list_tasks --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test cross_analysis --locked -- --test-threads=1
```

Expected: FAIL；当前中心分析没有独立持久阶段和幂等二次派发记录。

- [ ] **Step 3: 实现编排状态机**

```rust
pub enum DuplicateListStage {
    BuildCandidates,
    DispatchStage2,
    FinalCompare,
}

pub struct DuplicateListCoordinator<C, N> {
    central: Option<C>,
    nodes: N,
}
```

多机模式冻结 PostgreSQL 输入并按机器分组派发；单机模式调用目标 Node 本地分析接口。每次派发前读取 `analysis_stage2_dispatches`，只发送 `queued` 或明确失败后由用户重试的内容。最终分组在一个事务内替换。

- [ ] **Step 4: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-desktop-core --test duplicate_list_tasks --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test cross_analysis --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test cross_phase2 --locked -- --test-threads=1
```

Expected: PASS；断线恢复不重复计算已完成内容。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/desktop-core/src/analysis/task.rs crates/desktop-core/src/analysis/mod.rs crates/desktop-core/src/analysis/dispatch.rs crates/desktop-core/src/analysis/finalize.rs crates/desktop-core/src/app.rs crates/central-store/src/analysis.rs crates/central-store/src/cross_analysis.rs crates/node-engine/src/analysis/mod.rs crates/node-engine/src/analysis/phase2.rs crates/desktop-core/tests/duplicate_list_tasks.rs crates/desktop-core/tests/cross_analysis.rs
git commit -m "feat: persist duplicate list task stages"
```

### Task 9: 接入 Node 生命周期、PostgreSQL 降级和阶段进度

**Files:**
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-engine/src/config_repository.rs`
- Modify: `crates/node-engine/src/server.rs`
- Modify: `apps/node/src/lib.rs`
- Modify: `crates/desktop-core/src/runtime_tasks.rs`
- Modify: `crates/node-engine/tests/node_actor.rs`
- Modify: `crates/node-engine/tests/runtime_recovery.rs`
- Modify: `crates/node-engine/tests/scan_runtime_details.rs`
- Modify: `crates/desktop-core/tests/node_config_e2e.rs`
- Modify: `crates/desktop-core/tests/controller_runtime_tasks.rs`

**Interfaces:**
- Produces Node runtime kinds: `base_compute`、`stage2_compute`；produces Desktop runtime kind: `duplicate_list`。
- Produces stage IDs and independent timers defined by the spec。
- Produces `RuntimeProgressPublisher`，每 2 秒合并运行中更新，终态立即刷新。
- Produces optional `CentralCacheConnection` at Node startup/reconnect boundary。

- [ ] **Step 1: 写计时、2 秒节流和降级 RED**

```rust
#[tokio::test]
async fn each_stage_timer_starts_when_that_stage_really_starts() {
    let clock = FakeClock::new();
    let reporter = registry.begin(RuntimeTaskKind::BaseCompute, machine(), "基础计算").await;
    clock.advance_secs(10);
    reporter.start_stage_nowait(RuntimeStage::LookupBaseCache, Files).unwrap();
    clock.advance_secs(2);
    assert_eq!(details(&registry).stage("lookup_base_cache").elapsed_ms, 2_000);
}
```

另测 1.9 秒内 100 次进度变化只发布一次快照、2 秒 tick 后发布最新值、阶段完成立即发布；PostgreSQL 启动失败只增加 warning。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test scan_runtime_details stage_timer --locked -- --test-threads=1
cargo test -p dedup-node-engine --test runtime_recovery base_compute --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test node_config_e2e postgres --locked -- --test-threads=1
```

Expected: FAIL；任务种类、持久阶段恢复和 Node PostgreSQL 生命周期尚未接入。

- [ ] **Step 3: 实现运行时映射和恢复**

```rust
pub enum RuntimeTaskKind {
    Recovery,
    BaseCompute,
    LocalAnalysis,
    Stage2Compute,
    Delete,
}

pub enum DesktopRuntimeTaskKind {
    Node,
    DuplicateList,
    Sync,
    Delete,
}
```

Node 阶段固定为 `enumerate_files`、`lookup_base_cache`、`compute_base_features`、`lookup_stage2_cache`、`compute_stage2_features`。Desktop 清单阶段固定为 `build_candidates`、`dispatch_stage2`、`final_compare`。Node 启动时按 `postgres.enabled` 建立带超时连接；失败保存可观察 warning 并启动本地服务。持久 `task_stages` 用于重建阶段状态；速度和 ETA 从当前进程采样重新开始。枚举终态一次冻结总数，基础/二次计算以缓存命中数作为初始完成数。

- [ ] **Step 4: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test scan_runtime_details --locked -- --test-threads=1
cargo test -p dedup-node-engine --test runtime_recovery --locked -- --test-threads=1
cargo test -p dedup-node-engine --test node_actor --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test node_config_e2e --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test controller_runtime_tasks duplicate_list --locked -- --test-threads=1
```

Expected: PASS；阶段耗时互不继承，运行进度 2 秒合并，终态即时可见。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/node-engine/src/actor.rs crates/node-engine/src/runtime_tasks.rs crates/node-engine/src/config_repository.rs crates/node-engine/src/server.rs apps/node/src/lib.rs crates/desktop-core/src/runtime_tasks.rs crates/node-engine/tests/node_actor.rs crates/node-engine/tests/runtime_recovery.rs crates/node-engine/tests/scan_runtime_details.rs crates/desktop-core/tests/node_config_e2e.rs crates/desktop-core/tests/controller_runtime_tasks.rs
git commit -m "feat: expose persistent compute task progress"
```

### Task 10: 更新设置页与三类任务详情界面

**Files:**
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-core/src/view_state.rs`
- Modify: `crates/desktop-core/src/runtime_tasks.rs`
- Modify: `crates/desktop-ui/src/models.rs`
- Modify: `crates/desktop-ui/src/bindings.rs`
- Modify: `crates/desktop-ui/ui/pages/settings-workspace.slint`
- Modify: `crates/desktop-ui/ui/pages/task-center-page.slint`
- Modify: `crates/desktop-ui/ui/components/progress-bar.slint`
- Modify: `crates/desktop-ui/tests/bindings_contract.rs`
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`

**Interfaces:**
- Consumes: V4 `NodeConfigSnapshot`、三类 RuntimeTaskDetails。
- Produces settings fields: PostgreSQL enabled/host/port/database/username/password/connect-timeout。
- Produces task rows and stage details for `base_compute`、`duplicate_list`、`stage2_compute`。

- [ ] **Step 1: 写设置保留值和任务详情 RED**

```rust
#[test]
fn loaded_node_postgres_credentials_survive_view_refresh_and_save() {
    let app = configured_app("dedup", "secret");
    app.refresh_runtime_tasks();
    assert_eq!(app.node_postgres_username(), "dedup");
    assert_eq!(app.node_postgres_password(), "secret");
}
```

Offscreen 测试渲染三类任务各一条，断言阶段纵向排列、进度填充区域左边像素为主题色且右侧为空；不使用源文本 contains 断言。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-desktop-ui --test bindings_contract node_postgres --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout compute_task --locked -- --test-threads=1
```

Expected: FAIL；Node PostgreSQL 字段和三类任务展示尚不存在。

- [ ] **Step 3: 实现最小 UI 映射**

```rust
pub enum TaskKindView {
    BaseCompute,
    DuplicateList,
    Stage2Compute,
    Other,
}
```

输入控件直接绑定编辑缓冲区，运行时刷新不能覆盖正在编辑的用户名和密码。任务详情按后端阶段顺序显示，不把等待阶段标成运行中；Worker 行增加当前子步骤及缩略图复用/回退文案。

- [ ] **Step 4: 运行 GREEN**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout compute_task --locked -- --test-threads=1
```

Expected: PASS；所有进度条从左向右，凭据不会因刷新清空。

- [ ] **Step 5: 准备精确提交**

```powershell
git add -- crates/desktop-core/src/app.rs crates/desktop-core/src/view_state.rs crates/desktop-core/src/runtime_tasks.rs crates/desktop-ui/src/models.rs crates/desktop-ui/src/bindings.rs crates/desktop-ui/ui/pages/settings-workspace.slint crates/desktop-ui/ui/pages/task-center-page.slint crates/desktop-ui/ui/components/progress-bar.slint crates/desktop-ui/tests/bindings_contract.rs crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs
git commit -m "feat: show three compute task workflows"
```

### Task 11: 维护项目设计与部署文档

**Files:**
- Modify: `AGENTS.md`
- Modify: `docs/superpowers/specs/2026-08-22-three-task-compute-pipeline-design.md`
- Modify: `deploy/README-节点部署.md`
- Modify: `deploy/README-管理端部署.md`
- Create: `docs/verification/2026-08-22-three-task-compute-pipeline.md`

**Interfaces:**
- Documents: V4 进程职责、三类任务、SQLite/PostgreSQL 模式、Worker 文件会话、阶段进度、schema 3 和验收命令。
- Removes stale claims: Node 不访问 PostgreSQL、Node 使用 `SystemMd5`、视频二筛总是重读原视频、扫描任务只有旧五阶段。

- [ ] **Step 1: 对照实现维护主设计文档**

```text
AGENTS.md 必须同步更新：
1. 进程拓扑和 crate 责任表；
2. 数据所有权与 PostgreSQL 降级；
3. 三类任务和阶段 ID；
4. Worker 两步续算与同句柄 AVIO；
5. 缩略图复用和 schema 3 拒绝旧库；
6. 2 秒进度、失败跳过和恢复语义。
```

实现若与已批准规格出现技术性差异，先修正实现；只有得到用户确认后才能修改规格语义。规格文件只补充已落地的准确类型名、表名和验证命令。

- [ ] **Step 2: 更新部署说明**

```toml
[postgres]
enabled = false
host = "127.0.0.1"
port = 5432
database = "media_dedup"
username = "postgres"
password = ""
connect_timeout_seconds = 3
```

节点部署文档说明 SQLite-only 与多机两种模式、相对路径解析、旧数据库需手工重建。管理端文档说明远端机器无需 Desktop，以及多机清单任务需要中心 PostgreSQL。

- [ ] **Step 3: 创建验证记录模板并检查设计漂移**

```powershell
rg -n "Node.*不.*PostgreSQL|SystemMd5.*生产|视频二筛.*原视频|协议.*V3|user_version.*2" AGENTS.md deploy/README-节点部署.md deploy/README-管理端部署.md docs/superpowers/specs/2026-08-22-three-task-compute-pipeline-design.md
git diff --check -- AGENTS.md docs/superpowers/specs/2026-08-22-three-task-compute-pipeline-design.md deploy/README-节点部署.md deploy/README-管理端部署.md docs/verification/2026-08-22-three-task-compute-pipeline.md
```

Expected: `rg` 不返回陈旧设计断言；`git diff --check` 无格式错误。验证记录列出每个相关测试的命令、时间、结果和证据路径，不预填 PASS。

- [ ] **Step 4: 准备精确提交**

```powershell
git add -- AGENTS.md docs/superpowers/specs/2026-08-22-three-task-compute-pipeline-design.md deploy/README-节点部署.md deploy/README-管理端部署.md docs/verification/2026-08-22-three-task-compute-pipeline.md
git commit -m "docs: maintain compute pipeline design"
```

### Task 12: 执行相关集成门禁与真实媒体半小时验收

**Files:**
- Create: `crates/node-engine/tests/three_task_pipeline.rs`
- Modify: `docs/verification/2026-08-22-three-task-compute-pipeline.md`

**Interfaces:**
- Verifies: SQLite-only、PostgreSQL 模式、Worker 同句柄续算、连续调度、缩略图复用、二次派发恢复和三类任务进度。
- Real-media roots: local `I:\tmp`；remote `D:\tmp\-------2-4` 仅在最终半小时验收使用，原媒体只读。

- [ ] **Step 1: 写端到端相关行为 RED**

```rust
#[tokio::test]
async fn three_task_pipeline_reuses_base_and_stage2_results_across_restart() {
    let first = run_base_then_duplicate_list(fixture_cluster()).await;
    restart_nodes_and_desktop().await;
    let second = resume_same_inputs().await;
    assert_eq!(second.worker_md5_count, 0);
    assert_eq!(second.thumbnail_generation_count, 0);
    assert_eq!(second.stage2_dispatch_count, 0);
    assert_eq!(second.groups, first.groups);
}
```

- [ ] **Step 2: 运行 RED 后补齐集成边界**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-node-engine --test three_task_pipeline --locked -- --test-threads=1
```

Expected: 首次 FAIL 的原因必须是尚未接通的真实边界；只补齐该边界，不扩展功能。

- [ ] **Step 3: 运行全部相关自动化门禁**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-visual-fidelity-target'
cargo test -p dedup-protocol --test worker_base_compute_wire --locked -- --test-threads=1
cargo test -p dedup-node-store --test task_stages --locked -- --test-threads=1
cargo test -p dedup-central-store --test task_stages --locked -- --test-threads=1
cargo test -p dedup-media-ffmpeg --test custom_io --locked -- --test-threads=1
cargo test -p dedup-node-engine --test worker_base_session --locked -- --test-threads=1
cargo test -p dedup-node-engine --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --test stage2_thumbnail_cache --locked -- --test-threads=1
cargo test -p dedup-node-engine --test three_task_pipeline --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test duplicate_list_tasks --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout compute_task --locked -- --test-threads=1
```

Expected: 全部 PASS；不运行无关 workspace 测试。

- [ ] **Step 4: 执行只读真实媒体半小时验收**

```text
1. 本地以 I:\tmp 创建基础计算任务并持续运行至少 30 分钟。
2. 远端只在用户已部署验收包后，通过既有 SSH 连接对 D:\tmp\-------2-4 发起对应任务。
3. 观察 Worker 槽位、各物理盘吞吐、2 秒进度、阶段独立计时和失败记录。
4. 重复运行同一路径，确认 SQLite/PostgreSQL/缩略图/二次特征命中显著减少 Worker 计算。
5. 不删除、移动或改写任何真实媒体文件。
```

Expected: 任务持续推进，无阶段卡死、无限崩溃或进度倒退；证据写入验证文档。远端未部署或不可连接时明确记录未执行，不能写 PASS。

- [ ] **Step 5: 完成差异与文档一致性检查**

```powershell
git diff --check
git status --short
```

Expected: 无空白错误；状态中只包含用户原有改动及本计划明确修改的文件。除非用户随后要求，不自动打包、不上传、不扩大测试范围。

- [ ] **Step 6: 准备精确提交**

```powershell
git add -- crates/node-engine/tests/three_task_pipeline.rs docs/verification/2026-08-22-three-task-compute-pipeline.md
git commit -m "test: verify three task compute pipeline"
```
