# Rust V2 按物理盘加权任务分发 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在文件枚举前冻结扫描根的物理盘拓扑，并让 Hash、媒体读取和 Worker 派发在多物理盘均有可运行任务时按 Node 配置权重公平推进。

**Architecture:** 新增任务内 `ScanDiskPlan`，把扫描根、物理盘 lane 和文件归属固定在枚举之前；`WeightedDiskDispatcher` 维护按盘 Hash/Media Ready Queue、跨窗口亏欠游标和 RAII admission。SQLite 通过指定 `item_id` 的原子领取保持恢复权威，现有 `DiskReadScheduler` 继续执行真实读取许可、复合盘原子占用和最终硬上限。

**Tech Stack:** Rust 2024、Tokio、rusqlite/SQLite schema 3、Windows Storage API、Protobuf V4、PowerShell 7、Cargo 集成测试。

**Spec:** `docs/superpowers/specs/2026-08-27-physical-disk-weighted-task-dispatch-design.md`

## Global Constraints

- 读取权重和逐盘硬上限只来自 `read.hdd_threads_per_disk`、`read.ssd_threads_per_disk`、`read.unknown_threads_per_disk`；全局硬上限只来自 `read.total_threads`。
- 不修改上述配置默认值，不按文件大小动态猜测权重，不自动调整 Worker 数或 FFmpeg 线程数。
- 所有扫描根必须在 Everything/Walker 获取文件列表前完成物理盘编号和 HDD/SSD/Unknown 类型解析；失败码固定为 `SCAN_ROOT_STORAGE_RESOLVE_FAILED`。
- 同一底层物理盘的多个根只形成一条 lane；复合卷同时占用排序去重后的全部底层物理盘。
- `DiskReadScheduler` 仍是真实读取许可的最终权威；新增 dispatcher 不得绕过它。
- `task_items`、`task_scan_roots` 和 SQLite `PRAGMA user_version=3` 保持不变，不新增迁移或兼容层。
- 单文件失败不终止整个扫描任务；根规划、调度计数漂移等任务级基础设施错误才使任务失败。
- 全部新增方法、类型、字段和关键变量使用中文注释说明用途、用法、所有权、失败及恢复逻辑。
- 保留当前未提交改动；禁止 `git add -A`、`git clean`、`git reset`。每次提交只暂存本任务列出的文件。
- 当前重叠脏文件为 `crates/desktop-core/examples/runtime_acceptance.rs`、`crates/desktop-core/tests/runtime_acceptance_contract.rs`、`tests/windows/Measure-RustV2RuntimeAcceptance.ps1`、`tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`；修改前用带精确路径参数的 `git diff --` 保存基线，只做增量补丁，不覆盖已有内容。
- 不修改、打包进或替换 `I:\Tool`；真实媒体验收只写 `C:\tmp` 和仓库验证文档。
- 不把 Worker 崩溃、FFmpeg 解码、SSD 识别算法、文件版本一致性或六轮 A/B 测试并入本实施范围。

---

## File Structure

- `crates/windows/src/storage_device.rs`：一次解析扫描根的物理盘身份、介质类型和嵌套卷挂载边界。
- `crates/windows/src/walker.rs`：保证 Walker 不下钻目录重解析点。
- `crates/node-engine/src/scan/root_plan.rs`：冻结 `ScanDiskPlan`、合并同盘根、执行最具体根归属。
- `crates/node-engine/src/scan/disk_dispatch.rs`：实现共享加权亏欠游标、Hash/Media Ready Queue 和 `DiskAdmissionLease`。
- `crates/node-engine/src/io/fairness.rs`：保存 Hash/Media 类别选择与老化常量，供 dispatcher 和真实 scheduler 共用。
- `crates/node-engine/src/scan/input_order.rs`：使用冻结计划生成 path-cache 的按盘加权供给顺序，同盘内部仍按根轮转。
- `crates/node-engine/src/scan/pipeline.rs`：只使用冻结根计划申请真实读取许可，不再为每个文件查询 Windows 存储拓扑。
- `crates/node-engine/src/scan/base_compute.rs`：维护动态 Hash/Media Ready Queue、指定项领取、资源门禁、Worker 派发和恢复种子。
- `crates/node-store/src/tasks.rs`：提供指定项原子领取、扫描根和 queued 扫描项读取，不改变表结构。
- `crates/node-engine/src/runtime_tasks.rs` 与 `proto/node.proto`：投影 dispatcher 逐盘指标，保留现有真实 permit 指标。
- `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`：只在多盘同时 Ready 且未被额度/Worker 阻塞的窗口评估权重。

---

### Task 1: 冻结枚举前扫描根物理盘计划

**Files:**
- Create: `crates/node-engine/src/scan/root_plan.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/windows/src/storage_device.rs`
- Modify: `crates/windows/src/lib.rs`
- Modify: `crates/windows/src/walker.rs`
- Test: `crates/windows/tests/storage_device.rs`
- Test: `crates/node-engine/tests/scan_roots.rs`

**Interfaces:**
- Consumes: `DisplayPath`、`NormalizedPath`、`DiskReadConfig`、`dedup_windows::LocalDiskKind`，以及根路径的一次 Windows 存储解析。
- Produces: `ScanDiskPlan::build(roots, read_config, resolver) -> Result<Self, ScanError>`、`ScanDiskPlan::assign(scanned) -> Result<Option<PlannedScannedPath>, ScanError>`、`SystemScanRootResolver` 和稳定错误码 `SCAN_ROOT_STORAGE_RESOLVE_FAILED`。

- [ ] **Step 1: 写根计划失败测试，冻结枚举前顺序**

  在 `crates/node-engine/tests/scan_roots.rs` 增加测试 resolver 和 enumerator 共享的事件表，断言事件严格为“解析全部根后才枚举”：

  ```rust
  #[tokio::test]
  async fn physical_storage_is_frozen_before_first_enumerator_call() {
      let events = Arc::new(Mutex::new(Vec::new()));
      let plan = build_scan_disk_plan_for_test(
          [r"H:\pik", r"I:\tmp"],
          test_read_config(1, 5, 1, 6),
          recording_resolver(events.clone(), [(r"H:\pik", vec![0], LocalDiskKind::Hdd),
                                              (r"I:\tmp", vec![1], LocalDiskKind::Ssd)]),
      )
      .unwrap();
      recording_enumerator(events.clone()).enumerate(plan.display_roots()).unwrap();

      assert_eq!(&*events.lock().unwrap(), &["resolve:H:\\pik", "resolve:I:\\tmp", "enumerate"]);
  }
  ```

  同文件增加解析失败测试，断言枚举调用数为零且错误文本包含 `SCAN_ROOT_STORAGE_RESOLVE_FAILED` 和显示根。

- [ ] **Step 2: 运行 RED 测试**

  Run: `cargo test -p dedup-node-engine --features test-hooks --test scan_roots physical_storage_is_frozen_before_first_enumerator_call --locked -- --test-threads=1`

  Expected: FAIL，原因是 `build_scan_disk_plan_for_test` 尚不存在。

