# Rust V2 Node 物理磁盘 I/O 与多 Worker 流水线实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 把串行扫描改为按物理磁盘公平调度的有界流水线，支持可取消分块读取、超时重试、多 Worker 并行、持久文件故障、MD5 联系表复用和磁盘满时一次性清理全部合格派生产物。

**架构：** Windows 层把路径映射到物理盘并提供可取消块读取；Node `DiskReadScheduler` 同时持有每盘和全局许可；扫描协调器通过有界通道连接缓存查询、读取/MD5、Worker 与单 SQLite writer。WorkerPool 保持多项在途并以 task/item ID 乱序归并，运行文件租约覆盖 Worker 整个访问期。

**技术栈：** Rust 1.97.1、Tokio、Windows Overlapped I/O、rusqlite、Prost、MD5、FFmpeg WorkerPool。

**规格：** `docs/superpowers/specs/2026-08-21-node-runtime-scheduling-and-task-details-design.md`

**全局约束：** 先完成 `2026-08-21-node-remote-config-restart.md`。不自动迁移 schema v1；新 schema v2 遇到旧库直接拒绝。Worker 崩溃文件不重试且不熔断任务；读取超时默认 3 秒、重试 2 次；故障表不记录任务/项/偏移/块/PID/退出码/时间/次数。只支持本机物理盘。Cargo 输出固定 `C:\tmp\rust-v2-node-runtime-target`，只跑列出的相关测试。

---

### 任务 0：收束 Everything 默认启动与 Walker 降级前置能力

**Files:**
- Modify: `Cargo.lock`
- Modify: `crates/node-engine/Cargo.toml`
- Modify: `crates/node-engine/src/scan/everything.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/tests/enumerators.rs`

**Interfaces:**
- Consumes: `CreateScan.enumerator = "everything"`、与 `node.exe` 同目录的 `Everything.exe`。
- Produces: 复用已运行 Everything；未运行时只启动一次并等待数据库就绪；启动失败或就绪超时后整次回退 `WindowsWalker`。

- [ ] **Step 1: 验证现有批准草稿**

当前工作树已有未提交 Everything 启动草稿，必须原地审阅和测试，不能 checkout 或重写。测试固定：已就绪不启动；未就绪只启动一次；每 250ms 检查、最多 120 次；启动失败/30 秒超时返回 false；actor 只有收到 Everything 扫描请求时才调用启动逻辑。

- [ ] **Step 2: 运行定向行为测试**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --lib everything --locked -- --test-threads=1
cargo test -p dedup-node-engine --test enumerators --locked -- --test-threads=1
```

Expected: PASS；不可用 Everything 不导致整个扫描失败，而是整次使用 Walker。

- [ ] **Step 3: 精确提交前置能力**

```powershell
git add -- Cargo.lock crates/node-engine/Cargo.toml crates/node-engine/src/scan/everything.rs crates/node-engine/src/scan/mod.rs crates/node-engine/src/actor.rs crates/node-engine/tests/enumerators.rs
git commit -m "feat: start everything for node scans"
```

Expected: 提交只含 Everything 运行边界；发布复制在最终验收子计划完成。

---

### 任务 1：建立 schema v2 文件故障表并拒绝旧库

**Files:**
- Modify: `crates/node-store/src/schema.sql`
- Modify: `crates/node-store/src/open.rs`
- Create: `crates/node-store/src/faults.rs`
- Modify: `crates/node-store/src/lib.rs`
- Create: `crates/node-store/tests/file_faults.rs`
- Create: `crates/node-store/tests/open.rs`

**Interfaces:**
- Produces: `FileFaultKind`、`FileFaultRecord`、`upsert_file_fault`、`clear_file_fault`、`page_file_faults`；schema `user_version = 2`。

- [ ] **Step 1: 写 schema 和字段 RED**

测试唯一键为 `(machine_id, normalized_path, fault_kind)`；再次写同一故障只替换 size/stage/code/message；成功后按路径清除；分页稳定。使用手工创建的 `user_version=1` 当前产品库，断言 `NodeStore::open` 返回 `IncompatibleSchema`，绝不执行 ALTER 或复制。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-store --test file_faults --locked -- --test-threads=1
cargo test -p dedup-node-store --test open rejects_schema_v1 --locked -- --test-threads=1
```

