use std::{
    net::SocketAddr,
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_core::product_id;
use dedup_node_engine::server::{NodeRequestHandler, NodeServer};
use dedup_protocol::{PROTOCOL_VERSION, proto};
use dedup_transport::{FrameClass, FrameReader, FrameWriter};
use prost::Message;
use tokio::{
    net::TcpStream,
    sync::{Notify, oneshot},
};

#[derive(Clone)]
struct EchoHandler;

impl NodeRequestHandler for EchoHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        let payload = match request.payload {
            Some(proto::envelope::Payload::Ping(ping)) => {
                if ping.nonce == 1 {
                    tokio::time::sleep(Duration::from_millis(20)).await;
                }
                proto::envelope::Payload::Ping(ping)
            }
            _ => proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::InvalidRequest as i32,
                message: "fixture only accepts ping".into(),
            }),
        };
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(payload),
        }
    }
}

#[tokio::test]
async fn single_manager_is_exclusive_but_one_connection_handles_concurrent_requests() {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown_tx, shutdown_rx) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(listener, EchoHandler, shutdown_rx));

    let (mut first_reader, mut first_writer) = try_connect_and_hello(address, 1)
        .await
        .expect("第一个管理连接必须取得名额");

    let second = TcpStream::connect(address).await.unwrap();
    let (second_read, _) = second.into_split();
    let mut second_reader = FrameReader::new(second_read);
    let busy =
        proto::Envelope::decode(second_reader.read_frame().await.unwrap().as_slice()).unwrap();
    assert!(matches!(
        busy.payload,
        Some(proto::envelope::Payload::Error(proto::Error { code, .. }))
            if code == proto::ErrorCode::NodeBusy as i32
    ));

    write_envelope(&mut first_writer, ping(2, 1)).await;
    write_envelope(&mut first_writer, ping(3, 2)).await;
    let first_response = read_envelope(&mut first_reader).await;
    let second_response = read_envelope(&mut first_reader).await;
    let mut ids = [first_response.request_id, second_response.request_id];
    ids.sort();
    assert_eq!(ids, [2, 3]);

    drop(first_reader);
    drop(first_writer);
    let mut connected = None;
    for _ in 0..20 {
        match tokio::time::timeout(
            Duration::from_millis(100),
            try_connect_and_hello(address, 4),
        )
        .await
        {
            Ok(Some(value)) => {
                connected = Some(value);
                break;
            }
            Ok(None) | Err(_) => tokio::task::yield_now().await,
        }
    }
    assert!(connected.is_some(), "第一管理连接断开后必须释放名额");

    shutdown_tx.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn restart_response_notifies_handler_only_after_full_frame_write() {
    let (client, server_stream) = tokio::io::duplex(64);
    let handler = RecordingHandler::new(4096, None);
    let state = Arc::clone(&handler.state);
    let started = handler.started.clone();
    let flushed = handler.flushed.clone();
    let server = tokio::spawn(NodeServer::serve_stream_for_test(server_stream, handler));
    let (client_read, client_write) = tokio::io::split(client);
    let mut reader = FrameReader::new(client_read);
    let mut writer = FrameWriter::new(client_write);

    write_hello(&mut writer, 70).await;
    let welcome = read_envelope(&mut reader).await;
    assert!(matches!(
        welcome.payload,
        Some(proto::envelope::Payload::Hello(_))
    ));

    let started_wait = started.notified();
    write_envelope(&mut writer, ping(71, 9)).await;
    tokio::time::timeout(Duration::from_secs(1), started_wait)
        .await
        .unwrap();
    tokio::task::yield_now().await;
    assert!(state.lock().unwrap().flushed_ids.is_empty());

    let flushed_wait = flushed.notified();
    let response = read_envelope(&mut reader).await;
    assert_eq!(response.request_id, 71);
    tokio::time::timeout(Duration::from_secs(1), flushed_wait)
        .await
        .unwrap();
    assert_eq!(state.lock().unwrap().flushed_ids, [71]);

    drop(reader);
    drop(writer);
    tokio::time::timeout(Duration::from_secs(1), server)
        .await
        .unwrap()
        .unwrap();
}

