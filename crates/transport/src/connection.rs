//! 单 TCP 连接上的请求复用、主动事件和断线收束。

use std::{
    net::SocketAddr,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
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
        let (read, write) = stream.into_split();
        let pending = Arc::new(PendingRequests::new());
        let outgoing = PriorityWriter::new(64, 8);
        let (event_sender, events) = mpsc::channel(128);

        tokio::spawn(read_loop(
            read,
            Arc::clone(&pending),
            outgoing.clone(),
            event_sender,
        ));
        tokio::spawn(write_loop(write, Arc::clone(&pending), outgoing.clone()));

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

async fn read_loop(
    read: OwnedReadHalf,
    pending: Arc<PendingRequests>,
    outgoing: PriorityWriter<proto::Envelope>,
    events: mpsc::Sender<proto::Envelope>,
) {
    let mut reader = FrameReader::new(read);
    while let Ok(payload) = reader.read_frame().await {
        let Ok(envelope) = proto::Envelope::decode(payload.as_slice()) else {
            break;
        };
        if envelope.request_id == 0 {
            if events.send(envelope).await.is_err() {
                break;
            }
        } else {
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
) {
    let mut writer = FrameWriter::new(write);
    while let Some(envelope) = outgoing.next().await {
        if writer
            .write_frame(&envelope.encode_to_vec(), FrameClass::Ordinary)
            .await
            .is_err()
        {
            break;
        }
    }
    pending.fail_all();
    outgoing.close().await;
}
