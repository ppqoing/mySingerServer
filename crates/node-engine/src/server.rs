//! 节点单管理连接 TCP 服务；每条连接先握手，再按 request_id 并发处理请求。

use std::{future::Future, io, sync::Arc};

use dedup_core::product_id;
use dedup_protocol::{PROTOCOL_VERSION, proto};
use dedup_transport::{FrameClass, FrameError, FrameReader, FrameWriter};
use prost::Message;
use thiserror::Error;
use tokio::{
    io::{AsyncRead, AsyncWrite},
    net::{TcpListener, TcpStream},
    sync::{Semaphore, mpsc, oneshot},
    task::JoinSet,
};

const RESPONSE_FLUSH_ATTEMPTS: usize = 2;

/// 节点 TCP 监听循环的启动或协议错误。
#[derive(Debug, Error)]
pub enum ServerError {
    /// 监听套接字接受连接失败。
    #[error("TCP 监听失败: {0}")]
    Io(#[from] io::Error),
}

/// NodeEngine actor 的协议请求入口；实现只负责业务，不拥有网络写端。
pub trait NodeRequestHandler: Clone + Send + Sync + 'static {
    /// 处理一个已握手连接上的请求，并保留原 request_id 返回响应。
    fn handle(&self, request: proto::Envelope) -> impl Future<Output = proto::Envelope> + Send;

    /// 指定 request 的响应完整写入客户端后通知业务层；业务提交失败不重写响应。
    fn response_flushed(
        &self,
        _request_id: u64,
    ) -> impl Future<Output = Result<(), String>> + Send {
        async { Ok(()) }
    }

    /// 管理连接结束后释放该连接持有的快照等短生命周期资源。
    fn connection_closed(&self) -> impl Future<Output = ()> + Send {
        async {}
    }
}

/// 只允许一个管理端连接、但允许该连接复用并发请求的 TCP 服务。
pub struct NodeServer;

impl NodeServer {
    /// 使用已经绑定的监听器运行，直到收到关闭信号；关闭时终止现有连接任务。
    pub async fn serve_until<H>(
        listener: TcpListener,
        handler: H,
        mut shutdown: oneshot::Receiver<()>,
    ) -> Result<(), ServerError>
    where
        H: NodeRequestHandler,
    {
        let slot = Arc::new(Semaphore::new(1));
        let mut connections = JoinSet::new();
        loop {
            tokio::select! {
                _ = &mut shutdown => break,
                accepted = listener.accept() => {
                    let (stream, _) = accepted?;
                    match Arc::clone(&slot).try_acquire_owned() {
                        Ok(permit) => {
                            let connection_handler = handler.clone();
                            connections.spawn(async move {
                                let _permit = permit;
                                serve_connection(stream, connection_handler).await;
                            });
                        }
                        Err(_) => {
                            connections.spawn(send_busy(stream));
                        }
                    }
                }
                Some(_) = connections.join_next(), if !connections.is_empty() => {}
            }
        }
        connections.abort_all();
        while connections.join_next().await.is_some() {}
        Ok(())
    }

    /// 通过与真实 TCP 连接相同的握手、并发请求和写出路径运行测试流。
    #[doc(hidden)]
    pub async fn serve_stream_for_test<S, H>(stream: S, handler: H) -> Result<(), String>
    where
        S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
        H: NodeRequestHandler,
    {
        let (read, write) = tokio::io::split(stream);
        serve_connection_io(read, write, handler).await
    }
}

async fn send_busy(stream: TcpStream) {
    let (_, write) = stream.into_split();
    let mut writer = FrameWriter::new(write);
    let response = error_envelope(0, proto::ErrorCode::NodeBusy, "节点已有管理连接");
    let _ = write_envelope(&mut writer, &response).await;
}

async fn serve_connection<H>(stream: TcpStream, handler: H)
where
    H: NodeRequestHandler,
{
    let (read, write) = stream.into_split();
    if let Err(error) = serve_connection_io(read, write, handler).await {
        eprintln!("节点连接在响应刷出后提交重启失败，连接已关闭: {error}");
    }
}