Expected: FAIL，表和 API 不存在，旧库仍可能被接受。

- [ ] **Step 3: 实现精确表结构**

```sql
CREATE TABLE file_faults (
    machine_id TEXT NOT NULL,
    normalized_path TEXT NOT NULL,
    display_path TEXT NOT NULL,
    file_size INTEGER NOT NULL CHECK(file_size >= 0),
    fault_kind TEXT NOT NULL CHECK(fault_kind IN ('suspected_physical_read','worker_crash')),
    stage TEXT NOT NULL,
    windows_error_code INTEGER,
    message TEXT NOT NULL,
    PRIMARY KEY(machine_id, normalized_path, fault_kind)
) STRICT;
```

`open.rs` 同时校验产品 marker 与 `PRAGMA user_version == 2`。没有迁移函数。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-store --test file_faults --locked -- --test-threads=1
cargo test -p dedup-node-store --test open rejects_schema_v1 --locked -- --test-threads=1
git add -- crates/node-store/src/schema.sql crates/node-store/src/open.rs crates/node-store/src/faults.rs crates/node-store/src/lib.rs crates/node-store/tests/file_faults.rs crates/node-store/tests/open.rs
git commit -m "feat: persist node file faults"
```

Expected: PASS；schema v1 只拒绝不迁移。

---

### 任务 2：把本机路径映射到物理磁盘与介质类型

**Files:**
- Modify: `crates/windows/Cargo.toml`
- Modify: `crates/windows/src/lib.rs`
- Modify: `crates/windows/src/local_path.rs`
- Create: `crates/windows/src/storage_device.rs`
- Create: `crates/windows/tests/storage_device.rs`

**Interfaces:**
- Produces: `PhysicalDiskId`、`LocalDiskKind::{Hdd,Ssd,Unknown}`、`StorageLocation`、`resolve_storage_location(path)`。

- [ ] **Step 1: 写解析 RED**

用可注入 Windows 查询适配器覆盖：同一物理盘的两个卷得到相同 ID；不同盘不同 ID；SSD/HDD 映射；多 extent 卷降级为复合 Unknown；UNC、DRIVE_REMOTE、无物理 extent 全部拒绝。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-windows --test storage_device --locked -- --test-threads=1
```

Expected: FAIL，存储设备 API 不存在。

- [ ] **Step 3: 实现 Windows 查询**

增加 `Win32_System_IO`、`Win32_Storage_FileSystem` 和 `Win32_System_Ioctl` features。使用 `GetVolumePathNameW`、卷句柄 `IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS` 和设备查询获得物理盘编号与旋转属性；无法可靠判定旋转属性时返回 Unknown，不猜测 SSD。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-windows --test storage_device --locked -- --test-threads=1
git add -- crates/windows/Cargo.toml crates/windows/src/lib.rs crates/windows/src/local_path.rs crates/windows/src/storage_device.rs crates/windows/tests/storage_device.rs
git commit -m "feat: identify local physical disks"
```

Expected: PASS；网络盘没有兼容回退。

---

### 任务 3：实现可取消分块读取和固定重试语义

**Files:**
- Modify: `crates/windows/Cargo.toml`
- Modify: `crates/windows/src/lib.rs`
- Create: `crates/windows/src/overlapped_reader.rs`
- Create: `crates/node-engine/src/io/mod.rs`
- Create: `crates/node-engine/src/io/retrying_reader.rs`
- Create: `crates/node-engine/tests/retrying_reader.rs`

**Interfaces:**
- Produces: `OverlappedFileReader::read_at`、`RetryingFileReader::read_file_md5`、`ReadFailure::SuspectedPhysical`。

- [ ] **Step 1: 写确定性 RED**

fake block reader 返回 `Pending` 直到测试触发 timeout，断言每块最多 3 次尝试；第 2 次成功不写故障；第 3 次仍超时返回 suspected physical；取消 token 到达立即 `CancelIoEx`，不继续下一块；MD5 跨多个配置块与 `md5_file` 相同。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test retrying_reader --locked -- --test-threads=1
```

