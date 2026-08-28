# Task 6B：瞬态任务文件唯一 Dispatcher

## 范围

本次只实现任务文件队首 Dispatcher 与现有 `DiskReadScheduler` 的薄适配，不接入
BaseCompute、actor、扫描流水线、SQLite、协议或 UI。前置的加权 scheduler 实现继续由
同一个 actor 维护 deficit、cursor、active、复合盘原子许可和老化保护，Dispatcher 不复制
这些状态。

## 实现

- 新增 `crates/node-engine/src/task_dispatch.rs` 并从 `dedup-node-engine::task_dispatch`
  导出；`TaskFileDispatcher` 私有拥有 `TransientTaskFileSet`，对外只转发 lane 注册、批量
  追加、seal、状态 ACK、路径、健康检查和 discard。
- 每个任务文件 lane 只保存一个拥有型队首快照和一个 permit future。许可成功后才以完整
  `TaskFileIdentity` 调用 `take_lane`；许可失败或取消只丢弃 future，磁盘首字节和任务行仍为
  `P`，下一次调用可重新申请。
- `DispatchedTask` 同时持有身份、记录、读取类别和唯一 permit；后续读取器不需要再次申请
  磁盘许可。Hash/Media 与 ImageStage2/VideoStage2 的类别映射在同一处校验，非法掩码组合
  fail-closed。
- 使用任务文件 publication epoch/Notify 处理未 seal 的空 lane，避免忙等和漏唤醒；只有
  seal、全部任务进入 C/F 且没有在途项时才返回 `None`。有限预读仍由 Task6A 按
  `max(2, per_disk_limit * 2)` 控制。
- `SchedulerTaskLanePermitProvider` 只把冻结的物理盘集合、介质类型、`per_disk_limit` 和
  `configured_weight` 转换为 `DiskReadLane`，然后调用同一个 `DiskReadScheduler::acquire_lane`。
  不新增第二套权重、活动计数或老化逻辑。
- 测试使用拥有型临时目录 harness，删除了原预备测试的 `mem::forget` 泄漏。

## TDD 与验证

预备测试在没有 Dispatcher 模块时先得到真实编译 RED：`unresolved import
dedup_node_engine::task_dispatch`。首轮实现后补充行为测试，覆盖每 lane 单 outstanding、
许可前不 take、provider 失败与取消保留 P 并可重试、未 seal 空 lane 等待及追加唤醒、seal
终止、Base/Stage2 类别映射、复合 lane 单次 acquire、permit Drop、真实 scheduler 薄适配。

统一使用 `CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`，关闭增量/debug 信息并清除
继承的 MinGW 编译环境变量；未访问 `I:\Tool`。

| 命令 | 结果 |
|---|---:|
| `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1` | 10/10 通过 |
| `cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1` | 22/22 通过 |
| `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1` | 42/42 通过 |

当前只读文件 dispatcher 与前置 Task6A API 发生衔接：`TaskWorkMask::validate_for` 调整为
`pub(crate)`，无生产语义扩展；未修改 scheduler、actor、BaseCompute、pipeline、SQLite、
协议、UI。完整 node-engine lib、scan_roots、BaseCompute 和格式检查由控制 Agent 在本提交
前继续执行。