- [ ] **Step 3: 定义根计划的拥有型接口**

  在 `root_plan.rs` 实现以下精确结构；测试 resolver 返回拥有型数字集合，生产 resolver 只在这里调用 Windows API：

  ```rust
  pub(crate) const SCAN_ROOT_STORAGE_RESOLVE_FAILED: &str =
      "SCAN_ROOT_STORAGE_RESOLVE_FAILED";

  #[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
  pub(crate) struct PhysicalDiskKey(Box<[u32]>);

  #[derive(Clone, Debug)]
  pub(crate) struct PlannedDisk {
      pub(crate) key: PhysicalDiskKey,
      pub(crate) display_id: String,
      pub(crate) kind: LocalDiskKind,
      pub(crate) configured_weight: usize,
      pub(crate) configured_limit: usize,
  }

  #[derive(Clone, Debug)]
  pub(crate) struct ScanRootPlan {
      pub(crate) display_root: DisplayPath,
      pub(crate) normalized_root: NormalizedPath,
      pub(crate) disk: Arc<PlannedDisk>,
      pub(crate) excluded_nested_volumes: Arc<[NormalizedPath]>,
  }

  #[derive(Clone, Debug)]
  #[doc(hidden)]
  pub struct ScanDiskPlan {
      roots: Arc<[ScanRootPlan]>,
      disks: Arc<BTreeMap<PhysicalDiskKey, Arc<PlannedDisk>>>,
  }

  #[derive(Clone, Debug)]
  pub(crate) struct PlannedScannedPath {
      pub(crate) scanned: ScannedPath,
      pub(crate) disk: Arc<PlannedDisk>,
  }

  pub(crate) trait ScanRootStorageResolver {
      fn resolve(&self, root: &Path) -> io::Result<ResolvedScanRootStorage>;
  }
  ```

  `PhysicalDiskKey` 构造时排序、去重并拒绝空集合；显示身份固定为 `PhysicalDisk0` 或 `PhysicalDisk5+12`。

- [ ] **Step 4: 实现同盘合并、冲突降级和最具体根归属**

  `ScanDiskPlan::build` 先按 `NormalizedPath` 排序去重根，再收集全部观察；相同 key 类型冲突时使用 `Unknown`，有效权重/上限取 Unknown 配置与全部观察类型配置的最小值。`assign` 使用组件数最多、同深规范根最小的规则：嵌套卷返回 `Ok(None)` 供清单过滤，根外文件返回 `InvalidResult`，普通文件返回带共享盘计划的 `Some`。

  ```rust
  impl ScanDiskPlan {
      pub(crate) fn build<R: ScanRootStorageResolver>(
          roots: &[DisplayPath],
          read: &DiskReadConfig,
          resolver: &R,
      ) -> Result<Self, ScanError>;

      pub(crate) fn assign(
          &self,
          scanned: ScannedPath,
      ) -> Result<Option<PlannedScannedPath>, ScanError>;

      pub(crate) fn display_roots(&self) -> Vec<DisplayPath>;
  }
  ```

- [ ] **Step 5: 固定嵌套卷和目录重解析点边界**

  在 `dedup-windows` 增加根级返回值，生产解析一次返回物理盘和根下卷挂载点；Walker 对目录元数据的 `FILE_ATTRIBUTE_REPARSE_POINT` 直接跳过，不打开子目录。

  ```rust
  pub struct ResolvedScanRootStorage {
      location: StorageLocation,
      nested_volume_mount_points: Vec<PathBuf>,
  }

  pub fn resolve_scan_root_storage(root: &Path) -> io::Result<ResolvedScanRootStorage>;
  ```

  生产实现先用现有 `GetVolumeNameForVolumeMountPointW` 取得根卷名，再用 `FindFirstVolumeMountPointW`、`FindNextVolumeMountPointW` 和 `FindVolumeMountPointClose` 枚举该卷的挂载目录，只保留当前扫描根之下的规范路径。Walker 使用 `std::os::windows::fs::MetadataExt::file_attributes()` 检查 `FILE_ATTRIBUTE_REPARSE_POINT`，命中时不压入目录栈。

  `EverythingEnumerator` 的索引结果在暴露为本次稳定清单前按 `excluded_nested_volumes` 过滤；Walker 从源头不下钻。根外结果仍返回 `InvalidResult`，嵌套卷结果则排除而不使整个扫描失败；不对每个媒体文件再次调用存储设备 API。

- [ ] **Step 6: 把根规划放到生产枚举调用之前**

  在 `actor.rs` 的扫描后台入口先创建 `ScanDiskPlan`，成功后才执行 `ensure_everything_ready` 和 `FileEnumerator::enumerate`；把 plan 传入后续 `run_enumerated_scan_to_base_compute`。规划失败保存 `EnumerateFiles=Failed`，不创建远端缓存、读取器或 Worker 作业。

  ```rust
  let disk_plan = ScanDiskPlan::build(
      &options.roots,
      &read_config,
      &SystemScanRootResolver,
  )?;
  let rows = enumerator.enumerate(disk_plan.display_roots().as_slice());
  ```

- [ ] **Step 7: 运行根计划回归并提交**

  Run: `cargo test -p dedup-windows --test storage_device --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --features test-hooks --test scan_roots --locked -- --test-threads=1`

  Expected: 两组 PASS；同盘双根一条 lane、双物理盘两条 lane、Unknown、复合卷、冲突降级、解析失败和嵌套卷拒绝均有行为断言。

  ```powershell
  git add crates/windows/src/storage_device.rs crates/windows/src/lib.rs crates/windows/src/walker.rs crates/windows/tests/storage_device.rs crates/node-engine/src/scan/root_plan.rs crates/node-engine/src/scan/mod.rs crates/node-engine/src/actor.rs crates/node-engine/tests/scan_roots.rs
  git commit -m "feat: freeze scan disk topology before enumeration"
  ```

---

### Task 2: 实现共享加权游标并生成 path-cache 供给顺序

