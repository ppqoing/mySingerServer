//! 三类计算任务跨 SQLite 重启复用基础特征、联系表和二次特征的组合门禁。

use dedup_core::{DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath, Thresholds};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_engine::{
    analysis::{LocalAnalysisEngine, Stage2Processor, Stage2Request},
    scan::BaseComputeDecision,
    worker::Stage2Output,
};
use dedup_node_store::{
    AnalysisStatus, FeatureWrite, GroupKind, NewTaskItem, NodeStore, ScannedPath,
    TaskItemCompletion, VideoFrameStage1Fields, VideoFrameStage2Fields, VideoMetadataFields,
};

/// 记录重复文件清单是否仍向 Worker 派发二次计算。
#[derive(Default)]
struct RejectingStage2 {
    /// 实际收到的二次计算请求数。
    calls: usize,
}

impl Stage2Processor for RejectingStage2 {
    async fn process(&mut self, _request: Stage2Request) -> Result<Stage2Output, String> {
        self.calls += 1;
        Err("完整二次缓存不应再次派发 Worker".into())
    }
}

/// 基础缓存、联系表、二次缓存和最终清单在重启后都应直接复用。
#[tokio::test]
async fn three_task_pipeline_reuses_base_and_stage2_results_across_restart() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("node.db");
    let contact_root = directory.path().join("cache/contact-sheets");
    let machine_id = MachineId::from_sha256([0xa7; 32]);
    let scan_task;
    let first_group_shape;

    {
        let mut store = NodeStore::open(&database, machine_id.clone()).unwrap();
        let left = seed_cached_video(&mut store, &contact_root, r"D:\Media\left.mp4", [0x11; 16]);
        let right = seed_cached_video(&mut store, &contact_root, r"D:\Media\right.mp4", [0x22; 16]);
        scan_task = completed_base_task(&mut store, &[left, right], 100);
        let mut stage2 = RejectingStage2::default();
        let report = LocalAnalysisEngine::start(
            &mut store,
            &[scan_task],
            Thresholds::default(),
            &mut stage2,
            200,
        )
        .await
        .unwrap();

        assert_eq!(stage2.calls, 0, "首次清单也应复用已经存在的二次特征");
        assert_eq!(report.status, AnalysisStatus::Completed);
        first_group_shape = group_shape(&store, report.run_id);
    }

    let mut reopened = NodeStore::open(&database, machine_id).unwrap();
    let scanned = [
        scanned(r"D:\Media\left.mp4"),
        scanned(r"D:\Media\right.mp4"),
    ];
    let path_hits = reopened.lookup_scanned_paths(&scanned).unwrap();
    for hit in &path_hits {
        let content_id = hit.content_id.expect("完整路径缓存必须指向内容");
        let cached = reopened.load_base_cache_record(content_id).unwrap();
        assert!(cached.base_complete);
        let contact = reopened
            .contact_sheet_path(content_id)
            .unwrap()
            .expect("视频基础缓存必须保留联系表引用");
        assert!(directory.path().join("cache").join(contact).is_file());
        assert_eq!(
            BaseComputeDecision::for_cache(Some(&cached), true, false).missing_parts(),
            0,
            "重启后基础任务不得重新派发 MD5、缩略图或一筛"
        );
        assert!(reopened.load_complete_stage2(content_id).unwrap().is_some());
    }

    let mut stage2 = RejectingStage2::default();
    let second = LocalAnalysisEngine::start(
        &mut reopened,
        &[scan_task],
        Thresholds::default(),
        &mut stage2,
        300,
    )
    .await
    .unwrap();
    assert_eq!(stage2.calls, 0, "重启后的清单不得重复派发二次特征");
    assert_eq!(second.status, AnalysisStatus::Completed);
    assert_eq!(group_shape(&reopened, second.run_id), first_group_shape);
}

/// 写入一个基础、一筛、联系表和二次特征都完整的视频内容。
fn seed_cached_video(
    store: &mut NodeStore,
    contact_root: &std::path::Path,
    path: &str,
    md5: [u8; 16],
) -> (ScannedPath, LocationKey, dedup_node_store::ContentId) {
    let scanned = scanned(path);
    let content = store
        .upsert_content_and_location(&scanned, md5, MediaKind::Video)
        .unwrap();
    store.mark_base_complete(content.id).unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::VideoMetadata(VideoMetadataFields {
                duration_ms: Some(12_000),
                width: Some(1920),
                height: Some(1080),
            }),
        )
        .unwrap();
    for slot in 0..6 {
        let stage1 = stage1();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                    slot,
                    time_ms: u64::from(slot) * 2_000 + 1_000,
                    decoded: true,
                    width: Some(stage1.width),
                    height: Some(stage1.height),
                    pdq: Some(stage1.pdq),
                    quality: Some(stage1.quality),
                }),
            )
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                    slot,
                    features: stage2(),
                }),
            )
            .unwrap();
    }
    let digest = md5
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let relative = format!("contact-sheets/{}/{}.jpg", &digest[..2], digest);
    let target = contact_root
        .join(&digest[..2])
        .join(format!("{digest}.jpg"));
    std::fs::create_dir_all(target.parent().unwrap()).unwrap();
    std::fs::write(target, b"cached-contact-sheet").unwrap();
    store
        .commit_feature_result(content.id, None, FeatureWrite::ContactSheet(relative))
        .unwrap();
    let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path.clone());
    (scanned, location, content.id)
}

/// 创建完成态基础任务，让重复文件清单以稳定成功项冻结输入。
fn completed_base_task(
    store: &mut NodeStore,
    contents: &[(ScannedPath, LocationKey, dedup_node_store::ContentId)],
    now_ms: i64,
) -> dedup_core::TaskId {
    let items = contents
        .iter()
        .map(|(scanned, location, content_id)| {
            NewTaskItem::for_content(
                location.clone(),
                scanned.display_path.clone(),
                scanned.file_size,
                *content_id,
                "compute_base_features",
            )
        })
        .collect::<Vec<_>>();
    let task = store.create_task("base_compute", &items, now_ms).unwrap();
    while let Some(item) = store.claim_next_item(task, now_ms).unwrap() {
        store
            .complete_item(
                &item.item_id,
                TaskItemCompletion::Succeeded {
                    content_id: item.content_id,
                },
                now_ms,
            )
            .unwrap();
    }
    task
}

/// 返回最终组类型和成员数，忽略每次运行重新生成的 ID。
fn group_shape(store: &NodeStore, run_id: dedup_core::AnalysisRunId) -> Vec<(GroupKind, usize)> {
    store
        .page_groups(run_id, None, 20)
        .unwrap()
        .items
        .into_iter()
        .map(|group| {
            let count = store
                .page_group_members(run_id, &group.group_id, None, 20)
                .unwrap()
                .items
                .len();
            (group.kind, count)
        })
        .collect()
}

/// 构造固定的视频路径缓存输入。
fn scanned(path: &str) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        1_024,
    )
}

/// 构造让两个视频稳定进入候选的一筛特征。
fn stage1() -> ImageStage1 {
    ImageStage1 {
        width: 100,
        height: 100,
        pdq: PdqHash::from_bytes([0; 32]),
        quality: 100,
    }
}

/// 构造让两个候选稳定通过精准判重的二次特征。
fn stage2() -> ImageStage2 {
    ImageStage2 {
        phash_parts: [0; 9],
        sobel: [0.0; 128],
    }
}