#[tokio::test]
async fn restart_response_write_failure_does_not_notify_handler() {
    let (client, server_stream) = tokio::io::duplex(64);
    let release = Arc::new(Notify::new());
    let handler = RecordingHandler::new(4096, Some(Arc::clone(&release)));
    let state = Arc::clone(&handler.state);
    let started = handler.started.clone();
    let server = tokio::spawn(NodeServer::serve_stream_for_test(server_stream, handler));
    let (client_read, client_write) = tokio::io::split(client);
    let mut reader = FrameReader::new(client_read);
    let mut writer = FrameWriter::new(client_write);

    write_hello(&mut writer, 80).await;
    let welcome = read_envelope(&mut reader).await;
    assert!(matches!(
        welcome.payload,
        Some(proto::envelope::Payload::Hello(_))
    ));

    let started_wait = started.notified();
    write_envelope(&mut writer, ping(81, 10)).await;
    tokio::time::timeout(Duration::from_secs(1), started_wait)
        .await
        .unwrap();
    drop(reader);
    drop(writer);
    release.notify_one();
    tokio::time::timeout(Duration::from_secs(1), server)
        .await
        .unwrap()
        .unwrap();
    assert!(state.lock().unwrap().flushed_ids.is_empty());
}

#[derive(Default)]
struct RecordingState {
    flushed_ids: Vec<u64>,
}

#[derive(Clone)]
struct RecordingHandler {
    state: Arc<Mutex<RecordingState>>,
    response_bytes: usize,
    started: Arc<Notify>,
    flushed: Arc<Notify>,
    release: Option<Arc<Notify>>,
}

impl RecordingHandler {
    fn new(response_bytes: usize, release: Option<Arc<Notify>>) -> Self {
        Self {
            state: Arc::new(Mutex::new(RecordingState::default())),
            response_bytes,
            started: Arc::new(Notify::new()),
            flushed: Arc::new(Notify::new()),
            release,
        }
    }
}

impl NodeRequestHandler for RecordingHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        self.started.notify_one();
        if let Some(release) = &self.release {
            release.notified().await;
        }
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::Internal as i32,
                message: "x".repeat(self.response_bytes),
            })),
        }
    }

    async fn response_flushed(&self, request_id: u64) {
        self.state.lock().unwrap().flushed_ids.push(request_id);
        self.flushed.notify_one();
    }
}

async fn try_connect_and_hello(
    address: SocketAddr,
    request_id: u64,
) -> Option<(
    FrameReader<tokio::net::tcp::OwnedReadHalf>,
    FrameWriter<tokio::net::tcp::OwnedWriteHalf>,
)> {
    let stream = TcpStream::connect(address).await.unwrap();
    let (read, write) = stream.into_split();
    let mut reader = FrameReader::new(read);
    let mut writer = FrameWriter::new(write);
    write_envelope(
        &mut writer,
        proto::Envelope {
            request_id,
            payload: Some(proto::envelope::Payload::Hello(proto::Hello {
                protocol_version: PROTOCOL_VERSION,
                product_id: product_id().into(),
                peer_name: "desktop-test".into(),
            })),
        },
    )
    .await;
    let response = read_envelope(&mut reader).await;
    match response.payload {
        Some(proto::envelope::Payload::Hello(_)) => Some((reader, writer)),
        Some(proto::envelope::Payload::Error(proto::Error { code, .. }))
            if code == proto::ErrorCode::NodeBusy as i32 =>
        {
            None
        }
        payload => panic!("握手收到意外响应: {payload:?}"),
    }
}

async fn write_hello<W: tokio::io::AsyncWrite + Unpin>(
    writer: &mut FrameWriter<W>,
    request_id: u64,
) {
    write_envelope(
        writer,
        proto::Envelope {
            request_id,
            payload: Some(proto::envelope::Payload::Hello(proto::Hello {
                protocol_version: PROTOCOL_VERSION,
                product_id: product_id().into(),
                peer_name: "desktop-test".into(),
            })),
        },
    )
    .await;
}

fn ping(request_id: u64, nonce: u64) -> proto::Envelope {
    proto::Envelope {
        request_id,
        payload: Some(proto::envelope::Payload::Ping(proto::Ping { nonce })),
    }
}

async fn write_envelope<W: tokio::io::AsyncWrite + Unpin>(
    writer: &mut FrameWriter<W>,
    envelope: proto::Envelope,
) {
    writer
        .write_frame(&envelope.encode_to_vec(), FrameClass::Ordinary)
        .await
        .unwrap();
}

async fn read_envelope<R: tokio::io::AsyncRead + Unpin>(
    reader: &mut FrameReader<R>,
) -> proto::Envelope {
    proto::Envelope::decode(reader.read_frame().await.unwrap().as_slice()).unwrap()
}
