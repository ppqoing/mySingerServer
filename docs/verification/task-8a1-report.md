# Task8A1 Stage2 源读取完成事件验证报告

## 范围

本任务只建立二筛 Worker 源读取完成事件的协议与 WorkerPool 接收边界，不改变二筛调度、
分析或 actor 业务流程；Worker 真实发送点由后续任务接入。WorkerPool 收到
`Stage2SourceReadComplete` 时继续保留同一个 slot、CPU 权重和运行身份，只有收到
`Stage2Result` 才释放资源。

## 协议变更

- 新增 `Stage2SourceReadComplete`，字段与二筛运行项使用 `task_id`、`item_id` 和可选读取耗时。
- 新增 `WorkerEnvelope.stage2_source_read_complete = 29`；29 是当前未占用标签。
- 协议版本继续固定为 V5，不复用基础计算的 `BaseSourceReadComplete`，不修改旧字段号。

## WorkerPool 行为

- `run_slot` 将二筛源读取完成响应视为非终态并继续接收后续 Worker 响应。
- 合法事件产生独立的 `WorkerEvent::Stage2SourceReadComplete`。
- 新事件的 task/item 身份必须匹配当前 slot；错配事件被丢弃，不释放 slot/CPU，也不伪造
  `Completed`。
- `ControlledWorkerPool::stage2_source_read_complete` 提供仅测试用的受控事件注入；受控池在
  该事件后保持 busy/CPU 占用，终态清理语义不变。
- 基础计算事件匹配增加忽略分支，仅保证新增枚举成员不会改变基础计算状态机；没有接入二筛
  调度或分析逻辑。

## TDD 证据

先加入以下 RED 测试，再实现协议和 Pool：

1. `stage2_source_read_complete_round_trips_on_additive_tag_29`：新消息/oneof 尚不存在时，
   protocol 编译失败，缺少 `Stage2SourceReadComplete` 类型与 payload variant。
2. `stage2_source_read_complete_is_non_terminal_and_keeps_slot_owned`：Pool 缺少新事件类型时
   编译失败；实现后验证非终态事件、后续 Stage2Result 以及 CPU/slot 释放顺序。
3. `mismatched_stage2_source_read_complete_does_not_release_slot_or_emit_terminal`：验证身份错配
   不会释放当前运行项或伪造终态。
4. `controlled_stage2_source_read_complete_keeps_slot_until_stage2_result`：验证受控池身份错配、
   合法事件、busy/CPU 快照和终态事件顺序。

## 验证命令

所有 Cargo 命令复用 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`，未创建新 target。

- `cargo test -p dedup-protocol --test worker_base_compute_wire stage2_source_read_complete_round_trips_on_additive_tag_29 --locked`：1/1 PASS。
- `cargo test -p dedup-node-engine --lib stage2_source_read_complete --locked`：3/3 PASS。
- RED 阶段的预期编译失败已记录于本任务执行日志，失败点是新协议类型、payload variant 和 WorkerEvent 尚不存在。
- 未执行真实媒体、打包、部署或 `I:\Tool` 操作。

## 文件边界

本任务涉及 `proto/node.proto`、`crates/protocol/tests/worker_base_compute_wire.rs`、
`crates/node-engine/src/worker/pool.rs`、基础媒体事件的最小枚举兼容分支及本报告。没有修改
Stage2 调度、分析、Node actor、数据库或文件读取实现。
