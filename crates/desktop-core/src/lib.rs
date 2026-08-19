//! 多节点会话、中心同步、PostgreSQL 访问和跨机器分析编排。
#![warn(missing_docs)]

/// PostgreSQL schema 校验、同步写入与中心分析数据访问。
pub mod central;
/// 与手工配置节点的 TCP + Protobuf 会话边界。
pub mod node_session;
/// 增量、ACK、快照与自动/手动触发共用的同步状态机。
pub mod sync;
