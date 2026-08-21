//! 磁盘满时只清理显式注册可再生产物的隔离目录行为测试。

use std::{
    cell::Cell,
    fs,
    future::Future,
    io,
    path::Path,
    pin::Pin,
    sync::{Arc, Barrier},
    thread,
};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    artifact_registry::{ArtifactKind, RegenerableArtifactRegistry},
    disk_full_cleanup::{
        ArtifactDiskResolver, DiskFullCleaner, write_planned_artifact_with_disk_full_cleanup,
        write_with_disk_full_cleanup,
    },
    io::ReadFailure,
    scan::{
        FileEnumerator, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine, ScanError,
        ScanOptions, Stage1Processor, Stage1Request, SystemMd5, md5_bytes,
    },
    worker::{Stage1Frame, Stage1Output},
};
use dedup_node_store::{FeatureWrite, NodeStore, ScannedPath};
use dedup_windows::ReadCancellationToken;
use tempfile::tempdir;

#[derive(Clone, Copy)]
struct FixtureDiskResolver;

impl ArtifactDiskResolver for FixtureDiskResolver {
    fn shares_physical_disk(&self, artifact: &Path, _write_target: &Path) -> io::Result<bool> {
        Ok(artifact.file_name().and_then(|name| name.to_str()) != Some("other-disk.bin"))
    }
}

#[derive(Clone)]
struct OneFileEnumerator(ScannedPath);

impl FileEnumerator for OneFileEnumerator {
    fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Ok(vec![self.0.clone()])
    }
}

#[derive(Clone, Copy)]
struct ImmediateReader;

impl PipelineFileReader for ImmediateReader {
    type Lease = ();

    fn read(
        &self,
        scanned: ScannedPath,
        _cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send>> {
        Box::pin(async move {
            let bytes = fs::read(scanned.display_path.as_path()).map_err(|source| {
                ReadFailure::Io {
                    path: scanned.display_path.as_path().to_path_buf(),
                    block_offset: 0,
                    source,
                }
            })?;
            Ok(ReadProduct {
                md5: md5_bytes(&bytes),
                lease: (),
            })
        })
    }
}

struct VideoProcessor;

impl Stage1Processor for VideoProcessor {
    async fn process(&mut self, _request: Stage1Request) -> Result<Stage1Output, String> {
        Ok(Stage1Output {
            media_kind: MediaKind::Video,
            width: 1,
            height: 1,
            duration_ms: Some(1_000),
            frames: (0..6)
                .map(|slot| Stage1Frame {
                    slot,
                    feature: None,
                    error: Some("fixture".into()),
                })
                .collect(),
            contact_sheet_jpeg: Some(b"jpeg".to_vec()),
        })
    }
}

#[tokio::test]
async fn production_contact_sheet_write_registers_the_published_artifact() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("mySingerServer");
    let cache_root = install_root.join("data/node/cache");
    fs::create_dir_all(&cache_root).unwrap();
    let media = install_root.join("media/video.mp4");
    write_fixture(&media, b"production-video");
    let scanned = ScannedPath::new(
        NormalizedPath::new(&media).unwrap(),
        DisplayPath::new(&media).unwrap(),
        fs::metadata(&media).unwrap().len(),
    );
    let registry = Arc::new(
        RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap(),
    );
    let cleaner = DiskFullCleaner::new(Arc::clone(&registry), FixtureDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    let mut engine = ScanEngine::new(
        OneFileEnumerator(scanned),
        SystemMd5,
        &contact_root,
    )
    .with_disk_full_cleanup(Arc::clone(&registry), cleaner);
    let machine = MachineId::from_sha256([0x83; 32]);
    let mut store = NodeStore::open_in_memory(machine).unwrap();

    engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![DisplayPath::new(&install_root).unwrap()]).force_recompute(),
            ImmediateReader,
            &mut VideoProcessor,
            PipelineLimits::new(1, 1),
            ReadCancellationToken::new(),
            10,
        )
        .await
        .unwrap();

    let digest = md5_bytes(b"production-video")
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let final_path = contact_root
        .join(&digest[..2])
        .join(format!("{digest}.jpg"));
    assert_eq!(fs::read(&final_path).unwrap(), b"jpeg");
    let _lease = registry
        .lease(&final_path)
        .expect("真实写入发布后必须登记为可租约联系表");
}

