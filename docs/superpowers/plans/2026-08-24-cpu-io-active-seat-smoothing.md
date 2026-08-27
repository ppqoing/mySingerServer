# Rust V2 CPU / 磁盘 I/O 在途容量平滑 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不引入 Node 代读线程池、不调整磁盘类型识别、不处理 Worker 崩溃恢复的前提下，实现方案 B 的活动 seat、渐进 Hash 补位和 `2W` 解码信用，消除可归因于架构的 CPU / 磁盘 I/O 资源气泡，并用固定基准和真实媒体 A/B 硬门禁决定是否允许后续发布。

**Architecture:** 保留 Worker 直接打开媒体文件的现有边界。`DiskReadScheduler` 仍由单 actor 原子授予全局及每盘许可，但改为按 Hash / Media 实际 active seat 压力调度；`BaseCompute` 使用显式 output credit、refill token、decode credit 和 RAII 阶段守卫实现有界、渐进、可守恒的流水线。协议仅追加运行时观测字段；测试客户端、SQLite 只读结果导出器和 A/B 工具全部位于正式 ZIP 之外。

**Tech Stack:** Rust 2024、Tokio 1.53、Prost 0.14、SQLite / rusqlite 0.40、Slint 1.17、PowerShell 7、Windows x64 进程与物理盘性能采样、SHA-256、规范化 JSONL。

**Spec:** [Rust V2 CPU / 磁盘 I/O 在途容量平滑设计（方案 B）](../specs/2026-08-24-cpu-io-active-seat-smoothing-design.md)

## Global Constraints

- 当前实施工作树固定为 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup`；所有源码、测试、计划和证据路径都以该根为准。
- 当前工作树已有大量用户未提交修改，且 `base_compute.rs`、相关测试与基准仍是未跟踪文件。每项任务开始前保存 `git status --short` 和目标文件 SHA-256；禁止 `git reset`、`git clean`、整仓 `git add -A` 或覆盖已有改动。
- 对任务开始前已经 dirty 的目标文件，不直接提交整文件。先在实施账本记录 `COMMIT_DEFERRED_DIRTY_BASELINE`；只有能够用精确 hunk 证明 staged 内容全部属于本任务时才提交。新计划文档和全新、无依赖歧义的文件可使用精确 pathspec 单独提交。
- 新增或修改的方法、类型、字段和关键变量必须使用中文注释说明用途、调用时机、所有权边界和释放逻辑；实现保持小函数、显式状态和 RAII，不复制第二套调度器或流水线。
- Node 继续只负责 Hash 读取；媒体探测、抽帧和解码仍由 `worker.exe` 直接读取源文件。不得增加 Node 读取代理、共享文件块服务或新读取线程池。
- 不修改 SSD / HDD / Unknown 识别算法和默认值，不处理 Worker 原生崩溃根因，不调整特征算法阈值，不引入 GPU / 硬件解码。
- 生产媒体根只读。A/B 前后仅允许读取路径、长度和 mtime；数据库、缓存、日志、临时文件和证据全部写入 `C:\tmp` 隔离根。
- 不读取、写入、打包到或替换 `I:\Tool\mySingerServer-rust-v2-win-x64`。本计划最终只生成隔离测试包和门禁报告，生产部署需要后续单独授权。
- 正式发布 ZIP 顶层 EXE 白名单保持 `desktop.exe`、`node.exe`、`worker.exe`、`Everything.exe` 四项。`runtime_acceptance.exe` 和 `export_scan_result_summary.exe` 永远作为外置测试工具，不进入正式 ZIP。
- Protobuf 只允许追加字段和消息，不改已有 tag、含义或 Envelope。旧 Node 缺少新字段时必须保持 `None`，Desktop 显示 `—`、NDJSON 写 `null`，禁止从旧聚合指标反推。
- Scheduler 测试使用 permit gate、`Notify`、actor barrier、显式 Drop 和 `poll_once`；不得依赖短时间 `sleep` 或源码字符串匹配。
- 基础计算的成功、单项失败、取消、媒体许可失败、派发失败、Started 前终态和接收端关闭都必须证明 credit / permit / ownership 恰好释放一次。
- 任务快照目标周期固定 1,000 ms；系统采样固定 2,000 ms。报告使用真实时间戳和 `sample_interval_ms` 加权，不用固定样本数代替时长。
- 固定基准以 `elapsed_ms` 三轮中位数裁决 15% 门禁；局部阶段耗时、吞吐和相关系数只能解释，不能替代总墙钟。
- 真实媒体顺序固定 `A,B,B,A,A,B`。六轮都使用同一外置采集工具版本、同一只读媒体根和相同配置，但数据库、缓存、日志、临时目录和证据根互相隔离。
- 所有代码步骤遵循 RED → GREEN → 定向回归。每个 checkpoint 先运行 `git diff --check` 和精确测试，再记录实际命令、退出码、产物 SHA-256 与变更文件。

---

## File Structure

### 调度与基础计算

- Modify: `crates/node-engine/src/io/scheduler.rs`
  保存全局 / 每盘 total 与 class active，执行名义 seat、借用、自然收回、`T=1` 轮换和唯一老化保留。
- Modify: `crates/node-engine/tests/disk_scheduler.rs`
  以确定性许可测试覆盖活动 seat、复合盘、老化、取消和 Drop 守恒。
- Create: `crates/node-engine/src/scan/base_flow_control.rs`
  封装 Hash 阶段守卫、媒体许可阶段守卫、output / decode credit、refill token 和容量计算。
- Modify: `crates/node-engine/src/scan/mod.rs`
  注册私有 `base_flow_control` 模块，不扩大公共 API。
- Modify: `crates/node-engine/src/scan/pipeline.rs`
  在真实 Hash 许可取得边界发送读取阶段信号；Worker 媒体读取边界保持不变。
- Modify: `crates/node-engine/src/scan/base_compute.rs`
  接入单步 Hash 补位、两阶段 content 解析、`2W` decode credit、媒体 ready、派发中、Started 待归并和 item 完成时延。
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
  将 item 起点到 Applied ACK 的单调耗时带回协调器，不把时间写入 SQLite。
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
  覆盖 output/refill/decode ownership、事件边界、错误和取消。
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`
  保留固定 fixture，并增加新 ownership 最终归零断言。
- Reference: `crates/node-engine/benches/base_compute_pipeline.rs`
  固定基准入口不改变数据集和输出字段。

### 协议、运行详情与 Desktop

- Modify: `proto/node.proto`
  追加 `RuntimeOwnershipMetrics`、14 个 ownership 字段、1 个 refill control-state 字段和 item 完成时延字段。
- Modify: `crates/protocol/tests/runtime_tasks_wire.rs`
  固定新 tag、round-trip 和旧消息缺字段行为。
- Modify: `crates/node-engine/src/runtime_tasks.rs`
  保存 ownership current / peak / capacity 与 item 完成时延直方图。
- Modify: `crates/node-engine/tests/runtime_tasks.rs`
  验证容量、峰值、终态清零和缺失字段。
- Modify: `crates/desktop-core/examples/runtime_acceptance.rs`
  输出 1 秒任务快照、真实采样间隔、persistent task ID 和新指标。
- Modify: `crates/desktop-core/tests/runtime_acceptance_contract.rs`
  固定 NDJSON 契约和旧 Node `null` 兼容。
- Modify: `crates/desktop-ui/src/models.rs`
  按组格式化新指标，缺失值显示 `—`。
- Modify: `crates/desktop-ui/ui/pages/task-center-page.slint`
  仅在实际需要时增加指标卡可滚动内容高度。
- Modify: `crates/desktop-ui/tests/bindings_contract.rs`
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`

### 正确性导出与 Windows A/B

- Modify: `crates/node-store/Cargo.toml`
  增加仅 `acceptance-tools` feature 使用的 serde / JSON / SHA-256 依赖和 example 声明。
- Modify: `crates/node-store/src/lib.rs`
- Create: `crates/node-store/src/result_summary.rs`
  停止 Node 后以 SQLite read-only + query-only 导出规范结果。
- Create: `crates/node-store/examples/export_scan_result_summary.rs`
  外置测试 CLI，不进入正式包。
- Create: `crates/node-store/tests/result_summary_export.rs`
  覆盖排序、特征哈希、路径安全和只读性。
- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
  接受外置客户端 / 导出器、显式 evidence / report 路径和运行元数据。
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
  生成单轮加权指标、守恒检查和证据完整性结论。
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`
- Create: `tests/windows/Test-RustV2ResultSummary.ps1`
- Create: `scripts/build-rust-v2-cpu-io-test-package.ps1`
  调用正式 builder 后立刻复制唯一 A/B 包，并把外置工具和元数据放在 ZIP 外。
- Create: `tests/windows/Measure-RustV2CpuIoAb.ps1`
  固定执行六轮顺序和隔离布局。
- Create: `tests/windows/New-RustV2CpuIoAbReport.ps1`
  聚合六轮、计算基线分桶和全部硬门禁。
- Create: `tests/windows/Test-RustV2CpuIoAbReport.ps1`
- Create: `docs/verification/2026-08-24-cpu-io-active-seat-smoothing-implementation.md`
  保存 checkpoint、A0 / A / B 指纹、测试和最终裁决。

---

### Task 1: 冻结 dirty 基线、A0 固定基准和实施账本

**Files:**

- Create: `docs/verification/2026-08-24-cpu-io-active-seat-smoothing-implementation.md`
- Reference: `docs/superpowers/specs/2026-08-24-cpu-io-active-seat-smoothing-design.md`
- Reference: `crates/node-engine/benches/base_compute_pipeline.rs`
- Reference: `crates/node-engine/tests/base_compute_utilization.rs`

**Interfaces:**

- Produces: `baseline-head.txt`、`baseline-status.txt`、`baseline.patch`、`baseline-files.sha256`、A0 三轮诊断输出、A0 benchmark EXE 和 SHA-256。
- A0 只用于量化观测代码开销，不参与最终 15% 硬门禁；硬门禁使用相同最终 `Cargo.lock` 的 instrumented A 与 B。
- Produces: `source_tree_fingerprint`，定义为全部 Git 可见 tracked + untracked、非 ignored 文件的有序内容哈希清单 SHA-256；HEAD、status 与 patch 另存并在 metadata 中独立绑定。
- Does not produce: 代码修改、发布包或生产部署。

- [ ] **Step 1: 建立隔离证据根并记录工作树**

运行：

```powershell
$repo = 'D:\code\mySingerServer\.worktrees\rust-v2-media-dedup'
$evidence = 'C:\tmp\rust-v2-cpu-io-ab\benchmark\A0'
New-Item -ItemType Directory -Path $evidence -Force | Out-Null
Set-Location $repo
git rev-parse HEAD | Set-Content -LiteralPath (Join-Path $evidence 'baseline-head.txt') -Encoding utf8NoBOM
git status --short | Set-Content -LiteralPath (Join-Path $evidence 'baseline-status.txt') -Encoding utf8NoBOM
git diff --binary | Set-Content -LiteralPath (Join-Path $evidence 'baseline.patch') -Encoding utf8NoBOM
```

对 Git 可见的 tracked + untracked、非 ignored 文件生成统一源码清单：

```powershell
$manifestPath = Join-Path $evidence 'baseline-files.sha256'
$sourceRows = git ls-files --cached --others --exclude-standard |
    Sort-Object |
    ForEach-Object {
        $relativePath = $_
        $hash = (Get-FileHash -LiteralPath $relativePath -Algorithm SHA256).Hash.ToLowerInvariant()
        "$hash  $($relativePath.Replace('\','/'))"
    }
[IO.File]::WriteAllText(
    $manifestPath,
    (($sourceRows -join "`n") + "`n"),
    [Text.UTF8Encoding]::new($false)
)
$sourceTreeSha256 = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
```

`source_tree_fingerprint` 就是该 manifest 的小写 SHA-256。不得把 ignored `target`、运行 `data` 或外部媒体加入清单。

- [ ] **Step 2: 写实施账本头部**

账本必须记录：

- 当前 HEAD `0e796fca` 的完整值；
- dirty / untracked 目标文件；
- 方案规格路径与 SHA-256；
- Rust、Cargo、PowerShell、Windows 版本；
- `Cargo.lock` SHA-256；
- `COMMIT_DEFERRED_DIRTY_BASELINE` 规则；
- 明确 `I:\Tool` 未触碰。

- [ ] **Step 3: 运行 A0 定向正确性基线**

运行：

```powershell
cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_utilization --locked -- --test-threads=1
```

若已有测试失败，原样记录为 `A0_PREEXISTING_FAILURE`；不得在本任务顺手修复。

- [ ] **Step 4: 使用冻结命令运行 A0 三轮基准**

三轮均运行：

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-cpu-io-a0-target'
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