**Files:**
- Create: `crates/node-engine/src/scan/disk_dispatch.rs`
- Modify: `crates/node-engine/src/scan/input_order.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Test: `crates/node-engine/src/scan/disk_dispatch.rs`
- Test: `crates/node-engine/src/scan/input_order.rs`

**Interfaces:**
- Consumes: Task 1 的 `PhysicalDiskKey`、`PlannedDisk`、`ScanDiskPlan` 和 `PlannedScannedPath`。
- Produces: `WeightedLaneCursor<K>::select` 和 `order_rows_for_path_cache(plan, rows)`；Task 3 的动态 dispatcher 复用同一游标。

- [ ] **Step 1: 写 WDRR 跨窗口 RED 测试**

  在 `disk_dispatch.rs` 的单元测试中固定两条持续非空 lane，连续两次每次只取 3 个选择：

  ```rust
  #[test]
  fn ssd_five_hdd_one_keeps_deficit_across_global_three_windows() {
      let mut cursor = WeightedLaneCursor::new([(disk(0), 5), (disk(1), 1)]).unwrap();
      let first = select_n(&mut cursor, 3, |_| true);
      let second = select_n(&mut cursor, 3, |_| true);

      assert_eq!(first, [disk(0), disk(0), disk(0)]);
      assert_eq!([first, second].concat().iter().filter(|key| **key == disk(0)).count(), 5);
      assert_eq!([first, second].concat().iter().filter(|key| **key == disk(1)).count(), 1);
  }
  ```

  另写空 lane 不积攒额度、lane 恢复后不突发、两个 SSD/两个 HDD 各自计权测试。

- [ ] **Step 2: 运行 RED 单元测试**

  Run: `cargo test -p dedup-node-engine scan::disk_dispatch::tests --locked --lib -- --test-threads=1`

  Expected: FAIL，原因是 `WeightedLaneCursor` 尚不存在。

- [ ] **Step 3: 实现最小共享游标**

  ```rust
  pub(crate) struct WeightedLaneCursor<K> {
      lanes: Vec<WeightedCursorLane<K>>,
      cursor: usize,
  }

  struct WeightedCursorLane<K> {
      key: K,
      quantum: usize,
      deficit: usize,
      was_ready: bool,
  }

  impl<K: Clone + Ord> WeightedLaneCursor<K> {
      pub(crate) fn new(
          lanes: impl IntoIterator<Item = (K, usize)>,
      ) -> Result<Self, ScanError>;

      pub(crate) fn select(
          &mut self,
          is_ready: impl Fn(&K) -> bool,
      ) -> Option<K>;
  }
  ```

  选择成功扣 1；额度耗尽移动游标；空 lane 当场清空 deficit；游标和未用 deficit 跨调用保留。所有 quantum 为零或重复 key 直接返回 `ScanError::Stage1`。

- [ ] **Step 4: 把输入顺序改为物理盘加权、同盘根轮转**

  `input_order.rs` 先调用 `plan.assign`，按物理盘建立 lane；每条 lane 内保留现有最具体根 bucket 和 1:1 根轮转，lane 之间使用 `WeightedLaneCursor`。

  ```rust
  pub(crate) fn order_rows_for_path_cache(
      plan: &ScanDiskPlan,
      rows: Vec<ScannedPath>,
  ) -> Result<Vec<PlannedScannedPath>, ScanError>;
  ```

  新测试断言 SSD=5/HDD=1 的前 6 行来源为 5:1；H/I 两根同盘不会获得 2 倍盘权；重叠根仍由最具体根接管；输出路径集合和字节总数守恒。

- [ ] **Step 5: 运行回归并提交**

  Run: `cargo test -p dedup-node-engine scan::input_order::tests --locked --lib -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine scan::disk_dispatch::tests --locked --lib -- --test-threads=1`

  Expected: PASS，且没有固定写死 SSD/HDD 数值。

  ```powershell
  git add crates/node-engine/src/scan/disk_dispatch.rs crates/node-engine/src/scan/input_order.rs crates/node-engine/src/scan/mod.rs
  git commit -m "feat: weight path cache feed by physical disk"
  ```

---

### Task 3: 建立动态 Ready Queue 与 admission RAII

**Files:**
- Create: `crates/node-engine/src/io/fairness.rs`
- Modify: `crates/node-engine/src/io/mod.rs`
- Modify: `crates/node-engine/src/io/scheduler.rs`
- Modify: `crates/node-engine/src/scan/disk_dispatch.rs`
- Test: `crates/node-engine/tests/disk_scheduler.rs`

**Interfaces:**
- Consumes: Task 2 的 `WeightedLaneCursor`，现有 `DiskReadClass` 和真实 scheduler 老化语义。
- Produces: `WeightedDiskDispatcher<H, M>`、`DispatchReadiness`、`DispatchItem<H, M>`、`DispatchSelection<H, M>` 和 `DiskAdmissionLease`。

- [ ] **Step 1: 写动态队列、复合盘和 Drop 守恒 RED 测试**

  ```rust
  #[test]
  fn composite_selection_is_atomic_and_drop_releases_every_counter() {
      let mut dispatcher = test_dispatcher(6, [(disk(&[0, 1]), 1, 1)]);
      dispatcher.push_hash(disk(&[0, 1]), "composite").unwrap();

      let selected = dispatcher
          .select(DispatchReadiness { hash: true, media: false })
          .unwrap()
          .unwrap();
      assert_eq!(dispatcher.active_snapshot(), active(1, [(0, 1), (1, 1)]));
      drop(selected);
      assert_eq!(dispatcher.active_snapshot(), active(0, [(0, 0), (1, 0)]));
  }
  ```

  增加取消 future、claim 失败、媒体许可失败、持续 SSD/HDD 5:1、`MAX_CONFLICTING_BYPASSES` 有界、Hash/Media 均不饥饿测试。

- [ ] **Step 2: 运行 RED 测试**

  Run: `cargo test -p dedup-node-engine --test disk_scheduler weighted_dispatch --locked -- --test-threads=1`

  Expected: FAIL，原因是动态 dispatcher 和 admission 类型尚不存在。

- [ ] **Step 3: 提取唯一类别公平策略**

  把 scheduler 内现有常量移到 `io/fairness.rs`，原 scheduler 和新 dispatcher 都调用同一函数；不得复制第二组数值。

  ```rust
  pub(crate) const MEDIA_WEIGHT: u8 = 3;
  pub(crate) const MAX_CONFLICTING_BYPASSES: u8 = 8;

  pub(crate) fn choose_read_class(
      media_streak: u8,
      hash_ready: bool,
      media_ready: bool,
  ) -> Option<DiskReadClass>;
  ```

- [ ] **Step 4: 实现动态 dispatcher 公共边界**

  ```rust
  pub(crate) struct DispatchReadiness {
      pub(crate) hash: bool,
      pub(crate) media: bool,
  }

  pub(crate) enum DispatchItem<H, M> {
      Hash(H),
      Media(M),
  }

  pub(crate) struct DispatchSelection<H, M> {
      pub(crate) item: DispatchItem<H, M>,
      pub(crate) admission: DiskAdmissionLease,
  }

  pub(crate) struct WeightedDiskDispatcher<H, M> {
      lanes: BTreeMap<PhysicalDiskKey, DiskLane<H, M>>,
      cursor: WeightedLaneCursor<PhysicalDiskKey>,
      admission: Arc<AdmissionCounters>,
      aged_reservation: Option<AgedReadyItem>,
  }

  impl<H, M> WeightedDiskDispatcher<H, M> {
      pub(crate) fn push_hash(
          &mut self,
          disk: Arc<PlannedDisk>,
          item: H,
      ) -> Result<(), ScanError>;
      pub(crate) fn push_media(
          &mut self,
          disk: Arc<PlannedDisk>,
          item: M,
      ) -> Result<(), ScanError>;
      pub(crate) fn select(
          &mut self,
          readiness: DispatchReadiness,
      ) -> Result<Option<DispatchSelection<H, M>>, ScanError>;
  }
  ```

  lane 内 Hash/Media 各自稳定 FIFO；磁盘先按 WDRR 选择，再由共享类别策略选择子队列。全局或任一底层盘满时不部分增加计数。

- [ ] **Step 5: 实现 `DiskAdmissionLease` 恰好一次释放**

  ```rust
  pub(crate) struct DiskAdmissionLease {
      counters: Option<ReservedAdmissionCounters>,
  }

  impl Drop for DiskAdmissionLease {
      fn drop(&mut self) {
          if let Some(counters) = self.counters.take() {
              counters.release_all();
          }
      }
  }
  ```

  先预检全局及全部底层盘，再按编号顺序增加；任一步失败回滚此前增加；Drop 顺序相反。计数下溢、超过配置或同一个盘重复保留返回基础设施错误并记录 lane、item 和快照。

- [ ] **Step 6: 运行 scheduler 全量回归并提交**

  Run: `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1`

  Expected: PASS；旧真实 permit FIFO、复合盘、类别权重和老化测试不变，新 admission 终态全部归零。

  ```powershell
  git add crates/node-engine/src/io/fairness.rs crates/node-engine/src/io/mod.rs crates/node-engine/src/io/scheduler.rs crates/node-engine/src/scan/disk_dispatch.rs crates/node-engine/tests/disk_scheduler.rs
  git commit -m "feat: add weighted disk ready dispatcher"
  ```

---

### Task 4: 增加指定任务项原子领取与恢复查询

**Files:**
- Modify: `crates/node-store/src/tasks.rs`
- Modify: `crates/node-store/src/lib.rs`
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
- Test: `crates/node-store/tests/task_recovery.rs`

**Interfaces:**
- Consumes: 现有 `tasks`、`task_items`、`task_scan_roots` schema 3 表。
- Produces: `ClaimItemOutcome`、`QueuedScanItem`、`NodeStore::claim_item`、`NodeStore::scan_task_roots`、`NodeStore::queued_scan_items` 和对应 `BaseStoreHandle` 方法。

- [ ] **Step 1: 写指定项领取竞争 RED 测试**

  ```rust
  #[test]
  fn claim_item_is_atomic_and_rejects_wrong_task_stage_or_terminal_item() {
      let (mut store, task_id, item_id) = queued_scan_item("read_md5");

      assert!(matches!(
          store.claim_item(task_id, &item_id, "read_md5", 10).unwrap(),
          ClaimItemOutcome::Claimed(_)
      ));
      assert_eq!(
          store.claim_item(task_id, &item_id, "read_md5", 11).unwrap(),
          ClaimItemOutcome::Inactive
      );
      let other_item = store
          .append_task_item(task_id, &NewTaskItem::detached("read_md5"), 12)
          .unwrap();
      assert_eq!(
          store.claim_item(task_id, &other_item, "base_compute", 13).unwrap(),
          ClaimItemOutcome::Mismatch
      );
  }
  ```

  再增加 cancel/claim 两个独立连接竞争测试，断言最终只出现 running 或 cancelled 之一。

- [ ] **Step 2: 运行 RED 测试**

  Run: `cargo test -p dedup-node-store --test task_recovery claim_item --locked -- --test-threads=1`

  Expected: FAIL，原因是 `claim_item` 尚不存在。

- [ ] **Step 3: 定义结果和恢复行类型**

  ```rust
  #[derive(Clone, Debug, Eq, PartialEq)]
  pub enum ClaimItemOutcome {
      Claimed(ClaimedTaskItem),
      Inactive,
      Mismatch,
  }

  #[derive(Clone, Debug, Eq, PartialEq)]
  pub struct QueuedScanItem {
      pub item_id: String,
      pub normalized_path: NormalizedPath,
      pub display_path: DisplayPath,
      pub file_size: u64,
      pub content_id: Option<ContentId>,
      pub stage: String,
  }
  ```

- [ ] **Step 4: 用 Immediate 事务实现精确领取**

  `claim_item` 在同一 `TransactionBehavior::Immediate` 事务中读取 task/item/stage，执行带 task、queued、expected_stage 条件的单条 UPDATE，并只在更新 1 行后把任务置为 running。错误 task/item/stage 为 `Mismatch`；任务或项终态为 `Inactive`。

  ```rust
  pub fn claim_item(
      &mut self,
      task_id: TaskId,
      item_id: &str,
      expected_stage: &str,
      now_ms: i64,
  ) -> Result<ClaimItemOutcome, StoreError>;
  ```

- [ ] **Step 5: 读取现有根和 queued 项，不改 schema**

  ```rust
  pub fn scan_task_roots(&self, task_id: TaskId) -> Result<Vec<NormalizedPath>, StoreError>;

  pub fn queued_scan_items(
      &self,
      task_id: TaskId,
  ) -> Result<Vec<QueuedScanItem>, StoreError>;
  ```

  查询按 `normalized_root`、`item_id` 稳定排序；queued 扫描项必须完整具有规范路径、显示路径和大小，否则返回 `InvalidState`。测试断言 `PRAGMA user_version` 仍为 3，schema SQL 无新增列。

- [ ] **Step 6: 接入单写 actor 包装并提交**

  `BaseStoreHandle::claim_item`、`scan_task_roots`、`queued_scan_items` 各只进行一次 actor call；保留 `claim_next_item` 给其他任务流程。

  Run: `cargo test -p dedup-node-store --test task_recovery --locked -- --test-threads=1`

  Expected: PASS。

  ```powershell
  git add crates/node-store/src/tasks.rs crates/node-store/src/lib.rs crates/node-store/tests/task_recovery.rs crates/node-engine/src/scan/base_persistence.rs
  git commit -m "feat: claim scheduled scan items by identity"
  ```

---

### Task 5: 让生产读取器只消费冻结磁盘身份

**Files:**
- Modify: `crates/node-engine/src/io/scheduler.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/scan_runtime_details.rs`

**Interfaces:**
- Consumes: Task 1 的 `Arc<ScanDiskPlan>` 和 `PlannedDisk`，Task 3 保留的真实 `DiskReadScheduler`。
- Produces: `ScheduledFileReader::new(read, workers, plan)` 和 `DiskReadScheduler::acquire_planned(disk_numbers, kind, class)`。

- [ ] **Step 1: 写“根解析一次、文件读取零解析”RED 测试**

  ```rust
  #[tokio::test]
  async fn scheduled_reader_reuses_frozen_root_location_for_hash_and_media() {
      let probes = Arc::new(AtomicUsize::new(0));
      let plan = controlled_disk_plan(&probes, r"H:\media", vec![7], LocalDiskKind::Hdd);
      let (reader, _) = ScheduledFileReader::controlled_for_test(
          &read_config(),
          2,
          block_reader(),
          plan,
      )
      .unwrap();

      reader.read(scanned(r"H:\media\a.mp4"), token()).await.unwrap();
      reader.acquire_media_permit(scanned(r"H:\media\a.mp4"), token()).await.unwrap();
      assert_eq!(probes.load(Ordering::SeqCst), 1);
  }
  ```

- [ ] **Step 2: 运行 RED 测试**

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline scheduled_reader_reuses_frozen_root_location_for_hash_and_media --locked -- --test-threads=1`

  Expected: FAIL；旧读取器仍按每个文件调用 location resolver。

