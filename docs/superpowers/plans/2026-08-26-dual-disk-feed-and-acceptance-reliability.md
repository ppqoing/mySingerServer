# Rust V2 双盘任务供给与验收可靠性修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复多扫描根按路径前缀串行进入 BaseCompute 的架构问题，使不同根的请求尽早同时进入现有物理盘调度器；同时修复 Worker 退出导致系统采样中止和 SQLite 首次打开导致结果导出误报，形成可重复、不会污染生产的验收闭环。

**Architecture:** 枚举器继续输出全局稳定排序和去重清单，新增私有 `RootFairInputOrder` 在 BaseCompute 前按最具体扫描根做确定性 round-robin。`DiskReadScheduler` 继续负责真实物理盘许可和公平性。采样器通过进程属性快照与 skip 记录容忍退出竞态；结果导出器把 sidecar 快照移动到 SQLite 打开之后。逐盘许可状态通过追加协议字段进入 runtime NDJSON 和报告。

**Tech Stack:** Rust 2024、Tokio 1.53、Prost 0.14、rusqlite 0.40 / SQLite WAL、PowerShell 7、Windows 进程与物理盘性能计数器、SHA-256、规范化 JSONL。

**Spec:** [Rust V2 双盘任务供给与验收可靠性修复设计](../specs/2026-08-26-dual-disk-feed-and-acceptance-reliability-design.md)

## Global Constraints

- 实施工作树固定为 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup`。
- 当前工作树已有大量用户修改和未跟踪文件。每个任务开始前记录 `git status --short` 和目标文件 SHA-256；禁止 `git reset`、`git clean`、整仓 stage、覆盖或归并无关改动。
- 只修改本计划列出的精确文件。目标文件在实施前已 dirty 时，checkpoint 标记 `COMMIT_DEFERRED_DIRTY_BASELINE`；只有 staged hunk 全部可证明属于本任务时才允许精确 pathspec 提交。
- 新增或修改的方法、类型、字段和关键变量必须添加中文注释，说明用途、调用边界、所有权和失败行为。
- 不修改 SSD/HDD/Unknown 识别、Worker 数、读取许可默认值、FFmpeg 算法或 Worker 崩溃根因。
- 不修改媒体文件，不访问或替换 `I:\Tool\mySingerServer-rust-v2-win-x64`，不删除已有 `C:\tmp` 运行证据。
- 本计划禁止再次执行 `H:\pik\00000000000` + `I:\tmp` 全量真实媒体测试；只使用合成/临时 fixture。真实媒体复测需要用户另行授权。
- `runtime_acceptance.exe` 和 `export_scan_result_summary.exe` 保持正式 ZIP 外置；正式包顶层 EXE 白名单仍为 desktop/node/worker/Everything 四项。
- 单个 Worker 退出只允许使该 PID 的一次系统样本被跳过，不得吞掉 Worker 崩溃日志，也不得把业务崩溃改写成成功。
- 根轮转只改变执行供给顺序；结果摘要、同步输出和正确性比较继续按 `normalized_path` 排序。
- 所有实现步骤执行 RED → GREEN → 定向回归。禁止用源码字符串包含测试替代行为测试。
- 重型 Cargo 命令前检查 C/D 可用空间。任一盘低于 10 GiB 时，仅允许清理已盘点的可再生项目缓存，例如本任务专用 `C:\tmp\rust-v2-dual-feed-*-target`；不得删除 evidence、package、日志或源码。清理后记录精确路径、大小和剩余空间，再继续当前任务。
- 最终审查使用 `gpt-5.6-sol`、`max` reasoning；审查不能替代自动测试。
- Worker 协议帧截断/ACK 身份风险未修复前，最终结论保持 `NON_DEPLOYABLE`。

---

## File Structure

### 验收可靠性

- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
  增加进程属性原子快照、退出 skip、世代基线清理及 NDJSON 诊断字段。
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`
  增加 getter 返回 null、getter 抛错、健康进程并存和 PID 复用行为测试。
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
  汇总 `process_sample_skips`，但不把少量退出 skip 自动判为 INCONCLUSIVE。
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`
  固定 skip 汇总与既有证据完整性门禁。
- Modify: `crates/node-store/src/result_summary.rs`
  调整 SQLite 打开与 sidecar 快照顺序。
- Modify: `crates/node-store/tests/result_summary_export.rs`
  覆盖打开前自变更容忍、快照后外部变化拒绝。

### 多根供给

- Create: `crates/node-engine/src/scan/input_order.rs`
  实现按最具体规范根归属和确定性 round-robin。
- Modify: `crates/node-engine/src/scan/mod.rs`
  注册私有模块并只在 crate 内暴露排序入口。
- Modify: `crates/node-engine/src/actor.rs`
  在完整枚举成功后、冻结总数和调用 BaseCompute 前应用执行顺序。
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
  以受控 reader 证明首个有界窗口同时推进两个根，且 output/decode/persist ownership 不越界。
- Modify: `crates/node-engine/tests/enumerators.rs`
  保留原全局排序和去重契约，防止把执行顺序错误下沉到枚举器。
- Modify: `crates/node-store/tests/task_recovery.rs`
  证明交错 reserve 后 queued 项均可恢复和领取，状态统计不变。

### 逐物理盘遥测

- Modify: `proto/node.proto`
  追加 `RuntimeDiskReadMetrics` 和 `RuntimePipelineMetrics.disk_reads = 28`。
- Modify: `crates/protocol/tests/runtime_tasks_wire.rs`
  固定 message/tag/round-trip/旧消息缺字段行为。
- Modify: `crates/node-engine/src/runtime_tasks.rs`
  保存逐盘 waiting/active/累计授予/累计释放。
- Modify: `crates/node-engine/src/scan/pipeline.rs`
  在解析位置、等待许可、取得许可和 Drop 边界更新逐盘指标。
- Modify: `crates/node-engine/tests/runtime_tasks.rs`
  验证逐盘守恒、复合盘和终态归零。
- Modify: `crates/node-engine/tests/disk_scheduler.rs`
  保留并扩展“两个请求已经可见时盘间轮转”的确定性测试。
- Modify: `crates/desktop-core/examples/runtime_acceptance.rs`
  投影 `disk_reads` 到 1 秒 runtime NDJSON。
- Modify: `crates/desktop-core/tests/runtime_acceptance_contract.rs`
  固定字段和旧 Node 兼容。
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
  输出逐盘 waiting/active/授予/释放表和守恒结论。
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`
  增加双盘交叠、单盘串行和字段缺失 fixture。