每轮 stdout 单独保存为 `a0-run-01.txt`、`a0-run-02.txt`、`a0-run-03.txt`，解析全部固定字段；复制实际 benchmark EXE 到 `C:\tmp\rust-v2-cpu-io-ab\artifacts\A0\` 并记录 SHA-256。

- [ ] **Step 5: 验证 fixture 没有漂移**

核对并写入账本：

- seed 为 `0x2026_08_23_C0DE_0000`；
- 文件数为 4；
- 固定清单为 4 KiB、8 KiB、64 MiB、96 MiB；
- `total_threads=2`、每盘上限 2、`PipelineLimits::new(4,2)`、Worker 数 2；
- 三轮输出都含 `elapsed_ms` 和 `throughput_files_per_second`。

账本明确标记 `A0_DIAGNOSTIC_ONLY`；后续测试工具依赖可能改变 lockfile 的本地 package dependency 列表，因此不得用 A0 代替同 lockfile 的 A/B 门禁。

- [ ] **Step 6: Checkpoint**

运行 `git diff --check`。本任务只允许提交新实施账本：

```powershell
git add -- docs/superpowers/plans/2026-08-24-cpu-io-active-seat-smoothing.md docs/verification/2026-08-24-cpu-io-active-seat-smoothing-implementation.md
git diff --cached --name-only
```

若 staged 列表包含其他路径，立即取消本次 stage，不提交。推荐提交说明：`docs: start cpu io smoothing implementation ledger`。

---

### Task 2: 追加 ownership 协议和运行时 registry

**Files:**

- Modify: `proto/node.proto`
- Modify: `crates/protocol/tests/runtime_tasks_wire.rs`
- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-engine/tests/runtime_tasks.rs`

**Interfaces:**

- Produces: `proto::RuntimeOwnershipMetrics`。
- Produces: `RuntimePipelineOwnership`、`RuntimePipelineControl`、`RuntimeTaskReporter::update_ownership_nowait` 和 `update_control_state_nowait`。
- Produces: `RuntimeTaskReporter::record_item_completion_latency_nowait`。
- Existing Envelope and WorkerEnvelope tags remain unchanged.

- [ ] **Step 1: 写协议 RED 测试**

在 `runtime_tasks_wire.rs` 固定以下新消息和 tag：

```proto
message RuntimeOwnershipMetrics {
  optional uint64 current = 1;
  optional uint64 peak = 2;
  optional uint64 capacity = 3;
}
```

`RuntimePipelineMetrics` 的追加字段固定为：

| tag | 字段 |
|---:|---|
| 12 | `hash_waiting_permit` |
| 13 | `hash_reading` |
| 14 | `hash_completed_unjoined` |
| 15 | `media_permit_waiting` |
| 16 | `media_acquire_ready` |
| 17 | `media_permit_ready` |
| 18 | `worker_dispatching` |
| 19 | `worker_start_pending` |
| 20 | `worker_decode` |
| 21 | `worker_feature` |
| 22 | `worker_result_wait` |
| 23 | `worker_phase_unknown` |
| 24 | `content_output_credit_owned` |
| 25 | `hash_refill_token_available` |
| 26 | `decode_credit_owned` |
| 27 | `item_completion_latency`，类型为 `RuntimeLatencyHistogram` |

测试必须覆盖 descriptor、14 个真实 ownership + 1 个 control-state round-trip、新时延字段、旧字节流解码后全部新字段为 `None`，以及 `None` 与 `Some(0)` 的区别。协议为保持统一投影仍让字段 25 使用 `RuntimeOwnershipMetrics` 消息形状，但领域语义必须是 control-state，不能把 token 当作 RAII ownership。

- [ ] **Step 2: 运行协议 RED**

```powershell
cargo test -p dedup-protocol --test runtime_tasks_wire --locked
```

Expected: 新消息和 tag 尚不存在。

- [ ] **Step 3: 追加 Protobuf 字段**

只修改 `RuntimePipelineMetrics`，不新增 Envelope 分支。14 个 ownership 与字段 25 的 control-state 复用 `RuntimeOwnershipMetrics` wire shape；item 时延复用已有 `RuntimeLatencyHistogram`。

- [ ] **Step 4: 写 registry RED 测试**

增加以下行为测试：

- 第一次发布 ownership 时建立 capacity；
- current 更新峰值，current 可回落到 0；
- current 超过 capacity 返回 `CapacityExceeded`；
- 未发布的 ownership 在协议快照中保持 `None`；
- 任务终态把 current 清零但保留 peak；
- control-state 超容量失败、输入耗尽显式归零且不参与 Drop / ownership 下溢检查；
- item 完成时延累计 count、P50、P95、P99 和 max；
- 重复终态清理不下溢。

- [ ] **Step 5: 实现 registry 类型**

新增：

```rust
/// 基础计算中必须与真实所有权一一对应的细分状态。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimePipelineOwnership {
    HashWaitingPermit,
    HashReading,
    HashCompletedUnjoined,
    MediaPermitWaiting,
    MediaAcquireReady,
    MediaPermitReady,
    WorkerDispatching,
    WorkerStartPending,
    WorkerDecode,
    WorkerFeature,
    WorkerResultWait,
    WorkerPhaseUnknown,
    ContentOutputCreditOwned,
    DecodeCreditOwned,
}

/// 不持有外部资源、仅描述协调器控制状态的运行指标。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimePipelineControl {
    HashRefillTokenAvailable,
}

/// 一个细分状态的当前值、历史峰值和硬容量。
struct OwnershipMetricsEntry {
    current: u64,
    peak: u64,
    capacity: u64,
}
```

`PipelineMetricsEntry` 初始不预填 ownership 或 control-state。只有生产者首次调用 `update_ownership_nowait(kind,current,capacity)` 或 `update_control_state_nowait(kind,current,capacity)` 后才创建条目；这样观测基线 A 可以真实缺少尚未实现的 credit，而不会伪造为 0。`OwnershipMetricsEntry` 服从资源取得 / Drop 与下溢规则；独立 `ControlMetricsEntry` 只保存 current / peak / capacity，不参与 ownership 守恒或 RAII 释放，任务终态及权威输入耗尽时由 refill controller 显式发布 0。

- [ ] **Step 6: 运行 GREEN 和回归**

```powershell
cargo test -p dedup-protocol --test runtime_tasks_wire --locked
cargo test -p dedup-node-engine --features test-hooks --test runtime_tasks --locked -- --test-threads=1
cargo check -p dedup-node-engine --locked
```

- [ ] **Step 7: Checkpoint**

检查四个目标文件的基线 SHA、`git diff --check` 和精确 diff。推荐提交说明：`telemetry: add pipeline ownership contract`；目标文件基线已 dirty 时写 `COMMIT_DEFERRED_DIRTY_BASELINE`。

---

### Task 3: 增加不改变调度策略的精确阶段观测

**Files:**

- Create: `crates/node-engine/src/scan/base_flow_control.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/scan/base_persistence.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`

**Interfaces:**

- Produces: `HashPhaseGuard`、`HashReadStartedSignal`、`MediaAcquirePhaseGuard`。
- Produces exact A-observable fields 12–23 and item completion latency.
- Deliberately does not publish fields 24–26 yet.
- Keeps current burst refill, current scheduler selection and current `queue_capacity + W` decode bound unchanged for instrumented baseline A.

- [ ] **Step 1: 写阶段守卫 RED 单元测试**

在 `base_flow_control.rs` 的私有测试中验证：

- Hash guard 创建后为 waiting；
- 取得真实许可后 waiting → reading；
- future 返回后 reading → completed-unjoined；
- 协调器归并或取消后全部归零；
- media future 完成错误、`None` 和 `Some(permit)` 都进入 ready；
- 只有 `Some(permit)` 进入 permit-ready 子集；
- Drop 可重复路径不下溢。

- [ ] **Step 2: 定义阶段守卫**

实现共享原子状态，守卫 Drop 根据最后状态精确递减。公共读取 trait 只追加带默认实现的方法：

```rust
/// Hash 读取器取得真实磁盘许可后调用的一次性阶段信号。
#[doc(hidden)]
#[derive(Clone)]
pub struct HashReadStartedSignal {
    inner: std::sync::Weak<HashPhaseInner>,
}

/// 单个 Hash future 的互斥阶段。
#[repr(u8)]
enum HashPhase {
    WaitingPermit = 0,
    Reading = 1,
    CompletedUnjoined = 2,
}

/// 三个 Hash 阶段的共享原子计数。
struct HashPhaseCounters {
    waiting_permit: std::sync::atomic::AtomicUsize,
    reading: std::sync::atomic::AtomicUsize,
    completed_unjoined: std::sync::atomic::AtomicUsize,
}

/// 守卫和读取信号共享的状态；计数器归任务协调器所有。
struct HashPhaseInner {
    state: std::sync::atomic::AtomicU8,
    counters: Arc<HashPhaseCounters>,
}

pub trait PipelineFileReader: Clone + Send + Sync + 'static {
    type Lease: Send + 'static;

    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>;

    /// 默认测试读取器在 future 开始时进入 reading；生产读取器覆盖为许可取得后进入。
    fn read_with_phase(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
        started: HashReadStartedSignal,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>> {
        let future = self.read(scanned, cancellation);
        Box::pin(async move {
            started.mark_reading();
            future.await
        })
    }
}
```

代码块只展示现有 `read` 与新增 `read_with_phase`；trait 中既有 `acquire_media_permit` 和 `take_physical_disk_id` 原签名、默认值与语义必须原样保留。

`ScheduledFileReader` 必须覆盖 `read_with_phase`，在 `acquire_scheduled_permit` 以 `DiskReadClass::HashSequential` 成功取得许可后、`spawn_blocking` 读取开始前调用 `mark_reading()`。

- [ ] **Step 3: 写 BaseCompute 观测 RED 测试**

新增测试：

- `hash_phase_counts_match_join_set_ownership`；
- `media_ready_counts_error_none_and_real_permit`；
- `dispatch_ack_moves_dispatching_to_start_pending`；
- `started_without_authoritative_phase_is_unknown`；
- `worker_phase_events_are_mutually_exclusive`；
- `content_cache_wait_holds_no_hash_media_or_worker_ownership`；
- `item_completion_latency_starts_at_successful_claim_and_ends_at_applied_ack`。

使用现有 controlled WorkerPool 和 persistence gate，不使用短时 sleep。

- [ ] **Step 4: 在当前行为上接入 Hash / media 守卫**

`HashTaskOutput` 携带 `HashPhaseGuard`；future 返回前调用 `mark_completed_unjoined`，协调器处理一项结果后 Drop。`MediaAcquireOutput` 携带 `MediaAcquirePhaseGuard`，future 完成时记录 ready 与是否持有真实 permit，JoinSet 归并后 Drop。

本任务不得把 `drain_ready_hash_results` 改成单项，也不得改变 `fill_hash_tasks` 的 while 补满行为；这些属于候选 B。

- [ ] **Step 5: 接入 Worker 细分状态**

`ActiveBase` 追加权威 phase：

```rust
/// 已启动 Worker 最近一次权威阶段；Started 后未收到阶段事件时为 None。
worker_phase: Option<proto::RuntimeWorkerPhase>,
```

派发调用前发布 `worker_dispatching=1`，ACK 后发布 0 并由 `active(worker_slot=None)` 形成 `worker_start_pending`。`Started` 后设置 slot；尚无 Decode / Feature / ResultWait 时计入 unknown。Idle / Unspecified 也归 unknown，不得猜测为 Decode。

- [ ] **Step 6: 记录 item claim → Applied ACK 时延**

在协调器维护：

```rust
/// 每个活动任务项第一次被 claim 后的单调时刻。
let mut item_started_at = BTreeMap::<String, Instant>::new();
```

新任务和恢复任务都只在 `claim_next_item` 成功返回具体 item 后调用 `item_started_at.entry(item.item_id.clone()).or_insert_with(Instant::now)`；`reserve_scan_path` 只建立持久项，不开始计时。`apply_persist_ack` 对 Applied 成功、失败或取消移除时间并记录时延；Ignored 不进入时延直方图。任务取消收尾必须清空 map。

- [ ] **Step 7: 每次状态迁移后投影守恒**

统一函数 `update_pipeline_ownership` 从真实容器、阶段守卫快照和 `ActiveBase` 计算字段 12–23。它同时执行：

```text
hashing.len = waiting + reading + completed_unjoined
media_acquiring.len = media_waiting + media_ready
media_permit_ready <= media_ready
active.len = start_pending + decode + feature + result_wait + unknown
active(slot=None) = start_pending
active.len + media_acquiring.len + worker_dispatching <= W
```

违反守恒立即返回 `ScanError::Stage1`，不能只写日志。

文件进入本地或远端缓存查询等待容器前，按冻结 `item_id` 调用 `ensure_cache_wait_holds_no_compute_resource`，证明该项已经不在 Hash guard / permit、media acquire / permit 或 Active Worker 身份中；违反时返回带稳定代码 `CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION` 的 `ScanError::Stage1`。运行报告从 failure details 统计该代码，计数必须为 0；这样该硬门禁不依赖任务级聚合 current 猜测某个等待项的资源。

- [ ] **Step 8: 运行 GREEN 和 A 观测回归**

```powershell
cargo test -p dedup-node-engine --features test-hooks --lib scan::base_flow_control --locked
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_utilization --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test scan_runtime_details --locked -- --test-threads=1
```

- [ ] **Step 9: Checkpoint**

确认 A 观测提交没有改动 `scheduler.rs`、`fill_hash_tasks` 批量策略或 decode capacity。推荐提交说明：`telemetry: expose exact base compute phases`；dirty 基线按全局规则延期提交。

---

### Task 4: 更新 1 秒 NDJSON、persistent task ID 和 Desktop 展示

**Files:**

