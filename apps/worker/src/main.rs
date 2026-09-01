//! 媒体计算子进程入口；只负责匿名管道协议与媒体流水线装配。

mod protocol_loop;

use std::{env, process, sync::Mutex};

use dedup_core::logging::{
    FallbackLogWriter, ProcessDiagnostics, SizeRotatingWriter, log_filter, log_filter_from_env,
};
use dedup_media_ffmpeg::Ffmpeg;
use dedup_node_engine::worker::{FfmpegDecoder, WorkerPipeline, WorkerRequestHandler};
use dedup_windows::AppLayout;

/// 启动后 stdout 只写长度分帧的 WorkerEnvelope；诊断全部写入 data/node/logs。
#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let diagnostics = ProcessDiagnostics::new("worker");
    diagnostics.install_panic_hook();
    match run(&diagnostics).await {
        Ok(()) => Ok(()),
        Err(error) => {
            diagnostics.record_error("process_failed", "run", error.as_ref());
            Err(error)
        }
    }
}

/// 组合 Worker 日志、FFmpeg 和匿名管道协议循环。
async fn run(
    diagnostics: &ProcessDiagnostics,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let executable = env::current_exe()?;
    let layout = AppLayout::from_executable(&executable)?;
    initialize_file_log(&layout, diagnostics)?;
    tracing::info!(
        event = "process_started",
        process = "worker",
        pid = process::id(),
        version = env!("CARGO_PKG_VERSION"),
        "Worker 进程已启动"
    );

    let ffmpeg = Ffmpeg::load_from_worker_executable(&executable)?;
    let pipeline = WorkerPipeline::new(FfmpegDecoder::new(ffmpeg));
    let mut handler = WorkerRequestHandler::new(pipeline);
    protocol_loop::run_worker_protocol(
        tokio::io::stdin(),
        tokio::io::stdout(),
        &mut handler,
        process::id(),
    )
    .await?;
    tracing::info!(
        event = "process_stopped",
        process = "worker",
        pid = process::id(),
        reason = "protocol_closed",
        "Worker 进程正常停止"
    );
    Ok(())
}

/// 使用同步文件 writer，避免后台日志线程在 Worker 被 Job 强制结束时持有额外生命周期。
fn initialize_file_log(
    layout: &AppLayout,
    diagnostics: &ProcessDiagnostics,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let directory = layout.node_logs();
    let prefix = format!("worker-{}", process::id());
    let filter = match log_filter_from_env() {
        Ok(filter) => filter,
        Err(error) => {
            diagnostics.record_warning("configuration_rejected", "read_rust_log", &error);
            log_filter(None).expect("固定 INFO 过滤器必须有效")
        }
    };
    let writer = FallbackLogWriter::new(
        SizeRotatingWriter::production(&directory, &prefix)?,
        directory.join(format!("{prefix}.log")),
        diagnostics.clone(),
    );
    tracing_subscriber::fmt()
        .with_ansi(false)
        .with_target(false)
        .with_env_filter(filter)
        .with_writer(Mutex::new(writer))
        .try_init()?;
    diagnostics.mark_primary_ready();
    Ok(())
}
