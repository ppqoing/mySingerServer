//! 通过显式环境变量连接两台已运行物理节点的外部端到端验收。
//!
//! 必需输入是 `DEDUP_TEST_LOCAL_ENDPOINT`、`DEDUP_TEST_REMOTE_ENDPOINT`、
//! `DEDUP_TEST_LOCAL_SCAN_ROOT`、`DEDUP_TEST_REMOTE_SCAN_ROOT` 和
//! `DEDUP_TEST_POSTGRES_URL`。扫描默认不设测试端超时；需要时显式设置
//! `DEDUP_TEST_SCAN_TIMEOUT_SECONDS`，中心阶段默认允许十二小时。
//! `DEDUP_TEST_EXPECT_LOCAL_FILES`、`DEDUP_TEST_EXPECT_REMOTE_FILES` 可锁定扫描数；
//! `DEDUP_TEST_EXPECT_*_GROUP_MIN` 与 `DEDUP_TEST_EXPECT_CANDIDATE_MIN` 可锁定夹具下限。

#![cfg(windows)]

use std::{env, net::SocketAddr, time::Duration};

use dedup_core::{MachineId, NodeEndpoint, TaskId, Thresholds};
use dedup_desktop_core::{
    analysis::{CrossAnalysisCoordinator, CrossNodeSelection, CrossPollReport},
    central::{CentralAnalysisStatus, CentralCandidateStatus, CentralGroupKind, CentralStore},
    node_session::NodeSession,
    sync::{SyncEngine, SyncReport, SyncTrigger},
};
use dedup_protocol::proto;
use tokio::time::Instant;

const LOCAL_ENDPOINT_ENV: &str = "DEDUP_TEST_LOCAL_ENDPOINT";
const REMOTE_ENDPOINT_ENV: &str = "DEDUP_TEST_REMOTE_ENDPOINT";
const LOCAL_SCAN_ROOT_ENV: &str = "DEDUP_TEST_LOCAL_SCAN_ROOT";
const REMOTE_SCAN_ROOT_ENV: &str = "DEDUP_TEST_REMOTE_SCAN_ROOT";
const POSTGRES_URL_ENV: &str = "DEDUP_TEST_POSTGRES_URL";
const SCAN_TIMEOUT_ENV: &str = "DEDUP_TEST_SCAN_TIMEOUT_SECONDS";
const COORDINATOR_TIMEOUT_ENV: &str = "DEDUP_TEST_COORDINATOR_TIMEOUT_SECONDS";

/// 测试会写入两端节点任务数据库和专用 PostgreSQL；默认忽略，绝不由普通门禁隐式访问外部主机。
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "requires two physical nodes and DEDUP_TEST_POSTGRES_URL"]
async fn physical_two_hosts_scan_sync_and_cross_analysis() {
    run_physical_acceptance().await;
}