- Modify: `crates/desktop-core/examples/runtime_acceptance.rs`
- Modify: `crates/desktop-core/tests/runtime_acceptance_contract.rs`
- Modify: `crates/desktop-ui/src/models.rs`
- Modify: `crates/desktop-ui/ui/pages/task-center-page.slint`
- Modify: `crates/desktop-ui/tests/bindings_contract.rs`
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`

**Interfaces:**

- Produces: `RuntimeOwnershipSample`。
- Produces: `RuntimeAcceptanceSample.sample_interval_ms`。
- Produces: `RuntimeAcceptanceResult.scan_tasks`、`latest_completed_persistent_task_id` 和 `deadline_cancelled_persistent_task_id`。
- Desktop consumes optional new fields and never invents missing values.

- [ ] **Step 1: 写 runtime client RED 契约**

测试固定：

- `SAMPLE_SECONDS` 从 2 改为 1；
- 1,800 秒目标样本数为 1,800；
- 第一条 `sample_interval_ms=0`，后续使用实际相邻单调时间差；
- 15 个细分字段（14 个 ownership + 1 个 control-state）缺失时序列化为 `null`；
- item completion latency 可选；
- 每次成功创建扫描都追加 persistent / runtime task ID 和最终状态；
- final `runtime_result` 暴露最后一个 completed persistent task ID；
- 到期主动取消的 task 单独记录，不能覆盖最后一个 completed task；
- 1,800 秒内没有任何 completed scan 时，结果正确性证据为 `INCONCLUSIVE`。

- [ ] **Step 2: 实现 NDJSON DTO**

```rust
/// NDJSON 中一个细分所有权指标；旧 Node 缺失时外层 Option 为 None。
#[derive(Clone, Debug, serde::Serialize)]
struct RuntimeOwnershipSample {
    current: Option<u64>,
    peak: Option<u64>,
    capacity: Option<u64>,
}

/// 每个任务快照与上一快照之间的真实单调间隔。
sample_interval_ms: u64,
```

`map_pipeline_metrics` 逐字段映射，不用 queue/resource 推导。`run_acceptance` 保存上一采样 `Instant`；系统 2 秒采样仍由 PowerShell 控制。

最终扫描记录类型固定：

```rust
/// 半小时验收中一次实际创建的持久扫描及其运行终态。
#[derive(Clone, Debug, serde::Serialize)]
struct RuntimeAcceptanceScan {
    persistent_task_id: String,
    runtime_task_id: Option<String>,
    terminal_state: Option<String>,
}
```

- [ ] **Step 3: 运行 runtime client GREEN**

```powershell
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
```

- [ ] **Step 4: 写 Desktop RED 测试**

覆盖：

- 新字段存在时显示 `current/peak/capacity`；
- 缺失时显示 `—`；
- item 完成 P95 有值才显示；
- Desktop 自有任务和旧 Node 不显示伪值；
- accessible label 包含新增文本；
- 指标增加后可在任务详情滚动区域到达。

- [ ] **Step 5: 实现分组展示**

`pipeline_metrics_text` 固定为六组：队列、I/O、Hash / media、Worker phase、credit、吞吐 / item P95。新增 `ownership_metric` 只格式化 optional 值。仅当 offscreen 测试证明裁剪时调整 `task-center-page.slint` 指标区域高度。

- [ ] **Step 6: 运行 Desktop GREEN**

```powershell
cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
```

- [ ] **Step 7: Checkpoint**

推荐提交说明：`telemetry: publish one second runtime ownership samples`。只检查本任务七个文件；已有 UI 重构 hunk 不得被顺带 stage。

---

### Task 5: 实现停止 Node 后的只读结果摘要导出器

**Files:**

- Modify: `crates/node-store/Cargo.toml`
- Modify: `Cargo.lock`
- Modify: `crates/node-store/src/lib.rs`
- Create: `crates/node-store/src/result_summary.rs`
- Create: `crates/node-store/examples/export_scan_result_summary.rs`
- Create: `crates/node-store/tests/result_summary_export.rs`

**Interfaces:**

- Produces: `export_scan_result_summary(database_path, cache_root, task_id, output_path)`。
- Produces: canonical `result-summary.jsonl`、`result-summary-meta.json` 和整体 SHA-256。
- Consumes only: 已停止 Node 的隔离 `data\node\node.db` 和 `data\node\cache`。
- Does not consume: 生产协议、运行时 overall counters 或 `content_id` 作为 A/B 身份。

最终 Rust 接口固定：

```rust
/// 单次导出的证据完整性状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum ResultSummaryStatus {
    Pass,
    Missing,
    Inconclusive,
}

/// 导出文件及整体哈希的稳定返回值。
pub struct ResultSummaryExport {
    pub task_id: String,
    pub task_status: String,
    pub row_count: u64,
    pub missing_count: u64,
    pub inconclusive_count: u64,
    pub status: ResultSummaryStatus,
    pub output_path: PathBuf,
    pub metadata_path: PathBuf,
    pub sha256: String,
}

