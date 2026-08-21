//! Desktop NodeSession 到真实 Node repository 的配置保存、重启与重连门禁。

use std::{
    fs,
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_core::{MachineId, NodeConfig, NodeEndpoint};
use dedup_desktop_core::node_session::NodeSession;
use dedup_node_engine::{
    actor::{NodeEngine, NodeEngineHandle},
    config_repository::NodeConfigRepository,
    host_control::{NodeHostControl, NodeHostControlError},
    server::{NodeServer, ServerError},
};
use dedup_node_store::NodeStore;
use dedup_protocol::proto;
use dedup_windows::AppLayout;
use tempfile::TempDir;
use tokio::{sync::oneshot, task::JoinHandle};

#[tokio::test]
async fn relative_paths_round_trip_through_restart_and_reconnect() {
    run_restart_case(PathCase::Relative).await;
}

#[tokio::test]
async fn absolute_local_paths_round_trip_through_restart_and_reconnect() {
    run_restart_case(PathCase::Absolute).await;
}

#[derive(Clone, Copy, Debug)]
enum PathCase {
    Relative,
    Absolute,
}

async fn run_restart_case(path_case: PathCase) {
    let directory = TempDir::new().unwrap();
    let executable = directory.path().join("portable/node.exe");
    fs::create_dir_all(executable.parent().unwrap()).unwrap();
    let layout = AppLayout::from_executable(&executable).unwrap();
    let initial = initial_config();
    initialize_repository(&layout, &initial);
    let old_config_path = layout.node_config();
    let old_config_text = fs::read_to_string(&old_config_path).unwrap();
    let initial_snapshot = NodeConfigRepository::from_layout(&layout)
        .snapshot()
        .unwrap();
    let machine_id = MachineId::parse(&"d8".repeat(32)).unwrap();
    let host = RecordingHost::default();

    let old_node = RunningNode::start(
        &layout,
        machine_id.clone(),
        Some(Box::new(host.clone())),
        None,
    )
    .await;
    let node_address = old_node.address;
    let endpoint = endpoint(old_node.address);
    let session = NodeSession::connect(endpoint.clone()).await.unwrap();
    assert_eq!(session.machine_id(), &machine_id);
    let loaded = session.get_node_config().await.unwrap();
    assert_eq!(loaded.version_sha256, initial_snapshot.version_sha256);

    let changed = changed_config(path_case, directory.path());
    let wire = proto::NodeConfigValue::try_from(&changed).unwrap();
    let accepted = session
        .save_node_config_and_restart(&loaded.version_sha256, wire.clone())
        .await
        .unwrap();
    wait_for_host_commit(&host).await;
    assert_eq!(accepted.machine_id, machine_id.as_str());
    assert_ne!(accepted.saved_version_sha256, loaded.version_sha256);
    let host_state = host.state.lock().unwrap();
    assert_eq!(host_state.prepare_calls, 1);
    assert_eq!(host_state.commit_calls, 1);
    assert_eq!(
        host_state.prepared_versions,
        [accepted.saved_version_sha256.clone()]
    );
    drop(host_state);

    let bootstrap_text = fs::read_to_string(layout.node_bootstrap()).unwrap();
    let bootstrap: toml::Value = toml::from_str(&bootstrap_text).unwrap();
    assert_eq!(
        bootstrap.get("config_path").and_then(toml::Value::as_str),
        Some(changed.paths.config_path.as_str())
    );
    assert!(old_config_path.exists());
    assert_eq!(
        fs::read_to_string(&old_config_path).unwrap(),
        old_config_text
    );
    let repository = NodeConfigRepository::from_layout(&layout);
    let saved = repository.snapshot().unwrap();
    assert_eq!(saved.version_sha256, accepted.saved_version_sha256);
    assert_eq!(saved.config, changed);
    assert!(saved.resolved.config_path.exists());
    assert_eq!(
        proto::NodeConfigValue::try_from(&saved.config).unwrap(),
        wire
    );

    drop(session);
    old_node.shutdown().await;

    let new_node = RunningNode::start(&layout, machine_id.clone(), None, Some(node_address)).await;
    let reconnected = NodeSession::connect(endpoint).await.unwrap();
    assert_eq!(reconnected.machine_id(), &machine_id);
    let verified = reconnected.get_node_config().await.unwrap();
    assert_eq!(verified.machine_id, machine_id.as_str());
    assert_eq!(verified.version_sha256, accepted.saved_version_sha256);
    assert_eq!(verified.config, Some(wire));

    println!(
        "NODE_CONFIG_E2E_PASS case={path_case:?} machine={} old_sha={} saved_sha={} reconnected=true",
        machine_id.as_str(),
        loaded.version_sha256,
        verified.version_sha256
    );

    drop(reconnected);
    new_node.shutdown().await;
}

#[derive(Clone, Default)]
struct RecordingHost {
    state: Arc<Mutex<RecordingHostState>>,
}

#[derive(Default)]
struct RecordingHostState {
    prepare_calls: usize,
    commit_calls: usize,
    prepared_versions: Vec<String>,
    committed: bool,
}

impl NodeHostControl for RecordingHost {
    fn prepare_replacement(&self, saved_version: &str) -> Result<(), NodeHostControlError> {
        let mut state = self.state.lock().unwrap();
        state.prepare_calls += 1;
        state.prepared_versions.push(saved_version.into());
        Ok(())
    }

    fn commit_exit_after_response(&self) -> Result<(), NodeHostControlError> {
        let mut state = self.state.lock().unwrap();
        if !state.committed {
            state.committed = true;
            state.commit_calls += 1;
        }
        Ok(())
    }
}

struct RunningNode {
    address: SocketAddr,
    handle: NodeEngineHandle,
    actor: JoinHandle<()>,
    server_shutdown: oneshot::Sender<()>,
    server: JoinHandle<Result<(), ServerError>>,
}

impl RunningNode {
    async fn start(
        layout: &AppLayout,
        machine_id: MachineId,
        host: Option<Box<dyn NodeHostControl>>,
        address: Option<SocketAddr>,
    ) -> Self {
        let listener = tokio::net::TcpListener::bind(
            address.unwrap_or_else(|| "127.0.0.1:0".parse().unwrap()),
        )
        .await
        .unwrap();
        let address = listener.local_addr().unwrap();
        let repository = NodeConfigRepository::from_layout(layout);
        let loaded = repository.snapshot().unwrap();
        let store = NodeStore::open_in_memory(machine_id).unwrap();
        let (handle, actor) = NodeEngine::spawn_with_remote_config_for_test(
            store,
            address,
            &loaded.resolved.cache_path,
            Box::new(repository),
            host,
        );
        let (server_shutdown, shutdown) = oneshot::channel();
        let server = tokio::spawn(NodeServer::serve_until(listener, handle.clone(), shutdown));
        Self {
            address,
            handle,
            actor,
            server_shutdown,
            server,
        }
    }

    async fn shutdown(self) {
        let _ = self.server_shutdown.send(());
        self.server.await.unwrap().unwrap();
        self.handle.shutdown().await.unwrap();
        self.actor.await.unwrap();
    }
}

async fn wait_for_host_commit(host: &RecordingHost) {
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            if host.state.lock().unwrap().committed {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
}

fn initialize_repository(layout: &AppLayout, config: &NodeConfig) {
    fs::create_dir_all(layout.node_root()).unwrap();
    fs::write(layout.node_config(), config.to_toml().unwrap()).unwrap();
    fs::write(
        layout.node_bootstrap(),
        "config_path = \"data/node/config.toml\"\n",
    )
    .unwrap();
}

fn initial_config() -> NodeConfig {
    let mut config = NodeConfig::default();
    config.paths.data_path = "data/node".into();
    config.paths.config_path = "data/node/config.toml".into();
    config.paths.log_path = "data/node/logs".into();
    config.paths.cache_path = "data/node/cache".into();
    config
}

fn changed_config(path_case: PathCase, root: &Path) -> NodeConfig {
    let mut config = initial_config();
    config.port = 39092;
    match path_case {
        PathCase::Relative => {
            config.paths.data_path = "relative/data".into();
            config.paths.config_path = "relative/config/node.toml".into();
            config.paths.log_path = "relative/logs".into();
            config.paths.cache_path = "relative/cache".into();
        }
        PathCase::Absolute => {
            config.paths.data_path = absolute(root.join("absolute/data"));
            config.paths.config_path = absolute(root.join("absolute/config/node.toml"));
            config.paths.log_path = absolute(root.join("absolute/logs"));
            config.paths.cache_path = absolute(root.join("absolute/cache"));
        }
    }
    config
}

fn absolute(path: PathBuf) -> String {
    path.as_os_str().to_string_lossy().into_owned()
}

fn endpoint(address: SocketAddr) -> NodeEndpoint {
    NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    }
}
