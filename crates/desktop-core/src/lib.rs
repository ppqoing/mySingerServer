//! 多节点会话、中心同步、PostgreSQL 访问和跨机器分析编排。
#![warn(missing_docs)]

/// 固定高水位的跨机器一筛、二筛派发和代表分组协调器。
pub mod analysis;
/// Slint 与异步服务之间的单向命令、事件和后台控制循环。
pub mod app;
/// PostgreSQL schema 校验、同步写入与中心分析数据访问。
pub mod central;
/// 与手工配置节点的 TCP + Protobuf 会话边界。
pub mod node_session;
/// 增量、ACK、快照与自动/手动触发共用的同步状态机。
pub mod sync;
/// Slint 只读消费的节点、任务、设置和诊断状态快照。
pub mod view_state;
