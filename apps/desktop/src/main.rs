//! 管理工具组合入口：应用目录、日志、异步控制循环与 Slint 生命周期。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::{env, fs, sync::Mutex, time::Duration};

use dedup_core::{
    DesktopConfig,
    logging::{
        FallbackLogWriter, ProcessDiagnostics, SizeRotatingWriter, log_filter, log_filter_from_env,
    },
};
use dedup_desktop_core::{
    app::{DesktopApp, UiCommand},
    view_state::DesktopPaths,
};
use dedup_desktop_ui::{MainWindow, apply_event, bind_commands};
use dedup_windows::AppLayout;
use slint::ComponentHandle;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let diagnostics = ProcessDiagnostics::new("desktop");
    diagnostics.install_panic_hook();
    match run(&diagnostics) {
        Ok(()) => Ok(()),
        Err(error) => {
            diagnostics.record_error("process_failed", "run", error.as_ref());
            Err(error)
        }
    }
}

/// 组合 Desktop 的配置、异步控制循环和 Slint 生命周期。
fn run(diagnostics: &ProcessDiagnostics) -> Result<(), Box<dyn std::error::Error>> {
    let layout = AppLayout::from_executable(&env::current_exe()?)?;
    prepare_directories(&layout)?;
    initialize_file_log(&layout, diagnostics)?;
    tracing::info!(
        event = "process_started",
        process = "desktop",
        pid = std::process::id(),
        version = env!("CARGO_PKG_VERSION"),
        "Desktop 进程已启动"
    );
    let config = load_desktop_config(&layout)?;
    let paths = DesktopPaths {
        data: layout.desktop_root().to_path_buf(),
        logs: layout.desktop_logs(),
        cache: layout.desktop_cache(),
        config: layout.desktop_config(),
    };

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;
    let (app, mut events) = {
        let _runtime = runtime.enter();
        DesktopApp::start(config.clone(), paths)
    };
    app.command_sender().try_send(UiCommand::ConnectAll)?;
    let window = MainWindow::new()?;
    let binding = bind_commands(&window, app.command_sender(), config);
    let window_weak = window.as_weak();
    let event_task = runtime.spawn(async move {
        while let Some(event) = events.recv().await {
            let binding = binding.clone();
            if let Err(error) = window_weak
                .upgrade_in_event_loop(move |window| apply_event(&window, &binding, event))
            {
                tracing::error!(
                    event = "request_failed",
                    component = "desktop_ui",
                    request_id = 0_u64,
                    operation = "deliver_ui_event",
                    error = %error,
                    "向 UI 事件循环投递事件失败"
                );
                break;
            }
        }
    });

    window.run()?;
    if let Err(error) = runtime.block_on(app.send(UiCommand::Shutdown)) {
        tracing::error!(
            event = "request_failed",
            component = "desktop_controller",
            request_id = 0_u64,
            operation = "shutdown",
            error = %error,
            "发送 Desktop 关闭命令失败"
        );
    }
    runtime.block_on(settle_event_task(event_task, Duration::from_secs(2)));
    tracing::info!(
        event = "process_stopped",
        process = "desktop",
        pid = std::process::id(),
        reason = "window_closed",
        "Desktop 进程正常停止"
    );
    Ok(())
}

/// 在固定时限内收束 UI 事件 task，并记录 panic、JoinError 或预期取消。
async fn settle_event_task(mut task: tokio::task::JoinHandle<()>, timeout: Duration) {
    match tokio::time::timeout(timeout, &mut task).await {
        Ok(Ok(())) => {}
        Ok(Err(error)) => log_background_task_failure("join", &error),
        Err(error) => {
            tracing::info!(
                event = "expected_condition",
                component = "desktop",
                operation = "wait_event_task",
                reason = "shutdown_timeout",
                error = %error,
                "UI 事件 task 在关闭时限内未结束"
            );
            task.abort();
            if let Err(join_error) = task.await
                && !join_error.is_cancelled()
            {
                log_background_task_failure("join_after_abort", &join_error);
            }
        }
    }
}

