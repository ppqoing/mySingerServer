# CPU / 磁盘 I/O 活跃席位平滑：实施账本

## 范围与基线

- 方案规格：`docs/superpowers/specs/2026-08-24-cpu-io-active-seat-smoothing-design.md`
- 方案规格 SHA-256：`d0ae79a5a3cc8a9177ed12e51d83965988b8ca4c21de068cb28df078c125c67e`
- pre-plan HEAD（仅用于计划出处，不作为执行基线）：`0e796fcabbd5899678ab12e5ffad8bb94d1214f4`
- 执行 HEAD：`0db47462f9bd80352dc2a05fbb85ce84a0b4ed29`
- 执行基线 dirty / untracked：165 项，完整状态保存在 `C:\tmp\rust-v2-cpu-io-ab\benchmark\A0\baseline-status.txt`。
- `Cargo.lock` SHA-256：`57203a0c18d69b3e24c15df9de54e2b7eac06992025d5722072185201a59ea3b`
- 未触碰 `I:\Tool`；未修改生产代码、未打包、未部署。

`COMMIT_DEFERRED_DIRTY_BASELINE`：既有 165 项 dirty / untracked 内容是冻结输入；不得 reset、clean、`git add -A`、覆盖或提交。此次只精确 stage 并提交本账本。

## 冻结证据

证据根：`C:\tmp\rust-v2-cpu-io-ab\benchmark\A0`

- `baseline-head.txt`：`C:\tmp\rust-v2-cpu-io-ab\benchmark\A0\baseline-head.txt`，内容绑定为执行 HEAD `0db47462f9bd80352dc2a05fbb85ce84a0b4ed29`。
- `baseline-status.txt`：`C:\tmp\rust-v2-cpu-io-ab\benchmark\A0\baseline-status.txt`，165 项 dirty / untracked 状态，SHA-256 `597f81d19aa7ac55dbe46b33349e4859706c6fe1ae51f8ddba30b50db6addd2a`。
- `baseline.patch`：`C:\tmp\rust-v2-cpu-io-ab\benchmark\A0\baseline.patch`，基线工作树二进制 diff，SHA-256 `973c8cd0e6623f907fc699b8560b9dcf80cd0b285bdbb5c0e2f662241f57eed3`。
- `baseline-files.sha256`：Git 可见 tracked + untracked、非 ignored 文件的有序内容清单。
- `source_tree_fingerprint`：`81a858f21946d63433d5382517ffa4c9e52a808704b3b3ae7804249019f3122b`。

现有 dirty 树有 5 个 tracked 路径已经删除，无法按原始逐文件 `Get-FileHash` 命令取得内容；为不恢复、覆盖或遗漏其工作树状态，清单为这 5 项写入 `MISSING  <path>`，其余可见文件均使用小写 SHA-256：

- `crates/desktop-core/src/central/analysis.rs`
- `crates/desktop-core/src/central/content.rs`
- `crates/desktop-core/src/central/cross_analysis.rs`
- `crates/desktop-core/src/central/delete.rs`
- `crates/desktop-core/src/central/schema.rs`

环境记录在 `C:\tmp\rust-v2-cpu-io-ab\benchmark\A0\a0-environment.json`（SHA-256 `ee79a0e4effe6d4435bf676d8234efd01d3175c2b7299be422bca6100bc8079a`）：Rust `1.97.1`、Cargo `1.97.1`、PowerShell `7.6.4`、Windows 10 专业版 `10.0.19045 build 19045`。JSON 中独立包含 `baseline_head_path`/`head`、`baseline_status_path`/`status_sha256`、`baseline_patch_path`/`patch_sha256`；原始 status 与 patch 未被覆盖或重造。

## A0 定向正确性

使用默认配置和以下命令：

```powershell
cargo test -p dedup-node-engine --features test-hooks --test disk_scheduler --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --test base_compute_utilization --locked -- --test-threads=1
```

- `disk_scheduler`：exit 0，12 passed / 0 failed。
- `base_compute_pipeline`：`A0_PREEXISTING_FAILURE`，链接器 exit 1120；`libsqlite3_sys` 的 `sqlite3.o` 未解析 `___chkstk_ms` 与 `__isnan`。
- `base_compute_utilization`：`A0_PREEXISTING_FAILURE`，链接器 exit 1120；同一 `libsqlite3_sys` 未解析符号。

本任务不修复上述既有失败。

## A0 三轮诊断 benchmark

每轮使用冻结命令与默认配置：