### 门禁与记录

- Create: `docs/verification/2026-08-26-dual-disk-feed-and-acceptance-repair.md`
  保存实施 checkpoint、测试结果、产物哈希和最终裁决；不得覆盖 2026-08-26 原真实媒体报告。
- Reference: `crates/node-engine/benches/base_compute_pipeline.rs`
- Reference: `scripts/build-release.ps1`
- Reference: `scripts/verify-release.ps1`
- Reference: `tests/windows/Test-RustV2Package.ps1`

---

### Task 1: 冻结 dirty 基线和写实施账本

**Files:**

- Create: `docs/verification/2026-08-26-dual-disk-feed-and-acceptance-repair.md`
- Reference: `docs/verification/2026-08-26-dual-physical-disk-single-run.md`
- Reference: `docs/superpowers/specs/2026-08-26-dual-disk-feed-and-acceptance-reliability-design.md`

**Interfaces:**

- Produces: 当前 HEAD、status、目标文件哈希、工具链版本、可用空间和旧报告 SHA-256。
- Does not produce: 产品代码、真实媒体运行或部署。

- [ ] **Step 1: 建立只写 `C:\tmp` 的实施证据根**

```powershell
$repo = 'D:\code\mySingerServer\.worktrees\rust-v2-media-dedup'
$evidence = 'C:\tmp\rust-v2-dual-feed-repair'
New-Item -ItemType Directory -Path $evidence -Force | Out-Null
Set-Location $repo
git rev-parse HEAD | Set-Content -LiteralPath (Join-Path $evidence 'baseline-head.txt') -Encoding utf8NoBOM
git status --short | Set-Content -LiteralPath (Join-Path $evidence 'baseline-status.txt') -Encoding utf8NoBOM
```

- [ ] **Step 2: 对计划目标文件生成 SHA-256 清单**

清单至少包含本计划 `File Structure` 中所有已存在文件、`Cargo.lock`、设计文档和原真实媒体报告。不存在的新文件写 `MISSING_BEFORE`，不得用空文件占位。

- [ ] **Step 3: 记录磁盘空间并执行受限自动清理规则**

```powershell
Get-PSDrive -Name C,D | Select-Object Name,Used,Free
Get-ChildItem -LiteralPath 'C:\tmp' -Directory |
    Where-Object Name -Like 'rust-v2-dual-feed-*-target' |
    Select-Object FullName
```

