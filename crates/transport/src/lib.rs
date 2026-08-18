//! TCP 分帧、请求复用、事件分派和优先级写队列。
#![warn(missing_docs)]

mod connection;
mod frame;
mod pending;
mod priority_writer;

pub use connection::{ClientConnection, TransportError};
pub use frame::{
    FrameClass, FrameError, FrameReader, FrameWriter, MAX_ORDINARY_FRAME, encode_frame,
};
pub(crate) use pending::PendingRequests;
pub use priority_writer::PriorityWriter;

#[cfg(test)]
mod tests {
    use dedup_protocol::proto;
    use prost::Message;
    use tokio::io::AsyncWriteExt;
    use tokio::net::TcpListener;

    use super::{
        ClientConnection, FrameClass, FrameError, FrameReader, FrameWriter, PendingRequests,
        PriorityWriter, encode_frame,
    };

    /// 防止普通 Protobuf Envelope 绕过固定 8 MiB 分帧上限。
    #[test]
    fn rejects_ordinary_frame_above_eight_mib() {
        let frame = vec![0_u8; 8 * 1024 * 1024 + 1];
        assert!(matches!(
            encode_frame(&frame, FrameClass::Ordinary),
            Err(FrameError::TooLarge { .. })
        ));
    }

    /// 防止长度头使用本机小端序而破坏不同实现之间的互通。
    #[test]
    fn frame_length_is_four_byte_big_endian() {
        assert_eq!(
            &encode_frame(&[1, 2, 3], FrameClass::Ordinary).unwrap()[..4],
            &[0, 0, 0, 3]
        );
    }

    /// 防止零长度和正文截断在业务解码层才产生含糊错误。
    #[tokio::test]
    async fn reader_rejects_zero_and_truncated_frames() {
        let (mut zero_writer, zero_reader) = tokio::io::duplex(16);
        zero_writer.write_all(&[0, 0, 0, 0]).await.unwrap();
        drop(zero_writer);
        assert!(matches!(
            FrameReader::new(zero_reader).read_frame().await,
            Err(FrameError::Empty)
        ));

        let (mut short_writer, short_reader) = tokio::io::duplex(16);
        short_writer
            .write_all(&[0, 0, 0, 5, 1, 2, 3])
            .await
            .unwrap();
        drop(short_writer);
        assert!(matches!(
            FrameReader::new(short_reader).read_frame().await,
            Err(FrameError::Truncated)
        ));
    }

    /// 防止连续文件块长期占用写通道而饿死取消等控制消息。
    #[tokio::test]
    async fn control_message_preempts_next_file_chunk() {
        let writer = PriorityWriter::new(2, 2);
        writer.send_low("chunk-1").await.unwrap();
        writer.send_low("chunk-2").await.unwrap();
        writer.send_high("cancel").await.unwrap();
        assert_eq!(writer.next().await.unwrap(), "chunk-1");
        assert_eq!(writer.next().await.unwrap(), "cancel");
        assert_eq!(writer.next().await.unwrap(), "chunk-2");
    }

    /// 防止连接断开时请求永久留在 pending 表中等待。
    #[tokio::test]
    async fn disconnect_fails_every_pending_request() {
        let pending = PendingRequests::new();
        let first = pending.register(1);
        let second = pending.register(2);
        pending.fail_all();
        assert!(first.await.unwrap().is_err());
        assert!(second.await.unwrap().is_err());
    }

    /// 防止响应按到达顺序而不是 request_id 交给错误调用者。
    #[tokio::test]
    async fn response_is_dispatched_by_request_id() {
        let pending = PendingRequests::new();
        let response = pending.register(42);
        assert!(pending.resolve(proto::Envelope {
            request_id: 42,
            payload: Some(proto::envelope::Payload::Ping(proto::Ping { nonce: 7 })),
        }));
        assert_eq!(response.await.unwrap().unwrap().request_id, 42);
    }

    /// 防止真实 TCP 请求使用零 ID 或无法把同 ID 响应交还调用者。
    #[tokio::test]
    async fn client_connection_round_trips_unary_request() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let (read, write) = stream.into_split();
            let mut reader = FrameReader::new(read);
            let mut writer = FrameWriter::new(write);
            let request =
                proto::Envelope::decode(reader.read_frame().await.unwrap().as_slice()).unwrap();
            assert_ne!(request.request_id, 0);
            let response = proto::Envelope {
                request_id: request.request_id,
                payload: Some(proto::envelope::Payload::Ping(proto::Ping { nonce: 99 })),
            };
            writer
                .write_frame(&response.encode_to_vec(), FrameClass::Ordinary)
                .await
                .unwrap();
        });

        let client = ClientConnection::connect(address).await.unwrap();
        let response = client
            .request(proto::envelope::Payload::Ping(proto::Ping { nonce: 7 }))
            .await
            .unwrap();
        assert!(matches!(
            response.payload,
            Some(proto::envelope::Payload::Ping(proto::Ping { nonce: 99 }))
        ));
        server.await.unwrap();
    }
}
