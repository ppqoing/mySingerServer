//! 单节点真实便携运行时的 TCP 端到端验收。
//!
//! 显式运行：
//! `cargo test -p dedup-desktop-core --test local_node_e2e -- --ignored --test-threads=1`
//! 可用 `DEDUP_TEST_PACKAGE_ROOT` 指向已解压的真实发布根；未设置时读取仓库
//! `dist-rust-v2/staging`。测试只读取该发布根与仓库媒体夹具，所有运行时数据、媒体改写和
//! 永久删除都严格限制在本轮 `TempDir`。

#![cfg(windows)]

use std::{
    env, fs,
    io::Write,
    net::{IpAddr, Ipv4Addr, TcpListener},
    path::{Path, PathBuf},
    time::Duration,
};

use dedup_core::{
    AnalysisRunId, DeleteMode, EnumeratorKind, LocationKey, MachineId, NodeConfig, NodeEndpoint,
    NormalizedPath, TaskId, Thresholds,
};
use dedup_desktop_core::node_session::NodeSession;
use dedup_node_engine::actor::{FixedIdentityProvider, NodeRuntime};
use dedup_protocol::proto;
use dedup_windows::AppLayout;
use tempfile::{TempDir, tempdir};

const TEST_TIMEOUT: Duration = Duration::from_secs(240);
const POLL_INTERVAL: Duration = Duration::from_millis(50);
const FFMPEG_DLLS: [&str; 5] = [
    "avutil-60.dll",
    "swresample-6.dll",
    "swscale-9.dll",
    "avcodec-62.dll",
    "avformat-62.dll",
];
const IMAGE_MD5: [u8; 16] = [
    0x7a, 0x49, 0x7d, 0x6b, 0xc7, 0xae, 0x48, 0xac, 0x7c, 0x25, 0x8d, 0x8c, 0xf3, 0x61, 0xdd, 0xae,
];

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "requires DEDUP_TEST_PACKAGE_ROOT or dist-rust-v2 staging"]
async fn real_local_node_scan_analysis_review_delete_and_restart() {
    tokio::time::timeout(TEST_TIMEOUT, run_local_node_e2e())
        .await
        .expect("单节点真实进程验收超时");
}

