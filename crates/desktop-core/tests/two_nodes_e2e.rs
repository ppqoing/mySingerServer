//! 两个独立便携节点的真实 Worker、TCP 会话与跨机器编排端到端验收。

#![cfg(windows)]

use std::{
    collections::BTreeSet,
    fs,
    net::{IpAddr, Ipv4Addr, TcpListener},
    path::{Path, PathBuf},
};

use dedup_core::{
    AnalysisRunId, ContentKey, EnumeratorKind, LocationKey, MachineId, NodeConfig, NodeEndpoint,
    TaskId, Thresholds,
};
use dedup_desktop_core::{
    analysis::{CrossAnalysisCoordinator, CrossNodeSelection},
    central::{CentralAnalysisStatus, CentralCandidateStatus, CentralGroupKind, CentralStore},
    node_session::NodeSession,
};
use dedup_node_engine::actor::{FixedIdentityProvider, NodeRuntime};
use dedup_protocol::proto;
use dedup_windows::AppLayout;
use tempfile::{Builder, TempDir};
use uuid::Uuid;

const FFMPEG_DLLS: [&str; 5] = [
    "avutil-60.dll",
    "swresample-6.dll",
    "swscale-9.dll",
    "avcodec-62.dll",
    "avformat-62.dll",
];

/// 显式门禁使用一台 Windows 主机上的两个真实节点进程闭包，不需要第二台物理机或 PostgreSQL。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "requires DEDUP_TEST_PACKAGE_ROOT or dist-rust-v2 staging"]
async fn two_portable_nodes_isolate_identity_scan_highwater_and_inputs() {
    let package_root = package_root();
    let fixture = repository_root()
        .join("crates")
        .join("media")
        .join("testdata")
        .join("pdq")
        .join("bridge-original.jpg");
    let [left_port, right_port] = reserve_two_ports();
    let left_machine = unique_machine_id();
    let right_machine = unique_machine_id();
    assert_ne!(left_machine, right_machine);

    let (left, right) = tokio::join!(
        start_test_node(
            &package_root,
            &fixture,
            left_machine.clone(),
            left_port,
            false,
        ),
        start_test_node(
            &package_root,
            &fixture,
            right_machine.clone(),
            right_port,
            true,
        ),
    );
    assert_ne!(left.layout.node_database(), right.layout.node_database());
    assert!(left.layout.node_database().is_file());
    assert!(right.layout.node_database().is_file());

    let (left_session, right_session) = connect_both(&left, &right).await;
    assert_eq!(left_session.machine_id(), &left_machine);
    assert_eq!(right_session.machine_id(), &right_machine);
    assert_ne!(left_session.machine_id(), right_session.machine_id());

    let (left_status, right_status) =
        tokio::try_join!(left_session.status(), right_session.status(),)
            .expect("两个独立管理会话都应能并行读取状态");
    assert_eq!(left_status.machine_id, left_machine.as_str());
    assert_eq!(right_status.machine_id, right_machine.as_str());
    assert_eq!(left_status.listen_address, left.endpoint().to_string());
    assert_eq!(right_status.listen_address, right.endpoint().to_string());

    let (left_task, right_task) = scan_both(&left, &right, &left_session, &right_session).await;
    let (left_summary, right_summary) = tokio::try_join!(
        left_session.query_task(left_task),
        right_session.query_task(right_task),
    )
    .expect("两个真实扫描任务都应可查询");
    assert_completed_scan(&left_summary);
    assert_completed_scan(&right_summary);

    let (left_after, right_after) =
        tokio::try_join!(left_session.status(), right_session.status(),)
            .expect("扫描后应能并行读取节点高水位");
    assert!(left_summary.outbox_high_seq > 0);
    assert!(right_summary.outbox_high_seq > 0);
    assert!(left_after.outbox_high_seq >= left_summary.outbox_high_seq);
    assert!(right_after.outbox_high_seq >= right_summary.outbox_high_seq);

    let run_id = AnalysisRunId::new();
    let left_tasks = [left_task];
    let right_tasks = [right_task];
    let (left_page, right_page) = tokio::try_join!(
        left_session.prepare_analysis_input(run_id, &left_tasks, "", 1000),
        right_session.prepare_analysis_input(run_id, &right_tasks, "", 1000),
    )
    .expect("一个 desktop-core 应能并行取得两个节点的冻结候选输入");
    let left_content = assert_single_input(&left_page, &left_machine);
    let right_content = assert_single_input(&right_page, &right_machine);
    assert_ne!(
        left_content, right_content,
        "两个 JPEG 注释变体必须形成不同内容键"
    );

    drop((left_session, right_session));
    shutdown_both(left, right).await;
}