Expected: FAIL，读取抽象和重试策略不存在。

- [ ] **Step 3: 实现 Overlapped I/O**

Windows 生产实现用 `CreateFileW(FILE_FLAG_OVERLAPPED)`、事件、`ReadFile`、超时等待、`CancelIoEx`、`GetOverlappedResult`。Node 层只把 timeout/retry/block-size 从已验证配置注入。错误保留可选 `raw_os_error()`；文案固定“疑似物理读取故障”，不声称硬盘已确认损坏。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test retrying_reader --locked -- --test-threads=1
git add -- crates/windows/Cargo.toml crates/windows/src/lib.rs crates/windows/src/overlapped_reader.rs crates/node-engine/src/io/mod.rs crates/node-engine/src/io/retrying_reader.rs crates/node-engine/tests/retrying_reader.rs
git commit -m "feat: retry cancellable node reads"
```

Expected: PASS；默认单块超时和重试来自 NodeConfig 的 3 秒/2 次。

---

### 任务 4：实现每盘加全局许可的公平读取调度器

**Files:**
- Create: `crates/node-engine/src/io/scheduler.rs`
- Modify: `crates/node-engine/src/io/mod.rs`
- Create: `crates/node-engine/tests/disk_scheduler.rs`

**Interfaces:**
- Consumes: `StorageLocation`、`DiskReadConfig`。
- Produces: `DiskReadScheduler::acquire(location)`、持有到 Worker 文件访问结束的 `DiskReadPermit`。

- [ ] **Step 1: 写公平性与容量 RED**

测试同一 HDD 默认只进 1 项、同一 SSD 进 2 项、全局不超过 4；物理盘 A 队列很长时盘 B 在有限次调度内开始；同物理盘的不同卷共享计数；取消等待项不泄漏许可。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1
```

Expected: FAIL，调度器不存在。

- [ ] **Step 3: 实现双层许可和 round-robin**

调度器内部只有有界请求通道、每盘 FIFO 和盘 ID round-robin；授予时同时消耗全局和每盘 permit。`DiskReadPermit` 的 Drop 归还两层许可。队列容量为 `max(total_threads * 4, effective_worker_count * 2)`。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1
git add -- crates/node-engine/src/io/scheduler.rs crates/node-engine/src/io/mod.rs crates/node-engine/tests/disk_scheduler.rs
git commit -m "feat: schedule reads by physical disk"
```

Expected: PASS；不存在按字节大小的全局 limiter。

---

### 任务 5：把 ScanEngine 改为有界并行流水线和单 SQLite writer

**Files:**
- Modify: `crates/node-engine/src/scan/hash.rs`
- Modify: `crates/node-engine/src/scan/engine.rs`
- Create: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/node-engine/tests/scan_cache.rs`
- Modify: `crates/node-engine/tests/worker_pipeline.rs`
- Create: `crates/node-engine/tests/scan_parallelism.rs`

**Interfaces:**
- Consumes: `DiskReadScheduler`、`RetryingFileReader`、WorkerPool。
- Produces: enumerate/cache/read/worker/write 五段有界管道；多项 Worker 在途与乱序结果归并。

- [ ] **Step 1: 写现有串行行为 RED**

可控 reader 阻塞盘 A、释放盘 B；断言 B 可完成 MD5。可控 WorkerPool 的两个槽同时收到不同 item，先完成第二项后 SQLite writer 仍按身份写对 content。通道容量耗尽时枚举 producer 阻塞而不是累计全部路径。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test scan_parallelism --locked -- --test-threads=1
```

Expected: FAIL，第二项在第一项释放前不会派发。

- [ ] **Step 3: 实现拥有所有权的消息**

```rust
struct HashedFile {
    scanned: ScannedPath,
    storage: StorageLocation,
    md5: [u8; 16],
    permit: DiskReadPermit,
}