- [ ] **Step 3: 增加真实 scheduler 的冻结身份入口**

  ```rust
  pub(crate) async fn acquire_planned(
      &self,
      disk_numbers: &[u32],
      kind: LocalDiskKind,
      class: DiskReadClass,
  ) -> Result<DiskReadPermit, SchedulerError>;
  ```

  `acquire(StorageLocation, class)` 只转发到 `acquire_planned`；复合 key、上限、FIFO 和许可 Drop 继续由同一 actor 处理。

- [ ] **Step 4: 删除逐文件存储发现和可变 location 缓存**

  `ScheduledFileReader` 保存 `Arc<ScanDiskPlan>`；`acquire_scheduled_permit` 按 `scanned.normalized_path` 纯内存查找 `PlannedDisk`。移除生产 `LocationResolver::System`、`locations: BTreeMap<PathBuf, String>` 和 Hash 后 `take_physical_disk_id` 依赖，Worker 显示身份直接来自计划。

  ```rust
  pub(crate) fn new(
      read_config: &DiskReadConfig,
      effective_worker_count: usize,
      disk_plan: Arc<ScanDiskPlan>,
  ) -> Result<(Self, PipelineLimits), ScanError>;
  ```

- [ ] **Step 5: 运行读取与遥测回归并提交**

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline scheduled_reader --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --test scan_runtime_details --locked -- --test-threads=1`

  Expected: PASS；Hash/Media 使用相同计划身份，复合盘真实 permit 同时占用全部底层盘。

  ```powershell
  git add crates/node-engine/src/io/scheduler.rs crates/node-engine/src/scan/pipeline.rs crates/node-engine/src/scan/mod.rs crates/node-engine/tests/base_compute_pipeline.rs crates/node-engine/tests/scan_runtime_details.rs
  git commit -m "refactor: reuse frozen disk plan for file reads"
  ```

---

### Task 6: 接入动态 Hash Ready Queue 和指定项领取

**Files:**
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`

