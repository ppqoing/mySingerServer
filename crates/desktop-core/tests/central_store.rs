use dedup_core::{
    ContentKey, DeleteMode, DisplayPath, MachineId, MediaKind, NormalizedPath, TaskId, Thresholds,
};
use dedup_desktop_core::central::{
    CentralAnalysisInput, CentralAnalysisNode, CentralCandidate, CentralCandidateStatus,
    CentralDeleteOutcome, CentralDeleteResult, CentralDeleteSelection, CentralGroupKind,
    CentralGroupMember, CentralGroupWrite, CentralPairKind, CentralReviewDecision, CentralStore,
};
use dedup_desktop_core::sync::SNAPSHOT_TABLES;
use dedup_media::{ImageStage2, PdqHash};
use dedup_node_store::{
    AnalysisMode, ConfirmedDeleteItem, DeleteOutcome, DeleteResult, FeatureWrite, GroupKind,
    GroupMemberWrite, GroupWrite, ImageStage1Fields, NodeStore, ReviewDecision, ScannedPath,
    VideoFrameStage1Fields, VideoFrameStage2Fields, VideoMetadataFields,
};

#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn sync_uses_content_key_globally_and_location_key_per_machine() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let mut central = CentralStore::connect(&url).await.unwrap();
    let machine_a = MachineId::parse(&"a1".repeat(32)).unwrap();
    let machine_b = MachineId::parse(&"b2".repeat(32)).unwrap();
    let shared_md5 = [0x44; 16];

    let batch_a = node_batch(&machine_a, r"C:\A\same.bin", 100, shared_md5);
    let batch_b = node_batch(&machine_b, r"D:\B\same.bin", 100, shared_md5);
    let different_size = node_batch(&machine_a, r"C:\A\other.bin", 101, shared_md5);
    central
        .apply_sync_batch(&machine_a, &batch_a)
        .await
        .unwrap();
    central
        .apply_sync_batch(&machine_b, &batch_b)
        .await
        .unwrap();
    central
        .apply_sync_batch(&machine_a, &different_size)
        .await
        .unwrap();

    assert_eq!(central.content_count(shared_md5).await.unwrap(), 2);
    assert_eq!(central.location_count(shared_md5, 100).await.unwrap(), 2);
}