struct WorkerResult {
    task_id: TaskId,
    item_id: String,
    content_id: ContentId,
    output: Result<Stage1Output, FileProcessingFailure>,
}
```

permit 随请求进入 Worker 并在结果持久化后释放，确保 FFmpeg 访问也计入物理盘并发。只有 writer task 借用可重新打开的 `NodeStore` 写 SQLite；结果乱序不改变身份。

- [ ] **Step 4: 取消和背压**

取消后停止枚举和新派发，取消尚未领取的读请求，终止该任务正在运行的 Worker；已回来的结果只按已持久化 Cancelled 规则收束，不能覆盖取消状态。

- [ ] **Step 5: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test scan_parallelism --locked -- --test-threads=1
cargo test -p dedup-node-engine --test scan_cache --locked -- --test-threads=1
cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1
git add -- crates/node-engine/src/scan/hash.rs crates/node-engine/src/scan/engine.rs crates/node-engine/src/scan/pipeline.rs crates/node-engine/src/scan/mod.rs crates/node-engine/src/worker/pool.rs crates/node-engine/tests/scan_cache.rs crates/node-engine/tests/worker_pipeline.rs crates/node-engine/tests/scan_parallelism.rs
git commit -m "feat: run bounded parallel scan pipeline"
```

Expected: 三条命令 PASS；Worker 同时处理多个文件。

---

### 任务 6：永久跳过 Worker 崩溃项并记录文件故障

**Files:**
- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-store/src/tasks.rs`
- Modify: `crates/node-engine/tests/worker_pipeline.rs`
- Modify: `crates/node-store/tests/task_recovery.rs`

**Interfaces:**
- Consumes: `WorkerEvent::Crashed`、工作身份中的路径/size/stage。
- Produces: Failed task item、`worker_crash` file fault、补建槽位；不重新排队当前项。

- [ ] **Step 1: 写崩溃 RED**

真实 WorkerPool 终止处理 file-A 的进程；断言 file-A 失败且只派发一次，fault 行字段完整，file-B 继续执行，池恢复配置槽位数。关闭并重开 store 后，恢复逻辑不得把 file-A 改回 queued。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test worker_pipeline crash_fault --locked -- --test-threads=1
cargo test -p dedup-node-store --test task_recovery crashed_item --locked -- --test-threads=1
```

Expected: FAIL，崩溃身份没有足够路径信息或恢复会重排。

- [ ] **Step 3: 实现崩溃终态**

WorkerPool 的运行身份增加 machine/path/size/stage，但故障记录不得保存 PID 或退出码。actor 单写事务先完成 item=Failed，再 upsert fault，随后允许池补槽和调度后续项。不增加崩溃次数熔断。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test worker_pipeline crash_fault --locked -- --test-threads=1
cargo test -p dedup-node-store --test task_recovery crashed_item --locked -- --test-threads=1
git add -- crates/node-engine/src/worker/pool.rs crates/node-engine/src/scan/pipeline.rs crates/node-engine/src/actor.rs crates/node-store/src/tasks.rs crates/node-engine/tests/worker_pipeline.rs crates/node-store/tests/task_recovery.rs
git commit -m "fix: skip files that crash workers"
```

Expected: PASS；同一崩溃文件不会陷入循环。

---

### 任务 7：按 MD5 路径复用视频联系表

**Files:**
- Modify: `proto/node.proto`
- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-store/src/features.rs`
- Modify: `crates/node-engine/src/worker/pipeline.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Create: `crates/node-engine/src/contact_sheet_cache.rs`
- Modify: `crates/node-engine/tests/scan_cache.rs`
- Modify: `crates/node-engine/tests/worker_pipeline.rs`

**Interfaces:**
- Produces: `<cache>/contact-sheets/<md5[0..2]>/<md5>.jpg`；`ProbeAndStage1.generate_contact_sheet`；存在文件复用和 DB 引用修复。

- [ ] **Step 1: 写缓存 RED**

已有目标 JPG 且 DB 无引用时，强制重算仍执行 probe/feature，但联系表编码次数为 0，并把相对路径写回 DB。目标不存在时只生成一次，临时文件原子替换；MD5 十六进制固定小写 32 位。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test scan_cache contact_sheet_md5 --locked -- --test-threads=1
cargo test -p dedup-node-engine --test worker_pipeline skips_contact_sheet --locked -- --test-threads=1
```