```powershell
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-cpu-io-a0-target'
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

| 轮次 | 原始输出 | exit | 关键输出 |
| --- | --- | ---: | --- |
| 01 | `a0-run-01.txt` | 101 | `libsqlite3_sys` 链接时缺少 `___chkstk_ms`；未生成诊断字段。 |
| 02 | `a0-run-02.txt` | 101 | 同上；未生成 `elapsed_ms` 或 `throughput_files_per_second`。 |
| 03 | `a0-run-03.txt` | 101 | 同上；未生成 `elapsed_ms` 或 `throughput_files_per_second`。 |

三轮均在 benchmark EXE 链接前停止，因此 `C:\tmp\rust-v2-cpu-io-ab\artifacts\A0\` 中没有可复制的 EXE 或 SHA-256。这是 `A0_PREEXISTING_FAILURE` 的直接后果，不以替代文件伪造产物。

## Fixture 固定值核对

- seed：`0x2026_08_23_C0DE_0000`
- 文件数：4
- 固定清单：4 KiB、8 KiB、64 MiB、96 MiB
- `total_threads=2`，每盘上限 2，`PipelineLimits::new(4, 2)`，Worker 数 2
- 三轮均未运行至指标输出，故不含 `elapsed_ms` 或 `throughput_files_per_second`；失败原因已在原始输出与上表固定。

标记：`A0_DIAGNOSTIC_ONLY`。A0 仅量化观测代码开销，且不参与最终 15% 硬门禁；最终门禁必须由相同最终 `Cargo.lock` 的 instrumented A 与 B 执行。后续测试工具依赖可能改变 lockfile 的本地 package dependency 列表，因此不能用 A0 替代同 lockfile 的 A/B 门禁。

## TDD 与自审

TDD：不适用。本任务仅冻结证据并创建实施账本，不修改行为或生产代码。

自审：已逐项核对执行 HEAD 与 pre-plan HEAD 区分、165 项 dirty 基线、规格/lockfile 哈希、环境、定向测试、三轮 A0 命令、fixture 常量、A0 诊断边界与 `I:\Tool` 未触碰。待提交前运行 `git diff --check`，并确认暂存区仅包含本文件。

## Task 8：冻结带同等观测能力的真实媒体基线 A

执行状态：`A_PACKAGE_FROZEN`。本任务没有修改 Rust、proto 或生产运行逻辑；没有运行真实媒体六轮、没有部署、没有触碰 `I:\Tool\mySingerServer-rust-v2-win-x64`。

### A 最终输入与旧行为证明

- 工作树：`D:\code\mySingerServer\.worktrees\rust-v2-media-dedup`
- 分支：`codex/rust-v2-media-dedup`
- 源码 revision：`1f52f5ffb4a42ec3c1e0a996e6649507561d3812`
- 最终 `Cargo.lock` SHA-256：`db7464102569bd4bbb1a4b756490e1b80a5159eefd1767b329ec8e309e8ac563`
- 最终 source-tree manifest：`C:\tmp\rust-v2-cpu-io-ab\sources\A\source-tree.manifest`
- 最终 source-tree SHA-256：`7ddfb195000b05d65a2281e78fad63d1dad1366f9549b1fd8e9afef4e55d4c46`
- 完整 HEAD、tracked patch、untracked 内容哈希和 manifest 均保存在 `C:\tmp\rust-v2-cpu-io-ab\sources\A`；算法逐字复现 `scripts\build-rust-v2-cpu-io-test-package.ps1` 的 `Get-SourceTreeHash`，排除 `.git`、SDD、verification、dist 和 target。
- A 仍使用 `DiskReadScheduler` 的位置轮转/FIFO、入队序号老化和同盘媒体 3 次配额；`fill_hash_tasks` 仍可在同轮补满 Hash future；decode 展示容量仍为 `queue_capacity + worker_capacity`。
- A 没有候选 `HashRefillController` 或 `DecodeCredit` 调度实现；24–26 是可选遥测字段，缺失时保持 `null`，不是伪造 `0`。
- Task7 sidecar 初次缺陷尝试完整保留在 `C:\tmp\rust-v2-cpu-io-ab\packages\A-attempt-01-sidecar-invalid`、`sources\A-attempt-01-pre-sidecar-fix`、`benchmark\A-attempt-01-pre-sidecar-fix`；修复 builder 后重新生成最终 A，未复用旧 source SHA 或旧 benchmark 绑定。

### 共享观测测试

全部在清除 `CC`/`CXX`、`--locked`、`C:\tmp\rust-v2-cpu-io-a-target` 下通过：

- `dedup-protocol/runtime_tasks_wire`：4/4。
- `dedup-node-engine/runtime_tasks`：14/14。
- `dedup-node-engine/base_compute_pipeline`：41/41。
- `dedup-desktop-core/runtime_acceptance_contract`：16/16。
- `dedup-desktop-ui/bindings_contract`：15/15。

原始逐命令日志和汇总：`C:\tmp\rust-v2-cpu-io-ab\tests\A-corrected`。首次错误编排只产生 cargo usage，已单独保留，不计为产品测试结果。

### 固定 A benchmark

命令固定为：

```text
cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked
```

使用 `C:\tmp\rust-v2-cpu-io-a-bench-target` 连续三轮，最终 `elapsed_ms` 为 `115.946`、`119.375`、`110.058`，中位数 `115.946 ms`；三轮均 `persisted_completed=true`，4 files、2 cache hits、3 hash sessions、2 media decode jobs。原始输出、版本、配置、lock SHA、EXE 证据位于 `C:\tmp\rust-v2-cpu-io-ab\benchmark\A`。

- benchmark EXE：`C:\tmp\rust-v2-cpu-io-a-bench-target\x86_64-pc-windows-msvc\release\deps\base_compute_pipeline-33ce477ad6661485.exe`
- benchmark EXE SHA-256：`2c9429e54c14f64da1eb26eec081a1e73e97e912b0a364023bcf912956e915aa`

### 最终外置工具

工具由最终源码在 `C:\tmp\rust-v2-acceptance-tools-a-target` 增量重建，并复制到 formal release 外部的 `C:\tmp\rust-v2-acceptance-tools`：

- `runtime_acceptance.exe` SHA-256：`f5d407553a6c1a7538007989574471f630c3515a20a3354244cd3db1a354bc2a`。
- `export_scan_result_summary.exe` SHA-256：`88adace736822da8ff6a45feb67cc9430d17eba4ad6d2e993c1548916d2b06d1`。

### 最终 A test-only package

修复后的 Task7 builder 重新计算并校验 archive sidecar，marker 为 `RUST_V2_CPU_IO_TEST_PACKAGE_PASS`：

- test-only 根：`C:\tmp\rust-v2-cpu-io-ab\packages\A`
- formal ZIP：`C:\tmp\rust-v2-cpu-io-ab\packages\A\formal\A-formal.zip`
- ZIP SHA-256：`b60a8925080453a290406b6cdbff457cfef847de002a29d3f0360222392efbba`
- ZIP sidecar：`C:\tmp\rust-v2-cpu-io-ab\packages\A\formal\A-formal.zip.sha256`，内容文件名为 `A-formal.zip`，哈希与 ZIP 一致。
- metadata：`C:\tmp\rust-v2-cpu-io-ab\packages\A\test-package.json`
- metadata SHA-256：`b4694dab5ee50e9decffedc1580f3d175f288beff4ef78a5cc29382079bde868`
- expanded release root：`C:\tmp\rust-v2-cpu-io-ab\packages\A\release`
- manifest SHA-256：`765c3d98b46925f4d7bf1d23c747131f51c6c6a849281a7d319a99317f6eb6fa`

独立 `scripts\verify-release.ps1 -Package ...\A-formal.zip` 输出 `PACKAGE_PASS`、exit 0。真实 release root 枚举确认顶层恰有 `desktop.exe`、`node.exe`、`worker.exe`、`Everything.exe`，`runtime\ffmpeg` 恰有五个要求 DLL，无额外 EXE、data、数据库、验收客户端或 exporter；完整记录：`C:\tmp\rust-v2-cpu-io-ab\packages\A\boundary-check.json`，`boundary_pass=true`。

### 磁盘与清理记录

共享测试后 C 盘曾降至 `9.55 GiB`，按规则仅删除已盘点、可重建的旧 target `C:\tmp\rust-v2-task7-desktop-target`（约 `30.189 GiB`），未删源码、媒体、数据库、证据、包、凭据或 `I:\Tool`。清理后最终 A benchmark/工具/builder 后 C/D 约为 `36.07/18.97`、`35.47/18.97`、`34.77/18.97`、`32.33/18.97`、`32.12/18.97 GiB`；当前未触发新的清理。

### Task 8 checkpoint

`A_PACKAGE_FROZEN`：最终 A 的 benchmark、source-tree manifest、Cargo.lock、外置工具、formal ZIP、sidecar、manifest、metadata、expanded release root 已相互绑定。Task7 builder sidecar 修复及其 TDD/同 reviewer `REVIEW_CLEAN` 记录保留在 SDD 报告；本任务仍为 `COMMIT_DEFERRED_DIRTY_BASELINE`，不提交、不部署。

## Task 12：候选 B 全量回归、正式包和外置工具冻结

执行状态：`B_PACKAGE_FROZEN`；本任务采用 `COMMIT_DEFERRED_DIRTY_BASELINE`，未提交、未部署，未读取/写入/替换 `I:\Tool\mySingerServer-rust-v2-win-x64`。工作树为 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup`，分支 `codex/rust-v2-media-dedup`，HEAD `1f52f5ffb4a42ec3c1e0a996e6649507561d3812`。Cargo.lock SHA-256 固定为 `db7464102569bd4bbb1a4b756490e1b80a5159eefd1767b329ec8e309e8ac563`；所有 Cargo 命令均带 `--locked`，并在同一 PowerShell 进程清除了继承的 `CC`/`CXX`。