/// 统一记录 Desktop 后台 task 的不可恢复结束原因。
fn log_background_task_failure(operation: &'static str, error: &tokio::task::JoinError) {
    tracing::error!(
        event = "background_task_failed",
        component = "desktop",
        task_name = "desktop_event_bridge",
        operation,
        error = %error,
        "Desktop 后台 task 异常结束"
    );
}

/// 首次启动只创建管理端自身的数据、日志和缓存目录。
fn prepare_directories(layout: &AppLayout) -> std::io::Result<()> {
    fs::create_dir_all(layout.desktop_root())?;
    fs::create_dir_all(layout.desktop_logs())?;
    fs::create_dir_all(layout.desktop_cache())
}

/// 读取现有 TOML；不存在时写出含本机默认节点的初始配置。
fn load_desktop_config(layout: &AppLayout) -> Result<DesktopConfig, Box<dyn std::error::Error>> {
    let path = layout.desktop_config();
    if path.exists() {
        return Ok(DesktopConfig::from_toml(&fs::read_to_string(path)?)?);
    }
    let config = DesktopConfig::default();
    fs::write(path, config.to_toml()?)?;
    Ok(config)
}

/// 复用全局固定的 20 MiB × 10 同步滚动 writer。
fn initialize_file_log(
    layout: &AppLayout,
    diagnostics: &ProcessDiagnostics,
) -> Result<(), Box<dyn std::error::Error>> {
    let directory = layout.desktop_logs();
    let filter = match log_filter_from_env() {
        Ok(filter) => filter,
        Err(error) => {
            diagnostics.record_warning("configuration_rejected", "read_rust_log", &error);
            log_filter(None).expect("固定 INFO 过滤器必须有效")
        }
    };
    let writer = FallbackLogWriter::new(
        SizeRotatingWriter::production(&directory, "desktop")?,
        directory.join("desktop.log"),
        diagnostics.clone(),
    );
    tracing_subscriber::fmt()
        .with_ansi(false)
        .with_target(false)
        .with_env_filter(filter)
        .with_writer(Mutex::new(writer))
        .try_init()
        .map_err(|error| std::io::Error::other(error.to_string()))?;
    diagnostics.mark_primary_ready();
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::{
        io::{self, Write},
        sync::{Arc, Mutex},
        time::Duration,
    };

    use tracing_subscriber::fmt::MakeWriter;

    use super::settle_event_task;

    /// 收集事件任务收束边界写出的真实格式化日志。
    #[derive(Clone, Default)]
    struct SharedLogBuffer(Arc<Mutex<Vec<u8>>>);

    impl SharedLogBuffer {
        /// 返回当前 UTF-8 日志文本。
        fn text(&self) -> String {
            String::from_utf8(self.0.lock().unwrap().clone()).unwrap()
        }
    }

    /// 为一次 tracing 写入持有共享缓冲区。
    struct SharedLogWriter(SharedLogBuffer);

    impl Write for SharedLogWriter {
        fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
            self.0.0.lock().unwrap().extend_from_slice(buffer);
            Ok(buffer.len())
        }

        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    impl<'a> MakeWriter<'a> for SharedLogBuffer {
        type Writer = SharedLogWriter;

        fn make_writer(&'a self) -> Self::Writer {
            SharedLogWriter(self.clone())
        }
    }

    /// 防止事件桥接 task panic 只被 timeout/JoinHandle 吞掉。
    #[tokio::test(flavor = "current_thread")]
    async fn event_task_panic_writes_background_failure() {
        let output = SharedLogBuffer::default();
        let subscriber = tracing_subscriber::fmt()
            .without_time()
            .with_ansi(false)
            .with_target(false)
            .with_writer(output.clone())
            .finish();
        let _guard = tracing::subscriber::set_default(subscriber);
        let task = tokio::spawn(async { panic!("event-task-sentinel") });

        settle_event_task(task, Duration::from_secs(1)).await;

        let log = output.text();
        assert_eq!(log.matches("event=\"background_task_failed\"").count(), 1);
        assert!(log.contains("task_name=\"desktop_event_bridge\""));
        assert!(log.contains("event-task-sentinel"));
    }
}