只有可用空间低于 10 GiB 且上述目录经确认是本任务 Cargo target 时才删除；先把精确绝对路径和大小写入账本。不得使用通配符递归删除。

- [ ] **Step 4: 运行冻结前固定合成基准三轮**

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-dual-feed-baseline-target'
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

命令恰好执行三次，分别保存 stdout、`elapsed_ms`、`persisted_completed`、benchmark EXE 绝对路径和 SHA-256。三轮 `persisted_completed` 必须为 `true`，以三轮 `elapsed_ms` 中位数作为 Task 9 的唯一冻结前性能基线；2026-08-26 报告中的 125.719 ms 只作历史交叉检查。

- [ ] **Step 5: 写账本头部**

记录：问题三分法、旧运行根、旧报告哈希、`NO_REAL_MEDIA_RERUN`、`NO_I_TOOL_ACCESS`、`COMMIT_DEFERRED_DIRTY_BASELINE`、Worker 风险 `NON_DEPLOYABLE`。

- [ ] **Step 6: Checkpoint**

运行 `git diff --check`。只允许精确 stage 新账本和本计划文档；若 staged 列表出现其他文件，取消本次 stage。推荐提交说明：`docs: plan dual disk feed repair`。

---

### Task 2: 用 TDD 修复 Worker 退出导致系统采样中止

**Files:**

- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`
- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`

**Interfaces:**

- Adds: `Try-NewIsolatedProcessSample`，返回 `{ Sample, Skip }`。
- Adds: `Remove-IsolatedProcessBaselines`，按 `"$pid|"` 清理 CPU/I/O 世代。
- Adds to `system_sample`: `process_sample_skips`。
- Stable skip reason: `PROCESS_EXITED_DURING_SAMPLE`。

- [ ] **Step 1: 写 null getter RED fixture**

在 harness 现有逐 PID 样本测试旁加入：

```powershell
$departed = [pscustomobject]@{
    Id = 1002
    ProcessName = 'worker'
    StartTime = [DateTime]'2026-08-26T00:00:00Z'
    TotalProcessorTime = $null
    WorkingSet64 = $null
    PrivateMemorySize64 = $null
}
```

同时提供一个健康 PID。调用真实 `Write-SystemSample` 后断言：

- 命令不抛错；
- NDJSON 仍有一条 `system_sample`；
- 健康 PID 仍在 `processes`；
- 退出 PID 不在 `processes`；
- `process_sample_skips` 只有 PID 1002 和稳定 reason；
- PID 1002 的所有 `PreviousCpu` / `PreviousIo` key 已删除。

运行并确认 RED：

```powershell
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
```

预期当前代码在 `TotalMilliseconds` 处失败。

- [ ] **Step 2: 写 getter 抛错 RED fixture**

使用 `Add-Member -MemberType ScriptProperty` 创建读取 `TotalProcessorTime` 时抛错的进程对象；断言与 null fixture 相同。不得启动真实短命进程或用时间睡眠制造竞态。

- [ ] **Step 3: 实现先快照、后更新基线**

`Try-NewIsolatedProcessSample` 的顺序固定为：

