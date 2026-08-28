# Rust V2 瞬态任务文件与最近分析结果 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 SQLite 只承担长期缓存、当前文件事实、故障和同步；计算任务由按物理盘拆分的瞬态 TSV 文件实际驱动，本地分析只保留最近一次成功 TSV，删除只保留当前进程内状态，并让本地结果界面改为滑动窗口动态加载。

**Architecture:** 枚举前先冻结扫描根到物理盘 lane 的映射；枚举后使用现有 1,000 项批量 SQLite `SELECT` 分类，完整缓存命中只进入本轮内存清单，真正缺少数据的项才追加到对应物理盘任务文件。调度器只从已封闭行边界内顺序取得 `P` 行，Worker 只计算缺失掩码，NodeStore 提交成功 ACK 后才原位改为 `C`。扫描成功后用一个 SQLite 收尾事务更新当前文件事实和 outbox，并把完成扫描快照保留在当前 Node 进程内。本地分析从这些快照冻结输入，在内存执行候选和分组，最终通过 Windows 原子替换发布 `latest-analysis.result.tsv`；Node 结果读取器只保存内存行偏移并按窗口返回数据。复核、删除计划和删除结果只在内存，删除成功仅更新 `files.active`、库 revision 和 file outbox。

**Tech Stack:** Rust 2024、Tokio actor、rusqlite/WAL、Protobuf V5、Slint、Windows `MoveFileExW`、UTF-8 无 BOM TSV、PowerShell 验收脚本、Cargo 集成测试。

**Spec:** [`docs/superpowers/specs/2026-08-28-transient-task-files-and-latest-analysis-design.md`](../specs/2026-08-28-transient-task-files-and-latest-analysis-design.md)

## Global Constraints

- 所有实现任务先调用 `superpowers:test-driven-development` 保存真实 RED，再写最小 GREEN；禁止使用 `read_source()`、`contains()` 或源码字符串匹配充当行为测试。
- 每个业务源文件保留中文 `//!` 职责说明；新增公开类型、方法和字段使用中文 `///`；方法、类型、关键状态变量添加简洁中文注释说明用途和所有权。
- 保留 SQLite schema 3 和现有 26 张物理表，不迁移、不删除表；产品路径不得再写任务、分析、复核、删除历史表。
- 任务文件和分析结果只使用 UTF-8 无 BOM TSV；禁止 JSON、JSONL、持久化 `.idx`、任务 SQLite 或临时分析 SQLite。
- 任务文件必须是实际调度来源，不得先在内存完成调度后再把 TSV 当日志补写。
- 完整缓存命中不生成任务行；部分命中只计算真实缺失字段；合法全零特征不得按默认占位符误判。
- Node 重启或计算引擎重启不恢复旧计算、未完成分析、复核或删除进度；旧任务/删除/未完成分析 ID 在新进程返回不存在。`latest-analysis.result.tsv` 头中的最近成功 analysis ID 例外，启动校验通过后继续允许只读查看。
- 本地结果 UI 不显示“上一页/下一页/加载更多”；中心 PostgreSQL 的既有游标只能作为 Desktop Core 内部填充窗口的实现细节。
- 真实媒体根 `H:\pik\00000000000` 与 `I:\tmp` 只读；任何测试、构建、打包均不得写入或删除其中内容，不触碰 `I:\Tool`。
- 所有重型 Cargo 命令前记录 C、D 盘可用空间；C 盘低于 10 GiB 时只盘点并清理本计划创建的 `C:\tmp\rust-v2-transient-task-target` 等精确可再生 target，保留日志和验收 evidence 后继续。
- 当前工作树已有其他修改。每次只暂存本任务明确列出的文件，禁止 `git add -A`、`git clean`、`git reset --hard` 或覆盖并行改动。
- 正式打包前必须完成一次真实媒体全量双物理盘测试；该测试以任务终态为完成条件，1,800 秒仅作为超时上限，不进行六轮 A/B 或 A-3 重复跑测。
- 最终独立审查使用 `gpt-5.6-sol`、`max`；只修复有行为证据的范围内问题，不做无边界扩展。

---

## 文件与接口映射

### 现有单写边界

- `crates/node-store/src/content.rs`：保留 `lookup_base_cache_by_paths`、`lookup_base_cache_by_keys` 和特征缓存读写。
- `crates/node-engine/src/scan/base_persistence.rs`：保留唯一 SQLite 写 actor，但删除任务表 claim、stage、complete、finalize 职责。
- `crates/node-engine/src/runtime_tasks.rs`：继续作为当前进程任务状态、阶段、Worker 和失败的唯一 UI 数据源。
- `crates/node-engine/src/io/scheduler.rs`：继续作为按盘许可、全局额度、配置权重和老化保护的唯一 actor；任务文件适配器只向它提交各 lane 队首，不创建第二套公平状态。

### 新增核心接口

```rust
/// 枚举前冻结的物理盘调度 lane。
pub struct PlannedScannedPath {
    pub scanned: ScannedPath,
    pub lane: TaskDiskLane,
}

/// 任务行统一缺失掩码：MD5、基础媒体字段和二筛槽位各占固定 bit。
pub struct TaskWorkMask(u64);

/// 一个任务文件行在当前运行中的稳定身份。
pub struct TaskFileIdentity {
    pub run_id: String,
    pub item_id: String,
    pub physical_disk_id: String,
    pub line_offset: u64,
}

/// 一个任务文件对应的物理盘身份、介质类型和配置容量。
pub struct TaskDiskLane {
    pub physical_disk_id: String,
    pub physical_disk_numbers: Vec<u32>,
    pub disk_kind: LocalDiskKind,
    pub configured_weight: usize,
}

/// 当前进程内一个已成功扫描任务的分析输入快照。
pub struct CompletedScanSnapshot {
    pub task_id: TaskId,
    pub inputs: Vec<ScanAnalysisInput>,
    pub outbox_high_seq: u64,
    pub library_revision: u64,
}

/// 完成扫描快照中供本地/中心分析冻结的一条成功位置。
pub struct ScanAnalysisInput {
    pub content: ContentKey,
    pub location: LocationKey,
    pub display_path: DisplayPath,
    pub media_kind: MediaKind,
}

/// 最近一次成功本地分析的文件元数据。
pub struct PublishedAnalysisResult {
    pub run_id: AnalysisRunId,
    pub library_revision: u64,
    pub group_count: u64,
    pub member_count: u64,
    pub path: PathBuf,
}
```

### 固定任务掩码

```rust
const TASK_NEEDS_MD5: u64 = 1 << 0;
const TASK_BASE_PROBE: u64 = 1 << 8;
const TASK_BASE_STAGE1: u64 = 1 << 9;
const TASK_BASE_CONTACT_SHEET: u64 = 1 << 10;
const TASK_IMAGE_STAGE2: u64 = 1 << 16;
const TASK_VIDEO_STAGE2_SLOT_0: u64 = 1 << 24; // slot 0..5 连续占位
```

路径缓存完全未命中时只写 `TASK_NEEDS_MD5`，`known_md5` 留空。Node 得到 MD5 后再批量查询内容缓存并在内存得到基础媒体缺失掩码；同一任务行仍保持 `P`，不在未知内容键时预先声称 Probe/Stage1/联系表缺失。

---

## Task 1：建立 SQLite 长期/瞬态边界与文件库 revision

**Files:**

- Create: `crates/node-store/src/maintenance.rs`
- Modify: `crates/node-store/src/lib.rs`
- Modify: `crates/node-store/src/open.rs`
- Modify: `crates/node-store/src/schema.sql`
- Create: `crates/node-store/tests/runtime_state_boundary.rs`

**Interfaces:**

```rust
impl NodeStore {
    /// 清空 schema 3 中遗留的运行态表，不触碰缓存、文件、故障和同步事实。
    pub fn clear_transient_runtime_state(&mut self) -> Result<(), StoreError>;

    /// 读取当前文件库版本；新库和既有 schema 3 库都从 0 开始。
    pub fn library_revision(&self) -> Result<u64, StoreError>;
}

pub(crate) fn bump_library_revision(
    transaction: &rusqlite::Transaction<'_>,
) -> Result<u64, StoreError>;
```

- [ ] **Step 1: 写启动清理 RED**

  在真实临时 SQLite 中先创建长期内容/文件/特征/outbox，再直接向旧任务、分析、复核和删除表放入合法关联行。重新打开 `NodeStore` 后调用清理接口，断言旧运行态全空，长期表行数和 outbox 序号完全不变。

  ```rust
  #[test]
  fn startup_cleanup_preserves_cache_and_clears_only_transient_tables() {
      let fixture = seeded_schema3_database();
      let before = fixture.long_lived_fingerprint();
      let mut store = fixture.reopen();
      store.clear_transient_runtime_state().unwrap();
      assert_eq!(fixture.transient_row_count(), 0);
      assert_eq!(fixture.long_lived_fingerprint(), before);
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test runtime_state_boundary startup_cleanup_preserves_cache_and_clears_only_transient_tables --locked -- --test-threads=1`

  Expected: FAIL，`clear_transient_runtime_state` 尚不存在。

