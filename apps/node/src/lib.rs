//! node.exe 托盘命令的无 GUI 状态模型。
#![warn(missing_docs)]

use std::path::{Path, PathBuf};

/// Slint 托盘回调发送的三个用户命令。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TrayCommand {
    /// 在资源管理器中打开 `data/node/logs`。
    OpenLogs,
    /// 只重建媒体 Worker，不退出 TCP/SQLite 节点。
    RestartEngine,
    /// 有序停止 listener、actor 和 Worker Job 后退出。
    Exit,
}

/// 托盘状态模型交给进程组合层的明确副作用。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TrayAction {
    /// 打开已经由 AppLayout 确定的绝对日志目录。
    OpenLogs(PathBuf),
    /// 调用 NodeEngine 三阶段计划重启。
    RestartEngine,
    /// 调用 NodeRuntime 有序关闭。
    Shutdown,
}

/// 使 GUI 回调保持无状态，并保证退出只发送一次的最小状态机。
pub struct TrayState {
    logs: PathBuf,
    shutdown_sent: bool,
}

impl TrayState {
    /// 用 AppLayout 返回的绝对日志目录创建状态。
    pub fn new(logs: &Path) -> Self {
        Self {
            logs: logs.to_path_buf(),
            shutdown_sent: false,
        }
    }

    /// 把一个菜单命令映射为至多一个进程动作。
    pub fn apply(&mut self, command: TrayCommand) -> Option<TrayAction> {
        match command {
            TrayCommand::OpenLogs => Some(TrayAction::OpenLogs(self.logs.clone())),
            TrayCommand::RestartEngine => Some(TrayAction::RestartEngine),
            TrayCommand::Exit if !self.shutdown_sent => {
                self.shutdown_sent = true;
                Some(TrayAction::Shutdown)
            }
            TrayCommand::Exit => None,
        }
    }
}
