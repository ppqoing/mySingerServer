//! Node 任务与本地分析阶段的当前进程状态契约。

use dedup_core::{MachineId, Thresholds};
use dedup_node_store::{
    AnalysisMode, NewTaskItem, NodeStore, PersistentStageState, TaskStageWrite,
};

fn machine() -> MachineId {
    MachineId::parse("7373737373737373737373737373737373737373737373737373737373737373").unwrap()
}

fn stage(
    stage_id: &str,
    state: PersistentStageState,
    completed: u64,
    total: Option<u64>,
    started_at_ms: Option<u64>,
    finished_at_ms: Option<u64>,
) -> TaskStageWrite {
    TaskStageWrite {
        stage_id: stage_id.into(),
        state,
        completed,
        total,
        failed: 0,
        skipped: 0,
        started_at_ms,
        finished_at_ms,
        warning_text: None,
    }
}

#[test]
fn task_stage_is_discarded_after_reopen() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("node.sqlite3");
    let mut store = NodeStore::open(&path, machine()).unwrap();
    let task = store
        .create_task("base_compute", &[NewTaskItem::detached("file")], 1_000)
        .unwrap();
    store
        .save_task_stage(
            task,
            stage(
                "enumerate_files",
                PersistentStageState::Running,
                0,
                None,
                Some(1_100),
                None,
            ),
        )
        .unwrap();
    store
        .save_task_stage(
            task,
            stage(
                "enumerate_files",
                PersistentStageState::Completed,
                10,
                Some(10),
                Some(1_100),
                Some(1_300),
            ),
        )
        .unwrap();
    drop(store);

    let reopened = NodeStore::open(&path, machine()).unwrap();
    assert!(reopened.task_stages(task).unwrap().is_empty());
}

#[test]
fn local_analysis_stages_use_the_same_persistent_shape() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 10)
        .unwrap();
    store
        .save_analysis_stage(
            run,
            stage(
                "build_candidates",
                PersistentStageState::Running,
                4,
                Some(10),
                Some(20),
                None,
            ),
        )
        .unwrap();

    let stages = store.analysis_stages(run).unwrap();
    assert_eq!(stages.len(), 1);
    assert_eq!(stages[0].stage_id, "build_candidates");
    assert_eq!(stages[0].completed, 4);
}
