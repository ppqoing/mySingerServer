//! Worker stdin/stdout 的长度分帧请求循环；生产入口与进程测试共用同一实现。

use std::time::Instant;

use dedup_node_engine::worker::{MediaDecoder, WorkerRequestHandler};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_transport::{FrameClass, FrameError, FrameReader, FrameWriter};
use prost::Message;

/// 写出 Ready 后持续读取请求，并按 handler 顺序逐帧写出全部响应。
///
/// stdin 在帧边界正常关闭时，`FrameReader` 返回 `Truncated`，此处按 Worker 生命周期契约
/// 正常结束；其他 framing、Protobuf 或写出错误原样返回给进程入口。
pub async fn run_worker_protocol<R, W, D>(
    input: R,
    output: W,
    handler: &mut WorkerRequestHandler<D>,
    process_id: u32,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>>
where
    R: tokio::io::AsyncRead + Unpin,
    W: tokio::io::AsyncWrite + Unpin,
    D: MediaDecoder,
{
    let mut reader = FrameReader::new(input);
    let mut writer = FrameWriter::new(output);
    write_envelope(
        &mut writer,
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::WorkerReady(proto::WorkerReady {
                process_id,
            })),
        },
    )
    .await?;
    tracing::info!(process_id, "Worker 已就绪");

    loop {
        let payload = match reader.read_frame().await {
            Ok(payload) => payload,
            Err(FrameError::Truncated) => return Ok(()),
            Err(error) => return Err(error.into()),
        };
        let request = proto::WorkerEnvelope::decode(payload.as_slice())?;
        match request.payload {
            Some(worker_envelope::Payload::ComputeBaseFeatures(command)) => {
                run_base_compute(&mut writer, handler, command).await?;
            }
            payload => {
                for response in handler.handle(proto::WorkerEnvelope { payload }) {
                    write_envelope(&mut writer, response).await?;
                }
            }
        }
    }
}

/// 在真实执行边界逐帧写出一次性基础计算阶段和唯一终态。
async fn run_base_compute<W, D>(
    writer: &mut FrameWriter<W>,
    handler: &WorkerRequestHandler<D>,
    command: proto::ComputeBaseFeatures,
) -> Result<(), FrameError>
where
    W: tokio::io::AsyncWrite + Unpin,
    D: MediaDecoder,
{
    let task_id = command.task_id.clone();
    let item_id = command.item_id.clone();
    let request_started = Instant::now();
    write_phase(
        writer,
        &task_id,
        &item_id,
        proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
        request_started,
    )
    .await?;
    match handler.prepare_base_features(command) {
        Ok(prepared) => {
            write_envelope(
                writer,
                proto::WorkerEnvelope {
                    payload: Some(worker_envelope::Payload::BaseSourceReadComplete(
                        proto::BaseSourceReadComplete {
                            task_id: task_id.clone(),
                            item_id: item_id.clone(),
                            request_elapsed_us: Some(elapsed_us(request_started)),
                        },
                    )),
                },
            )
            .await?;
            write_phase(
                writer,
                &task_id,
                &item_id,
                proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
                request_started,
            )
            .await?;
            let terminal = handler.finish_base_features(prepared);
            write_phase(
                writer,
                &task_id,
                &item_id,
                proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
                request_started,
            )
            .await?;
            write_envelope(writer, terminal).await
        }
        Err(terminal) => {
            write_phase(
                writer,
                &task_id,
                &item_id,
                proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
                request_started,
            )
            .await?;
            write_envelope(writer, terminal).await
        }
    }
}

/// 写出一个携带请求累计耗时的真实 Worker 阶段事件。
async fn write_phase<W>(
    writer: &mut FrameWriter<W>,
    task_id: &str,
    item_id: &str,
    phase: proto::RuntimeWorkerPhase,
    request_started: Instant,
) -> Result<(), FrameError>
where
    W: tokio::io::AsyncWrite + Unpin,
{
    write_envelope(
        writer,
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::WorkerPhaseChanged(
                proto::WorkerPhaseChanged {
                    task_id: task_id.into(),
                    item_id: item_id.into(),
                    phase: phase as i32,
                    request_elapsed_us: Some(elapsed_us(request_started)),
                },
            )),
        },
    )
    .await
}

/// 把单调请求耗时饱和转换为协议微秒。
fn elapsed_us(started: Instant) -> u64 {
    started.elapsed().as_micros().try_into().unwrap_or(u64::MAX)
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