- [ ] **Step 2: 写 revision RED**

  覆盖空库初值、既有 schema 3 缺少 key 时补 0、非法非十进制值拒绝三条行为。

  ```rust
  #[test]
  fn schema3_database_gets_a_strict_library_revision() {
      let store = open_existing_schema3_without_revision();
      assert_eq!(store.library_revision().unwrap(), 0);
      corrupt_revision_to("not-a-number");
      assert!(matches!(reopen(), Err(StoreError::IncompatibleSchema)));
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test runtime_state_boundary schema3_database_gets_a_strict_library_revision --locked -- --test-threads=1`

  Expected: FAIL，revision key 和读取接口尚不存在。

- [ ] **Step 3: 实现最小事务清理**

  子表到父表顺序固定为 `delete_items → delete_batches → deletion_tombstones → review_marks → group_members → duplicate_groups → candidate_pairs → analysis_run_inputs → analysis_run_stages → analysis_runs → task_stages → task_scan_roots → task_items → tasks`。全部 `DELETE` 放在一个事务中；不得使用动态表名、循环拼 SQL 或 `VACUUM`。

- [ ] **Step 4: 初始化并严格解析 revision**

  新库 schema 插入 `library_revision=0`；既有合法 schema 3 在 `initialize_or_validate` 中用 `INSERT ... ON CONFLICT DO NOTHING` 补 key，再按 `u64` 十进制严格解析。revision 修改只能通过同一 SQLite 事务内的 `bump_library_revision`。

- [ ] **Step 5: 跑 GREEN 与 crate 回归**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test runtime_state_boundary --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --locked -- --test-threads=1`

  Expected: 新测试全部 PASS；既有 schema/open/cache/outbox 测试无回归。

- [ ] **Step 6: 提交**

  ```powershell
  git add crates/node-store/src/maintenance.rs crates/node-store/src/lib.rs crates/node-store/src/open.rs crates/node-store/src/schema.sql crates/node-store/tests/runtime_state_boundary.rs
  git commit -m "feat: define transient sqlite runtime boundary"
  ```

---

## Task 2：集中定义缓存完整性和真实缺失掩码

**Files:**

- Create: `crates/node-store/src/cache_integrity.rs`
- Modify: `crates/node-store/src/lib.rs`
- Modify: `crates/node-store/src/rows.rs`
- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Modify: `crates/node-store/tests/content_cache.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/local_analysis.rs`

**Interfaces:**

```rust
/// SQLite 缓存字段通过结构校验后得到的缺失描述。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CacheCompleteness {
    pub base_missing_parts: u32,
    pub image_stage2_missing: bool,
    pub video_stage2_missing_slots: u8,
}

