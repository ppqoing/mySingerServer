//! 跨机器一筛双门禁、完整特征筛选和代表中心分组契约。

use dedup_core::{
    ContentKey, DisplayPath, LocationKey, MachineId, MediaKind, NodeEndpoint, NormalizedPath,
    TaskId, Thresholds,
};
use dedup_desktop_core::{
    analysis::{
        CrossAnalysisCoordinator, CrossFeatureSet, CrossNodeSelection, CrossTaskState,
        GateDecision, GateState, build_groups, screen_candidates, stage_gate,
    },
    central::{
        CentralAnalysisInput, CentralCandidate, CentralCandidateStatus, CentralGroupKind,
        CentralPairKind, CentralStore,
    },
    node_session::NodeSession,
};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_engine::{actor::NodeEngine, server::NodeServer};
use dedup_node_store::{
    FeatureWrite, ImageStage1Fields, NewTaskItem, NodeStore, ScannedPath, TaskItemCompletion,
};

#[test]
fn stage1_requires_every_completed_task_and_synced_highwater() {
    let ready = gate(CrossTaskState::Completed, 12, 12);
    assert_eq!(
        stage_gate(std::slice::from_ref(&ready)),
        GateDecision::Ready
    );

    for blocked in [
        gate(CrossTaskState::Queued, 0, 99),
        gate(CrossTaskState::Running, 0, 99),
        gate(CrossTaskState::Failed, 7, 99),
        gate(CrossTaskState::Cancelled, 7, 99),
        gate(CrossTaskState::Completed, 12, 11),
    ] {
        assert_eq!(stage_gate(&[ready.clone(), blocked]), GateDecision::Waiting);
    }
}

#[test]
fn screen_uses_complete_inputs_and_shared_pdq_band() {
    let left = content(1, 100);
    let right = content(2, 101);
    let incomplete = content(3, 102);
    let mut features = CrossFeatureSet::default();
    features.image_stage1.insert(left, image(0, 100));
    features.image_stage1.insert(right, image(1, 100));
    features
        .media_kinds
        .insert(left, dedup_core::MediaKind::Image);
    features
        .media_kinds
        .insert(right, dedup_core::MediaKind::Image);
    features
        .media_kinds
        .insert(incomplete, dedup_core::MediaKind::Image);

    let (candidates, skipped) = screen_candidates(&features, &Thresholds::default());
    assert_eq!(skipped, 1);
    assert_eq!(candidates.len(), 1);
    assert_eq!(candidates[0].left, left);
    assert_eq!(candidates[0].right, right);
    assert_eq!(candidates[0].status, CentralCandidateStatus::Stage1Passed);
}

#[test]
fn final_groups_do_not_expand_through_non_representative_edges() {
    let a = content(1, 100);
    let b = content(2, 100);
    let c = content(3, 100);
    let exact = content(4, 200);
    let inputs = vec![
        input(a, 'a'),
        input(b, 'b'),
        input(c, 'c'),
        input(exact, 'd'),
        input(exact, 'e'),
    ];
    let candidates = vec![passed(a, b, 0.9), passed(b, c, 0.8)];

    let groups = build_groups(&inputs, &candidates);
    let exact_group = groups
        .iter()
        .find(|group| group.kind == CentralGroupKind::Exact)
        .unwrap();
    assert_eq!(exact_group.members.len(), 2);
    let image_group = groups
        .iter()
        .find(|group| group.kind == CentralGroupKind::Image)
        .unwrap();
    assert_eq!(image_group.representative, a);
    assert_eq!(image_group.members.len(), 2);
    assert!(image_group.members.iter().all(|member| member.content != c));
}

