# Task 6B 调度器复审修复报告

## 范围

本次 follow-up 只收敛 `DiskReadScheduler` 的加权 lane、公平状态提交和错误边界，未接入
`task_dispatch`，未修改 actor、BaseCompute、扫描流水线、SQLite、协议或 UI。未跟踪的
`crates/node-engine/tests/task_dispatch.rs` 保持原样。

## 修复内容

- 加权外层先选择可运行的物理盘 lane，再在选中 lane 内执行复合盘原子检查、T=1
  轮转、Hash/Media 规则和 FIFO；不同权重的共享 T=1 lane 不再先被压成一个代表。
- 加权入口首次出现后，旧 `acquire` 请求以权重 1 进入同一个 actor 轮转；纯旧入口仍走
  原等权路径。
- `WeightedChoice` 只在许可响应成功交付后提交 deficit/cursor。发送失败、取消和测试注入的
  关闭响应都会释放已预留许可但不消耗公平状态。
- 加权等待项离开后，若同一物理盘仍有 legacy 等待项，则清零旧 deficit 并保留权重 1
  占位；加权项重新出现不会继承历史突发额度，也不会反复重置全局 cursor 造成 legacy 饥饿。
- `acquire_lane` 的超大冻结逐盘额度在 actor 内返回 `InvalidConfiguration`，不再在名义
  seat 计算处 panic；同一物理盘的冻结权重冲突直接返回错误，不进入队列，也不静默取最小值。
- 老化直通作为一次真实成功授予提交一次公平状态，并将该 lane deficit 清零、游标转到
  下一候选，避免老化保护之后产生额外突发；既有 8 次冲突绕过上限保持不变。

## TDD 证据

在 `73fb66c4` 的旧实现上先固定了以下真实失败：共享 T=1 代表压缩会让低权重 lane
先被选中；加权 lane 和 legacy 同时竞争时连续六次全部交付给高权重 lane。实现过程中又以
确定性测试钩子覆盖发送失败不提交 deficit/cursor，覆盖同 key legacy 保留时的 deficit
清理，以及错误额度和冻结权重冲突的返回边界。

新增/增强行为门禁覆盖：

- 5:1、7:2 配置权重及非硬编码比例；
- 先按 lane 权重、再做共享 T=1/复合盘选择，并校验复合许可覆盖全部底层盘；
- weighted 与 legacy 外层轮转；
- 响应发送失败、等待取消和 active 计数清理；
- weighted lane 离队后重新出现不继承 deficit；
- 老化最多 8 次绕过，保护性授予后下一次不产生 lane 突发；
- 超大逐盘额度、同 key 权重冲突均快速返回且不污染队列。

## 验证

统一使用 `C:\tmp\rust-v2-core-scope-target`，清除继承的 MinGW/C 编译环境变量并关闭增量
和 debug 信息：

- `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1`
  —— 39/39 通过；
- `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` —— 66/66 通过；
- `rustfmt --edition 2024 --check crates/node-engine/src/io/scheduler.rs crates/node-engine/tests/disk_scheduler.rs`
  —— 通过；
- `git diff --check -- crates/node-engine/src/io/scheduler.rs crates/node-engine/tests/disk_scheduler.rs`
  —— 通过。

全仓 `cargo fmt --all -- --check` 仍会报告另一 agent 未跟踪的
`crates/node-engine/tests/task_dispatch.rs` 格式差异；本次没有修改该文件。C/D 盘在重型
命令前约为 17.58/10.24 GiB，未触发清理。未运行真实媒体、打包、部署，也未触碰
`I:\Tool`。

## 第二轮复审修复

第二轮先以确定性行为测试固定三条 RED：活动 weight=5 permit 存在时 weight=7 未被拒绝；
weighted waiter 全部离开后 legacy 仍沿用加权路径；legacy 队首后取消的非队首 weighted
项仍参与冻结权重冲突。修复后：

- 每个 weighted lane 由 actor 持有活动 permit 原子计数；队列清空时只清理 deficit，
  最后一个 permit 释放后才移除冻结配置；
- `weighted_mode` 每轮从当前开放且未取消的 weighted waiter 计算，全部离开即恢复旧的
  capacity-one/rotation 选择；
- 两类 FIFO 清理改为 retain 存活项，任意位置取消项都会释放队列槽位，配置扫描忽略已关闭
  的响应且保留其余存活项顺序。

第二轮修复后的 `disk_scheduler` 全量为 42/42，`dedup-node-engine --lib` 为 66/66；本段
两文件 rustfmt 与 diff-check 通过。活动配置、legacy 恢复和非队首取消三条新增测试均为
1/1。未修改 `task_dispatch.rs`，未运行真实媒体、打包或部署。
