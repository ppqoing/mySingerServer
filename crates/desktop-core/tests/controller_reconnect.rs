use std::{
    env,
    net::IpAddr,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use dedup_core::{DesktopConfig, MachineId, NodeEndpoint};
use dedup_desktop_core::{
    app::{DesktopApp, UiCommand, UiEvent},
    view_state::{DesktopPaths, NodeConnectionState},
};
use dedup_node_engine::server::{NodeRequestHandler, NodeServer};
use dedup_protocol::proto;
use tempfile::TempDir;
use tokio::sync::{Notify, oneshot};

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
                    listen_address: "127.0.0.1".into(),
                    ..Default::default()
                })
            }
            Some(proto::envelope::Payload::ListTasks(mut page)) => {
                page.tasks.clear();
                page.next_cursor.clear();
                proto::envelope::Payload::ListTasks(page)
            }
            _ => proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::InvalidRequest as i32,
                message: "测试节点只提供状态和任务列表".into(),
            }),
        };
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(payload),
        }
    }
}

#[derive(Clone)]
struct SlowSyncHandler {
    machine_id: MachineId,
    pull_started: Arc<Notify>,
    release_pull: Arc<Notify>,
}

impl NodeRequestHandler for SlowSyncHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        let payload = match request.payload {
            Some(proto::envelope::Payload::NodeStatus(_)) => {
                proto::envelope::Payload::NodeStatus(proto::NodeStatus {
                    machine_id: self.machine_id.as_str().into(),
                    listen_address: "127.0.0.1".into(),
                    ..Default::default()
                })
            }
            Some(proto::envelope::Payload::ListTasks(mut page)) => {
                page.tasks.clear();
                page.next_cursor.clear();
                proto::envelope::Payload::ListTasks(page)
            }
            Some(proto::envelope::Payload::SyncAck(ack)) => proto::envelope::Payload::SyncAck(ack),
            Some(proto::envelope::Payload::PullChanges(_)) => {
                self.pull_started.notify_one();
                self.release_pull.notified().await;
                proto::envelope::Payload::SyncChangeBatch(proto::SyncChangeBatch::default())
            }
            _ => proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::InvalidRequest as i32,
                message: "测试节点只提供状态、任务列表和空同步批次".into(),
            }),
        };
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(payload),
        }
    }
}

#[derive(Clone)]
struct CountingSyncHandler {
    machine_id: MachineId,
    acknowledgements: Arc<AtomicUsize>,
}

impl NodeRequestHandler for CountingSyncHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        let payload = match request.payload {
            Some(proto::envelope::Payload::NodeStatus(_)) => {
                proto::envelope::Payload::NodeStatus(proto::NodeStatus {
                    machine_id: self.machine_id.as_str().into(),
                    listen_address: "127.0.0.1".into(),
                    ..Default::default()
                })
            }
            Some(proto::envelope::Payload::ListTasks(mut page)) => {
                page.tasks.clear();
                page.next_cursor.clear();
                proto::envelope::Payload::ListTasks(page)
            }
            Some(proto::envelope::Payload::SyncAck(ack)) => {
                self.acknowledgements.fetch_add(1, Ordering::SeqCst);
                proto::envelope::Payload::SyncAck(ack)
            }
            Some(proto::envelope::Payload::PullChanges(_)) => {
                proto::envelope::Payload::SyncChangeBatch(proto::SyncChangeBatch::default())
            }
            _ => proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::InvalidRequest as i32,
                message: "测试节点只提供状态、任务列表和空同步批次".into(),
            }),
        };
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(payload),
        }
    }
}

/// 固定重连间隔必须由桌面控制循环实际消费，而不只是保存在配置或独立帮助函数中。
#[tokio::test]
async fn controller_reconnects_when_manual_endpoint_comes_online() {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let machine_id = MachineId::parse(&"ad".repeat(32)).unwrap();
    let (first_shutdown_sender, first_shutdown) = oneshot::channel();
    let first_server = tokio::spawn(NodeServer::serve_until(
        listener,
        StatusHandler {
            machine_id: machine_id.clone(),
        },
        first_shutdown,
    ));

    let temp = TempDir::new().unwrap();
    let paths = desktop_paths(&temp);
    let config = DesktopConfig {
        nodes: vec![NodeEndpoint {
            ip: IpAddr::from([127, 0, 0, 1]),
            port: address.port(),
        }],
        reconnect_interval_seconds: 1,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(config, paths);

    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            if let Some(UiEvent::ViewChanged(state)) = events.recv().await
                && state.nodes()[0].connection == NodeConnectionState::Online
            {
                break;
            }
        }
    })
    .await
    .expect("桌面端没有建立初始成功会话");

    first_shutdown_sender.send(()).unwrap();
    first_server.await.unwrap().unwrap();
    tokio::time::timeout(Duration::from_secs(6), async {
        loop {
            if let Some(UiEvent::ViewChanged(state)) = events.recv().await
                && state.nodes()[0].connection == NodeConnectionState::Error
            {
                break;
            }
        }
    })
    .await
    .expect("已经建立的会话断开后没有进入 Error");

    let listener = tokio::net::TcpListener::bind(address).await.unwrap();
    let (shutdown_sender, shutdown) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        StatusHandler { machine_id },
        shutdown,
    ));

    let online = tokio::time::timeout(Duration::from_secs(4), async {
        loop {
            if let Some(UiEvent::ViewChanged(state)) = events.recv().await
                && state.nodes()[0].connection == NodeConnectionState::Online
            {
                break;
            }
        }
    })
    .await;

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
    assert!(online.is_ok(), "节点上线后桌面端没有按固定间隔自动重连");
}

