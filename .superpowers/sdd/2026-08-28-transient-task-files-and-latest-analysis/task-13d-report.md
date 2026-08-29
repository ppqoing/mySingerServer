# Task 13D：外置真实媒体验收客户端报告

## 范围

本任务只修改：

- `crates/desktop-core/examples/runtime_acceptance.rs`
- `crates/desktop-core/tests/runtime_acceptance_contract.rs`

未修改 Node、Central、`desktop-core/src/app.rs`、结果窗口、PowerShell、协议定义和正式发布包。验收客户端仍是 ZIP 外的 test-only 工具。

## 实施结果

- 继续支持 `RUST_V2_REAL_MEDIA_ROOT` 单根，并支持 `RUST_V2_REAL_MEDIA_ROOTS_JSON` 按输入顺序传递多个根；`runtime_result.media_roots` 保留完整根列表。
- 单轮模式仍由 `RUST_V2_ACCEPTANCE_SINGLE_RUN=1/true` 启用。第一个运行任务被观察为 `completed`、`failed` 或 `cancelled` 后立即写出 `runtime_result` 并退出，不创建 forced scan；1800 秒仍只是最大窗口。
- 每条 runtime sample 保存现有协议提供的 `outbox_high_seq`；每条扫描终态记录运行任务 ID、完整媒体根、终态和该终态的 outbox 高水位。
- 现有协议没有公开任务文件 lane 的 P/C/F 和“缓存命中但未进入任务文件”计数，因此结果中保存 `task_file_stats`，各计数为 `null`，并以 `source=runtime_protocol_not_exposed` 明确不可用；没有用整体阶段计数冒充任务文件统计。

## TDD 证据

先增加行为测试 `single_run_records_roots_runtime_identity_terminal_and_outbox_highwater`，旧实现实际失败于结果缺少 `media_roots`（`Null` 而非预期根列表）。随后完成最小客户端映射后，该定向测试通过。

## 验证

统一环境：

- `CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target-task7b2d2c1`
- 清除 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`
- `CARGO_INCREMENTAL=0`、`CARGO_PROFILE_DEV_DEBUG=0`、`CARGO_PROFILE_TEST_DEBUG=0`

新鲜命令：

```text
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
```

结果：23 passed，0 failed。

目标文件单独格式与差异检查：

```text
rustfmt --edition 2024 --check crates/desktop-core/examples/runtime_acceptance.rs crates/desktop-core/tests/runtime_acceptance_contract.rs
git diff --check -- crates/desktop-core/examples/runtime_acceptance.rs crates/desktop-core/tests/runtime_acceptance_contract.rs
```

结果：通过。

全仓 `cargo fmt --all -- --check` 仍发现共享工作树既有的 `crates/node-store/tests/result_summary_export.rs` 排版差异；该文件不属于本任务，未修改。

## 未覆盖边界

当前协议没有任务文件 P/C/F、缓存命中或缓存命中未入文件字段，客户端只能如实输出不可用状态。若后续需要这些数字，应先扩展 Node runtime 协议和 Node 遥测，再由客户端消费；本任务不伪造或扩大协议范围。

本任务未执行真实媒体、打包、部署，也未读取或触碰 `I:\Tool`。
