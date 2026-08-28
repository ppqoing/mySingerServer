//! 分析状态机、输入快照、稳定分页和复核持久化契约。

use dedup_core::{
    ContentKey, DisplayPath, GroupId, LocationKey, MachineId, MediaKind, NormalizedPath, Thresholds,
};
use dedup_node_store::{
    AnalysisMode, AnalysisStatus, GroupKind, GroupMemberWrite, GroupWrite, NewTaskItem, NodeStore,
    ReviewDecision, ScannedPath, TaskItemCompletion,
};

fn machine() -> MachineId {
    MachineId::from_sha256([0x72; 32])
}

fn scan(path: &str, size: u64) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        size,
    )
}

fn location(path: &str) -> LocationKey {
    LocationKey::new(machine(), NormalizedPath::new(path).unwrap())
}

/// 只接受确认过的状态边，并保留 partial 到二筛派发的显式重试入口。
#[test]
fn analysis_run_accepts_only_confirmed_transitions() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    assert!(
        store
            .transition_analysis_run(run, AnalysisStatus::Completed, 2)
            .is_err()
    );

    for (index, status) in [
        AnalysisStatus::Stage1Synced,
        AnalysisStatus::Screening,
        AnalysisStatus::Phase2Dispatched,
        AnalysisStatus::Phase2Synced,
        AnalysisStatus::Finalizing,
        AnalysisStatus::Completed,
    ]
    .into_iter()
    .enumerate()
    {
        store
            .transition_analysis_run(run, status, 10 + index as i64)
            .unwrap();
    }
    assert_eq!(
        store.analysis_run_snapshot(run).unwrap().status,
        AnalysisStatus::Completed
    );

    let retry = store
        .create_analysis_run(AnalysisMode::Central, Thresholds::default(), 20)
        .unwrap();
    store
        .transition_analysis_run(retry, AnalysisStatus::Partial, 21)
        .unwrap();
    store
        .transition_analysis_run(retry, AnalysisStatus::Phase2Dispatched, 22)
        .unwrap();

    let cancelled = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 30)
        .unwrap();
    store
        .transition_analysis_run(cancelled, AnalysisStatus::Cancelled, 31)
        .unwrap();
}

/// 冻结只读取已完成任务的当前活动位置，去重排序后禁止再次追加。
#[test]
fn analysis_inputs_are_deduplicated_and_immutable() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let scanned = scan(r"D:\Media\same.jpg", 90);
    let content = store
        .upsert_content_and_location(&scanned, [0x22; 16], MediaKind::Image)
        .unwrap();
    let task = store
        .create_task(
            "scan",
            &[
                NewTaskItem::for_content(
                    location(r"D:\Media\same.jpg"),
                    scanned.display_path.clone(),
                    90,
                    content.id,
                    "stage1",
                ),
                NewTaskItem::for_content(
                    location(r"D:\Media\same.jpg"),
                    scanned.display_path.clone(),
                    90,
                    content.id,
                    "stage1",
                ),
            ],
            1,
        )
        .unwrap();
    while let Some(item) = store.claim_next_item(task, 2).unwrap() {
        store
            .complete_item(
                &item.item_id,
                TaskItemCompletion::Succeeded {
                    content_id: Some(content.id),
                },
                3,
            )
            .unwrap();
    }

    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 4)
        .unwrap();
    assert_eq!(store.freeze_analysis_inputs(run, &[task], 5).unwrap(), 1);
    assert_eq!(store.analysis_inputs(run).unwrap().len(), 1);
    assert!(store.freeze_analysis_inputs(run, &[task], 6).is_err());
}

/// 分组游标只依赖固定排序键；相同库状态重复读取必须给出相同第一页。
#[test]
fn group_pages_have_stable_cursors() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    add_content(&mut store, r"D:\A1.bin", 1, 1);
    add_content(&mut store, r"D:\A2.bin", 11, 11);
    add_content(&mut store, r"D:\B1.jpg", 2, 2);
    add_content(&mut store, r"D:\B2.jpg", 12, 12);
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let group_a = group(
        GroupId::new(),
        GroupKind::Exact,
        1,
        r"D:\A1.bin",
        r"D:\A2.bin",
    );
    let group_b = group(
        GroupId::new(),
        GroupKind::Image,
        2,
        r"D:\B1.jpg",
        r"D:\B2.jpg",
    );
    store.replace_groups(run, &[group_b, group_a]).unwrap();

    let first = store.page_groups(run, None, 1).unwrap();
    let repeated = store.page_groups(run, None, 1).unwrap();
    assert_eq!(first, repeated);
    assert_eq!(first.items.len(), 1);
    let second = store
        .page_groups(run, first.next_cursor.as_deref(), 1)
        .unwrap();
    assert_eq!(second.items.len(), 1);
    assert_ne!(first.items[0].group_id, second.items[0].group_id);
}

