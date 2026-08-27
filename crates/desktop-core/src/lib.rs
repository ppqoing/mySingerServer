//! 多节点会话、中心同步、PostgreSQL 访问和跨机器分析编排。
#![warn(missing_docs)]

/// 固定高水位的跨机器一筛、二筛派发和代表分组协调器。
pub mod analysis;
/// Slint 与异步服务之间的单向命令、事件和后台控制循环。
pub mod app;
/// PostgreSQL schema 校验、同步写入与中心分析数据访问。
pub mod central;
/// 删除确认摘要和混合执行结果进度。
pub mod delete;
/// 与手工配置节点的 TCP + Protobuf 会话边界。
pub mod node_session;
/// 本地/中心统一结果分页、成员动作和有限窗口。
pub mod results;
/// 持久复核标记与只更新标记的快捷规则。
pub mod review;
/// Desktop 进程内跨机器分析、同步和删除运行详情。
pub mod runtime_tasks;
/// 增量、ACK、快照与自动/手动触发共用的同步状态机。
pub mod sync;
/// Slint 只读消费的节点、任务、设置和诊断状态快照。
pub mod view_state;
