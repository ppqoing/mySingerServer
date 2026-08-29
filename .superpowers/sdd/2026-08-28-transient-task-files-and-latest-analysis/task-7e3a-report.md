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

## Fix round 1：writer 收束后的瞬态目录清理

审查指出成功与 runner 失败路径曾在 `BaseStoreActor::finish()` 前调用
`production.discard()`。现已统一为：runner 返回 owner 后先 drop `BaseStoreHandle` 与 ACK
receiver，再调用 `finish()` 归还或终结 Store actor；成功路径在已归还 Store 上读取实际
`outbox_high_seq`，最后才精确删除当前 run 目录。若 writer join 或 highwater 读取失败，也在
actor owner 已结束后尽力执行同一精确清理，并在原始 runner/writer 错误后附加清理诊断。

新增两条真实行为测试，均使用 task-file runner、受控 WorkerPool、`BaseStoreActor` 与其已有
join 观测，不使用源码字符串断言：

- 成功 ACK 路径确认 `discard` 触发时 writer 已真实 join，当前 run 目录仍存在，随后只删除该目录。
- 运行中取消路径确认同一顺序，且 writer 收束成功时错误仍归还 Store。

### RED / GREEN 证据

所有 Cargo 命令均沿用本报告前述已清理环境变量和固定 target。首次可编译的 RED：

```powershell
cargo test -p dedup-node-engine --features test-hooks production_task_file_stage2_finishes_writer_before_discarding --locked -- --test-threads=1
```

旧顺序结果为 2 failed、0 passed；成功和取消路径均在
`“必须先真实 join SQLite writer，最后才 discard”` 断言失败，实际证明 `discard` 先于
writer 收束。

最小顺序修复后重跑同一命令：2 passed、0 failed。

### 本轮验证

```powershell
cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1
cargo fmt --all -- --check
git diff --check
```

完整 lib 为 137 passed、0 failed；格式和 diff 检查通过。Cargo 仍输出既有未使用导入和测试
hook 警告，未在本轮扩大范围处理。工作树中的 `progress.md` 是既有外部未提交修改，未触碰。