/// 导出器只报告数据库、文件、JSON、参数和路径安全错误。
#[derive(Debug, thiserror::Error)]
pub enum ResultSummaryError {
    #[error("SQLite 读取失败: {0}")]
    Sqlite(#[from] rusqlite::Error),
    #[error("文件读取或写入失败: {0}")]
    Io(#[from] std::io::Error),
    #[error("JSON 编码失败: {0}")]
    Json(#[from] serde_json::Error),
    #[error("导出参数无效: {0}")]
    InvalidArgument(String),
    #[error("联系表路径越出隔离缓存根")]
    UnsafeArtifactPath,
}

pub fn export_scan_result_summary(
    database_path: &Path,
    cache_root: &Path,
    task_id: &str,
    output_path: &Path,
) -> Result<ResultSummaryExport, ResultSummaryError>
```

- [ ] **Step 1: 增加 acceptance-tools feature 并安全更新 lockfile**

在 `crates/node-store/Cargo.toml` 增加：

```toml
[features]
default = []
acceptance-tools = ["dep:serde", "dep:serde_json", "dep:sha2"]
```

在现有 `[dependencies]` 表中追加：

```toml
serde = { workspace = true, optional = true }
serde_json = { version = "1", optional = true }
sha2 = { workspace = true, optional = true }
```

先保存 `Cargo.lock` 的基线 SHA 和副本，然后仅本步骤允许不带 `--locked` 刷新本地 package dependency 边：

```powershell
$lockEvidence = 'C:\tmp\rust-v2-cpu-io-ab\lockfile'
New-Item -ItemType Directory -Path $lockEvidence -Force | Out-Null
Copy-Item -LiteralPath Cargo.lock -Destination (Join-Path $lockEvidence 'Cargo.lock.before')
cargo check -p dedup-node-store
cargo metadata --format-version 1 --no-deps --locked | Out-Null
git diff -- Cargo.lock
```

只允许 `dedup-node-store` 增加本计划声明的 dependency edge；若解析器升级其他包或覆盖已有用户 lockfile 改动，停止并记录 `LOCKFILE_UPDATE_SCOPE_VIOLATION`。从此处起重新记录 `Cargo.lock` SHA，A 与 B 全部命令都使用这一份最终 lockfile 和 `--locked`。`lib.rs` 只在该 feature 下公开 `result_summary`，正式默认构建不编译导出器。

- [ ] **Step 2: 写导出 RED 测试**

使用临时 SQLite 和缓存根覆盖：

- 结果按 `normalized_path`、`machine_id`、`item_id` 的二进制顺序读取；
- canonical JSONL 只以 `normalized_path` 为 A/B 主身份，拒绝同任务重复 normalized path；
- 两次数据库使用不同 `content_id` 和 `item_id` 时仍得到相同 canonical hash；
- MD5 和媒体类型来自 `contents`；
- 图片 stage1 / stage2、视频 metadata、六个 stage1 slot、六个 stage2 slot 的每个原始字段都影响对应 hash；
- BLOB 以小写十六进制进入 canonical payload，不解码成浮点再编码；
- contact sheet 内容 SHA-256 正确；
- 绝对 contact sheet 路径、`..` 和越出 cache 根的路径被拒绝；
- thumbnail 固定输出 unsupported + null，不冒充 contact sheet；
- running / failed / cancelled、缺内容、缺基础特征、缺视频联系表分别产生明确状态；
- 导出前后数据库文件 SHA-256 相同，且 read-only 连接执行写语句返回 SQLite readonly 错误。

- [ ] **Step 3: 运行 exporter RED**

```powershell
cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export --locked
```

Expected: feature、模块和导出函数尚不存在。

- [ ] **Step 4: 实现只读连接和主查询**

连接固定为：

```rust
/// 以不会创建 schema、WAL 或迁移的方式打开验收数据库。
fn open_read_only_database(path: &Path) -> Result<rusqlite::Connection, ResultSummaryError> {
    let connection = rusqlite::Connection::open_with_flags(
        path,
        rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY
            | rusqlite::OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )?;
    connection.execute_batch("PRAGMA query_only = ON; PRAGMA busy_timeout = 5000;")?;
    Ok(connection)
}
```

主查询固定：

```sql
SELECT
    ti.item_id,
    ti.machine_id,
    ti.normalized_path,
    ti.display_path,
    ti.file_size,
    ti.content_id,
    ti.status,
    ti.stage,
    ti.error,
    c.md5,
    c.file_size,
    c.media_kind,
    c.base_complete
FROM task_items ti
LEFT JOIN contents c ON c.content_id = ti.content_id
WHERE ti.task_id = ?1
ORDER BY ti.normalized_path COLLATE BINARY,
         ti.machine_id COLLATE BINARY,
         ti.item_id COLLATE BINARY
```

`content_id` 只用于继续查询本地表，不能写入 canonical JSONL 或 feature hash。

- [ ] **Step 5: 冻结 canonical 行和 feature hash**

canonical 比较行字段顺序固定：

```rust
/// A/B 比较使用的稳定行；不包含任务 ID、item ID、PID、时间或本地自增 ID。
#[derive(serde::Serialize)]
struct CanonicalResultRow {
    schema_version: u32,
    normalized_path: String,
    status: String,
    file_size: Option<u64>,
    md5: Option<String>,
    media_type: Option<String>,
    base_complete: Option<bool>,
    feature_payloads: FeaturePayloadHashes,
    feature_payload_sha256: Option<String>,
    contact_sheet_sha256: Option<String>,
    thumbnail_sha256: Option<String>,
    thumbnail_state: &'static str,
}

/// 每类数据库 payload 的独立 SHA-256；视频数组固定六个槽位。
#[derive(serde::Serialize)]
struct FeaturePayloadHashes {
    image_stage1: Option<String>,
    image_stage2: Option<String>,
    video_metadata: Option<String>,
    video_frame_stage1: [Option<String>; 6],
    video_frame_stage2: [Option<String>; 6],
}
```

每个 payload 用固定字段顺序 struct 序列化为紧凑 JSON 后计算 SHA-256；`feature_payload_sha256` 再对完整 `FeaturePayloadHashes` 的紧凑 JSON 计算。stage2 不属于基础计算成功的必需条件，但存在时必须参与比较。

- [ ] **Step 6: 冻结状态和文件格式**

导出器状态：

- `PASS`：任务存在且终态，任务项全部 succeeded，内容和该媒体类型必需的基础特征完整，视频联系表存在。
- `MISSING`：数据库已成功只读打开，但指定任务不存在、任务为空，或任务引用的预期内容 / artifact 不存在；仍写出可用行和 metadata。
- `INCONCLUSIVE`：任务仍运行、存在 failed / cancelled / queued item、任务终态与 item 计数矛盾，或内容 / feature 行虽然存在但完成标记和必需 payload 相互矛盾。

数据库无法打开、输出父目录不存在 / 不可写、参数非法或 JSON 写入失败返回 `ResultSummaryError`，CLI 写稳定错误码到 stderr 并以非零退出；这些错误无法伪造一份 `MISSING` 摘要，harness 将其作为证据缺失处理。

thumbnail 当前没有独立 schema / artifact，固定：

```json
{"thumbnail_sha256":null,"thumbnail_state":"unsupported_no_thumbnail_artifact"}
```

这不是 missing。JSONL 使用 UTF-8、紧凑 JSON、每行单个 LF；整体 SHA-256 对包含最终 LF 的完整字节流计算。诊断 metadata 单独保存 task ID、item ID、错误、计数和状态，不参与 A/B canonical hash。

- [ ] **Step 7: 实现 CLI 和稳定输出**

在 `Cargo.toml` 追加 example 声明：

```toml
[[example]]
name = "export_scan_result_summary"
required-features = ["acceptance-tools"]
```

CLI 只接受：

```text
--database C:\tmp\rust-v2-cpu-io-ab\runs\A-1\data\node\node.db
--cache-root C:\tmp\rust-v2-cpu-io-ab\runs\A-1\data\node\cache
--task-id 00000000-0000-0000-0000-000000000001
--output C:\tmp\rust-v2-cpu-io-ab\runs\A-1\evidence\result-summary.jsonl
```

解析器不增加 clap。stdout 固定输出：

```text
RESULT_SUMMARY_STATUS=PASS|MISSING|INCONCLUSIVE
RESULT_SUMMARY_PATH=C:\tmp\rust-v2-cpu-io-ab\runs\A-1\evidence\result-summary.jsonl
RESULT_SUMMARY_SHA256=64位小写十六进制SHA256
RESULT_SUMMARY_ROW_COUNT=非负十进制整数
RESULT_SUMMARY_MISSING_COUNT=非负十进制整数
RESULT_SUMMARY_INCONCLUSIVE_COUNT=非负十进制整数
RESULT_SUMMARY_TASK_ID=00000000-0000-0000-0000-000000000001
```

- [ ] **Step 8: 运行 GREEN 和构建测试工具**

```powershell
cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export --locked
cargo build -p dedup-node-store --features acceptance-tools --example export_scan_result_summary --release --locked --target x86_64-pc-windows-msvc
```

- [ ] **Step 9: Checkpoint**

推荐提交说明：`test-tools: export canonical scan result summaries`。确认正式 builder 和 verifier 未修改，example EXE 未复制到 `dist-rust-v2`。

---

### Task 6: 改造单轮 Windows harness 和报告

**Files:**

- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`
- Create: `tests/windows/Test-RustV2ResultSummary.ps1`

**Interfaces:**

- Measure adds: `-AcceptanceClientPath`、`-ResultExporterPath`、`-EvidenceRoot`、`-ReportPath`、`-Variant`、`-RunIndex`、`-SourceRevision`、`-SourceTreeSha256`、`-PackagePath`、`-PackageSha256`。
- `ReleaseRoot` validates only the formal four EXEs and FFmpeg DLLs.
- Exporter runs only after Node and all Workers exit.
- Per-run report produces `PASS`、`FAIL` or `INCONCLUSIVE`，不承担六轮最终裁决。

- [ ] **Step 1: 写 harness 输入 RED 测试**

将 fixture 从 release 根移除 `runtime_acceptance.exe`，改为两个外置文件。测试必须拒绝：

- release 缺任一正式 EXE / DLL；
- acceptance client 或 exporter 缺失；
- 任一工具路径位于 formal ZIP 解压根内；
- duration 小于 1,800；
- system sample 不是 2 秒；
- variant 不是 A / B；
- run index 不在 1..3；
- evidence / report 路径会覆盖另一次 run。

- [ ] **Step 2: 修改输入和复制边界**

`Copy-RuntimeAcceptanceRelease` 只复制 `desktop/node/worker/Everything` 与五个 FFmpeg DLL。外置 acceptance client 和 exporter 保留原绝对路径，不复制进 release root；运行根仅可复制 acceptance client 到独立 `tools` 子目录，且该目录不得用于 formal ZIP 校验。

- [ ] **Step 3: 增加显式隔离布局**

外层传入 evidence 时固定：

```text
C:\tmp\rust-v2-cpu-io-ab\runs\A-1\
  release\
  data\node\
  logs\
  cache\
  temp\
  evidence\
    runtime.ndjson
    system.ndjson
    media-before.json
    media-after.json
    result-summary.jsonl
    result-summary-meta.json
    harness-result.json
    report.md
```

`New-RuntimeAcceptanceLayout` 对 standalone 仍可生成 GUID，但不得把报告写入共享 docs 默认文件。

- [ ] **Step 4: 扩展 harness-result 元数据**

写入有序 JSON：

```powershell
$harnessResult = [ordered]@{
    schema_version = 2
    variant = $Variant
    run_index = $RunIndex
    source_revision = $SourceRevision
    source_tree_sha256 = $SourceTreeSha256
    package_path = $PackagePath
    package_sha256 = $PackageSha256
    release_root = $layout.Release
    config_sha256 = $configSha256
    package_manifest_sha256 = $packageManifestSha256
    media_before_sha256 = $mediaBeforeSha256
    media_after_sha256 = $mediaAfterSha256
    result_summary_path = $resultSummary.Path
    result_summary_sha256 = $resultSummary.Sha256
    result_summary_status = $resultSummary.Status
    result_summary_task_id = $resultSummary.TaskId
    result_summary_missing_count = $resultSummary.MissingCount
    result_summary_inconclusive_count = $resultSummary.InconclusiveCount
}
```

- [ ] **Step 5: 冻结停止与导出顺序**

顺序必须是：

1. 启动隔离 Node；
2. 启动外置 acceptance client；
3. 写 runtime / system NDJSON；
4. 等待 client 终态；
5. 保存 media-after；
6. 请求 Node 退出并等待 Node / Worker 全部退出；
7. 从最后一条 `runtime_result.latest_completed_persistent_task_id` 取得最后一个成功完成的 task ID；到期主动取消 ID 只写诊断；
8. 运行 exporter；
9. 解析固定 stdout；
10. 写 harness-result；
11. 生成本轮 report。

缺 task ID、exporter 非零退出或摘要缺失都写入证据，并把单轮状态设为 `INCONCLUSIVE`；不得回退到 overall counters 证明结果相等。

- [ ] **Step 6: 写 per-run report RED fixture**

fixture 覆盖：

- runtime 1 秒、system 2 秒、不规则实际间隔；
- 15 个细分字段的 current / peak / capacity，并明确 refill token 不进入 ownership 求和；
- A 基线允许字段 24–26 为 null，但字段 12–23 和字段 27 必须存在；
- B 的字段 12–26 全部必需；
- Hash、media、active、Worker admission 和 B credit 守恒；
- 最大 runtime gap、最大 system gap；
- finalization 阶段按 `ComputeBaseFeatures` stage 终态分开；
- result summary PASS / MISSING / INCONCLUSIVE；
- 最后一个 completed scan 被导出，到期主动取消 scan 只作诊断且不误报为任务失败；
- 除到期主动取消外，任一 failed / cancelled scan 都判 FAIL；
- `CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION` fixture 必须判 FAIL，并在报告列出 `cache_wait_resource_ownership_violations`；
- media manifest 改变；
- Node unexpected exit、任务失败和 ownership 越界。

同时保留现有 per-run 门禁：有效 Worker 大于 1 时必须观察到并发活动，多物理盘时必须观察到重叠读取，不能重复报告同一任务项失败，必须至少完成一项，execution config / pipeline metrics 不能缺失。

- [ ] **Step 7: 实现真实时间加权**

报告优先使用 `utc_unix_ms` 相邻差，使用 `sample_interval_ms` 交叉校验；不再按固定 1 秒或旧 `elapsed_seconds` 推断。首样本权重为 0。相邻 runtime gap 大于 2,500 ms、system gap 大于 6,000 ms 时标记 `INCONCLUSIVE`。

- [ ] **Step 8: 运行 PowerShell GREEN**

```powershell
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2ResultSummary.ps1
```

Expected markers:

```text
RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_PASS
RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS
RUST_V2_RESULT_SUMMARY_WIRING_PASS
```

- [ ] **Step 9: Checkpoint**

推荐提交说明：`test: isolate runtime acceptance tools and evidence`。`scripts/build-release.ps1` 和 formal verifier 不在本任务修改范围。

---

### Task 7: 实现 test-only 包、六轮编排和 A/B 聚合报告

**Files:**

- Create: `scripts/build-rust-v2-cpu-io-test-package.ps1`
- Create: `tests/windows/Measure-RustV2CpuIoAb.ps1`
- Create: `tests/windows/New-RustV2CpuIoAbReport.ps1`
- Create: `tests/windows/Test-RustV2CpuIoAbReport.ps1`
- Reference: `scripts/build-release.ps1`
- Reference: `scripts/verify-release.ps1`
- Reference: `tests/windows/Test-RustV2Package.ps1`

**Interfaces:**

- Test package builder consumes a formal package and immediately archives a unique A or B copy.
- A/B orchestrator enforces exactly `A,B,B,A,A,B`。
- Aggregate report consumes six evidence roots plus A/B benchmark evidence and produces one final status.

- [ ] **Step 1: 写 test package builder RED 测试夹具**

在 `Test-RustV2CpuIoAbReport.ps1` 的 setup 中验证：

- variant A / B 的 formal ZIP 名称唯一；
- 第二次 formal build 不覆盖已经归档的 A；
- metadata 绑定 source revision、source tree SHA、formal ZIP SHA、manifest SHA 和 config SHA；
- acceptance client / exporter 只能出现在外置 tools metadata，不能进入 ZIP；
- metadata 明确 `test_only=true`、`deployable=false`。

- [ ] **Step 2: 实现 test-only builder**

参数固定：

```powershell
param(
    [ValidateSet('A','B')][string]$Variant,
    [string]$CargoTargetDir,
    [string]$OutputRoot = 'C:\tmp\rust-v2-cpu-io-ab\packages',
    [string]$SourceRevision,
    [string]$SourceTreeSha256,
    [string]$AcceptanceClientPath,
    [string]$ResultExporterPath
)
```

脚本调用 `scripts\build-release.ps1` 和 `scripts\verify-release.ps1`；成功后立即复制 ZIP、sidecar 并解压到 `$OutputRoot\$Variant\release`。它只写 test metadata，不改 formal manifest。

- [ ] **Step 3: 写 A/B order 与证据绑定 RED 测试**

聚合 fixture 必须拒绝：

- 顺序不是 A,B,B,A,A,B；
- A 三轮 package SHA 不同或 B 三轮 package SHA 不同；
- 同一 variant 三轮的 source fingerprint 不一致，或任一 package metadata 不能绑定自身 source fingerprint；A 与 B 源码本来就应不同，不要求两者 source fingerprint 相等；
- A / B 的 `Cargo.lock`、工具链、构建配置、运行 config 或媒体 manifest 不匹配；
- 六个 report/evidence 路径重复；
- result summary 缺失或 canonical SHA 不一致；
- 系统样本找不到 2,500 ms 内之前最近的任务快照；
- 生产时长覆盖低于 95%；
- baseline 非零 I/O 样本为空。

- [ ] **Step 4: 实现六轮 orchestrator**

`Measure-RustV2CpuIoAb.ps1` 参数契约固定：

```powershell
param(
    [Parameter(Mandatory)][string]$MediaRoot,
    [ValidateRange(1800,86400)][int]$DurationSeconds = 1800,
    [ValidateSet(2)][int]$SampleSeconds = 2,
    [Parameter(Mandatory)][string]$BaselineReleaseRoot,
    [Parameter(Mandatory)][string]$CandidateReleaseRoot,
    [Parameter(Mandatory)][string]$BaselineMetadataPath,
    [Parameter(Mandatory)][string]$CandidateMetadataPath,
    [Parameter(Mandatory)][string]$AcceptanceClientPath,
    [Parameter(Mandatory)][string]$ResultExporterPath,
    [Parameter(Mandatory)][string]$OutputRoot,
    [ValidateRange(1,256)][int]$WorkerCount = 12,
    [ValidateRange(1,256)][int]$HddThreadsPerDisk = 1,
    [ValidateRange(1,256)][int]$SsdThreadsPerDisk = 16,
    [ValidateRange(1,256)][int]$UnknownThreadsPerDisk = 1,
    [ValidateRange(1,256)][int]$TotalReadThreads = 16,
    [ValidateRange(0,255)][int]$ReservedCores = 1,
    [bool]$LibraryOnly = $true
)
```

输入校验先解析 A / B metadata，验证各自 release、formal package / manifest / source SHA、相同最终 `Cargo.lock`、相同工具 SHA 和相同运行 config；`OutputRoot` 已含任一 run 目录时拒绝覆盖。固定顺序为：

```powershell
$runOrder = @(
    @{ Variant = 'A'; RunIndex = 1 },
    @{ Variant = 'B'; RunIndex = 1 },
    @{ Variant = 'B'; RunIndex = 2 },
    @{ Variant = 'A'; RunIndex = 2 },
    @{ Variant = 'A'; RunIndex = 3 },
    @{ Variant = 'B'; RunIndex = 3 }
)
```

每轮根固定为 `$OutputRoot\A-1`、`B-1`、`B-2`、`A-2`、`A-3`、`B-3`，并向 `Measure-RustV2RuntimeAcceptance.ps1` 显式传入本轮 release、metadata、外置工具、`EvidenceRoot=$runRoot\evidence`、`ReportPath=$runRoot\evidence\report.md`、variant、run index 和全部固定配置。顶层始终写 `ab-run-manifest.json`，按执行顺序记录 intended / started / completed / status / evidence_root / report_path。

六轮全部完成时 stdout 固定输出：

```text
RUST_V2_CPU_IO_AB_RUN_COMPLETE
AB_ROOT=C:\tmp\rust-v2-cpu-io-ab\runs
AB_MANIFEST=C:\tmp\rust-v2-cpu-io-ab\runs\ab-run-manifest.json
```

任何一轮输入、启动或采集基础设施失败时保留该轮目录和原始 stderr，写 `ab-run-result.json` 为 `INCONCLUSIVE`，输出 `RUST_V2_CPU_IO_AB_RUN_INCONCLUSIVE` 后以非零退出；停止后续轮次，不得自动改顺序补跑。业务正确性 / 性能失败仍允许六轮采集完成，由聚合报告裁决 `FAIL`。

- [ ] **Step 5: 实现时间戳配对和生产窗口**

对每个 system sample 选择时间戳不晚于它的最近 runtime snapshot。配对年龄 ≤2,500 ms 才计入；有效配对权重 / 全部生产权重 ≥95% 才可裁决。

生产窗口定义为 `ComputeBaseFeatures` stage 为 running 的区间；该 stage 终态后的样本全部进入 finalization tail 单独报告，不使用固定 120 秒硬切割。

- [ ] **Step 6: 实现基线分桶和硬门禁**

从 A 三轮全部非零磁盘读取生产样本的“物理盘读取字节每秒合计”按真实时间权重计算 P25 / P75，并把数值原样用于 B。三轮聚合统一取每轮值的中位数。

报告精确计算：

```text
throughput = persisted terminal files / production seconds
idle_worker_seconds_while_media_waits
  = Σ(dt × (W - worker_slots.current)), when media_permit_waiting > 0
worker_cpu_cores
  = Σ(worker CpuDeltaMs) / sample_interval_ms
resource_bubble_seconds
  = Σ(dt), when pending > 0 and worker_slots=0 and hash_io=0 and media_io=0
cache_wait_resource_ownership_violations
  = count(failure details with stable code CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION)
private_bytes
  = node PrivateBytes + all live same-run Worker PrivateBytes
```

时间加权经验分位数用于物理盘读取队列 P95；任务 summary 累计终态数用于每个 scan 的 90% → 最后 Applied ACK 尾部跨度，单轮值取该轮 completed scans 尾部跨度的中位数。`item_completion_latency` 是每个 runtime task 内累计直方图：每个 completed scan 只取最后一个快照，按 bucket count 合并三十分钟内全部 completed scans 后重算 P95，禁止把每秒重复快照当成独立样本。

所有比例门禁使用整数交叉相乘，避免浮点边界漂移。要求“降低 50%”的基线值为 0 时，候选也为 0 才通过；要求“不得超过 110% / 125%”的基线值为 0 时同样只接受候选为 0。没有有效分母所需样本则是证据缺失，不把除零结果写成 0。

- [ ] **Step 7: 固定最终状态规则**

先收集“已证实硬失败”和“证据缺失”两组原因。已证实的任务失败、结果不一致、媒体变化、容量 / 守恒突破或性能硬门禁失败足以判 `FAIL`，不能被另一项缺失证据降级成 `INCONCLUSIVE`。没有已证实硬失败时，以下证据缺失条件判 `INCONCLUSIVE`：

- 必要文件 / 样本 / metadata / result summary 缺失；
- 配对覆盖 <95%；
- runtime gap >2,500 ms；
- 无 baseline 非零 I/O 样本；
- A 缺字段 12–23、27，或 B 缺字段 12–27。

已证实 `FAIL` 条件：

- 任一正确性摘要、计数或媒体清单不一致；
- 遗留 queued / running；
- 任一硬容量或守恒失败；
- 固定基准改善 <15%；
- 真实媒体任一性能硬门禁失败。

只有所有必要证据完整且全部门禁通过才输出 `PASS`。

- [ ] **Step 8: 写每个性能门禁 fixture**

至少覆盖：

- throughput <95%；
- media waiting idle Worker-seconds 未降低 50%；
- 低 / 高 I/O idle Worker 差未降低 50%；
- Worker CPU 核当量差未降低 50%；
- disk queue P95 >110%；
- private bytes peak >125%；
- resource bubble 未降低 50%；
- cache wait resource ownership violation count 非 0；
- 90% 尾部变长；
- item P95 >110%；
- 固定 benchmark <15%；
- 全部门禁通过。

- [ ] **Step 9: 运行 GREEN**

```powershell
pwsh -NoProfile -File tests\windows\Test-RustV2CpuIoAbReport.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2Package.ps1
```

Expected:

```text
RUST_V2_CPU_IO_AB_REPORT_PASS
RUST_V2_PACKAGE_TEST_PASS
```

- [ ] **Step 10: Checkpoint**

推荐提交说明：`test-tools: add repeatable cpu io ab gate`。确认没有任何脚本引用 `I:\Tool`，也没有把外置 EXE 加到 formal whitelist。

---

### Task 8: 冻结带同等观测能力的真实媒体基线 A

**Files:**

- Modify: `docs/verification/2026-08-24-cpu-io-active-seat-smoothing-implementation.md`
- Reference: Tasks 2–7 files

**Interfaces:**

- A is the current scheduling/refill/decode behavior plus exact fields 12–23 and field 27 item latency.
- A fields 24–26 remain null because the old implementation has no corresponding real credit.
- Produces: verified A formal ZIP, expanded A release root, source snapshot, package / manifest / source SHA.

- [ ] **Step 1: 证明 A 没有候选行为**

定向 diff 和测试必须确认：

- scheduler 仍按现有授予历史，而非 active pressure；
- `fill_hash_tasks` 仍可能同轮补满；
- 没有 `HashRefillController`；
- decode 展示容量仍为 `queue_capacity + W`；
- 没有 `DecodeCredit`；
- fields 24–26 为 null，不是伪造 0。

- [ ] **Step 2: 运行 A 全部共享观测测试**

```powershell
cargo test -p dedup-protocol --test runtime_tasks_wire --locked
cargo test -p dedup-node-engine --features test-hooks --test runtime_tasks --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1
```

- [ ] **Step 3: 生成 A source tree fingerprint**

先用最终共享观测代码和当前 `Cargo.lock` 运行 A 三轮固定 benchmark：

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-cpu-io-a-bench-target'
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

原始输出保存到 `C:\tmp\rust-v2-cpu-io-ab\benchmark\A`，记录 benchmark EXE 和 `Cargo.lock` SHA。然后按 Task 1 同一算法保存 HEAD、tracked patch、未跟踪目标文件内容哈希和 `Cargo.lock`。源码证据归档到 `C:\tmp\rust-v2-cpu-io-ab\sources\A`；不得复制 `target`、`data`、媒体或凭据。

同时在 A benchmark 根保存：

```powershell
rustc -Vv | Set-Content -LiteralPath C:\tmp\rust-v2-cpu-io-ab\benchmark\A\rustc-version.txt -Encoding utf8NoBOM
cargo -V | Set-Content -LiteralPath C:\tmp\rust-v2-cpu-io-ab\benchmark\A\cargo-version.txt -Encoding utf8NoBOM
$benchConfig = [ordered]@{
    rustc_host = ((rustc -Vv | Select-String '^host:').Line -replace '^host:\s*','')
    cargo_build_target = $env:CARGO_BUILD_TARGET
    cargo_config_sha256 = if (Test-Path -LiteralPath '.cargo\config.toml') { (Get-FileHash -LiteralPath '.cargo\config.toml' -Algorithm SHA256).Hash.ToLowerInvariant() } else { $null }
    profile = 'bench'
    rustflags = $env:RUSTFLAGS
    encoded_rustflags = $env:CARGO_ENCODED_RUSTFLAGS
    cargo_profile_bench = @(Get-ChildItem Env: | Where-Object Name -Like 'CARGO_PROFILE_BENCH_*' | Sort-Object Name | ForEach-Object { "$($_.Name)=$($_.Value)" })
}
[IO.File]::WriteAllText('C:\tmp\rust-v2-cpu-io-ab\benchmark\A\benchmark-config.json', ($benchConfig | ConvertTo-Json -Depth 4 -Compress), [Text.UTF8Encoding]::new($false))
```

`CARGO_TARGET_DIR` 只是隔离输出路径，不进入 A/B 相等比较；Rust、Cargo、host / build target、Cargo config SHA、bench profile 和 flags 必须逐字相同。source manifest 的 SHA 另写入 `C:\tmp\rust-v2-cpu-io-ab\sources\A\source-tree.sha256`，禁止只留在当前 PowerShell 变量中。

- [ ] **Step 4: 构建 A 版外置工具用于包输入校验**

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-acceptance-tools-a-target'
cargo build -p dedup-desktop-core --example runtime_acceptance --release --locked --target x86_64-pc-windows-msvc
cargo build -p dedup-node-store --features acceptance-tools --example export_scan_result_summary --release --locked --target x86_64-pc-windows-msvc
New-Item -ItemType Directory -Path C:\tmp\rust-v2-acceptance-tools -Force | Out-Null
Copy-Item -LiteralPath C:\tmp\rust-v2-acceptance-tools-a-target\x86_64-pc-windows-msvc\release\examples\runtime_acceptance.exe -Destination C:\tmp\rust-v2-acceptance-tools\runtime_acceptance.exe
Copy-Item -LiteralPath C:\tmp\rust-v2-acceptance-tools-a-target\x86_64-pc-windows-msvc\release\examples\export_scan_result_summary.exe -Destination C:\tmp\rust-v2-acceptance-tools\export_scan_result_summary.exe
```

这些只是 provisional 工具；不得运行六轮。Task 12 会用 B 源码构建最终统一工具并更新 A 的 test-only metadata。

- [ ] **Step 5: 构建并立刻归档 A formal package**

```powershell
$sourceTreeSha256 = (Get-Content -LiteralPath C:\tmp\rust-v2-cpu-io-ab\sources\A\source-tree.sha256 -Raw).Trim()
if ($sourceTreeSha256 -notmatch '^[0-9a-f]{64}$') { throw 'RUST_V2_A_SOURCE_FINGERPRINT_INVALID' }
pwsh -NoProfile -File scripts\build-rust-v2-cpu-io-test-package.ps1 -Variant A -CargoTargetDir C:\tmp\rust-v2-cpu-io-a-target -OutputRoot C:\tmp\rust-v2-cpu-io-ab\packages -SourceRevision (git rev-parse HEAD) -SourceTreeSha256 $sourceTreeSha256 -AcceptanceClientPath C:\tmp\rust-v2-acceptance-tools\runtime_acceptance.exe -ResultExporterPath C:\tmp\rust-v2-acceptance-tools\export_scan_result_summary.exe
```

- [ ] **Step 6: 验证 A 包边界**

运行 formal verifier，确认：

- 顶层只有四个 EXE；
- 五个 FFmpeg DLL 完整；
- manifest 和 ZIP sidecar 正确；
- 无 data / DB / runtime_acceptance / exporter；
- A ZIP 已复制到唯一目录，后续 B build 不会覆盖。

- [ ] **Step 7: Checkpoint**

在实施账本记录 `A_PACKAGE_FROZEN`、package SHA、manifest SHA、source tree SHA、release root 和外置工具待绑定状态。此任务不部署。

---

### Task 9: 将 DiskReadScheduler 改为 active seat 压力调度

**Files:**

- Modify: `crates/node-engine/src/io/scheduler.rs`
- Modify: `crates/node-engine/tests/disk_scheduler.rs`
- Reference: `crates/node-engine/src/io/mod.rs`
- Reference: `crates/node-engine/src/scan/pipeline.rs`

**Interfaces:**

- Keeps public: `DiskReadScheduler::new`、`acquire`、`acquire_for_test`、`shutdown`。
- Produces internal: `NominalSeats`、`PressureRatio`、`AgedReservation`。
- `io/mod.rs` remains unchanged unless compilation proves an existing re-export is missing.

- [ ] **Step 1: 写 active seat RED 测试**

新增或改写以下确定性测试：

```text
both_classes_on_four_seat_disk_converge_to_three_media_and_one_hash_by_active_count
single_waiting_class_can_borrow_all_four_seats
borrowed_seat_is_not_preempted_and_natural_drop_restores_media_target
global_class_pressure_applies_to_cross_disk_candidates
capacity_one_rotation_is_media_three_then_hash_one
capacity_one_composite_with_conflicting_preferences_chooses_oldest_atomic_request
aged_reservation_freezes_intersecting_and_last_global_seat_but_allows_disjoint_work
aged_reservation_is_cleared_after_cancel
class_active_counts_return_to_zero_after_permit_drop
composite_permit_updates_and_releases_every_disk_class_counter_atomically
same_disk_lower_observed_limit_recomputes_nominal_seats_without_preempting_active_permits
composite_location_uses_minimum_limit_for_all_underlying_disks
```

保留现有 hard-limit、round-robin、复合盘和取消测试。

- [ ] **Step 2: 运行 Scheduler RED**

```powershell
cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler both_classes_on_four_seat_disk_converge_to_three_media_and_one_hash_by_active_count --locked -- --exact --test-threads=1
```

Expected: 当前 grant-history 逻辑不能按 active 数收敛。

- [ ] **Step 3: 冻结名义 seat 计算**

实现：

```rust
/// Hash 与媒体读取在指定硬上限下的名义活动容量。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct NominalSeats {
    /// MediaDecode 软目标；T=1 时没有比例分母。
    media: Option<usize>,
    /// HashSequential 软目标；T=1 时没有比例分母。
    hash: Option<usize>,
}

/// 按任务启动时冻结的 Worker 数计算软目标，不改变硬上限。
fn nominal_seats(
    limit: usize,
    worker_count: usize,
) -> Result<NominalSeats, SchedulerError> {
    if limit == 1 {
        return Ok(NominalSeats {
            media: None,
            hash: None,
        });
    }
    let three_quarters = limit
        .checked_mul(3)
        .ok_or(SchedulerError::InvalidConfiguration("名义 seat 计算溢出"))?
        / 4;
    let media = worker_count
        .min(limit - 1)
        .min(three_quarters);
    Ok(NominalSeats {
        media: Some(media),
        hash: Some(limit - media),
    })
}
```

配置验证已保证 `limit>0`、`worker_count>0`；ActorConfig 必须保存 effective worker count，`DiskReadScheduler::new` 将它传入 actor。

- [ ] **Step 4: 增加 global / per-disk class active**

`UnderlyingDiskState` 保存 total、hash、media 三个 active 原子计数和本盘 nominal seats。`ActorState` 保存全局对应计数和 nominal seats。

同一底层盘后来通过另一逻辑位置或复合位置观察到更小硬上限时，`enqueue` 使用 `effective_limit=min(existing_limit, observed_limit)`，并同时按新上限重算该盘 `NominalSeats`；不能只降低 hard limit 而保留旧压力分母。若已有 active 暂时高于新上限，不抢占许可，只暂停该盘新授予，等待 RAII Drop 自然降回上限。复合位置对每个底层盘分别应用已观察到的最小上限。

`DiskReadPermit` 冻结 class 对应的原子计数。授予时依次增加：

1. global total；
2. global class；
3. 每盘 total；
4. 每盘 class。

Drop 反向精确归还并 `notify_one`。`reply.send` 失败直接 Drop 完整 permit。

- [ ] **Step 5: 实现整数压力比较**

```rust
/// 候选授予后的 active / nominal，用交叉相乘比较。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct PressureRatio {
    numerator: usize,
    denominator: usize,
}

/// 全局唯一、只保护一个达到老化阈值的队首请求。
#[derive(Clone, Debug, Eq, PartialEq)]
struct AgedReservation {
    key: DiskKey,
    class: DiskReadClass,
    sequence: u64,
}
```

对每个 grantable 候选计算：

```text
max(
  global (active_class + 1) / nominal_class,
  each involved disk with T>=2 (active_class + 1) / nominal_class
)
```

比值先无损转换成 `u128`，再比较 `left.numerator * right.denominator` 与 `right.numerator * left.denominator`；`usize` 两项乘积可完整容纳于 `u128`，不使用浮点。

- [ ] **Step 6: 实现选择优先级**

`select_waiter(&mut self)` 顺序固定：

1. prune cancelled heads，并刷新唯一 aged reservation；
2. aged 可原子授予时优先；
3. aged 不可授予时，只过滤与其盘集合相交或会占用最后 global seat 的年轻请求；
4. `T=1` 双类冲突按 Media×3 → Hash×1；
5. 多个 `T=1` 资源偏好冲突时选最老 grantable 原子请求，并用实际 class 同步所有相关轮换；
6. 其余候选选授予后压力最低者；
7. 比例相同选 sequence 更小者；
8. sequence 相同使用现有 disk rotation。

只有一类可授予时必须借满空闲 seat。名义容量不得进入 `can_reserve_all`，不得抢占已发 permit。

- [ ] **Step 7: 实现唯一老化保留**

达到 `MAX_CONFLICTING_BYPASSES=8` 的队首中选择 sequence 最小者。保留只在以下情况清除：

- 该请求成功授予；
- 该请求被取消 / reply closed；
- 队首身份不再等于冻结 key + class + sequence。

不相交且不争最后 global seat 的工作继续推进。

- [ ] **Step 8: 运行 Scheduler GREEN**

```powershell
cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler --locked -- --test-threads=1
cargo check -p dedup-node-engine --locked
```

- [ ] **Step 9: Checkpoint**

检查只有 scheduler 源码和测试产生本任务增量；`io/mod.rs` 与 `pipeline.rs` 不得产生 Task 9 的新增 hunk，Task 3 已存在的观测改动保持不变。推荐提交说明：`fix: schedule disk reads by active class seats`。

---

### Task 10: 实现 Hash output credit 和渐进 refill token

**Files:**

- Modify: `crates/node-engine/src/scan/base_flow_control.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`

**Interfaces:**

- Produces: `ContentOutputCredits`、`ContentOutputCredit`。
- Produces: `HashRefillController`、`HashRefillPhase`、`ContentDeparture`、`HashStartResult`。
- Replaces: same-turn `while hashing.len() < hash_capacity` refill.
- Publishes: `content_output_credit_owned` and `hash_refill_token_available`。

- [ ] **Step 1: 写纯状态机 RED 测试**

覆盖：

```text
warmup_starts_with_hash_capacity_tokens
successful_spawn_consumes_exactly_one_token
missing_task_slot_or_output_credit_keeps_token
open_upstream_empty_claim_waits_for_publish_without_spinning
closed_upstream_empty_claim_clears_tokens_and_marks_exhausted
first_media_departure_clears_unused_warmup_and_adds_one_stable_token
cache_hit_and_item_failure_each_add_at_most_one_token
task_cancellation_returns_credit_without_adding_token
token_count_never_exceeds_hash_capacity
```

- [ ] **Step 2: 实现 credit 和 refill 类型**

```rust
/// 限制 Hash 结果到离开内容供给阶段之间的真实内存所有权。
#[derive(Clone)]
pub(super) struct ContentOutputCredits {
    semaphore: Arc<Semaphore>,
    capacity: usize,
}

/// 随单个文件移动并在 Drop 时归还的 output credit。
pub(super) struct ContentOutputCredit {
    permit: tokio::sync::OwnedSemaphorePermit,
}

/// Hash 补位所处的一次性预热或永久稳定阶段。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum HashRefillPhase {
    Warmup,
    Stable,
}

