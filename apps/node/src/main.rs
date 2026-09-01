//! node.exe 组合入口：应用目录、配置、日志、NodeRuntime 与 Slint 托盘生命周期。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod restart_lifecycle;

use std::{
    cell::RefCell,
    env,
    io::{self, Write},
    path::Path,
    rc::Rc,
    sync::{Arc, Mutex, MutexGuard, mpsc as std_mpsc},
    thread,
};

use dedup_core::{
    NodeConfig,
    logging::{
        FallbackLogWriter, ProcessDiagnostics, SizeRotatingWriter, log_filter, log_filter_from_env,
    },
};
use dedup_node_engine::actor::{NodeRuntime, SmbiosIdentityProvider};
use dedup_windows::{AppLayout, open_folder};
use node::{TrayAction, TrayCommand, TrayState};
use restart_lifecycle::load_or_initialize_node_config;
use tokio::sync::mpsc;

slint::include_modules!();

enum RuntimeCommand {
    Restart,
    Shutdown,
}

#[derive(Clone)]
struct CloseableLogWriter {
    inner: Arc<Mutex<Option<FallbackLogWriter<SizeRotatingWriter>>>>,
}

struct CloseableLogGuard<'a> {
    inner: MutexGuard<'a, Option<FallbackLogWriter<SizeRotatingWriter>>>,
}

impl CloseableLogWriter {
    /// 创建可在托盘退出前显式刷新的日志 writer。
    fn new(writer: FallbackLogWriter<SizeRotatingWriter>) -> Self {
        Self {
            inner: Arc::new(Mutex::new(Some(writer))),
        }
    }

    fn close(&self) -> io::Result<()> {
        let mut writer = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .take();
        if let Some(writer) = writer.as_mut() {
            writer.flush()?;
        }
        Ok(())
    }
}

impl Write for CloseableLogGuard<'_> {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        self.inner
            .as_mut()
            .map_or(Ok(buffer.len()), |writer| writer.write(buffer))
    }

    fn flush(&mut self) -> io::Result<()> {
        self.inner.as_mut().map_or(Ok(()), Write::flush)
    }
}

impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for CloseableLogWriter {
    type Writer = CloseableLogGuard<'a>;

    fn make_writer(&'a self) -> Self::Writer {
        CloseableLogGuard {
            inner: self
                .inner
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner()),
        }
    }
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let diagnostics = ProcessDiagnostics::new("node");
    diagnostics.install_panic_hook();
    match run(&diagnostics) {
        Ok(()) => Ok(()),
        Err(error) => {
            diagnostics.record_error("process_failed", "run", error.as_ref());
            Err(error)
        }
    }
}

/// 组合 Node 的配置、日志、运行时线程和托盘生命周期。
fn run(diagnostics: &ProcessDiagnostics) -> Result<(), Box<dyn std::error::Error>> {
    let executable = env::current_exe()?;
    let layout = AppLayout::from_executable(&executable)?;
    let loaded = load_or_initialize_node_config(&layout)?;
    let log_writer = initialize_file_log(&loaded.resolved.log_path, diagnostics)?;
    tracing::info!(
        event = "process_started",
        process = "node",
        pid = std::process::id(),
        version = env!("CARGO_PKG_VERSION"),
        "Node 进程已启动"
    );
    let logs_path = loaded.resolved.log_path.clone();
    let (ready_sender, ready_receiver) = std_mpsc::sync_channel(1);
    let runtime_layout = layout.clone();
    let runtime_thread = thread::spawn(move || {
        run_runtime(runtime_layout, loaded.config, loaded.resolved, ready_sender)
    });
    let (listen_address, commands) = ready_receiver
        .recv()
        .map_err(|_| "节点运行时在报告启动结果前退出")?
        .map_err(|error| format!("节点运行时启动失败: {error}"))?;

    let tray = NodeTray::new()?;
    tray.set_status_text("状态：运行中".into());
    tray.set_address_text(format!("地址：{listen_address}").into());
    let state = Rc::new(RefCell::new(TrayState::new(&logs_path)));
    bind_tray(&tray, &state, &commands);
    slint::run_event_loop()?;

    dispatch_action(state.borrow_mut().apply(TrayCommand::Exit), &commands);
    let runtime_result = runtime_thread
        .join()
        .map_err(|_| "节点运行时线程异常退出")?;
    drop(tray);
    drop(state);
    drop(commands);
    runtime_result?;
    tracing::info!(
        event = "process_stopped",
        process = "node",
        pid = std::process::id(),
        reason = "event_loop_closed",
        "Node 进程正常停止"
    );
    log_writer.close()?;
    Ok(())
}

/// 初始化 Node 主日志，并在主 writer 失败时写入进程应急日志。
fn initialize_file_log(
    logs_path: &Path,
    diagnostics: &ProcessDiagnostics,
) -> Result<CloseableLogWriter, Box<dyn std::error::Error>> {
    let filter = match log_filter_from_env() {
        Ok(filter) => filter,
        Err(error) => {
            diagnostics.record_warning("configuration_rejected", "read_rust_log", &error);
            log_filter(None).expect("固定 INFO 过滤器必须有效")
        }
    };
    let writer = FallbackLogWriter::new(
        SizeRotatingWriter::production(logs_path, "node")?,
        logs_path.join("node.log"),
        diagnostics.clone(),
    );
    let writer = CloseableLogWriter::new(writer);
    tracing_subscriber::fmt()
        .with_ansi(false)
        .with_target(false)
        .with_env_filter(filter)
        .with_writer(writer.clone())
        .try_init()
        .map_err(|error| std::io::Error::other(error.to_string()))?;
    diagnostics.mark_primary_ready();
    Ok(writer)
}

fn run_runtime(
    layout: AppLayout,
    config: NodeConfig,
    paths: dedup_node_engine::config_repository::ResolvedNodePaths,
    ready: std_mpsc::SyncSender<
        Result<(std::net::SocketAddr, mpsc::Sender<RuntimeCommand>), String>,
    >,
) -> Result<(), String> {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|error| error.to_string())?;
    runtime.block_on(async move {
        let node =
            match NodeRuntime::start_with_paths(&layout, &config, &paths, &SmbiosIdentityProvider)
                .await
            {
                Ok(node) => node,
                Err(error) => {
                    let message = error.to_string();
                    if let Err(send_error) = ready.send(Err(message.clone())) {
                        tracing::info!(
                            event = "expected_condition",
                            component = "node_runtime",
                            operation = "report_start_failure",
                            reason = "tray_thread_closed",
                            error = %send_error,
                            "托盘线程已经关闭"
                        );
                    }
                    return Err(message);
                }
            };
        let (commands, mut command_receiver) = mpsc::channel(16);
        ready
            .send(Ok((node.listen_address(), commands)))
            .map_err(|_| "托盘线程未接收节点启动结果".to_owned())?;
        loop {
            tokio::select! {
                command = command_receiver.recv() => {
                    let Some(command) = command else {
                        break;
                    };
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
        if let Err(error) = slint::quit_event_loop() {
            tracing::info!(
                event = "expected_condition",
                component = "node_tray",
                operation = "quit_event_loop",
                reason = "event_loop_closed",
                error = %error,
                "托盘事件循环已经关闭"
            );
        }
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
