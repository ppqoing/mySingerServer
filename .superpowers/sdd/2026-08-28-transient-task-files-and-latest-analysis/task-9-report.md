# Task 9：最近分析 TSV 安全发布报告

## 范围

- 起点 HEAD：`6b6731dd4c72592367dc94a6b83b11fe707cbc54`，开始时工作树干净。
- 实现了同目录 Windows `MoveFileExW` 原子替换，以及最近一次分析的固定 UTF-8/LF TSV 写入、发布和校验。
- 未接入 Task 10 的 `LocalAnalysisEngine`，未修改 actor、proto、UI；未实现 Task 11 滑动窗口；未写 JSON、`.idx`、历史结果或恢复逻辑。
- 未触碰 `I:\Tool`。

## TDD 记录

先写入真实临时目录行为测试，再运行定向 RED：

```text
error[E0432]: unresolved import `dedup_windows::atomic_replace_file`
error[E0432]/[E0425]: analysis 中不存在 AnalysisResultWriter、verify_result_file 及相关结果类型
```

随后最小实现并运行 GREEN：

```text
cargo test -p dedup-windows --test atomic_file --locked -- --test-threads=1
4 passed; 0 failed

cargo test -p dedup-node-engine --test analysis_result_file --locked -- --test-threads=1
3 passed; 0 failed
```

覆盖的真实边界：旧文件替换、首次发布、缺失 source、`CreateFileW` share=0 锁定目标时旧字节保留；H/M/F 固定列、无 BOM/LF、F 前 SHA-256、成员数、旧 result 在 discard/写入失败时保留、非有限分数和 TSV 控制字符拒绝、篡改结果拒绝。

## 回归与门禁

```text
cargo test -p dedup-windows --locked -- --test-threads=1
23 passed; 0 failed; 1 ignored

cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1
142 passed; 0 failed

cargo fmt --all -- --check
passed

git diff --check
passed
```

默认 feature 的 `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` 也已尝试，但在本任务未修改代码的既有测试夹具配置差异处失败：`analysis/phase2.rs` 导入受 `test-hooks` feature 限制的 `crate::scan::BasePersistTestController`（E0432）。按既有正确组合启用 `test-hooks` 后 142 项通过；未对该范围外问题作修改。