/// 真实中心编排依赖管理员已经手工创建的 V2 PostgreSQL schema，因此不进入普通门禁。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn coordinator_runs_stage1_batched_phase2_and_final_group_across_two_nodes() {
    let postgres_url = std::env::var("DEDUP_TEST_POSTGRES_URL")
        .expect("忽略测试必须显式提供 DEDUP_TEST_POSTGRES_URL");
    let package_root = package_root();
    let fixture = repository_root()
        .join("crates")
        .join("media")
        .join("testdata")
        .join("pdq")
        .join("bridge-original.jpg");
    let [left_port, right_port] = reserve_two_ports();
    let left_machine = unique_machine_id();
    let right_machine = unique_machine_id();
    let (left, right) = tokio::join!(
        start_test_node(
            &package_root,
            &fixture,
            left_machine.clone(),
            left_port,
            false,
        ),
        start_test_node(
            &package_root,
            &fixture,
            right_machine.clone(),
            right_port,
            true,
        ),
    );
    let (left_session, right_session) = connect_both(&left, &right).await;
    let (left_task, right_task) = scan_both(&left, &right, &left_session, &right_session).await;

    let mut central = CentralStore::connect(&postgres_url)
        .await
        .expect("PostgreSQL 必须已经手工执行 deploy/central-v2.sql");
    let mut coordinator = CrossAnalysisCoordinator::start(
        &mut central,
        &[
            CrossNodeSelection::new(&left_session, left_task),
            CrossNodeSelection::new(&right_session, right_task),
        ],
        Thresholds::default(),
    )
    .await
    .expect("两个节点扫描任务应能冻结为同一个中心运行");

    let dispatched = coordinator
        .poll(&mut central, &[&left_session, &right_session])
        .await
        .expect("一筛同步后应向两个来源节点派发缺失二筛");
    assert_eq!(dispatched.status, CentralAnalysisStatus::Phase2Dispatched);
    assert_eq!(dispatched.candidate_count, 1);
    assert_eq!(dispatched.phase2_task_count, 2);

    let completed = coordinator
        .poll(&mut central, &[&left_session, &right_session])
        .await
        .expect("二筛任务高水位同步后应完成中心分组");
    assert_eq!(completed.status, CentralAnalysisStatus::Completed);
    assert_eq!(completed.candidate_count, 1);
    assert_eq!(completed.unresolved_candidates, 0);
    let candidates = central
        .analysis_candidates(completed.run_id)
        .await
        .expect("应能读回最终候选");
    assert_eq!(candidates.len(), 1);
    assert_eq!(candidates[0].status, CentralCandidateStatus::Passed);

    let groups = central
        .page_groups(completed.run_id, None, 10)
        .await
        .expect("应能读回最终中心组");
    assert_eq!(groups.items.len(), 1);
    assert_eq!(groups.items[0].kind, CentralGroupKind::Image);
    assert_eq!(groups.items[0].member_count, 2);
    let members = central
        .page_group_members(completed.run_id, &groups.items[0].group_id, None, 10)
        .await
        .expect("应能读回两个物理身份隔离的位置成员");
    assert_eq!(members.items.len(), 2);
    assert_eq!(
        members
            .items
            .iter()
            .map(|member| member.location.machine_id().clone())
            .collect::<BTreeSet<_>>(),
        BTreeSet::from([left_machine, right_machine]),
    );

    drop((left_session, right_session));
    shutdown_both(left, right).await;
}

