//! Node 扫描流水线到 Desktop 运行详情控制器的真实 TCP 集成门禁。

use std::{
    fs,
    net::SocketAddr,
    path::Path,
    time::{Duration, Instant},
};

use dedup_core::{DesktopConfig, EnumeratorKind, MachineId, MediaKind, NodeEndpoint};
use dedup_desktop_core::{
    app::{DesktopApp, UiCommand, UiEvent},
    runtime_tasks::DesktopRuntimeTaskState,
    view_state::{DesktopPaths, NodeConnectionState, RuntimeTaskControllerState},
};
use dedup_node_engine::{
    actor::{NodeEngine, NodeEngineHandle},
    runtime_tasks::RuntimeTaskRegistry,
    server::{NodeServer, ServerError},
    worker::{Stage1Output, WorkerPool},
};
use dedup_node_store::{NewTaskItem, NodeStore};
use tempfile::TempDir;
use tokio::{
    sync::{mpsc, oneshot},
    task::JoinHandle,
};

/// 两个真实可控 Worker、真实 TCP 和固定两秒 Desktop tick 必须形成一致运行详情。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn node_pipeline_reaches_desktop_details_and_restart_forgets_transient_tasks() {
    let fixture = TempDir::new().unwrap();
    let database = fixture.path().join("node.db");
    let cache = fixture.path().join("cache");
    let media = fixture.path().join("media");
    fs::create_dir_all(&media).unwrap();
    for (name, body) in [
        ("alpha.jpg", b"alpha".as_slice()),
        ("beta.mp4", b"beta".as_slice()),
    ] {
        fs::write(media.join(name), body).unwrap();
    }

    let machine_id = MachineId::from_sha256([0xd1; 32]);
    let old_task_id = seed_recovery_task(&database, machine_id.clone());
    let (first_workers, mut started, worker_control) = WorkerPool::controlled_batch_for_test(2);
    let (mut first_node, first_registry) =
        RunningNode::start(&database, &cache, machine_id.clone(), first_workers, None).await;
    assert!(
        first_registry.list().await.is_empty(),
        "启动清理后的首次运行任务列表必须为空，不能发布旧 SQLite 任务"
    );
    let observer = rusqlite::Connection::open(&database).unwrap();
    let remaining_tasks: i64 = observer
        .query_row(
            "SELECT COUNT(*) FROM tasks WHERE task_id=?1",
            [&old_task_id],
            |row| row.get(0),
        )
        .unwrap();
    let remaining_items: i64 = observer
        .query_row(
            "SELECT COUNT(*) FROM task_items WHERE task_id=?1",
            [&old_task_id],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(remaining_tasks, 0, "启动事务必须清除旧 running task 行");
    assert_eq!(
        remaining_items, 0,
        "启动事务必须清除旧 running task_items 行"
    );

    let config = DesktopConfig {
        nodes: vec![endpoint(first_node.address)],
        reconnect_interval_seconds: 1,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(config, desktop_paths(&fixture));
    wait_for_online(&mut events).await;

    app.send(UiCommand::CreateScan {
        node_index: 0,
        roots: vec![media.to_string_lossy().into_owned()],
        force_recalculate: true,
        enumerator: EnumeratorKind::WindowsWalker,
    })
    .await
    .unwrap();

    let first = recv_started(&mut started).await;
    let second = recv_started(&mut started).await;
    let scan_runtime_id = scan_runtime_id(&first_registry).await;
    let (listed, tick_count) = wait_runtime_state(&mut events, "两秒列表刷新", |state| {
        state.summaries().iter().any(|task| {
            task.key.id == scan_runtime_id && task.state == DesktopRuntimeTaskState::Running
        })
    })
    .await;
    assert!(
        listed
            .summaries()
            .iter()
            .any(|task| task.key.id == scan_runtime_id && task.machine_ids == [machine_id.as_str()]),
        "两秒 tick 必须从真实 Node 列出当前扫描"
    );

    let terminal_started = Instant::now();
    for item in [first, second] {
        worker_control
            .complete(
                item.0,
                item.1,
                Stage1Output {
                    media_kind: MediaKind::Other,
                    width: 0,
                    height: 0,
                    duration_ms: None,
                    frames: Vec::new(),
                    contact_sheet_jpeg: None,
                },
            )
            .await;
    }
    let (_terminal, _) = wait_runtime_state(&mut events, "主动终态事件", |state| {
        state.summaries().iter().any(|task| {
            task.key.id == scan_runtime_id && task.state == DesktopRuntimeTaskState::Completed
        })
    })
    .await;
    let terminal_elapsed = terminal_started.elapsed();
    assert!(
        terminal_elapsed < Duration::from_secs(2),
        "终态必须由主动事件在下一次两秒 tick 前到达，实际 {terminal_elapsed:?}"
    );

    first_node.disconnect().await;
    let restart_address = first_node.address;
    // 断线后的选中详情 stale 由控制器另行覆盖；本用例只验证下一次成功完整列表会清空旧摘要。
    first_node.shutdown_actor().await;

    let (second_workers, _second_started, _second_control) =
        WorkerPool::controlled_batch_for_test(2);
    let (second_node, second_registry) = RunningNode::start(
        &database,
        &cache,
        machine_id.clone(),
        second_workers,
        Some(restart_address),
    )
    .await;
    assert!(
        second_registry.list().await.is_empty(),
        "新进程不能用 recovery 包装或重新发布旧运行任务"
    );
    let (_reconnected, _) = wait_runtime_state(&mut events, "重连空运行列表", |state| {
        state
            .summaries()
            .iter()
            .all(|task| task.key.id != scan_runtime_id)
    })
    .await;
    println!(
        "RUNTIME_TASK_E2E_PASS ticks={tick_count} terminal_ms={} restart_runtime_empty=true",
        terminal_elapsed.as_millis()
    );

    app.send(UiCommand::Shutdown).await.unwrap();
    second_node.shutdown().await;
}

/// 真实 Node 进程与 TCP 服务的可控生命周期。
struct RunningNode {
    /// Node 绑定地址；重启时复用。
    address: SocketAddr,
    /// actor 控制句柄。
    handle: NodeEngineHandle,
    /// actor 任务。
    actor: JoinHandle<()>,
    /// TCP 服务关闭信号。
    server_shutdown: Option<oneshot::Sender<()>>,
    /// TCP 服务任务。
    server: Option<JoinHandle<Result<(), ServerError>>>,
}

impl RunningNode {
    /// 从真实 SQLite、可控双槽 WorkerPool 和指定地址启动 Node。
    async fn start(
        database: &Path,
        cache: &Path,
        machine_id: MachineId,
        workers: WorkerPool,
        address: Option<SocketAddr>,
    ) -> (Self, RuntimeTaskRegistry) {
        let listener = tokio::net::TcpListener::bind(
            address.unwrap_or_else(|| "127.0.0.1:0".parse().unwrap()),
        )
        .await
        .unwrap();
        let address = listener.local_addr().unwrap();
        let store = NodeStore::open(database, machine_id).unwrap();
        let (handle, actor) = NodeEngine::spawn(
            store,
            workers,
            address,
            cache,
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();
        let (server_shutdown, shutdown) = oneshot::channel();
        let server = tokio::spawn(NodeServer::serve_until(listener, handle.clone(), shutdown));
        (
            Self {
                address,
                handle,
                actor,
                server_shutdown: Some(server_shutdown),
                server: Some(server),
            },
            registry,
        )
    }

    /// 只关闭 TCP 服务，保留 actor 以便先观察 Desktop stale。
    async fn disconnect(&mut self) {
        if let Some(shutdown) = self.server_shutdown.take() {
            let _ = shutdown.send(());
        }
        if let Some(server) = self.server.take() {
            server.await.unwrap().unwrap();
        }
    }

    /// TCP 已断开后关闭 actor，释放 SQLite 文件供同地址重启。
    async fn shutdown_actor(self) {
        self.handle.shutdown().await.unwrap();
        self.actor.await.unwrap();
    }

    /// 完整关闭 TCP 与 actor。
    async fn shutdown(mut self) {
        self.disconnect().await;
        self.shutdown_actor().await;
    }
}

/// 在真实数据库中放入一个未完成任务，用于验证启动清理不会重新发布旧任务。
fn seed_recovery_task(database: &Path, machine_id: MachineId) -> String {
    let mut store = NodeStore::open(database, machine_id).unwrap();
    let task_id = store
        .create_task("scan", &[NewTaskItem::detached("queued")], 1)
        .unwrap();
    store
        .claim_next_item(task_id, 2)
        .unwrap()
        .expect("夹具任务必须进入 running");
    task_id.as_uuid().to_string()
}

/// 返回 registry 当前唯一扫描运行 ID。
async fn scan_runtime_id(registry: &RuntimeTaskRegistry) -> String {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            if let Some(task) = registry
                .list()
                .await
                .into_iter()
                .find(|task| task.task_kind == "base_compute")
            {
                return task.runtime_task_id;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("真实扫描必须创建运行 registry 项")
}

/// 等待可控 WorkerPool 真实占用一个槽位。
async fn recv_started(started: &mut mpsc::Receiver<(String, String)>) -> (String, String) {
    tokio::time::timeout(Duration::from_secs(5), started.recv())
        .await
        .expect("扫描必须派发到两个可控 Worker")
        .expect("可控 Worker started 通道不得提前关闭")
}

/// 等待 Desktop 成功握手并建立可供命令使用的会话。
async fn wait_for_online(events: &mut mpsc::Receiver<UiEvent>) {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Some(UiEvent::ViewChanged(state)) = events.recv().await
                && state.nodes()[0].connection == NodeConnectionState::Online
            {
                return;
            }
        }
    })
    .await
    .expect("Desktop 必须连接真实 NodeServer");
}