#[tokio::test]
#[ignore = "需要 DEDUP_TEST_POSTGRES_URL 指向手工创建的 V2 PostgreSQL schema"]
async fn coordinator_freezes_syncs_screens_and_finalizes_through_real_tcp_and_postgres() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let machine_id = machine('f');
    let mut node_store = NodeStore::open_in_memory(machine_id.clone()).unwrap();
    let mut seeded = Vec::new();
    for (suffix, md5) in [('x', [0xf1; 16]), ('y', [0xf2; 16])] {
        let path = format!(r"D:\cross-{suffix}.jpg");
        let scanned = ScannedPath::new(
            NormalizedPath::new(&path).unwrap(),
            DisplayPath::new(&path).unwrap(),
            100,
        );
        let record = node_store
            .upsert_content_and_location(&scanned, md5, MediaKind::Image)
            .unwrap();
        node_store
            .commit_feature_result(
                record.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(image(0, 100))),
            )
            .unwrap();
        node_store
            .commit_feature_result(record.id, None, FeatureWrite::ImageStage2(image_stage2()))
            .unwrap();
        seeded.push((scanned, record));
    }
    let task_items = seeded
        .iter()
        .map(|(scanned, record)| {
            NewTaskItem::for_content(
                LocationKey::new(machine_id.clone(), scanned.normalized_path.clone()),
                scanned.display_path.clone(),
                scanned.file_size,
                record.id,
                "stage1",
            )
        })
        .collect::<Vec<_>>();
    let scan_task = node_store.create_task("scan", &task_items, 1).unwrap();
    while let Some(item) = node_store.claim_next_item(scan_task, 2).unwrap() {
        node_store
            .complete_item(
                &item.item_id,
                TaskItemCompletion::Succeeded {
                    content_id: item.content_id,
                },
                3,
            )
            .unwrap();
    }

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (handle, actor) = NodeEngine::spawn_for_test(
        node_store,
        address,
        std::path::Path::new(r"C:\fixture\cache"),
    );
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel();
    let server_handle = handle.clone();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        server_handle,
        shutdown_rx,
    ));
    let session = NodeSession::connect(NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    })
    .await
    .unwrap();
    let mut central = CentralStore::connect(&url).await.unwrap();
    let mut coordinator = CrossAnalysisCoordinator::start(
        &mut central,
        &[CrossNodeSelection::new(&session, scan_task)],
        Thresholds::default(),
    )
    .await
    .unwrap();

    let report = coordinator.poll(&mut central, &[&session]).await.unwrap();
    assert_eq!(
        report.status,
        dedup_desktop_core::central::CentralAnalysisStatus::Completed
    );
    assert_eq!(report.candidate_count, 1);
    let groups = central.page_groups(report.run_id, None, 10).await.unwrap();
    assert_eq!(groups.items.len(), 1);
    assert_eq!(groups.items[0].kind, CentralGroupKind::Image);

    let fresh = CrossAnalysisCoordinator::start(
        &mut central,
        &[CrossNodeSelection::new(&session, scan_task)],
        Thresholds::default(),
    )
    .await
    .unwrap();
    assert_ne!(fresh.run_id(), report.run_id);

    drop(session);
    shutdown_tx.send(()).unwrap();
    server.await.unwrap().unwrap();
    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

fn gate(state: CrossTaskState, task_highwater: u64, sync_highwater: u64) -> GateState {
    GateState {
        machine_id: machine('a'),
        task_id: TaskId::new(),
        state,
        task_highwater,
        sync_highwater,
    }
}

fn content(byte: u8, size: u64) -> ContentKey {
    ContentKey::new([byte; 16], size)
}

fn machine(byte: char) -> MachineId {
    MachineId::parse(&byte.to_string().repeat(64)).unwrap()
}

fn input(content: ContentKey, suffix: char) -> CentralAnalysisInput {
    CentralAnalysisInput {
        content,
        location: LocationKey::new(
            machine(suffix),
            NormalizedPath::new(format!(r"C:\media\{suffix}.jpg")).unwrap(),
        ),
    }
}

fn image(last_byte: u8, quality: u8) -> ImageStage1 {
    let mut pdq = [0_u8; 32];
    pdq[31] = last_byte;
    ImageStage1 {
        width: 100,
        height: 100,
        pdq: PdqHash::from_bytes(pdq),
        quality,
    }
}

fn image_stage2() -> ImageStage2 {
    ImageStage2 {
        phash_parts: [0; 9],
        sobel: [0.0; 128],
    }
}

fn passed(left: ContentKey, right: ContentKey, score: f64) -> CentralCandidate {
    CentralCandidate {
        kind: CentralPairKind::Image,
        left,
        right,
        stage1_score: score,
        phash_passed_parts: Some(9),
        stage2_score: Some(score),
        status: CentralCandidateStatus::Passed,
    }
}
