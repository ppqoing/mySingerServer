# Task10A 实施报告：本地分析运行模型迁移

## 结果

本任务把本地分析算法使用的输入、候选、分组和成员迁移到
`dedup-node-engine::analysis::model`。`ScanAnalysisInput` 使用
`dedup_core::DisplayPath` 保留原始路径拼写；精确分组、图片/视频一筛、二筛候选判定和
代表直连分组均消费运行模型。旧 `NodeStore` 写模型和 API 保留不动，
`LocalAnalysisEngine` 只在 `replace_candidates`、`replace_groups` 边界做逐字段转换。
旧持久化输入没有显示路径字段，因此兼容转换以规范绝对路径作为显示路径兜底；最终写入
旧组表时按约定丢弃运行态 `display_path`。

未修改 actor、协议、Desktop、Task9 writer、任务文件、取消/结果发布、零 SQLite 写入、
TaskCatalog 或恢复逻辑。

## TDD 记录

1. **RED**：先在 `representative_grouping.rs` 增加真实运行模型行为测试，构造两个内容和
   两个大小写不同的 `DisplayPath`，断言代表、成员路径及直接证据。旧代码先以
   `cargo test -p dedup-node-engine --test representative_grouping --locked -- --test-threads=1`
   运行，编译失败锚点为 `analysis` 没有 `group_analysis_results`，且找不到
   `analysis::model`。
2. **GREEN**：新增 `analysis/model.rs`，迁移 exact/image/video/grouping 与 phase2
   候选/最终判定，补上 Store 边界逐字段转换。行为测试通过，未使用源码文本匹配。

## 验证

按 brief 使用 `CARGO_TARGET_DIR=C:\\tmp\\rust-v2-core-scope-target-task7b2d2c1`，清除
`CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，并设置
`CARGO_INCREMENTAL=0`、`CARGO_PROFILE_DEV_DEBUG=0`。执行前 C: 约 24.66 GiB、D: 约
11.95 GiB 可用，均高于 10 GiB 门槛。

- `representative_grouping`：3 passed，0 failed。
- `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`：
  142 passed，0 failed。
- `local_analysis`：11 passed，0 failed。`explicit_stage2_batch_republishes_requested_cached_slot_without_worker`
  保留真实 outbox 重发和 `processor.calls == 0` 断言，并以 `page_tasks(None, 20)` 为空
  证明 transient Stage2 不创建 `tasks/task_items/task_stages` 行。
- `cargo fmt --all -- --check`：通过。
- `git diff --check`：通过。
