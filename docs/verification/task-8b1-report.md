# Task8B1 NodeStore 瞬态二筛事务验证报告

## 范围

本任务只为按物理盘任务文件驱动的二筛结果提供 NodeStore 窄事务入口，不保存计算任务、任务项或任务阶段。
二筛结果提交成功后才由上层把任务文件行标记为完成；本任务不修改 Worker、协议、Node actor、任务文件调度
或真实媒体流程。

## 实现边界

修改文件：

- `crates/node-store/src/features.rs`
- `crates/node-store/tests/taskless_stage2.rs`

新增 `NodeStore::commit_stage2_taskless`，其行为固定为：

1. 只接受 `FeatureWrite::ImageStage2` 和 `FeatureWrite::VideoFrameStage2`。
2. 校验内容存在、媒体类型匹配、基础计算已经完成以及视频槽位位于 `0..=5`。
3. 校验 Sobel 向量为有限值；缺失、非有限或其他阶段结果均拒绝。
4. 在一个 SQLite 事务中写入二筛特征和对应同步 outbox，并返回本次事务真实的最后高水位。
5. 事务不触碰 `tasks`、`task_items`、`task_stages`，任一后续结果非法时整体回滚，既有特征和 outbox 不变。
6. pHash 和 Sobel 的合法全零值不视为占位值，仍允许作为有效计算结果提交。

## TDD 行为证据

先加入调用新接口的测试并运行 RED，旧实现因缺少 `commit_stage2_taskless` 而无法编译；随后完成最小实现并
验证 GREEN。新增 6 项行为测试：

- `taskless_image_stage2_commits_without_task_rows`：图片二筛、outbox 和真实高水位同事务提交。
- `taskless_stage2_accepts_legal_zero_features`：合法全零 pHash/Sobel 可以提交并读取。
- `taskless_video_stage2_commits_selected_slots_without_task_rows`：视频只写指定二筛槽位，不伪造其他槽位。
- `taskless_stage2_rejects_stage1_and_rolls_back_atomically`：混入一筛结果时拒绝，并回滚此前二筛写入。
- `taskless_stage2_rejects_non_finite_features_without_overwrite`：非有限 Sobel 被拒绝，不覆盖既有有效结果。
- `taskless_stage2_rejects_placeholder_content`：基础计算未完成的占位内容不能写入二筛。

## 验证结果

所有 Cargo 命令复用 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`，没有创建新 target：

- `cargo test -p dedup-node-store --test taskless_stage2 --locked -- --test-threads=1`：6/6 PASS。
- `cargo test -p dedup-node-store --locked -- --test-threads=1`：全量 74/74 PASS。
- `cargo fmt --all -- --check`：PASS。
- NodeStore 源文件和新增测试的 rustfmt 检查：PASS。
- NodeStore 修改的 diff 检查：PASS。

本任务未执行真实媒体、打包、部署或 `I:\Tool` 操作。
