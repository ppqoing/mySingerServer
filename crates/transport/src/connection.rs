//! 单 TCP 连接上的请求复用、主动事件和断线收束。

use std::{
    future::Future,
    net::SocketAddr,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
};

use dedup_protocol::proto;
use prost::Message;
use thiserror::Error;
use tokio::{
    net::{TcpStream, tcp::OwnedReadHalf, tcp::OwnedWriteHalf},
    sync::{Mutex, mpsc},
};

use crate::{FrameClass, FrameError, FrameReader, FrameWriter, PendingRequests, PriorityWriter};

/// TCP 分帧、Protobuf 编解码和连接生命周期错误。
#[derive(Debug, Error)]
pub enum TransportError {
    /// 长度头、尺寸限制或底层流读写失败。
    #[error(transparent)]
    Frame(#[from] FrameError),
    /// 收到的正文不是有效 Envelope。
    #[error("Protobuf Envelope 解码失败: {0}")]
    Decode(#[from] prost::DecodeError),
    /// 写循环或读循环已经结束，所有请求均已收束。
    #[error("TCP 连接已断开")]
    ConnectionClosed,
    /// 主动事件接收端已经关闭。
    #[error("主动事件通道已关闭")]
    EventChannelClosed,
}

/// 管理端与一个节点之间可并发复用请求的持久连接。
pub struct ClientConnection {
    next_request_id: AtomicU64,
    pending: Arc<PendingRequests>,
    outgoing: PriorityWriter<proto::Envelope>,
    events: Mutex<mpsc::Receiver<proto::Envelope>>,
}

impl ClientConnection {
    /// 连接手工配置的节点地址并启动独立读写循环。
    pub async fn connect(address: SocketAddr) -> Result<Self, TransportError> {
        Self::from_stream(TcpStream::connect(address).await.map_err(FrameError::Io)?).await
    }

    /// 从一个已连接 TCP 流创建请求复用器，供节点会话和集成测试使用。
    pub async fn from_stream(stream: TcpStream) -> Result<Self, TransportError> {
        let peer = stream.peer_addr().map_err(FrameError::Io)?;
        let (read, write) = stream.into_split();
        let pending = Arc::new(PendingRequests::new());
        let outgoing = PriorityWriter::new(64, 8);
        let (event_sender, events) = mpsc::channel(128);
        let termination_logged = Arc::new(AtomicBool::new(false));

        spawn_observed_connection_task(
            "read_loop",
            read_loop(
                read,
                Arc::clone(&pending),
                outgoing.clone(),
                event_sender,
                peer,
                Arc::clone(&termination_logged),
            ),
        );
        spawn_observed_connection_task(
            "write_loop",
            write_loop(
                write,
                Arc::clone(&pending),
                outgoing.clone(),
                peer,
                termination_logged,
            ),
        );

        Ok(Self {
            next_request_id: AtomicU64::new(1),
            pending,
            outgoing,
            events: Mutex::new(events),
        })
    }

    /// 发送一个高优先级请求并等待具有相同非零 request ID 的响应。
    pub async fn request(
        &self,
        payload: proto::envelope::Payload,
    ) -> Result<proto::Envelope, TransportError> {
        let request_id = self.allocate_request_id();
        let response = self.pending.register(request_id);
        if let Err(error) = self
            .outgoing
            .send_high(proto::Envelope {
                request_id,
                payload: Some(payload),
            })
            .await
        {
            self.pending.fail(request_id);
            return Err(error);
        }
        response
            .await
            .map_err(|_| TransportError::ConnectionClosed)?
    }

    /// 等待节点主动推送的 `request_id = 0` 任务或状态事件。
    pub async fn next_event(&self) -> Result<proto::Envelope, TransportError> {
        self.events
            .lock()
            .await
            .recv()
            .await
            .ok_or(TransportError::EventChannelClosed)
    }

    fn allocate_request_id(&self) -> u64 {
        loop {
            let request_id = self.next_request_id.fetch_add(1, Ordering::Relaxed);
            if request_id != 0 {
                return request_id;
            }
        }
    }
}

impl Drop for ClientConnection {
    fn drop(&mut self) {
        self.outgoing.close_now();
    }
}

async fn read_loop(
    read: OwnedReadHalf,
    pending: Arc<PendingRequests>,
    outgoing: PriorityWriter<proto::Envelope>,
    events: mpsc::Sender<proto::Envelope>,
    peer: SocketAddr,
    termination_logged: Arc<AtomicBool>,
) {
    let mut reader = FrameReader::new(read);
    loop {
        let payload = match reader.read_frame().await {
            Ok(payload) => payload,
            Err(error @ FrameError::Truncated) => {
                log_expected_close_once(&termination_logged, peer, "read", &error);
                break;
            }
            Err(error) => {
                log_connection_failure_once(&termination_logged, peer, "read", &error);
                break;
            }
        };
        let envelope = match proto::Envelope::decode(payload.as_slice()) {
            Ok(envelope) => envelope,
            Err(error) => {
                log_connection_failure_once(&termination_logged, peer, "decode", &error);
                break;
            }
        };
        if envelope.request_id == 0 {
            match events.try_send(envelope) {
                Ok(()) => {}
                Err(mpsc::error::TrySendError::Full(dropped_event)) => {
                    drop(dropped_event);
                    tracing::warn!(
                        event = "request_failed",
                        component = "transport_connection",
                        request_id = 0_u64,
                        operation = "deliver_unsolicited_event",
                        peer = %peer,
                        error = "event channel full",
                        "主动事件通道已满，本条事件无法投递"
                    );
                }
                Err(mpsc::error::TrySendError::Closed(dropped_event)) => {
                    drop(dropped_event);
                    tracing::info!(
                        event = "expected_condition",
                        component = "transport_connection",
                        operation = "deliver_unsolicited_event",
                        reason = "event_receiver_closed",
                        peer = %peer,
                        error = "event channel closed",
                        "主动事件消费者已结束，普通响应连接继续运行"
                    );
                }
            }
        } else {
            // false 表示请求已由其它路径移除；send 产生的预期 Err 由 PendingRequests 自身记录。
            pending.resolve(envelope);
        }
    }
    pending.fail_all();
    outgoing.close().await;
}

async fn write_loop(
    write: OwnedWriteHalf,
    pending: Arc<PendingRequests>,
    outgoing: PriorityWriter<proto::Envelope>,
    peer: SocketAddr,
    termination_logged: Arc<AtomicBool>,
) {
    let mut writer = FrameWriter::new(write);
    while let Some(envelope) = outgoing.next().await {
        if let Err(error) = writer
            .write_frame(&envelope.encode_to_vec(), FrameClass::Ordinary)
            .await
        {
            log_connection_failure_once(&termination_logged, peer, "write", &error);
            break;
        }
    }
    pending.fail_all();
    outgoing.close().await;
}

/// 启动连接循环并由独立观察器消费唯一 JoinError，避免 detached task panic 静默结束。
fn spawn_observed_connection_task(
    task_name: &'static str,
    future: impl Future<Output = ()> + Send + 'static,
) {
    let task = tokio::spawn(future);
    let observer = tokio::spawn(async move {
        if let Err(error) = task.await {
            tracing::error!(
                event = "background_task_failed",
                component = "transport_connection",
                task_name,
                operation = "join",
                error = %error,
                "TCP 连接后台循环异常终止"
            );
        }
    });
    // 观察器内部消费连接循环的 JoinError，自身没有业务错误返回。
    drop(observer);
}

/// 在读写两个并行循环之间只记录一次连接根因。
fn log_connection_failure_once(
    logged: &AtomicBool,
    peer: SocketAddr,
    operation: &'static str,
    error: &dyn std::fmt::Display,
) {
    if !logged.swap(true, Ordering::AcqRel) {
        tracing::warn!(
            event = "transport_connection_failed",
            peer = %peer,
            operation,
            error = %error,
            "TCP 连接处理失败"
        );
    }
}

/// 把底层以错误表示的对端关闭记录为低频预期状态。
fn log_expected_close_once(
    logged: &AtomicBool,
    peer: SocketAddr,
    operation: &'static str,
    error: &dyn std::fmt::Display,
) {
    if !logged.swap(true, Ordering::AcqRel) {
        tracing::info!(
            event = "expected_condition",
            peer = %peer,
            operation,
            reason = "peer_closed",
            error = %error,
            "TCP 对端已关闭连接"
        );
    }
}
