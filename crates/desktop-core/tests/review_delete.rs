//! 结果统一模型、有限窗口、复核快捷规则与删除确认的行为门禁。

use dedup_core::{ContentKey, DeleteMode, LocationKey, MachineId, NormalizedPath};
use dedup_desktop_core::central::{
    CentralGroup, CentralGroupKind, CentralGroupMember, CentralGroupMemberPage, CentralGroupPage,
    CentralReviewDecision,
};
use dedup_desktop_core::{
    delete::{DeleteConfirmation, DeleteItemOutcome, DeleteProgress, ReviewGroup},
    results::{
        GroupKind, MemberView, PagedWindow, group_page_from_central, group_page_from_node,
        member_page_from_central, member_page_from_node,
    },
    review::{QuickReviewRule, ReviewBoard, ReviewDecision},
};
use dedup_protocol::proto;

#[test]
fn node_and_central_pages_share_one_model_and_window_stays_bounded() {
    let content = content(1, 2048);
    let node = group_page_from_node(proto::ListGroups {
        analysis_run_id: uuid::Uuid::now_v7().to_string(),
        group_kind: proto::GroupKind::GroupExact as i32,
        cursor: String::new(),
        limit: 1,
        groups: vec![proto::DuplicateGroup {
            group_id: "local".into(),
            kind: proto::GroupKind::GroupExact as i32,
            representative: Some((&content).into()),
            member_count: 2,
            reclaimable_bytes: 2048,
        }],
        next_cursor: "node-next".into(),
    })
    .unwrap();
    let central = group_page_from_central(CentralGroupPage {
        items: vec![CentralGroup {
            group_id: "central".into(),
            kind: CentralGroupKind::Exact,
            representative: content,
            member_count: 2,
            reclaimable_bytes: 2048,
        }],
        next_cursor: Some("central-next".into()),
    });

    assert_eq!(node.items[0].kind, GroupKind::Exact);
    assert_eq!(central.items[0].kind, GroupKind::Exact);
    assert_eq!(
        node.items[0].representative,
        central.items[0].representative
    );

    let mut window = PagedWindow::new(2);
    window.append(node);
    window.append(central);
    window.append(dedup_desktop_core::results::GroupPage {
        items: vec![dedup_desktop_core::results::GroupView {
            group_id: "third".into(),
            kind: GroupKind::Exact,
            representative: content,
            member_count: 2,
            reclaimable_bytes: 2048,
        }],
        next_cursor: None,
    });
    assert_eq!(window.items().len(), 2);
    assert_eq!(window.items()[0].group_id, "central");
    assert_eq!(window.next_cursor(), None);
}

#[test]
fn offline_member_disables_preview_open_and_delete() {
    let offline = member("offline", 10, false, None, None);
    assert!(!offline.actions.preview);
    assert!(!offline.actions.open);
    assert!(!offline.actions.delete);

    let online = member("online", 20, true, Some((1920, 1080)), Some(80));
    assert!(online.actions.preview);
    assert!(online.actions.open);
    assert!(online.actions.delete);
}

/// 节点和中心都必须把真实失活状态传入统一动作门禁。
#[test]
fn inactive_members_disable_actions_in_node_and_central_models() {
    let machine = MachineId::from_sha256([0x61; 32]);
    let location = LocationKey::new(
        machine.clone(),
        NormalizedPath::new(r"D:\Inactive\old.bin").unwrap(),
    );
    let content = ContentKey::new([0x62; 16], 62);
    let node = member_page_from_node(
        proto::ListGroupMembers {
            analysis_run_id: uuid::Uuid::now_v7().to_string(),
            group_id: "inactive-node".into(),
            cursor: String::new(),
            limit: 1,
            members: vec![proto::GroupMember {
                location: Some((&location).into()),
                content: Some((&content).into()),
                active: false,
                ..Default::default()
            }],
            next_cursor: String::new(),
        },
        true,
    )
    .unwrap();
    let central = member_page_from_central(
        CentralGroupMemberPage {
            items: vec![CentralGroupMember {
                location,
                content,
                representative: false,
                stage1_score: 1.0,
                phash_passed_parts: None,
                stage2_score: None,
                review: CentralReviewDecision::Undecided,
                width: None,
                height: None,
                quality: None,
                active: false,
            }],
            next_cursor: None,
        },
        |_| true,
    );

    for member in [&node.items[0], &central.items[0]] {
        assert!(!member.active);
        assert_eq!(
            member.actions,
            dedup_desktop_core::results::MemberActions {
                preview: false,
                open: false,
                delete: false,
            }
        );
    }
}

