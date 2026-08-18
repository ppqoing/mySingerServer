//! 扫描、持久任务、Worker 池、本地分析、预览和安全删除编排。
#![warn(missing_docs)]

/// 文件枚举、缓存复用、MD5 与一筛任务编排。
pub mod scan;
/// 隔离媒体计算的 Worker 流水线与进程池。
pub mod worker;