struct TestNode {
    _portable: TempDir,
    layout: AppLayout,
    media_root: PathBuf,
    runtime: NodeRuntime,
}

impl TestNode {
    fn endpoint(&self) -> NodeEndpoint {
        let address = self.runtime.listen_address();
        NodeEndpoint {
            ip: address.ip(),
            port: address.port(),
        }
    }
}

async fn start_test_node(
    package_root: &Path,
    fixture: &Path,
    machine_id: MachineId,
    port: u16,
    right_side: bool,
) -> TestNode {
    let portable = Builder::new()
        .prefix("dedup-two-node-")
        .tempdir()
        .expect("应能创建隔离便携目录");
    copy_worker_runtime(package_root, portable.path());
    let media_root = portable.path().join("media");
    fs::create_dir_all(&media_root).expect("应能创建节点媒体目录");
    let media_file = media_root.join(if right_side {
        "bridge-right.jpg"
    } else {
        "bridge-left.jpg"
    });
    // 每次运行都使用唯一机器 ID 作为 JPEG 注释，避免复跑 PG 测试时误复用旧全局二筛缓存。
    write_jpeg_comment_variant(fixture, &media_file, machine_id.as_str().as_bytes());

    let layout = AppLayout::from_executable(&portable.path().join("node.exe"))
        .expect("临时便携目录应能形成独立 AppLayout");
    let config = NodeConfig {
        listen_ip: IpAddr::V4(Ipv4Addr::LOCALHOST),
        port,
        worker_count: 1,
        enumerator: EnumeratorKind::WindowsWalker,
        ..NodeConfig::default()
    };
    let identity = FixedIdentityProvider::new(machine_id);
    let runtime = NodeRuntime::start(&layout, &config, &identity)
        .await
        .expect("真实 worker.exe 与 FFmpeg 闭包应能启动 NodeRuntime");
    assert_eq!(runtime.listen_address().port(), port);
    TestNode {
        _portable: portable,
        layout,
        media_root,
        runtime,
    }
}

async fn connect_both(left: &TestNode, right: &TestNode) -> (NodeSession, NodeSession) {
    tokio::try_join!(
        NodeSession::connect(left.endpoint()),
        NodeSession::connect(right.endpoint()),
    )
    .expect("一个 desktop-core 应能并行连接两个独立节点")
}

async fn scan_both(
    left: &TestNode,
    right: &TestNode,
    left_session: &NodeSession,
    right_session: &NodeSession,
) -> (TaskId, TaskId) {
    tokio::try_join!(
        left_session.create_scan(
            vec![left.media_root.to_string_lossy().into_owned()],
            false,
            "windows_walker",
        ),
        right_session.create_scan(
            vec![right.media_root.to_string_lossy().into_owned()],
            false,
            "windows_walker",
        ),
    )
    .expect("两个独立 NodeRuntime 应能并行完成真实 Worker 扫描")
}

fn assert_completed_scan(summary: &proto::TaskSummary) {
    assert_eq!(summary.task_kind, "scan");
    assert_eq!(summary.state, proto::TaskState::TaskCompleted as i32);
    assert_eq!(summary.total_items, 1);
    assert_eq!(summary.completed_items, 1);
    assert_eq!(summary.failed_items, 0);
    assert_eq!(summary.skipped_items, 0);
}

fn assert_single_input(page: &proto::PrepareAnalysisInput, machine_id: &MachineId) -> ContentKey {
    assert!(page.next_cursor.is_empty());
    assert_eq!(page.inputs.len(), 1);
    let input = &page.inputs[0];
    assert_eq!(input.media_kind, proto::MediaKind::MediaImage as i32);
    assert!(input.stage1_complete);
    assert!(!input.stage2_complete);
    assert_eq!(input.locations.len(), 1);
    let location: LocationKey = input.locations[0]
        .clone()
        .try_into()
        .expect("节点应返回规范位置键");
    assert_eq!(location.machine_id(), machine_id);
    input
        .content
        .clone()
        .expect("节点分析输入应包含内容键")
        .try_into()
        .expect("节点应返回规范内容键")
}