/// 只消费运行任务事件，返回满足谓词的快照以及本轮观察到的 tick/event 次数。
async fn wait_runtime_state(
    events: &mut mpsc::Receiver<UiEvent>,
    label: &str,
    predicate: impl Fn(&RuntimeTaskControllerState) -> bool,
) -> (RuntimeTaskControllerState, usize) {
    tokio::time::timeout(Duration::from_secs(8), async {
        let mut count = 0;
        loop {
            if let Some(UiEvent::RuntimeTasksChanged(state)) = events.recv().await {
                count += 1;
                if predicate(&state) {
                    return (state, count);
                }
            }
        }
    })
    .await
    .unwrap_or_else(|_| panic!("Desktop 没有发布预期运行任务状态：{label}"))
}

/// 断言 Desktop 保留的 Node 详情同时匹配会话机器和运行 ID。
/// 构造 Desktop 测试所需的独立数据、日志、缓存和配置路径。
fn desktop_paths(fixture: &TempDir) -> DesktopPaths {
    DesktopPaths {
        data: fixture.path().join("desktop-data"),
        logs: fixture.path().join("desktop-logs"),
        cache: fixture.path().join("desktop-cache"),
        config: fixture.path().join("desktop-config.toml"),
    }
}

/// 把 Node 监听地址转成 Desktop 配置端点。
fn endpoint(address: SocketAddr) -> NodeEndpoint {
    NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    }
}
