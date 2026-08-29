# Task13B Desktop 内存复核与删除派发验证报告

日期：2026-08-30
范围：`dedup-desktop-core` 的 Desktop 复核和跨节点删除路径。
约束：本任务只改 `crates/desktop-core/src/app.rs` 与本报告；未修改 `node-engine`、`node-store`、`central-store`，未提交、未打包、未部署，也未触碰 `I:\Tool`。

## 1. 实施结果

- `save_one_review` 和 `apply_quick_review` 只更新当前进程的 `ReviewBoard`、`review_boards` 和成员窗口，并发布当前视图事件；不再调用 PostgreSQL 复核写入。
- 删除了 Desktop 产品路径中的 `persist_central_review` 和 `central_review` 转换函数。
- `prepare_delete` 仍从中心结果窗口一次加载完整组，按当前内存复核决定成员，并继续使用 `DeleteConfirmation` 的活动位置、Keep 和在线门禁。
- `execute_central_delete` 不再调用 `CentralStore::create_delete_plan` 或 `apply_delete_results`。它在当前进程生成一次 UUID v7 批次 ID，将确认成员按 `LocationKey` 稳定排序，生成批次内稳定项 ID，再按 `MachineId` 分组。
- 发往节点的每个 `DeleteItem` 原样携带 `LocationKey` 和确认时的 `ContentKey`。各节点仍通过现有 `NodeSession::execute_central_delete_batch` 执行，Desktop 只汇总节点返回的进度和结果。
- 节点响应在汇总前逐项校验批次 ID、数量、item ID、组 ID、位置、内容身份和允许的终态（`recycled`、`deleted`、`skipped`、`failed`）；缺失、重复、额外、篡改或未知终态均返回错误，并沿现有 `confirm_delete` 错误路径将 Desktop 运行任务标记为 Failed。
- Task12 的滑动窗口、当前组/运行作用域、迟到响应门禁和路由未改动；没有新增恢复、历史、JSON、`.idx` 或分页逻辑。

## 2. TDD 证据

### RED

在纯内存删除计划测试中，临时把项 ID 改回旧中心计划常见的随机 UUID 生成方式，模拟旧实现的非稳定项 ID；未保留该临时改动。测试随后真实失败：

```text
assertion left == right failed: 相同确认集合必须生成稳定的本地删除计划
crates\\desktop-core\\src\\app.rs:2899
```

该 RED 不连接 PostgreSQL，不修改数据库，仅验证相同确认集合在输入顺序变化时能否得到相同的批次内计划。

本次正确性收口又以临时“直接接受节点响应”的实现运行受控响应测试，旧行为真实失败：

```text
tampered_node_delete_response_is_rejected ... FAILED
未知 outcome 不得被当作成功结果
```

覆盖未知 outcome、缺项、多项、重复 item ID，以及替换 group/location/content 的响应。该临时宽松实现未保留。

### GREEN

恢复按稳定位置排序并使用 `{batch_id}:{序号}` 生成项 ID 后，以下测试通过：

| 测试 | 结果 |
|---|---:|
| `desktop_delete_plan_groups_members_without_central_persistence` | 1/1 |
| `review_commands_update_only_the_current_memory_board` | 1/1 |
| `review_delete` | 7/7 |
| `delete_scope` | 1/1 |
| `cross_phase2` | 3/3 |
| `tampered_node_delete_response_is_rejected` | 1/1 |
| `dedup-desktop-core --lib` | 9/9 |

## 3. 执行命令

测试均使用固定共享 target：
`C:\\tmp\\rust-v2-core-scope-target-task7b2d2c`，并清除了外部 C/C++ 编译器和 Rust wrapper 环境变量；Cargo 运行因共享 target ACL 使用了提升权限。

```text
cargo test -p dedup-desktop-core --lib desktop_delete_plan_groups_members_without_central_persistence --locked -- --test-threads=1
cargo test -p dedup-desktop-core --lib review_commands_update_only_the_current_memory_board --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test review_delete --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test delete_scope --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test cross_phase2 --locked -- --test-threads=1
cargo test -p dedup-desktop-core --lib --locked -- --test-threads=1
```

任务文件执行了 `rustfmt --edition 2024 --check crates/desktop-core/src/app.rs` 和
`git diff --check -- crates/desktop-core/src/app.rs`，均通过。完整工作树的 `cargo fmt --all -- --check` 尚需等待其他并行任务的未完成文件收口，不能用本任务结果代替全工作树格式验证。

## 4. 已知边界

本任务没有为 Desktop 删除增加持久化恢复或历史记录。Desktop 进程退出后，临时批次和内存复核自然消失；节点返回的真实删除结果仍由节点侧现有删除事务处理。当前共享工作树仍有其他任务的未提交改动，本报告不代表那些改动已完成或已验证。