#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn analysis_groups_use_stable_pages_and_successful_delete_shrinks_group() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let mut central = CentralStore::connect(&url).await.unwrap();
    let machine_c = MachineId::parse(&"c3".repeat(32)).unwrap();
    let machine_d = MachineId::parse(&"d4".repeat(32)).unwrap();
    let exact = ContentKey::new([0x10; 16], 210);
    let image_left = ContentKey::new([0x20; 16], 220);
    let image_right = ContentKey::new([0x30; 16], 230);
    let exact_c = location(machine_c.clone(), r"C:\CentralTest\exact-c.bin");
    let exact_d = location(machine_d.clone(), r"D:\CentralTest\exact-d.bin");
    let image_c = location(machine_c.clone(), r"C:\CentralTest\image-c.jpg");
    let image_d = location(machine_d.clone(), r"D:\CentralTest\image-d.jpg");

    sync_location(&mut central, &exact_c, exact).await;
    sync_location(&mut central, &exact_d, exact).await;
    sync_location(&mut central, &image_c, image_left).await;
    sync_location(&mut central, &image_d, image_right).await;

    let run_id = central
        .create_analysis_run(
            &Thresholds::default(),
            &[
                analysis_node(machine_c.clone()),
                analysis_node(machine_d.clone()),
            ],
        )
        .await
        .unwrap();
    let inputs = [
        analysis_input(exact, exact_c.clone()),
        analysis_input(exact, exact_d.clone()),
        analysis_input(image_left, image_c.clone()),
        analysis_input(image_right, image_d.clone()),
    ];
    central
        .insert_analysis_inputs(run_id, &inputs)
        .await
        .unwrap();
    central
        .replace_candidates(
            run_id,
            &[CentralCandidate {
                kind: CentralPairKind::Image,
                left: image_left,
                right: image_right,
                stage1_score: 0.91,
                phash_passed_parts: Some(9),
                stage2_score: Some(0.93),
                status: CentralCandidateStatus::Passed,
            }],
        )
        .await
        .unwrap();

    let exact_group = format!("exact-{}", run_id.as_uuid());
    let image_group = format!("image-{}", run_id.as_uuid());
    central
        .replace_groups(
            run_id,
            &[
                CentralGroupWrite {
                    group_id: exact_group.clone(),
                    kind: CentralGroupKind::Exact,
                    representative: exact,
                    members: vec![
                        group_member(exact_c.clone(), exact, true),
                        group_member(exact_d.clone(), exact, false),
                    ],
                },
                CentralGroupWrite {
                    group_id: image_group,
                    kind: CentralGroupKind::Image,
                    representative: image_left,
                    members: vec![
                        group_member(image_c, image_left, true),
                        group_member(image_d, image_right, false),
                    ],
                },
            ],
        )
        .await
        .unwrap();

    let first = central.page_groups(run_id, None, 1).await.unwrap();
    assert_eq!(first.items.len(), 1);
    assert_eq!(first.items[0].kind, CentralGroupKind::Exact);
    let second = central
        .page_groups(run_id, first.next_cursor.as_deref(), 1)
        .await
        .unwrap();
    assert_eq!(second.items.len(), 1);
    assert_eq!(second.items[0].kind, CentralGroupKind::Image);

    central
        .save_review_mark(run_id, &exact_group, &exact_c, CentralReviewDecision::Keep)
        .await
        .unwrap();
    central
        .save_review_mark(
            run_id,
            &exact_group,
            &exact_d,
            CentralReviewDecision::Delete,
        )
        .await
        .unwrap();
    let plan = central
        .create_delete_plan(
            run_id,
            &[CentralDeleteSelection::new(
                exact_group.clone(),
                exact_d.clone(),
                exact,
            )],
            DeleteMode::RecycleBin,
        )
        .await
        .unwrap();
    assert_eq!(plan.items.len(), 1);
    central
        .apply_delete_results(
            &plan.batch_id,
            &[CentralDeleteResult {
                item_id: plan.items[0].item_id.clone(),
                outcome: CentralDeleteOutcome::Recycled,
                message: None,
            }],
        )
        .await
        .unwrap();
    let remaining = central.page_groups(run_id, None, 10).await.unwrap();
    assert_eq!(remaining.items.len(), 1);
    assert_eq!(remaining.items[0].kind, CentralGroupKind::Image);
}

