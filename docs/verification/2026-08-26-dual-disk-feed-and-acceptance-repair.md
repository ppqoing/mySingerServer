# Rust V2 双盘任务供给与验收可靠性修复：基线与最终回归账本

日期：2026-08-26

工作树：`D:\code\mySingerServer\.worktrees\rust-v2-media-dedup`

分支：`codex/rust-v2-media-dedup`

## 基线冻结

- HEAD：`1f52f5ffb4a42ec3c1e0a996e6649507561d3812`
- 工作树为既有 dirty/untracked 基线；原始 `git status --short` 保存在 `C:\tmp\rust-v2-dual-feed-repair\baseline-status.txt`。
- 目标文件、`Cargo.lock`、设计文档和旧真实媒体报告的存在性/SHA-256 清单：`C:\tmp\rust-v2-dual-feed-repair\baseline-file-sha256.json`。
- 旧报告 `docs/verification/2026-08-26-dual-physical-disk-single-run.md` SHA-256：`4C0A1333293E946B5B966419ECDA48F3F545DE36E71DBB9AB66106AB031209E2`。
- 工具链：rustc/cargo `1.97.1`，target `x86_64-pc-windows-msvc`；详情见 `C:\tmp\rust-v2-dual-feed-repair\toolchain.txt`。
- 空间快照：C free `24866070528` bytes（约 23.16 GiB），D free `19870326784` bytes（约 18.51 GiB）；低于 10 GiB 清理规则未触发。未删除任何目录。

## 问题三分法与执行边界

本账本对应三个独立问题：多扫描根请求在 BaseCompute 前按路径前缀串行供给；Worker 退出导致系统采样器属性读取竞态；SQLite 首次只读打开引起 sidecar 首次变更误判。未修改产品代码，未运行真实媒体，未访问 `I:\Tool`，未部署。旧运行根仍以原报告为准；Worker 崩溃/ACK 风险保持 `NON_DEPLOYABLE`。

固定标记：`NO_REAL_MEDIA_RERUN`、`NO_I_TOOL_ACCESS`、`COMMIT_DEFERRED_DIRTY_BASELINE`。

## 环境预检失败（不计入冻结基线）

固定命令（每轮一次，`CARGO_TARGET_DIR=C:\tmp\rust-v2-dual-feed-baseline-target`）：

`cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked`

以下三次是环境预检失败，不是 Task 1 规定的有效三轮，不产生基线裁决：均在链接阶段失败，退出码均为 101；错误为 `libsqlite3_sys` 引用未解析符号 `___chkstk_ms`（LNK2019/LNK1120）。它们没有可执行 benchmark、`elapsed_ms` 或 `persisted_completed`；唯一冻结基线仍是后文清除环境变量后成功三轮的 **131.769 ms** 中位数。

| 轮次 | 退出码 | wall elapsed_ms | elapsed_ms | persisted_completed | EXE/SHA-256 |
|---|---:|---:|---|---|---|
| 1 | 101 | 62383.751 | BUILD_FAILED_NO_METRIC | BUILD_FAILED_NO_METRIC | MISSING / MISSING |
| 2 | 101 | 2987.225 | BUILD_FAILED_NO_METRIC | BUILD_FAILED_NO_METRIC | MISSING / MISSING |
| 3 | 101 | 2971.154 | BUILD_FAILED_NO_METRIC | BUILD_FAILED_NO_METRIC | MISSING / MISSING |

每轮原始 stdout 和 JSON 记录分别位于：
`C:\tmp\rust-v2-dual-feed-repair\benchmark-round-{1,2,3}.stdout.txt` 与 `benchmark-round-{1,2,3}.json`。

## Checkpoint

### MSVC 环境修复后的冻结基线

此前三轮失败已归档为环境预检失败（保留原日志）：`CC/CXX` 指向 MinGW，导致 bundled SQLite 以 MinGW 目标生成并在 MSVC link 阶段缺失 `___chkstk_ms`。清除 `Env:CC` 与 `Env:CXX` 后，在新 target `C:\tmp\rust-v2-dual-feed-baseline-msvc-target` 严格运行三轮，均成功且 `persisted_completed=true`。

