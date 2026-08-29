use std::{net::SocketAddr, time::Duration};

use dedup_core::{MachineId, product_id};
use dedup_node_engine::{
    actor::NodeEngine,
    runtime_tasks::{
        RuntimeFailureUpdate, RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate,
        RuntimeTaskKind, RuntimeTaskState,
    },
    server::{NodeRequestHandler, NodeServer},
};
use dedup_node_store::NodeStore;
use dedup_protocol::{PROTOCOL_VERSION, proto};
use dedup_transport::{ClientConnection, FrameClass, FrameReader, FrameWriter};
use prost::Message;
use tokio::{
    net::TcpStream,
    sync::{broadcast, oneshot},
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
async fn runtime_events_actor_lists_pages_details_and_pushes_terminal_once() {
    let directory = tempfile::tempdir().unwrap();
    let machine = MachineId::from_sha256([0xb1; 32]);
    let store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let (handle, actor) =
        NodeEngine::spawn_for_test(store, "127.0.0.1:39091".parse().unwrap(), directory.path());
    let registry = handle.runtime_tasks_for_test();
    let first = registry
        .begin(RuntimeTaskKind::Scan, machine.clone(), "扫描一")
        .await;
    let second = registry
        .begin(RuntimeTaskKind::Delete, machine, "删除二")
        .await;
    first
        .update_stage(RuntimeStageUpdate::running(
            RuntimeStage::ReadMd5,
            RuntimeProgressUnit::Bytes,
            7,
            Some(10),
        ))
        .await
        .unwrap();
    for index in 0..25 {
        first
            .record_failure(RuntimeFailureUpdate {
                stage: RuntimeStage::ReadMd5,
                display_path: format!(r"D:\broken-{index}.bin"),
                message: format!("failure-{index}"),
            })
            .await
            .unwrap();
    }

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown_tx, shutdown_rx) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        handle.clone(),
        shutdown_rx,
    ));
    let connection = ClientConnection::connect(address).await.unwrap();
    hello_connection(&connection).await;

    let first_page = connection
        .request(proto::envelope::Payload::ListRuntimeTasks(
            proto::ListRuntimeTasks {
                cursor: String::new(),
                limit: 1,
                tasks: Vec::new(),
                next_cursor: String::new(),
            },
        ))
        .await
        .unwrap();
    assert_ne!(first_page.request_id, 0);
    let Some(proto::envelope::Payload::ListRuntimeTasks(first_page)) = first_page.payload else {
        panic!("expected runtime task page");
    };
    assert_eq!(first_page.tasks.len(), 1);
    assert!(!first_page.next_cursor.is_empty());
    let second_page = connection
        .request(proto::envelope::Payload::ListRuntimeTasks(
            proto::ListRuntimeTasks {
                cursor: first_page.next_cursor,
                limit: 1,
                tasks: Vec::new(),
                next_cursor: String::new(),
            },
        ))
        .await
        .unwrap();
    let Some(proto::envelope::Payload::ListRuntimeTasks(second_page)) = second_page.payload else {
        panic!("expected second runtime task page");
    };
    assert_eq!(second_page.tasks.len(), 1);
    assert!(second_page.next_cursor.is_empty());

    let details = connection
        .request(proto::envelope::Payload::GetRuntimeTaskDetails(
            proto::GetRuntimeTaskDetails {
                runtime_task_id: first.id().into(),
                details: None,
            },
        ))
        .await
        .unwrap();
    let Some(proto::envelope::Payload::GetRuntimeTaskDetails(details)) = details.payload else {
        panic!("expected runtime details");
    };
    assert_eq!(details.details.unwrap().failures.len(), 20);
    let missing = connection
        .request(proto::envelope::Payload::GetRuntimeTaskDetails(
            proto::GetRuntimeTaskDetails {
                runtime_task_id: "missing-runtime".into(),
                details: None,
            },
        ))
        .await
        .unwrap();
    assert!(matches!(
        missing.payload,
        Some(proto::envelope::Payload::Error(proto::Error { code, .. }))
            if code == proto::ErrorCode::NotFound as i32
    ));

    second.update_overall(1, Some(2), 0, 0).await.unwrap();
    let progress_event = tokio::time::timeout(Duration::from_secs(1), connection.next_event())
        .await
        .expect("首次运行进度应发布快照")
        .unwrap();
    assert!(matches!(
        progress_event.payload,
        Some(proto::envelope::Payload::RuntimeTaskChanged(
            proto::RuntimeTaskChanged {
                runtime_task_id,
                state,
                ..
            }
        )) if runtime_task_id == second.id() && state == "running"
    ));
    second.finish(RuntimeTaskState::Completed).await.unwrap();
    let event = connection.next_event().await.unwrap();
    assert_eq!(event.request_id, 0);
    assert!(matches!(
        event.payload,
        Some(proto::envelope::Payload::RuntimeTaskChanged(
            proto::RuntimeTaskChanged {
                runtime_task_id,
                state,
                ..
            }
        )) if runtime_task_id == second.id() && state == "completed"
    ));
    assert!(second.finish(RuntimeTaskState::Failed).await.is_err());
    assert!(
        tokio::time::timeout(Duration::from_millis(30), connection.next_event())
            .await
            .is_err(),
        "terminal event must be broadcast exactly once"
    );
    let ping_response = connection
        .request(proto::envelope::Payload::Ping(proto::Ping { nonce: 99 }))
        .await
        .unwrap();
    assert_ne!(ping_response.request_id, 0);
    assert!(matches!(
        ping_response.payload,
        Some(proto::envelope::Payload::Ping(proto::Ping { nonce: 99 }))
    ));

    drop(connection);
    shutdown_tx.send(()).unwrap();
    server.await.unwrap().unwrap();
    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[derive(Clone)]