### Step 1：格式、静态检查和 workspace check

| 命令 | CARGO_TARGET_DIR | exit | 结果/证据 |
| --- | --- | ---: | --- |
| `cargo fmt --all -- --check` | `C:\tmp\rust-v2-cpu-io-b-check-target` | 1 | 工作树既有 `crates/protocol/tests/node_config_wire.rs` 等格式差异；未格式化或修改实现。原始日志：`C:\tmp\rust-v2-cpu-io-ab\task-12\step-1\cargo-fmt-check.log`。 |
| `git diff --check` | — | 0 | 通过；原始日志：`C:\tmp\rust-v2-cpu-io-ab\task-12\step-1\git-diff-check.log`。 |
| `cargo check --workspace --locked` | `C:\tmp\rust-v2-cpu-io-b-check-target` | 0 | workspace check 通过（约 70 秒）；原始日志：`C:\tmp\rust-v2-cpu-io-ab\task-12\step-1\cargo-check-workspace.log`。 |

Step 1 的首次编排因沙箱拒绝 `C:\tmp` 且命令数组漏传可执行文件，没有执行产品命令；该编排错误未计入产品结果，修正后的原始日志如上。Step 1 完成后按空间规则删除不再依赖的 `C:\tmp\rust-v2-cpu-io-b-check-target`（4.289 GiB）。

### Step 2：Rust 定向回归

12 条 brief 命令均按原顺序执行，原始日志和汇总在 `C:\tmp\rust-v2-cpu-io-ab\task-12\step-2`。共 `202 passed / 2 failed`；10 条命令 exit 0，2 条为真实测试断言失败：

| 测试 | 结果 |
| --- | --- |
| `dedup-protocol/runtime_tasks_wire` | 4/4，exit 0 |
| `dedup-node-store/result_summary_export`（`acceptance-tools`） | 20/20，exit 0 |
| `dedup-node-engine/disk_scheduler` | 24/24，exit 0 |
| `dedup-node-engine/runtime_tasks` | 14/14，exit 0 |
| `dedup-node-engine/base_compute_pipeline` | 51/51，exit 0 |
| `dedup-node-engine/base_compute_utilization` | 3/3，exit 0 |
| `dedup-node-engine/scan_parallelism` | 9/9，exit 0 |
| `dedup-node-engine/scan_runtime_details` | 10 passed / 1 failed，exit 101；`controlled_two_disks_and_actual_two_worker_slots_expose_live_known_totals` 断言“两盘首批读取必须进入两个真实 Worker slot”失败。 |
| `dedup-desktop-core/runtime_acceptance_contract` | 16/16，exit 0 |
| `dedup-desktop-ui/bindings_contract` | 15/15，exit 0 |
| `dedup-desktop-ui/window_contract` | 21/21，exit 0 |
| `dedup-desktop-ui/offscreen_layout` | 15 passed / 1 failed，exit 101；`annotated_pages_center_each_real_icon_text_group` 找不到“日志筛选”可访问元素。 |

