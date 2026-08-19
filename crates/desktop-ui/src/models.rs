//! desktop-core 强类型状态到 Slint 行模型的单向映射。

use std::fmt::Write as _;

use dedup_desktop_core::{
    results::{GroupKind, GroupPage, MemberPage},
    review::ReviewDecision,
    view_state::{DesktopViewState, NodeConnectionState, TaskView, ViewTaskState},
};
use slint::{Color, ModelRc, SharedString, VecModel};

use crate::{UiGroupRow, UiMemberRow, UiNodeRow, UiTaskRow};

/// 把节点快照映射为 Slint 只读列表。
pub(crate) fn nodes(state: &DesktopViewState) -> ModelRc<UiNodeRow> {
    let rows = state
        .nodes()
        .iter()
        .enumerate()
        .map(|(index, node)| {
            let (status, color) = match node.connection {
                NodeConnectionState::Offline => ("离线", rgb(148, 163, 184)),
                NodeConnectionState::Connecting => ("连接中", rgb(245, 158, 11)),
                NodeConnectionState::Online => ("在线", rgb(34, 197, 94)),
                NodeConnectionState::Error => ("错误", rgb(248, 113, 113)),
            };
            let stats = node.stats.as_ref();
            UiNodeRow {
                index: index as i32,
                name: if index == 0 {
                    "本机节点".into()
                } else {
                    format!("计算节点 {}", index + 1).into()
                },
                address: node.endpoint.to_string().into(),
                status: status.into(),
                status_color: color,
                machine_id: node
                    .machine_id
                    .clone()
                    .unwrap_or_else(|| "尚未握手".into())
                    .into(),
                worker_text: stats.map_or_else(
                    || "—".into(),
                    |stats| format!("{}/{} 忙碌", stats.busy_workers, stats.worker_count).into(),
                ),
                task_text: stats.map_or_else(
                    || "—".into(),
                    |stats| {
                        format!("{} 排队 / {} 运行", stats.queued_items, stats.running_items).into()
                    },
                ),
                sync_text: stats.map_or_else(
                    || "—".into(),
                    |stats| format!("{} / {}", stats.sync_high_seq, stats.outbox_high_seq).into(),
                ),
                error_text: node.error_text.clone().unwrap_or_default().into(),
            }
        })
        .collect::<Vec<_>>();
    ModelRc::new(VecModel::from(rows))
}

/// 把任务快照映射为 Slint 只读列表。
pub(crate) fn tasks(state: &DesktopViewState) -> ModelRc<UiTaskRow> {
    let rows = state.tasks().iter().map(task_row).collect::<Vec<_>>();
    ModelRc::new(VecModel::from(rows))
}

/// 把统一结果页映射为 Slint 有限组列表；游标仍由调用方单独保存。
pub(crate) fn groups(page: &GroupPage) -> ModelRc<UiGroupRow> {
    let rows = page
        .items
        .iter()
        .map(|group| UiGroupRow {
            id: group.group_id.clone().into(),
            kind: group_kind(group.kind).into(),
            md5: md5_hex(group.representative.md5()).into(),
            size: bytes(group.representative.file_size()).into(),
            members: group.member_count as i32,
            reclaimable: bytes(group.reclaimable_bytes).into(),
        })
        .collect::<Vec<_>>();
    ModelRc::new(VecModel::from(rows))
}

/// 把统一成员页映射为可直接驱动预览、复核和删除门禁的 Slint 行。
pub(crate) fn members(page: &MemberPage) -> ModelRc<UiMemberRow> {
    let rows = page
        .items
        .iter()
        .map(|member| {
            let (review, review_color) = review(member.review);
            UiMemberRow {
                machine_id: member.location.machine_id().as_str().into(),
                path: member.display_path.clone().into(),
                md5: md5_hex(member.content.md5()).into(),
                size: bytes(member.content.file_size()).into(),
                representative: member.representative,
                stage1: format!("{:.3}", member.stage1_score).into(),
                phash: member
                    .phash_passed_parts
                    .map_or_else(|| "—".into(), |passed| format!("{passed}/9").into()),
                stage2: member
                    .stage2_score
                    .map_or_else(|| "—".into(), |score| format!("{score:.3}").into()),
                metadata: metadata(member.dimensions, member.quality).into(),
                review: review.into(),
                review_color,
                online: member.online,
                preview_enabled: member.actions.preview,
                delete_enabled: member.actions.delete,
            }
        })
        .collect::<Vec<_>>();
    ModelRc::new(VecModel::from(rows))
}

fn task_row(task: &TaskView) -> UiTaskRow {
    let (status, color) = match task.state {
        ViewTaskState::Queued => ("排队中", rgb(148, 163, 184)),
        ViewTaskState::Running => ("运行中", rgb(59, 130, 246)),
        ViewTaskState::Completed => ("已完成", rgb(34, 197, 94)),
        ViewTaskState::Failed => ("失败", rgb(248, 113, 113)),
        ViewTaskState::Cancelled => ("已取消", rgb(245, 158, 11)),
    };
    UiTaskRow {
        id: task.task_id.clone().into(),
        node_index: task.node_index as i32,
        title: task.title.clone().into(),
        stage: task.stage.clone().into(),
        status: status.into(),
        status_color: color,
        progress: i32::from(task.progress_percent()),
        counts: format!(
            "{} / {} · 失败 {} · 跳过 {}",
            task.completed_items, task.total_items, task.failed_items, task.skipped_incomplete
        )
        .into(),
    }
}

fn group_kind(kind: GroupKind) -> &'static str {
    match kind {
        GroupKind::Exact => "精确重复",
        GroupKind::SimilarImage => "相似图片",
        GroupKind::SimilarVideo => "相似视频",
    }
}

fn review(decision: ReviewDecision) -> (&'static str, Color) {
    match decision {
        ReviewDecision::Undecided => ("未决定", rgb(148, 163, 184)),
        ReviewDecision::Keep => ("保留", rgb(34, 197, 94)),
        ReviewDecision::Delete => ("删除", rgb(248, 113, 113)),
    }
}

fn metadata(dimensions: Option<(u32, u32)>, quality: Option<u8>) -> String {
    let dimensions =
        dimensions.map_or_else(|| "—".into(), |(width, height)| format!("{width}×{height}"));
    quality.map_or(dimensions.clone(), |value| {
        format!("{dimensions} · Q{value}")
    })
}

fn md5_hex(md5: [u8; 16]) -> String {
    let mut value = String::with_capacity(32);
    for byte in md5 {
        write!(&mut value, "{byte:02x}").expect("写入 String 不会失败");
    }
    value
}

pub(crate) fn bytes(value: u64) -> String {
    const UNITS: [&str; 5] = ["B", "KiB", "MiB", "GiB", "TiB"];
    let mut amount = value as f64;
    let mut unit = 0;
    while amount >= 1024.0 && unit < UNITS.len() - 1 {
        amount /= 1024.0;
        unit += 1;
    }
    if unit == 0 {
        format!("{value} B")
    } else {
        format!("{amount:.1} {}", UNITS[unit])
    }
}

pub(crate) fn postgres_health(state: &DesktopViewState) -> (SharedString, Color) {
    let message = state.postgres_message();
    let color = match state.postgres_message().as_str() {
        value if value.contains("正常") => rgb(34, 197, 94),
        value if value.contains("缺失") || value.contains("不可用") => rgb(248, 113, 113),
        _ => rgb(245, 158, 11),
    };
    (message.into(), color)
}

fn rgb(red: u8, green: u8, blue: u8) -> Color {
    Color::from_rgb_u8(red, green, blue)
}
