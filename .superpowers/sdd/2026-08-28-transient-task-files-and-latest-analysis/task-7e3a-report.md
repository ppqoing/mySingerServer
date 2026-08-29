# Task 7E3A：Phase2 生产任务文件编排报告

## 结果

已在 `analysis::phase2` 新增 `run_stage2_batch_production`。入口接收后台独占的
`NodeStore`、已冻结的 `Stage2BatchPlan`、唯一 `WorkerPool` 和运行选项，成功时归还
`NodeStore`、内容级完成/失败数与 SQLite 实际 `outbox_high_seq`；任务级错误也尽力携带
已恢复的 Store。

实现顺序固定如下：

1. 在任何本地或远端二筛缓存查询前，以全部有效媒体文件路径建立 `ScanDiskPlan`，并一次性
   `assign_all`，再交给 `Stage2TransientPlanner::freeze`。
2. 本地缓存以 ContentKey 一次批量查询；仅本机基础字段完整且存在二筛缺口时批量查询远端。
3. 本机缓存重发和远端导入直接写 SQLite/outbox；`IncompleteBase` 不生成 TSV、不启动 Worker，
   按失败/未完成内容统计。
4. 只将 `Compute` 转换为 `Stage2TaskInput`。视频无论联系表是否存在都固定使用内容 MD5
   推导目标路径，从而保留原视频回退与重建语义。
5. 有 Compute 时只创建一个 `ScheduledFileReader`，并将同一实例作为 task-file runner 的
   lane permit provider；通过唯一 `BaseStoreActor` 和 ACK 推进 TSV 的 `C/F`。
6. 所有 Worker、permit、ACK 和 writer 收束后取回 Store、读取真实高水位，并精确删除当前
   `runtime/<run-id>`。构建任务文件在交还 owner 前失败时，也只尝试删除该已知 run-id 目录。

未修改 `actor.rs`、Desktop、协议和打包脚本。为下一项 Actor 接线，新增 Phase2 入口的
`analysis` 内部 re-export，以及 `BaseStoreActor` 的窄 `scan` 内部 re-export。

## TDD 证据

先添加生产入口行为测试并运行定向 RED。旧代码编译失败，报出
`Stage2TaskFileRunOptions` 和 `run_stage2_batch_production` 未定义，证明尚未存在将 Phase2
接入 TSV/统一 scheduler/单写 ACK 的生产入口。

新增视频槽位行为测试后，临时突变为跳过视频 `Compute` 的 TSV 输入；测试在 2 秒超时内失败。
恢复正式实现后同一测试通过。

行为覆盖：

- 完整本地二筛缓存：零 runtime TSV、零 Worker、零旧任务表行，返回真实高水位。
- 两条冻结 lane：一条 Worker 崩溃为 `F` 后，另一条继续经 SQLite ACK 写入；当前 run 目录被删除。
- 缺失视频唯一槽位：仅该槽由 task-file runner 处理，和既有五槽组成完整视频二筛。
- 物理盘解析顺序：全部来源解析完成后才允许第一次本地批量缓存查询。

## 验证命令与结果

验证环境按简报清除了 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，并使用：

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-core-scope-target-task7b2d2c1'
$env:CARGO_INCREMENTAL='0'
$env:CARGO_PROFILE_DEV_DEBUG='0'
$env:CARGO_PROFILE_TEST_DEBUG='0'
```

通过：

```powershell
cargo test -p dedup-node-engine --features test-hooks analysis::phase2 --locked -- --test-threads=1
cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1
cargo fmt --all -- --check
git diff --check
```

完整 lib 测试结果为 135 passed、0 failed。Cargo 仍报告当前分支已有的未使用导入/测试钩子
警告；新增 `Stage2TaskFileRunError::into_store` 也会在 Actor 尚未接线前提示未使用，供 7E3B
在错误路径取回 Store。本任务没有扩大范围处理这些警告。

## 边界与后续

该入口已可被 Actor 使用，但按照任务限制没有把现有 `DispatchStage2`/本地分析调用接到它；
这一接线由后续 7E3B 负责。