/// 长同步必须运行在节点自己的同步循环，不能阻止唯一控制器消费 Shutdown。
#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn controller_remains_responsive_while_a_node_sync_is_waiting() {
    let postgres_url = env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let pull_started = Arc::new(Notify::new());
    let release_pull = Arc::new(Notify::new());
    let (shutdown_sender, shutdown) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        SlowSyncHandler {
            machine_id: MachineId::parse(&"ae".repeat(32)).unwrap(),
            pull_started: Arc::clone(&pull_started),
            release_pull: Arc::clone(&release_pull),
        },
        shutdown,
    ));
    let temp = TempDir::new().unwrap();
    let config = DesktopConfig {
        nodes: vec![NodeEndpoint {
            ip: address.ip(),
            port: address.port(),
        }],
        postgres_url: Some(postgres_url),
        reconnect_interval_seconds: 1,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(config, desktop_paths(&temp));

    tokio::time::timeout(Duration::from_secs(5), pull_started.notified())
        .await
        .expect("自动同步没有进入节点 PullChanges");
    app.send(UiCommand::Shutdown).await.unwrap();
    let responsive = tokio::time::timeout(Duration::from_secs(1), async {
        loop {
            if events.recv().await == Some(UiEvent::ShutdownComplete) {
                break;
            }
        }
    })
    .await;

    release_pull.notify_waiters();
    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
    assert!(
        responsive.is_ok(),
        "节点同步等待期间控制器没有及时处理 Shutdown"
    );
}

/// 已建立的 PG client 被服务端终止后，节点同步循环必须重建连接并再次 ACK。
#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn controller_reconnects_postgres_after_established_clients_are_terminated() {
    let postgres_url = env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let acknowledgements = Arc::new(AtomicUsize::new(0));
    let (shutdown_sender, shutdown) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        CountingSyncHandler {
            machine_id: MachineId::parse(&"af".repeat(32)).unwrap(),
            acknowledgements: Arc::clone(&acknowledgements),
        },
        shutdown,
    ));
    let temp = TempDir::new().unwrap();
    let config = DesktopConfig {
        nodes: vec![NodeEndpoint {
            ip: address.ip(),
            port: address.port(),
        }],
        postgres_url: Some(postgres_url.clone()),
        reconnect_interval_seconds: 1,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(config, desktop_paths(&temp));
    wait_for_acknowledgements(&acknowledgements, 1, Duration::from_secs(5)).await;

    let (killer, connection) = tokio_postgres::connect(&postgres_url, tokio_postgres::NoTls)
        .await
        .unwrap();
    let connection = tokio::spawn(async move {
        let _ = connection.await;
    });
    let terminated = killer
        .query(
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity \
             WHERE datname = current_database() AND pid <> pg_backend_pid()",
            &[],
        )
        .await
        .unwrap();
    assert!(!terminated.is_empty(), "测试必须终止至少一个既有 PG client");
    let baseline = acknowledgements.load(Ordering::SeqCst);
    wait_for_acknowledgements(&acknowledgements, baseline + 1, Duration::from_secs(15)).await;

    app.send(UiCommand::Shutdown).await.unwrap();
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            if events.recv().await == Some(UiEvent::ShutdownComplete) {
                break;
            }
        }
    })
    .await
    .expect("PG 重连后控制器仍应正常关闭");
    drop(killer);
    connection.abort();
    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
}

async fn wait_for_acknowledgements(
    acknowledgements: &AtomicUsize,
    expected: usize,
    timeout: Duration,
) {
    tokio::time::timeout(timeout, async {
        while acknowledgements.load(Ordering::SeqCst) < expected {
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .unwrap_or_else(|_| panic!("同步 ACK 未达到 {expected}"));
}

fn desktop_paths(temp: &TempDir) -> DesktopPaths {
    DesktopPaths {
        data: temp.path().to_path_buf(),
        logs: temp.path().join("logs"),
        cache: temp.path().join("cache"),
        config: temp.path().join("config.toml"),
    }
}
