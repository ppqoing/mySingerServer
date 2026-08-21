use dedup_core::{AnalysisRunId, MachineId};
use dedup_desktop_core::{
    analysis::CrossPollReport,
    central::CentralAnalysisStatus,
    runtime_tasks::{DesktopRuntimeTaskRegistry, DesktopRuntimeTaskState, RuntimeStageState},
    sync::{SyncPhase, SyncProgress, SyncTrigger, sync_trigger_channel},
};
use dedup_protocol::proto;

#[test]
fn cross_analysis_real_poll_shape_updates_seven_fixed_stages() {
    let registry = DesktopRuntimeTaskRegistry::new();
    let machines = [
        MachineId::from_sha256([0xd1; 32]),
        MachineId::from_sha256([0xd2; 32]),
    ];
    let reporter = registry.begin_cross_analysis("cross", &machines, "跨机器分析");
    reporter.update_cross_poll(
        &CrossPollReport {
            run_id: AnalysisRunId::new(),
            status: CentralAnalysisStatus::CollectingStage1,
            skipped_incomplete: 0,
            candidate_count: 0,
            unresolved_candidates: 0,
            phase2_task_count: 0,
        },
        2,
    );
    reporter.update_cross_poll(
        &CrossPollReport {
            run_id: AnalysisRunId::new(),
            status: CentralAnalysisStatus::Phase2Dispatched,
            skipped_incomplete: 0,
            candidate_count: 12,
            unresolved_candidates: 5,
            phase2_task_count: 2,
        },
        2,
    );
    reporter.update_cross_poll(
        &CrossPollReport {
            run_id: AnalysisRunId::new(),
            status: CentralAnalysisStatus::Completed,
            skipped_incomplete: 0,
            candidate_count: 12,
            unresolved_candidates: 0,
            phase2_task_count: 2,
        },
        2,
    );
    reporter.finish(DesktopRuntimeTaskState::Completed).unwrap();

    let details = registry.details(reporter.key()).unwrap();
    assert_eq!(details.stages.len(), 7);
    assert!(details.stages.iter().all(|stage| stage.state.is_terminal()));
    let candidates = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "stage1_screening")
        .unwrap();
    assert_eq!(candidates.unit, "candidate_pairs");
    assert_eq!(candidates.completed, 12);
    let wait = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "wait_nodes")
        .unwrap();
    assert_eq!(wait.unit, "nodes");
    assert_eq!(wait.total, Some(2));
}

#[test]
fn sync_progress_merges_active_machine_and_maps_ack_incremental_snapshot_caught_up() {
    let registry = DesktopRuntimeTaskRegistry::new();
    let machine = MachineId::from_sha256([0xd3; 32]);
    let reporter = registry.begin_or_merge_sync(&machine, "自动同步");
    let duplicate = registry.begin_or_merge_sync(&machine, "手动同步");
    assert_eq!(reporter.key(), duplicate.key());
    for (phase, committed, changes, pages) in [
        (SyncPhase::Acknowledging, 0, 0, 0),
        (SyncPhase::Incremental, 0, 4, 0),
        (SyncPhase::Snapshot, 0, 4, 3),
        (SyncPhase::CaughtUp, 9, 4, 3),
    ] {
        reporter.update_sync_progress(SyncProgress {
            trigger: SyncTrigger::Automatic,
            phase,
            committed_seq: committed,
            node_high_seq: 9,
            batch_count: 1,
            change_count: changes,
            snapshot_page_count: pages,
        });
    }
    reporter.finish(DesktopRuntimeTaskState::Completed).unwrap();

    let details = registry.details(reporter.key()).unwrap();
    assert_eq!(details.stages.len(), 4);
    assert!(
        details
            .stages
            .iter()
            .all(|stage| stage.state == RuntimeStageState::Completed)
    );
    assert_eq!(
        details
            .stages
            .iter()
            .find(|stage| stage.stage_id == "incremental")
            .unwrap()
            .completed,
        4
    );
    assert_eq!(
        details
            .stages
            .iter()
            .find(|stage| stage.stage_id == "snapshot")
            .unwrap()
            .completed,
        3
    );
    let next = registry.begin_or_merge_sync(&machine, "下一轮");
    assert_ne!(next.key(), reporter.key());
}

#[test]
fn delete_runtime_observes_confirmed_results_without_creating_or_expanding_commands() {
    let registry = DesktopRuntimeTaskRegistry::new();
    let machine_a = MachineId::from_sha256([0xd4; 32]);
    let machine_b = MachineId::from_sha256([0xd5; 32]);
    let reporter = registry.begin_delete("confirmed-delete", &[machine_a, machine_b], "删除", 2);
    reporter.mark_delete_prepared();
    let confirmed_results = vec![
        proto::DeleteItem {
            delete_item_id: "a".into(),
            outcome: "deleted".into(),
            ..Default::default()
        },
        proto::DeleteItem {
            delete_item_id: "b".into(),
            outcome: "failed".into(),
            message: "sharing violation".into(),
            ..Default::default()
        },
    ];
    reporter.finish_delete_results(&confirmed_results);
    reporter.finish(DesktopRuntimeTaskState::Failed).unwrap();

    let details = registry.details(reporter.key()).unwrap();
    assert_eq!(details.overall_total, Some(2));
    assert_eq!(details.overall_completed, 1);
    assert_eq!(details.overall_failed, 1);
    assert_eq!(details.failures.len(), 1);
    assert!(details.failures[0].message.contains("sharing violation"));
    assert_eq!(
        details
            .stages
            .iter()
            .find(|stage| stage.stage_id == "delete_items")
            .unwrap()
            .failed,
        1
    );
}

#[tokio::test]
async fn queued_sync_triggers_are_drained_into_the_active_runtime_row() {
    let (sender, mut receiver) = sync_trigger_channel(4);
    sender.connected().await.unwrap();
    assert_eq!(receiver.next().await, Some(SyncTrigger::Automatic));

    sender.manual().await.unwrap();
    sender.catch_up_tick().await.unwrap();
    assert_eq!(receiver.drain_pending(), 2);
}