async fn serve_connection_io<R, W, H>(read: R, write: W, handler: H) -> Result<(), String>
where
    R: AsyncRead + Unpin + Send + 'static,
    W: AsyncWrite + Unpin + Send + 'static,
    H: NodeRequestHandler,
{
    let mut reader = FrameReader::new(read);
    let mut writer = FrameWriter::new(write);
    let Ok(first) = read_envelope(&mut reader).await else {
        return Ok(());
    };
    let request_id = first.request_id;
    let Some(proto::envelope::Payload::Hello(hello)) = first.payload else {
        let _ = write_envelope(
            &mut writer,
            &error_envelope(
                request_id,
                proto::ErrorCode::InvalidRequest,
                "首帧必须是 Hello",
            ),
        )
        .await;
        return Ok(());
    };
    if hello.protocol_version != PROTOCOL_VERSION || hello.product_id != product_id() {
        let _ = write_envelope(
            &mut writer,
            &error_envelope(
                request_id,
                proto::ErrorCode::Conflict,
                "协议或产品标识不匹配",
            ),
        )
        .await;
        return Ok(());
    }
    let welcome = proto::Envelope {
        request_id,
        payload: Some(proto::envelope::Payload::Hello(proto::Hello {
            protocol_version: PROTOCOL_VERSION,
            product_id: product_id().into(),
            peer_name: "node".into(),
        })),
    };
    if write_envelope(&mut writer, &welcome).await.is_err() {
        return Ok(());
    }

    let (responses, mut response_reader) = mpsc::channel::<proto::Envelope>(64);
    let writer_handler = handler.clone();
    let writer_task = tokio::spawn(async move {
        while let Some(response) = response_reader.recv().await {
            if write_envelope(&mut writer, &response).await.is_err() {
                return Ok(());
            }
            retry_response_flushed(&writer_handler, response.request_id).await?;
        }
        Ok::<(), String>(())
    });
    while let Ok(request) = read_envelope(&mut reader).await {
        let request_handler = handler.clone();
        let response_sender = responses.clone();
        tokio::spawn(async move {
            let response = request_handler.handle(request).await;
            let _ = response_sender.send(response).await;
        });
    }
    drop(responses);
    let writer_result = writer_task
        .await
        .map_err(|error| format!("节点响应写任务异常终止: {error}"))?;
    handler.connection_closed().await;
    writer_result
}

async fn retry_response_flushed<H>(handler: &H, request_id: u64) -> Result<(), String>
where
    H: NodeRequestHandler,
{
    let mut last_error = None;
    for _ in 0..RESPONSE_FLUSH_ATTEMPTS {
        match handler.response_flushed(request_id).await {
            Ok(()) => return Ok(()),
            Err(error) => last_error = Some(error),
        }
    }
    Err(format!(
        "request_id {request_id} 响应刷出后的宿主提交连续失败 {RESPONSE_FLUSH_ATTEMPTS} 次: {}",
        last_error.unwrap_or_else(|| "未知错误".to_owned())
    ))
}

async fn read_envelope<R>(reader: &mut FrameReader<R>) -> Result<proto::Envelope, ConnectionError>
where
    R: tokio::io::AsyncRead + Unpin,
{
    Ok(proto::Envelope::decode(
        reader.read_frame().await?.as_slice(),
    )?)
}

async fn write_envelope<W>(
    writer: &mut FrameWriter<W>,
    envelope: &proto::Envelope,
) -> Result<(), FrameError>
where
    W: tokio::io::AsyncWrite + Unpin,
{
    writer
        .write_frame(&envelope.encode_to_vec(), FrameClass::Ordinary)
        .await
}

fn error_envelope(request_id: u64, code: proto::ErrorCode, message: &str) -> proto::Envelope {
    proto::Envelope {
        request_id,
        payload: Some(proto::envelope::Payload::Error(proto::Error {
            code: code as i32,
            message: message.into(),
        })),
    }
}

#[derive(Debug, Error)]
enum ConnectionError {
    #[error(transparent)]
    Frame(#[from] FrameError),
    #[error(transparent)]
    Decode(#[from] prost::DecodeError),
}