| 轮次 | 退出码 | wall elapsed_ms | elapsed_ms | persisted_completed | EXE SHA-256 |
|---|---:|---:|---:|---|---|
| 1 | 0 | 35690.656 | 136.552 | true | `5801C196FE5653D06C14F3FBC482B85881DEF9DD184700B418FE42D596C85658` |
| 2 | 0 | 464.493 | 131.769 | true | `5801C196FE5653D06C14F3FBC482B85881DEF9DD184700B418FE42D596C85658` |
| 3 | 0 | 448.454 | 125.864 | true | `5801C196FE5653D06C14F3FBC482B85881DEF9DD184700B418FE42D596C85658` |

EXE：`C:\tmp\rust-v2-dual-feed-baseline-msvc-target\x86_64-pc-windows-msvc\release\deps\base_compute_pipeline-33ce477ad6661485.exe`。Task 1 冻结前基线中位数：**131.769 ms**。三轮 stdout/JSON：`C:\tmp\rust-v2-dual-feed-repair\benchmark-msvc-round-{1,2,3}.*`。

- `git diff --check`：退出码 0（仅显示既有工作树的 LF/CRLF 警告，无 whitespace error）。
- 未执行 git stage/commit/reset/clean；未改变既有 dirty/untracked 文件。
- reference 文件 `crates/node-engine/benches/base_compute_pipeline.rs`、`scripts/build-release.ps1`、`scripts/verify-release.ps1`、`tests/windows/Test-RustV2Package.ps1` 已补入 `baseline-file-sha256.json`。
- 唯一冻结前 synthetic benchmark 中位数为 **131.769 ms**，可供 Task 9 使用；此前 MinGW 链接失败仅作为环境预检记录，不是当前基线。

## Task 9 审查修复后最终门禁（2026-08-27）

本节为最终审查修复后的唯一裁决依据。首轮审查前候选仍保留在 `C:\tmp\rust-v2-dual-feed-repair\task9`，其 ZIP SHA-256 为 `538CF0D4F7695001DAFE3D12B2837B10152547041358C24AE47A09FB203C4D62`，不作为最终结果引用。最终证据独立保存于 `C:\tmp\rust-v2-dual-feed-repair\task9-final`。

本轮源码已包含 I-01 actor 生产边界、I-02 采样器 null 属性边界、I-03 SQLite 首次真实读取顺序三项修复；只验证确定性 fixture、Windows 验收夹具、合成 benchmark 和隔离正式包；没有再次运行真实媒体，没有访问 `I:\Tool`，没有部署。

### Rust 定向门禁

统一使用 `CARGO_TARGET_DIR=C:\tmp\rust-v2-dual-feed-final-target`、MSVC target、清除继承的 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，并以 `--locked -- --test-threads=1` 执行。10 项全部通过，共 175 个测试：

| 门禁 | 结果 |
|---|---:|
| `dedup-node-store/result_summary_export`（acceptance-tools） | 22/22 |
| `dedup-protocol/runtime_tasks_wire` | 4/4 |
| `dedup-node-engine/enumerators`（test-hooks） | 4/4 |
| `dedup-node-engine/disk_scheduler`（test-hooks） | 27/27 |
| `dedup-node-engine/base_compute_pipeline`（test-hooks） | 59/59 |
| `dedup-node-engine/runtime_tasks`（test-hooks） | 17/17 |
| `dedup-node-store/task_recovery` | 9/9 |
| `dedup-desktop-core/runtime_acceptance_contract` | 22/22 |
| `dedup-node-engine/node_actor`（test-hooks） | 10/10 |
| `dedup-node-engine/runtime_recovery`（test-hooks） | 1/1 |

上述 175 项为最终集成命令计数；actor 私有的两条惰性依赖错误路径单测另以 focused 2/2 通过，见 `final-fix-i01-report.md`，不计入 `node_actor` 10/10 或 BaseCompute 59/59。

逐项 stdout/stderr、exit code 和 C/D 前后空间记录见 `C:\tmp\rust-v2-dual-feed-repair\task9-final\rust`。

### Windows 与格式门禁

