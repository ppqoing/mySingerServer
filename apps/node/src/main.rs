//! node.exe 组合入口：应用目录、配置、日志、NodeRuntime 与 Slint 托盘生命周期。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::{
    cell::RefCell,
    env, fs,
    path::Path,
    rc::Rc,
    sync::{Mutex, mpsc as std_mpsc},
    thread,
};

use dedup_core::{NodeConfig, logging::SizeRotatingWriter};
use dedup_node_engine::actor::{NodeRuntime, SmbiosIdentityProvider};
use dedup_windows::{AppLayout, open_folder};
use node::{TrayAction, TrayCommand, TrayState};
use tokio::sync::mpsc;

slint::include_modules!();

enum RuntimeCommand {
    Restart,
    Shutdown,
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let executable = env::current_exe()?;
    let layout = AppLayout::from_executable(&executable)?;
    fs::create_dir_all(layout.node_root())?;
    initialize_file_log(&layout)?;
    let config = load_node_config(&layout)?;
    let (ready_sender, ready_receiver) = std_mpsc::sync_channel(1);
    let runtime_layout = layout.clone();
    let runtime_thread = thread::spawn(move || run_runtime(runtime_layout, config, ready_sender));
    let (listen_address, commands) = ready_receiver
        .recv()
        .map_err(|_| "节点运行时在报告启动结果前退出")?
        .map_err(|error| format!("节点运行时启动失败: {error}"))?;

    let tray = NodeTray::new()?;
    tray.set_status_text("状态：运行中".into());
    tray.set_address_text(format!("地址：{listen_address}").into());
    let state = Rc::new(RefCell::new(TrayState::new(&layout.node_logs())));
    bind_tray(&tray, &state, &commands);
    slint::run_event_loop()?;

    dispatch_action(state.borrow_mut().apply(TrayCommand::Exit), &commands);
    runtime_thread
        .join()
        .map_err(|_| "节点运行时线程异常退出")??;
    Ok(())
}

fn load_node_config(layout: &AppLayout) -> Result<NodeConfig, Box<dyn std::error::Error>> {
    let path = layout.node_config();
    if path.exists() {
        return Ok(NodeConfig::from_toml(&fs::read_to_string(path)?)?);
    }
    let config = NodeConfig::default();
    fs::write(path, config.to_toml()?)?;
    Ok(config)
}

fn initialize_file_log(layout: &AppLayout) -> Result<(), Box<dyn std::error::Error>> {
    let writer = SizeRotatingWriter::production(layout.node_logs(), "node")?;
    tracing_subscriber::fmt()
        .with_ansi(false)
        .with_target(false)
        .with_writer(Mutex::new(writer))
        .try_init()
        .map_err(|error| std::io::Error::other(error.to_string()))?;
    Ok(())
}

fn run_runtime(
    layout: AppLayout,
    config: NodeConfig,
    ready: std_mpsc::SyncSender<
        Result<(std::net::SocketAddr, mpsc::Sender<RuntimeCommand>), String>,
    >,
) -> Result<(), String> {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|error| error.to_string())?;
    runtime.block_on(async move {
        let node = match NodeRuntime::start(&layout, &config, &SmbiosIdentityProvider).await {
            Ok(node) => node,
            Err(error) => {
                let message = error.to_string();
                let _ = ready.send(Err(message.clone()));
                return Err(message);
            }
        };
        let (commands, mut command_receiver) = mpsc::channel(16);
        ready
            .send(Ok((node.listen_address(), commands)))
            .map_err(|_| "托盘线程未接收节点启动结果".to_owned())?;
        while let Some(command) = command_receiver.recv().await {
            match command {
                RuntimeCommand::Restart => {
                    if let Err(error) = node.handle().restart_engine().await {
                        tracing::error!(error = %error, "重启计算引擎失败");
                    } else {
                        tracing::info!("计算引擎重启完成");
                    }
                }
                RuntimeCommand::Shutdown => break,
            }
        }
        node.shutdown().await.map_err(|error| error.to_string())
    })
}

fn bind_tray(
    tray: &NodeTray,
    state: &Rc<RefCell<TrayState>>,
    commands: &mpsc::Sender<RuntimeCommand>,
) {
    let open_state = Rc::clone(state);
    let open_commands = commands.clone();
    tray.on_open_logs(move || {
        dispatch_action(
            open_state.borrow_mut().apply(TrayCommand::OpenLogs),
            &open_commands,
        );
    });
    let restart_state = Rc::clone(state);
    let restart_commands = commands.clone();
    tray.on_restart_engine(move || {
        dispatch_action(
            restart_state.borrow_mut().apply(TrayCommand::RestartEngine),
            &restart_commands,
        );
    });
    let exit_state = Rc::clone(state);
    let exit_commands = commands.clone();
    tray.on_exit_node(move || {
        dispatch_action(
            exit_state.borrow_mut().apply(TrayCommand::Exit),
            &exit_commands,
        );
        let _ = slint::quit_event_loop();
    });
}

fn dispatch_action(action: Option<TrayAction>, commands: &mpsc::Sender<RuntimeCommand>) {
    match action {
        Some(TrayAction::OpenLogs(path)) => {
            if let Err(error) = open_folder(Path::new(&path)) {
                tracing::error!(error = %error, "打开日志目录失败");
            }
        }
        Some(TrayAction::RestartEngine) => {
            if let Err(error) = commands.try_send(RuntimeCommand::Restart) {
                tracing::error!(error = %error, "发送重启命令失败");
            }
        }
        Some(TrayAction::Shutdown) => {
            if let Err(error) = commands.try_send(RuntimeCommand::Shutdown) {
                tracing::error!(error = %error, "发送退出命令失败");
            }
        }
        None => {}
    }
}
