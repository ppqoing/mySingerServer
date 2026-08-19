use dedup_core::{
    ContentKey, DeleteMode, DisplayPath, MachineId, MediaKind, NormalizedPath, TaskId, Thresholds,
};
use dedup_desktop_core::central::{
    CentralAnalysisInput, CentralAnalysisNode, CentralCandidate, CentralCandidateStatus,
    CentralDeleteOutcome, CentralDeleteResult, CentralGroupKind, CentralGroupMember,
    CentralGroupWrite, CentralPairKind, CentralReviewDecision, CentralStore,
};
use dedup_node_store::{NodeStore, ScannedPath};

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
            std::slice::from_ref(&exact_group),
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
    }
}