上述两个失败均保留完整 stdout/断言位置；本任务未修改实现代码，不能把全量 Rust 回归标为全绿。

### Step 3：Windows 工具回归

五条命令均 exit 0，markers 如下；原始日志在 `C:\tmp\rust-v2-cpu-io-ab\task-12\step-3`：

| 命令 | marker |
| --- | --- |
| `Test-RustV2RuntimeAcceptanceHarness.ps1` | `RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_PASS` |
| `Test-RustV2RuntimeAcceptanceReport.ps1` | `RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS` |
| `Test-RustV2ResultSummary.ps1` | `RUST_V2_RESULT_SUMMARY_WIRING_PASS` |
| `Test-RustV2CpuIoAbReport.ps1` | `RUST_V2_CPU_IO_AB_REPORT_PASS` |
| `Test-RustV2Package.ps1` | `RUST_V2_PACKAGE_TEST_PASS` |

### Step 4：B source tree fingerprint

按 Task 1 与 Task 7 builder 的同一算法（HEAD 行；tracked + untracked 非 ignored 文件有序内容/大小哈希；排除 `.git`、`.superpowers/sdd`、`docs/verification`、`dist-rust-v2`、`target`；删除 tracked 文件记录 `DELETED`）生成：

- source evidence：`C:\tmp\rust-v2-cpu-io-ab\sources\B`；manifest：`source-tree.manifest`；manifest SHA/source-tree SHA：`b06f3ef6cf87cb4c8ce750d1849c426f732d045f8ca7e172cbfe273e8355b20a`。
- HEAD：`1f52f5ffb4a42ec3c1e0a996e6649507561d3812`；manifest 5853 行（5845 文件、7 deleted、18 excluded）；当前未忽略的 `.review-target-task11` 内容按算法实际计入，未擅自排除。
- `Cargo.lock.sha256`：`db7464102569bd4bbb1a4b756490e1b80a5159eefd1767b329ec8e309e8ac563`；tracked/untracked patch、status、目标文件哈希均保存在该 source evidence 根。
- 三轮稳定复算证据：`C:\tmp\rust-v2-cpu-io-ab\sources\B\source-stability.tsv`；run-01/run-02/run-03 均为 `b06f3ef6cf87cb4c8ce750d1849c426f732d045f8ca7e172cbfe273e8355b20a`、5853 行、5845 文件、7 deleted、18 excluded，marker `RUST_V2_B_SOURCE_FINGERPRINT_STABLE`。

### Step 5：唯一外置工具冻结

两个 release example 均使用 `C:\tmp\rust-v2-acceptance-tools-target`、`x86_64-pc-windows-msvc`、`--locked` 构建并 exit 0；复制完成后六轮禁止重建或替换：

- `C:\tmp\rust-v2-acceptance-tools\runtime_acceptance.exe`：SHA-256 `e669e144c607caaef5490c7b1a43ffa138147df82f62eed640826a862e86a52f`。
- `C:\tmp\rust-v2-acceptance-tools\export_scan_result_summary.exe`：SHA-256 `746e8bf88ca7384d6afe4a1cd037bec00210de8989354c582654caa1ce41209e`。

### Step 6：B formal package

正式 build/verify 链路由 `scripts/build-rust-v2-cpu-io-test-package.ps1` 调用并通过，marker `RUST_V2_CPU_IO_TEST_PACKAGE_PASS`、exit 0；原始输出：`C:\tmp\rust-v2-cpu-io-ab\task-12\step-6\build-package.log`。

- B package root：`C:\tmp\rust-v2-cpu-io-ab\packages\B`；formal ZIP：`C:\tmp\rust-v2-cpu-io-ab\packages\B\formal\B-formal.zip`；ZIP SHA-256：`91c14232eb8341a677020a7b65b90090878dbeccb16b5752a69ff34f6f2e5bfb`。
- B sidecar：`C:\tmp\rust-v2-cpu-io-ab\packages\B\formal\B-formal.zip.sha256`，内容绑定 `B-formal.zip`；sidecar 文件 SHA-256：`5e46ef8a1b66fb76c7aeaa92bf4996b7867a37aae981db7225d3d575c4b04655`。
- B expanded release：`C:\tmp\rust-v2-cpu-io-ab\packages\B\release`；formal manifest SHA-256：`9aff3a44799e591c94faf939f90bf18fbb66e264c9f2d49600d95eb2c7e6c14e`。
- B metadata：`C:\tmp\rust-v2-cpu-io-ab\packages\B\test-package.json`；metadata SHA-256：`9c433d613958a3b5d0ab9725a8a9320f1b539d0066bb2614fd7d12c17d6f0701`；source tree SHA 绑定上述 `b06f3ef6...`。

### Step 7：A metadata rebind 与 A/B formal 核验

最终工具路径/SHA 已写入 A 与 B test-only metadata，A formal ZIP、sidecar 和 expanded manifest 未修改：

