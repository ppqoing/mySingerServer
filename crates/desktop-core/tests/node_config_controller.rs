//! Desktop 远程 Node 配置加载、保存、重连与验证状态机。

use std::{
    net::IpAddr,
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_core::{DesktopConfig, NodeEndpoint};
use dedup_desktop_core::{
    app::{DesktopApp, UiCommand, UiEvent},
    view_state::{DesktopPaths, NodeConfigControllerState, NodeConfigSavePhase},
};
use dedup_node_engine::server::{NodeRequestHandler, NodeServer};
use dedup_protocol::proto;
use tempfile::TempDir;
use tokio::sync::oneshot;

#[derive(Clone)]
struct ConfigHandler {
    state: Arc<Mutex<HandlerState>>,
    post_save_status_delay: Duration,
}

struct HandlerState {
    initial_machine_id: String,
    reconnect_machine_id: String,
    initial_version: String,
    verified_version: String,
    config: proto::NodeConfigValue,
    saved: bool,
    saves: Vec<proto::SaveNodeConfigAndRestart>,
    post_save_status_calls: usize,
    post_save_sync_side_effects: usize,
    delay_next_post_save_status: bool,
}

impl ConfigHandler {
    fn new(machine: &str, reconnect_machine: &str, verified_version: &str) -> Self {
        Self {
            state: Arc::new(Mutex::new(HandlerState {
                initial_machine_id: machine.into(),
                reconnect_machine_id: reconnect_machine.into(),
                initial_version: "old-sha".into(),
                verified_version: verified_version.into(),
                config: config_value(39100),
                saved: false,
                saves: Vec::new(),
                post_save_status_calls: 0,
                post_save_sync_side_effects: 0,
                delay_next_post_save_status: false,
            })),
            post_save_status_delay: Duration::ZERO,
        }
    }

    fn with_reconnect_delay(machine: &str, delay: Duration) -> Self {
        let mut handler = Self::new(machine, machine, "saved-sha");
        handler.post_save_status_delay = delay;
        handler.state.lock().unwrap().delay_next_post_save_status = true;
        handler
    }
}

impl NodeRequestHandler for ConfigHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        let delay_status = if matches!(
            &request.payload,
            Some(proto::envelope::Payload::NodeStatus(_))
        ) {
            let mut state = self.state.lock().unwrap();
            let delay = state.saved && state.delay_next_post_save_status;
            if delay {
                state.delay_next_post_save_status = false;
            }
            delay
        } else {
            false
        };
        if delay_status {
            tokio::time::sleep(self.post_save_status_delay).await;
        }
        let mut state = self.state.lock().unwrap();
        let payload = match request.payload {
            Some(proto::envelope::Payload::NodeStatus(_)) => {
                if state.saved {
                    state.post_save_status_calls += 1;
                }
                proto::envelope::Payload::NodeStatus(proto::NodeStatus {
                    machine_id: if state.saved {
                        state.reconnect_machine_id.clone()
                    } else {
                        state.initial_machine_id.clone()
                    },
                    listen_address: "127.0.0.1:0".into(),
                    ..Default::default()
                })
            }
            Some(proto::envelope::Payload::ListTasks(mut page)) => {
                if state.saved {
                    state.post_save_sync_side_effects += 1;
                }
                page.tasks.clear();
                page.next_cursor.clear();
                proto::envelope::Payload::ListTasks(page)
            }
            Some(proto::envelope::Payload::GetNodeConfig(_)) => {
                proto::envelope::Payload::NodeConfigSnapshot(proto::NodeConfigSnapshot {
                    machine_id: if state.saved {
                        state.reconnect_machine_id.clone()
                    } else {
                        state.initial_machine_id.clone()
                    },
                    version_sha256: if state.saved {
                        state.verified_version.clone()
                    } else {
                        state.initial_version.clone()
                    },
                    config: Some(state.config.clone()),
                    logical_cpu_count: 8,
                    effective_worker_count: 7,
                })
            }
            Some(proto::envelope::Payload::SaveNodeConfigAndRestart(save)) => {
                let accepted_machine = state.initial_machine_id.clone();
                state.config = save.config.clone().unwrap();
                state.saves.push(save);
                state.saved = true;
                proto::envelope::Payload::NodeRestartAccepted(proto::NodeRestartAccepted {
                    machine_id: accepted_machine,
                    saved_version_sha256: "saved-sha".into(),
                })
            }
            Some(proto::envelope::Payload::SyncAck(ack)) => {
                if state.saved {
                    state.post_save_sync_side_effects += 1;
                }
                proto::envelope::Payload::SyncAck(ack)
            }
            Some(proto::envelope::Payload::PullChanges(_)) => {
                if state.saved {
                    state.post_save_sync_side_effects += 1;
                }
                proto::envelope::Payload::SyncChangeBatch(proto::SyncChangeBatch::default())
            }
            _ => proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::InvalidRequest as i32,
                message: "fixture only accepts config lifecycle requests".into(),
            }),
        };
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(payload),
        }
    }
}