**Interfaces:**
- Consumes: Task 2 的 `PlannedScannedPath`，Task 3 的 `WeightedDiskDispatcher`，Task 4 的 `claim_item`，Task 5 的计划化 reader。
- Produces: `HashReadyItem` 发布与 `try_dispatch_one_ready` 的 Hash 分支；下游 `HashedBaseItem` 持续携带同一 `Arc<PlannedDisk>`。

- [ ] **Step 1: 写缓存偏斜后的真实 Hash 5:1 RED 测试**

  构造 SSD 30 项、HDD 6 项；path cache 让输入前部命中比例相反，确保真正 miss 的 `item_id` 不按 SQLite ID 聚集。记录 reader 首 6 次开始读取的物理盘：

  ```rust
  #[tokio::test]
  async fn hash_ready_queue_uses_disk_weight_after_path_cache_skew() {
      let observation = run_weighted_base_fixture(weighted_fixture(5, 1, 6)).await;

      assert_eq!(observation.first_hash_disks, ["PhysicalDisk1", "PhysicalDisk1",
          "PhysicalDisk1", "PhysicalDisk1", "PhysicalDisk1", "PhysicalDisk0"]);
      assert_eq!(observation.duplicate_claims, 0);
      assert_eq!(observation.admission_active_at_end, 0);
  }
  ```

- [ ] **Step 2: 运行 RED 测试**

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline hash_ready_queue_uses_disk_weight_after_path_cache_skew --locked -- --test-threads=1`

  Expected: FAIL；旧 `claim_next_item` 仍按 `item_id` 领取。

- [ ] **Step 3: 在 SQLite 成功 queued 后发布 Hash Ready**

  `ReservedBase` 和所有 path context 改为携带 `PlannedScannedPath`。`apply_path_context` 的 miss 分支严格执行：先 `queue_scan_item_for_read`，再 `dispatcher.push_hash`，最后 `refill.on_upstream_item_published()`；任一步失败都不能留下只存在于内存的 item。

  ```rust
  struct HashReadyItem {
      item_id: String,
      planned: PlannedScannedPath,
  }
  ```

- [ ] **Step 4: 用统一选择函数启动 Hash**

  把 `try_start_one_hash_task` 改为接收 `HashReadyItem` 和 `DiskAdmissionLease`。先判断 hash slot、output credit、refill token，再调用 dispatcher；选择后使用 `claim_item(task_id, item_id, "read_md5", now_ms)`。

  ```rust
  fn start_selected_hash<F: PipelineFileReader>(
      selection: DispatchSelection<HashReadyItem, BaseComputeJob>,
      store: &BaseStoreHandle,
      reader: &F,
      /* existing ownership arguments */
  ) -> Result<HashStartResult, ScanError>;
  ```

  `Inactive` 丢弃该 Ready 项并释放 admission；`Mismatch` 返回任务级错误；成功后 future 同时拥有 admission，Hash 读完或错误/取消/Drop 时释放。

- [ ] **Step 5: 保持 refill、output credit 和身份守恒**

  用既有 `HashRefillController` 记录真实 publish/started/departure，不根据 Ready Queue 长度猜测 token。读取结果使用计划的 `display_id`，不再调用 reader 的 mutable location side channel。

- [ ] **Step 6: 运行 BaseCompute 回归并提交**

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --test base_compute_utilization --locked -- --test-threads=1`

  Expected: PASS；缓存命中、读取失败、取消、output credit 满和 Hash future Drop 均无重复领取或 admission 泄漏。

  ```powershell
  git add crates/node-engine/src/scan/base_compute.rs crates/node-engine/src/scan/base_persistence.rs crates/node-engine/tests/base_compute_pipeline.rs crates/node-engine/tests/base_compute_utilization.rs
  git commit -m "feat: dispatch hash work from disk ready queues"
  ```

---

### Task 7: 接入动态 Media Ready Queue 并消除 Worker 窗口头阻塞

**Files:**
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/worker_pipeline.rs`

**Interfaces:**
- Consumes: Task 3 的 dispatcher/admission、Task 6 已接入的统一 Hash 选择循环、现有 decode/output/Worker ownership。
- Produces: dispatcher 的 Media 分支、`AdmissionBoundMediaPermit`，以及在 `BaseSourceReadComplete` 释放真实 permit 与 admission 的唯一生命周期。

- [ ] **Step 1: 写 HDD 不得占满 Worker admission 的 RED 测试**

  ```rust
  #[tokio::test]
  async fn hdd_one_does_not_fill_all_media_acquire_slots_ahead_of_ready_ssd() {
      let observation = run_media_dispatch_fixture(
          media_fixture().hdd_limit(1).ssd_limit(5).total(6).workers(6),
      )
      .await;

      assert_eq!(observation.first_worker_disks, ["PhysicalDisk1", "PhysicalDisk1",
          "PhysicalDisk1", "PhysicalDisk1", "PhysicalDisk1", "PhysicalDisk0"]);
      assert!(observation.max_active_for("PhysicalDisk0") <= 1);
  }
  ```

  增加 Worker slot 已满、媒体 permit future 取消、许可失败、Worker 崩溃前后和 `BaseSourceReadComplete` 重复事件测试。

- [ ] **Step 2: 运行 RED 测试**

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline hdd_one_does_not_fill_all_media_acquire_slots_ahead_of_ready_ssd --locked -- --test-threads=1`

  Expected: FAIL；旧 `pending_compute.pop_front()` 会让同盘 future 占用窗口。

