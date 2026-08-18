//! 媒体计算子进程入口；只负责匿名管道协议与媒体流水线装配。

use std::{env, fs, process};

use dedup_media_ffmpeg::Ffmpeg;
use dedup_node_engine::worker::{FfmpegDecoder, WorkerPipeline, handle_worker_request};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_transport::{FrameClass, FrameError, FrameReader, FrameWriter};
use dedup_windows::AppLayout;
use prost::Message;

/// 启动后 stdout 只写长度分帧的 WorkerEnvelope；诊断全部写入 data/node/logs。
#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let executable = env::current_exe()?;
    let layout = AppLayout::from_executable(&executable)?;
    initialize_file_log(&layout)?;

    let ffmpeg = Ffmpeg::load_from_worker_executable(&executable)?;
    let pipeline = WorkerPipeline::new(FfmpegDecoder::new(ffmpeg));
    let mut reader = FrameReader::new(tokio::io::stdin());
    let mut writer = FrameWriter::new(tokio::io::stdout());

    write_envelope(
        &mut writer,
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::WorkerReady(proto::WorkerReady {
                process_id: process::id(),
            })),
        },
    )
    .await?;
    tracing::info!(process_id = process::id(), "Worker 已就绪");

    loop {
        let payload = match reader.read_frame().await {
            Ok(payload) => payload,
            Err(FrameError::Truncated) => return Ok(()),
            Err(error) => return Err(error.into()),
        };
        let request = proto::WorkerEnvelope::decode(payload.as_slice())?;
        let response = handle_worker_request(&pipeline, request);
        write_envelope(&mut writer, response).await?;
    }
}

/// 使用同步文件 writer，避免后台日志线程在 Worker 被 Job 强制结束时持有额外生命周期。
fn initialize_file_log(layout: &AppLayout) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let directory = layout.node_logs();
    fs::create_dir_all(&directory)?;
    let filename = format!("worker-{}.log", process::id());
    let appender = tracing_appender::rolling::never(directory, filename);
    tracing_subscriber::fmt()
        .with_ansi(false)
        .with_target(false)
        .with_writer(appender)
        .try_init()?;
    Ok(())
}

/// 编码并写出一个完整 WorkerEnvelope，保持 stdout 没有任何文本混入。
async fn write_envelope<W>(
    writer: &mut FrameWriter<W>,
    envelope: proto::WorkerEnvelope,
) -> Result<(), FrameError>
where
    W: tokio::io::AsyncWrite + Unpin,
{
    writer
        .write_frame(&envelope.encode_to_vec(), FrameClass::Ordinary)
        .await
}