#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn central_results_follow_incremental_and_snapshot_location_activity() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let machine = unique_machine(0x27);
    let content = ContentKey::new([0x81; 16], 810);
    let paths = [
        r"C:\CurrentResults\a.bin",
        r"C:\CurrentResults\b.bin",
        r"C:\CurrentResults\c.bin",
    ];
    let locations = paths.map(|path| location(machine.clone(), path));
    let mut node = NodeStore::open_in_memory(machine.clone()).unwrap();
    for path in paths {
        add_content(
            &mut node,
            path,
            content.file_size(),
            content.md5(),
            MediaKind::Other,
        );
    }
    let initial = node.pull_changes(0, 1000).unwrap();
    let initial_highwater = initial.high_seq;
    let mut central = CentralStore::connect(&url).await.unwrap();
    central
        .apply_sync_batch(
            &machine,
            &dedup_protocol::proto::SyncChangeBatch {
                changes: initial.changes,
                high_seq: initial.high_seq,
                pruned_through_seq: initial.pruned_through_seq,
            },
        )
        .await
        .unwrap();
    let run = central
        .create_analysis_run(&Thresholds::default(), &[analysis_node(machine.clone())])
        .await
        .unwrap();
    central
        .insert_analysis_inputs(
            run,
            &locations
                .iter()
                .cloned()
                .map(|location| analysis_input(content, location))
                .collect::<Vec<_>>(),
        )
        .await
        .unwrap();
    let group_id = format!("current-results-{}", run.as_uuid());
    central
        .replace_groups(
            run,
            &[CentralGroupWrite {
                group_id: group_id.clone(),
                kind: CentralGroupKind::Exact,
                representative: content,
                members: locations
                    .iter()
                    .cloned()
                    .enumerate()
                    .map(|(index, location)| group_member(location, content, index == 0))
                    .collect(),
            }],
        )
        .await
        .unwrap();

    let scan = node
        .create_scan_task(&[NormalizedPath::new(r"C:\CurrentResults").unwrap()], 1)
        .unwrap();
    node.finalize_scan_task(
        scan,
        &[
            NormalizedPath::new(paths[0]).unwrap(),
            NormalizedPath::new(paths[2]).unwrap(),
        ],
        2,
    )
    .unwrap();
    let incremental = node.pull_changes(initial_highwater, 1000).unwrap();
    central
        .apply_sync_batch(
            &machine,
            &dedup_protocol::proto::SyncChangeBatch {
                changes: incremental.changes,
                high_seq: incremental.high_seq,
                pruned_through_seq: incremental.pruned_through_seq,
            },
        )
        .await
        .unwrap();

    let group = central
        .page_groups(run, None, 10)
        .await
        .unwrap()
        .items
        .remove(0);
    assert_eq!(group.member_count, 2);
    assert_eq!(group.reclaimable_bytes, 810);
    let members = central
        .page_group_members(run, &group_id, None, 10)
        .await
        .unwrap()
        .items;
    assert_eq!(
        members
            .iter()
            .map(|member| member.active)
            .collect::<Vec<_>>(),
        [true, false, true]
    );

    let mut fresh = NodeStore::open_in_memory(machine.clone()).unwrap();
    add_content(
        &mut fresh,
        paths[2],
        content.file_size(),
        content.md5(),
        MediaKind::Other,
    );
    for seed in 0x91..=0x93 {
        add_content(
            &mut fresh,
            &format!(r"C:\CurrentResults\padding-{seed}.bin"),
            u64::from(seed),
            [seed; 16],
            MediaKind::Other,
        );
    }
    let snapshot = fresh.begin_snapshot().unwrap();
    let mut writer = central
        .begin_snapshot_replace(&machine, snapshot.high_seq())
        .await
        .unwrap();
    for table in SNAPSHOT_TABLES {
        let page = snapshot.read_page(table, "", 1000).unwrap();
        let rows = page
            .rows
            .into_iter()
            .map(|row| row.payload)
            .collect::<Vec<_>>();
        writer.apply_page(table, &rows).await.unwrap();
    }
    writer.commit().await.unwrap();

    assert!(
        central
            .page_groups(run, None, 10)
            .await
            .unwrap()
            .items
            .is_empty()
    );
    let members = central
        .page_group_members(run, &group_id, None, 10)
        .await
        .unwrap()
        .items;
    assert_eq!(
        members
            .iter()
            .map(|member| member.active)
            .collect::<Vec<_>>(),
        [false, false, true]
    );
}

#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn central_delete_plan_never_expands_beyond_confirmed_locations() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let mut central = CentralStore::connect(&url).await.unwrap();
    let keep_machine = unique_machine(0x31);
    let confirmed_machine = unique_machine(0x32);
    let late_machine = unique_machine(0x33);
    let content = ContentKey::new([0x71; 16], 710);
    let keep = location(keep_machine.clone(), r"C:\Freeze\keep.bin");
    let confirmed = location(confirmed_machine.clone(), r"D:\Freeze\confirmed.bin");
    let late = location(late_machine.clone(), r"E:\Freeze\late.bin");
    for location in [&keep, &confirmed, &late] {
        sync_location(&mut central, location, content).await;
    }
    let run = central
        .create_analysis_run(
            &Thresholds::default(),
            &[
                analysis_node(keep_machine),
                analysis_node(confirmed_machine),
                analysis_node(late_machine),
            ],
        )
        .await
        .unwrap();
    central
        .insert_analysis_inputs(
            run,
            &[
                analysis_input(content, keep.clone()),
                analysis_input(content, confirmed.clone()),
                analysis_input(content, late.clone()),
            ],
        )
        .await
        .unwrap();
    let group_id = format!("central-freeze-{}", run.as_uuid());
    central
        .replace_groups(
            run,
            &[CentralGroupWrite {
                group_id: group_id.clone(),
                kind: CentralGroupKind::Exact,
                representative: content,
                members: vec![
                    group_member(keep.clone(), content, true),
                    group_member(confirmed.clone(), content, false),
                    group_member(late.clone(), content, false),
                ],
            }],
        )
        .await
        .unwrap();
    central
        .save_review_mark(run, &group_id, &keep, CentralReviewDecision::Keep)
        .await
        .unwrap();
    central
        .save_review_mark(run, &group_id, &confirmed, CentralReviewDecision::Delete)
        .await
        .unwrap();
    let frozen = CentralDeleteSelection::new(group_id.clone(), confirmed.clone(), content);
    central
        .save_review_mark(run, &group_id, &late, CentralReviewDecision::Delete)
        .await
        .unwrap();

    let plan = central
        .create_delete_plan(run, &[frozen], DeleteMode::RecycleBin)
        .await
        .unwrap();

    assert_eq!(plan.items.len(), 1);
    assert_eq!(plan.items[0].location, confirmed);
}