/// 控制每次 select 边界最多启动一个 Hash 的稳定令牌状态。
pub(super) struct HashRefillController {
    capacity: usize,
    available: usize,
    phase: HashRefillPhase,
    input_exhausted: bool,
    waiting_for_upstream_publish: bool,
}

/// 文件离开内容供给阶段的唯一原因；取消不属于 departure。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ContentDeparture {
    MediaRequested,
    TerminalItem,
}

/// 单个 select epoch 的 Hash 启动结果。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum HashStartResult {
    Started,
    NoTaskSlot,
    NoOutputCredit,
    NoToken,
    WaitingForUpstream,
    InputExhausted,
}
```

`HashRefillController` 必须实现 `new`、`can_attempt_claim`、`consume_after_started`、`observe_empty_claim`、`on_upstream_item_published`、`on_upstream_closed`、`on_content_departed`、`available` 和 `input_exhausted`。`on_content_departed` 在 input exhausted 后不补令牌。第一次 `MediaRequested` 清空未用 warmup token、进入 Stable，再只加当前文件的一个替代 token。

`ContentOutputCredits` 是 BaseCompute 从 Hash 结果到内容供给离开的独立所有权边界，禁止复用 `CacheResolverHandle.content_credits`；后者只限制远端 content 查询结果容量，释放时机与本 credit 不同。

- [ ] **Step 3: 写 Hash admission RED 测试**

新增：

```text
hash_refill_starts_at_most_one_task_per_select_boundary
hash_refill_does_not_consume_token_without_output_credit
multiple_ready_hashes_do_not_refill_the_whole_window
hash_failure_returns_one_credit_and_one_replacement_token
all_cache_hits_continue_past_the_initial_hash_window
all_item_failures_continue_past_the_initial_hash_window
input_open_empty_claim_preserves_token_until_item_publish
closed_input_empty_claim_stops_future_claims
first_media_request_enters_stable_once
```

- [ ] **Step 4: 让 output credit 随文件移动**

所有权固定：

```text
Hash future
  -> HashedBaseItem
  -> ContentResolveContext or pending local resolution
  -> BaseComputeJob
  -> first media acquire registration: Drop