async fn shutdown_both(left: TestNode, right: TestNode) {
    let (left_result, right_result) =
        tokio::join!(left.runtime.shutdown(), right.runtime.shutdown());
    left_result.expect("左节点应有序关闭");
    right_result.expect("右节点应有序关闭");
}

fn copy_worker_runtime(package_root: &Path, portable_root: &Path) {
    let source_worker = package_root.join("worker.exe");
    assert!(
        source_worker.is_file(),
        "测试包缺少真实 worker.exe: {}",
        source_worker.display()
    );
    fs::copy(&source_worker, portable_root.join("worker.exe")).expect("应能复制真实 worker.exe");
    let source_runtime = package_root.join("runtime").join("ffmpeg");
    let target_runtime = portable_root.join("runtime").join("ffmpeg");
    fs::create_dir_all(&target_runtime).expect("应能创建 FFmpeg 运行库目录");
    for name in FFMPEG_DLLS {
        let source = source_runtime.join(name);
        assert!(
            source.is_file(),
            "测试包缺少 FFmpeg DLL: {}",
            source.display()
        );
        fs::copy(source, target_runtime.join(name)).expect("应能复制真实 FFmpeg DLL");
    }
}

fn write_jpeg_comment_variant(source: &Path, destination: &Path, comment: &[u8]) {
    let bytes = fs::read(source).expect("应能读取 JPEG 夹具");
    assert!(bytes.starts_with(&[0xff, 0xd8]), "夹具必须以 JPEG SOI 开始");
    let segment_size = u16::try_from(comment.len() + 2).expect("JPEG 注释长度必须可编码");
    let mut variant = Vec::with_capacity(bytes.len() + comment.len() + 4);
    variant.extend_from_slice(&bytes[..2]);
    // 合法 COM 段只改变文件字节和 ContentKey，不改变解码像素及两层图片特征。
    variant.extend_from_slice(&[0xff, 0xfe]);
    variant.extend_from_slice(&segment_size.to_be_bytes());
    variant.extend_from_slice(comment);
    variant.extend_from_slice(&bytes[2..]);
    fs::write(destination, variant).expect("应能写入 JPEG 注释变体");
}

fn package_root() -> PathBuf {
    let configured = std::env::var_os("DEDUP_TEST_PACKAGE_ROOT")
        .map(PathBuf::from)
        .unwrap_or_else(|| repository_root().join("dist-rust-v2").join("staging"));
    configured.canonicalize().unwrap_or_else(|error| {
        panic!(
            "无法定位真实 Rust V2 测试包 {}: {error}",
            configured.display()
        )
    })
}

fn repository_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("..")
        .canonicalize()
        .expect("应能定位工作区根目录")
}

fn unique_machine_id() -> MachineId {
    let half = Uuid::new_v4().simple().to_string();
    MachineId::parse(&format!("{half}{half}")).expect("UUID 应能组成 64 位小写十六进制机器 ID")
}

fn reserve_two_ports() -> [u16; 2] {
    // 生产 NodeConfig 明确禁止 port=0；测试先让系统分配两个端口并同时占住，读回后再释放。
    let left = TcpListener::bind((Ipv4Addr::LOCALHOST, 0)).expect("应能预留左节点端口");
    let right = TcpListener::bind((Ipv4Addr::LOCALHOST, 0)).expect("应能预留右节点端口");
    let ports = [
        left.local_addr().expect("左端口应可读取").port(),
        right.local_addr().expect("右端口应可读取").port(),
    ];
    assert_ne!(ports[0], ports[1]);
    ports
}
