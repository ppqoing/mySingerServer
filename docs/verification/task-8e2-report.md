# Task8E2：Phase2 瞬态二筛编排边界

## 范围

本步骤只收口 `analysis/phase2.rs` 的二筛批次入口。`begin_stage2_batch` 负责校验活动
位置、内容键和视频槽位，并在当前进程生成临时 `TaskId`；它不再创建 `tasks`、
`task_items` 或 `task_stages`。执行阶段仍保留本地 SQLite 批量查询、可选远端批量导入、
本地缓存重发和 Worker 计算，但不把二筛进度写入旧任务表。

## 已落地行为

- 完整本地二筛缓存只重发已有特征，不启动 Worker，也不产生旧任务行。
- 远端二筛命中先导入本地，再按内容键批量重查；本地已有的视频槽位只重发一次，
  远端或 Worker 只补真正缺失的槽位。
- Worker 请求使用本轮临时运行 ID 和新的 UUID 项身份；结果仍通过现有 `persist_stage2`
  写入 SQLite 和 outbox，失败项继续处理同批后续项。
- `RuntimeTaskReporter` 的查缓存、计算阶段更新和远端降级警告保持不变；阶段状态不再
  保存为旧 `task_stages` 行。TSV、`ScheduledFileReader` 和单写 actor 接线由后续 E3
  负责，本步骤没有扩大执行器边界。

## TDD 与验证

父代理先固定真实 RED：

```text
cargo test -p dedup-node-engine --lib analysis::phase2::tests::complete_stage2_cache_does_not_create_legacy_task_rows --locked -- --test-threads=1
```

旧实现实际创建了 `tasks/task_items/task_stages`，在“旧任务行应为空”的断言处失败。

GREEN 及回归使用唯一 target `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`，并清空
`CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`：

```text
cargo test -p dedup-node-engine --lib analysis::phase2::tests::complete_stage2_cache_does_not_create_legacy_task_rows --locked -- --test-threads=1
cargo test -p dedup-node-engine --lib analysis::phase2::tests --locked -- --test-threads=1
```

结果分别为 `1/1`、`3/3` 通过，覆盖完整缓存、远端导入和选择性重发。指定文件的
`rustfmt --edition 2024` 与 `git diff --check` 通过；全量 `cargo fmt --all -- --check`
仍可能受共享工作树其他并行文件影响，未把该并行格式差异归因于本步骤。

本步骤未暂存、未提交、未打包、未部署，也未触碰 `I:\Tool`。当前 `dispatch_missing`
和 actor 生产接线仍待 Task8E3 切换到瞬态 TSV 执行器。