#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn incremental_sync_covers_features_and_tombstones_but_not_contact_sheets() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let machine = MachineId::parse(&"f7".repeat(32)).unwrap();
    let mut node = NodeStore::open_in_memory(machine.clone()).unwrap();
    let image = add_content(
        &mut node,
        r"C:\SyncScope\image.jpg",
        310,
        [0x61; 16],
        MediaKind::Image,
    );
    node.commit_feature_result(
        image.id,
        None,
        FeatureWrite::ImageStage1(ImageStage1Fields {
            width: Some(640),
            height: Some(480),
            pdq: Some(PdqHash::from_bytes([0x62; 32])),
            quality: Some(90),
        }),
    )
    .unwrap();
    node.commit_feature_result(image.id, None, FeatureWrite::ImageStage2(stage2(0x63)))
        .unwrap();

    let video = add_content(
        &mut node,
        r"C:\SyncScope\video.mp4",
        320,
        [0x64; 16],
        MediaKind::Video,
    );
    node.commit_feature_result(
        video.id,
        None,
        FeatureWrite::VideoMetadata(VideoMetadataFields {
            duration_ms: Some(60_000),
            width: Some(1920),
            height: Some(1080),
        }),
    )
    .unwrap();
    for slot in 0..6 {
        node.commit_feature_result(
            video.id,
            None,
            FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                slot,
                time_ms: u64::from(slot) * 10_000,
                decoded: true,
                width: Some(1920),
                height: Some(1080),
                pdq: Some(PdqHash::from_bytes([slot + 1; 32])),
                quality: Some(80),
            }),
        )
        .unwrap();
        node.commit_feature_result(
            video.id,
            None,
            FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                slot,
                features: stage2(slot + 1),
            }),
        )
        .unwrap();
    }
    node.commit_feature_result(
        video.id,
        None,
        FeatureWrite::ContactSheet("contact-sheets/local-only.jpg".into()),
    )
    .unwrap();

    add_successful_delete(&mut node, &machine);
    let pulled = node.pull_changes(0, 1000).unwrap();
    assert!(
        pulled
            .changes
            .iter()
            .any(|change| change.entity_kind == "deletion_tombstone")
    );
    assert!(
        pulled
            .changes
            .iter()
            .any(|change| change.entity_kind == "contact_sheet")
    );

    let mut central = CentralStore::connect(&url).await.unwrap();
    central
        .apply_sync_batch(
            &machine,
            &dedup_protocol::proto::SyncChangeBatch {
                changes: pulled.changes,
                high_seq: pulled.high_seq,
                pruned_through_seq: pulled.pruned_through_seq,
            },
        )
        .await
        .unwrap();

    let (client, connection) = tokio_postgres::connect(&url, tokio_postgres::NoTls)
        .await
        .unwrap();
    tokio::spawn(async move { connection.await.unwrap() });
    assert_eq!(table_count(&client, "image_stage1").await, 1);
    assert_eq!(table_count(&client, "image_stage2").await, 1);
    assert_eq!(table_count(&client, "video_metadata").await, 1);
    assert_eq!(table_count(&client, "video_frame_stage1").await, 6);
    assert_eq!(table_count(&client, "video_frame_stage2").await, 6);
    let tombstones: i64 = client
        .query_one(
            "SELECT COUNT(*) FROM deletion_tombstones WHERE machine_id=$1",
            &[&machine.as_str()],
        )
        .await
        .unwrap()
        .get(0);
    assert_eq!(tombstones, 1);
    let contact_table: Option<String> = client
        .query_one("SELECT to_regclass('public.contact_sheets')::text", &[])
        .await
        .unwrap()
        .get(0);
    assert!(contact_table.is_none());
}

fn node_batch(
    machine: &MachineId,
    path: &str,
    size: u64,
    md5: [u8; 16],
) -> dedup_protocol::proto::SyncChangeBatch {
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let scanned = ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        size,
    );
    store
        .upsert_content_and_location(&scanned, md5, MediaKind::Other)
        .unwrap();
    let batch = store.pull_changes(0, 1000).unwrap();
    dedup_protocol::proto::SyncChangeBatch {
        changes: batch.changes,
        high_seq: batch.high_seq,
        pruned_through_seq: batch.pruned_through_seq,
    }
}

