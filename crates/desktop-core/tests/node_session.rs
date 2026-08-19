use std::{
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use dedup_core::{MachineId, NodeEndpoint};
use dedup_desktop_core::node_session::NodeSession;
use dedup_node_engine::server::{NodeRequestHandler, NodeServer};
use dedup_protocol::proto;
use tokio::sync::{oneshot, watch};

#[derive(Clone)]
struct StatusHandler {
    machine_id: MachineId,
}

impl NodeRequestHandler for StatusHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        let payload = match request.payload {
            Some(proto::envelope::Payload::NodeStatus(_)) => {
                proto::envelope::Payload::NodeStatus(proto::NodeStatus {
                    machine_id: self.machine_id.as_str().into(),
                    listen_address: "127.0.0.1:0".into(),
                    ..Default::default()
                })
            }
            _ => proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::InvalidRequest as i32,
                message: "fixture only accepts status".into(),
            }),
        };
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(payload),
        }
    }
}

#[tokio::test]
async fn session_connects_with_v2_hello_and_reads_physical_machine_id() {
    let machine_id = MachineId::parse(&"ab".repeat(32)).unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown_sender, shutdown) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        StatusHandler {
            machine_id: machine_id.clone(),
        },
        shutdown,
    ));

    let session = NodeSession::connect(NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    })
    .await
    .unwrap();

    assert_eq!(session.machine_id(), &machine_id);
    assert_eq!(session.endpoint().port, address.port());
    drop(session);

    let (_close_sender, mut close) = watch::channel(false);
    let reconnected = tokio::time::timeout(
        Duration::from_secs(2),
        NodeSession::connect_with_retry(
            NodeEndpoint {
                ip: address.ip(),
                port: address.port(),
            },
            Duration::from_millis(10),
            &mut close,
            |_| {},
        ),
    )
    .await
    .unwrap()
    .unwrap();
    assert_eq!(reconnected.machine_id(), &machine_id);
    drop(reconnected);
    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn fixed_interval_retry_connects_when_the_same_manual_endpoint_comes_online() {
    let machine_id = MachineId::parse(&"ac".repeat(32)).unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let endpoint = NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    };
    let failures = Arc::new(AtomicUsize::new(0));
    let observed_failures = Arc::clone(&failures);
    let (_shutdown_sender, mut shutdown) = watch::channel(false);
    let connecting = tokio::spawn(async move {
        NodeSession::connect_with_retry(endpoint, Duration::from_millis(10), &mut shutdown, |_| {
            observed_failures.fetch_add(1, Ordering::Relaxed);
        })
        .await
    });

    let (shutdown_sender, server_shutdown) = oneshot::channel();
    let server = tokio::spawn(async move {
        let (first, _) = listener.accept().await.unwrap();
        drop(first);
        NodeServer::serve_until(
            listener,
            StatusHandler {
                machine_id: machine_id.clone(),
            },
            server_shutdown,
        )
        .await
    });
    let session = tokio::time::timeout(Duration::from_secs(2), connecting)
        .await
        .unwrap()
        .unwrap()
        .unwrap();

    assert_eq!(session.machine_id().as_str(), &"ac".repeat(32));
    assert!(failures.load(Ordering::Relaxed) >= 1);
    drop(session);
    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
}