async fn run_local_node_e2e() {
    let portable = copy_real_portable_runtime();
    let layout = AppLayout::from_executable(&portable.path().join("node.exe")).unwrap();
    let fixtures = create_media_fixtures(portable.path());
    let machine_id = MachineId::parse(&"91".repeat(32)).unwrap();
    let identity = FixedIdentityProvider::new(machine_id.clone());
    let mut config = node_config();

    let runtime = NodeRuntime::start(&layout, &config, &identity)
        .await
        .expect("真实 worker.exe 与 FFmpeg DLL 应完成 Ready 握手");
    config.port = runtime.listen_address().port();
    let session = connect(&runtime).await;
    assert_eq!(session.machine_id(), &machine_id);

    // 第一次扫描写入真实 MD5/媒体特征；随后只改首字节且保持大小不变。第二次若错误地重读
    // 文件，就会得到新内容键并派发已经损坏的 JPEG；命中路径+大小缓存则仍返回固定旧 MD5。
    let first_cache_scan = create_scan(&session, &fixtures.cache_root).await;
    assert_completed_task(&session, first_cache_scan, 1).await;
    corrupt_first_byte_without_resizing(&fixtures.cache_probe);
    let second_cache_scan = create_scan(&session, &fixtures.cache_root).await;
    assert_completed_task(&session, second_cache_scan, 1).await;
    let cached = session
        .prepare_analysis_input(AnalysisRunId::new(), &[second_cache_scan], "", 10)
        .await
        .unwrap();
    assert_eq!(cached.inputs.len(), 1);
    let cached_content = cached.inputs[0].content.as_ref().unwrap();
    assert_eq!(cached_content.md5.as_slice(), IMAGE_MD5);
    assert_eq!(cached_content.file_size, fixtures.cache_probe_size);

    // 主扫描只包含 TempDir 中复制/派生的真实 JPEG、MP4。两组完全相同的文件形成两个精确
    // 组；带 JPEG 尾随数据和合法 MP4 free box 的变体拥有不同 MD5，但解码像素完全相同。
    let scan_task = create_scan(&session, &fixtures.media_root).await;
    assert_completed_task(&session, scan_task, fixtures.media_files.len() as u64).await;

    let thresholds = Thresholds::default();
    let exact_run = create_analysis(
        &session,
        scan_task,
        proto::GroupKind::GroupExact,
        &thresholds,
    )
    .await;
    let image_run = create_analysis(
        &session,
        scan_task,
        proto::GroupKind::GroupSimilarImage,
        &thresholds,
    )
    .await;
    let video_run = create_analysis(
        &session,
        scan_task,
        proto::GroupKind::GroupSimilarVideo,
        &thresholds,
    )
    .await;

    // limit=1 强制走真实不透明游标。精确结果至少跨两页；图片和视频结果分别验证成员分页。
    let exact_groups = collect_groups(&session, exact_run, proto::GroupKind::GroupExact).await;
    assert_eq!(exact_groups.len(), 2, "JPEG 与 MP4 应各形成一个精确组");
    let image_groups =
        collect_groups(&session, image_run, proto::GroupKind::GroupSimilarImage).await;
    assert_eq!(image_groups.len(), 1, "三个图片内容键应形成一个相似组");
    let image_group = &image_groups[0];
    let image_members = collect_members(&session, image_run, &image_group.group_id).await;
    assert!(image_members.len() >= 3);
    let video_groups =
        collect_groups(&session, video_run, proto::GroupKind::GroupSimilarVideo).await;
    assert_eq!(video_groups.len(), 1, "三个视频内容键应形成一个相似组");
    let video_members = collect_members(&session, video_run, &video_groups[0].group_id).await;
    assert!(video_members.len() >= 3);

    // 在不会被本轮删除触及的图片组写入复核标记，并立即从 TCP 分页结果反查。
    let reviewed_location = location(&image_members[0]);
    session
        .save_review_mark(
            image_run,
            &image_group.group_id,
            &reviewed_location,
            proto::ReviewDecision::ReviewKeep,
        )
        .await
        .unwrap();
    assert_review(
        &session,
        image_run,
        &image_group.group_id,
        &reviewed_location,
        proto::ReviewDecision::ReviewKeep,
    )
    .await;

    // 删除视频精确组的非代表成员：先为代表写 Keep、目标写 Delete，再让节点重新核对大小与
    // MD5 并永久删除。选视频组可确保上面的图片复核标记仍可用于重启持久化检查。
    let (delete_group, delete_members) =
        find_video_exact_group(&session, exact_run, &exact_groups).await;
    let keep = delete_members
        .iter()
        .find(|member| member.representative)
        .expect("精确组应有代表成员");
    let target = delete_members
        .iter()
        .find(|member| !member.representative)
        .expect("精确组应有可删除成员");
    let keep_location = location(keep);
    let target_location = location(target);
    let target_path = fixtures.path_for(&target_location);
    session
        .save_review_mark(
            exact_run,
            &delete_group.group_id,
            &keep_location,
            proto::ReviewDecision::ReviewKeep,
        )
        .await
        .unwrap();
    session
        .save_review_mark(
            exact_run,
            &delete_group.group_id,
            &target_location,
            proto::ReviewDecision::ReviewDelete,
        )
        .await
        .unwrap();
    let deleted = session
        .create_delete_batch(
            exact_run,
            vec![proto::DeleteItem {
                delete_item_id: String::new(),
                group_id: delete_group.group_id.clone(),
                location: Some((&target_location).into()),
                expected_content: target.content.clone(),
                outcome: String::new(),
                message: String::new(),
            }],
            DeleteMode::Permanent,
        )
        .await
        .unwrap();
    assert_eq!(deleted.items.len(), 1);
    assert_eq!(deleted.items[0].outcome, "deleted");
    assert!(!target_path.exists(), "永久删除必须移除 TempDir 目标文件");
    let remaining_exact = collect_groups(&session, exact_run, proto::GroupKind::GroupExact).await;
    assert_eq!(remaining_exact.len(), 1);
    assert!(
        remaining_exact
            .iter()
            .all(|group| group.group_id != delete_group.group_id)
    );

    // 有序关闭后以同一 AppLayout/SQLite 重启真实 Worker 和 TCP listener；只经 NodeSession
    // 验证任务、三个分析运行、分页结果、复核标记和删除后的组状态均从持久库恢复。
    drop(session);
    runtime.shutdown().await.unwrap();
    config.port = reusable_runtime_port();
    let restarted = NodeRuntime::start(&layout, &config, &identity)
        .await
        .expect("节点应能从同一便携 SQLite 重启");
    let session = connect(&restarted).await;
    assert_eq!(session.machine_id(), &machine_id);
    assert_completed_task(&session, scan_task, fixtures.media_files.len() as u64).await;
    for run_id in [exact_run, image_run, video_run] {
        assert_eq!(wait_for_analysis(&session, run_id).await.state, "completed");
    }
    assert_review(
        &session,
        image_run,
        &image_group.group_id,
        &reviewed_location,
        proto::ReviewDecision::ReviewKeep,
    )
    .await;
    let restored_exact = collect_groups(&session, exact_run, proto::GroupKind::GroupExact).await;
    assert_eq!(restored_exact.len(), 1);
    assert!(!target_path.exists());

    drop(session);
    restarted.shutdown().await.unwrap();
}