- [ ] **Step 3: 把 `pending_compute` 改为 dispatcher Media lane**

  `ResolvedBaseItem::Compute` 成功获得 decode credit 后调用 `dispatcher.push_media(job.disk.clone(), job)`；不再维护全局 FIFO `VecDeque<BaseComputeJob>`。`decode_queue_owned` 改为统计 dispatcher media ready、media acquiring、pending dispatch 和尚未 Started 的 active 总和。

- [ ] **Step 4: 先验证非磁盘资源，再选择并取得 admission**

  主循环每个 epoch 计算：

  ```rust
  let readiness = DispatchReadiness {
      hash: hash_slot_available && output_credit_available && refill.can_attempt_claim(),
      media: worker_admission_available && decode_ownership_valid,
  };
  ```

  只有 readiness 为 true 的类别参加 lane 选择。媒体选择后才 spawn `acquire_media_permit`；因此 HDD=1 的一个等待 future 不会阻止另一盘候选进入 Worker 窗口。

- [ ] **Step 5: 将 admission 与真实媒体许可绑定**

  ```rust
  struct AdmissionBoundMediaPermit<L> {
      scheduled: Option<L>,
      admission: Option<DiskAdmissionLease>,
  }

  impl<L> Drop for AdmissionBoundMediaPermit<L> {
      fn drop(&mut self) {
          drop(self.scheduled.take());
          drop(self.admission.take());
      }
  }
  ```

  取得真实 permit 前 future 持有 admission；取得后把两者一起擦除为 `ErasedMediaPermit`。`BaseSourceReadComplete`、Worker 终态、崩溃、取消和 ActiveBase Drop 都复用现有幂等 `release_media_permit()`，恰好释放一次。

- [ ] **Step 6: 运行 Worker/BaseCompute 回归并提交**

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1`

  Expected: PASS；Worker、decode、persist、真实 permit、admission 的峰值不超容量，终态全部归零。

  ```powershell
  git add crates/node-engine/src/scan/base_compute.rs crates/node-engine/tests/base_compute_pipeline.rs crates/node-engine/tests/worker_pipeline.rs
  git commit -m "feat: dispatch media work by physical disk"
  ```

---

### Task 8: 从 SQLite 阶段边界恢复 Ready Queue 并自动续跑

**Files:**
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Test: `crates/node-engine/tests/base_compute_pipeline.rs`
- Test: `crates/node-engine/tests/runtime_tasks.rs`
- Test: `crates/node-store/tests/task_recovery.rs`

**Interfaces:**
- Consumes: Task 4 的 `scan_task_roots`/`queued_scan_items`，Task 1 的根计划，Task 6/7 的 Ready Queue。
- Produces: `BaseComputeSeed::Enumerated`、`BaseComputeSeed::Recovered`、`RecoveredScanJob` 和 actor 的顺序恢复队列。

- [ ] **Step 1: 写混合 `read_md5`/`base_compute` 恢复 RED 测试**

  ```rust
  #[tokio::test]
  async fn restart_rebuilds_hash_and_media_lanes_without_reenumeration() {
      let fixture = interrupted_scan_fixture()
          .queued_read_md5(r"H:\media\a.bin")
          .queued_base_compute(r"I:\media\b.mp4")
          .succeeded(r"I:\media\done.jpg");
      let observation = fixture.restart_node().await;

      assert_eq!(observation.enumerator_calls, 0);
      assert_eq!(observation.claimed_once, 2);
      assert_eq!(observation.final_task_status, TaskStatus::Completed);
      assert_eq!(observation.persisted_item_count, 3);
  }
  ```

- [ ] **Step 2: 运行 RED 测试**

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline restart_rebuilds_hash_and_media_lanes_without_reenumeration --locked -- --test-threads=1`

  Expected: FAIL；当前启动只发布恢复详情，不继续计算。

- [ ] **Step 3: 给 BaseCompute 增加明确恢复种子**

  ```rust
  enum BaseComputeSeed {
      Enumerated(Vec<PlannedScannedPath>),
      Recovered(Vec<QueuedScanItem>),
  }
  ```

  `read_md5` 项按最具体根归属后进入 Hash Ready；`base_compute` 项必须具有 `content_id`，通过 `load_base_cache_record` 取得 `ContentKey`/MD5 和当前缺失掩码后进入 Media Ready。其他 stage 返回任务级 `InvalidState`，不猜测阶段。

- [ ] **Step 4: 在 actor 启动后顺序续跑恢复扫描**

  `publish_recovery_runtime_tasks` 返回 `(TaskSnapshot, RuntimeTaskReporter)`；`EngineState` 保存 `VecDeque<RecoveredScanJob>`。actor 启动时若无 active job，启动第一项；`BackgroundFinished` 归还 WorkerPool 后立即启动下一项，始终只保留一个后台媒体任务。

  ```rust
  struct RecoveredScanJob {
      task_id: TaskId,
      roots: Vec<DisplayPath>,
      items: Vec<QueuedScanItem>,
      reporter: RuntimeTaskReporter,
  }
  ```

  根计划使用当前启动配置重新生成；deficit、cursor 和 bypass 归零；已成功/失败/取消项不重新加入。

- [ ] **Step 5: 验证取消、根失败和多任务顺序恢复**

  增加三个行为测试：启动前取消项不 claim；恢复根解析失败只 fail 当前任务并继续下一个；两个恢复任务不并发共享 WorkerPool。收尾继续调用 `finalize_scan_task_from_items`，不重新枚举或误失效路径。

- [ ] **Step 6: 运行恢复回归并提交**

  Run: `cargo test -p dedup-node-store --test task_recovery --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline restart_ --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --test runtime_tasks --locked -- --test-threads=1`

  Expected: PASS；重启后无丢项、重复 claim、重复最终化或事件序号倒退。

  ```powershell
  git add crates/node-engine/src/actor.rs crates/node-engine/src/scan/base_compute.rs crates/node-engine/src/scan/base_persistence.rs crates/node-engine/src/runtime_tasks.rs crates/node-engine/tests/base_compute_pipeline.rs crates/node-engine/tests/runtime_tasks.rs crates/node-store/tests/task_recovery.rs
  git commit -m "feat: resume disk ready queues after node restart"
  ```

---

### Task 9: 投影任务分发层逐盘遥测

