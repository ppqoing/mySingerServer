//! Desktop NodeSession 与真实 Node 配置仓库的保存边界。

use std::{
    fs,
    net::SocketAddr,
    path::{Path, PathBuf},
};

use dedup_core::{MachineId, NodeConfig, NodeEndpoint};
use dedup_desktop_core::node_session::NodeSession;
use dedup_node_engine::{
    actor::{NodeEngine, NodeEngineHandle},
    config_repository::NodeConfigRepository,
    server::{NodeServer, ServerError},
};
use dedup_node_store::NodeStore;
use dedup_protocol::proto;
use dedup_windows::AppLayout;
use tempfile::TempDir;
use tokio::{sync::oneshot, task::JoinHandle};

#[tokio::test]
async fn relative_paths_save_in_place_without_bootstrap_change() {
    run_save_case(PathCase::Relative).await;
}

#[tokio::test]
async fn absolute_paths_save_in_place_without_bootstrap_change() {
    run_save_case(PathCase::Absolute).await;
}

#[derive(Clone, Copy, Debug)]
enum PathCase {
    Relative,
    Absolute,
}

async fn run_save_case(path_case: PathCase) {
    let directory = TempDir::new().unwrap();
    let executable = directory.path().join("portable/node.exe");
    fs::create_dir_all(executable.parent().unwrap()).unwrap();
    let layout = AppLayout::from_executable(&executable).unwrap();
    let initial = initial_config(path_case, directory.path());
    initialize_repository(&layout, &initial);
    let bootstrap_path = layout.node_bootstrap();
    let bootstrap_bytes = fs::read(&bootstrap_path).unwrap();
    let repository = NodeConfigRepository::from_layout(&layout);
    let initial_snapshot = repository.snapshot().unwrap();
    let machine_id = MachineId::parse(&"d8".repeat(32)).unwrap();

    let node = RunningNode::start(&layout, machine_id.clone()).await;
    let endpoint = endpoint(node.address);
    let session = NodeSession::connect(endpoint).await.unwrap();
    assert_eq!(session.machine_id(), &machine_id);
    let loaded = session.get_node_config().await.unwrap();
    assert_eq!(loaded.version_sha256, initial_snapshot.version_sha256);

    let changed = changed_config(path_case, directory.path());
    let wire = proto::NodeConfigValue::try_from(&changed).unwrap();
    let saved = session
        .save_node_config(&loaded.version_sha256, wire.clone())
        .await
        .unwrap();
    assert_eq!(saved.machine_id, machine_id.as_str());
    assert_ne!(saved.saved_version_sha256, loaded.version_sha256);
    assert_eq!(fs::read(&bootstrap_path).unwrap(), bootstrap_bytes);

    let repository = NodeConfigRepository::from_layout(&layout);
    let snapshot = repository.snapshot().unwrap();
    assert_eq!(snapshot.version_sha256, saved.saved_version_sha256);
    assert_eq!(snapshot.config, changed);
    assert!(snapshot.resolved.config_path.exists());
    assert_eq!(
        proto::NodeConfigValue::try_from(&snapshot.config).unwrap(),
        wire
    );

    let verified = session.get_node_config().await.unwrap();
    assert_eq!(verified.machine_id, machine_id.as_str());
    assert_eq!(verified.version_sha256, saved.saved_version_sha256);
    assert_eq!(verified.config, Some(wire));

    println!(
        "NODE_CONFIG_E2E_PASS case={path_case:?} machine={} old_sha={} saved_sha={} restart=false",
        machine_id.as_str(),
        loaded.version_sha256,
        verified.version_sha256
    );

    drop(session);
    node.shutdown().await;
}

struct RunningNode {
    address: SocketAddr,
    handle: NodeEngineHandle,
    actor: JoinHandle<()>,
    server_shutdown: oneshot::Sender<()>,
    server: JoinHandle<Result<(), ServerError>>,
}

impl RunningNode {
    /// 启动使用真实配置仓库的测试 Node，不创建重启替代进程。
    async fn start(layout: &AppLayout, machine_id: MachineId) -> Self {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let repository = NodeConfigRepository::from_layout(layout);
        let loaded = repository.snapshot().unwrap();
        let store = NodeStore::open_in_memory(machine_id).unwrap();
        let (handle, actor) = NodeEngine::spawn_with_config_repository_for_test(
            store,
            address,
            &loaded.resolved.cache_path,
            Box::new(repository),
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

    /// 关闭测试 Node 的 TCP 服务和唯一 actor。
    async fn shutdown(self) {
        let _ = self.server_shutdown.send(());
        self.server.await.unwrap().unwrap();
        self.handle.shutdown().await.unwrap();
        self.actor.await.unwrap();
    }
}

fn initialize_repository(layout: &AppLayout, config: &NodeConfig) {
    let config_path = if Path::new(&config.paths.config_path).is_absolute() {
        PathBuf::from(&config.paths.config_path)
    } else {
        layout.executable_dir().join(&config.paths.config_path)
    };
    fs::create_dir_all(config_path.parent().unwrap()).unwrap();
    fs::write(&config_path, config.to_toml().unwrap()).unwrap();

    let mut bootstrap = toml::map::Map::new();
    bootstrap.insert(
        "config_path".into(),
        toml::Value::String(config.paths.config_path.clone()),
    );
    fs::write(
        layout.node_bootstrap(),
        toml::to_string(&toml::Value::Table(bootstrap)).unwrap(),
    )
    .unwrap();
}

fn initial_config(path_case: PathCase, root: &Path) -> NodeConfig {
    let mut config = NodeConfig::default();
    match path_case {
        PathCase::Relative => {
            config.paths.data_path = "data/node".into();
            config.paths.config_path = "data/node/config.toml".into();
            config.paths.log_path = "data/node/logs".into();
            config.paths.cache_path = "data/node/cache".into();
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

fn changed_config(path_case: PathCase, root: &Path) -> NodeConfig {
    let mut config = initial_config(path_case, root);
    config.port = 39092;
    config.paths.data_path = match path_case {
        PathCase::Relative => "relative/data".into(),
        PathCase::Absolute => absolute(root.join("changed/data")),
    };
    config.paths.log_path = match path_case {
        PathCase::Relative => "relative/logs".into(),
        PathCase::Absolute => absolute(root.join("changed/logs")),
    };
    config.paths.cache_path = match path_case {
        PathCase::Relative => "relative/cache".into(),
        PathCase::Absolute => absolute(root.join("changed/cache")),
    };
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
