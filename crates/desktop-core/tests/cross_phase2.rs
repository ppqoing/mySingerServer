//! 跨机器二筛缓存选择、批量派发、门禁和不完整结果契约。

use std::collections::BTreeSet;

use dedup_core::{
    ContentKey, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath, TaskId, Thresholds,
};
use dedup_desktop_core::{
    analysis::{
        CrossFeatureSet, CrossTaskState, GateDecision, GateState, Stage2Availability,
        evaluate_candidates, phase2_gate, plan_stage2_batches, stage_gate,
    },
    central::{CentralCandidate, CentralCandidateStatus, CentralPairKind},
};
use dedup_media::ImageStage2;
use dedup_node_engine::{
    analysis::{Stage2BatchItem, Stage2Processor, Stage2Request, dispatch_stage2_batch},
    worker::Stage2Output,
};
use dedup_node_store::{FeatureWrite, NodeStore, ScannedPath, TaskStatus};

#[derive(Default)]
struct CountingWorker {
    calls: usize,
}

impl Stage2Processor for CountingWorker {
    async fn process(&mut self, _request: Stage2Request) -> Result<Stage2Output, String> {
        self.calls += 1;
        Err("缓存复用测试不应启动 Worker".into())
    }
}

#[test]
fn dispatch_reuses_postgres_then_prefers_node_cache() {
    let left = content(1);
    let right = content(2);
    let candidate = pending(left, right);
    let mut features = CrossFeatureSet::default();
    features.image_stage2.insert(left, stage2());
    let cached_machine = machine('b');
    let uncached_machine = machine('a');
    let availability = vec![
        available(right, uncached_machine.clone(), 'z', false),
        available(right, cached_machine.clone(), 'a', true),
    ];
    let online = BTreeSet::from([uncached_machine, cached_machine.clone()]);

    let batches = plan_stage2_batches(&[candidate], &features, &availability, &online, 1000);
    assert_eq!(batches.len(), 1);
    assert_eq!(batches[0].machine_id, cached_machine);
    assert_eq!(batches[0].items.len(), 1);
    assert_eq!(batches[0].items[0].content, right);
}

#[test]
fn phase2_waits_for_task_and_cursor_and_missing_is_not_zero() {
    let waiting = GateState {
        machine_id: machine('a'),
        task_id: TaskId::new(),
        state: CrossTaskState::Completed,
        task_highwater: 44,
        sync_highwater: 43,
    };
    assert_eq!(
        stage_gate(std::slice::from_ref(&waiting)),
        GateDecision::Waiting
    );

    let failed_terminal = GateState {
        state: CrossTaskState::Failed,
        task_highwater: 44,
        sync_highwater: 44,
        ..waiting.clone()
    };
    assert_eq!(phase2_gate(&[failed_terminal]), GateDecision::Ready);

    let candidate = pending(content(1), content(2));
    let (evaluated, unresolved) = evaluate_candidates(
        &[candidate],
        &CrossFeatureSet::default(),
        &Thresholds::default(),
    );
    assert_eq!(unresolved, 1);
    assert_eq!(evaluated[0].status, CentralCandidateStatus::Incomplete);
    assert_eq!(evaluated[0].stage2_score, None);
}

#[tokio::test]
async fn node_cache_is_republished_without_worker_computation() {
    let machine_id = machine('c');
    let path = NormalizedPath::new(r"D:\media\cached.jpg").unwrap();
    let mut store = NodeStore::open_in_memory(machine_id.clone()).unwrap();
    let record = store
        .upsert_content_and_location(
            &ScannedPath::new(
                path.clone(),
                DisplayPath::new(r"D:\media\cached.jpg").unwrap(),
                100,
            ),
            [9; 16],
            MediaKind::Image,
        )
        .unwrap();
    store
        .commit_feature_result(record.id, None, FeatureWrite::ImageStage2(stage2()))
        .unwrap();
    let before = store.outbox_high_seq().unwrap();
    let mut worker = CountingWorker::default();

    let task_id = dispatch_stage2_batch(
        &mut store,
        &[Stage2BatchItem {
            content: record.key,
            source: LocationKey::new(machine_id, path),
            frame_slots: Vec::new(),
        }],
        &mut worker,
        10,
    )
    .await
    .unwrap();

    assert_eq!(worker.calls, 0);
    let task = store.task_snapshot(task_id).unwrap();
    assert_eq!(task.status, TaskStatus::Completed);
    assert!(task.outbox_high_seq > before);
}

fn content(byte: u8) -> ContentKey {
    ContentKey::new([byte; 16], 100)
}

fn machine(byte: char) -> MachineId {
    MachineId::parse(&byte.to_string().repeat(64)).unwrap()
}

fn available(
    content: ContentKey,
    machine_id: MachineId,
    suffix: char,
    stage2_complete: bool,
) -> Stage2Availability {
    Stage2Availability {
        content,
        location: LocationKey::new(
            machine_id,
            NormalizedPath::new(format!(r"D:\media\{suffix}.jpg")).unwrap(),
        ),
        stage2_complete,
    }
}

fn pending(left: ContentKey, right: ContentKey) -> CentralCandidate {
    CentralCandidate {
        kind: CentralPairKind::Image,
        left,
        right,
        stage1_score: 0.9,
        phash_passed_parts: None,
        stage2_score: None,
        status: CentralCandidateStatus::Stage1Passed,
    }
}

fn stage2() -> ImageStage2 {
    ImageStage2 {
        phash_parts: [0; 9],
        sobel: [0.0; 128],
    }
}