**Files:**
- Modify: `proto/node.proto`
- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-engine/src/scan/disk_dispatch.rs`
- Test: `crates/protocol/tests/runtime_tasks_wire.rs`
- Test: `crates/node-engine/tests/runtime_tasks.rs`
- Test: `crates/node-engine/tests/scan_runtime_details.rs`

**Interfaces:**
- Consumes: Task 3 的 lane 状态和 admission 原因，保留现有 `RuntimeDiskReadMetrics disk_reads=28`。
- Produces: 新 `RuntimeDiskDispatchMetrics` 和 `RuntimePipelineMetrics.disk_dispatch=29`；`RuntimeTaskReporter` 提供注册、Ready、选择、等待、bypass 和 aged 更新。

- [ ] **Step 1: 写协议描述符和缺失字段 RED 测试**

  在 `runtime_tasks_wire.rs` 断言新 message 字段号和旧字节流兼容：

  ```rust
  let dispatch = message(messages, "RuntimeDiskDispatchMetrics").unwrap();
  assert_field_numbers(dispatch, &[
      ("physical_disk_id", 1), ("configured_weight", 2), ("configured_limit", 3),
      ("hash_ready_current", 4), ("hash_ready_peak", 5),
      ("media_ready_current", 6), ("media_ready_peak", 7),
      ("dispatch_selected_total", 8), ("global_limit_wait_total", 9),
      ("disk_limit_wait_total", 10), ("worker_slot_wait_total", 11),
      ("bypass_total", 12), ("aged_forced_total", 13),
  ]);
  assert_eq!(field_number(pipeline, "disk_dispatch"), 29);
  ```

- [ ] **Step 2: 运行协议 RED 测试**

  Run: `cargo test -p dedup-protocol --test runtime_tasks_wire --locked -- --test-threads=1`

  Expected: FAIL，新 message 尚不存在。

- [ ] **Step 3: 只追加协议字段**

  ```protobuf
  message RuntimeDiskDispatchMetrics {
    string physical_disk_id = 1;
    optional uint64 configured_weight = 2;
    optional uint64 configured_limit = 3;
    optional uint64 hash_ready_current = 4;
    optional uint64 hash_ready_peak = 5;
    optional uint64 media_ready_current = 6;
    optional uint64 media_ready_peak = 7;
    optional uint64 dispatch_selected_total = 8;
    optional uint64 global_limit_wait_total = 9;
    optional uint64 disk_limit_wait_total = 10;
    optional uint64 worker_slot_wait_total = 11;
    optional uint64 bypass_total = 12;
    optional uint64 aged_forced_total = 13;
  }

  message RuntimePipelineMetrics {
    // 保留 1..28 原字段。
    repeated RuntimeDiskDispatchMetrics disk_dispatch = 29;
  }
  ```

- [ ] **Step 4: 在 registry 中原子维护 lane 快照**

  `PipelineMetricsEntry` 新增 `disk_dispatch: BTreeMap<String, DiskDispatchMetricsEntry>`。注册 lane 时固定 weight/limit；push/pop 更新 current/peak；选择和阻塞只做饱和累加；复合 lane 使用 `PhysicalDisk5+12` 作为一条分发记录，真实底层 permit 仍出现在既有 `disk_reads`。

  ```rust
  pub(crate) enum RuntimeDiskDispatchWait {
      GlobalLimit,
      DiskLimit,
      WorkerSlot,
  }

  pub fn configure_disk_lane_nowait(
      &self,
      physical_disk_id: &str,
      weight: u64,
      limit: u64,
  ) -> Result<(), RuntimeTaskError>;
  ```

- [ ] **Step 5: 由 dispatcher 的真实边界更新遥测**

  lane push 后更新 Ready；成功选择后减少 Ready 并增加 selected；每个选择 epoch 对持续 Ready 但受阻的 lane 只增加一次对应 wait；达到共享老化阈值时增加 bypass/aged。遥测失败作为任务级基础设施错误，不带着半快照继续派发。

- [ ] **Step 6: 运行协议与 registry 回归并提交**

  Run: `cargo test -p dedup-protocol --test runtime_tasks_wire --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --test runtime_tasks --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --test scan_runtime_details --locked -- --test-threads=1`

  Expected: PASS；旧 wire 缺少 tag 29 时解码为空，新累计计数单调且终态 Ready/active 为零。

  ```powershell
  git add proto/node.proto crates/node-engine/src/runtime_tasks.rs crates/node-engine/src/scan/disk_dispatch.rs crates/protocol/tests/runtime_tasks_wire.rs crates/node-engine/tests/runtime_tasks.rs crates/node-engine/tests/scan_runtime_details.rs
  git commit -m "feat: expose weighted disk dispatch telemetry"
  ```

---

### Task 10: 扩展 acceptance 客户端与权重报告

**Files:**
- Modify: `crates/desktop-core/examples/runtime_acceptance.rs`
- Modify: `crates/desktop-core/tests/runtime_acceptance_contract.rs`
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`

**Interfaces:**
- Consumes: Task 9 的 `disk_dispatch` runtime 样本、现有 1 秒任务快照和 2 秒系统样本。
- Produces: NDJSON `pipeline_metrics.disk_dispatch`、报告“有效双盘窗口”表、守恒/权重结论和 INCONCLUSIVE 规则。

- [ ] **Step 1: 保存重叠脏文件差异并写报告 RED fixture**

  Run: `git diff --output=C:\tmp\rust-v2-weighted-dispatch-overlap-before.diff -- crates/desktop-core/examples/runtime_acceptance.rs crates/desktop-core/tests/runtime_acceptance_contract.rs tests/windows/Measure-RustV2RuntimeAcceptance.ps1 tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`

  在 PowerShell fixture 中生成 8 个 1 秒样本：两条 lane 均 Ready 的 6 个有效样本 selected 增量为 5:1；另两个样本分别增加 global wait 和 worker wait，必须排除。

  ```powershell
  $valid = @($report.disk_weight_windows | Where-Object eligible)
  Assert-Equal $valid.Count 6 '仅保留双盘同时 Ready 且无阻塞增量的窗口'
  Assert-Equal $report.disk_weight_totals.PhysicalDisk0 1 'HDD 选择数'
  Assert-Equal $report.disk_weight_totals.PhysicalDisk1 5 'SSD 选择数'
  ```

- [ ] **Step 2: 运行报告 RED 测试**

  Run: `pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1`

  Expected: FAIL，旧报告未读取 `disk_dispatch`。

- [ ] **Step 3: 将新协议字段原样写入 runtime NDJSON**

  `runtime_acceptance.rs` 仅映射新 message，不推导权重结论；保持现有 `sample_interval_ms`、时间戳和旧指标。契约测试断言字段名、整数、缺失值和 1 秒采样。

  ```json
  {
    "physical_disk_id": "PhysicalDisk1",
    "configured_weight": 5,
    "configured_limit": 5,
    "hash_ready_current": 12,
    "media_ready_current": 4,
    "dispatch_selected_total": 30,
    "global_limit_wait_total": 0,
    "disk_limit_wait_total": 0,
    "worker_slot_wait_total": 0
  }
  ```

- [ ] **Step 4: 用累计差分判定有效权重窗口**

  `New-RustV2RuntimeAcceptanceReport.ps1` 按相邻任务样本和实际 `sample_interval_ms` 计算：至少两条 lane 在窗口两端 `hash_ready_current + media_ready_current > 0`；三类 wait 总量均无增加；selected 总量有增加。其余窗口标记原因并排除，不删除原始行。

  权重门禁对有效窗口累计 selected 与配置权重比较；单位任务累计误差大于 1 时 FAIL；有效样本覆盖不足 95% 或没有有效双盘窗口时 INCONCLUSIVE，不伪造 PASS。

- [ ] **Step 5: 增加守恒和终态检查**

  报告断言 Ready current 不超过 peak、selected/wait/bypass/aged 单调、终态 Ready 为零；既有 `disk_reads` 继续断言 released 不超过 granted、active 不超过 capacity、终态 active 为零。媒体耗尽后的单盘窗口只报告，不进入比例失败。