pub fn classify_cache_completeness(
    record: &BaseCacheRecord,
    contact_sheet_valid: bool,
) -> CacheCompleteness;
```

- [ ] **Step 1: 写字段完整性矩阵 RED**

  测试完整图片、合法全零图片特征、NULL/空字段、零尺寸、`base_complete=false`、完整视频六槽、缺槽、decoded 失败、损坏联系表、完整/部分二筛。

  ```rust
  #[test]
  fn valid_zero_features_are_hits_but_structural_gaps_are_missing() {
      let zero_image = valid_zero_image_cache();
      assert_eq!(classify_cache_completeness(&zero_image, true), CacheCompleteness::complete());
      let missing = image_cache_without_sobel();
      assert!(classify_cache_completeness(&missing, true).image_stage2_missing);
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test content_cache valid_zero_features_are_hits_but_structural_gaps_are_missing --locked -- --test-threads=1`

  Expected: FAIL，集中分类接口尚不存在。

- [ ] **Step 2: 固定“失败不是占位特征”RED**

  增加 Worker 失败后只出现 `file_faults`、对应特征字段仍缺失的行为测试；再发起新任务时分类仍返回缺失。

- [ ] **Step 3: 实现唯一分类器**

  复用当前固定 BLOB 长度、有限浮点、六槽和最少有效帧规则。不得用 `all(|value| value == 0)` 判断占位；`base_complete=false`、结构缺失和显式失败才算缺失。基础缺失继续映射协议既有 `BASE_MISSING_PROBE/STAGE1/CONTACT_SHEET`，二筛用图片布尔和视频六位 slot mask 表达。

- [ ] **Step 4: 删除重复判断**

  `BaseComputeDecision::for_cache` 与 `analysis::phase2` 改为消费 `CacheCompleteness`；保留薄转换，不再各自判断 NULL、槽位和尺寸。

- [ ] **Step 5: 跑 GREEN 与相关回归**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1`

  Expected: 完整缓存零计算；部分缓存只返回真实缺失位；合法全零仍命中。

- [ ] **Step 6: 提交**

  ```powershell
  git add crates/node-store/src/cache_integrity.rs crates/node-store/src/lib.rs crates/node-store/src/rows.rs crates/node-store/src/content.rs crates/node-engine/src/scan/base_compute.rs crates/node-engine/src/analysis/phase2.rs crates/node-store/tests/content_cache.rs crates/node-engine/tests/base_compute_pipeline.rs crates/node-engine/tests/local_analysis.rs
  git commit -m "fix: classify only structurally missing cache fields"
  ```

---

## Task 3：在枚举前冻结扫描根与物理盘 lane

**Files:**

- Modify: `crates/windows/src/storage_device.rs`
- Modify: `crates/windows/src/lib.rs`
- Modify: `crates/windows/tests/storage_device.rs`
- Create: `crates/node-engine/src/scan/root_plan.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/tests/scan_roots.rs`

**Interfaces:**

```rust
/// 一个扫描根在枚举前解析出的稳定存储位置。
pub struct ResolvedScanRootStorage {
    pub normalized_root: NormalizedPath,
    pub physical_disk_id: PhysicalDiskId,
    pub disk_kind: LocalDiskKind,
}

pub trait ScanRootStorageResolver: Send + Sync {
    fn resolve(&self, root: &Path) -> io::Result<ResolvedScanRootStorage>;
}

pub struct ScanDiskPlan;

impl ScanDiskPlan {
    pub fn build(
        roots: &[DisplayPath],
        read_config: &DiskReadConfig,
        resolver: &dyn ScanRootStorageResolver,
    ) -> Result<Self, ScanError>;
    pub fn assign(&self, scanned: ScannedPath) -> Result<PlannedScannedPath, ScanError>;
}
```

- [ ] **Step 1: 写枚举顺序 RED**

  受控 resolver 记录调用，受控 enumerator 在第一次 `enumerate` 时断言全部根已经解析；覆盖 H/I 两根、同盘两根、复合盘 ID、`D:\A` 与 `D:\AB` 组件边界。

  ```rust
  #[test]
  fn physical_storage_is_frozen_before_first_enumerator_call() {
      let trace = run_controlled_scan(&[r"H:\media", r"I:\tmp"]);
      assert_eq!(trace, ["resolve:H", "resolve:I", "enumerate"]);
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --features test-hooks --test scan_roots physical_storage_is_frozen_before_first_enumerator_call --locked -- --test-threads=1`

  Expected: FAIL，`ScanDiskPlan` 尚不存在，当前磁盘身份到首次读取时才解析。

- [ ] **Step 2: 实现 Windows 根存储解析**

  复用 `resolve_storage_location` 的卷 extent 和介质类型；把物理盘编号排序去重并冻结成 `PhysicalDisk7` 或 `PhysicalDisk5+12`。根解析失败返回稳定 `SCAN_ROOT_STORAGE_RESOLVE_FAILED`，不得静默归入空 lane。

- [ ] **Step 3: 实现最长组件根匹配**

  `ScanDiskPlan::assign` 使用 `NormalizedPath::is_within`，多个根命中时选择组件最深者；把 `TaskDiskLane { physical_disk_id, disk_kind }` 附在 `PlannedScannedPath`。枚举器仍只负责列文件，不自行访问磁盘配置。

- [ ] **Step 4: 消除读取期二次磁盘身份事实**

  `ScheduledFileReader` 获取许可时直接消费冻结 lane；系统 resolver 只在根计划建立时调用。保留测试注入 resolver，不保留 `take_physical_disk_id` 的可变路径缓存。

- [ ] **Step 5: 跑 GREEN**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-windows --test storage_device --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --features test-hooks --test scan_roots --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1`

  Expected: 枚举前解析顺序、组件边界、复合盘和既有许可测试全部 PASS。

- [ ] **Step 6: 提交**

  ```powershell
  git add crates/windows/src/storage_device.rs crates/windows/src/lib.rs crates/windows/tests/storage_device.rs crates/node-engine/src/scan/root_plan.rs crates/node-engine/src/scan/mod.rs crates/node-engine/src/actor.rs crates/node-engine/src/scan/pipeline.rs crates/node-engine/tests/scan_roots.rs
  git commit -m "feat: freeze scan disk lanes before enumeration"
  ```

---

## Task 4：实现按物理盘拆分的瞬态任务文件

**Files:**

- Create: `crates/node-engine/src/task_files.rs`
- Create: `crates/node-engine/src/task_dispatch.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Create: `crates/node-engine/tests/transient_task_files.rs`
- Create: `crates/node-engine/tests/task_dispatch.rs`

**Interfaces:**

```rust
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskLineStatus { Pending, Completed, Failed }

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskWorkKind { Base, ImageStage2, VideoStage2 }

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct TaskWorkMask(u64);

impl TaskWorkMask {
    pub const fn needs_md5(self) -> bool;
    pub const fn base_missing_parts(self) -> u32;
    pub const fn image_stage2_missing(self) -> bool;
    pub const fn video_stage2_slots(self) -> u8;
}

pub struct TaskFileRecord {
    pub item_id: String,
    pub work_kind: TaskWorkKind,
    pub scanned: ScannedPath,
    pub known_md5: Option<[u8; 16]>,
    pub missing: TaskWorkMask,
}

pub struct TransientTaskFileSet;

impl TransientTaskFileSet {
    pub fn create(runtime_root: &Path, run_id: &str) -> io::Result<Self>;
    pub fn append_batch(&mut self, lane: &TaskDiskLane, rows: &[TaskFileRecord]) -> io::Result<Vec<TaskFileIdentity>>;
    pub fn seal(&mut self) -> io::Result<()>;
    pub fn peek_lane(&mut self, lane: &TaskDiskLane) -> io::Result<Option<&TaskFileRecord>>;
    pub fn take_lane(&mut self, lane: &TaskDiskLane) -> io::Result<Option<(TaskFileIdentity, TaskFileRecord)>>;
    pub fn mark_completed(&mut self, identity: &TaskFileIdentity) -> io::Result<()>;
    pub fn mark_failed(&mut self, identity: &TaskFileIdentity) -> io::Result<()>;
    pub fn all_terminal(&self) -> bool;
}

pub trait TaskLanePermitProvider: Clone + Send + Sync + 'static {
    type Permit: Send + 'static;
    fn acquire(
        &self,
        lane: TaskDiskLane,
        class: DiskReadClass,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<Self::Permit, ReadFailure>> + Send>>;
}

pub struct TaskFileDispatcher<P: TaskLanePermitProvider>;

impl<P: TaskLanePermitProvider> TaskFileDispatcher<P> {
    pub async fn next(
        &mut self,
        cancellation: ReadCancellationToken,
    ) -> Result<Option<DispatchedTask<P::Permit>>, ReadFailure>;
}
```

- [ ] **Step 1: 写固定字节格式 RED**

  固定行格式为 `状态、item_id、work_kind、normalized_path、display_path、file_size、known_md5、missing_mask` 八列，使用 tab 分隔和 LF 结尾。覆盖 UTF-8 无 BOM、空 MD5、64 位小写十六进制掩码、路径 tab/newline 拒绝、非法状态和重复 UUID v7 item ID 拒绝；缓存命中项不分配 item ID。

  ```rust
  #[test]
  fn task_rows_are_fixed_tsv_without_json_or_bom() {
      let bytes = write_one_task_row(base_record_needing_md5());
      assert_eq!(bytes[0], b'P');
      assert!(!bytes.starts_with(&[0xEF, 0xBB, 0xBF]));
      assert_eq!(bytes.iter().filter(|byte| **byte == 0x09).count(), 7);
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test transient_task_files task_rows_are_fixed_tsv_without_json_or_bom --locked -- --test-threads=1`

  Expected: FAIL，模块和类型尚不存在。

- [ ] **Step 2: 写真实来源和状态时序 RED**

  两个 lane 各追加任务，seal 后通过 dispatcher 读取；SQLite ACK 模拟失败时首字节保持 `P`，ACK 成功后仅该行首字节变 `C`，文件失败变 `F`，其余字节 SHA 保持不变。

- [ ] **Step 3: 实现固定格式和单所有者**

  每个 lane 文件名只由已验证 `PhysicalDisk...` 和 `hdd|ssd|unknown` 构造。`TransientTaskFileSet` 独占追加句柄、读取游标、已发布长度、行偏移和状态；使用 `BufWriter` 批量追加，flush 后才扩大可读边界。禁止其他组件直接持有文件句柄。

- [ ] **Step 4: 实现原位状态更新**

  Windows 使用 `std::os::windows::fs::FileExt::seek_write` 在 `line_offset` 写一个 ASCII 字节。写前读取并核对整行 item ID、run ID 和 lane 身份；只允许 `P→C` 或 `P→F`，重复同终态幂等，其他转换报错。

- [ ] **Step 5: 实现有限预读和唯一 scheduler 适配**

  每 lane 只预读 `max(2, lane_capacity * 2)` 行；`TaskFileDispatcher` 对每个非空 lane 最多保留一个队首 permit future，并全部交给现有 `DiskReadScheduler`。哪个 future 先获得 permit 就从对应 TSV `take_lane`，把 permit 连同记录交给 Hash/Media；不得在读取器内再次 acquire，也不得先把全部 TSV 反序列化进 `Vec`。配置权重、全局不足时轮转和老化继续由唯一 scheduler 决定。

- [ ] **Step 6: 跑 GREEN**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1`

  Expected: 格式、双盘、状态 ACK、损坏拒绝、有限预读、每 lane 单队首以及 5:1/1:1 配置权重测试全部 PASS；不存在双重 permit。

- [ ] **Step 7: 提交**

  ```powershell
  git add crates/node-engine/src/task_files.rs crates/node-engine/src/task_dispatch.rs crates/node-engine/src/lib.rs crates/node-engine/tests/transient_task_files.rs crates/node-engine/tests/task_dispatch.rs
  git commit -m "feat: add per-disk transient task files"
  ```

---

## Task 5：用一次事务收尾当前扫描清单

**Files:**

- Create: `crates/node-store/src/inventory.rs`
- Modify: `crates/node-store/src/lib.rs`
- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-store/src/outbox.rs`
- Create: `crates/node-store/tests/inventory_finalize.rs`

**Interfaces:**

```rust
pub struct ResolvedScanFile {
    pub scanned: ScannedPath,
    pub content: ContentKey,
}

pub struct ScanFinalizeInput {
    pub roots: Vec<NormalizedPath>,
    pub seen_paths: Vec<NormalizedPath>,
    pub resolved_files: Vec<ResolvedScanFile>,
}

pub struct ScanFinalizeResult {
    pub outbox_high_seq: u64,
    pub library_revision: u64,
}

impl NodeStore {
    pub fn finalize_scan_manifest(
        &mut self,
        input: &ScanFinalizeInput,
        now_ms: i64,
    ) -> Result<ScanFinalizeResult, StoreError>;
}
```

- [ ] **Step 1: 写收尾原子性 RED**

  覆盖缓存命中、计算成功、读取失败但已见、`D:\A`/`D:\AB`、取消、事务注入失败。失败或取消不得失活旧位置，不得推进 revision/outbox。

  ```rust
  #[test]
  fn seen_but_failed_path_is_not_falsely_deactivated() {
      let mut store = seeded_active_file(r"D:\A\broken.mp4");
      let input = finalize_input_seen_without_resolved(r"D:\A\broken.mp4");
      store.finalize_scan_manifest(&input, 10).unwrap();
      assert!(store.active_file(&location(r"D:\A\broken.mp4")).unwrap().is_some());
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test inventory_finalize seen_but_failed_path_is_not_falsely_deactivated --locked -- --test-threads=1`

  Expected: FAIL，新接口尚不存在。

- [ ] **Step 2: 实现临时清单表和单事务**

  在连接级 `TEMP` 表写入本轮 `seen_paths` 与 `resolved_files`，每批最多 1,000 行并复用 prepared statement；正式表更新、路径组件失活、file outbox、高水位和 revision 放在同一事务。成功收尾 revision 恰好推进一次。

- [ ] **Step 3: 冻结失败语义**

  `seen_paths` 包含所有枚举行，因此单文件 `F` 不误失活旧位置；`resolved_files` 只包含完整缓存命中和成功 SQLite ACK。枚举失败、任务文件失败、取消和任务级错误不调用此接口。

- [ ] **Step 4: 跑 GREEN 与 outbox 回归**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test inventory_finalize --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test outbox --locked -- --test-threads=1`

  Expected: 组件边界、原子性、高水位和 revision 全部 PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add crates/node-store/src/inventory.rs crates/node-store/src/lib.rs crates/node-store/src/content.rs crates/node-store/src/outbox.rs crates/node-store/tests/inventory_finalize.rs
  git commit -m "feat: finalize scans from transient manifests"
  ```

---

## Task 6：把基础计算改为任务文件真实调度源

**Files:**

- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/scan/engine.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`
- Modify: `crates/node-engine/tests/scan_cache.rs`

**Interfaces:**

```rust
pub struct ScanRunResult {
    pub summary: ScanSummary,
    pub completed: CompletedScanSnapshot,
}

pub struct ScanRunInput {
    pub task_id: TaskId,
    pub options: ScanOptions,
    pub rows: Vec<PlannedScannedPath>,
    pub runtime_root: PathBuf,
    pub contact_sheet_root: PathBuf,
    pub read_config: DiskReadConfig,
    pub cancellation: ReadCancellationToken,
    pub now_ms: i64,
}

pub async fn run_existing<R, F>(
    store: &mut NodeStore,
    worker_pool: &mut WorkerPool,
    remote: R,
    remote_available: bool,
    input: ScanRunInput,
    reader: F,
    limits: PipelineLimits,
    reporter: &RuntimeTaskReporter,
    artifact_registry: &Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: &DiskFullCleaner,
) -> Result<ScanRunResult, ScanError>;
```

- [ ] **Step 1: 写查询前零任务写入 RED**

  用真实 SQLite trace 跑 1,000 项路径缓存批次，断言第一批查询前 `tasks/task_items/task_stages` 的 INSERT/UPDATE 为 0；完整命中时整个运行目录没有 `.tasks.tsv`。

  ```rust
  #[tokio::test]
  async fn full_path_cache_hits_never_create_task_rows_or_task_files() {
      let result = run_1_000_cached_paths().await.unwrap();
      assert_eq!(result.task_sql_writes, 0);
      assert!(result.task_files.is_empty());
      assert_eq!(result.summary.cache_hits, 1_000);
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline full_path_cache_hits_never_create_task_rows_or_task_files --locked -- --test-threads=1`

  Expected: FAIL，当前 `reserve_rows` 在查询前逐项写 SQLite。

- [ ] **Step 2: 写三类任务行 RED**

  固定输入包含：完整命中、已知 MD5 但缺联系表、路径未命中。断言只生成后两行；部分命中含已知 MD5 和联系表 bit，路径未命中 MD5 为空且只有 `TASK_NEEDS_MD5`。

- [ ] **Step 3: 写 ACK 和崩溃隔离 RED**

  受控 persist actor 在第一项提交前暂停：任务行仍为 `P`；提交 ACK 后为 `C`。Worker 崩溃只把对应行改 `F`，另一物理盘继续得到 Worker 并最终 `C`。

- [ ] **Step 4: 删除持久任务 claim 路径**

  删除 `reserve_rows`、`reserve_scan_path`、`queue_scan_item_for_read`、`claim_next_item`、`complete_item_guarded` 和 `finalize_scan_task_from_items` 在基础计算中的调用。`BaseStoreActor` 只接收缓存查询、内容/位置/特征合并、故障和最终 manifest 收尾消息。

- [ ] **Step 5: 接入任务文件调度**

  每个缓存批次分类后立即按 lane `append_batch`；缓存命中直接加入 `seen_paths/resolved_files` 和 runtime completed。Hash/Media readiness 来自 `TaskFileDispatcher::next` 返回的任务行和已取得 permit，不再来自 `remaining_rows` 或 SQLite queued 项，也不在 `ScheduledFileReader` 内二次 acquire。

- [ ] **Step 6: 保持两步 Worker 会话**

  `TASK_NEEDS_MD5` 行先进入 `WorkerFileSession`/Hash，MD5 返回后批量内容查询并在内存形成 `BaseComputeDecision`。内容完整命中时只提交位置关系，ACK 后标 `C`；仍缺字段时同一会话继续 Worker，且只下发 `decision.missing_parts()`。

- [ ] **Step 7: 收尾并返回当前进程快照**

  生产者完成后 seal；所有行 `C/F`、persist ACK 排空后调用 `finalize_scan_manifest`。把成功 resolved 输入、outbox 高水位和 revision 放入 `CompletedScanSnapshot` 返回。任何任务级错误不收尾。

- [ ] **Step 8: 跑 GREEN 和流水线回归**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test scan_cache --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test base_compute_utilization --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --lib --locked -- --test-threads=1`

  Expected: 完整命中零任务文件；双盘继续并行；joint Hash/Media、公平配额、credit/permit 守恒测试全部 PASS。

- [ ] **Step 9: 提交**

  ```powershell
  git add crates/node-engine/src/scan/base_compute.rs crates/node-engine/src/scan/base_persistence.rs crates/node-engine/src/scan/pipeline.rs crates/node-engine/src/scan/engine.rs crates/node-engine/src/scan/mod.rs crates/node-engine/tests/base_compute_pipeline.rs crates/node-engine/tests/base_compute_utilization.rs crates/node-engine/tests/scan_cache.rs
  git commit -m "refactor: drive base compute from task files"
  ```

---

## Task 7：让二筛任务复用同一瞬态任务文件边界

**Files:**

- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Modify: `crates/node-engine/src/analysis/mod.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/task_files.rs`
- Modify: `crates/node-engine/tests/local_analysis.rs`
- Modify: `crates/node-engine/tests/three_task_pipeline.rs`
- Modify: `crates/node-engine/tests/stage2_thumbnail_cache.rs`

**Interfaces:**

```rust
pub struct Stage2TaskPlan {
    pub task_id: TaskId,
    pub run_root: PathBuf,
    pub items: Vec<PlannedStage2Item>,
}

pub async fn dispatch_missing_from_task_files<P: Stage2Processor>(
    store: &mut NodeStore,
    plan: Stage2TaskPlan,
    task_files: &mut TransientTaskFileSet,
    processor: &mut P,
    reporter: &RuntimeTaskReporter,
) -> Result<MissingDispatchReport, AnalysisBlocked>;
```

- [ ] **Step 1: 写二筛缓存/任务文件 RED**

  同时放入完整图片二筛、缺 Sobel 图片、完整视频、只缺槽 2/5 视频。断言完整项不生成行；图片行只含 `TASK_IMAGE_STAGE2`；视频行只含 slot 2/5 bits。

- [ ] **Step 2: 写外部 DispatchStage2 RED**

  通过真实 Node actor 发送重复内容和跨两个物理盘的 `DispatchStage2`，断言去重后每盘一个任务文件，完整缓存项零 Worker，缺失项由文件顺序派发。

- [ ] **Step 3: 移除持久 stage2 task**

  `analysis::phase2` 和 actor 不再调用 `create_task/append_task_item/claim_next_item/complete_item`。本地分析和中心重新发布共用 `Stage2TaskPlan`；建立 plan 时先对去重后的来源位置解析并冻结 `TaskDiskLane`，再做二筛缓存查询和任务文件追加。缓存命中只发布必要 outbox，不写任务行。

- [ ] **Step 4: 接入 ACK 状态**

  Worker 结果通过同一 NodeStore 单写者合并，成功 ACK 后标 `C`；单文件错误标 `F` 并让分析候选保持 `Incomplete`。不存在失败占位特征。

- [ ] **Step 5: 跑 GREEN**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test three_task_pipeline --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test stage2_thumbnail_cache --locked -- --test-threads=1`

  Expected: 二筛只计算缺失字段，缓存命中零任务行，F 项不阻塞其他 lane。

- [ ] **Step 6: 提交**

  ```powershell
  git add crates/node-engine/src/analysis/phase2.rs crates/node-engine/src/analysis/mod.rs crates/node-engine/src/actor.rs crates/node-engine/src/task_files.rs crates/node-engine/tests/local_analysis.rs crates/node-engine/tests/three_task_pipeline.rs crates/node-engine/tests/stage2_thumbnail_cache.rs
  git commit -m "refactor: route stage2 through transient task files"
  ```

---

## Task 8：建立当前进程扫描目录并移除任务恢复

**Files:**

- Create: `crates/node-engine/src/scan/session_catalog.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-engine/tests/runtime_recovery.rs`
- Modify: `crates/node-engine/tests/runtime_tasks.rs`
- Modify: `crates/node-engine/tests/node_actor.rs`
- Modify: `crates/desktop-core/tests/local_node_e2e.rs`

**Interfaces:**

```rust
#[derive(Default)]
pub struct ScanSessionCatalog {
    completed: BTreeMap<TaskId, CompletedScanSnapshot>,
}

impl ScanSessionCatalog {
    pub fn insert(&mut self, snapshot: CompletedScanSnapshot);
    pub fn get(&self, task_id: TaskId) -> Option<&CompletedScanSnapshot>;
    pub fn analysis_inputs(&self, task_ids: &[TaskId]) -> Result<Vec<ScanAnalysisInput>, ScanCatalogError>;
}

enum BackgroundOutcome {
    Scan(CompletedScanSnapshot),
    LocalAnalysis(PublishedAnalysisResult),
    Stage2,
    Failed,
}
```

- [ ] **Step 1: 写重启清空 RED**

  完成一个真实扫描后 `ListTasks/QueryTask/PrepareAnalysisInput` 可见；关闭并用同一 SQLite 重启 Node 后三者都返回空/NotFound，但缓存再次查询仍命中。

  ```rust
  #[tokio::test]
  async fn restart_forgets_task_ids_but_keeps_cache_results() {
      let old_id = complete_scan_and_restart_node().await;
      assert_task_not_found(old_id).await;
      assert_eq!(run_same_fixture_again().await.cache_hits, FIXTURE_FILES);
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test runtime_recovery restart_forgets_task_ids_but_keeps_cache_results --locked -- --test-threads=1`

  Expected: FAIL，当前启动会从 SQLite 恢复任务。

- [ ] **Step 2: 让后台结果回到 actor**

  `BackgroundFinished` 携带 `BackgroundOutcome`；只有成功扫描才插入 `ScanSessionCatalog`。catalog 保存完成扫描的成功输入和 highwater，不复制 Worker/阶段动态状态。

- [ ] **Step 3: 切换任务协议数据源**

  `ListTasks`、`QueryTask`、`PrepareAnalysisInput(scan_task_ids)` 和中心分析输入全部从 `ScanSessionCatalog + RuntimeTaskRegistry` 读取。当前活动任务取消仍通过 cancellation/WorkerPool；不读取旧 SQLite task 表。

- [ ] **Step 4: 切换启动和计算引擎重启**

  `NodeRuntime::start` 精确清空 `data/node/runtime` 和 `.partial.tsv`，再调用 `clear_transient_runtime_state`。删除 `recover_for_actor`、`recover_active_computation_tasks` 和 `requeue_planned_items` 的产品调用。计算引擎重启先取消当前任务并等待 Worker 收束，再创建新 Pool；不恢复旧行。

- [ ] **Step 5: 跑 GREEN**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test runtime_recovery --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test runtime_tasks --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test node_actor --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-core --test local_node_e2e --locked -- --test-threads=1`

  Expected: 当前进程任务可用，重启后清空，缓存不丢失，任务中心仍由 RuntimeTaskRegistry 单写。

- [ ] **Step 6: 提交**

  ```powershell
  git add crates/node-engine/src/scan/session_catalog.rs crates/node-engine/src/scan/mod.rs crates/node-engine/src/actor.rs crates/node-engine/src/runtime_tasks.rs crates/node-engine/tests/runtime_recovery.rs crates/node-engine/tests/runtime_tasks.rs crates/node-engine/tests/node_actor.rs crates/desktop-core/tests/local_node_e2e.rs
  git commit -m "refactor: keep completed scans in process memory"
  ```

---

## Task 9：实现最近分析 TSV 的安全发布

**Files:**

- Create: `crates/windows/src/atomic_file.rs`
- Modify: `crates/windows/src/lib.rs`
- Create: `crates/windows/tests/atomic_file.rs`
- Create: `crates/node-engine/src/analysis/result_file.rs`
- Modify: `crates/node-engine/src/analysis/mod.rs`
- Create: `crates/node-engine/tests/analysis_result_file.rs`

**Interfaces:**

```rust
pub fn atomic_replace_file(source: &Path, destination: &Path) -> io::Result<()>;

pub struct AnalysisResultWriter;

impl AnalysisResultWriter {
    pub fn begin(results_root: &Path, header: &AnalysisResultHeader) -> Result<Self, AnalysisResultError>;
    pub fn write_member(&mut self, row: &AnalysisResultRow) -> Result<(), AnalysisResultError>;
    pub fn publish(self) -> Result<PublishedAnalysisResult, AnalysisResultError>;
    pub fn discard(self) -> Result<(), AnalysisResultError>;
}
```

- [ ] **Step 1: 写 Windows 原子替换 RED**

  旧 destination 内容存在时替换成功；source 不存在、destination 被锁和替换失败时旧 destination 字节不变。实现测试只使用临时目录。

- [ ] **Step 2: 写 TSV 格式 RED**

  固定记录：`H` 头行、每成员一条 `M`、`F` 尾行。`H` 依次保存 format_version、analysis_id、library_revision、analysis_mode、created_at_ms 和九个独立阈值；`M` 依次保存 group_kind、group_id、representative 标记、代表 ContentKey、LocationKey、display_path、成员 ContentKey、stage1、pHash 通过块数和 stage2；`F` 保存成员行数和此前全部字节 SHA-256。禁止 TOML/JSON 嵌套。

  ```rust
  #[test]
  fn published_result_has_header_member_rows_and_verified_footer() {
      let published = publish_fixture_result();
      let parsed = verify_result_file(&published.path).unwrap();
      assert_eq!(parsed.member_count, 3);
      assert_eq!(parsed.sha256, recompute_before_footer(&published.path));
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test analysis_result_file published_result_has_header_member_rows_and_verified_footer --locked -- --test-threads=1`

  Expected: FAIL，writer 尚不存在。

- [ ] **Step 3: 实现临时文件生命周期**

  开始新分析时创建/截断 `latest-analysis.partial.tsv`；写入期间旧 `latest-analysis.result.tsv` 保持可读。flush、`sync_all`、关闭句柄后调用 `atomic_replace_file(partial, result)`；替换成功后才返回 `PublishedAnalysisResult`。取消/失败删除 partial，不修改旧 result。

- [ ] **Step 4: 实现固定编码**

  MD5 小写十六进制，布尔为 `0/1`，缺失可选分数为空列，浮点使用可往返有限十进制。路径包含 tab/newline 时在扫描边界拒绝，不做 JSON 转义。组和代表字段在每个成员行重复保存。

- [ ] **Step 5: 跑 GREEN**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-windows --test atomic_file --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test analysis_result_file --locked -- --test-threads=1`

  Expected: 原子替换、旧结果保护、校验损坏拒绝、仅最近结果保留全部 PASS。

- [ ] **Step 6: 提交**

  ```powershell
  git add crates/windows/src/atomic_file.rs crates/windows/src/lib.rs crates/windows/tests/atomic_file.rs crates/node-engine/src/analysis/result_file.rs crates/node-engine/src/analysis/mod.rs crates/node-engine/tests/analysis_result_file.rs
  git commit -m "feat: publish latest analysis as verified tsv"
  ```

---

## Task 10：把本地分析运行态移出 SQLite

**Files:**

- Create: `crates/node-engine/src/analysis/model.rs`
- Modify: `crates/node-engine/src/analysis/mod.rs`
- Modify: `crates/node-engine/src/analysis/exact.rs`
- Modify: `crates/node-engine/src/analysis/image.rs`
- Modify: `crates/node-engine/src/analysis/video.rs`
- Modify: `crates/node-engine/src/analysis/grouping.rs`
- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/tests/local_analysis.rs`
- Modify: `crates/node-engine/tests/representative_grouping.rs`
- Modify: `crates/node-store/tests/analysis_state.rs`

**Interfaces:**

```rust
pub struct LocalAnalysisRun {
    pub run_id: AnalysisRunId,
    pub library_revision: u64,
    pub thresholds: Thresholds,
    pub inputs: Vec<ScanAnalysisInput>,
}

impl LocalAnalysisEngine {
    pub fn begin(
        catalog: &ScanSessionCatalog,
        selected_tasks: &[TaskId],
        library_revision: u64,
        thresholds: Thresholds,
    ) -> Result<LocalAnalysisRun, AnalysisBlocked>;

    pub async fn run<P: Stage2Processor>(
        store: &mut NodeStore,
        run: LocalAnalysisRun,
        processor: &mut P,
        result_writer: AnalysisResultWriter,
        reporter: &RuntimeTaskReporter,
    ) -> Result<PublishedAnalysisResult, AnalysisBlocked>;
}
```

- [ ] **Step 1: 写零分析表写入 RED**

  用真实 SQLite trace 完成精确和相似分析，断言 `analysis_runs/analysis_run_stages/analysis_run_inputs/candidate_pairs/duplicate_groups/group_members/review_marks` 的 INSERT/UPDATE/DELETE 全为 0，同时结果 TSV 可读。

  ```rust
  #[tokio::test]
  async fn local_analysis_publishes_tsv_without_sqlite_runtime_writes() {
      let report = run_local_analysis_with_sql_trace().await.unwrap();
      assert_eq!(report.analysis_table_writes, 0);
      assert!(report.result_path.ends_with("latest-analysis.result.tsv"));
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test local_analysis local_analysis_publishes_tsv_without_sqlite_runtime_writes --locked -- --test-threads=1`

  Expected: FAIL，当前运行、输入、候选和组写入 SQLite。

- [ ] **Step 2: 抽出进程内模型**

  把 `AnalysisInput/CandidateWrite/GroupWrite/GroupMemberWrite` 的运行期版本放到 `analysis/model.rs`；候选和分组纯函数改消费这些值。NodeStore 仍只提供按 ContentKey 读取完整特征和活动文件事实。

- [ ] **Step 3: 改 begin 门禁**

  只允许 `ScanSessionCatalog` 中当前进程已完成的任务；合并、排序、去重 `ScanAnalysisInput`，冻结阈值和当前 library revision。旧 task ID 返回 NotFound，活动/失败任务返回明确门禁错误。

- [ ] **Step 4: 改 run 状态链**

  阶段只发布到 RuntimeTaskRegistry；候选和最终组保存在当前 background job 内存。运行开始时创建空 `latest-analysis.partial.tsv` 作为唯一 staging 身份；最终组产生后才顺序写 Task 9 的 `H/M/F`，不把输入和候选重复落盘。缺二筛通过 Task 7 生成瞬态文件；`Incomplete` 删除 partial 并保持失败/partial 运行终态，不覆盖旧成功结果。

- [ ] **Step 5: actor 安装最新结果**

  `BackgroundOutcome::LocalAnalysis` 成功时替换 `EngineState.latest_analysis`；`QueryAnalysisRun` 从活动 job/runtime snapshot 或 latest metadata 返回。失败和取消仅清 partial、结束 runtime task，旧 latest 保留。

- [ ] **Step 6: 反向门禁旧 SQLite 写入**

  把 `crates/node-store/tests/analysis_state.rs` 改为验证启动清理和“产品本地分析不调用旧 API”；保留 schema 兼容测试，不再把恢复分析当正确行为。

- [ ] **Step 7: 跑 GREEN**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test representative_grouping --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --test analysis_state --locked -- --test-threads=1`

  Expected: 算法得分和分组不变，SQLite 分析表零产品写入，最新结果发布/旧结果保护通过。

- [ ] **Step 8: 提交**

  ```powershell
  git add crates/node-engine/src/analysis/model.rs crates/node-engine/src/analysis/mod.rs crates/node-engine/src/analysis/exact.rs crates/node-engine/src/analysis/image.rs crates/node-engine/src/analysis/video.rs crates/node-engine/src/analysis/grouping.rs crates/node-engine/src/analysis/phase2.rs crates/node-engine/src/actor.rs crates/node-engine/tests/local_analysis.rs crates/node-engine/tests/representative_grouping.rs crates/node-store/tests/analysis_state.rs
  git commit -m "refactor: keep local analysis outside sqlite"
  ```

---

## Task 11：实现无 `.idx` 的本地结果滑动窗口协议

**Files:**

- Create: `crates/node-engine/src/analysis/result_reader.rs`
- Modify: `crates/node-engine/src/analysis/mod.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `proto/node.proto`
- Modify: `crates/protocol/src/lib.rs`
- Create: `crates/protocol/tests/local_result_window_wire.rs`
- Modify: `crates/desktop-core/src/results.rs`
- Modify: `crates/desktop-core/src/node_session.rs`
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-core/tests/local_node_e2e.rs`
- Create: `crates/node-engine/tests/analysis_result_window.rs`

**Interfaces:**

```rust
pub enum LocalResultWindowKind {
    Groups(GroupKind),
    Members { group_id: String },
}

pub struct LatestAnalysisReader {
    metadata: PublishedAnalysisResult,
    group_offsets: Vec<u64>,
    member_offsets: BTreeMap<String, Vec<u64>>,
}

impl LatestAnalysisReader {
    pub fn open_verified(path: &Path) -> Result<Self, AnalysisResultError>;
    pub fn read_window(&mut self, kind: LocalResultWindowKind, start: u64, count: u32) -> Result<LocalResultWindow, AnalysisResultError>;
}
```

新增一个 envelope tag 46 的 `ReadLocalResultWindow`，请求和响应共用同一 message：

```protobuf
enum LocalResultWindowKind {
  LOCAL_RESULT_WINDOW_UNSPECIFIED = 0;
  LOCAL_RESULT_WINDOW_GROUPS = 1;
  LOCAL_RESULT_WINDOW_MEMBERS = 2;
}

message ReadLocalResultWindow {
  string analysis_run_id = 1;
  LocalResultWindowKind kind = 2;
  string group_id = 3;
  uint64 start_index = 4;
  uint32 visible_count = 5;
  uint64 total_rows = 6;
  bool stale = 7;
  uint64 result_revision = 8;
  uint64 current_revision = 9;
  repeated DuplicateGroup groups = 10;
  repeated GroupMember members = 11;
}

// 在既有 GroupMember message 的 fields 1..11 之后追加：
string display_path = 12;
```

- [ ] **Step 1: 写完整校验和无 `.idx` RED**

  第一次打开顺序扫描文件，校验版本、每行列数、成员数和 footer SHA；只保存 `u64` 偏移，不保存全部 row 对象，不创建任何 sibling `.idx`。损坏文件拒绝整个展示。关闭并重启 Node 后，合法 result 头中的 latest analysis ID 仍可读取，partial 和 review 决定均已清空。

- [ ] **Step 2: 写窗口行为 RED**

  生成 10,000 成员结果，依次读取 `[0,100)`、`[5_000,5_100)`、回到 `[50,150)`；断言顺序正确、峰值解析对象不超过窗口+预读、偏移可复用、进程重开后重新扫描。

  ```rust
  #[test]
  fn result_windows_reuse_memory_offsets_without_persistent_index() {
      let mut reader = LatestAnalysisReader::open_verified(&fixture_10k()).unwrap();
      assert_eq!(reader.read_window(groups(), 5_000, 100).unwrap().items.len(), 100);
      assert!(!fixture_dir().join("latest-analysis.result.tsv.idx").exists());
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test analysis_result_window result_windows_reuse_memory_offsets_without_persistent_index --locked -- --test-threads=1`

  Expected: FAIL，reader 尚不存在。

- [ ] **Step 3: 实现组/成员偏移**

  `open_verified` 后台顺序扫描并建立组首行偏移与每组成员偏移；内存只保存偏移和最小组摘要。窗口读取 seek 到已知偏移并只解析请求行数加前后各一屏预读。不得生成 `.idx` 或把所有 `AnalysisResultRow` 放入 Vec。

- [ ] **Step 4: 实现 stale 门禁**

  Node 启动时若 result 存在则校验并安装 reader metadata，若不存在则 latest 为空。每次响应比较结果 header revision 与 `NodeStore::library_revision()`；不一致仍返回只读行且 `stale=true`。请求 ID 不是 result 头中的 latest analysis ID 时返回 NotFound；损坏结果返回明确 InvalidResult，不回退旧 SQLite group 表。

- [ ] **Step 5: 增加 wire 和 Desktop Core 映射**

  使用新 tag 46，不复用旧 tag；现有 V5 field number 保持不变，`GroupMember.display_path=12` 只作末尾追加，`PROTOCOL_VERSION` 保持 5。`NodeSession::read_local_result_window` 映射成 `LocalResultWindow`。结果文件直接提供 display path；宽高和 Quality 对窗口内唯一 ContentKey 调用现有 `lookup_base_cache_by_keys` 批量补齐，不执行逐成员 SELECT。本地来源走窗口；中心来源继续由 Desktop Core 内部游标缓存填充窗口，UI 不接触 cursor。

- [ ] **Step 6: 跑 GREEN**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test analysis_result_window --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-protocol --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-core --test local_node_e2e --locked -- --test-threads=1`

  Expected: 本地窗口、stale、无 idx、wire 往返和中心现有结果读取全部 PASS。

- [ ] **Step 7: 提交**

  ```powershell
  git add crates/node-engine/src/analysis/result_reader.rs crates/node-engine/src/analysis/mod.rs crates/node-engine/src/actor.rs proto/node.proto crates/protocol/src/lib.rs crates/protocol/tests/local_result_window_wire.rs crates/desktop-core/src/results.rs crates/desktop-core/src/node_session.rs crates/desktop-core/src/app.rs crates/desktop-core/tests/local_node_e2e.rs crates/node-engine/tests/analysis_result_window.rs
  git commit -m "feat: stream local results through sliding windows"
  ```

---

## Task 12：把 Slint 结果列表改为滑动窗口并将复核留在内存

**Files:**

- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-core/src/results.rs`
- Modify: `crates/desktop-core/src/review.rs`
- Modify: `crates/desktop-core/src/delete.rs`
- Modify: `crates/desktop-core/tests/review_delete.rs`
- Modify: `crates/desktop-ui/src/bindings.rs`
- Modify: `crates/desktop-ui/src/models.rs`
- Modify: `crates/desktop-ui/ui/app.slint`
- Modify: `crates/desktop-ui/ui/pages/duplicate-workspace.slint`
- Modify: `crates/desktop-ui/ui/components/group-table.slint`
- Modify: `crates/desktop-ui/ui/components/member-list.slint`
- Modify: `crates/desktop-ui/ui/pages/review-delete-workspace.slint`
- Modify: `crates/desktop-ui/tests/bindings_contract.rs`
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`

**Interfaces:**

```rust
pub struct ResultWindowRequest {
    pub scope: ResultScope,
    pub node_index: usize,
    pub analysis_run_id: String,
    pub kind: GroupKind,
    pub start_index: u64,
    pub visible_count: u32,
}

// 在现有 UiCommand 中新增以下两个精确变体。
pub enum ResultWindowCommand {
    RequestGroupWindow(ResultWindowRequest),
    RequestMemberWindow { request: ResultWindowRequest, group_id: String },
}

pub struct ResultWindowState<T> {
    pub start_index: u64,
    pub total_rows: u64,
    pub items: Vec<T>,
    pub loading: bool,
    pub stale: bool,
}
```

- [ ] **Step 1: 写无分页 UI RED**

  真实 `MainWindow` 断言不再出现“加载更多/下一页”，滚动组表和成员表会发送 `start_index + visible_count`，不发送 cursor。

  ```rust
  #[test]
  fn local_results_request_windows_when_scroll_position_changes() {
      let window = window_with_10k_result_rows();
      scroll_group_table_to_row(&window, 5_000);
      assert_eq!(next_command(), expected_group_window(4_950, 200));
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-ui --test window_contract local_results_request_windows_when_scroll_position_changes --locked -- --test-threads=1`

  Expected: FAIL，当前 UI 只有 cursor 和“加载更多”。

- [ ] **Step 2: 改 Slint 窗口模型**

  删除 `group-next-cursor/member-next-cursor` 属性和加载更多按钮。组/成员行固定高度；`ScrollView.viewport-y` 变化后按首个可见行计算 start，前后各预读一屏，并去重同一在途窗口请求。模型只保存当前窗口，不累加历史页。

- [ ] **Step 3: 保留中心模式透明窗口**

  中心模式滚动也使用同一 UI 命令。Desktop Core 内部按需连续调用既有 PostgreSQL cursor API 填充目标窗口；cursor 只存在 core cache，UI 属性和回调不暴露分页。

- [ ] **Step 4: 展示 stale 和加载状态**

  `loading=true` 显示“正在加载结果窗口”；`stale=true` 显示“文件库已变化，结果只读”，禁用 SaveReview、快捷复核、PrepareDelete 和 ConfirmDelete，但仍允许滚动，并允许预览当前仍存在的文件。

- [ ] **Step 5: 让复核只存在内存**

  `ReviewBoard` 以 `(analysis_run_id, group_id, LocationKey)` 为键作为 Desktop UI 会话缓存；只有 Node 的 `SaveReviewMark` ACK 后才更新。Task 13 的 Node 进程内 registry 是创建删除计划时的权威复核事实，窗口替换后 Desktop 只把已 ACK 标记合并回行模型，双方均不写 SQLite。

- [ ] **Step 6: 跑 GREEN**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-core --test review_delete --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1`

  Expected: 滑动窗口、回滚滚动、窗口替换、中心透明缓存、stale 禁用和复核内存测试全部 PASS。

- [ ] **Step 7: 提交**

  ```powershell
  git add crates/desktop-core/src/app.rs crates/desktop-core/src/results.rs crates/desktop-core/src/review.rs crates/desktop-core/src/delete.rs crates/desktop-core/tests/review_delete.rs crates/desktop-ui/src/bindings.rs crates/desktop-ui/src/models.rs crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/pages/duplicate-workspace.slint crates/desktop-ui/ui/components/group-table.slint crates/desktop-ui/ui/components/member-list.slint crates/desktop-ui/ui/pages/review-delete-workspace.slint crates/desktop-ui/tests/bindings_contract.rs crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs
  git commit -m "feat: virtualize result browsing with sliding windows"
  ```

---

## Task 13：移除删除历史写入并完成端到端门禁

**Files:**

- Create: `crates/node-engine/src/analysis/review_registry.rs`
- Modify: `crates/node-engine/src/analysis/mod.rs`
- Modify: `crates/node-engine/src/delete.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-store/src/inventory.rs`
- Modify: `crates/node-store/src/outbox.rs`
- Modify: `crates/node-store/src/snapshot.rs`
- Modify: `crates/node-engine/tests/delete.rs`
- Modify: `crates/node-engine/tests/delete_runtime_details.rs`
- Modify: `crates/node-store/tests/delete_group_update.rs`
- Modify: `crates/node-store/src/result_summary.rs`
- Modify: `crates/node-store/examples/export_scan_result_summary.rs`
- Modify: `crates/node-store/tests/result_summary_export.rs`
- Modify: `crates/desktop-core/examples/runtime_acceptance.rs`
- Modify: `crates/desktop-core/tests/runtime_acceptance_contract.rs`
- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `AGENTS.md`
- Create: `docs/verification/2026-08-28-transient-task-files-and-latest-analysis.md`

**Interfaces:**

```rust
#[derive(Default)]
pub struct ReviewRegistry {
    decisions: BTreeMap<(AnalysisRunId, String, LocationKey), ReviewDecision>,
}

impl NodeStore {
    /// 提交本批成功删除形成的当前文件事实；有成功项时 revision 推进一步。
    pub fn deactivate_deleted_files(
        &mut self,
        rows: &[VerifiedDeletedFile],
        now_ms: i64,
    ) -> Result<u64, StoreError>;
}
```

- [ ] **Step 1: 写删除零历史 RED**

  完成一组回收站 fixture 删除后断言 `delete_batches/delete_items/deletion_tombstones/review_marks/group_members` 没有新增行；`files.active=0`、file outbox 和 revision 正确。失败项只出现在 RuntimeTaskRegistry/返回值和普通日志。

  ```rust
  #[tokio::test]
  async fn successful_delete_updates_current_fact_without_history_rows() {
      let result = execute_verified_delete_fixture().await.unwrap();
      assert_eq!(result.sqlite_delete_history_rows, 0);
      assert!(!result.file_active);
      assert_eq!(result.revision_after, result.revision_before + 1);
  }
  ```

  Run: `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --test delete successful_delete_updates_current_fact_without_history_rows --locked -- --test-threads=1`

  Expected: FAIL，当前删除会持久化 batch/item/tombstone 并修改持久组。

- [ ] **Step 2: 改复核和删除计划所有权**

  `SaveReviewMark` 更新 `EngineState.review_registry`；新分析成功或 Node 重启清空旧 registry。创建删除计划先核对 latest run ID、result revision 与当前 revision，并确认每组至少一个 Keep；计划冻结到当前 active job 内存。

- [ ] **Step 3: 保留逐文件安全复核**

  每项执行前继续核对活动 LocationKey、实际大小和 1 MiB 缓冲流式 MD5。成功项批量提交 `files.active=0` 和 file outbox；同一提交有一个或多个成功项时 revision 只前进一步，revision 是版本号而非删除计数器。同一已冻结计划可继续，其后不能从 stale 结果创建新计划。

- [ ] **Step 4: 切断旧运行态 API 的产品调用**

  运行以下只读检查并处理所有产品命中；测试 fixture 和 schema 定义可保留：

  ```powershell
  rg -n "(self\.)?store\.(reserve_scan_path|recover_active_computation_tasks|transition_analysis_run|replace_candidates|replace_groups|save_review_mark|create_delete_batch|apply_delete_results)" crates/node-engine/src apps
  ```

  Expected: 产品运行路径无旧 SQLite 任务/分析/复核/删除写调用。

- [ ] **Step 5: 改验收结果导出边界**

  `export_scan_result_summary` 不再按 SQLite task ID 连接 task_items；改为接收一到多个规范媒体根，从 `files.active + contents + 特征表` 导出固定 TSV。`runtime_acceptance` 在任务 completed 后、关闭 Node 前记录当前 task ID、根、outbox highwater 和任务文件汇总；PowerShell evidence 改用 `result-summary.tsv` 和 SHA-256，不把测试 EXE 放入正式包。

- [ ] **Step 6: 更新运行时验收脚本**

  `Measure-RustV2RuntimeAcceptance.ps1` 接受 `string[] MediaRoot`、`-Enumerator everything`、`-CompleteWhenTaskTerminal`。默认显式启动/确认同目录 Everything；任务 completed/failed/cancelled 后立即最终化，1,800 秒只是超时。报告新增每个任务文件 lane 的 P/C/F、缓存命中未入文件数、SQLite task/analysis/delete 写入数、两个物理盘 I/O 重叠和 CPU/Worker 指标。

- [ ] **Step 7: 跑定向和全 crate 回归**

  Run:

  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-node-engine --features test-hooks --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-protocol --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-core --locked -- --test-threads=1`
  - `$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-target'; cargo test -p dedup-desktop-ui --locked -- --test-threads=1`
  - `pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1`
  - `pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1`
  - `pwsh -NoProfile -File tests\windows\Test-RustV2Package.ps1`
  - `cargo fmt --all -- --check`
  - `git diff --check`

  Expected: 全部 PASS；只允许既有明确记录的 warning，不接受新失败。

- [ ] **Step 8: 运行一次双物理盘真实媒体全量验收**

  先用精确 target 构建候选正式包和两个外置测试工具：

  ```powershell
  $env:CARGO_TARGET_DIR='C:\tmp\rust-v2-transient-task-release-target'
  cargo build -p dedup-desktop-core --example runtime_acceptance --release --locked --target x86_64-pc-windows-msvc
  cargo build -p dedup-node-store --features acceptance-tools --example export_scan_result_summary --release --locked --target x86_64-pc-windows-msvc
  pwsh -NoProfile -File scripts\build-release.ps1 -CargoTargetDir C:\tmp\rust-v2-transient-task-release-target
  pwsh -NoProfile -File scripts\verify-release.ps1 -Package D:\code\mySingerServer\dist-rust-v2\mySingerServer-rust-v2-win-x64.zip
  ```

  再使用 staging 和外置 acceptance client/exporter，Worker 20、总读取线程 12、Everything 枚举，媒体根只读：

  ```powershell
  pwsh -NoProfile -File tests\windows\Measure-RustV2RuntimeAcceptance.ps1 `
    -MediaRoot @('H:\pik\00000000000','I:\tmp') `
    -DurationSeconds 1800 -SampleSeconds 2 `
    -WorkerCount 20 -TotalReadThreads 12 `
    -Enumerator everything -CompleteWhenTaskTerminal `
    -ReleaseRoot 'D:\code\mySingerServer\dist-rust-v2\staging' `
    -AcceptanceClientPath 'C:\tmp\rust-v2-transient-task-release-target\x86_64-pc-windows-msvc\release\examples\runtime_acceptance.exe' `
    -ResultExporterPath 'C:\tmp\rust-v2-transient-task-release-target\x86_64-pc-windows-msvc\release\examples\export_scan_result_summary.exe'
  ```

  命令执行前验证上述三个绝对路径存在，并把正式 ZIP、staging manifest、两个测试 EXE 的 SHA-256 写入验证文档。真实全量只运行一次，不继续 A-3 或六轮 A/B。第二次缓存命中行为只用小型固定 fixture 验证，不重复全量真实媒体。

  Acceptance:

  - Node 任务终态为 completed；单文件失败按明细记录但不阻塞其他 lane。
  - `PhysicalDisk` 对应 H/I 的任务文件分别存在并由真实 dispatcher 消费；完整缓存命中不出现任务行。
  - 两块物理盘在有 ready 项的共同区间存在重叠读取；额度和分配符合配置权重，HDD 老化保护无饥饿。
  - 任务/分析/删除 SQLite 表保持 0 产品写入；文件/特征/outbox 正确。
  - 结果 TSV footer、row count 和 SHA-256 通过；没有 `.idx`、JSON 任务文件或 JSON 分析结果。

- [ ] **Step 9: 更新长期文档和验证证据**

  `AGENTS.md` 只写已经落地的最终所有权、文件格式、重启语义、滑动窗口和验证命令；验证文档记录 RED/GREEN 命令、测试数量、真实媒体根、任务文件统计、每盘 I/O、CPU、Worker、结果 hash 和未触碰 `I:\Tool`。

- [ ] **Step 10: 独立最终审查与修复**

  使用 `gpt-5.6-sol`、`max` 只读审查四个边界：任务文件是否为真实来源、ACK 前状态是否仍 P、旧 SQLite 运行态是否还有产品写入、stale 结果是否能绕过删除门禁。对 Important 以上且可复现的问题按 TDD 修复并复跑受影响门禁。

- [ ] **Step 11: 构建并验证候选包，不部署**

  Run:

  - `pwsh -NoProfile -File scripts\build-release.ps1 -CargoTargetDir C:\tmp\rust-v2-transient-task-release-target`
  - `pwsh -NoProfile -File scripts\verify-release.ps1 -Package dist-rust-v2\mySingerServer-rust-v2-win-x64.zip`

  Expected: `RUST_V2_RELEASE_BUILD_PASS` 与 `PACKAGE_PASS`；正式 ZIP 仍只有 desktop/node/worker/Everything 四个顶层 EXE。不得复制或替换 `I:\Tool`。

- [ ] **Step 12: 提交最终集成**

  ```powershell
  git add crates/node-engine/src/analysis/review_registry.rs crates/node-engine/src/analysis/mod.rs crates/node-engine/src/delete.rs crates/node-engine/src/actor.rs crates/node-store/src/inventory.rs crates/node-store/src/outbox.rs crates/node-store/src/snapshot.rs crates/node-engine/tests/delete.rs crates/node-engine/tests/delete_runtime_details.rs crates/node-store/tests/delete_group_update.rs crates/node-store/src/result_summary.rs crates/node-store/examples/export_scan_result_summary.rs crates/node-store/tests/result_summary_export.rs crates/desktop-core/examples/runtime_acceptance.rs crates/desktop-core/tests/runtime_acceptance_contract.rs tests/windows/Measure-RustV2RuntimeAcceptance.ps1 tests/windows/New-RustV2RuntimeAcceptanceReport.ps1 tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1 tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1 AGENTS.md docs/verification/2026-08-28-transient-task-files-and-latest-analysis.md
  git commit -m "feat: complete transient runtime storage migration"
  ```

---

## Final Verification Checklist

- [ ] 1,000 项基础路径缓存查询的业务 `SELECT` 数量与批次大小无关，查询前任务表写入为 0。
- [ ] 完整缓存命中不生成任务行；路径未命中只先记录 NEEDS_MD5；部分缓存只包含真实缺失位。
- [ ] 每个物理盘有独立 TSV，实际 dispatcher 从 TSV 取任务，配置权重和老化保护保持。
- [ ] SQLite ACK 前行状态为 P，成功后 C，单文件失败 F；崩溃不阻塞其他物理盘。
- [ ] 扫描成功收尾一次提交 files/outbox/revision；失败、取消和枚举错误不失活旧路径。
- [ ] Node 重启清理 runtime/partial 和旧运行态表，不恢复 task/analysis/delete ID；长期缓存仍命中。
- [ ] 本地分析不写 SQLite 运行态表，只原子发布最近一次成功 TSV；旧成功结果不被失败运行覆盖。
- [ ] 结果读取不分页、不建 `.idx`、不把全部对象载入内存；UI 只维护滑动窗口。
- [ ] revision 不匹配时结果仍只读可看，但复核和删除入口全部禁用。
- [ ] 删除计划/结果/历史不持久化；成功只更新 files.active、file outbox 和 revision。
- [ ] 当前生产调用中不存在旧任务/分析/复核/删除 SQLite 写入。
- [ ] 定向、crate、Windows harness、格式和 diff 门禁全部通过。
- [ ] 一次 H/I 双物理盘全量真实媒体测试通过，完成即结束，不进行六轮重复测试。
- [ ] 最终 `gpt-5.6-sol max` 审查无未解决 Important；候选包验证通过且未部署到 `I:\Tool`。
