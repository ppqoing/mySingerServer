//! node.exe 父进程等待参数与替代进程宿主控制。

use std::{ffi::OsString, fs, io, path::PathBuf, sync::Mutex};

use dedup_core::NodeConfig;
use dedup_node_engine::{
    config_repository::{LoadedNodeConfig, NodeConfigRepository},
    host_control::{NodeHostControl, NodeHostControlError},
};
use dedup_windows::{AppLayout, spawn_replacement_node, wait_for_process_exit};
use tokio::sync::mpsc;

#[derive(Default)]
struct HostState {
    replacement_started: bool,
    shutdown_sent: bool,
}

/// 应用入口拥有的替代进程创建与一次性有序退出通知器。
pub struct NodeRestartHost {
    executable: PathBuf,
    parent_pid: u32,
    shutdown: mpsc::Sender<()>,
    state: Mutex<HostState>,
}

impl NodeRestartHost {
    /// 用当前 node.exe、当前 PID 与运行时退出通道创建宿主控制器。
    pub fn new(executable: PathBuf, parent_pid: u32, shutdown: mpsc::Sender<()>) -> Self {
        Self {
            executable,
            parent_pid,
            shutdown,
            state: Mutex::new(HostState::default()),
        }
    }
}

/// 首次创建固定默认 bootstrap/config，随后只通过仓库加载原始配置与解析路径。
pub fn load_or_initialize_node_config(
    layout: &AppLayout,
) -> Result<LoadedNodeConfig, Box<dyn std::error::Error>> {
    if !layout.node_bootstrap().exists() {
        fs::create_dir_all(layout.node_root())?;
        if !layout.node_config().exists() {
            fs::write(layout.node_config(), NodeConfig::default().to_toml()?)?;
        }
        fs::write(
            layout.node_bootstrap(),
            "config_path = \"data/node/config.toml\"\n",
        )?;
    }
    Ok(NodeConfigRepository::from_layout(layout).load()?)
}

impl NodeHostControl for NodeRestartHost {
    fn prepare_replacement(&self, _saved_version: &str) -> Result<(), NodeHostControlError> {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        if state.replacement_started {
            return Ok(());
        }
        spawn_replacement_node(&self.executable, self.parent_pid)
            .map_err(|error| NodeHostControlError::Failed(error.to_string()))?;
        state.replacement_started = true;
        Ok(())
    }

    fn commit_exit_after_response(&self) -> Result<(), NodeHostControlError> {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        if state.shutdown_sent {
            return Ok(());
        }
        if !state.replacement_started {
            return Err(NodeHostControlError::Failed(
                "替代 Node 尚未成功启动".into(),
            ));
        }
        self.shutdown
            .try_send(())
            .map_err(|error| NodeHostControlError::Failed(error.to_string()))?;
        state.shutdown_sent = true;
        Ok(())
    }
}

/// 解析可选 `--wait-for-parent <PID>`，并在继续启动前等待父进程退出。
pub fn wait_for_requested_parent(args: impl IntoIterator<Item = OsString>) -> io::Result<()> {
    let mut args = args.into_iter();
    let Some(flag) = args.next() else {
        return Ok(());
    };
    if flag != "--wait-for-parent" {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "node.exe 只接受 --wait-for-parent <PID>",
        ));
    }
    let pid = args
        .next()
        .and_then(|value| value.into_string().ok())
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "缺少父进程 PID"))?
        .parse::<u32>()
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "父进程 PID 无效"))?;
    if args.next().is_some() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "--wait-for-parent 后存在多余参数",
        ));
    }
    wait_for_process_exit(pid)
}