- A formal ZIP：`C:\tmp\rust-v2-cpu-io-ab\packages\A\formal\A-formal.zip`；before/after SHA 均为 `b60a8925080453a290406b6cdbff457cfef847de002a29d3f0360222392efbba`；formal manifest before/after 均为 `765c3d98b46925f4d7bf1d23c747131f51c6c6a849281a7d319a99317f6eb6fa`；sidecar 原样不变；`a_formal_immutable=true`。
- A metadata before SHA `b4694dab5ee50e9decffedc1580f3d175f288beff4ef78a5cc29382079bde868`，rebind 后 SHA `de1acc47348c4b934f4f6b138f9b9e865b5ce2096a8fe80a2314df1ccbdc9586`；仅更新最终外置工具 SHA。
- A source tree SHA `7ddfb195000b05d65a2281e78fad63d1dad1366f9549b1fd8e9afef4e55d4c46`；B source tree SHA `b06f3ef6cf87cb4c8ce750d1849c426f732d045f8ca7e172cbfe273e8355b20a`；二者各自绑定对应 metadata/package。
- `scripts/verify-release.ps1` 对 A ZIP、A release、B ZIP、B release 均输出 `PACKAGE_PASS`、exit 0；日志在 `C:\tmp\rust-v2-cpu-io-ab\task-12\step-7`。
- A/B 实物 boundary、sidecar、metadata、EXE/DLL 集合汇总：`C:\tmp\rust-v2-cpu-io-ab\task-12\step-8\boundary-metadata.json`，A/B 均 `boundary_pass=true`。四个顶层 EXE 恰为 `desktop.exe`、`Everything.exe`、`node.exe`、`worker.exe`；FFmpeg 恰为五个要求 DLL；无 data/数据库/外置工具混入。首次顺序比较编排误报保留为 `boundary-metadata-attempt-01.json`，修正后集合核验通过。

### Step 8 checkpoint、自审与空间

`B_PACKAGE_FROZEN`：A/B formal ZIP、manifest、metadata、source tree、Cargo.lock、统一外置工具、sidecar、expanded release root 已相互绑定；六轮前不再重建工具/包，此时仍不部署。唯一仓库交付文件为本实施账本；本任务 before 副本和 SHA 在 `.superpowers/sdd/2026-08-24-cpu-io-active-seat-smoothing/task-12-before`。

空间规则实际触发两次，均先盘点绝对路径再清理可再生旧 target：

- Step 2 后 C/D `5.240/15.357 GiB`；删除 `C:\tmp\rust-v2-cpu-io-b-check-target`（4.289 GiB）、`C:\tmp\rust-v2-acceptance-tools-a-target`（0.741 GiB）、`C:\tmp\rust-v2-cpu-io-a0-target`（0.609 GiB），保留 A/B 包、source、evidence、最终 tools 与 A benchmark/B target；清理后 C/D `10.082/15.357 GiB`。
- Step 5 后 C/D `9.385/15.357 GiB`；删除旧 `C:\tmp\rust-v2-task9-msvc-target`（2.729 GiB）和 `C:\tmp\rust-v2-task10-review-target`（3.094 GiB），保留当前 B target、A/B 包/source/tools/evidence；清理后 C/D `14.860/15.357 GiB`。
- Step 8 最终 C/D `12.424/15.356 GiB`；未删除源码、SDD/verification 证据、包、source manifest、工具冻结目录、Git 元数据、用户文件或 `I:\Tool`。

自审：Step 1 的格式检查失败和 Step 2 的两个测试失败已按原始输出记录，未将其伪装为通过；B package/metadata/boundary/sidecar 已由独立 verifier 与实物核验确认；未修改 Rust/proto/PowerShell 实现。Task 12 专属 before→after unified diff：`.superpowers/sdd/2026-08-24-cpu-io-active-seat-smoothing/review-task-12-working-tree.diff`。

### Concerns 修复轮 1：最终冻结

原始 Step 1–2 的失败先按 RED 保留并完成四阶段根因调查：`node_config_wire.rs` 仅为 rustfmt 机械差异；`scan_runtime_details` 的“两盘首批读取”失败是测试夹具在两个真实 Worker slot 开始前释放 gate 的时序竞态，不是 Task 9–11 active-seat/worker admission 产品回归；`offscreen_layout` 缺少日志筛选可访问元素则违反当前 UI 契约。权威设计规范 `docs/superpowers/specs/2026-08-19-rust-v2-concept-ui-design.md:250` 要求日志筛选/导出/清空/环境版本在无接口时保留布局并禁用标注“当前版本未提供”。

最小 GREEN 变更为：`cargo fmt --all` 机械格式化 `crates/protocol/tests/node_config_wire.rs`；让 `ReadGate` 等待首批路径的显式 release 并在 release 前等待两个 `wait_started` 事件；在真实 `settings-workspace.slint` 的 `DisabledFeatureRow` 中补齐四个禁用元素并放在诊断状态卡后、路径卡前以保持页面几何契约。未延长 sleep、放宽断言、降低门禁或修改生产 Rust/PowerShell/proto。

修复后 `cargo fmt --all -- --check`、`git diff --check`、`cargo check --workspace --locked` 均 exit 0；brief Step 2 十二条定向回归 `204/204` 全绿（4、20、24、14、51、3、9、11、16、15、21、16）；Step 3 五个 Windows marker 均 exit 0：`RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_PASS`、`RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS`、`RUST_V2_RESULT_SUMMARY_WIRING_PASS`、`RUST_V2_CPU_IO_AB_REPORT_PASS`、`RUST_V2_PACKAGE_TEST_PASS`。完整修复证据在 `C:\tmp\rust-v2-cpu-io-ab\task-12-fix-round-1`；其中一次工具参数编排错误未启动产品命令，已保留并与 corrected marker 区分。

最终 B source tree SHA 为 `0bedfb87e8637a9ee96c713d52e8439452649836422fdebb23b53998e26121cb`，三轮稳定；manifest `5853` 行（5845 FILE、7 DELETED、18 excluded），Cargo.lock SHA 仍为 `db7464102569bd4bbb1a4b756490e1b80a5159eefd1767b329ec8e309e8ac563`。唯一外置工具已在 `C:\tmp\rust-v2-acceptance-tools-target` 重新构建并冻结：`runtime_acceptance.exe` SHA `e669e144c607caaef5490c7b1a43ffa138147df82f62eed640826a862e86a52f`，`export_scan_result_summary.exe` SHA `746e8bf88ca7384d6afe4a1cd037bec00210de8989354c582654caa1ce41209e`。