async fn run_physical_acceptance() {
    let config = PhysicalConfig::from_env();
    eprintln!(
        "PHYSICAL_TWO_HOSTS_CONFIG local_endpoint={} remote_endpoint={} local_root={} remote_root={}",
        config.local_endpoint,
        config.remote_endpoint,
        config.local_scan_root,
        config.remote_scan_root,
    );

    let (local, remote) = tokio::try_join!(
        NodeSession::connect(config.local_endpoint.clone()),
        NodeSession::connect(config.remote_endpoint.clone()),
    )
    .expect("两台物理节点必须已经启动，且各自没有被另一管理连接占用");
    assert_ne!(
        local.machine_id(),
        remote.machine_id(),
        "本机与远端 endpoint 不得指向同一物理 MachineId"
    );
    assert_expected_machine("DEDUP_TEST_EXPECT_LOCAL_MACHINE_ID", local.machine_id());
    assert_expected_machine("DEDUP_TEST_EXPECT_REMOTE_MACHINE_ID", remote.machine_id());
    eprintln!(
        "PHYSICAL_TWO_HOSTS_MACHINES local_machine_id={} remote_machine_id={}",
        local.machine_id().as_str(),
        remote.machine_id().as_str(),
    );

    // CreateScan 当前在 actor 完整扫描后才返回 TaskAccepted；返回前没有 task_id 可供进度轮询。
    eprintln!(
        "PHYSICAL_TWO_HOSTS_SCAN_START side=local root={}",
        config.local_scan_root
    );
    eprintln!(
        "PHYSICAL_TWO_HOSTS_SCAN_START side=remote root={}",
        config.remote_scan_root
    );
    let (local_task, remote_task) = create_scans(&local, &remote, &config).await;
    let (local_summary, remote_summary) = tokio::join!(
        wait_for_completed_task(&local, local_task, config.scan_timeout),
        wait_for_completed_task(&remote, remote_task, config.scan_timeout),
    );
    print_scan_summary("local", &local_summary);
    print_scan_summary("remote", &remote_summary);
    assert_expected_count("DEDUP_TEST_EXPECT_LOCAL_FILES", local_summary.total_items);
    assert_expected_count("DEDUP_TEST_EXPECT_REMOTE_FILES", remote_summary.total_items);

    let mut central = CentralStore::connect(&config.postgres_url)
        .await
        .expect("PostgreSQL 必须已经由管理员手工执行 deploy/central-v2.sql");
    let local_sync = SyncEngine::new()
        .sync_node(&local, &mut central, SyncTrigger::Manual)
        .await
        .expect("本机节点增量或快照同步必须成功");
    let remote_sync = SyncEngine::new()
        .sync_node(&remote, &mut central, SyncTrigger::Manual)
        .await
        .expect("远端节点增量或快照同步必须成功");
    print_sync_summary("local", &local_sync);
    print_sync_summary("remote", &remote_sync);
    assert_eq!(local_sync.committed_seq, local_sync.node_high_seq);
    assert_eq!(remote_sync.committed_seq, remote_sync.node_high_seq);

    let selections = [
        CrossNodeSelection::new(&local, local_task),
        CrossNodeSelection::new(&remote, remote_task),
    ];
    let mut coordinator =
        CrossAnalysisCoordinator::start(&mut central, &selections, Thresholds::default())
            .await
            .expect("两个已完成扫描任务必须能冻结为同一中心分析运行");
    let sessions = [&local, &remote];
    let completed = run_coordinator(&mut coordinator, &mut central, &sessions, &config).await;

    let candidates = central
        .analysis_candidates(completed.run_id)
        .await
        .expect("应能读回最终候选");
    let candidate_counts = CandidateCounts::from_candidates(&candidates);
    let group_counts = count_groups(&central, completed.run_id).await;
    assert_eq!(completed.candidate_count, candidate_counts.total as usize);
    assert_minimum("DEDUP_TEST_EXPECT_CANDIDATE_MIN", candidate_counts.total);
    assert_minimum("DEDUP_TEST_EXPECT_GROUP_MIN", group_counts.total);
    assert_minimum("DEDUP_TEST_EXPECT_EXACT_GROUP_MIN", group_counts.exact);
    assert_minimum("DEDUP_TEST_EXPECT_IMAGE_GROUP_MIN", group_counts.image);
    assert_minimum("DEDUP_TEST_EXPECT_VIDEO_GROUP_MIN", group_counts.video);
    eprintln!(
        "PHYSICAL_TWO_HOSTS_RESULT run_id={} rounds={} candidates={} passed={} rejected={} incomplete={} groups={} exact_groups={} image_groups={} video_groups={}",
        completed.run_id.as_uuid(),
        completed.rounds,
        candidate_counts.total,
        candidate_counts.passed,
        candidate_counts.rejected,
        candidate_counts.incomplete,
        group_counts.total,
        group_counts.exact,
        group_counts.image,
        group_counts.video,
    );
}

struct PhysicalConfig {
    local_endpoint: NodeEndpoint,
    remote_endpoint: NodeEndpoint,
    local_scan_root: String,
    remote_scan_root: String,
    postgres_url: String,
    scan_timeout: Option<Duration>,
    coordinator_timeout: Duration,
}

impl PhysicalConfig {
    fn from_env() -> Self {
        Self {
            local_endpoint: endpoint_env(LOCAL_ENDPOINT_ENV),
            remote_endpoint: endpoint_env(REMOTE_ENDPOINT_ENV),
            local_scan_root: required_env(LOCAL_SCAN_ROOT_ENV),
            remote_scan_root: required_env(REMOTE_SCAN_ROOT_ENV),
            postgres_url: required_env(POSTGRES_URL_ENV),
            // 大型真实根可能耗时数小时；默认不从测试端取消节点扫描。
            scan_timeout: optional_seconds(SCAN_TIMEOUT_ENV),
            coordinator_timeout: optional_seconds(COORDINATOR_TIMEOUT_ENV)
                .unwrap_or(Duration::from_secs(12 * 60 * 60)),
        }
    }
}

