//! 媒体计算子进程入口；只负责匿名管道协议与媒体流水线装配。

mod protocol_loop;

use std::{env, process, sync::Mutex};

use dedup_core::logging::SizeRotatingWriter;
use dedup_media_ffmpeg::Ffmpeg;
use dedup_node_engine::worker::{FfmpegDecoder, WorkerPipeline, WorkerRequestHandler};
use dedup_windows::AppLayout;

/// 启动后 stdout 只写长度分帧的 WorkerEnvelope；诊断全部写入 data/node/logs。
#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let executable = env::current_exe()?;
    let layout = AppLayout::from_executable(&executable)?;
    initialize_file_log(&layout)?;

    let ffmpeg = Ffmpeg::load_from_worker_executable(&executable)?;
    let pipeline = WorkerPipeline::new(FfmpegDecoder::new(ffmpeg));
    let mut handler = WorkerRequestHandler::new(pipeline);
    protocol_loop::run_worker_protocol(
        tokio::io::stdin(),
        tokio::io::stdout(),
        &mut handler,
        process::id(),
    )
    .await
}

/// 使用同步文件 writer，避免后台日志线程在 Worker 被 Job 强制结束时持有额外生命周期。
fn initialize_file_log(layout: &AppLayout) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let directory = layout.node_logs();
    let writer = SizeRotatingWriter::production(directory, format!("worker-{}", process::id()))?;
    tracing_subscriber::fmt()
        .with_ansi(false)
        .with_target(false)
        .with_writer(Mutex::new(writer))
        .try_init()?;
    Ok(())
}