最终 B test-only package：`C:\tmp\rust-v2-cpu-io-ab\packages\B\formal\B-formal.zip`，ZIP SHA `da4be8e6bc64d1158ad4d1c696aa2d26a7596f9a39ebcf1a6a6ef51fe172a6f4`，sidecar SHA `30dd195709f0d0d466b29427b7b70f69c1eba43f6b8b23205b349072988fa1d9`，expanded manifest SHA `18fb9febd09094f9405efb4076432d5b1b608f8e398874c815dbf062ce16059a`，metadata SHA `8900c45632be7f2124530fff5c5801ae8b7be9e082afc4f4ed40727a4e0cb032`。A formal ZIP/manifest/sidecar 保持 `b60a8925080453a290406b6cdbff457cfef847de002a29d3f0360222392efbba` / `765c3d98b46925f4d7bf1d23c747131f51c6c6a849281a7d319a99317f6eb6fa` / `694687424d8a1d6575b55aa935a26d87b1e8bf4ffe0dc9b46d2ea0612d847b8d`，A metadata before/after 均为 `de1acc47348c4b934f4f6b138f9b9e865b5ce2096a8fe80a2314df1ccbdc9586`；A/B 四条 formal verifier 均 `PACKAGE_PASS`，boundary metadata marker 为 `TASK12_STEP8_BOUNDARY_METADATA_PASS` 且 A/B `boundary_pass=true`。

修复前旧 B package/source/tools 已分别移动到 `packages\B-superseded-fix-round-0`、`sources\B-superseded-fix-round-0`、`C:\tmp\rust-v2-acceptance-tools-superseded-fix-round-0` 保存；旧 ZIP/source SHA `91c14232eb8341a677020a7b65b90090878dbeccb16b5752a69ff34f6f2e5bfb` / `b06f3ef6cf87cb4c8ce750d1849c426f732d045f8ca7e172cbfe273e8355b20a` 明确 superseded。修复后唯一仓库源码/测试变更为上述三文件和本账本，dirty baseline 其余内容保留；仍 `B_PACKAGE_FROZEN`、`COMMIT_DEFERRED_DIRTY_BASELINE`，不部署。

### fix round 2：工具 provenance gap 闭环与最终顺序冻结

最终 Step 1–3 已在 `15:44:28–15:49:39` 全绿并作为本轮输入；round2 不修改源码、测试或 UI。此前 `task-12-fix-round-1/step-5/build-tools.log` 只有丢参后的 Cargo help 和 exit 0，已明确标记无效。该轮 B/source/tools 精确保存为 `B-superseded-fix-round-1-provenance-gap`、`B-superseded-fix-round-1-provenance-gap` 和 `rust-v2-acceptance-tools-superseded-fix-round-1-provenance-gap`，不得复用。

round2 按 Step 4→8 严格重做：B source-tree SHA `0bedfb87e8637a9ee96c713d52e8439452649836422fdebb23b53998e26121cb` 三轮稳定，manifest 5853 行（5845 FILE、7 DELETED、18 excluded），Cargo.lock SHA `db7464102569bd4bbb1a4b756490e1b80a5159eefd1767b329ec8e309e8ac563`。两条外置工具命令均显式带完整参数、`--locked`、MSVC target，并在独立日志写出 `Finished` 与 exit 0；最终工具 SHA 为 `e669e144c607caaef5490c7b1a43ffa138147df82f62eed640826a862e86a52f` / `746e8bf88ca7384d6afe4a1cd037bec00210de8989354c582654caa1ce41209e`。

最终 B package：ZIP `86416c2b91b1b97ae3867296fd279bc411c6e2baa985104b5150fa6e6c0284cd`，sidecar `89e4bb8156f287c737ff3715de807c0e6d543569fc4eb988c14053b1cd78399e`，manifest `18fb9febd09094f9405efb4076432d5b1b608f8e398874c815dbf062ce16059a`，metadata `c986a9947c107dedae2c960c876dd68fbf9d5df38764685e5e46631cc7a487e6`。A ZIP/manifest/sidecar 仍为 `b60a8925080453a290406b6cdbff457cfef847de002a29d3f0360222392efbba` / `765c3d98b46925f4d7bf1d23c747131f51c6c6a849281a7d319a99317f6eb6fa` / `694687424d8a1d6575b55aa935a26d87b1e8bf4ffe0dc9b46d2ea0612d847b8d`，A metadata 未变化；四条 formal verifier 均 `PACKAGE_PASS`，boundary marker `TASK12_FIX_ROUND2_BOUNDARY_METADATA_PASS` 且 A/B `boundary_pass=true`。

round2 仍为 `B_PACKAGE_FROZEN`、`COMMIT_DEFERRED_DIRTY_BASELINE`，不部署；当前 C/D 约 `12.93/15.36 GiB`，未触发清理。最终 Step 4–8 证据目录：`C:\tmp\rust-v2-cpu-io-ab\task-12-fix-round-2`。

## Task 13：固定基准与六轮真实媒体 A/B