async fn create_scans(
    local: &NodeSession,
    remote: &NodeSession,
    config: &PhysicalConfig,
) -> (TaskId, TaskId) {
    let scans = async {
        tokio::join!(
            local.create_scan(
                vec![config.local_scan_root.clone()],
                false,
                "windows_walker",
            ),
            remote.create_scan(
                vec![config.remote_scan_root.clone()],
                false,
                "windows_walker",
            ),
        )
    };
    let (local_result, remote_result) = match config.scan_timeout {
        Some(limit) => tokio::time::timeout(limit, scans)
            .await
            .unwrap_or_else(|_| {
                panic!("双物理节点扫描超过显式超时 {limit:?}；节点任务可能仍在继续")
            }),
        None => scans.await,
    };
    (
        local_result.expect("本机物理节点扫描请求必须成功"),
        remote_result.expect("远端物理节点扫描请求必须成功"),
    )
}

async fn wait_for_completed_task(
    session: &NodeSession,
    task_id: TaskId,
    timeout: Option<Duration>,
) -> proto::TaskSummary {
    let started = Instant::now();
    loop {
        let summary = session
            .query_task(task_id)
            .await
            .expect("扫描返回 TaskAccepted 后必须能查询持久任务");
        match task_state(&summary) {
            proto::TaskState::TaskCompleted => {
                assert!(summary.total_items > 0, "扫描根不得为空");
                assert_eq!(
                    summary.completed_items, summary.total_items,
                    "Completed 扫描的处理数必须等于总文件数"
                );
                return summary;
            }
            proto::TaskState::TaskFailed | proto::TaskState::TaskCancelled => {
                panic!(
                    "扫描任务 {} 以 {:?} 终止：completed={} failed={} skipped={} total={}",
                    summary.task_id,
                    task_state(&summary),
                    summary.completed_items,
                    summary.failed_items,
                    summary.skipped_items,
                    summary.total_items,
                );
            }
            proto::TaskState::TaskQueued | proto::TaskState::TaskRunning => {
                if let Some(limit) = timeout {
                    assert!(started.elapsed() < limit, "扫描任务超过显式超时 {limit:?}");
                }
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            proto::TaskState::Unspecified => panic!("节点返回未指定扫描任务状态"),
        }
    }
}

async fn run_coordinator(
    coordinator: &mut CrossAnalysisCoordinator,
    central: &mut CentralStore,
    sessions: &[&NodeSession],
    config: &PhysicalConfig,
) -> CompletedRun {
    let started = Instant::now();
    let mut rounds = 0_u64;
    loop {
        rounds += 1;
        let report = coordinator
            .poll(central, sessions)
            .await
            .expect("中心协调器必须完成节点查询、同步与持久化");
        eprintln!(
            "PHYSICAL_TWO_HOSTS_COORDINATOR round={} status={:?} candidates={} unresolved={} phase2_tasks={} skipped_incomplete={}",
            rounds,
            report.status,
            report.candidate_count,
            report.unresolved_candidates,
            report.phase2_task_count,
            report.skipped_incomplete,
        );
        match report.status {
            CentralAnalysisStatus::Completed => {
                return CompletedRun::new(report, rounds);
            }
            CentralAnalysisStatus::Partial => panic!(
                "跨机器分析停在 partial：候选={} 未解决={}",
                report.candidate_count, report.unresolved_candidates
            ),
            CentralAnalysisStatus::Cancelled => panic!("跨机器分析被取消"),
            _ => {}
        }
        assert!(
            started.elapsed() < config.coordinator_timeout,
            "中心协调超过 {:?}",
            config.coordinator_timeout
        );
        tokio::time::sleep(Duration::from_secs(1)).await;
    }
}

struct CompletedRun {
    run_id: dedup_core::AnalysisRunId,
    candidate_count: usize,
    rounds: u64,
}

impl CompletedRun {
    fn new(report: CrossPollReport, rounds: u64) -> Self {
        Self {
            run_id: report.run_id,
            candidate_count: report.candidate_count,
            rounds,
        }
    }
}

#[derive(Default)]
struct CandidateCounts {
    total: u64,
    passed: u64,
    rejected: u64,
    incomplete: u64,
}

impl CandidateCounts {
    fn from_candidates(candidates: &[dedup_desktop_core::central::CentralCandidate]) -> Self {
        let mut counts = Self::default();
        for candidate in candidates {
            counts.total += 1;
            match candidate.status {
                CentralCandidateStatus::Passed => counts.passed += 1,
                CentralCandidateStatus::Rejected => counts.rejected += 1,
                CentralCandidateStatus::Incomplete => counts.incomplete += 1,
                CentralCandidateStatus::Stage1Passed => {
                    panic!("Completed 运行不得保留未判定的一筛候选")
                }
            }
        }
        counts
    }
}

#[derive(Default)]
struct GroupCounts {
    total: u64,
    exact: u64,
    image: u64,
    video: u64,
}

async fn count_groups(central: &CentralStore, run_id: dedup_core::AnalysisRunId) -> GroupCounts {
    let mut counts = GroupCounts::default();
    let mut cursor = None;
    loop {
        let page = central
            .page_groups(run_id, cursor.as_deref(), 1000)
            .await
            .expect("应能稳定分页读取全部中心组");
        for group in page.items {
            counts.total += 1;
            match group.kind {
                CentralGroupKind::Exact => counts.exact += 1,
                CentralGroupKind::Image => counts.image += 1,
                CentralGroupKind::Video => counts.video += 1,
            }
        }
        let Some(next) = page.next_cursor else { break };
        assert_ne!(cursor.as_deref(), Some(next.as_str()), "中心组游标必须前进");
        cursor = Some(next);
    }
    counts
}

fn print_scan_summary(side: &str, summary: &proto::TaskSummary) {
    eprintln!(
        "PHYSICAL_TWO_HOSTS_SCAN side={} task_id={} state={:?} total={} completed={} failed={} skipped={} outbox_highwater={}",
        side,
        summary.task_id,
        task_state(summary),
        summary.total_items,
        summary.completed_items,
        summary.failed_items,
        summary.skipped_items,
        summary.outbox_high_seq,
    );
}

fn print_sync_summary(side: &str, report: &SyncReport) {
    eprintln!(
        "PHYSICAL_TWO_HOSTS_SYNC side={} committed_seq={} node_high_seq={} batches={} changes={} snapshot_pages={}",
        side,
        report.committed_seq,
        report.node_high_seq,
        report.batch_count,
        report.change_count,
        report.snapshot_page_count,
    );
}

fn task_state(summary: &proto::TaskSummary) -> proto::TaskState {
    proto::TaskState::try_from(summary.state).unwrap_or(proto::TaskState::Unspecified)
}

fn endpoint_env(name: &str) -> NodeEndpoint {
    let raw = required_env(name);
    let address: SocketAddr = raw
        .parse()
        .unwrap_or_else(|error| panic!("{name} 必须是 IP:port（IPv6 使用 [IP]:port）：{error}"));
    assert_ne!(address.port(), 0, "{name} 端口不能为 0");
    NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    }
}