```powershell
try {
    $processId = [int]$Process.Id
    $name = [string]$Process.ProcessName
    $startTimeUtc = Get-ProcessStartTimeUtc -Row $Row -Process $Process
    $cpuTime = $Process.TotalProcessorTime
    if ($null -eq $cpuTime) { throw 'PROCESS_EXITED_DURING_SAMPLE' }
    $totalCpuMs = [double]$cpuTime.TotalMilliseconds
    $workingSet = [long]$Process.WorkingSet64
    $privateMemory = [long]$Process.PrivateMemorySize64
}
catch {
    Remove-IsolatedProcessBaselines -ProcessId ([int]$Row.ProcessId) `
        -PreviousCpu $PreviousCpu -PreviousIo $PreviousIo
    return [pscustomobject]@{
        Sample = $null
        Skip = [pscustomobject]@{
            process_id = [int]$Row.ProcessId
            reason = 'PROCESS_EXITED_DURING_SAMPLE'
        }
    }
}
```

快照完整后才计算 delta 并写入两个 baseline；返回值的 `Sample` 使用局部值，不再二次读取 `$Process`。

- [ ] **Step 4: 接入系统采样写入**

`Write-SystemSample` 对每一行调用 `Try-NewIsolatedProcessSample`，分别收集 `Sample` 和 `Skip`。`New-SystemSampleRecord` 追加 `ProcessSampleSkips` 参数并序列化为 `process_sample_skips`。逻辑核、物理盘和 `Add-Content` 无论单个 PID 是否退出都继续执行。

- [ ] **Step 5: 验证 PID 复用和大计数回归**

运行：

```powershell
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
```

必须同时保留：首见世代零增量、同世代增量、PID 复用重建基线、超过 Int32 的 I/O 增量、退出 skip 和健康进程继续写入。

- [ ] **Step 6: Checkpoint**

```powershell
git diff --check -- tests/windows/Measure-RustV2RuntimeAcceptance.ps1 tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1
```

推荐提交说明：`fix: tolerate worker exit during runtime sampling`。目标文件已有旧改动时按 dirty 基线规则延迟提交。

---

### Task 3: 用 TDD 修复 SQLite 首次打开导致导出误报

**Files:**

- Modify: `crates/node-store/src/result_summary.rs`
- Modify: `crates/node-store/tests/result_summary_export.rs`

**Interfaces:**

- Keeps: `export_scan_result_summary(database_path, cache_root, task_id, output_path)`。
- Adds test-only `ResultSummaryReadTestHook::{AfterDatabaseOpenBeforeSidecarCapture, AfterSidecarCapture}` under `acceptance-tools`。
- Adds test-only `set_result_summary_read_test_hook(ResultSummaryReadTestHook)` and `set_result_summary_read_test_callback(Option<fn(&Path)>)`。
- Keeps strict verification after capture.

- [ ] **Step 1: 写首次打开 RED 测试**

在 `result_summary_export.rs` 建立 WAL 数据库、任务和最小完整结果。测试 hook 在 read-only connection 已打开、sidecar 尚未冻结时模拟 SHM 首次初始化，然后调用导出器。断言导出成功且 canonical/metadata/lease 三件套通过 `validate_result_summary_pair`。

当前顺序 `capture_sidecars → open_read_only_database` 下，该 fixture 必须因 sidecar hash 改变而 RED。

- [ ] **Step 2: 保留快照后变化拒绝测试**

新增第二个 hook `AfterSidecarCapture`，修改 WAL 或 SHM 内容；断言返回 `ResultSummaryError::InvalidArgument`，且 canonical/metadata/lease 均未提交。

- [ ] **Step 3: 调整生产顺序**

将函数入口顺序改为：

```rust
let canonical_cache_root = canonical_cache_root(cache_root)?;
let connection = open_read_only_database(database_path)?;
run_result_summary_read_test_hook(
    ResultSummaryReadTestHook::AfterDatabaseOpenBeforeSidecarCapture,
    database_path,
);
let sidecars = capture_sidecars(database_path)?;
run_result_summary_read_test_hook(
    ResultSummaryReadTestHook::AfterSidecarCapture,
    database_path,
);
// 现有 load_task_header、load_task_items 和 build_canonical_row 查询继续使用该 connection。
drop(connection);
verify_sidecars(&sidecars)?;
```

`verify_sidecars` 之后从 `classify_summary`、`encode_canonical_jsonl`、`atomic_write_pair`、`validate_result_summary_pair` 到 `ResultSummaryExport` 的现有提交流程逐项保留。测试 hook 只在 `acceptance-tools` feature 下存在；不得使用 `immutable=1`，不得删除现有文件身份和 SHA-256 复验。

- [ ] **Step 4: 运行定向测试**

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-dual-feed-store-target'
cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export --locked -- --test-threads=1
```

- [ ] **Step 5: Checkpoint**

```powershell
git diff --check -- crates/node-store/src/result_summary.rs crates/node-store/tests/result_summary_export.rs
```

推荐提交说明：`fix: snapshot sqlite sidecars after read-only open`。

---

### Task 4: 用 TDD 实现确定性多根 round-robin

**Files:**

- Create: `crates/node-engine/src/scan/input_order.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`

**Interfaces:**

```rust
pub(crate) fn interleave_rows_by_root(
    roots: &[DisplayPath],
    rows: Vec<ScannedPath>,
) -> Result<Vec<ScannedPath>, ScanError>;

#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub fn interleave_rows_by_root_for_test(
    roots: &[DisplayPath],
    rows: Vec<ScannedPath>,
) -> Result<Vec<ScannedPath>, ScanError>;
```

- [ ] **Step 1: 在新模块内写行为 RED 测试**

测试固定以下输入/输出：