#[test]
fn review_board_restores_marks_and_quick_selection_only_updates_decisions() {
    let smaller = member("small", 10, true, Some((800, 600)), Some(60));
    let larger = member("large", 20, true, Some((1920, 1080)), Some(90));
    let mut board = ReviewBoard::from_members(&[
        MemberView {
            review: ReviewDecision::Delete,
            ..smaller.clone()
        },
        MemberView {
            review: ReviewDecision::Keep,
            ..larger.clone()
        },
    ]);
    assert_eq!(board.decision(&smaller.location), ReviewDecision::Delete);
    assert_eq!(board.decision(&larger.location), ReviewDecision::Keep);

    let changes = board.apply_quick_rule(
        &[smaller.clone(), larger.clone()],
        QuickReviewRule::HighestQuality,
    );
    assert_eq!(changes.len(), 2);
    assert_eq!(board.decision(&larger.location), ReviewDecision::Keep);
    assert_eq!(board.decision(&smaller.location), ReviewDecision::Delete);
}

#[test]
fn delete_confirmation_requires_active_keep_and_reports_totals() {
    let keep = MemberView {
        review: ReviewDecision::Keep,
        ..member("keep", 100, true, None, None)
    };
    let delete_a = MemberView {
        review: ReviewDecision::Delete,
        ..member("delete-a", 40, true, None, None)
    };
    let delete_b = MemberView {
        review: ReviewDecision::Delete,
        ..member_on_machine("delete-b", 60, true, 2)
    };
    let valid = DeleteConfirmation::from_groups(
        DeleteMode::RecycleBin,
        &[ReviewGroup::new("g", vec![keep, delete_a, delete_b])],
    );
    assert!(valid.can_execute);
    assert_eq!(valid.file_count, 2);
    assert_eq!(valid.node_count, 2);
    assert_eq!(valid.reclaimable_bytes, 100);

    let invalid = DeleteConfirmation::from_groups(
        DeleteMode::Permanent,
        &[ReviewGroup::new(
            "bad",
            vec![MemberView {
                review: ReviewDecision::Delete,
                ..member("only-delete", 10, true, None, None)
            }],
        )],
    );
    assert!(!invalid.can_execute);
    assert!(invalid.warning.contains("永久"));
}

#[test]
fn central_historical_page_offline_delete_disables_complete_confirmation() {
    let mut historical = (0..200)
        .map(|index| member(&format!("history-{index:03}"), 10, true, None, None))
        .collect::<Vec<_>>();
    historical[0].review = ReviewDecision::Keep;
    historical[1].review = ReviewDecision::Delete;
    historical[1].set_availability(true, false);
    let current = MemberView {
        review: ReviewDecision::Delete,
        ..member("current-delete", 20, true, None, None)
    };

    let current_only = DeleteConfirmation::from_groups(
        DeleteMode::RecycleBin,
        &[ReviewGroup::new(
            "central",
            vec![historical[0].clone(), current.clone()],
        )],
    );
    assert!(current_only.can_execute, "当前页本身不能暴露历史页离线目标");

    historical.push(current);
    let complete = DeleteConfirmation::from_groups(
        DeleteMode::RecycleBin,
        &[ReviewGroup::new("central", historical)],
    );
    assert_eq!(complete.file_count, 2);
    assert!(!complete.can_execute);
    assert!(complete.warning.contains("在线"));
}

#[test]
fn mixed_delete_results_only_remove_successes_and_leave_retryable_items() {
    let members = vec![
        member("recycled", 10, true, None, None),
        member("failed", 20, true, None, None),
        member("skipped", 30, true, None, None),
    ];
    let mut progress = DeleteProgress::new("batch", &members);
    progress.apply("recycled", DeleteItemOutcome::Recycled, None);
    progress.apply("failed", DeleteItemOutcome::Failed, Some("占用中".into()));
    progress.apply(
        "skipped",
        DeleteItemOutcome::Skipped,
        Some("身份变化".into()),
    );

    assert_eq!(progress.remaining_paths(), ["failed", "skipped"]);
    assert_eq!(progress.retryable_paths(), ["failed", "skipped"]);
    assert_eq!(progress.released_bytes(), 10);
}

fn member(
    path: &str,
    size: u64,
    online: bool,
    dimensions: Option<(u32, u32)>,
    quality: Option<u8>,
) -> MemberView {
    member_on_machine_with_metadata(path, size, online, 1, dimensions, quality)
}

fn member_on_machine(path: &str, size: u64, online: bool, machine: u8) -> MemberView {
    member_on_machine_with_metadata(path, size, online, machine, None, None)
}

fn member_on_machine_with_metadata(
    path: &str,
    size: u64,
    online: bool,
    machine: u8,
    dimensions: Option<(u32, u32)>,
    quality: Option<u8>,
) -> MemberView {
    let location = LocationKey::new(
        MachineId::parse(&format!("{machine:02x}").repeat(32)).unwrap(),
        NormalizedPath::new(format!(r"C:\Media\{path}")).unwrap(),
    );
    MemberView::new(location, content(machine, size), false, online)
        .with_metadata(dimensions, quality)
        .with_display_path(path)
}

fn content(byte: u8, size: u64) -> ContentKey {
    ContentKey::new([byte; 16], size)
}