CacheHit or item failure
  -> persist terminal queue: Drop
```

Hash 开始前必须先取得 credit，再调用 `claim_next_item`。claim 返回 None 时 credit 自动 Drop，token 保留或按权威上游关闭规则清空。

- [ ] **Step 5: 将 Hash 启动改为单次尝试**

最终签名：

```rust
/// 在一个 select epoch 内最多领取并启动一个 Hash。
fn try_start_one_hash_task<F: PipelineFileReader>(
    store: &BaseStoreHandle,
    task_id: TaskId,
    now_ms: i64,
    hash_capacity: usize,
    reader: &F,
    cancellation: &ReadCancellationToken,
    hashing: &mut JoinSet<HashTaskOutput>,
    output_credits: &ContentOutputCredits,
    refill: &mut HashRefillController,
    upstream_closed: bool,
) -> Result<HashStartResult, ScanError>
```

只有成功 claim + spawn 返回 `HashStartResult::Started` 并消费 token。

- [ ] **Step 6: 强制 select epoch**

主循环保存 `hash_spawn_allowed`。本 epoch 成功启动后置 false；只有 `tokio::select!` 实际完成一个分支后才重置 true。同步 queue 处理或 `continue` 不得重置。

`drain_ready_hash_results` 改为 `drain_one_ready_hash_result`；一次只归并一个 ready output，然后回到 select。

- [ ] **Step 7: 区分临时空与权威耗尽**

path 阶段实际执行 `queue_scan_item_for_read` 后调用 `refill.on_upstream_item_published()`。上游仍开放且 claim None 时设置 waiting-for-publish；`lookup_finished` 置位时调用 `on_upstream_closed()` 允许最后一次权威 claim。该次仍 None 才设置 input exhausted 并清空 token。

- [ ] **Step 8: 发布并校验 ownership 与 control-state**

每次迁移发布：

```text
content_output_credit_owned <= queue_capacity
hash_refill_token_available <= hash_capacity
```

`content_output_credit_owned` 走 ownership 取得 / Drop 守恒；`hash_refill_token_available` 通过 `update_control_state_nowait` 发布，只检查容量与状态机，不进入 ownership 求和。任务正常终态、失败和取消后两者 current 必须为 0；input exhausted 时 token 0 是正确终态，不是泄漏。

- [ ] **Step 9: 运行 GREEN**

```powershell
cargo test -p dedup-node-engine --features test-hooks --lib scan::base_flow_control --locked
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_utilization --locked -- --test-threads=1
```

- [ ] **Step 10: Checkpoint**

推荐提交说明：`fix: smooth hash refill with explicit output credit`。本任务不得同时改变 decode capacity 或 Worker Started 释放规则。

---

### Task 11: 实现 2W decode credit 和 Worker admission 生命周期

**Files:**

- Modify: `crates/node-engine/src/scan/base_flow_control.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/base_compute_utilization.rs`
- Reference: `crates/node-engine/src/worker/pool.rs`
- Reference: `crates/node-store/src/tasks.rs`

**Interfaces:**

- Produces: `DecodeCredits`、`DecodeCredit`。
- Produces: `ContentResolutionNeed` and two-phase content consume.
- Publishes: `decode_credit_owned`。
- Keeps: existing Worker protocol and NodeStore schema.

- [ ] **Step 1: 写 decode 状态机 RED 测试**

覆盖：

```text
decode_capacity_is_exactly_twice_worker_count
local_compute_candidate_waits_without_consuming_context_when_credit_is_full
remote_compute_candidate_waits_without_consuming_cursor_when_credit_is_full
cache_hit_does_not_require_decode_credit
decode_credit_moves_pending_media_dispatch_start_pending
dispatch_ack_keeps_credit_until_authoritative_started
media_acquire_failure_releases_credit_once
dispatch_failure_releases_credit_once
terminal_before_started_releases_credit_once
task_cancel_releases_all_decode_credit
media_ready_error_and_none_still_count_toward_admission_until_join
```

- [ ] **Step 2: 实现受检 2W 容量**

```rust
/// 限制尚未收到权威 Worker Started 的解码候选总数。
#[derive(Clone)]
pub(super) struct DecodeCredits {
    semaphore: Arc<Semaphore>,
    capacity: usize,
}

/// 随 pending/media/dispatch/start-pending 移动的单项解码所有权。
pub(super) struct DecodeCredit {
    permit: tokio::sync::OwnedSemaphorePermit,
}

/// 计算 2W，溢出或零 Worker 时返回明确配置错误。
fn decode_credit_capacity(worker_capacity: usize) -> Result<usize, ScanError> {
    worker_capacity
        .checked_mul(2)
        .filter(|capacity| *capacity > 0)
        .ok_or_else(|| ScanError::Stage1("decode credit 容量无效或溢出".into()))
}
```

`RuntimeExecutionConfig.decode_queue_capacity` 改为该精确值。

- [ ] **Step 3: 拆分 content 规划与提交**

先只读判断最佳 local / remote cache 是否还需 Worker：

```rust
/// 在不消费原上下文、不写 Store 的前提下判断是否需要 Worker。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ContentResolutionNeed {
    CacheHit,
    WorkerCompute,
}

fn content_resolution_need(
    local: Option<&BaseCacheRecord>,
    remote: Option<&BaseCacheRecord>,
    contact_sheet_exists: bool,
    force_recompute: bool,
) -> ContentResolutionNeed
```

若为 WorkerCompute，先 `try_acquire` decode credit。失败时：

- local 路径保留 `pending_hashed.front()`；
- remote 路径保留 cursor `items.front()` 和 `content_contexts`；
- 不调用 import / upsert / `set_running_item_content_and_stage`；
- 返回主循环等待 credit 释放。

取得 credit 后才 pop / remove 上下文、执行 Store 写入并构造 `BaseComputeJob`。

- [ ] **Step 4: 冻结 decode credit 转移**

`BaseComputeJob` 持有 decode credit。转移顺序：

```text
pending_compute
  -> media_acquiring waiting or ready
  -> worker_dispatching
  -> active(worker_slot=None)
  -> authoritative WorkerEvent::Started: Drop decode credit
```

`fill_media_acquires` 首次实际注册 MediaDecode 请求时调用 Task 10 的 `on_content_departed(MediaRequested)`，同时归还 output credit。

- [ ] **Step 5: 冻结 media ready 子状态**

继续使用 Task 3 的 guard，保证：

```text
media_acquiring.len
  = media_permit_waiting + media_acquire_ready