#[tokio::test]
async fn save_freezes_machine_and_endpoint_then_verifies_reconnected_version() {
    let machine = "b1".repeat(32);
    let handler = ConfigHandler::new(&machine, &machine, "saved-sha");
    let handler_state = Arc::clone(&handler.state);
    let (address, shutdown, server) = start_server(handler).await;
    let temp = TempDir::new().unwrap();
    let (app, mut events) = DesktopApp::start(config(address, 1), desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;

    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    let loaded = wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;
    assert_eq!(loaded.snapshot().unwrap().machine_id, machine);
    assert_eq!(loaded.snapshot().unwrap().version_sha256, "old-sha");

    let changed = config_value(39200);
    app.send(UiCommand::SaveNodeConfigAndRestart {
        node_index: 0,
        config: changed.clone(),
    })
    .await
    .unwrap();
    let phases = collect_until_terminal(&mut events).await;
    assert_eq!(
        phases,
        [
            NodeConfigSavePhase::Validating,
            NodeConfigSavePhase::Saving,
            NodeConfigSavePhase::Restarting,
            NodeConfigSavePhase::WaitingForReconnect,
            NodeConfigSavePhase::Verifying,
            NodeConfigSavePhase::Completed,
        ]
    );
    let saves = &handler_state.lock().unwrap().saves;
    assert_eq!(saves.len(), 1);
    assert_eq!(saves[0].expected_version_sha256, "old-sha");
    assert_eq!(saves[0].config, Some(changed));

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn reconnect_with_a_different_machine_id_fails_even_at_the_same_index() {
    let initial_machine = "b2".repeat(32);
    let other_machine = "c2".repeat(32);
    let handler = ConfigHandler::new(&initial_machine, &other_machine, "saved-sha");
    let handler_state = Arc::clone(&handler.state);
    let (address, shutdown, server) = start_server(handler).await;
    let temp = TempDir::new().unwrap();
    let (app, mut events) = DesktopApp::start(config(address, 1), desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;
    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;
    app.send(UiCommand::SaveNodeConfigAndRestart {
        node_index: 0,
        config: config_value(39201),
    })
    .await
    .unwrap();

    let failed = wait_for_config_state(&mut events, |state| {
        state.phase() == NodeConfigSavePhase::Failed
    })
    .await;
    assert!(failed.error().unwrap().contains(&initial_machine));
    assert!(failed.error().unwrap().contains(&other_machine));
    let handler_state = handler_state.lock().unwrap();
    assert_eq!(handler_state.post_save_status_calls, 1);
    assert_eq!(handler_state.post_save_sync_side_effects, 0);
    drop(handler_state);

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn reconnect_with_the_same_machine_but_wrong_saved_sha_fails_verification() {
    let machine = "b5".repeat(32);
    let handler = ConfigHandler::new(&machine, &machine, "different-sha");
    let (address, shutdown, server) = start_server(handler).await;
    let temp = TempDir::new().unwrap();
    let (app, mut events) = DesktopApp::start(config(address, 1), desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;
    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;
    app.send(UiCommand::SaveNodeConfigAndRestart {
        node_index: 0,
        config: config_value(39203),
    })
    .await
    .unwrap();

    let failed = wait_for_config_state(&mut events, |state| {
        state.phase() == NodeConfigSavePhase::Failed
    })
    .await;
    assert!(failed.error().unwrap().contains("saved-sha"));
    assert!(failed.error().unwrap().contains("different-sha"));

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn offline_load_is_rejected_and_switching_selection_clears_the_snapshot() {
    let machine = "b3".repeat(32);
    let handler = ConfigHandler::new(&machine, &machine, "saved-sha");
    let (address, shutdown, server) = start_server(handler).await;
    let unused = unused_endpoint().await;
    let temp = TempDir::new().unwrap();
    let desktop_config = DesktopConfig {
        nodes: vec![
            NodeEndpoint {
                ip: address.ip(),
                port: address.port(),
            },
            unused,
        ],
        reconnect_interval_seconds: 1,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(desktop_config, desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;
    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;

    app.send(UiCommand::LoadNodeConfig { node_index: 1 })
        .await
        .unwrap();
    let cleared = wait_for_config_state(&mut events, |state| {
        state.selected_node_index() == Some(1) && state.snapshot().is_none()
    })
    .await;
    assert_eq!(cleared.phase(), NodeConfigSavePhase::Idle);
    let error = wait_for_error(&mut events).await;
    assert!(error.contains("未连接"));

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn accepted_save_without_reconnect_ends_in_explicit_timeout_failure() {
    let machine = "b4".repeat(32);
    let handler = ConfigHandler::new(&machine, &machine, "saved-sha");
    let (address, shutdown, server) = start_server(handler).await;
    let temp = TempDir::new().unwrap();
    let (app, mut events) = DesktopApp::start(config(address, 1), desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;
    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;
    app.send(UiCommand::SaveNodeConfigAndRestart {
        node_index: 0,
        config: config_value(39202),
    })
    .await
    .unwrap();
    wait_for_config_state(&mut events, |state| {
        state.phase() == NodeConfigSavePhase::WaitingForReconnect
    })
    .await;
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();

    let failed = wait_for_config_state(&mut events, |state| {
        state.phase() == NodeConfigSavePhase::Failed
    })
    .await;
    assert!(failed.error().unwrap().contains("重连超时"));
    assert!(failed.error().unwrap().contains(&machine));
    app.send(UiCommand::Shutdown).await.unwrap();
}

#[tokio::test]
async fn pending_save_rejects_load_edit_and_remove_without_losing_target() {
    let machine = "b6".repeat(32);
    let handler = ConfigHandler::new(&machine, &machine, "saved-sha");
    let (address, shutdown, server) = start_server(handler).await;
    let unused = unused_endpoint().await;
    let temp = TempDir::new().unwrap();
    let desktop_config = DesktopConfig {
        nodes: vec![
            NodeEndpoint {
                ip: address.ip(),
                port: address.port(),
            },
            unused,
        ],
        reconnect_interval_seconds: 10,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(desktop_config, desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;
    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;
    app.send(UiCommand::SaveNodeConfigAndRestart {
        node_index: 0,
        config: config_value(39204),
    })
    .await
    .unwrap();
    let waiting = wait_for_config_state(&mut events, |state| {
        state.phase() == NodeConfigSavePhase::WaitingForReconnect
    })
    .await;
    let target_machine = waiting.target_machine_id().unwrap().to_owned();
    let target_endpoint = waiting.target_endpoint().unwrap().clone();

    app.send(UiCommand::LoadNodeConfig { node_index: 1 })
        .await
        .unwrap();
    assert!(wait_for_error(&mut events).await.contains("重启验证进行中"));
    assert_pending_target_unchanged(&mut events, &target_machine, &target_endpoint).await;

    app.send(UiCommand::EditNode {
        index: 0,
        ip: "127.0.0.2".into(),
        port: address.port(),
    })
    .await
    .unwrap();
    assert!(wait_for_error(&mut events).await.contains("重启验证进行中"));
    assert_pending_target_unchanged(&mut events, &target_machine, &target_endpoint).await;

    app.send(UiCommand::RemoveNode { index: 0 }).await.unwrap();
    assert!(wait_for_error(&mut events).await.contains("重启验证进行中"));
    assert_pending_target_unchanged(&mut events, &target_machine, &target_endpoint).await;

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn candidate_that_appears_after_deadline_remains_failed() {
    let machine = "b7".repeat(32);
    let handler = ConfigHandler::with_reconnect_delay(&machine, Duration::from_millis(3_200));
    let (address, shutdown, server) = start_server(handler).await;
    let temp = TempDir::new().unwrap();
    let (app, mut events) = DesktopApp::start(config(address, 1), desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;
    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;
    app.send(UiCommand::SaveNodeConfigAndRestart {
        node_index: 0,
        config: config_value(39205),
    })
    .await
    .unwrap();
    wait_for_config_state(&mut events, |state| {
        state.phase() == NodeConfigSavePhase::WaitingForReconnect
    })
    .await;
    let terminal = wait_for_config_state(&mut events, |state| {
        matches!(
            state.phase(),
            NodeConfigSavePhase::Completed | NodeConfigSavePhase::Failed
        )
    })
    .await;
    assert_eq!(terminal.phase(), NodeConfigSavePhase::Failed);
    assert!(terminal.error().unwrap().contains("重连超时"));

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();
}

async fn start_server(
    handler: ConfigHandler,
) -> (
    std::net::SocketAddr,
    oneshot::Sender<()>,
    tokio::task::JoinHandle<Result<(), dedup_node_engine::server::ServerError>>,
) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown, receiver) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(listener, handler, receiver));
    (address, shutdown, server)
}

async fn wait_until_online(events: &mut tokio::sync::mpsc::Receiver<UiEvent>, index: usize) {
    tokio::time::timeout(Duration::from_secs(4), async {
        loop {
            if let Some(UiEvent::ViewChanged(state)) = events.recv().await
                && state.nodes()[index].connection
                    == dedup_desktop_core::view_state::NodeConnectionState::Online
            {
                break;
            }
        }
    })
    .await
    .unwrap();
}

async fn wait_for_config_state(
    events: &mut tokio::sync::mpsc::Receiver<UiEvent>,
    predicate: impl Fn(&NodeConfigControllerState) -> bool,
) -> NodeConfigControllerState {
    tokio::time::timeout(Duration::from_secs(6), async {
        loop {
            if let Some(UiEvent::NodeConfigChanged(state)) = events.recv().await
                && predicate(&state)
            {
                break state;
            }
        }
    })
    .await
    .unwrap()
}

async fn collect_until_terminal(
    events: &mut tokio::sync::mpsc::Receiver<UiEvent>,
) -> Vec<NodeConfigSavePhase> {
    tokio::time::timeout(Duration::from_secs(7), async {
        let mut phases = Vec::new();
        loop {
            if let Some(UiEvent::NodeConfigChanged(state)) = events.recv().await {
                phases.push(state.phase());
                if matches!(
                    state.phase(),
                    NodeConfigSavePhase::Completed | NodeConfigSavePhase::Failed
                ) {
                    break phases;
                }
            }
        }
    })
    .await
    .unwrap()
}

async fn wait_for_error(events: &mut tokio::sync::mpsc::Receiver<UiEvent>) -> String {
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            if let Some(UiEvent::Error(error)) = events.recv().await {
                break error;
            }
        }
    })
    .await
    .unwrap()
}

async fn assert_pending_target_unchanged(
    events: &mut tokio::sync::mpsc::Receiver<UiEvent>,
    machine_id: &str,
    endpoint: &NodeEndpoint,
) {
    let state = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            if let Some(UiEvent::ViewChanged(state)) = events.recv().await
                && state.node_config().phase() == NodeConfigSavePhase::WaitingForReconnect
            {
                break state;
            }
        }
    })
    .await
    .unwrap();
    assert_eq!(state.node_config().target_machine_id(), Some(machine_id));
    assert_eq!(state.node_config().target_endpoint(), Some(endpoint));
}

fn config(address: std::net::SocketAddr, reconnect_seconds: u64) -> DesktopConfig {
    DesktopConfig {
        nodes: vec![NodeEndpoint {
            ip: address.ip(),
            port: address.port(),
        }],
        reconnect_interval_seconds: reconnect_seconds,
        ..DesktopConfig::default()
    }
}

fn config_value(port: u32) -> proto::NodeConfigValue {
    proto::NodeConfigValue {
        listen_ip: "127.0.0.1".into(),
        port,
        enumerator: proto::NodeEnumerator::NodeWindowsWalker as i32,
        data_path: "data/node".into(),
        config_path: "data/node/config.toml".into(),
        log_path: "data/node/logs".into(),
        cache_path: "data/node/cache".into(),
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 2,
        unknown_threads_per_disk: 1,
        total_threads: 4,
        block_size_bytes: 4 * 1024 * 1024,
        block_timeout_seconds: 3,
        block_retries: 2,
        legacy_worker_count: 4,
        worker_mode: proto::NodeWorkerMode::NodeWorkerAutomatic as i32,
        reserved_cores: 1,
        manual_worker_count: 1,
    }
}

async fn unused_endpoint() -> NodeEndpoint {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    drop(listener);
    NodeEndpoint {
        ip: IpAddr::from([127, 0, 0, 1]),
        port: address.port(),
    }
}

fn desktop_paths(temp: &TempDir) -> DesktopPaths {
    DesktopPaths {
        data: temp.path().to_path_buf(),
        logs: temp.path().join("logs"),
        cache: temp.path().join("cache"),
        config: temp.path().join("config.toml"),
    }
}