#[test]
fn disk_full_cleanup_deletes_the_complete_registered_same_disk_set_once() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("mySingerServer");
    let cache_root = install_root.join("data/node/cache");
    let contact = cache_root.join("contact-sheets/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.jpg");
    let preview = cache_root.join("previews/preview.bin");
    let orphan = cache_root.join("tmp/write.partial");
    let derivation = cache_root.join("derived/result.bin");
    let leased = cache_root.join("previews/leased.bin");
    let other_disk = cache_root.join("derived/other-disk.bin");
    let protected = [
        install_root.join("data/node/node.db"),
        install_root.join("data/node/config.toml"),
        install_root.join("data/node/logs/node.log"),
        install_root.join("node.exe"),
        install_root.join("src/main.rs"),
        install_root.join("target/debug/node.exe"),
        install_root.join("dist/release.zip"),
        install_root.join("portable.zip"),
        install_root.join("media/source.mp4"),
    ];
    for (path, bytes) in [
        (&contact, b"contact".as_slice()),
        (&preview, b"preview".as_slice()),
        (&orphan, b"partial".as_slice()),
        (&derivation, b"derivation".as_slice()),
        (&leased, b"leased".as_slice()),
        (&other_disk, b"other".as_slice()),
    ] {
        write_fixture(path, bytes);
    }
    for path in &protected {
        write_fixture(path, b"protected");
    }
    let media = install_root.join("media/video.mp4");
    write_fixture(&media, b"video");

    let registry = Arc::new(
        RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap(),
    );
    registry.register(&contact, ArtifactKind::ContactSheet).unwrap();
    registry.register(&preview, ArtifactKind::Preview).unwrap();
    registry
        .register(&orphan, ArtifactKind::OrphanTemporary)
        .unwrap();
    registry
        .register(&derivation, ArtifactKind::RegisteredDerivation)
        .unwrap();
    registry.register(&leased, ArtifactKind::Preview).unwrap();
    registry
        .register(&other_disk, ArtifactKind::RegisteredDerivation)
        .unwrap();
    for path in &protected {
        assert!(
            registry
                .register(path, ArtifactKind::RegisteredDerivation)
                .is_err(),
            "保护路径即使被误登记也必须拒绝: {}",
            path.display()
        );
    }
    assert!(
        registry
            .register(&media, ArtifactKind::RegisteredDerivation)
            .is_err(),
        "扫描媒体即使被误登记也必须拒绝"
    );
    let _active_lease = registry.lease(&leased).unwrap();

    let machine = MachineId::from_sha256([0x81; 32]);
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let scanned = ScannedPath::new(
        NormalizedPath::new(&media).unwrap(),
        DisplayPath::new(&media).unwrap(),
        fs::metadata(&media).unwrap().len(),
    );
    let content = store
        .upsert_content_and_location(&scanned, [0xaa; 16], MediaKind::Video)
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ContactSheet(
                "contact-sheets/aa/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.jpg".into(),
            ),
        )
        .unwrap();

    let cleaner = DiskFullCleaner::new(registry.clone(), FixtureDiskResolver);
    let write_target = cache_root.join("tmp/new-output.bin");
    fs::create_dir_all(write_target.parent().unwrap()).unwrap();
    let attempts = Cell::new(0);
    write_with_disk_full_cleanup(&cleaner, &mut store, &write_target, || {
        attempts.set(attempts.get() + 1);
        if attempts.get() == 1 {
            Err(io::Error::from_raw_os_error(112))
        } else {
            fs::write(&write_target, b"written")
        }
    })
    .unwrap();

    assert_eq!(attempts.get(), 2);
    for path in [&contact, &preview, &orphan, &derivation] {
        assert!(!path.exists(), "全部合格项都必须删除: {}", path.display());
    }
    assert!(leased.exists(), "活动 lease 必须排除");
    assert!(other_disk.exists(), "其他物理盘必须排除");
    for path in &protected {
        assert!(path.exists(), "未注册保护文件不得删除: {}", path.display());
    }
    assert!(media.exists(), "扫描媒体不得删除");
    assert_eq!(store.contact_sheet_path(content.id).unwrap(), None);
    let summary = cleaner.recent_summary().expect("应保存最近一次清理摘要");
    assert_eq!(summary.deleted_files, 4);
    assert_eq!(summary.deleted_bytes, 7 + 7 + 7 + 10);
    assert_eq!(summary.skipped_active, 1);
    assert_eq!(summary.skipped_other_disk, 1);
    assert_eq!(summary.failed_files, 0);
    assert!(summary.triggered_at_unix_ms > 0);
    assert!(
        store.page_file_faults(None, 10).unwrap().items.is_empty(),
        "清理运行摘要不得写入 file_faults"
    );
}

