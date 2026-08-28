### Task 6：瞬态 TSV 任务文件与唯一 dispatcher 基础设施

**目标：** 为基础计算和后续二筛建立共用的按物理盘 TSV 任务文件、原位 `P/C/F` 状态机、有限预读和唯一磁盘 dispatcher。任务文件只保存真正需要计算的项，不使用 JSON、不生成 `.idx`；SQLite ACK 成功前状态必须保持 `P`。

本任务只提供基础设施和真实行为门禁，不把当前 `BaseCompute`、本地分析或二筛生产路径切换为 TSV 来源；真实接入在后续 Task 7/8 完成。

为减少共享状态和审查风险，按两个顺序提交实施：

1. **Task 6A：** TSV 单所有者、固定格式、可见边界、有限预读、原位状态和身份校验。
2. **Task 6B：** 每 lane 单队首 dispatcher、配置权重和老化接入唯一 `DiskReadScheduler`。

**Files:**

- Create: `crates/node-engine/src/task_files.rs`
- Create: `crates/node-engine/src/task_dispatch.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Modify: `crates/node-engine/src/io/scheduler.rs`
- Modify only if an existing scheduler type must be exported: `crates/node-engine/src/io/mod.rs`
- Create: `crates/node-engine/tests/transient_task_files.rs`
- Create: `crates/node-engine/tests/task_dispatch.rs`
- Modify: `crates/node-engine/tests/disk_scheduler.rs`
- Preserve: `crates/node-engine/src/actor.rs`
- Preserve: `crates/node-engine/src/scan/base_compute.rs`
- Preserve: `crates/node-engine/src/scan/pipeline.rs`

## Task 6A：TSV 单所有者与状态机

**固定类型与位布局：**

```rust
/// TSV 行首的唯一运行状态。
pub enum TaskLineStatus { Pending, Completed, Failed }

/// 一行任务需要进入的计算入口。
pub enum TaskWorkKind { Base, ImageStage2, VideoStage2 }

/// TSV 固定 64 位缺失掩码。
pub struct TaskWorkMask(u64);

impl TaskWorkMask {
    // bits 0..=2：原样保存既有 BASE_MISSING_* 三位。
    // bit 3：需要先计算 MD5。
    // bit 4：缺少图片二筛。
    // bits 5..=10：缺少视频二筛槽位 0..=5。
    pub const fn needs_md5(self) -> bool;
    pub const fn base_missing_parts(self) -> u32;
    pub const fn image_stage2_missing(self) -> bool;
    pub const fn video_stage2_slots(self) -> u8;
}

/// 一个真正需要计算的 TSV 项。
pub struct TaskFileRecord {
    pub item_id: Uuid,
    pub work_kind: TaskWorkKind,
    pub scanned: ScannedPath,
    pub known_md5: Option<[u8; 16]>,
    pub missing: TaskWorkMask,
}

/// 结果提交时必须原样回传的文件身份。
pub struct TaskFileIdentity {
    // run id、item id、lane、line offset、line length 和 missing mask 均不可由调用者改写。
}

