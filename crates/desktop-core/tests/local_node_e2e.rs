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
    AnalysisRunId, EnumeratorKind, LocationKey, MachineId, NodeConfig, NodeEndpoint,
    NormalizedPath, TaskId,
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
async fn real_local_node_scan_and_preview() {
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
    let config = node_config();

    let runtime = NodeRuntime::start(&layout, &config, &identity)
        .await
        .expect("真实 worker.exe 与 FFmpeg DLL 应完成 Ready 握手");
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

    // 主扫描只包含 TempDir 中复制/派生的真实 JPEG、MP4，验证 Node 计算和任务收尾。
    let scan_task = create_scan(&session, &fixtures.media_root).await;
    assert_completed_task(&session, scan_task, fixtures.media_files.len() as u64).await;

    // 预览仍通过在线 Node 的 ReadFile 边界按块读取；结果页不会因此获得 Node 本地分析模型。
    let preview_location = LocationKey::new(
        machine_id.clone(),
        NormalizedPath::new(&fixtures.media_files[0]).unwrap(),
    );
    let preview = session
        .read_file_chunk(&preview_location, "original", 0, 1_048_576)
        .await
        .unwrap();
    assert!(!preview.data.is_empty(), "在线成员预览必须返回原始文件数据");

    drop(session);
    runtime.shutdown().await.unwrap();
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

fn repository_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("..").join("..")
}