#[test]
fn disk_full_cleanup_retries_only_once_and_ignores_other_io_errors() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("mySingerServer");
    let cache_root = install_root.join("cache");
    fs::create_dir_all(&cache_root).unwrap();
    let registry = Arc::new(
        RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap(),
    );
    let first_cleanup = install_root.join("cache/derived/first.bin");
    write_fixture(&first_cleanup, b"first");
    registry
        .register(&first_cleanup, ArtifactKind::RegisteredDerivation)
        .unwrap();
    let cleaner = DiskFullCleaner::new(registry.clone(), FixtureDiskResolver);
    let machine = MachineId::from_sha256([0x82; 32]);
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let write_target = install_root.join("cache/derived/output.bin");
    let late = install_root.join("cache/derived/late.bin");
    let attempts = Cell::new(0);
    let error = write_with_disk_full_cleanup(&cleaner, &mut store, &write_target, || {
        attempts.set(attempts.get() + 1);
        if attempts.get() == 2 {
            write_fixture(&late, b"late");
            registry
                .register(&late, ArtifactKind::RegisteredDerivation)
                .unwrap();
        }
        Err::<(), _>(io::Error::from_raw_os_error(if attempts.get() == 1 {
            112
        } else {
            39
        }))
    })
    .unwrap_err();
    assert_eq!(error.raw_os_error(), Some(39));
    assert_eq!(attempts.get(), 2);
    assert!(!first_cleanup.exists());
    assert!(late.exists(), "第二次磁盘满不得再次触发清理");

    let untouched = install_root.join("cache/derived/untouched.bin");
    write_fixture(&untouched, b"untouched");
    let second_registry = Arc::new(
        RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap(),
    );
    second_registry
        .register(&untouched, ArtifactKind::RegisteredDerivation)
        .unwrap();
    let second_cleaner = DiskFullCleaner::new(second_registry, FixtureDiskResolver);
    let mut calls = 0;
    let error = write_with_disk_full_cleanup(
        &second_cleaner,
        &mut store,
        &write_target,
        || {
            calls += 1;
            Err::<(), _>(io::Error::from_raw_os_error(5))
        },
    )
    .unwrap_err();
    assert_eq!(error.raw_os_error(), Some(5));
    assert_eq!(calls, 1);
    assert!(untouched.exists(), "非磁盘满错误不得清理");
    assert!(second_cleaner.recent_summary().is_none());
}

#[test]
fn artifact_registry_rejects_paths_outside_the_absolute_install_root() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("mySingerServer");
    let cache_root = install_root.join("custom-cache");
    fs::create_dir_all(&cache_root).unwrap();
    let outside = fixture.path().join("outside.bin");
    write_fixture(&outside, b"outside");
    let registry = RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap();

    assert!(
        registry
            .register(Path::new("relative.bin"), ArtifactKind::Preview)
            .is_err()
    );
    assert!(registry.register(&outside, ArtifactKind::Preview).is_err());
    assert!(outside.exists());

    for (path, kind) in [
        (
            cache_root.join("contact-sheets/aa/sheet.jpg"),
            ArtifactKind::ContactSheet,
        ),
        (cache_root.join("previews/preview.bin"), ArtifactKind::Preview),
        (
            cache_root.join("tmp/write.partial"),
            ArtifactKind::OrphanTemporary,
        ),
        (
            cache_root.join("derived/result.bin"),
            ArtifactKind::RegisteredDerivation,
        ),
    ] {
        write_fixture(&path, b"artifact");
        registry
            .register(&path, kind)
            .expect("自定义 exact cache root 下固定子树应允许登记");
    }

    let decoy = install_root.join("foo/cache/contact-sheets/aa/decoy.jpg");
    write_fixture(&decoy, b"decoy");
    assert!(
        registry
            .register(&decoy, ArtifactKind::ContactSheet)
            .is_err(),
        "安装根内另一个同名 cache 子树不能冒充 exact cache root"
    );
    for name in ["node.db-wal", "node.db-shm", "node.db-journal", "node.sqlite-wal"] {
        let sidecar = cache_root.join("derived").join(name);
        write_fixture(&sidecar, b"database-sidecar");
        assert!(
            registry
                .register(&sidecar, ArtifactKind::RegisteredDerivation)
                .is_err(),
            "所有数据库 sidecar 都必须硬拒绝: {name}"
        );
    }

    let external_cache = fixture.path().join("external-cache");
    fs::create_dir_all(&external_cache).unwrap();
    assert!(
        RegenerableArtifactRegistry::new(&install_root, &external_cache).is_err(),
        "cache_root 位于 install_root 外必须明确拒绝"
    );
}