执行状态：`BLOCKED_WITH_EVIDENCE`；本任务保持 `COMMIT_DEFERRED_DIRTY_BASELINE`，没有提交、部署、生产目录复制或读取/写入/替换 `I:\Tool\mySingerServer-rust-v2-win-x64`。Task 13 开始前保存的实施账本副本为 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup\.superpowers\sdd\2026-08-24-cpu-io-active-seat-smoothing\task-13-before\implementation.md`，SHA-256 `0796a49dbefba9b7746fb29bfbb4162913b8c1361aa59860919dd6f7ec340b12`；当时 dirty/untracked 状态 177 行，status SHA-256 `2eb6d79386b74f5b05e92c474af0df7d3733fd24842836fc51d365e2b9338feb`。

### Step 1：运行输入核验

输入、边界和路径核验通过：A/B metadata 分别为 `C:\tmp\rust-v2-cpu-io-ab\packages\A\test-package.json` / `B\test-package.json`；A/B formal ZIP SHA-256 为 `b60a8925080453a290406b6cdbff457cfef847de002a29d3f0360222392efbba` / `86416c2b91b1b97ae3867296fd279bc411c6e2baa985104b5150fa6e6c0284cd`；A/B release root 分离且由各自 formal ZIP 展开；两工具 SHA-256 为 `e669e144c607caaef5490c7b1a43ffa138147df82f62eed640826a862e86a52f` / `746e8bf88ca7384d6afe4a1cd037bec00210de8989354c582654caa1ce41209e`；两 package 的 config fingerprint 均 `4c39ae398b8c8934924c8d6f436c693be598e5a2c50b18a90855d5223e0ef230`；Cargo.lock SHA-256 `db7464102569bd4bbb1a4b756490e1b80a5159eefd1767b329ec8e309e8ac563`；显式媒体根为 `I:\tmp`，存在且不在输出根/仓库/`I:\Tool` 内；六轮输出根启动前不存在。启动 C 盘可用字节 `13640871936`，D 盘可用空间约 `15.36 GiB`。完整 preflight 记录及 SHA 在 `C:\tmp\rust-v2-cpu-io-ab\task-13\preflight.txt` / `checkpoint.tsv`。

### Step 2–3：B benchmark

按 brief 冻结命令创建 `C:\tmp\rust-v2-cpu-io-ab\benchmark\B`，清除 `CC/CXX` 并核对 `Cargo.lock`。初次编排的字节差异已完整保留在 `C:\tmp\rust-v2-cpu-io-ab\benchmark\B-initial-mismatch`；控制器 Ruling 8 明确以 A 实物语义为权威，随后用 `rustc -Vv`、`cargo -Vv`、LF-only、最终 LF 重生成 B 三文件，逐项与 A SHA 相等：`rustc-version.txt` `7fad09dd8c16d49de37932475436003b5b1763c1015c5ce081ad7692db368ca3`、`cargo-version.txt` `d950eb0ce4b58c936e802cfffba2b474fd5b60951a2c3702851b749699883221`、`benchmark-config.json` `7f3c9524bfc925cccfec04cb318c948a03b5c7c4825b23dc969617da29594715`。B 三轮实际 `elapsed_ms` 为 `131.156/134.882/106.967`，中位数 `131.156 ms`；A 中位数 `115.946 ms`，改善 `-13.117%`，固定 benchmark ≥15% 门禁 `FAIL`。原始日志在 `C:\tmp\rust-v2-cpu-io-ab\benchmark\B\run-01.log`、`run-02.log`、`run-03.log`，B benchmark EXE 为 `C:\tmp\rust-v2-cpu-io-b-bench-target\x86_64-pc-windows-msvc\release\deps\base_compute_pipeline-33ce477ad6661485.exe`，SHA-256 `d0ff2470fbe6e245f7f8f4325663f43c1f2d5ff588dbff23eb7e0879233d016c`。

### Step 4–5：六轮真实媒体尝试

实际执行了 brief 固定命令（`RUST_V2_REAL_MEDIA_ROOT=I:\tmp`、Duration=1800、Sample=2、Worker12/HDD1/SSD16/Unknown1/TotalRead16/Reserved1、输出根 `C:\tmp\rust-v2-cpu-io-ab\runs`）。输出根及 top-level media manifest 已创建，媒体语义 SHA `333f9b747d6bf7aa85f8da868e85ebe181eb072eebd73df7cf95ddaf1e211fec`，manifest 文件 SHA `0e85f03f5b973f2f67e43ddf383238eaf02c31e852ca657c353646199f41b919`。固定顺序第一轮 A-1 在正式 Node/Worker 启动前约 1.8 秒基础设施失败，序列按规则停止，未进入 B-1；未拼接任何旧轮。

失败边界是：`Measure-RustV2CpuIoAb.ps1` 在 `A-1\evidence` 预创建证据目录，随后调用的 `Measure-RustV2RuntimeAcceptance.ps1` 严格校验该目录必须不存在，抛出 `RUST_V2_ACCEPTANCE_EVIDENCE_EXISTS`（行 1372）。外层 exit `1`；`A-1` manifest 状态 `INCONCLUSIVE`，A-1 before/after 媒体语义 SHA 均为上述值，未发现媒体变更。原始证据：`C:\tmp\rust-v2-cpu-io-ab\runs\A-1\runner.stderr.log` SHA-256 `24d4632f20965336c81a60d123dbff0adb04d8ff7ee1b3dfd2ee214bd39a7db1`，`A-1\ab-run-result.json` SHA-256 `ccab6b4b47d568c33c4bd3c40c5059017505a281507c5477a5d4a0e452bc6479`，顶层 manifest SHA-256 `245d54ff6753dafc7ea3af68c7661eb10d0319ccf9e3d8a7b8b1e4ffa64bf392`；任务命令日志为 `C:\tmp\rust-v2-cpu-io-ab\task-13\measure-six.stdout-stderr.log`，exit 记录为 `measure-six.exit.txt`。

按 brief，单轮基础设施失败后必须保留证据、停止序列；修复基础设施后须换新 output root 整组六轮重来。本任务边界禁止修改脚本，因此没有删除 `runs`、没有绕过证据校验、没有重跑或拼接六轮。C 盘失败后可用字节 `13636665344`，未触发清理规则。

### Step 6–9：最终裁决与 checkpoint

冻结聚合脚本以当前不完整根运行一次，exit `3`、marker `RUST_V2_CPU_IO_AB_REPORT_INCONCLUSIVE`，因缺少其余五轮在内部索引处停止，未生成自动 final；此脚本失败本身已记录为证据，不伪造报告。正确性硬门禁：六轮 total/succeeded/failed/skipped、queued/running、canonical result SHA、ownership 守恒、snapshot coverage、Node/Worker 退出均为 `INCONCLUSIVE`；仅有 A-1 的 before/after 媒体语义 SHA，canonical result SHA 缺失。十项真实媒体性能门禁均为 `INCONCLUSIVE`；fixed benchmark 门禁为 `FAIL`（A `115.946 ms`、B `131.156 ms`、改善 `-13.117%`，要求至少 15%）。

所有命令、exit、原始 evidence root、SHA 与失败边界汇总在 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup\.superpowers\sdd\2026-08-24-cpu-io-active-seat-smoothing\task-13-report.md`；Task 13 专属 before→after diff 为 `review-task-13-working-tree.diff`。未提交、未打标签、未部署。