/// 把发布 worker 与固定五 DLL 复制到本轮临时便携根，绝不就地运行或修改 staging。
fn copy_real_portable_runtime() -> TempDir {
    let repository = repository_root();
    let source = env::var_os("DEDUP_TEST_PACKAGE_ROOT")
        .map(PathBuf::from)
        .unwrap_or_else(|| repository.join("dist-rust-v2").join("staging"));
    assert!(
        source.join("worker.exe").is_file(),
        "真实发布根缺少 worker.exe：{}",
        source.display()
    );
    let portable = tempdir().unwrap();
    fs::copy(
        source.join("worker.exe"),
        portable.path().join("worker.exe"),
    )
    .unwrap();
    let destination = portable.path().join("runtime").join("ffmpeg");
    fs::create_dir_all(&destination).unwrap();
    for name in FFMPEG_DLLS {
        let dll = source.join("runtime").join("ffmpeg").join(name);
        assert!(
            dll.is_file(),
            "真实发布根缺少 FFmpeg DLL：{}",
            dll.display()
        );
        fs::copy(dll, destination.join(name)).unwrap();
    }
    portable
}

struct MediaFixtures {
    cache_root: PathBuf,
    cache_probe: PathBuf,
    cache_probe_size: u64,
    media_root: PathBuf,
    media_files: Vec<PathBuf>,
}

impl MediaFixtures {
    fn path_for(&self, location: &LocationKey) -> PathBuf {
        self.media_files
            .iter()
            .find(|path| NormalizedPath::new(path).unwrap() == *location.normalized_path())
            .cloned()
            .expect("协议位置必须来自本轮 TempDir 媒体夹具")
    }
}