```text
roots = [H:\Media, I:\Media]
input = [H:\Media\a, H:\Media\b, H:\Media\c, I:\Media\a, I:\Media\b]
output = [H:\Media\a, I:\Media\a, H:\Media\b, I:\Media\b, H:\Media\c]
```

再覆盖：

- 单根顺序完全不变；
- 三根、不均衡长度和空 bucket；
- 重复根去重；
- `C:\Media` 与 `C:\Media\Album` 重叠时选择组件最多的 `Album`；
- UNC 根；
- 不属于任一根的行返回 `ScanError::InvalidResult`；
- 输出路径集合、长度和总字节与输入完全相同。

运行并确认新模块尚未实现时 RED：

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-dual-feed-engine-target'
cargo test -p dedup-node-engine --features test-hooks input_order --locked -- --test-threads=1
```

- [ ] **Step 2: 实现根描述和归属**

内部类型固定为：

```rust
struct RootBucket {
    normalized_root: NormalizedPath,
    component_count: usize,
    rows: VecDeque<ScannedPath>,
}
```

根先规范化、字典序排序、去重。对每行筛选 `row.normalized_path.is_within(root)`，按 `component_count` 降序、`normalized_root` 升序选择唯一 bucket。

- [ ] **Step 3: 实现有界 round-robin 合并**

预分配 `Vec::with_capacity(rows.len())`；每轮每个非空 bucket 只 `pop_front()` 一项。不得 clone 媒体行，不得读取文件，不得改变 `ScannedPath`。

- [ ] **Step 4: GREEN 与格式检查**

```powershell
cargo fmt --all -- --check
cargo test -p dedup-node-engine --features test-hooks input_order --locked -- --test-threads=1
git diff --check -- crates/node-engine/src/scan/input_order.rs crates/node-engine/src/scan/mod.rs
```

- [ ] **Step 5: Checkpoint**

推荐提交说明：`feat: interleave scan roots before base compute`。

---

### Task 5: 在真实 Node → BaseCompute 边界接入轮转顺序

**Files:**

- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/enumerators.rs`
- Modify: `crates/node-store/tests/task_recovery.rs`

**Interfaces:**

- `FileEnumerator::enumerate` 继续返回全局 normalized 排序/去重清单。
- `run_background_job` 只把执行副本变换为根轮转顺序。
- BaseCompute、SQLite schema 和任务终态接口不变。

- [ ] **Step 1: 写枚举契约保护测试**

在 `enumerators.rs` 使用两个临时根，断言 Windows Walker 和可用时的 Everything 仍返回全局 normalized 排序且无重复。该测试防止实现者把 round-robin 放进枚举器、破坏稳定输出契约。

- [ ] **Step 2: 写 BaseCompute 双根可见性 RED 测试**

在 `base_compute_pipeline.rs` 建立：

- H 虚拟根 1,001 项；
- I 虚拟根 3 项；
- `PipelineLimits::new(4, 2)`；
- 受控 `PipelineFileReader` 按根映射到虚拟 PhysicalDisk1/2，并记录 Hash 启动路径；
- path cache、content cache、Worker 与持久化使用现有受控 fixture。

通过 `scan::interleave_rows_by_root_for_test` 取得与生产函数完全相同的输出并交给 BaseCompute，断言：

- 前四个成功 Hash 启动包含 H 和 I；
- I 首项在 H 的第 1,001 项之前启动；
- 两盘 active 都不超过虚拟每盘限制；
- 终态完成数等于输入，所有 ownership 归零。

- [ ] **Step 3: 接入 actor**

在 `run_background_job` 进入现有 `match rows` 前执行：

```rust
let rows = rows.and_then(|rows| interleave_rows_by_root(&options.roots, rows));
```

这样顺序规划失败会进入现有 `Err(error)` 分支，沿用枚举阶段失败持久化和 runtime failure；不能启动部分 BaseCompute。

- [ ] **Step 4: 写恢复契约测试**

在 `task_recovery.rs` 交错 reserve 两根项目，分别制造 queued/running/succeeded/failed 后重开 store；断言每项仍只出现一次、状态计数正确、queued 项均能领取。不要断言最终结果按执行顺序输出。