/// 隐藏全部文件句柄并串行化访问的任务文件集合。
pub struct TransientTaskFileSet;
```

实际 API 可以用内部 `Arc<Mutex<State>> + Notify` 或等价短小封装，让生产者和 dispatcher 共享一个文件所有者；任何 `File`、`BufWriter`、读取游标或状态写句柄都不得逃逸。文件 IO 期间不得持有跨 `await` 的同步锁。

**固定 TSV：**

```text
状态\t任务项ID\t工作类型\t规范路径\t显示路径\t文件大小\t已知MD5\t缺失字段掩码\n
```

- UTF-8、无 BOM、严格 8 列、7 个 tab、LF 结尾。
- 状态只能为 ASCII `P/C/F`；工作类型只能为 `base/image_stage2/video_stage2`。
- item ID 和 run ID 必须是规范 UUID v7；文件大小为十进制 `u64`；MD5 为空或 32 位小写十六进制；掩码固定 16 位小写十六进制。
- 规范路径和显示路径禁止 tab、CR、LF；显示路径必须能无损转换为 UTF-8，禁止 `to_string_lossy()`。
- 空 item ID、重复 item ID、空缺失掩码、非法工作种类/掩码组合在追加前拒绝。完整缓存命中项不创建 `TaskFileRecord`。

**文件与目录：**

- 调用方传入真实 `ResolvedNodePaths.data_path.join("runtime")`；本任务不硬编码 `AppLayout` 或用户目录。
- 每次 run 只创建一个全新 `<runtime>/<run-id>/`，已存在目录拒绝复用。
- lane 文件名只由已验证 `TaskDiskLane` 构造：`PhysicalDisk7-hdd.tasks.tsv`、`PhysicalDisk5+12-unknown.tasks.tsv`。盘号排序去重，文件名不得包含用户路径。
- 提供精确的旧 run 清理 helper，但本任务不接入 `NodeRuntime::start_inner`；后续真实接入时只允许清理 `data_path/runtime/*`，不得登记为可再生产物或被磁盘满清理删除正在运行的任务源。

**可见边界和有限预读：**

- `append_batch` 先完整序列化/校验整批，再写 lane 私有 `BufWriter`；flush 成功后才推进 `published_len` 并通知 dispatcher。失败不得发布半行。
- `seal` flush 全部 lane，固定最终可见长度；seal 后 append 必须失败。
- 每 lane 只解析 `max(2, per_disk_limit * 2)` 个完整行对象到预读窗口；允许为重复 ID/状态维护最小 offset 元数据，但禁止把全部路径/记录反序列化为 `Vec<TaskFileRecord>`。
- 空且未 seal 表示等待生产者，不能返回完成；sealed 且无可派发/在途项才可结束。

**原位状态：**

- `take_lane(expected_identity)` 只移动读取/在途所有权，磁盘首字节仍为 `P`。
- `mark_completed` 仅在 SQLite 事务 ACK 成功后调用；单文件读取/Worker 失败调用 `mark_failed`。
- 写前 flush，并按 run、lane、offset、行长、item ID、mask 重读整行校验。Windows 使用绝对 offset 只写 1 个 ASCII 字节。
- 只允许 `P→C`、`P→F`；重复相同终态幂等；`C→F`、`F→C`、错误身份、损坏行和越过 published 边界必须失败。
- `all_terminal` 同时要求 producer sealed、所有行 `C/F`、没有 dispatcher/ACK 在途所有权。取消不是逐行 `F`；后续调用方收束 Worker/permit 后删除整个 run 目录。

## Task 6B：唯一 dispatcher 与配置权重

**接口：**

```rust
pub trait TaskLanePermitProvider: Clone + Send + Sync + 'static {
    type Permit: Send + 'static;
    fn acquire(
        &self,
        lane: TaskDiskLane,
        class: DiskReadClass,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<Self::Permit, ReadFailure>> + Send>>;
}

pub struct DispatchedTask<P> {
    pub identity: TaskFileIdentity,
    pub record: TaskFileRecord,
    pub class: DiskReadClass,
    pub permit: P,
}

pub struct TaskFileDispatcher<P: TaskLanePermitProvider>;
```

- `TaskFileDispatcher` 对每个当前有 `P` 队首的 lane 最多保留一个 permit future；permit 成功后才按 expected identity `take_lane`。取消或 provider 失败时该行保持 `P`。
- 映射只决定本次初始读取类别：`Base + needs_md5 → HashSequential`；`Base + known_md5`、图片/视频二筛 → `MediaDecode`。基础任务 MD5 后继续媒体阶段的同身份 reacquire 在 Task 7 接入时完成，本任务不伪造第二行。
- dispatcher 不保存 deficit、active counter、资源 lease 或第二套老化 reservation；返回的 `DispatchedTask` 已持有唯一 permit，后续读取器不得再次 acquire。
- scheduler provider 把完整物理盘集合、`per_disk_limit` 和 `configured_weight` 一次交给现有 `DiskReadScheduler`。

**加权和老化唯一权威：**

- lane 的权重只来自 Task 5 冻结的 `configured_weight`，当前值由 HDD/SSD/Unknown 配置额度得到；禁止硬编码 5:1，也不能按介质类型聚合多块物理盘。
- 在 `DiskReadScheduler` actor 内增加 canonical lane 的 weighted deficit/cursor；`TaskFileDispatcher` 不保存权重状态。
- 外层先按 lane 权重选可运行队首，内层继续复用现有 Hash/Media 3:1、容量 1、class pressure 和 FIFO 规则。
- 既有 `AgedReservation` 仍是唯一老化保护；低权重 lane 连续被冲突请求绕过达到现有上限后，必须获得下一个可运行冲突席位。自身逐盘阻塞时允许不相交盘继续。
- global/per-disk/composite active 只在既有 `reserve_all` 成功后增加；等待 head 不占全局席位，permit Drop 仍是唯一释放边界。
- 普通现有 `acquire/acquire_with_limit` 保持默认等权语义；新增带 lane weight 的入口只供 dispatcher provider，当前 BaseCompute 生产路径本任务不改。
- lane 空、sealed 耗尽或取消时清理其 deficit/cursor 状态，重新出现不能累计历史突发额度。

## TDD 与验证

- [ ] **Step 1：Task 6A 固定格式 RED**

  当前无模块时先得到真实编译 RED；随后覆盖无 BOM、7 tab、LF、空 MD5、固定掩码、控制字符/非 UTF-8 路径、非法状态/UUID/掩码组合和跨 lane 重复 item ID。

- [ ] **Step 2：Task 6A 文件/状态 RED→GREEN**

  双 lane append/flush/seal/有限预读；ACK 失败保持 `P`，ACK 成功只改首字节为 `C`，文件失败改 `F`，其余行体 hash 不变；错误 run/lane/item/offset/mask、损坏行和非法转换全部拒绝。

- [ ] **Step 3：提交并独立审查 Task 6A**

  审查单所有者、BufWriter 与绝对写入顺序、published 边界、内存上限和目录安全；通过后再开始 6B。

- [ ] **Step 4：Task 6B dispatcher RED**

  fake provider 验证每 lane 最多一个 outstanding head、permit 成功后才 take、取消/失败保持 `P`、未 seal 空 lane等待、sealed终止、有限预读和复合盘 identity。

- [ ] **Step 5：Task 6B weighted scheduler RED→GREEN**

  至少覆盖配置 5:1 与 7:2（证明非硬编码）、两个 SSD/两个 HDD 按物理 lane 独立计权、同盘多根不翻倍、复合盘原子许可、阻塞盘不占 global、低权重 lane 不超过既有 8 次冲突绕过、Hash/Media 3:1 保持、取消后 waiting/active 归零。

- [ ] **Step 6：完整回归**

  ```powershell
  cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1
  cargo test -p dedup-node-engine --features test-hooks --test scan_roots --locked -- --test-threads=1
  cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
  cargo test -p dedup-node-engine --lib --locked -- --test-threads=1
  cargo fmt --all -- --check
  git diff --check
  ```

  全部 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，关闭 incremental/debug info并清除继承的 MinGW 环境变量。重型命令前检查 C、D；任一低于 10 GiB 时只清理本计划精确确认的可再生 target 后继续。

- [ ] **Step 7：报告和边界**

  分别生成中文 Task 6A/6B 报告和提交。不得修改 actor/BaseCompute/SQLite/协议/UI，不得运行真实媒体、打包、部署或触碰 `I:\Tool`。