## Task 13：用户覆盖后的最终状态

早期失败根和修复轮全部原样保留。round9 最终 B source fingerprint 为 `fbd2d41763a60f1ddc2461198529e8e76d9c5a1d307bb2441e976bf040e834b3`；B formal ZIP `0ccd94e3fa7c2ebf7f4d6f1b8ad10904e5f94aa7115b40b7293004f50dacde47`；metadata `b966eac9e1c8f202551306cac853022ce5b404b5611d910157915ff23cc36df2`；manifest `34c17388d349b9e5f827e25bccd9c14349b0325978c8b6c47950abc407015b09`。固定 benchmark B 三轮 `129.940/126.830/136.577 ms`，中位数 `129.940 ms`；相对 A `115.946 ms` 的改善为 `-12.069412%`，15% 门禁 `FAIL`。

用户随后明确覆盖 Task 13 真实媒体流程：不再执行六轮或 A-3/B-3，也不要求等待满 1800 秒；仅使用候选 B、Worker20、Read12 运行一次全量媒体，任务进入终态即完成。第一遍 runtime task `e1e91ec6-dc0e-4a12-bb0a-94d0902d5824` 在 `760 s` 进入 `completed`；自动开始的第二遍被排除并停止，未纳入统计。

第一遍持久 task `01a03c82-ce3c-78a0-abf6-6a15a2fc0242` 为 completed，14786 项中 14757 succeeded、29 failed、0 cancelled。29 项由 8 个 FFmpeg 不可解码输入与 21 个 Worker 管道协议帧截断组成。停机后只读 DB/WAL/SHM 快照稳定，结果 JSONL 14786 行、SHA-256 `93c95980c7d00b4caef8f1b8140ad065e0f31852653e3ab025bc362773fba664`，metadata/lease/JSONL 绑定有效，但严格 overall status 为 `INCONCLUSIVE`。媒体 before/after 文件 SHA-256 均为 `03a554790321db83f9bac2f72dbb07c26ae50d73e8f8d40fcaf5a6364ec4dc31`，文件数与总字节一致。

计算阶段含尾段平均活跃 Worker `9.634/20`，高 I/O 样本中平均空闲 Worker `13.815`、Worker CPU `6.498` 核、读队列 `13.173`；20 Worker 全活跃时磁盘吞吐和队列下降。约 `107.950 s` 最终尾段没有新增完成项且所有 Worker idle。结论为 Worker20/Read12 仍存在 CPU/I/O 相位分离，不支持继续单纯提高 Worker 或读取线程。详细报告：`docs/verification/2026-08-26-worker20-read12-single-run.md`。

## Task 14：最终验证与交付边界

最终工作树执行新鲜静态与行为门禁：

- `git diff --check` PASS；`cargo fmt --all -- --check` PASS。
- Rust 定向测试：protocol `4/4`、result summary `20/20`、disk scheduler `24/24`、base pipeline `51/51`、utilization `3/3`、runtime tasks `14/14`、runtime acceptance contract `16/16`，全部 PASS。
- Windows fixtures：`RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_PASS`、`RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS`、`RUST_V2_CPU_IO_AB_REPORT_PASS`、`RUST_V2_PACKAGE_TEST_PASS`。
- A/B formal ZIP 分别执行 `scripts\verify-release.ps1`，均 exit 0 / `PACKAGE_PASS`；sidecar、manifest、4 个 x64 EXE、5 个 FFmpeg DLL、禁入文件、source/package/tool metadata 绑定均通过。

最终冻结引用：A ZIP `b60a8925080453a290406b6cdbff457cfef847de002a29d3f0360222392efbba`、source `7ddfb195000b05d65a2281e78fad63d1dad1366f9549b1fd8e9afef4e55d4c46`；B ZIP `0ccd94e3fa7c2ebf7f4d6f1b8ad10904e5f94aa7115b40b7293004f50dacde47`、source `fbd2d41763a60f1ddc2461198529e8e76d9c5a1d307bb2441e976bf040e834b3`；外置 runtime/exporter SHA 仍为 `e669e144c607caaef5490c7b1a43ffa138147df82f62eed640826a862e86a52f` / `746e8bf88ca7384d6afe4a1cd037bec00210de8989354c582654caa1ce41209e`。

最终裁决为 `FAIL`：固定 benchmark 已确认未达到 15% 硬门禁；单次真实媒体严格结果为 `INCONCLUSIVE`，原六轮 A/B 性能门禁也因用户覆盖保持 `INCONCLUSIVE`。静态、行为与包边界通过不能覆盖性能失败。A formal ZIP 仅作为测试回滚参考；未部署、未执行生产回滚、未读取/写入/替换 `I:\Tool`。工作树保留 `COMMIT_DEFERRED_DIRTY_BASELINE`，不宽泛 stage 或提交用户既有 hunk。