- [ ] **Step 5: 运行定向回归**

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-dual-feed-engine-target'
cargo test -p dedup-node-engine --features test-hooks --test enumerators --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-store --test task_recovery --locked -- --test-threads=1
```

- [ ] **Step 6: Checkpoint**

```powershell
git diff --check -- crates/node-engine/src/actor.rs crates/node-engine/tests/base_compute_pipeline.rs crates/node-engine/tests/enumerators.rs crates/node-store/tests/task_recovery.rs
```

推荐提交说明：`fix: expose multiple scan roots to disk scheduler`。

---

### Task 6: 追加逐物理盘许可协议和 runtime registry

**Files:**

- Modify: `proto/node.proto`
- Modify: `crates/protocol/tests/runtime_tasks_wire.rs`
- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-engine/tests/runtime_tasks.rs`

**Interfaces:**

```proto
message RuntimeDiskReadMetrics {
  string physical_disk_id = 1;
  optional uint64 capacity = 2;
  optional uint64 hash_waiting = 3;
  optional uint64 media_waiting = 4;
  optional uint64 hash_active = 5;
  optional uint64 media_active = 6;
  optional uint64 hash_granted_total = 7;
  optional uint64 media_granted_total = 8;
  optional uint64 hash_released_total = 9;
  optional uint64 media_released_total = 10;
}

message RuntimePipelineMetrics {
  // existing fields 1..27 unchanged
  repeated RuntimeDiskReadMetrics disk_reads = 28;
}
```

- [ ] **Step 1: 写 wire RED 测试**

在 descriptor 测试固定新消息全部 tag 和 `disk_reads=28`；构造两盘 round-trip。再用不含 field 28 的旧字节消息解码，断言 `disk_reads.is_empty()`。

- [ ] **Step 2: 写 registry RED 测试**

新增 `RuntimeDiskReadClass::{Hash, Media}` 和 reporter 行为测试：

- waiting `0→1→0`；
- active `0→1→0`；
- granted/released 单调增加；
- active/released 不得超过 granted；
- 复合盘对两个底层盘同时更新；
- 任务终态时 waiting/active 必须全零。

- [ ] **Step 3: 实现 registry API**

接口固定为：

```rust
pub fn disk_read_waiting_nowait(
    &self,
    disk_ids: &[String],
    class: RuntimeDiskReadClass,
    capacity: u64,
) -> Result<(), RuntimeTaskError>;

pub fn disk_read_wait_cancelled_nowait(
    &self,
    disk_ids: &[String],
    class: RuntimeDiskReadClass,
) -> Result<(), RuntimeTaskError>;

pub fn disk_read_acquired_nowait(
    &self,
    disk_ids: &[String],
    class: RuntimeDiskReadClass,
) -> Result<(), RuntimeTaskError>;

pub fn disk_read_released_nowait(
    &self,
    disk_ids: &[String],
    class: RuntimeDiskReadClass,
) -> Result<(), RuntimeTaskError>;
```

`disk_read_waiting_nowait` 执行 waiting +1；`disk_read_wait_cancelled_nowait` 执行 waiting -1；`disk_read_acquired_nowait` 在同一 registry 写锁内执行 waiting -1、active +1、granted +1；`disk_read_released_nowait` 执行 active -1、released +1。内部按 `physical_disk_id` 使用 `BTreeMap`，快照保持稳定顺序。减少到负数、active 超 capacity、released 超 granted返回新增的 `RuntimeTaskError::InvalidTransition` 或现有 `CapacityExceeded`，不得 saturating 吞错。

- [ ] **Step 4: 运行协议与 registry 测试**

```powershell
cargo test -p dedup-protocol --test runtime_tasks_wire --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test runtime_tasks --locked -- --test-threads=1
```

- [ ] **Step 5: Checkpoint**

推荐提交说明：`feat: expose per-disk read permit metrics`。

---

### Task 7: 用 RAII 把逐盘遥测接到真实读取许可生命周期

**Files:**

- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/disk_scheduler.rs`

**Interfaces:**

- Adds private `RuntimeDiskWaitGuard`。
- Extends private `ScheduledReadPermit` with `disk_ids` and `class` only for telemetry ownership.
- Does not change `DiskReadScheduler::acquire` or `PipelineFileReader` public behavior.

- [ ] **Step 1: 写取消和错误 RED 测试**

覆盖：位置解析失败、等待中取消、scheduler 关闭、许可取得后 read 失败、正常 Drop、复合盘许可。每条路径断言 waiting/active 最终归零且 granted/released 守恒。

- [ ] **Step 2: 实现 waiting guard**

位置解析得到底层盘号后转换成稳定 `PhysicalDiskN` 列表，按 `DiskReadConfig` 和本次 `LocalDiskKind` 计算观察到的每盘容量，创建 `RuntimeDiskWaitGuard`：构造时调用 `disk_read_waiting_nowait`；取得许可时调用 `disk_read_acquired_nowait`；未取得许可直接 Drop 时调用 `disk_read_wait_cancelled_nowait`。同一物理盘后来以更低容量出现时，registry 只允许把已记录 capacity 收紧到两者最小值，且不能低于当前 active。

- [ ] **Step 3: 扩展 active permit Drop**

`ScheduledReadPermit::drop` 在真实 `DiskReadPermit` 释放同一边界执行 active -1、released +1。禁止在 Worker Started 或结果持久化时释放读取许可指标。

- [ ] **Step 4: 保留 scheduler 原行为**

在 `disk_scheduler.rs` 增加显式场景：Disk1 和 Disk2 请求同时入队时，两盘均在全局容量允许下取得许可；Disk1 长 FIFO 不能阻止 Disk2。不得改变 seat、老化或复合盘算法来让测试通过。

- [ ] **Step 5: 运行定向测试**

```powershell
cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
```

- [ ] **Step 6: Checkpoint**

推荐提交说明：`feat: track per-disk permit lifecycle`。

---

### Task 8: 投影逐盘指标并修正验收报告语义

**Files:**

- Modify: `crates/desktop-core/examples/runtime_acceptance.rs`
- Modify: `crates/desktop-core/tests/runtime_acceptance_contract.rs`
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`

**Interfaces:**

- `runtime_sample.pipeline_metrics.disk_reads[]` mirrors protocol fields.
- `system_sample.process_sample_skips[]` is diagnostic evidence.
- Report statuses remain `PASS` / `FAIL` / `INCONCLUSIVE`; no new fourth state.

- [ ] **Step 1: 写 runtime NDJSON RED 契约**

构造 PhysicalDisk1/2 指标，断言 JSON 字段名、数值和稳定数组顺序。旧 Node 缺字段时输出空数组或 `null`，保持单一约定并固定测试。

- [ ] **Step 2: 写报告 RED fixtures**

至少三组：

1. 两盘 waiting/active 在同一有效窗口重叠，全部守恒；
2. `physical-disk-map.json` 明确两根位于不同盘，但 PhysicalDisk2 在 PhysicalDisk1 释放全部工作前从未 waiting/active，输出 `DISK_REQUEST_VISIBILITY_NOT_MET`；
3. 字段缺失，输出 `INCONCLUSIVE`，不得用 Worker 路径猜 permit。

另加一个 Worker 退出 skip fixture：system 样本完整、间隔合格时报告继续裁决，附 skip 次数；采样文件中断仍 INCONCLUSIVE。

- [ ] **Step 3: 实现报告表格和守恒门禁**

报告从 harness 现有 `physical_disk_map_path` / SHA-256 绑定预期媒体盘，再逐盘输出 capacity、waiting peak、active peak、grant/release totals、首次/末次 active 时间。硬失败条件：

- waiting/active 超 capacity；
- released 超 granted；
- 终态 waiting/active 非零；
- 多盘均有工作但一盘直到另一盘耗尽前从未进入 waiting/active。

如果任务没有终态，只能输出 INCONCLUSIVE，不得用架构窗口结论替代完成验收。

- [ ] **Step 4: 运行契约和 PowerShell tests**

```powershell
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1
```

- [ ] **Step 5: Checkpoint**

推荐提交说明：`test: report dual disk visibility and sampler skips`。

---

### Task 9: 完整定向回归、合成基准和包边界

**Files:**

- Modify: `docs/verification/2026-08-26-dual-disk-feed-and-acceptance-repair.md`
- Reference: `crates/node-engine/benches/base_compute_pipeline.rs`
- Reference: `scripts/build-release.ps1`
- Reference: `scripts/verify-release.ps1`
- Reference: `tests/windows/Test-RustV2Package.ps1`