media_permit_ready <= media_acquire_ready
```

成功、错误和 `None` 在协调器 join 前都占 Worker admission；只有成功 `Some` 计入 permit-ready。

- [ ] **Step 6: 冻结 dispatch / Started 边界**

协调器一次最多派发一个，因此使用：

```rust
/// dispatch ACK 返回前由协调器独占的 Worker 作业和资源。
struct PendingWorkerDispatch {
    job: BaseComputeJob,
    media_permit: Option<ErasedMediaPermit>,
}
```

局部状态类型为 `Option<PendingWorkerDispatch>`。调用 `dispatch_scan().await` 前进入 worker_dispatching；错误时释放 media permit 和 job 中的 decode credit。ACK 成功后把 job 字段移入 `ActiveBase { worker_slot: None, decode_credit: Some(decode_credit) }`。

只有匹配冻结身份的 `WorkerEvent::Started`：

- 设置真实 slot；
- Drop decode credit；
- 进入 unknown，等待权威 phase。

Completed / Crashed / Cancelled 在 Started 前到达时通过移除 `ActiveBase` 恰好释放一次 credit。

- [ ] **Step 7: 强制两个独立守恒**

每次迁移检查：

```text
worker_admission_owned
  = active.len + media_acquiring.len + worker_dispatching
  <= W

decode_credit_owned
  = pending_compute.len
  + media_acquiring.len
  + worker_dispatching
  + worker_start_pending
  <= 2W
```

`active(worker_slot=None)=worker_start_pending`。已 Started 的 Worker 不再持有 decode credit。

- [ ] **Step 8: 运行 GREEN 和恢复测试**

```powershell
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_utilization --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test scan_runtime_details --locked -- --test-threads=1
cargo test -p dedup-node-engine --test runtime_recovery --locked -- --test-threads=1
```

- [ ] **Step 9: Checkpoint**

推荐提交说明：`fix: bound pre-start decode ownership to two worker windows`。确认 Worker protocol、NodeStore schema 和 SSD 识别没有改动。

---

### Task 12: 候选 B 全量回归、正式包和外置工具冻结

**Files:**

- Modify: `docs/verification/2026-08-24-cpu-io-active-seat-smoothing-implementation.md`
- Reference: all implementation files
- Reference: `scripts/build-release.ps1`
- Reference: `scripts/verify-release.ps1`

**Interfaces:**

- Produces: verified B formal ZIP and expanded release.
- Produces one external tool pair used unchanged for all six real-media runs.
- Rebinds A and B metadata to the same tool SHA values.

- [ ] **Step 1: 运行格式和静态检查**

```powershell
cargo fmt --all -- --check
git diff --check
cargo check --workspace --locked
```

- [ ] **Step 2: 运行 Rust 定向回归**

```powershell
cargo test -p dedup-protocol --test runtime_tasks_wire --locked
cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export --locked
cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test runtime_tasks --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_utilization --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test scan_parallelism --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test scan_runtime_details --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
```

- [ ] **Step 3: 运行 Windows 工具回归**

```powershell
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2ResultSummary.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2CpuIoAbReport.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2Package.ps1
```

- [ ] **Step 4: 生成 B source tree fingerprint**

按 Task 1 同一算法归档到 `C:\tmp\rust-v2-cpu-io-ab\sources\B`，记录 HEAD、tracked patch、未跟踪目标哈希和 `Cargo.lock`。B 的 `Cargo.lock` SHA 必须与 Task 8 A benchmark 记录完全一致；不一致时停止构建并标记 `RUST_V2_AB_LOCKFILE_MISMATCH`。

把 B source manifest SHA 写入 `C:\tmp\rust-v2-cpu-io-ab\sources\B\source-tree.sha256`。A 与 B 的 source fingerprint 应各自绑定对应包且各自三轮稳定，但二者不要求相同。

- [ ] **Step 5: 构建唯一外置工具**

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-acceptance-tools-target'
cargo build -p dedup-desktop-core --example runtime_acceptance --release --locked --target x86_64-pc-windows-msvc
cargo build -p dedup-node-store --features acceptance-tools --example export_scan_result_summary --release --locked --target x86_64-pc-windows-msvc
New-Item -ItemType Directory -Path C:\tmp\rust-v2-acceptance-tools -Force | Out-Null
Copy-Item -LiteralPath C:\tmp\rust-v2-acceptance-tools-target\x86_64-pc-windows-msvc\release\examples\runtime_acceptance.exe -Destination C:\tmp\rust-v2-acceptance-tools\runtime_acceptance.exe
Copy-Item -LiteralPath C:\tmp\rust-v2-acceptance-tools-target\x86_64-pc-windows-msvc\release\examples\export_scan_result_summary.exe -Destination C:\tmp\rust-v2-acceptance-tools\export_scan_result_summary.exe
```

记录两个 EXE SHA-256；六轮禁止重新构建或替换。

- [ ] **Step 6: 构建并归档 B formal package**

```powershell
$sourceTreeSha256 = (Get-Content -LiteralPath C:\tmp\rust-v2-cpu-io-ab\sources\B\source-tree.sha256 -Raw).Trim()
if ($sourceTreeSha256 -notmatch '^[0-9a-f]{64}$') { throw 'RUST_V2_B_SOURCE_FINGERPRINT_INVALID' }
pwsh -NoProfile -File scripts\build-rust-v2-cpu-io-test-package.ps1 -Variant B -CargoTargetDir C:\tmp\rust-v2-cpu-io-b-target -OutputRoot C:\tmp\rust-v2-cpu-io-ab\packages -SourceRevision (git rev-parse HEAD) -SourceTreeSha256 $sourceTreeSha256 -AcceptanceClientPath C:\tmp\rust-v2-acceptance-tools\runtime_acceptance.exe -ResultExporterPath C:\tmp\rust-v2-acceptance-tools\export_scan_result_summary.exe
```

- [ ] **Step 7: 重新绑定并核验 A metadata**

把最终工具路径和 SHA 写入 A / B 的 test-only metadata。不得修改 A formal ZIP 或 manifest。验证 A package SHA 自 Task 8 起未改变。

- [ ] **Step 8: Checkpoint**

实施账本记录 `B_PACKAGE_FROZEN`、A/B formal verifier 输出、两个 package SHA、两个 source tree SHA 和统一工具 SHA。此时仍不部署。

---

### Task 13: 运行固定基准和六轮真实媒体 A/B

**Files:**

- Modify: `docs/verification/2026-08-24-cpu-io-active-seat-smoothing-implementation.md`
- Create: `docs/verification/2026-08-24-cpu-io-active-seat-smoothing-final.md`
- Reference: `crates/node-engine/benches/base_compute_pipeline.rs`
- Reference: `tests/windows/Measure-RustV2CpuIoAb.ps1`
- Reference: `tests/windows/New-RustV2CpuIoAbReport.ps1`

**Interfaces:**

- Consumes immutable A0 diagnostic evidence、A/B benchmark evidence、A formal package、B formal package and one external tool pair.
- Produces B benchmark three-run evidence and six immutable real-media run roots.
- Does not mutate source or production media.

- [ ] **Step 1: 再次核验运行输入**

运行前核对：

- A / B package SHA 与 metadata 一致；
- A / B release root 来自各自 ZIP；
- 两个外置工具 SHA 与 A / B metadata 一致；
- config fingerprint 预期相同；
- `RUST_V2_REAL_MEDIA_ROOT` 已设置、存在且不是 A/B output root；
- output root 不在媒体根、仓库和 `I:\Tool` 内；
- C: 剩余空间足以容纳六个独立 DB / cache / log / evidence。

- [ ] **Step 2: 使用冻结命令运行 B 三轮 benchmark**

```powershell
$benchmarkB = 'C:\tmp\rust-v2-cpu-io-ab\benchmark\B'
New-Item -ItemType Directory -Path $benchmarkB -Force | Out-Null
rustc -Vv | Set-Content -LiteralPath (Join-Path $benchmarkB 'rustc-version.txt') -Encoding utf8NoBOM
cargo -V | Set-Content -LiteralPath (Join-Path $benchmarkB 'cargo-version.txt') -Encoding utf8NoBOM
$benchConfig = [ordered]@{
    rustc_host = ((rustc -Vv | Select-String '^host:').Line -replace '^host:\s*','')
    cargo_build_target = $env:CARGO_BUILD_TARGET
    cargo_config_sha256 = if (Test-Path -LiteralPath '.cargo\config.toml') { (Get-FileHash -LiteralPath '.cargo\config.toml' -Algorithm SHA256).Hash.ToLowerInvariant() } else { $null }
    profile = 'bench'
    rustflags = $env:RUSTFLAGS
    encoded_rustflags = $env:CARGO_ENCODED_RUSTFLAGS
    cargo_profile_bench = @(Get-ChildItem Env: | Where-Object Name -Like 'CARGO_PROFILE_BENCH_*' | Sort-Object Name | ForEach-Object { "$($_.Name)=$($_.Value)" })
}
[IO.File]::WriteAllText((Join-Path $benchmarkB 'benchmark-config.json'), ($benchConfig | ConvertTo-Json -Depth 4 -Compress), [Text.UTF8Encoding]::new($false))
foreach ($name in @('rustc-version.txt','cargo-version.txt','benchmark-config.json')) {
    $aHash = (Get-FileHash -LiteralPath (Join-Path 'C:\tmp\rust-v2-cpu-io-ab\benchmark\A' $name) -Algorithm SHA256).Hash
    $bHash = (Get-FileHash -LiteralPath (Join-Path $benchmarkB $name) -Algorithm SHA256).Hash
    if ($aHash -ne $bHash) { throw "RUST_V2_AB_BENCH_ENV_MISMATCH:$name" }
}
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-cpu-io-b-bench-target'
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

分别保存原始 stdout、实际 benchmark EXE 路径、SHA-256 和当前 `Cargo.lock` SHA。任一环境或 lockfile 不匹配时停止并输出 `INCONCLUSIVE`；不得把 `decode_and_persist_ms` 或 files/s 代替 `elapsed_ms`。

- [ ] **Step 3: 裁决固定 benchmark**

计算：

```text
improvement_percent
  = (median(A elapsed_ms) - median(B elapsed_ms))
    / median(A elapsed_ms)
    × 100
