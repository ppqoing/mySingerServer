//! 中心结果的复核与删除确认只使用当前进程内的同一组成员投影。

use dedup_core::{AnalysisRunId, ContentKey, DeleteMode, LocationKey, MachineId, NormalizedPath};
use dedup_desktop_core::{
    delete::{DeleteConfirmation, ReviewGroup},
    results::MemberView,
    review::{ReviewBoard, ReviewDecision},
};

/// 中心组切换时必须丢弃旧组决定，删除确认只接受当前内存投影。
#[test]
fn central_review_and_delete_scope_is_process_local() {
    let run_id = AnalysisRunId::new();
    let keep = member("keep", 100);
    let delete = member("delete", 40);
    let mut board =
        ReviewBoard::for_central(run_id, "central-group", &[keep.clone(), delete.clone()]);

    board.set(keep.location.clone(), ReviewDecision::Keep);
    board.set(delete.location.clone(), ReviewDecision::Delete);
    let mut projected = vec![keep.clone(), delete.clone()];
    projected[0].review = board.decision(&projected[0].location);
    projected[1].review = board.decision(&projected[1].location);

    let confirmation = DeleteConfirmation::from_groups(
        DeleteMode::RecycleBin,
        &[ReviewGroup::new("central-group", projected)],
    );
    assert!(confirmation.can_execute);
    assert_eq!(confirmation.file_count, 1);
    assert!(board.is_scoped_to(run_id, "central-group"));

    let delete_location = delete.location.clone();
    let switched = ReviewBoard::for_central(AnalysisRunId::new(), "other-group", &[delete]);
    assert_eq!(
        switched.decision(&delete_location),
        ReviewDecision::Undecided,
        "切换中心运行或组后不得复用旧的进程内决定",
    );
}

fn member(path: &str, size: u64) -> MemberView {
    let machine = MachineId::from_sha256([0x71; 32]);
    let location = LocationKey::new(
        machine,
        NormalizedPath::new(format!(r"D:\Central\{path}.bin")).unwrap(),
    );
    MemberView::new(
        location,
        ContentKey::new([size as u8; 16], size),
        false,
        true,
    )
}
