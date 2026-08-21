//! Node 替代进程与响应刷出后的退出通知边界。

use thiserror::Error;

/// 宿主进程创建替代 Node 并在响应写出后提交退出的可测试边界。
pub trait NodeHostControl: Send + Sync {
    /// 在当前 Node 仍运行时创建已保存配置对应的替代进程。
    fn prepare_replacement(&self, saved_version: &str) -> Result<(), NodeHostControlError>;

    /// 仅在客户端已收到成功响应后通知宿主有序退出。
    ///
    /// 本操作必须幂等：调用方会在前一次返回错误时重试，即使前一次已经部分提交退出；
    /// 一次成功后的重复调用也必须安全，不能产生第二次退出副作用。
    fn commit_exit_after_response(&self) -> Result<(), NodeHostControlError>;
}

/// 宿主生命周期操作失败。
#[derive(Debug, Error)]
pub enum NodeHostControlError {
    /// 替代进程无法创建或有序退出无法提交。
    #[error("Node 宿主控制失败: {0}")]
    Failed(String),
}