/// 结果页必须按当前位置收缩，而不是继续信任分析时冻结的成员活动位。
#[test]
fn group_pages_follow_current_location_activity() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let first = add_content(&mut store, r"D:\Activity\a.bin", 100, 0x31);
    let second = add_content(&mut store, r"D:\Activity\b.bin", 200, 0x32);
    let third = add_content(&mut store, r"D:\Activity\c.bin", 300, 0x33);
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let group_id = GroupId::new().as_uuid().to_string();
    store
        .replace_groups(
            run,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Exact,
                representative: first,
                members: vec![
                    GroupMemberWrite::new(location(r"D:\Activity\a.bin"), first, true),
                    GroupMemberWrite::new(location(r"D:\Activity\b.bin"), second, false),
                    GroupMemberWrite::new(location(r"D:\Activity\c.bin"), third, false),
                ],
            }],
        )
        .unwrap();

    let scan = store
        .create_scan_task(&[NormalizedPath::new(r"D:\Activity").unwrap()], 2)
        .unwrap();
    store
        .finalize_scan_task(
            scan,
            &[
                NormalizedPath::new(r"D:\Activity\a.bin").unwrap(),
                NormalizedPath::new(r"D:\Activity\c.bin").unwrap(),
            ],
            3,
        )
        .unwrap();

    let group = store.page_groups(run, None, 10).unwrap().items.remove(0);
    assert_eq!(group.member_count, 2);
    assert_eq!(group.representative, first);
    assert_eq!(group.reclaimable_bytes, 300);
    let members = store
        .page_group_members(run, &group_id, None, 10)
        .unwrap()
        .items;
    assert_eq!(members.len(), 3);
    assert_eq!(
        members
            .iter()
            .map(|member| member.active)
            .collect::<Vec<_>>(),
        [true, false, true]
    );

    let scan = store
        .create_scan_task(&[NormalizedPath::new(r"D:\Activity").unwrap()], 4)
        .unwrap();
    store
        .finalize_scan_task(
            scan,
            &[NormalizedPath::new(r"D:\Activity\c.bin").unwrap()],
            5,
        )
        .unwrap();
    assert!(store.page_groups(run, None, 10).unwrap().items.is_empty());
}

/// 路径仍活动但内容键改变时，旧分组成员必须失活并重新选择当前代表。
#[test]
fn group_pages_reject_replaced_content_at_same_path() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let first = add_content(&mut store, r"D:\Replaced\a.bin", 100, 0x41);
    let second = add_content(&mut store, r"D:\Replaced\b.bin", 200, 0x42);
    let third = add_content(&mut store, r"D:\Replaced\c.bin", 300, 0x43);
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let group_id = GroupId::new().as_uuid().to_string();
    store
        .replace_groups(
            run,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Exact,
                representative: first,
                members: vec![
                    GroupMemberWrite::new(location(r"D:\Replaced\a.bin"), first, true),
                    GroupMemberWrite::new(location(r"D:\Replaced\b.bin"), second, false),
                    GroupMemberWrite::new(location(r"D:\Replaced\c.bin"), third, false),
                ],
            }],
        )
        .unwrap();
    add_content(&mut store, r"D:\Replaced\a.bin", 110, 0x44);

    let group = store.page_groups(run, None, 10).unwrap().items.remove(0);
    assert_eq!(group.member_count, 2);
    assert_eq!(group.representative, second);
    assert_eq!(group.reclaimable_bytes, 300);
    let members = store
        .page_group_members(run, &group_id, None, 10)
        .unwrap()
        .items;
    assert_eq!(
        members
            .iter()
            .map(|member| (member.active, member.representative))
            .collect::<Vec<_>>(),
        [(false, false), (true, true), (true, false)]
    );
}

/// 复核选择只属于当前运行，进程重开后不允许恢复旧删除意图。
#[test]
fn review_mark_is_discarded_after_reopen() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("review.db");
    let run;
    let group_id = GroupId::new().as_uuid().to_string();
    let member = location(r"D:\Review\keep.jpg");
    {
        let mut store = NodeStore::open(&database, machine()).unwrap();
        run = store
            .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
            .unwrap();
        let write = GroupWrite {
            group_id: group_id.clone(),
            kind: GroupKind::Image,
            representative: ContentKey::new([9; 16], 9),
            members: vec![
                GroupMemberWrite::new(member.clone(), ContentKey::new([9; 16], 9), true),
                GroupMemberWrite::new(
                    location(r"D:\Review\delete.jpg"),
                    ContentKey::new([8; 16], 8),
                    false,
                ),
            ],
        };
        store.replace_groups(run, &[write]).unwrap();
        store
            .save_review_mark(run, &group_id, &member, ReviewDecision::Keep)
            .unwrap();
    }
    let store = NodeStore::open(&database, machine()).unwrap();
    assert_eq!(store.review_mark(run, &group_id, &member).unwrap(), None);
}

fn group(id: GroupId, kind: GroupKind, seed: u8, first: &str, second: &str) -> GroupWrite {
    let representative = ContentKey::new([seed; 16], u64::from(seed));
    GroupWrite {
        group_id: id.as_uuid().to_string(),
        kind,
        representative,
        members: vec![
            GroupMemberWrite::new(location(first), representative, true),
            GroupMemberWrite::new(
                location(second),
                ContentKey::new([seed + 10; 16], u64::from(seed + 10)),
                false,
            ),
        ],
    }
}

fn add_content(store: &mut NodeStore, path: &str, size: u64, seed: u8) -> ContentKey {
    store
        .upsert_content_and_location(&scan(path, size), [seed; 16], MediaKind::Other)
        .unwrap()
        .key
}