/// 复制仓库固定 JPEG/MP4，并仅通过容器允许的尾随数据制造“内容键不同、解码内容相同”。
fn create_media_fixtures(portable: &Path) -> MediaFixtures {
    let source = repository_root()
        .join("tests")
        .join("fixtures")
        .join("media");
    let cache_root = portable.join("fixtures-cache");
    let media_root = portable.join("fixtures-media");
    fs::create_dir_all(&cache_root).unwrap();
    fs::create_dir_all(&media_root).unwrap();

    let cache_probe = cache_root.join("cache-probe.jpg");
    fs::copy(source.join("image.jpg"), &cache_probe).unwrap();
    let cache_probe_size = fs::metadata(&cache_probe).unwrap().len();

    let mut media_files = Vec::new();
    for name in ["exact-image-a.jpg", "exact-image-b.jpg"] {
        let target = media_root.join(name);
        fs::copy(source.join("image.jpg"), &target).unwrap();
        media_files.push(target);
    }
    for (name, suffix) in [
        ("similar-image-a.jpg", b"similar-a".as_slice()),
        ("similar-image-b.jpg", b"similar-b".as_slice()),
    ] {
        let target = media_root.join(name);
        fs::copy(source.join("image.jpg"), &target).unwrap();
        append(&target, suffix);
        media_files.push(target);
    }
    for name in ["exact-video-a.mp4", "exact-video-b.mp4"] {
        let target = media_root.join(name);
        fs::copy(source.join("video-12s.mp4"), &target).unwrap();
        media_files.push(target);
    }
    for (name, payload) in [("similar-video-a.mp4", b'A'), ("similar-video-b.mp4", b'B')] {
        let target = media_root.join(name);
        fs::copy(source.join("video-12s.mp4"), &target).unwrap();
        append(&target, &[0, 0, 0, 9, b'f', b'r', b'e', b'e', payload]);
        media_files.push(target);
    }

    MediaFixtures {
        cache_root,
        cache_probe,
        cache_probe_size,
        media_root,
        media_files,
    }
}

fn append(path: &Path, bytes: &[u8]) {
    fs::OpenOptions::new()
        .append(true)
        .open(path)
        .unwrap()
        .write_all(bytes)
        .unwrap();
}

fn corrupt_first_byte_without_resizing(path: &Path) {
    let mut bytes = fs::read(path).unwrap();
    assert_eq!(bytes[0], 0xff, "固定 JPEG 应以 SOI 标记开始");
    bytes[0] = 0xfe;
    fs::write(path, &bytes).unwrap();
}

/// `NodeRuntime` 当前调用 `NodeConfig::validate`，而公开校验仍拒绝端口 0。优先保留 port=0
/// 语义；在现有 API 下退回由 OS 预分配的 loopback 临时端口，避免使用固定测试端口。
fn node_config() -> NodeConfig {
    let mut config = NodeConfig {
        listen_ip: IpAddr::V4(Ipv4Addr::LOCALHOST),
        port: 0,
        worker_count: 1,
        enumerator: EnumeratorKind::WindowsWalker,
        ..NodeConfig::default()
    };
    if config.validate().is_err() {
        config.port = reusable_runtime_port();
    }
    config
}

