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
