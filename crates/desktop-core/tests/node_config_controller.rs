//! Desktop 远程 Node 配置加载与单文件保存行为。

use std::{
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
}

struct HandlerState {
    machine_id: String,
    response_machine_id: String,
    response_version: String,
    config: proto::NodeConfigValue,
    saved: bool,
    save_error: Option<String>,
    saves: Vec<proto::SaveNodeConfig>,
    post_save_status_calls: usize,
    post_save_sync_side_effects: usize,
}

impl ConfigHandler {
    /// 创建一个可记录配置请求的节点协议夹具。
    fn new(machine_id: &str) -> Self {
        Self {
            state: Arc::new(Mutex::new(HandlerState {
                machine_id: machine_id.into(),
                response_machine_id: machine_id.into(),
                response_version: "saved-sha".into(),
                config: config_value(39100),
                saved: false,
                save_error: None,
                saves: Vec::new(),
                post_save_status_calls: 0,
                post_save_sync_side_effects: 0,
            })),
        }
    }

    /// 让保存响应返回错误，验证桌面只进入失败态而不伪造完成。
    fn with_save_error(machine_id: &str, message: &str) -> Self {
        let handler = Self::new(machine_id);
        handler.state.lock().unwrap().save_error = Some(message.into());
        handler
    }

    /// 修改保存响应身份，验证桌面拒绝不属于当前会话的响应。
    fn with_response_machine(machine_id: &str, response_machine_id: &str) -> Self {
        let handler = Self::new(machine_id);
        handler.state.lock().unwrap().response_machine_id = response_machine_id.into();
        handler
    }
}

impl NodeRequestHandler for ConfigHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        let mut state = self.state.lock().unwrap();
        let payload = match request.payload {
            Some(proto::envelope::Payload::NodeStatus(_)) => {
                if state.saved {
                    state.post_save_status_calls += 1;
                }
                proto::envelope::Payload::NodeStatus(proto::NodeStatus {
                    machine_id: state.machine_id.clone(),
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
                    machine_id: state.machine_id.clone(),
                    version_sha256: if state.saved {
                        state.response_version.clone()
                    } else {
                        "old-sha".into()
                    },
                    config: Some(state.config.clone()),
                    logical_cpu_count: 8,
                    effective_worker_count: 7,
                })
            }
            Some(proto::envelope::Payload::SaveNodeConfig(save)) => {
                state.saves.push(save.clone());
                if let Some(message) = state.save_error.clone() {
                    proto::envelope::Payload::Error(proto::Error {
                        code: proto::ErrorCode::Conflict as i32,
                        message,
                    })
                } else {
                    state.config = save.config.clone().unwrap();
                    state.saved = true;
                    proto::envelope::Payload::NodeConfigSaved(proto::NodeConfigSaved {
                        machine_id: state.response_machine_id.clone(),
                        saved_version_sha256: state.response_version.clone(),
                    })
                }
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
async fn save_completes_without_reconnect_or_restart_side_effects() {
    let machine = "b1".repeat(32);
    let handler = ConfigHandler::new(&machine);
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
    app.send(UiCommand::SaveNodeConfig {
        node_index: 0,
        config: changed.clone(),
    })
    .await
    .unwrap();
    assert_eq!(
        collect_until_terminal(&mut events).await,
        [NodeConfigSavePhase::Saving, NodeConfigSavePhase::Completed]
    );

    let state = handler_state.lock().unwrap();
    assert_eq!(state.saves.len(), 1);
    assert_eq!(state.saves[0].expected_version_sha256, "old-sha");
    assert_eq!(state.saves[0].config, Some(changed));
    assert_eq!(state.post_save_status_calls, 0);
    assert_eq!(state.post_save_sync_side_effects, 0);
    drop(state);

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn save_error_is_reported_without_completed_state() {
    let machine = "b2".repeat(32);
    let handler = ConfigHandler::with_save_error(&machine, "版本冲突");
    let (address, shutdown, server) = start_server(handler).await;
    let temp = TempDir::new().unwrap();
    let (app, mut events) = DesktopApp::start(config(address, 1), desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;
    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;

    app.send(UiCommand::SaveNodeConfig {
        node_index: 0,
        config: config_value(39201),
    })
    .await
    .unwrap();
    let failed = wait_for_config_state(&mut events, |state| {
        state.phase() == NodeConfigSavePhase::Failed
    })
    .await;
    assert_eq!(failed.error(), Some("节点协议错误 4: 版本冲突"));

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown.send(()).unwrap();
    server.await.unwrap().unwrap();
}

#[tokio::test]
async fn mismatched_save_response_is_rejected() {
    let machine = "b3".repeat(32);
    let other_machine = "c3".repeat(32);
    let handler = ConfigHandler::with_response_machine(&machine, &other_machine);
    let (address, shutdown, server) = start_server(handler).await;
    let temp = TempDir::new().unwrap();
    let (app, mut events) = DesktopApp::start(config(address, 1), desktop_paths(&temp));
    wait_until_online(&mut events, 0).await;
    app.send(UiCommand::LoadNodeConfig { node_index: 0 })
        .await
        .unwrap();
    wait_for_config_state(&mut events, |state| state.snapshot().is_some()).await;

    app.send(UiCommand::SaveNodeConfig {
        node_index: 0,
        config: config_value(39202),
    })
    .await
    .unwrap();
    let failed = wait_for_config_state(&mut events, |state| {
        state.phase() == NodeConfigSavePhase::Failed
    })
    .await;
    assert!(failed.error().unwrap().contains("机器不匹配"));
    assert!(failed.error().unwrap().contains(&other_machine));

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
        postgres: Some(proto::NodePostgresConfigValue::default()),
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
