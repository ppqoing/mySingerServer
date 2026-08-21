use std::{
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use dedup_core::{MachineId, NodeEndpoint};
use dedup_desktop_core::node_session::{NodeSession, SessionError};
use dedup_node_engine::{
    actor::NodeEngine,
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskState},
    server::{NodeRequestHandler, NodeServer},
};
use dedup_node_store::NodeStore;
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

#[tokio::test]
async fn runtime_tasks_share_one_connection_and_demux_terminal_events() {
    let directory = tempfile::tempdir().unwrap();
    let machine_id = MachineId::parse(&"ad".repeat(32)).unwrap();
    let store = NodeStore::open_in_memory(machine_id.clone()).unwrap();
    let (handle, actor) =
        NodeEngine::spawn_for_test(store, "127.0.0.1:39091".parse().unwrap(), directory.path());
    let registry = handle.runtime_tasks_for_test();
    let first = registry
        .begin(RuntimeTaskKind::Scan, machine_id.clone(), "扫描")
        .await;
    let second = registry
        .begin(RuntimeTaskKind::Delete, machine_id.clone(), "删除")
        .await;
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown_sender, shutdown) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(listener, handle.clone(), shutdown));
    let session = NodeSession::connect(NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    })
    .await
    .unwrap();

    let first_page = session.list_runtime_tasks("", 1).await.unwrap();
    assert_eq!(first_page.tasks.len(), 1);
    assert!(!first_page.next_cursor.is_empty());
    let second_page = session
        .list_runtime_tasks(&first_page.next_cursor, 1)
        .await
        .unwrap();
    assert_eq!(second_page.tasks.len(), 1);
    assert!(second_page.next_cursor.is_empty());
    let details = session.runtime_task_details(first.id()).await.unwrap();
    assert_eq!(
        details.summary.as_ref().unwrap().runtime_task_id,
        first.id()
    );
    let missing = session
        .runtime_task_details("missing-runtime-task")
        .await
        .unwrap_err();
    assert!(matches!(
        missing,
        SessionError::Protocol { code, .. } if code == proto::ErrorCode::NotFound as i32
    ));

    second.update_overall(1, Some(2), 0, 0).await.unwrap();
    assert!(
        tokio::time::timeout(Duration::from_millis(30), session.next_runtime_event())
            .await
            .is_err()
    );
    second.finish(RuntimeTaskState::Completed).await.unwrap();
    let (status, event) = tokio::join!(session.status(), session.next_runtime_event());
    assert_eq!(status.unwrap().machine_id, machine_id.as_str());
    let event = event.unwrap();
    assert_eq!(event.runtime_task_id, second.id());
    assert_eq!(event.state, "completed");

    for index in 0..160 {
        registry
            .begin(
                RuntimeTaskKind::Scan,
                machine_id.clone(),
                format!("终态洪峰 {index}"),
            )
            .await
            .finish(RuntimeTaskState::Completed)
            .await
            .unwrap();
        tokio::task::yield_now().await;
    }
    let (status, page, details) = tokio::time::timeout(Duration::from_secs(1), async {
        tokio::join!(
            session.status(),
            session.list_runtime_tasks("", 1),
            session.runtime_task_details(first.id()),
        )
    })
    .await
    .expect("未消费的有界终态事件不得阻塞普通响应 demux");
    assert_eq!(status.unwrap().machine_id, machine_id.as_str());
    assert_eq!(page.unwrap().tasks.len(), 1);
    assert_eq!(
        details.unwrap().summary.as_ref().unwrap().runtime_task_id,
        first.id()
    );

    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
    let disconnected = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            match session.next_runtime_event().await {
                Ok(_) => continue,
                Err(error) => break error,
            }
        }
    })
    .await
    .expect("server disconnect must terminate the event reader");
    assert!(matches!(disconnected, SessionError::Transport(_)));
    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}
