# Task8E1：运行任务终态 outbox 高水位记录

## 范围

本步骤只为当前进程 `RuntimeTaskRegistry` 增加终态 outbox 高水位的内存投影，供瞬态
Stage2 完成门禁读取。不增加 `TaskCatalog`，不写 SQLite，不做任务恢复，不改 actor、扫描、
Stage2 业务流程或 Worker。

## TDD 证据

先加入真实行为测试
`terminal_outbox_highwater_is_consistent_and_not_restored`，覆盖以下边界：

- 运行态摘要没有 outbox 高水位；
- `finish_with_outbox_high_seq(Completed, 42)` 后，`list` 和 `details` 都返回 `Some(42)`；
- 终态 `RuntimeTaskChanged` 事件携带同一个 `Some(42)`；
- 新建一个 `RuntimeTaskRegistry` 后原任务不可见，确认没有恢复行为。

RED 命令：

```text
cargo test -p dedup-node-engine --test runtime_tasks terminal_outbox_highwater_is_consistent_and_not_restored --locked -- --test-threads=1
```

结果为预期失败：测试 crate 编译时报告 `RuntimeTaskSummary`、`RuntimeTaskChanged` 缺少
`outbox_high_seq`，以及 `RuntimeTaskReporter` 缺少
`finish_with_outbox_high_seq`；不是测试运行时偶发失败。

GREEN 命令（使用唯一 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`，清理外部编译器环境变量）：

```text
cargo test -p dedup-node-engine --test runtime_tasks terminal_outbox_highwater_is_consistent_and_not_restored --locked -- --test-threads=1
```

结果：`1 passed; 0 failed; 17 filtered out`。

协议 wire 夹具同时覆盖 `RuntimeTaskSummary.outbox_high_seq=Some(42)` 的 round-trip，并对
`RuntimeTaskChanged` 的精确字段集合和 `outbox_high_seq=3` 标签做 descriptor 断言；该协议
定向测试由父代理串行复跑确认。

## 实现边界

- `proto/node.proto` 在未占用 tag 12/3 增加可选 `outbox_high_seq`，协议主版本保持 V5；
- `TaskEntry` 只在进程内保存 `Option<u64>`；registry 新建时为空；
- 普通 `finish` 使用 `None`，新增方法在同一写锁内写入终态和高水位，再发布终态事件；
- 运行中和周期进度事件透传当前字段，默认仍为 `None`；
- 为保持现有 Protobuf 结构体构造和模式匹配可编译，相关协议测试/桌面事件夹具补充默认
  字段或忽略新增字段，不改变业务行为。

本步骤未暂存、未提交；未触碰 `I:\Tool`。
