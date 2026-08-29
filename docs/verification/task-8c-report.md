# Task8C 瞬态二筛计划器实施记录

## 范围

本次只增加本地/跨机二筛的瞬态分类计划器和行为测试，不接入 Node actor、Desktop、Worker 执行器或任务恢复。计划器不写 SQLite、不写任务文件，也不启动 Worker；调用方在得到 `Compute` 项后，才可以把其中的 `TaskFileRecord` 写入按物理盘划分的 TSV。

## 已落地边界

- `Stage2PlanningInput` 在缓存查询前携带内容键、活动位置、扫描快照和已解析的 `TaskDiskLane`。`Stage2TransientPlanner::freeze` 先去重内容，校验活动位置、规范路径、文件大小、媒体类型与视频槽位，并复制冻结 lane。
- `Stage2TransientPlanner::plan` 只接收与冻结输入同序的本地/远端批量结果切片，没有 `NodeStore` 或远端连接参数，因此分类阶段不能退化为逐项 SQLite 查询；长度不一致直接拒绝。
- 本地完整图片或视频槽位只生成 `RepublishLocal`，不生成 Worker 工作项；远端完整结果只覆盖当前缺失字段，已经由本地命中的字段不会被远端替换。
- 图片只在本地二筛缺失且远端未提供完整联合特征时生成一个图片 `Compute` 项。视频只从一筛成功、当前候选槽位中形成缺失掩码；本地已有槽位、远端已提供槽位和一筛失败槽位均不会进入 Worker 掩码。
- 基础探测或一筛不完整时只返回 `IncompleteBase`，不生成 TSV 行，不写失败占位。完整命中和基础不完整都不会出现在 `worker_items()`。

## TDD 证据状态

测试文件已先于计划器实现建立，覆盖：

1. 重复内容和非活动来源在缓存批次产生前被拒绝；
2. 本地/远端结果按冻结顺序对齐，图片和视频只安排真正缺失字段；
3. 完整本地命中忽略远端结果，Worker 工作项保留冻结的物理盘 lane。

测试先于实现写入，但受串行 Cargo 约束，没有取得旧实现的独立 RED 运行证据，不能把静态缺失当作已执行 RED。主代理随后使用唯一 target `C:\tmp\rust-v2-core-scope-target-task7b2d2c1` 运行 `cargo test -p dedup-node-engine --test stage2_transient_planner --locked -- --test-threads=1`，3/3 PASS；`rustfmt --edition 2024 --check` 与 `git diff --check` 通过。

后续补充了完整视频缓存命中回归：旧实现会在没有缺失槽位时触发 `缺失选择存在时才会进入二筛计划` panic；修复后只对候选的已有槽位生成 `RepublishLocal`，Worker 工作项为零。主代理重新运行同一计划器测试，4/4 PASS。

## 文件边界

- `crates/node-engine/src/analysis/stage2_planner.rs`：独立瞬态分类计划器。
- `crates/node-engine/tests/stage2_transient_planner.rs`：计划器行为测试夹具。
- `crates/node-engine/src/analysis/mod.rs`：公开计划器模块声明。

未修改 `worker` pipeline/protocol loop、WorkerPool、`node-store` features、Task8B2 执行器、actor 或 desktop；未运行真实媒体、未打包部署、未触碰 `I:\Tool`。