fn reusable_runtime_port() -> u16 {
    TcpListener::bind((Ipv4Addr::LOCALHOST, 0))
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

async fn connect(runtime: &NodeRuntime) -> NodeSession {
    NodeSession::connect(NodeEndpoint {
        ip: runtime.listen_address().ip(),
        port: runtime.listen_address().port(),
    })
    .await
    .unwrap()
}

async fn create_scan(session: &NodeSession, root: &Path) -> TaskId {
    session
        .create_scan(
            vec![root.to_string_lossy().into_owned()],
            false,
            "windows_walker",
        )
        .await
        .unwrap()
}

async fn assert_completed_task(session: &NodeSession, task_id: TaskId, total_items: u64) {
    let task = wait_for_task(session, task_id).await;
    assert_eq!(
        proto::TaskState::try_from(task.state).unwrap(),
        proto::TaskState::TaskCompleted
    );
    assert_eq!(task.total_items, total_items);
    assert_eq!(task.completed_items, total_items);
    assert_eq!(task.failed_items, 0);
}

async fn wait_for_task(session: &NodeSession, task_id: TaskId) -> proto::TaskSummary {
    loop {
        let task = session.query_task(task_id).await.unwrap();
        match proto::TaskState::try_from(task.state).unwrap() {
            proto::TaskState::TaskCompleted
            | proto::TaskState::TaskFailed
            | proto::TaskState::TaskCancelled => return task,
            _ => tokio::time::sleep(POLL_INTERVAL).await,
        }
    }
}

async fn create_analysis(
    session: &NodeSession,
    scan_task: TaskId,
    kind: proto::GroupKind,
    thresholds: &Thresholds,
) -> AnalysisRunId {
    let run_id = session
        .create_local_analysis(&[scan_task], kind, thresholds)
        .await
        .unwrap();
    assert_eq!(wait_for_analysis(session, run_id).await.state, "completed");
    run_id
}

async fn wait_for_analysis(
    session: &NodeSession,
    run_id: AnalysisRunId,
) -> proto::QueryAnalysisRun {
    loop {
        let run = session.query_analysis_run(run_id).await.unwrap();
        if matches!(run.state.as_str(), "completed" | "partial" | "cancelled") {
            return run;
        }
        tokio::time::sleep(POLL_INTERVAL).await;
    }
}

/// limit=1 收集全部组；两个精确组会保证 `next_cursor` 的续页分支实际执行。
async fn collect_groups(
    session: &NodeSession,
    run_id: AnalysisRunId,
    kind: proto::GroupKind,
) -> Vec<proto::DuplicateGroup> {
    let mut cursor = String::new();
    let mut groups = Vec::new();
    loop {
        let page = session.list_groups(run_id, kind, &cursor, 1).await.unwrap();
        groups.extend(page.groups);
        if page.next_cursor.is_empty() {
            return groups;
        }
        assert_ne!(page.next_cursor, cursor, "组分页游标必须前进");
        cursor = page.next_cursor;
    }
}

/// limit=1 收集成员，确保每类结果都至少走一次成员续页。
async fn collect_members(
    session: &NodeSession,
    run_id: AnalysisRunId,
    group_id: &str,
) -> Vec<proto::GroupMember> {
    let mut cursor = String::new();
    let mut members = Vec::new();
    loop {
        let page = session
            .list_group_members(run_id, group_id, &cursor, 1)
            .await
            .unwrap();
        members.extend(page.members);
        if page.next_cursor.is_empty() {
            assert!(members.len() >= 2, "重复组至少应有两个成员");
            return members;
        }
        assert_ne!(page.next_cursor, cursor, "成员分页游标必须前进");
        cursor = page.next_cursor;
    }
}

async fn find_video_exact_group(
    session: &NodeSession,
    run_id: AnalysisRunId,
    groups: &[proto::DuplicateGroup],
) -> (proto::DuplicateGroup, Vec<proto::GroupMember>) {
    for group in groups {
        let members = collect_members(session, run_id, &group.group_id).await;
        if members.iter().all(|member| {
            member
                .location
                .as_ref()
                .is_some_and(|location| location.normalized_path.ends_with(".MP4"))
        }) {
            return (group.clone(), members);
        }
    }
    panic!("两个精确组中应包含固定 MP4 组");
}

async fn assert_review(
    session: &NodeSession,
    run_id: AnalysisRunId,
    group_id: &str,
    expected_location: &LocationKey,
    expected: proto::ReviewDecision,
) {
    let members = collect_members(session, run_id, group_id).await;
    let reviewed = members
        .iter()
        .find(|member| location(member) == *expected_location)
        .expect("复核位置应仍在持久组内");
    assert_eq!(
        proto::ReviewDecision::try_from(reviewed.review).unwrap(),
        expected
    );
}

fn location(member: &proto::GroupMember) -> LocationKey {
    member
        .location
        .clone()
        .expect("组成员必须携带位置键")
        .try_into()
        .unwrap()
}

fn repository_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("..").join("..")
}
