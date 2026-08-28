### Task 5：在文件枚举前冻结扫描根与物理盘 lane

**目标：** Node 在第一次调用 Everything 或 Windows Walker 枚举文件前，一次解析本轮全部扫描根对应的物理盘编号、介质类型和配置额度。枚举结果只按路径组件归属到已冻结 lane；Hash 与 Media 读取始终消费同一份 lane，不再按文件路径重新查询 Windows 存储身份。

本任务只建立稳定的根计划和读取边界，不创建 TSV、不改变现有全局/逐盘许可算法，也不提前实现加权轮转。后续 Task 6 的唯一 dispatcher 将消费这里冻结的配置权重和物理盘集合。

**Files:**

- Create: `crates/node-engine/src/scan/root_plan.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify only if the existing public boundary is insufficient: `crates/windows/src/storage_device.rs`
- Modify only if a new public type must be exported: `crates/windows/src/lib.rs`
- Create/Modify: `crates/node-engine/tests/scan_roots.rs`
- Modify only for missing Windows behavior coverage: `crates/windows/tests/storage_device.rs`
- Preserve: `crates/node-engine/tests/enumerators.rs`
- Preserve: `crates/node-engine/tests/disk_scheduler.rs`

**建议接口：**

```rust
/// 一个扫描根在枚举前解析出的稳定存储位置。
pub struct ResolvedScanRootStorage {
    /// 已规范化的扫描根。
    pub normalized_root: NormalizedPath,
    /// 一个或多个底层物理盘组成的稳定身份。
    pub physical_disk_id: PhysicalDiskId,
    /// Windows 边界保守判断出的介质类型。
    pub disk_kind: LocalDiskKind,
}

/// 只在建立扫描根计划时调用的存储位置解析器。
pub trait ScanRootStorageResolver: Send + Sync {
    fn resolve(&self, root: &Path) -> io::Result<ResolvedScanRootStorage>;
}

/// 本轮任务项使用的冻结物理盘 lane。
pub struct TaskDiskLane {
    pub physical_disk_id: PhysicalDiskId,
    pub physical_disk_numbers: Vec<u32>,
    pub disk_kind: LocalDiskKind,
    pub configured_weight: usize,
    pub per_disk_limit: usize,
}

/// 已枚举路径及其唯一冻结 lane。
pub struct PlannedScannedPath {
    pub scanned: ScannedPath,
    pub lane: TaskDiskLane,
}

/// 扫描开始时一次建立、随后只读的根与 lane 计划。
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

实际类型名可按现有模块做最小调整，但必须保留“根解析只发生一次、枚举行携带冻结 lane、读取期不再解析”的所有权。

**绑定约束：**

- `run_background_job` 必须在任何生产 enumerator 的首次 `enumerate` 调用前完成全部根解析。允许先选择 Everything/Walker 实现，但不能先枚举再补计划。
- 复用 `dedup-windows::resolve_storage_location` 已有卷 extent 与 seek-penalty 判断。解析失败返回包含显示根的稳定错误 `SCAN_ROOT_STORAGE_RESOLVE_FAILED`，不得静默降级到 Unknown，也不得调用 enumerator。
- 对扫描根先规范化、排序和去重。枚举行归属使用 `NormalizedPath::is_within` 的路径组件语义；多根命中选择组件最深者，同深度按规范路径稳定排序。`D:\A` 不得匹配 `D:\AB`。
- 相同 `PhysicalDiskId` 的多个根合并为同一 lane。物理盘编号保持排序去重；复合卷如 `[5,12]` 必须保留完整集合和稳定显示 `PhysicalDisk5+12`，后续调度才能原子占用所有底层盘。
- 保留 Windows 解析边界给出的保守介质类型，不得用 `PhysicalDiskId::is_composite()` 重新推导。相同 lane 观察到不同类型时降为 `Unknown`，额度取 `unknown_threads_per_disk` 与已观察类型额度的最小值。
- `configured_weight` 和 `per_disk_limit` 都只从本轮冻结的 `DiskReadConfig` 中按 HDD/SSD/Unknown 类型取得；全局额度仍只来自 `total_threads`，不复制进 lane。
- `EverythingEnumerator` 与 `WindowsWalker` 继续只负责全局规范路径排序、去重和文件清单，不能读取磁盘配置或自行选择 lane。
- `ScheduledFileReader` 的 Hash 与 Media 两条许可路径都必须直接消费 `PlannedScannedPath.lane`；删除读取期 `LocationResolver`、路径位置缓存和 `take_physical_disk_id` 事实。不得只改 Hash 而遗漏 Media。
- 本任务不实现跨盘加权选择。当前 `DiskReadScheduler` 的全局、逐盘、复合盘原子许可和老化保护保持唯一权威；Task 6 再在唯一 dispatcher 中按冻结权重轮转，不能出现两套调度状态。
- 不改 Protobuf、SQLite schema、缓存分类、运行任务模型、TSV 格式或 UI。

- [ ] **Step 1：写枚举前解析 RED**

  用受控 resolver 和 enumerator 记录真实调用顺序。H/I 两根必须先得到 `resolve:H, resolve:I`，之后才出现 `enumerate`；根解析失败时 enumerator 调用数必须为 0。先在当前实现上保存失败证据。

- [ ] **Step 2：写 lane 归属与配置 RED**

  覆盖同盘两根合并、不同盘两 lane、复合 `[5,12]`、Unknown、混合类型降级、重复根、嵌套根、`D:\A`/`D:\AB` 和不属于任一根的枚举行。验证 HDD/SSD/Unknown 使用本次配置值，而不是硬编码示例数值。

- [ ] **Step 3：写 Hash/Media 不再解析 RED**

  受控 resolver 只允许在根计划阶段调用。完成枚举后让同一个 `PlannedScannedPath` 经过真实 Hash 与 Media 许可边界，解析调用次数不得增加，两阶段观察到的物理盘集合和介质类型必须完全相同。

- [ ] **Step 4：实现最小根计划并接入 actor**

  新增 `root_plan.rs`，在扫描后台作业首次枚举前构建只读计划；枚举完成后为每行附加唯一 lane。根解析错误直接结束本次扫描，并保留既有旧路径，不进入扫描收尾。

- [ ] **Step 5：让读取器只消费冻结 lane**

  移除每次读取的 Windows 位置解析和可变路径缓存；Hash 与 Media 统一把 lane 中完整的 `PhysicalDiskId`、介质类型和逐盘额度交给唯一 `DiskReadScheduler`。

- [ ] **Step 6：运行 GREEN 和回归**

  ```powershell
  cargo test -p dedup-windows --test storage_device --locked -- --test-threads=1
  cargo test -p dedup-node-engine --features test-hooks --test scan_roots --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test enumerators --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1
  cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
  cargo fmt --all -- --check
  git diff --check
  ```

  所有 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，关闭 incremental/debug info并清除继承的 MinGW 编译变量。每次重型测试前检查 C、D 可用空间；任一低于 10 GiB 时只清理本计划精确确认的可再生 target，然后继续。

- [ ] **Step 7：提交、报告与独立审查**

  提交产品代码、真实行为测试和中文 `task-5-report.md`。独立审查必须核对首次枚举前解析、组件边界、复合盘、Hash/Media 同 lane，以及未提前实现 TSV/加权 dispatcher。不得运行真实媒体、打包、部署或触碰 `I:\Tool`。