fn required_env(name: &str) -> String {
    let value = env::var(name).unwrap_or_else(|_| panic!("缺少必需环境变量 {name}"));
    assert!(!value.trim().is_empty(), "环境变量 {name} 不能为空");
    value
}

fn optional_seconds(name: &str) -> Option<Duration> {
    optional_u64(name).map(|seconds| {
        assert!(seconds > 0, "环境变量 {name} 必须大于 0");
        Duration::from_secs(seconds)
    })
}

fn optional_u64(name: &str) -> Option<u64> {
    match env::var(name) {
        Ok(value) => Some(
            value
                .parse()
                .unwrap_or_else(|error| panic!("环境变量 {name} 必须是非负整数：{error}")),
        ),
        Err(env::VarError::NotPresent) => None,
        Err(error) => panic!("无法读取环境变量 {name}：{error}"),
    }
}

fn assert_expected_machine(name: &str, actual: &MachineId) {
    let Ok(expected) = env::var(name) else { return };
    let expected = MachineId::parse(&expected)
        .unwrap_or_else(|error| panic!("环境变量 {name} 不是规范 MachineId：{error}"));
    assert_eq!(actual, &expected, "{name} 与实际节点身份不一致");
}

fn assert_expected_count(name: &str, actual: u64) {
    if let Some(expected) = optional_u64(name) {
        assert_eq!(actual, expected, "{name} 与实际扫描文件数不一致");
    }
}

fn assert_minimum(name: &str, actual: u64) {
    if let Some(minimum) = optional_u64(name) {
        assert!(
            actual >= minimum,
            "{name} 要求至少 {minimum}，实际为 {actual}"
        );
    }
}