- [ ] **Step 6: 运行客户端、报告、harness 回归并提交**

  Run: `cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1`

  Run: `pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1`

  Run: `pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1`

  Expected: 全部 PASS；检查 `git diff --check` 后确认原脏改动仍在增量 diff 中，没有被覆盖。

  先完整暂存原本干净的报告文件；四个重叠脏文件只交互暂存本任务新增 hunk，并用 cached diff 对照 Step 1 保存的基线，禁止把旧 hunk 混入提交。

  ```powershell
  git add tests/windows/New-RustV2RuntimeAcceptanceReport.ps1 tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1
  git add -p crates/desktop-core/examples/runtime_acceptance.rs crates/desktop-core/tests/runtime_acceptance_contract.rs tests/windows/Measure-RustV2RuntimeAcceptance.ps1 tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1
  git diff --cached --check
  git diff --cached -- crates/desktop-core/examples/runtime_acceptance.rs crates/desktop-core/tests/runtime_acceptance_contract.rs tests/windows/Measure-RustV2RuntimeAcceptance.ps1 tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1
  git commit -m "test: report weighted physical disk dispatch"
  ```

---

### Task 11: 完成定向回归、正式包验证和一次双物理盘验收

**Files:**
- Create: `docs/verification/2026-08-27-physical-disk-weighted-dispatch-acceptance.md`
- Test: `crates/node-engine/tests/base_compute_pipeline.rs`
- Test: `tests/windows/Test-RustV2Package.ps1`

**Interfaces:**
- Consumes: Tasks 1–10 的实现和遥测，不引入新产品接口。
- Produces: 可复核的定向测试记录、正式包 SHA256、一次 H/I 真实媒体运行证据和最终裁决；不部署。

- [ ] **Step 1: 运行格式与定向 Rust 回归**

  Run: `cargo fmt --all -- --check`

  Run: `cargo test -p dedup-windows --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-store --test task_recovery --locked -- --test-threads=1`

  Run: `cargo test -p dedup-protocol --test runtime_tasks_wire --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1`

  Run: `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1`

  Expected: 全部 exit 0；若磁盘空间低于 10 GiB，只删除项目路径下已确认可再生的 Cargo target/cache，记录删除路径和前后空间后继续原命令。

- [ ] **Step 2: 运行桌面协议与 Windows 报告回归**

  Run: `cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1`

  Run: `pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1`

  Run: `pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1`

  Run: `pwsh -NoProfile -File tests\windows\Test-RustV2Package.ps1`

  Expected: 全部 exit 0。

- [ ] **Step 3: 构建并独立验证正式包，不部署**

  Run: `pwsh -NoProfile -File scripts\build-release.ps1 -CargoTargetDir C:\tmp\rust-v2-weighted-dispatch-target`

  Run: `pwsh -NoProfile -File scripts\verify-release.ps1 -Package dist-rust-v2\mySingerServer-rust-v2-win-x64.zip`

  Expected: `RUST_V2_RELEASE_BUILD_PASS` 和 `PACKAGE_PASS`；记录 ZIP、manifest、node.exe、worker.exe SHA256。正式 ZIP 仍只含 desktop/node/worker/Everything 四个顶层 EXE，不加入 acceptance 客户端。

- [ ] **Step 4: 运行一次完整双物理盘真实媒体任务**

  不传 `EvidenceRoot`，由 Measure 脚本在 `C:\tmp\rust-v2-runtime-acceptance` 下创建本次独立 GUID 证据根；媒体根固定为 `H:\pik\00000000000` 和 `I:\tmp`。配置 Worker 数 20、`read.total_threads=12`，HDD/SSD/Unknown 逐盘数值读取本次测试配置；枚举器保持默认 Everything，只有日志明确记录 IPC/数据库不可用时才允许整次回退 Walker。

  ```powershell
  pwsh -NoProfile -File tests\windows\Measure-RustV2RuntimeAcceptance.ps1 `
    -MediaRoots @('H:\pik\00000000000','I:\tmp') `
    -DurationSeconds 1800 -SampleSeconds 2 `
    -ReleaseRoot (Resolve-Path 'dist-rust-v2\staging').Path `
    -AcceptanceClientPath 'C:\tmp\rust-v2-acceptance-tools\runtime_acceptance.exe' `
    -WorkerCount 20 -TotalReadThreads 12 `
    -HddThreadsPerDisk 1 -SsdThreadsPerDisk 5 -UnknownThreadsPerDisk 1 `
    -Enumerator everything -SingleRun -RequireDistinctPhysicalDisks
  ```

  harness 在扫描任务进入 `completed` 后立即结束，不强制等待满 1800 秒；只运行这一轮，不启动 A/B 六轮或 A-3。运行前后媒体清单必须一致。

- [ ] **Step 5: 写最终验证文档**

  `docs/verification/2026-08-27-physical-disk-weighted-dispatch-acceptance.md` 记录：源码修订、包/EXE SHA、配置、根计划物理盘编号与类型、Everything 状态、任务终态、每盘 configured/ready/selected/wait/bypass/aged、真实 permit、CPU/磁盘 I/O、有效权重窗口比例、媒体清单 SHA 和证据根。

  结论规则固定：任务 completed、媒体不变、守恒通过且有效窗口权重误差不超过 1 才 PASS；证据覆盖不足或双盘没有同时 Ready 窗口为 INCONCLUSIVE；任务失败、计数越界或媒体变化为 FAIL。

- [ ] **Step 6: 提交验证文档并确认部署边界**

  Run: `git diff --check`

  Run: `git status --short`

  Expected: 仅验证文档和用户原有脏文件可见；没有 `I:\Tool` 变更。

  ```powershell
  git add docs/verification/2026-08-27-physical-disk-weighted-dispatch-acceptance.md
  git commit -m "docs: verify weighted disk task dispatch"
  ```

---

## Final Review Gate

- [ ] 对照 spec 的 14 个章节逐项确认：枚举前根规划、同盘合并、Unknown/复合盘、path-cache 供给、Hash Ready、Media Ready、WDRR、全局/逐盘/Worker 门禁、老化、admission/permit RAII、指定项 claim、重启恢复、遥测和一次真实验收均已有对应测试与证据。
- [ ] 搜索计划和实现中是否出现独立 SSD=5/HDD=1 常量；5/1 只能存在于测试 fixture 和示例配置，生产代码只读取 `DiskReadConfig`。
- [ ] 搜索 `claim_next_item` 的 BaseCompute 调用；基础扫描 Hash 路径必须只使用 `claim_item`，其他任务类型可保留原接口。
- [ ] 搜索 `resolve_storage_location` 的扫描文件调用；生产基础扫描只能在 `ScanDiskPlan` 根规划中调用，不得逐媒体文件调用。
- [ ] 检查协议 tag 1..28、SQLite schema 3、正式包四 EXE 白名单和用户原脏文件均未被改写。
- [ ] 最终代码审查使用 `gpt-5.6-sol`、`max` reasoning，仅审查本计划提交范围和测试证据；不扩展到 Worker 崩溃、FFmpeg、SSD 识别或部署。