Expected: FAIL，现有路径仍以 content_id 命名且 Worker 总是编码。

- [ ] **Step 3: 实现复用**

扫描在已有 MD5 后计算目标并设置 `generate_contact_sheet = !target.exists()`。Worker 在 false 时仍返回六帧特征但 `contact_sheet_jpeg=None`。写入使用同目录 `.partial`、flush、rename；DB 只存 `contact-sheets/ab/<md5>.jpg`。旧 `content_id.jpg` 不迁移、不删除。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test scan_cache contact_sheet_md5 --locked -- --test-threads=1
cargo test -p dedup-node-engine --test worker_pipeline skips_contact_sheet --locked -- --test-threads=1
git add -- proto/node.proto crates/node-store/src/content.rs crates/node-store/src/features.rs crates/node-engine/src/worker/pipeline.rs crates/node-engine/src/scan/pipeline.rs crates/node-engine/src/contact_sheet_cache.rs crates/node-engine/tests/scan_cache.rs crates/node-engine/tests/worker_pipeline.rs
git commit -m "feat: reuse md5 contact sheet cache"
```

Expected: PASS；已存在 JPG 不再计算。

---

### 任务 8：实现磁盘满触发的一次性全量派生产物清理

**Files:**
- Modify: `crates/windows/src/storage_device.rs`
- Create: `crates/node-engine/src/artifact_registry.rs`
- Create: `crates/node-engine/src/disk_full_cleanup.rs`
- Modify: `crates/node-engine/src/contact_sheet_cache.rs`
- Modify: `crates/node-store/src/features.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Create: `crates/node-engine/tests/disk_full_cleanup.rs`

**Interfaces:**
- Produces: `ArtifactLease`、`RegenerableArtifactRegistry`、`write_with_disk_full_cleanup()`、最近一次清理摘要。

- [ ] **Step 1: 写安全集合 RED**

fixture 在同盘安装根内创建 contact-sheet、preview、专用 `cache/tmp/*.partial`、活动 lease、数据库、配置、日志、exe、源码、target、dist、zip 和扫描媒体；fake writer 第一次返回 Windows 112，第二次成功。断言一次触发删除所有合格且未 lease 文件，不按释放字节提前停止；排除项全部保留；DB 清除已删 contact sheet 引用；原写入只重试一次。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test disk_full_cleanup --locked -- --test-threads=1
```

Expected: FAIL，artifact registry 和清理器不存在。

- [ ] **Step 3: 实现冻结集合与租约**

registry 只接受安装根下的规范绝对路径和固定 kind：`ContactSheet`、`Preview`、`OrphanTemporary`、`RegisteredDerivation`。清理时冻结快照，按 `resolve_storage_location` 过滤同物理盘，排除 active lease，删除整个集合并汇总数量/字节。绝不遍历或清理源码、Cargo target、dist 或扫描根。

- [ ] **Step 4: 固定触发和重试次数**

只识别 `ERROR_DISK_FULL(112)` 和 `ERROR_HANDLE_DISK_FULL(39)`。第一次写失败 → 完整清理 → 重试一次；第二次失败直接返回，不再清理。其他 I/O 错误不触发。

- [ ] **Step 5: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test disk_full_cleanup --locked -- --test-threads=1
git add -- crates/windows/src/storage_device.rs crates/node-engine/src/artifact_registry.rs crates/node-engine/src/disk_full_cleanup.rs crates/node-engine/src/contact_sheet_cache.rs crates/node-store/src/features.rs crates/node-engine/src/lib.rs crates/node-engine/tests/disk_full_cleanup.rs
git commit -m "feat: clean regenerable files on disk full"
```