struct LaggedEventHandler {
    events: broadcast::Sender<proto::RuntimeTaskChanged>,
}

impl NodeRequestHandler for LaggedEventHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        proto::Envelope {
            request_id: request.request_id,
            payload: request.payload,
        }
    }

    fn subscribe_runtime_events(&self) -> Option<broadcast::Receiver<proto::RuntimeTaskChanged>> {
        Some(self.events.subscribe())
    }
}

#[tokio::test]
async fn runtime_events_lagged_subscriber_keeps_responses_and_disconnect_cleanup_alive() {
    let (client, server_stream) = tokio::io::duplex(128);
    let (events, _) = broadcast::channel(2);
    let handler = LaggedEventHandler {
        events: events.clone(),
    };
    let server = tokio::spawn(NodeServer::serve_stream_for_test(server_stream, handler));
    let (client_read, client_write) = tokio::io::split(client);
    let mut reader = FrameReader::new(client_read);
    let mut writer = FrameWriter::new(client_write);
    write_hello(&mut writer, 500).await;
    read_envelope(&mut reader).await;

    for index in 0..200 {
        let _ = events.send(proto::RuntimeTaskChanged {
            runtime_task_id: format!("runtime-{index}"),
            state: "completed".into(),
            ..Default::default()
        });
    }
    write_envelope(&mut writer, ping(501, 501)).await;
    let response = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let envelope = read_envelope(&mut reader).await;
            if envelope.request_id == 501 {
                break envelope;
            }
            assert_eq!(envelope.request_id, 0);
        }
    })
    .await
    .expect("lagged event receiver must not starve a normal response");
    assert!(matches!(
        response.payload,
        Some(proto::envelope::Payload::Ping(proto::Ping { nonce: 501 }))
    ));

    drop(reader);
    drop(writer);
    tokio::time::timeout(Duration::from_secs(1), server)
        .await
        .expect("disconnect must stop reader and writer even while event sender remains alive")
        .unwrap()
        .unwrap();
}

async fn hello_connection(connection: &ClientConnection) {
    let response = connection
        .request(proto::envelope::Payload::Hello(proto::Hello {
            protocol_version: PROTOCOL_VERSION,
            product_id: product_id().into(),
            peer_name: "runtime-events-test".into(),
        }))
        .await
        .unwrap();
    assert!(matches!(
        response.payload,
        Some(proto::envelope::Payload::Hello(_))
    ));
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
