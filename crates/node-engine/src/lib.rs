//! 扫描、持久任务、Worker 池、本地分析、预览和安全删除编排。
#![warn(missing_docs)]

mod central_cache;
mod contact_sheet_cache;

pub use central_cache::{
    DisabledRemoteFeatureCache, NodeRemoteFeatureCache, PostgresFeatureCache, RemoteCacheError,
    RemoteFeatureCache, Stage2CacheLookup,
};

/// 显式可再生产物登记与活动租约。
pub mod artifact_registry;
/// Windows 磁盘满触发的同盘全量派生产物清理。
pub mod disk_full_cleanup;

/// 串行独占 NodeStore 与 WorkerPool 的节点业务 actor。
pub mod actor;
/// 纯 SQLite 本地精确、相似两层分析和代表分组。
pub mod analysis;
/// 固定 bootstrap 与完整 Node 配置的本机原子持久化边界。
pub mod config_repository;
/// 文件删除前复核和成功后的 SQLite 收缩事务。
pub mod delete;
/// Node 替代进程与响应刷出后的退出通知边界。
pub mod host_control;
/// 可取消分块读取、超时重试和流式 MD5 边界。
pub mod io;
/// 图片原文件和视频 JPEG 联系表的有界分块预览。
pub mod preview;
/// Node 进程内运行任务阶段、Worker 和最近失败 registry。
pub mod runtime_tasks;
/// 文件枚举、缓存复用、MD5 与一筛任务编排。
pub mod scan;
/// 单管理连接 TCP 服务和连接内请求复用。
pub mod server;
/// 隔离媒体计算的 Worker 流水线与进程池。
pub mod worker;
