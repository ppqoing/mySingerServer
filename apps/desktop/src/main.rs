//! 管理工具组合入口：应用目录、日志、异步控制循环与 Slint 生命周期。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::{env, fs, sync::Mutex, time::Duration};

use dedup_core::{DesktopConfig, logging::SizeRotatingWriter};
use dedup_desktop_core::{
    app::{DesktopApp, UiCommand},
    view_state::DesktopPaths,
};
use dedup_desktop_ui::{MainWindow, apply_event, bind_commands};
use dedup_windows::AppLayout;
use slint::ComponentHandle;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let layout = AppLayout::from_executable(&env::current_exe()?)?;
    prepare_directories(&layout)?;
    initialize_file_log(&layout)?;
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
            if window_weak
                .upgrade_in_event_loop(move |window| apply_event(&window, &binding, event))
                .is_err()
            {
                break;
            }
        }
    });

    window.run()?;
    runtime.block_on(app.send(UiCommand::Shutdown)).ok();
    runtime.block_on(async {
        let _ = tokio::time::timeout(Duration::from_secs(2), event_task).await;
    });
    Ok(())
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
fn initialize_file_log(layout: &AppLayout) -> Result<(), Box<dyn std::error::Error>> {
    let writer = SizeRotatingWriter::production(layout.desktop_logs(), "desktop")?;
    tracing_subscriber::fmt()
        .with_ansi(false)
        .with_target(false)
        .with_writer(Mutex::new(writer))
        .try_init()
        .map_err(|error| std::io::Error::other(error.to_string()))?;
    Ok(())
}