- `Test-RustV2RuntimeAcceptanceHarness.ps1`：`RUST_V2_RUNTIME_ACCEPTANCE_HARNESS_PASS`。
- `Test-RustV2RuntimeAcceptanceReport.ps1`：`RUST_V2_RUNTIME_ACCEPTANCE_REPORT_PASS`。
- `Test-RustV2Package.ps1`：`RUST_V2_PACKAGE_TEST_PASS`。
- `cargo fmt --all -- --check`：exit 0。
- `git diff --check`：exit 0。构建后再次执行 verify、Package Test、fmt、diff-check，均 exit 0；日志见 `C:\tmp\rust-v2-dual-feed-repair\task9-final\final-checks`。

### 合成 benchmark

命令为 `cargo bench -p dedup-node-engine --bench base_compute_pipeline --locked`，target 为 `C:\tmp\rust-v2-dual-feed-bench-final-target`。三轮均 exit 0 且 `persisted_completed=true`：

| 轮次 | `elapsed_ms` |
|---|---:|
| 1 | 129.191 |
| 2 | 125.788 |
| 3 | 126.638 |

候选中位数 **126.638 ms**；冻结基线 **131.769 ms**，5% 上限 **138.35745 ms**，`SYNTHETIC_REGRESSION_GATE=PASS`。Benchmark EXE 为 `C:\tmp\rust-v2-dual-feed-bench-final-target\x86_64-pc-windows-msvc\release\deps\base_compute_pipeline-33ce477ad6661485.exe`，SHA-256=`916B2CDC4B3B2611E7DE93941A8BFEE5745E0D1CA4DD521911F6BD4DDE55BA7F`。

### 隔离正式包

使用 `scripts\build-release.ps1 -CargoTargetDir C:\tmp\rust-v2-dual-feed-final-target` 构建，并显式执行 `scripts\verify-release.ps1`，两者均 exit 0，`RUST_V2_RELEASE_BUILD_PASS`、`PACKAGE_PASS` 均出现。

- ZIP：`D:\code\mySingerServer\.worktrees\rust-v2-media-dedup\dist-rust-v2\mySingerServer-rust-v2-win-x64.zip`
- ZIP SHA-256=`989F1D964D1591FBDF7CCADB1CE7672210A21F3562B4FEF586674ACDAE338698`
- sidecar 文件 SHA-256=`3560BA450AB056EB24427F23979847286587BD8D1F7EC57914D1D062A4AE0302`，内容中的文件名与 ZIP basename 一致。
- manifest `staging\manifest\files.sha256` SHA-256=`6D59A05581851DE61A1204E1DE4D63D37D8D9791C1E7D6F2040545DF0B048577`。
- 顶层 EXE 恰为 `desktop.exe`、`node.exe`、`worker.exe`、`Everything.exe`；包内无额外 EXE，明确无 `runtime_acceptance.exe`、`export_scan_result_summary.exe`。
- ZIP、sidecar、manifest 和 `package-evidence.json` 已复制到 `C:\tmp\rust-v2-dual-feed-repair\task9-final\package`，未复制到生产目录。源副本和证据副本 SHA 一致；包内仅四个顶层 EXE，无 acceptance client/exporter。

### 空间与最终裁决

本轮每个重型命令前后均检查 C/D；最低约为 C=12.52 GiB、D=18.51 GiB，未触发 10 GiB 停止线，未删除文件。旧 `task9` 证据、最终 `final/bench` target、日志、报告、ZIP 和哈希证据均保留。

最终标记：

- `SAMPLER_RACE_FIXED=PASS`
- `RESULT_EXPORT_OPEN_ORDER_FIXED=PASS`
- `MULTI_ROOT_REQUEST_VISIBILITY_FIXED=PASS`
- `PER_DISK_TELEMETRY_COMPLETE=PASS`
- `SYNTHETIC_REGRESSION_GATE=PASS`
- `PACKAGE_STRUCTURE_GATE=PASS`
- `REAL_MEDIA_RUN=NOT_EXECUTED`
- `REAL_MEDIA_ACCEPTANCE=NOT_RUN`
- `DEPLOYMENT=NON_DEPLOYABLE`：Worker 崩溃/ACK 风险仍未独立关闭，且本轮未获得生产部署授权。
