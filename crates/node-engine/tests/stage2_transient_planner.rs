//! 二筛瞬态规划器的缓存分类和物理盘 lane 行为测试。

use dedup_core::{ContentKey, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_engine::analysis::stage2_planner::{
    Stage2ActiveSource, Stage2PlanAction, Stage2PlanError, Stage2PlanningInput,
    Stage2TransientPlanner,
};
use dedup_node_engine::scan::TaskDiskLane;
use dedup_node_store::{BaseCacheRecord, CompleteStage1, ContentId, NodeStore, ScannedPath};
use dedup_windows::{LocalDiskKind, PhysicalDiskId};

const MACHINE_BYTES: [u8; 32] = [0x31; 32];

fn machine() -> MachineId {
    MachineId::from_sha256(MACHINE_BYTES)
}

fn lane(numbers: &[u32], kind: LocalDiskKind, limit: usize) -> TaskDiskLane {
    TaskDiskLane {
        physical_disk_id: PhysicalDiskId::from_disk_numbers(numbers.iter().copied()).unwrap(),
        physical_disk_numbers: numbers.to_vec(),
        disk_kind: kind,
        configured_weight: limit,
        per_disk_limit: limit,
    }
}

fn scanned(path: &str, size: u64) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        size,
    )
}

fn active_source(
    content: ContentKey,
    content_id: ContentId,
    path: &str,
    media_kind: MediaKind,
    frame_slots: Vec<u8>,
    task_lane: TaskDiskLane,
) -> Stage2ActiveSource {
    let scanned = scanned(path, content.file_size());
    let location = LocationKey::new(machine(), scanned.normalized_path.clone());
    Stage2ActiveSource {
        content,
        content_id,
        location,
        scanned,
        media_kind,
        frame_slots,
        lane: task_lane,
    }
}

fn input(active: Stage2ActiveSource) -> Stage2PlanningInput {
    Stage2PlanningInput::from_active(active)
}

fn image_stage1() -> ImageStage1 {
    ImageStage1 {
        width: 1920,
        height: 1080,
        pdq: PdqHash::from_bytes([0x22; 32]),
        quality: 80,
    }
}

fn stage2(seed: u64) -> ImageStage2 {
    ImageStage2 {
        phash_parts: [seed; 9],
        sobel: [seed as f32; 128],
    }
}

fn image_record(
    content: ContentKey,
    content_id: ContentId,
    image_stage2: Option<ImageStage2>,
) -> BaseCacheRecord {
    BaseCacheRecord {
        content_id: Some(content_id),
        content_key: content,
        media_kind: MediaKind::Image,
        base_complete: true,
        width: Some(1920),
        height: Some(1080),
        duration_ms: None,
        stage1: Some(CompleteStage1::Image(image_stage1())),
        image_stage2,
        video_stage2: Box::new([None; 6]),
        contact_sheet_relative_path: None,
    }
}

fn video_record(
    content: ContentKey,
    content_id: ContentId,
    stage2_slots: &[(u8, ImageStage2)],
    base_complete: bool,
) -> BaseCacheRecord {
    let stage1 = std::array::from_fn(|slot| (slot < 4).then_some(image_stage1()));
    let mut video_stage2 = Box::new([None; 6]);
    for (slot, feature) in stage2_slots {
        video_stage2[usize::from(*slot)] = Some(*feature);
    }
    BaseCacheRecord {
        content_id: Some(content_id),
        content_key: content,
        media_kind: MediaKind::Video,
        base_complete,
        width: Some(3840),
        height: Some(2160),
        duration_ms: Some(60_000),
        stage1: Some(CompleteStage1::Video(Box::new(stage1))),
        image_stage2: None,
        video_stage2,
        contact_sheet_relative_path: None,
    }
}

fn add_content(
    store: &mut NodeStore,
    path: &str,
    size: u64,
    md5: [u8; 16],
) -> (ContentKey, ContentId) {
    let content = store
        .upsert_content_and_location(&scanned(path, size), md5, MediaKind::Image)
        .unwrap();
    (content.key, content.id)
}

#[test]
fn freeze_rejects_duplicate_content_and_non_active_source_before_cache_batch() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let (content, content_id) = add_content(&mut store, r"C:\image.jpg", 10, [1; 16]);
    let active = active_source(
        content,
        content_id,
        r"C:\image.jpg",
        MediaKind::Image,
        Vec::new(),
        lane(&[7], LocalDiskKind::Ssd, 5),
    );
    let duplicate = Stage2TransientPlanner::freeze(&[input(active.clone()), input(active)])
        .expect_err("重复 content 必须在缓存查询前拒绝");
    assert!(matches!(
        duplicate,
        Stage2PlanError::DuplicateContent { .. }
    ));

    let mut mismatched = input(active_source(
        content,
        content_id,
        r"C:\image.jpg",
        MediaKind::Image,
        Vec::new(),
        lane(&[7], LocalDiskKind::Ssd, 5),
    ));
    mismatched.requested_source =
        LocationKey::new(machine(), NormalizedPath::new(r"C:\other.jpg").unwrap());
    let error = Stage2TransientPlanner::freeze(&[mismatched]).unwrap_err();
    assert!(matches!(error, Stage2PlanError::SourceIsNotActive));
}