```

`improvement_percent >= 15` 才通过。A / B seed、清单、`Cargo.lock`、工具链和构建配置任一不一致则 `INCONCLUSIVE`。A0 仅列为观测开销参考。

- [ ] **Step 4: 运行六轮只读真实媒体**

```powershell
if ([string]::IsNullOrWhiteSpace($env:RUST_V2_REAL_MEDIA_ROOT)) {
    throw 'RUST_V2_REAL_MEDIA_ROOT_MISSING'
}
pwsh -NoProfile -File tests\windows\Measure-RustV2CpuIoAb.ps1 -MediaRoot $env:RUST_V2_REAL_MEDIA_ROOT -DurationSeconds 1800 -SampleSeconds 2 -BaselineReleaseRoot C:\tmp\rust-v2-cpu-io-ab\packages\A\release -CandidateReleaseRoot C:\tmp\rust-v2-cpu-io-ab\packages\B\release -BaselineMetadataPath C:\tmp\rust-v2-cpu-io-ab\packages\A\test-package.json -CandidateMetadataPath C:\tmp\rust-v2-cpu-io-ab\packages\B\test-package.json -AcceptanceClientPath C:\tmp\rust-v2-acceptance-tools\runtime_acceptance.exe -ResultExporterPath C:\tmp\rust-v2-acceptance-tools\export_scan_result_summary.exe -OutputRoot C:\tmp\rust-v2-cpu-io-ab\runs -WorkerCount 12 -HddThreadsPerDisk 1 -SsdThreadsPerDisk 16 -UnknownThreadsPerDisk 1 -TotalReadThreads 16 -ReservedCores 1
```

若 Windows 弹出需要用户确认的防火墙或进程提示，停在当前轮等待用户确认；不得通过改变端口、关闭安全功能或跳过该轮绕过。

- [ ] **Step 5: 每轮结束立即做不可变检查**

每轮完成后记录：

- runtime / system 行数、最早 / 最晚时间戳、最大 gap；
- Node / Worker PID 世代和 unexpected exit；
- media-before / after SHA；
- runtime_result 的 task ID；
- result summary 状态、行数和 SHA；
- package / source / config / tool SHA；
- 单轮 report 状态。

若一轮失败，保留证据并停止自动序列；修复基础设施后整组六轮从新 output root 重新开始，不把旧轮与新轮拼接。

- [ ] **Step 6: 生成六轮聚合报告**

```powershell
pwsh -NoProfile -File tests\windows\New-RustV2CpuIoAbReport.ps1 -AbRoot C:\tmp\rust-v2-cpu-io-ab\runs -BenchmarkRoot C:\tmp\rust-v2-cpu-io-ab\benchmark -OutputPath D:\code\mySingerServer\.worktrees\rust-v2-media-dedup\docs\verification\2026-08-24-cpu-io-active-seat-smoothing-final.md
```

- [ ] **Step 7: 人工核对正确性硬门禁**

报告必须列出六轮逐项：

- total / succeeded / failed / skipped；
- queued / running 遗留数；
- canonical result SHA；
- media manifest SHA；
- 每项 ownership 守恒和 peak / capacity；
- cache wait resource ownership violation count 必须为 0；
- snapshot coverage；
- Node / Worker 异常退出。

六个 canonical result SHA 必须完全相同。相同 overall counters 但 result SHA 缺失仍为 `INCONCLUSIVE`。

- [ ] **Step 8: 核对性能硬门禁**

报告逐项显示 baseline median、candidate median、比例、阈值和 PASS / FAIL：

- production throughput ≥95%；
- media waiting idle Worker-seconds 降低 ≥50%；
- 低 / 高 I/O idle Worker 均值差降低 ≥50%；
- 低 / 高 I/O Worker CPU 核当量差降低 ≥50%；
- disk read queue weighted P95 ≤110%；
- same-run Node + Worker private bytes peak ≤125%；
- resource bubble seconds 降低 ≥50%；
- 缓存查询等待项持有 Worker slot、Hash permit 或 Media permit 的 violation count 必须为 0；
- 90% → last ACK tail 不增加；
- item completion P95 ≤110%；
- A → B fixed benchmark elapsed median 改善 ≥15%；A0 只报告 instrumented A 的观测开销差异。

相关系数和 CPU / disk 峰值只列在“解释性指标”，不参与通过。

- [ ] **Step 9: Checkpoint**

将所有命令、退出码、原始 evidence root 和最终报告 SHA 写入实施账本。此任务不打正式发布标签、不复制生产目录。

---

### Task 14: 最终验证、裁决和交付边界

**Files:**

- Modify: `docs/verification/2026-08-24-cpu-io-active-seat-smoothing-implementation.md`
- Modify: `docs/verification/2026-08-24-cpu-io-active-seat-smoothing-final.md`
- Reference: all changed files and immutable A/B evidence

**Interfaces:**

- Produces exactly one final state: `PASS`、`FAIL` or `INCONCLUSIVE`。
- Produces a rollback pointer to the frozen A package.
- Does not deploy, replace production, delete evidence or rewrite media.

- [ ] **Step 1: 运行新鲜的最终静态门禁**

```powershell
git diff --check
cargo fmt --all -- --check
cargo test -p dedup-protocol --test runtime_tasks_wire --locked
cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export --locked
cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_utilization --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test runtime_tasks --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2CpuIoAbReport.ps1
pwsh -NoProfile -File tests\windows\Test-RustV2Package.ps1
```

- [ ] **Step 2: 重新验证两个 formal ZIP**

对 A 和 B 各运行 `scripts\verify-release.ps1`。重新计算 ZIP、sidecar、manifest 和四个 EXE SHA，必须与 frozen metadata 一致。

- [ ] **Step 3: 做一次聚焦代码复核**

使用 `superpowers:requesting-code-review`，只复核：

- permit / credit Drop 和错误路径；
- aged reservation work-conserving 边界；
- open-empty / closed-empty Hash 语义；
- Started 前后 decode credit；
- protobuf 兼容；
- exporter canonical hash 排除运行特有字段；
- A/B 报告数学与状态优先级。

发现问题时返回对应 Task 的 RED 测试，不做无关重构。

- [ ] **Step 4: 使用 verification-before-completion 复核证据**

在声称完成前使用 `superpowers:verification-before-completion`，逐条读取本次新鲜命令输出、package verifier、A/B report 和 result summary；不得从旧日志推断当前通过。

- [ ] **Step 5: 写最终裁决**

规则：

- 证据缺失、覆盖不足或版本不一致：`INCONCLUSIVE`；
- 证据完整但任一正确性或性能硬门禁失败：`FAIL`；
- 证据完整且全部门禁通过：`PASS`。

若 fixed benchmark 未达到 15%，即使真实媒体相位改善也必须写：

```text
调度指标改善，部署门禁未通过
```

只有用户基于完整原始证据明确豁免，才能在后续任务改变发布裁决。

- [ ] **Step 6: 冻结回滚与非部署声明**

最终报告记录：

- A formal ZIP / SHA / source fingerprint；
- B formal ZIP / SHA / source fingerprint；
- 外置工具 SHA；
- 六轮 evidence root；
- A package 作为测试回滚参考；
- `I:\Tool` 未读取、未写入、未替换；
- 生产部署未执行。

- [ ] **Step 7: 安全提交或延期**

运行：

```powershell
git status --short
git diff --cached --name-only
git diff --check
```

只有 staged 文件全部属于本方案且不包含基线用户 hunk 时提交。推荐提交组：

```text
telemetry: add pipeline ownership contract
telemetry: expose exact base compute phases
test-tools: add canonical cpu io acceptance evidence
fix: schedule disk reads by active class seats
fix: smooth hash refill with explicit output credit
fix: bound pre-start decode ownership to two worker windows
docs: record cpu io smoothing final gate
```

否则保留 `COMMIT_DEFERRED_DIRTY_BASELINE` 和精确变更清单，不伪造 commit。

---

### Task 15: 扩展外置验收客户端为多根单轮模式

**User override（2026-08-26）：** 双物理盘测试只执行一个全量真实媒体任务；第一个任务进入 `completed`、`failed` 或 `cancelled` 终态即结束，不等待 1800 秒，也不创建 forced scan。1800 秒仅作为最大截止时间。

**Files:**

- Modify: `crates/desktop-core/examples/runtime_acceptance.rs`
- Modify: `crates/desktop-core/tests/runtime_acceptance_contract.rs`

**Interfaces:**

- 新环境变量 `RUST_V2_REAL_MEDIA_ROOTS_JSON` 为非空 JSON 字符串数组；存在时优先于旧 `RUST_V2_REAL_MEDIA_ROOT`，旧单根调用保持兼容。
- 新环境变量 `RUST_V2_ACCEPTANCE_SINGLE_RUN` 接受 `1/true`；默认仍保持原持续时长/forced scan 行为。
- 首次和后续扫描都必须传递同一完整 roots 数组；单轮模式观察到第一个任务终态后立即写最终 `runtime_result` 并退出。
- `runtime_result` 记录实际 roots 与 `single_run`，作为 Windows harness 与任务根绑定证据的一部分。

- [ ] **Step 1: RED**

先增加真实 contract tests，证明旧实现：不能把 H/I 两根同时交给 `create_scan`；首个 completed 后仍会创建 forced scan。测试必须断言实际 client seam 收到的 roots 与扫描次数，不得匹配源码字符串。

- [ ] **Step 2: GREEN**

用最小配置解析和终态分支实现多根与单轮模式；为空、空白或 JSON 非数组时返回稳定配置错误。新增方法、字段和关键变量添加中文注释。

- [ ] **Step 3: 定向回归**

运行：

```powershell
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
```

---

### Task 16: 扩展 Windows 证据为双物理盘单轮采集

**Files:**

- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
- Modify: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`
- Modify: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`

**Interfaces:**

- 增加 `-MediaRoots [string[]]`、`-SingleRun` 与 `-RequireDistinctPhysicalDisks`；旧 `-MediaRoot` 保持兼容，两者不能同时传入。
- 多根必须全部为绝对、存在的普通目录，不能是 reparse/junction，根之间不能相同或互相包含。
- 运行前用 `Get-Partition` / `Get-Disk` 将每个根盘符绑定到物理 `DiskNumber`，写入 `physical-disk-map.json`；要求双物理盘时，DiskNumber 不同才允许启动。
- `media-before.json` / `media-after.json` 保存所有根的组合只读清单；同时保存 `media-before-root-01.json`、`media-after-root-01.json` 等分根清单。前后按 root、相对路径、长度和 UTC mtime 精确比较。
- 给客户端设置 roots JSON 和 single-run 环境变量；`harness-result.json` 记录 roots、single_run、物理盘映射和各分根清单路径/SHA。
- 系统采样继续为 2 秒；任务快照继续为 1 秒。报告按实际间隔加权，不把 1800 秒作为完成条件。

- [ ] **Step 1: RED**

先增加行为 fixture，覆盖：多根参数传入、嵌套/重复/reparse 拒绝、同物理盘拒绝、roots JSON 环境变量、首终态即退出、组合和分根媒体清单、旧单根兼容。

- [ ] **Step 2: GREEN**

以小函数实现 roots 解析、路径边界、物理盘映射和组合 manifest；不创建 junction，不修改 Node/协议，不新增读取线程池。

- [ ] **Step 3: 定向回归**

运行 Harness、Runtime report fixtures、PowerShell parser 和 `git diff --check`。

---

### Task 17: 冻结候选并执行一次双物理盘真实媒体任务

**Inputs:**

- 只读根：`H:\pik\00000000000`、`I:\tmp`
- 候选 B：最终修复后的 Node/Worker；A 只保留历史回滚参考。
- 配置：Worker `20`、总读取 `12`、HDD/盘 `1`、SSD/盘 `16`、Unknown/盘 `1`、Reserved core `1`、系统采样 `2 秒`。
- 停止条件：首个全量任务进入任一终态；最大截止 `1800 秒`。

**Outputs:**

- Create: `docs/verification/2026-08-26-dual-physical-disk-single-run.md`
- Evidence: `C:\tmp\rust-v2-dual-disk-worker20-read12-20260826-*`
- 不部署、不读取或触碰 `I:\Tool`。

- [ ] **Step 1: 预检与空间门禁**

确认 H/I 存在、不是 reparse、映射到两个不同 DiskNumber；确认 C/D 可用空间。任一盘低于 10 GiB 时，只盘点并清理项目路径内精确、可再生成的 Cargo target/cache，保留所有源码、包和测试证据。

- [ ] **Step 2: 新鲜静态门禁与终审**

运行调度器、WorkerPool、base pipeline、runtime acceptance、Windows harness/report fixtures、fmt 和 diff-check；最终代码审查使用 `gpt-5.6-sol / max`。

- [ ] **Step 3: 重新冻结 B**

重建外置 `runtime_acceptance.exe`，重建 B formal/test-only package，记录 source tree、Cargo.lock、EXE、manifest、ZIP 与工具 SHA。正式 ZIP 仍只允许四个顶层 EXE，验收客户端仍在 ZIP 外。

- [ ] **Step 4: 重跑固定 benchmark**

使用相同 fixture 运行 B 三轮，记录 `elapsed_ms` 中位数；历史 A 不变。该结果仍按原 15% 门禁裁决，但不会改变用户授权的单次双盘诊断范围。

- [ ] **Step 5: 启动一次双盘全量任务**

使用全新隔离根启动一次 B；不得复用或拼接旧轮 evidence。首个任务终态后立即停止采集、退出 Node/Worker、生成结果摘要和报告。

- [ ] **Step 6: 双盘调度判定**

报告分别列出两个 DiskNumber 的时间加权读吞吐、IOPS、队列 P50/P95/峰值；统计两盘同一采样窗口都发生读取的重叠秒数；结合 runtime Worker 的 `display_path`、`physical_disk_id`、phase、空闲 Worker、Hash/Media permit 等字段判断：

- 两根都进入同一任务并产生工作；
- 两块盘能在同一时间窗口推进；
- 无一块有可运行工作却长期被另一盘饿死；
- 全局 12 与各盘类别上限没有越界；
- CPU/磁盘交替相位属于正常流水线重叠还是调度气泡。

当前生产遥测没有按盘 Hash/Media permit 精确计数，因此不得声称观测到了不存在的逐盘 permit 字段；结论必须区分系统盘吞吐、Worker 路径证据和全局 permit 证据。

---

## Spec Coverage Matrix

| 规格约束 | 实施任务 | 主要证据 |
|---|---:|---|
| Worker 直接读媒体，不建 Node read broker | 3、11 | `pipeline.rs` 契约和 WorkerPool 回归 |
| `M=min(W,T-1,floor(3T/4))`、`H=T-M` | 9 | Scheduler RED/GREEN |
| 借用、自然收回、`T=1` 3:1 | 9 | 确定性 permit 测试 |
| 唯一 aged reservation、复合盘原子性 | 9 | 老化 / 取消 / 最后 global seat 测试 |
| Hash waiting / reading / completed-unjoined | 3 | RAII 守卫与 JoinSet 守恒 |
| output credit 与 refill token 分离 | 10 | 纯状态机和 BaseCompute 行为测试 |
| 每个 select 边界最多一个 Hash | 10 | gated coordinator test |
| open-empty / closed-empty 区分 | 10 | upstream publish / exhausted tests |
| 本地 / 远端先拿 decode credit | 11 | 两阶段 content resolution tests |
| `2W` decode ownership、Started 才释放 | 11 | dispatch / Started / terminal tests |
| 新遥测、旧 Node null / — | 2–4 | wire、NDJSON、Desktop tests |
| item claim → Applied ACK P95 | 2–4、7 | runtime histogram + A/B report |
| 任务 1 秒、系统 2 秒、真实间隔加权 | 4、6、7 | contract 和 report fixtures |
| canonical JSONL、全部特征 / artifact SHA | 5、6 | read-only exporter tests |
| 测试工具不进入 formal ZIP | 6、7、8、12 | package verifier |
| A/B fixed benchmark 15% | 8、13 | 相同 lockfile 的三轮 median；A0 仅诊断 |
| `A,B,B,A,A,B` 和 95% 覆盖 | 7、13 | orchestrator / aggregate fixtures |
| 原全部正确性与性能硬门禁 | 7、13、14 | final report |
| 不部署、不触碰 `I:\Tool` | 全局、14 | ledger 与 final declaration |
| 多根同一任务、首终态即结束 | 15、16 | runtime contract 与 Windows harness fixture |
| H/I 物理盘映射、分根只读清单 | 16、17 | physical-disk-map 与媒体前后 manifest |
| 单次双盘重叠与饥饿观察 | 17 | runtime/system NDJSON 与双盘报告 |

## Execution Handoff

实施时从 Task 1 开始，不跳过 A0 或 A 冻结。推荐在当前任务中使用 `superpowers:subagent-driven-development`，每个 Task 由一个实现 worker 和一个聚焦 reviewer 顺序完成；若改为独立会话执行，则使用 `superpowers:executing-plans`，在 Task 8、Task 12 和 Task 14 三个 checkpoint 停下来核对包与证据。