Expected: PASS；一次触发删除所有合格文件。

---

### 任务 9：暴露文件故障协议与设置页诊断列表

**Files:**
- Modify: `proto/node.proto`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/desktop-core/src/node_session.rs`
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-core/src/view_state.rs`
- Modify: `crates/desktop-ui/ui/theme.slint`
- Modify: `crates/desktop-ui/ui/app.slint`
- Modify: `crates/desktop-ui/ui/pages/settings-workspace.slint`
- Modify: `crates/desktop-ui/src/bindings.rs`
- Modify: `crates/desktop-ui/tests/bindings_contract.rs`
- Modify: `crates/desktop-ui/tests/window_contract.rs`

**Interfaces:**
- Produces: `ListFileFaults`、`ClearFileFault`、诊断分页和最近清理摘要。

- [ ] **Step 1: 写协议与 UI RED**

断言分页只返回批准的字段；清理按机器/path/kind 精确一次；UI 不显示任务 ID、偏移、PID、时间或次数；“疑似物理读取故障”文案准确；最近清理展示触发时间仅为运行摘要，不进入 file_faults 表。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-ui --test bindings_contract file_faults --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract file_faults --locked -- --test-threads=1
```

Expected: FAIL，协议、绑定和诊断列表不存在。

- [ ] **Step 3: 实现协议和列表**

Envelope 使用紧接配置消息之后的未占字段号。设置页“日志与诊断”提供节点选择、加载下一页和手工清除；离线时禁用。最近磁盘满清理摘要从 Node 运行状态返回，不写 SQLite。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-ui --test bindings_contract file_faults --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract file_faults --locked -- --test-threads=1
cargo check -p dedup-node-engine -p dedup-node-store -p dedup-desktop-core -p dedup-desktop-ui -p node -p worker --locked
git add -- proto/node.proto crates/node-engine/src/actor.rs crates/desktop-core/src/node_session.rs crates/desktop-core/src/app.rs crates/desktop-core/src/view_state.rs crates/desktop-ui/ui/theme.slint crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/pages/settings-workspace.slint crates/desktop-ui/src/bindings.rs crates/desktop-ui/tests/bindings_contract.rs crates/desktop-ui/tests/window_contract.rs
git commit -m "feat: expose node file fault diagnostics"
```

Expected: 两个行为测试和相关包 check PASS。

---

### 任务 10：完成 I/O 子系统定向集成门禁

**Files:**
- Create: `crates/node-engine/tests/resource_pipeline.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Verifies: 双物理盘 fake、读取 timeout、Worker crash、联系表复用、单写者和任务继续运行。

- [ ] **Step 1: 写组合测试**

组合两个盘、三个读取项、两个 Worker：盘 A 首项超时三次后故障，盘 B 两项并行；一个媒体项触发 Worker crash，后续项成功；最终任务 Completed 且 `failed_items=2`，fault 表两类各一行，联系表路径为 MD5 分层，writer 并发峰值为 1。

- [ ] **Step 2: 运行定向门禁**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test resource_pipeline --locked -- --test-threads=1 --nocapture
```

Expected: PASS；日志显示两盘交错推进和 Worker 补槽。

- [ ] **Step 3: 更新架构并提交**

`AGENTS.md` 记录 schema v2 不迁移、双层许可、单写者、故障字段禁区、联系表路径和磁盘满清理排除项。

```powershell
git diff --check
git add -- crates/node-engine/tests/resource_pipeline.rs AGENTS.md
git commit -m "test: verify node resource pipeline"
```

Expected: 无空白错误；不运行 workspace 全量测试。