#[test]
fn plan_uses_aligned_batches_and_only_schedules_true_missing_fields() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let (image_hit, image_hit_id) = add_content(&mut store, r"C:\hit.jpg", 10, [2; 16]);
    let (image_miss, image_miss_id) = add_content(&mut store, r"C:\miss.jpg", 11, [3; 16]);
    let (video, video_id) = add_content(&mut store, r"C:\clip.mp4", 12, [4; 16]);
    let (incomplete, incomplete_id) = add_content(&mut store, r"C:\incomplete.mp4", 13, [5; 16]);

    let sources = vec![
        active_source(
            image_hit,
            image_hit_id,
            r"C:\hit.jpg",
            MediaKind::Image,
            Vec::new(),
            lane(&[7], LocalDiskKind::Ssd, 5),
        ),
        active_source(
            image_miss,
            image_miss_id,
            r"C:\miss.jpg",
            MediaKind::Image,
            Vec::new(),
            lane(&[8], LocalDiskKind::Hdd, 1),
        ),
        active_source(
            video,
            video_id,
            r"C:\clip.mp4",
            MediaKind::Video,
            vec![0, 1, 2, 3],
            lane(&[8], LocalDiskKind::Hdd, 1),
        ),
        active_source(
            incomplete,
            incomplete_id,
            r"C:\incomplete.mp4",
            MediaKind::Video,
            vec![0, 1],
            lane(&[9], LocalDiskKind::Unknown, 1),
        ),
    ];
    let frozen =
        Stage2TransientPlanner::freeze(&sources.into_iter().map(input).collect::<Vec<_>>())
            .unwrap();

    let local = vec![
        Some(image_record(image_hit, image_hit_id, Some(stage2(1)))),
        Some(image_record(image_miss, image_miss_id, None)),
        Some(video_record(video, video_id, &[(0, stage2(10))], true)),
        Some(video_record(incomplete, incomplete_id, &[], false)),
    ];
    let remote = vec![
        Some(dedup_node_store::CompleteStage2::Image(Box::new(stage2(2)))),
        None,
        Some(dedup_node_store::CompleteStage2::Video(Box::new(
            std::array::from_fn(|slot| (slot < 2).then_some(stage2(20 + slot as u64))),
        ))),
        Some(dedup_node_store::CompleteStage2::Video(Box::new(
            std::array::from_fn(|_| Some(stage2(99))),
        ))),
    ];

    let plan = Stage2TransientPlanner::plan(&frozen, &local, Some(&remote)).unwrap();
    assert_eq!(plan.items().len(), 4);

    assert!(matches!(
        &plan.items()[0].actions()[..],
        [Stage2PlanAction::RepublishLocal {
            selection: dedup_node_engine::analysis::stage2_planner::Stage2Selection::Image
        }]
    ));
    assert!(matches!(
        &plan.items()[1].actions()[..],
        [Stage2PlanAction::Compute(work)]
            if work.selection().is_image()
    ));
    assert!(matches!(
        &plan.items()[2].actions()[..],
        [
            Stage2PlanAction::RepublishLocal { .. },
            Stage2PlanAction::ImportRemote { .. },
            Stage2PlanAction::Compute(work),
        ] if work.selection().video_slots() == 0b1100
    ));
    assert!(matches!(
        &plan.items()[3].actions()[..],
        [Stage2PlanAction::IncompleteBase]
    ));
}

#[test]
fn local_complete_ignores_remote_and_work_item_keeps_frozen_lane() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let (content, content_id) = add_content(&mut store, r"H:\hit.jpg", 20, [6; 16]);
    let task_lane = lane(&[12], LocalDiskKind::Unknown, 2);
    let active = active_source(
        content,
        content_id,
        r"H:\hit.jpg",
        MediaKind::Image,
        Vec::new(),
        task_lane.clone(),
    );
    let frozen = Stage2TransientPlanner::freeze(&[input(active)]).unwrap();
    let local = vec![Some(image_record(content, content_id, Some(stage2(43))))];
    let remote = vec![Some(dedup_node_store::CompleteStage2::Image(Box::new(
        stage2(44),
    )))];
    let plan = Stage2TransientPlanner::plan(&frozen, &local, Some(&remote)).unwrap();
    assert!(matches!(
        &plan.items()[0].actions()[..],
        [Stage2PlanAction::RepublishLocal {
            selection: dedup_node_engine::analysis::stage2_planner::Stage2Selection::Image
        }]
    ));

    let missing = Stage2TransientPlanner::plan(
        &Stage2TransientPlanner::freeze(&[input(active_source(
            content,
            content_id,
            r"H:\hit.jpg",
            MediaKind::Image,
            Vec::new(),
            task_lane.clone(),
        ))])
        .unwrap(),
        &[Some(image_record(content, content_id, None))],
        None,
    )
    .unwrap();
    let Stage2PlanAction::Compute(work) = &missing.items()[0].actions()[0] else {
        panic!("本地缺失且远端不可用时应生成一个 Worker 工作项");
    };
    assert_eq!(work.source().lane, task_lane);
    assert_eq!(work.task_record().missing.bits(), 1 << 4);
}

#[test]
fn complete_video_cache_hit_republishes_existing_slots_without_compute() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let (content, content_id) = add_content(&mut store, r"H:\complete.mp4", 24, [7; 16]);
    let active = active_source(
        content,
        content_id,
        r"H:\complete.mp4",
        MediaKind::Video,
        vec![0, 1, 2, 3],
        lane(&[12], LocalDiskKind::Hdd, 1),
    );
    let frozen = Stage2TransientPlanner::freeze(&[input(active)]).unwrap();
    let local = vec![Some(video_record(
        content,
        content_id,
        &[
            (0, stage2(40)),
            (1, stage2(41)),
            (2, stage2(42)),
            (3, stage2(43)),
        ],
        true,
    ))];

    let plan = Stage2TransientPlanner::plan(&frozen, &local, None).unwrap();

    assert!(matches!(
        &plan.items()[0].actions()[..],
        [Stage2PlanAction::RepublishLocal {
            selection: dedup_node_engine::analysis::stage2_planner::Stage2Selection::VideoSlots(
                slots
            )
        }] if *slots == 0b1111
    ));
    assert_eq!(plan.worker_items().len(), 0);
}