fn location(machine: MachineId, path: &str) -> dedup_core::LocationKey {
    dedup_core::LocationKey::new(machine, NormalizedPath::new(path).unwrap())
}

fn unique_machine(seed: u8) -> MachineId {
    let id = uuid::Uuid::now_v7();
    let mut bytes = [seed; 32];
    bytes[..16].copy_from_slice(id.as_bytes());
    MachineId::from_sha256(bytes)
}

async fn sync_location(
    central: &mut CentralStore,
    location: &dedup_core::LocationKey,
    content: ContentKey,
) {
    let batch = node_batch(
        location.machine_id(),
        location.normalized_path().as_str(),
        content.file_size(),
        content.md5(),
    );
    central
        .apply_sync_batch(location.machine_id(), &batch)
        .await
        .unwrap();
}

fn analysis_node(machine_id: MachineId) -> CentralAnalysisNode {
    CentralAnalysisNode {
        machine_id,
        task_id: TaskId::new(),
        task_highwater: 1,
        sync_highwater: 1,
        task_status: "completed".into(),
    }
}

fn analysis_input(content: ContentKey, location: dedup_core::LocationKey) -> CentralAnalysisInput {
    CentralAnalysisInput { content, location }
}

fn group_member(
    location: dedup_core::LocationKey,
    content: ContentKey,
    representative: bool,
) -> CentralGroupMember {
    CentralGroupMember {
        location,
        content,
        representative,
        stage1_score: 1.0,
        phash_passed_parts: None,
        stage2_score: None,
        review: dedup_desktop_core::central::CentralReviewDecision::Undecided,
        width: None,
        height: None,
        quality: None,
        active: true,
    }
}

fn add_content(
    store: &mut NodeStore,
    path: &str,
    size: u64,
    md5: [u8; 16],
    kind: MediaKind,
) -> dedup_node_store::ContentRecord {
    store
        .upsert_content_and_location(
            &ScannedPath::new(
                NormalizedPath::new(path).unwrap(),
                DisplayPath::new(path).unwrap(),
                size,
            ),
            md5,
            kind,
        )
        .unwrap()
}

fn stage2(seed: u8) -> ImageStage2 {
    let mut sobel = [0.0; 128];
    sobel[usize::from(seed) % 128] = 1.0;
    ImageStage2 {
        phash_parts: [u64::from(seed); 9],
        sobel,
    }
}

fn add_successful_delete(store: &mut NodeStore, machine: &MachineId) {
    let deleted = add_content(
        store,
        r"C:\SyncScope\deleted.bin",
        330,
        [0x65; 16],
        MediaKind::Other,
    );
    let kept = add_content(
        store,
        r"C:\SyncScope\kept.bin",
        340,
        [0x66; 16],
        MediaKind::Other,
    );
    let deleted_location = location(machine.clone(), r"C:\SyncScope\deleted.bin");
    let kept_location = location(machine.clone(), r"C:\SyncScope\kept.bin");
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let group_id = format!("sync-delete-{}", run.as_uuid());
    store
        .replace_groups(
            run,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Image,
                representative: kept.key,
                members: vec![
                    GroupMemberWrite::new(kept_location.clone(), kept.key, true),
                    GroupMemberWrite::new(deleted_location.clone(), deleted.key, false),
                ],
            }],
        )
        .unwrap();
    store
        .save_review_mark(run, &group_id, &kept_location, ReviewDecision::Keep)
        .unwrap();
    store
        .save_review_mark(run, &group_id, &deleted_location, ReviewDecision::Delete)
        .unwrap();
    let batch = store
        .create_delete_batch(
            run,
            &[ConfirmedDeleteItem::new(
                group_id,
                deleted_location,
                deleted.key,
            )],
            DeleteMode::RecycleBin,
            2,
        )
        .unwrap();
    store
        .apply_delete_results(
            &batch.batch_id,
            &[DeleteResult::new(
                batch.items[0].item_id.clone(),
                DeleteOutcome::Recycled,
                None,
            )],
        )
        .unwrap();
}

async fn table_count(client: &tokio_postgres::Client, table: &str) -> i64 {
    client
        .query_one(&format!("SELECT COUNT(*) FROM {table}"), &[])
        .await
        .unwrap()
        .get(0)
}
