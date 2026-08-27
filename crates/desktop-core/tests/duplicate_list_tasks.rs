use dedup_desktop_core::{
    analysis::{DuplicateListStage, stage2_dispatch_stage, waiting_stages},
    central::PersistentStageState,
};

#[test]
fn duplicate_list_stages_keep_product_order_and_stable_ids() {
    let stages = waiting_stages();
    assert_eq!(
        stages
            .iter()
            .map(|stage| stage.stage_id.as_str())
            .collect::<Vec<_>>(),
        vec![
            DuplicateListStage::BuildCandidates.id().to_owned(),
            DuplicateListStage::DispatchStage2.id().to_owned(),
            DuplicateListStage::FinalCompare.id().to_owned(),
        ]
    );
    assert!(
        stages
            .iter()
            .all(|stage| stage.state == PersistentStageState::Waiting)
    );
}

#[test]
fn stage2_progress_counts_content_items_and_finishes_on_terminal_states() {
    let running = stage2_dispatch_stage(
        &["completed".into(), "running".into(), "cancelled".into()],
        100,
        200,
    );
    assert_eq!(running.state, PersistentStageState::Running);
    assert_eq!((running.completed, running.total), (1, Some(3)));
    assert_eq!((running.failed, running.skipped), (0, 1));
    assert_eq!(
        (running.started_at_ms, running.finished_at_ms),
        (Some(100), None)
    );

    let completed = stage2_dispatch_stage(
        &["completed".into(), "failed".into(), "cancelled".into()],
        100,
        220,
    );
    assert_eq!(completed.state, PersistentStageState::Completed);
    assert_eq!((completed.completed, completed.total), (1, Some(3)));
    assert_eq!((completed.failed, completed.skipped), (1, 1));
    assert_eq!(completed.finished_at_ms, Some(220));
}