- [ ] **Step 1: 运行 Rust 定向门禁**

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-dual-feed-final-target'
cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export --locked -- --test-threads=1
cargo test -p dedup-protocol --test runtime_tasks_wire --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test enumerators --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test runtime_tasks --locked -- --test-threads=1
cargo test -p dedup-node-store --test task_recovery --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
```

- [ ] **Step 2: 运行 Windows 门禁**

```powershell
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2Package.ps1
```

- [ ] **Step 3: 固定合成基准三轮**

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-dual-feed-bench-target'
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

运行三次，保存每轮 stdout、benchmark EXE 路径/SHA-256、`elapsed_ms` 和 `persisted_completed`。门禁：三轮全部完成，候选中位数不得比本次冻结前基线慢超过 5%。该基准只防止明显回退，不证明双盘真实性能。

- [ ] **Step 4: 构建并校验隔离候选包**

```powershell
pwsh -NoProfile -File scripts\build-release.ps1 -CargoTargetDir 'C:\tmp\rust-v2-dual-feed-final-target'
pwsh -NoProfile -File scripts\verify-release.ps1 -Package 'dist-rust-v2\mySingerServer-rust-v2-win-x64.zip'
```

保存 ZIP、sidecar、manifest 和 SHA-256 到新的 `C:\tmp\rust-v2-dual-feed-repair\package` 证据目录。不得复制到生产目录；外置 acceptance client/exporter 不得进入 ZIP。

- [ ] **Step 5: 明确禁止真实媒体复跑**

账本写入：

```text
REAL_MEDIA_RUN=NOT_EXECUTED
REASON=用户已要求当前双盘全量真实媒体只运行一次；本计划仅完成确定性 fixture 和包门禁
```

- [ ] **Step 6: 最终差异与空间检查**

```powershell
cargo fmt --all -- --check
git diff --check
git status --short
Get-PSDrive -Name C,D | Select-Object Name,Used,Free
```

若空间不足，只清理本任务专用 Cargo target，并在账本记录；保留所有测试日志、报告和包哈希。

- [ ] **Step 7: 使用 `gpt-5.6-sol` / `max` 最终审查**

审查范围只包含本计划精确差异与下列问题：

- 多根是否在去重后、BaseCompute 前轮转；
- 单根/重叠根/不匹配路径是否确定；
- 是否错误修改了 `DiskReadScheduler` 公平算法；
- 退出 PID 是否可能留下世代基线或中止采样；
- sidecar 快照后外部变化是否仍被拒绝；
- 逐盘 waiting/active 是否在取消、错误、复合盘和 Drop 路径恰好归零；
- 正式包是否仍只含允许的四个顶层 EXE。

发现问题时只修复审查指出的精确缺陷并重跑受影响门禁；不启动新一轮广泛重构。

- [ ] **Step 8: 写最终裁决**

账本必须分别给出：

- `SAMPLER_RACE_FIXED`；
- `RESULT_EXPORT_OPEN_ORDER_FIXED`；
- `MULTI_ROOT_REQUEST_VISIBILITY_FIXED`；
- `PER_DISK_TELEMETRY_COMPLETE`；
- `SYNTHETIC_REGRESSION_GATE`；
- `PACKAGE_STRUCTURE_GATE`；
- `REAL_MEDIA_ACCEPTANCE=NOT_RUN`；
- `DEPLOYMENT=NON_DEPLOYABLE`，直到 Worker 崩溃/ACK 风险独立关闭且用户授权部署。

---

## Implementation Order

严格按以下依赖执行：

```text
Task 1 基线
  ├─ Task 2 采样器可靠性 ───────────────┐
  ├─ Task 3 导出器时序 ─────────────────┤
  └─ Task 4 多根顺序 → Task 5 Node 接入 ┤
                        Task 6 协议 → Task 7 RAII 遥测
                                             │
                                             ▼
                                      Task 8 报告投影
                                             │
                                             ▼
                                      Task 9 最终门禁
```

Task 2、3、4 可独立实现；Task 5 依赖 Task 4，Task 7 依赖 Task 6，Task 8 依赖 Task 2/6/7，Task 9 依赖全部前置任务。

## Completion Criteria

- 退出 Worker 的受控 fixture 不再中止 `Write-SystemSample`，健康 PID/CPU 核/磁盘样本仍写入。
- 首次只读打开后的 sidecar 快照导出成功，快照后的外部变化仍稳定拒绝。
- 1,001 个 H 项 + 3 个 I 项的受控 BaseCompute fixture 在首个 Hash 窗口同时出现两根。
- 现有 DiskReadScheduler 硬上限、公平、复合盘、老化和 Drop 测试全部通过。
- 逐盘 waiting/active/granted/released 守恒，旧协议兼容。
- 所有定向 Rust/PowerShell 门禁、三轮合成 benchmark 和正式包校验通过。
- 未再次运行真实媒体，未触碰 `I:\Tool`，未部署。
- Worker 崩溃/ACK 风险未关闭前不宣称生产可用。