#[test]
fn artifact_registry_tracks_a_planned_partial_before_the_first_write() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("mySingerServer");
    let cache_root = install_root.join("data/node/custom-cache");
    let partial = cache_root.join("tmp/new.partial");
    fs::create_dir_all(partial.parent().unwrap()).unwrap();
    let registry = RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap();

    registry
        .register(&partial, ArtifactKind::OrphanTemporary)
        .expect("磁盘满前必须能先登记尚未落盘的专用 partial");
    write_fixture(&partial, b"partial");
    let _lease = registry.lease(&partial).unwrap();
}

#[test]
fn planned_partial_lease_blocks_external_cleanup_and_releases_only_for_its_own_disk_full() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("mySingerServer");
    let cache_root = install_root.join("custom-cache");
    fs::create_dir_all(&cache_root).unwrap();
    let registry = Arc::new(
        RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap(),
    );
    let cleaner = DiskFullCleaner::new(Arc::clone(&registry), FixtureDiskResolver);
    let database = install_root.join("data/node.db");
    fs::create_dir_all(database.parent().unwrap()).unwrap();
    let machine = MachineId::from_sha256([0x84; 32]);
    let mut cleanup_store = NodeStore::open(&database, machine.clone()).unwrap();
    let partial = cache_root.join("tmp/blocked.partial");
    fs::create_dir_all(partial.parent().unwrap()).unwrap();
    let entered = Arc::new(Barrier::new(2));
    let release = Arc::new(Barrier::new(2));
    let writer_registry = Arc::clone(&registry);
    let writer_cleaner = cleaner.clone();
    let writer_database = database.clone();
    let writer_machine = machine.clone();
    let writer_partial = partial.clone();
    let writer_entered = Arc::clone(&entered);
    let writer_release = Arc::clone(&release);
    let writer = thread::spawn(move || {
        let mut store = NodeStore::open(&writer_database, writer_machine).unwrap();
        write_planned_artifact_with_disk_full_cleanup(
            &writer_cleaner,
            &mut store,
            &writer_registry,
            &writer_partial,
            ArtifactKind::OrphanTemporary,
            || {
                write_fixture(&writer_partial, b"writing");
                writer_entered.wait();
                writer_release.wait();
                Ok(())
            },
        )
        .unwrap()
    });

    entered.wait();
    let trigger = cache_root.join("derived/trigger.bin");
    fs::create_dir_all(trigger.parent().unwrap()).unwrap();
    let mut trigger_calls = 0;
    write_with_disk_full_cleanup(&cleaner, &mut cleanup_store, &trigger, || {
        trigger_calls += 1;
        if trigger_calls == 1 {
            Err(io::Error::from_raw_os_error(112))
        } else {
            Ok(())
        }
    })
    .unwrap();
    assert!(partial.exists(), "外部清理不能删除正在 write/flush/sync 的 partial");
    assert_eq!(cleaner.recent_summary().unwrap().skipped_active, 1);
    release.wait();
    let (_, active_lease) = writer.join().unwrap();
    drop(active_lease);

    let retry_partial = cache_root.join("tmp/retry.partial");
    let retry_calls = Cell::new(0);
    let (_, retry_lease) = write_planned_artifact_with_disk_full_cleanup(
        &cleaner,
        &mut cleanup_store,
        &registry,
        &retry_partial,
        ArtifactKind::OrphanTemporary,
        || {
            retry_calls.set(retry_calls.get() + 1);
            if retry_calls.get() == 1 {
                write_fixture(&retry_partial, b"first-partial");
                Err(io::Error::from_raw_os_error(39))
            } else {
                assert!(
                    !retry_partial.exists(),
                    "自身磁盘满必须先 drop lease，允许完整清理删掉 partial"
                );
                write_fixture(&retry_partial, b"retry-success");
                Ok(())
            }
        },
    )
    .unwrap();
    assert_eq!(retry_calls.get(), 2);
    assert_eq!(fs::read(&retry_partial).unwrap(), b"retry-success");
    drop(retry_lease);

    let recent_before_other_error = cleaner.recent_summary();
    let other_error_partial = cache_root.join("tmp/other-error.partial");
    let error = write_planned_artifact_with_disk_full_cleanup(
        &cleaner,
        &mut cleanup_store,
        &registry,
        &other_error_partial,
        ArtifactKind::OrphanTemporary,
        || {
            write_fixture(&other_error_partial, b"other-error");
            Err::<(), _>(io::Error::from_raw_os_error(5))
        },
    )
    .unwrap_err();
    assert_eq!(error.raw_os_error(), Some(5));
    assert!(other_error_partial.exists());
    assert_eq!(cleaner.recent_summary(), recent_before_other_error);
}

fn write_fixture(path: &Path, bytes: &[u8]) {
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, bytes).unwrap();
}
