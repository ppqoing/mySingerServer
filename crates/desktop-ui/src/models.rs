//! desktop-core 强类型状态到 Slint 行模型的单向映射。

use std::fmt::Write as _;

use dedup_desktop_core::{
    results::{GroupKind, GroupPage, MemberPage},
    review::ReviewDecision,
    runtime_tasks::{
        DesktopRuntimeTaskState, RuntimeStageSnapshot, RuntimeStageState, RuntimeTaskOwner,
        RuntimeTaskSnapshot,
    },
    view_state::{
        DesktopViewState, NodeConnectionState, RuntimeTaskControllerState, RuntimeTaskDetailsView,
        TaskView, ViewTaskState,
    },
};
use dedup_protocol::proto;
use slint::{Color, ModelRc, SharedString, VecModel};

use crate::{
    UiGroupRow, UiMemberRow, UiNodeRow, UiRuntimeFailureRow, UiRuntimeStageRow, UiRuntimeWorkerRow,
    UiTaskRow,
};

/// 一次 `RuntimeTasksChanged` 事件对应的全部 Slint 模型和详情摘要。
pub(crate) struct RuntimeUiModels {
    pub(crate) tasks: ModelRc<UiTaskRow>,
    pub(crate) stages: ModelRc<UiRuntimeStageRow>,
    pub(crate) workers: ModelRc<UiRuntimeWorkerRow>,
    pub(crate) failures: ModelRc<UiRuntimeFailureRow>,
    pub(crate) title: SharedString,
    pub(crate) machine_id: SharedString,
    pub(crate) state: SharedString,
    pub(crate) counts: SharedString,
    pub(crate) stale: bool,
    pub(crate) error: SharedString,
}

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

/// 把 Node/Desktop 统一运行状态整体转换为任务、阶段、Worker 和失败模型。
pub(crate) fn runtime_tasks(state: &RuntimeTaskControllerState) -> RuntimeUiModels {
    let selected = state.selected();
    let rows = state
        .summaries()
        .iter()
        .map(|task| runtime_task_row(task, state.is_stale() && selected == Some(&task.key)))
        .collect::<Vec<_>>();
    let selected_summary = selected.and_then(|key| {
        state
            .summaries()
            .iter()
            .find(|summary| &summary.key == key)
    });
    let (stages, workers, failures) = runtime_detail_rows(state.details());
    let (title, machine_id, status, counts) = selected_summary.map_or_else(
        || (String::new(), String::new(), String::new(), String::new()),
        |summary| {
            (
                summary.title.clone(),
                summary.machine_ids.join("、"),
                runtime_task_state(summary.state).0.to_owned(),
                runtime_counts(
                    summary.overall_completed,
                    summary.overall_total,
                    summary.overall_failed,
                    summary.overall_skipped,
                ),
            )
        },
    );
    RuntimeUiModels {
        tasks: ModelRc::new(VecModel::from(rows)),
        stages: ModelRc::new(VecModel::from(stages)),
        workers: ModelRc::new(VecModel::from(workers)),
        failures: ModelRc::new(VecModel::from(failures)),
        title: title.into(),
        machine_id: machine_id.into(),
        state: status.into(),
        counts: counts.into(),
        stale: state.is_stale(),
        error: state.error().unwrap_or_default().into(),
    }
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
        runtime_id: task.task_id.clone().into(),
        owner_kind: "legacy".into(),
        node_index: task.node_index as i32,
        machine_id: SharedString::default(),
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
        stale: false,
    }
}

/// 把一条统一运行任务摘要映射为任务中心左栏行。
fn runtime_task_row(task: &RuntimeTaskSnapshot, stale: bool) -> UiTaskRow {
    let (owner_kind, node_index) = match task.key.owner {
        RuntimeTaskOwner::Node { node_index } => ("node", node_index as i32),
        RuntimeTaskOwner::Desktop => ("desktop", -1),
    };
    let (status, status_color) = runtime_task_state(task.state);
    let progress = task.overall_total.map_or(0, |total| {
        percent(task.overall_completed, total)
    });
    let stage = task
        .stages
        .iter()
        .find(|stage| stage.state == RuntimeStageState::Running)
        .or_else(|| task.stages.last())
        .map_or("—", |stage| stage.display_name.as_str());
    UiTaskRow {
        id: task.key.id.clone().into(),
        runtime_id: task.key.id.clone().into(),
        owner_kind: owner_kind.into(),
        node_index,
        machine_id: task.machine_ids.join("、").into(),
        title: task.title.clone().into(),
        stage: stage.into(),
        status: status.into(),
        status_color,
        progress,
        counts: runtime_counts(
            task.overall_completed,
            task.overall_total,
            task.overall_failed,
            task.overall_skipped,
        )
        .into(),
        stale,
    }
}

/// 把当前 Node 或 Desktop 详情拆为三个可整体替换的行模型。
fn runtime_detail_rows(
    details: Option<&RuntimeTaskDetailsView>,
) -> (
    Vec<UiRuntimeStageRow>,
    Vec<UiRuntimeWorkerRow>,
    Vec<UiRuntimeFailureRow>,
) {
    match details {
        Some(RuntimeTaskDetailsView::Node { details, .. }) => (
            details.stages.iter().map(node_stage_row).collect(),
            details.workers.iter().map(worker_row).collect(),
            recent_failures(
                details
                    .failures
                    .iter()
                    .map(|failure| UiRuntimeFailureRow {
                        stage_id: failure.stage_id.clone().into(),
                        path: failure.display_path.clone().into(),
                        message: failure.message.clone().into(),
                    })
                    .collect(),
            ),
        ),
        Some(RuntimeTaskDetailsView::Desktop(details)) => (
            details.stages.iter().map(desktop_stage_row).collect(),
            Vec::new(),
            recent_failures(
                details
                    .failures
                    .iter()
                    .map(|failure| UiRuntimeFailureRow {
                        stage_id: failure.stage_id.clone().into(),
                        path: failure.display_path.clone().into(),
                        message: failure.message.clone().into(),
                    })
                    .collect(),
            ),
        ),
        None => (Vec::new(), Vec::new(), Vec::new()),
    }
}

/// Node 阶段保留协议中的速度、耗时和 ETA。
fn node_stage_row(stage: &proto::RuntimeStageDetails) -> UiRuntimeStageRow {
    let state = proto::RuntimeStageState::try_from(stage.state)
        .unwrap_or(proto::RuntimeStageState::Unspecified);
    let (state_text, state_color) = node_stage_state(state);
    UiRuntimeStageRow {
        stage_id: stage.stage_id.clone().into(),
        name: stage.display_name.clone().into(),
        state: state_text.into(),
        state_color,
        unit: unit_text(&stage.unit).into(),
        progress: stage
            .total_known
            .then(|| percent(stage.completed, stage.total))
            .unwrap_or(0),
        counts: progress_counts(stage.completed, stage.total_known.then_some(stage.total)).into(),
        speed: format_speed(stage.speed_per_second, &stage.unit).into(),
        elapsed: format_duration(stage.elapsed_ms).into(),
        eta: stage.eta_ms.map_or_else(|| "—".into(), format_duration).into(),
        failures: format!("失败 {} · 跳过 {}", stage.failed, stage.skipped).into(),
    }
}

/// Desktop 阶段沿用同一视觉模型，未采集的速度和 ETA 明确显示为占位符。
fn desktop_stage_row(stage: &RuntimeStageSnapshot) -> UiRuntimeStageRow {
    let (state, state_color) = desktop_stage_state(stage.state);
    UiRuntimeStageRow {
        stage_id: stage.stage_id.clone().into(),
        name: stage.display_name.clone().into(),
        state: state.into(),
        state_color,
        unit: unit_text(&stage.unit).into(),
        progress: stage.total.map_or(0, |total| percent(stage.completed, total)),
        counts: progress_counts(stage.completed, stage.total).into(),
        speed: "—".into(),
        elapsed: "—".into(),
        eta: "—".into(),
        failures: format!("失败 {} · 跳过 {}", stage.failed, stage.skipped).into(),
    }
}

/// Worker PID 缺失时仍用真实 slot 建立可辨识身份。
fn worker_row(worker: &proto::RuntimeWorkerDetails) -> UiRuntimeWorkerRow {
    UiRuntimeWorkerRow {
        slot: worker.slot as i32,
        identity: worker.process_id.map_or_else(
            || format!("槽位 {}", worker.slot).into(),
            |pid| format!("PID {pid} · 槽位 {}", worker.slot).into(),
        ),
        stage_id: worker.stage_id.clone().into(),
        path: worker.display_path.clone().into(),
        disk: worker.physical_disk_id.clone().into(),
        completed: format!("{} 个文件", worker.completed_files).into(),
        speed: format_speed(worker.speed_per_second, "files").into(),
    }
}

/// 后端异常输入超过协议上限时仍只展示最新 20 条。
fn recent_failures(mut rows: Vec<UiRuntimeFailureRow>) -> Vec<UiRuntimeFailureRow> {
    let keep_from = rows.len().saturating_sub(20);
    rows.drain(0..keep_from);
    rows
}

fn runtime_task_state(state: DesktopRuntimeTaskState) -> (&'static str, Color) {
    match state {
        DesktopRuntimeTaskState::Running => ("运行中", rgb(59, 130, 246)),
        DesktopRuntimeTaskState::Completed => ("已完成", rgb(34, 197, 94)),
        DesktopRuntimeTaskState::Failed => ("失败", rgb(248, 113, 113)),
        DesktopRuntimeTaskState::Cancelled => ("已取消", rgb(245, 158, 11)),
    }
}

fn node_stage_state(state: proto::RuntimeStageState) -> (&'static str, Color) {
    match state {
        proto::RuntimeStageState::RuntimeStageWaiting => ("等待", rgb(148, 163, 184)),
        proto::RuntimeStageState::RuntimeStageRunning => ("运行中", rgb(59, 130, 246)),
        proto::RuntimeStageState::RuntimeStageCompleted => ("已完成", rgb(34, 197, 94)),
        proto::RuntimeStageState::RuntimeStageFailed => ("失败", rgb(248, 113, 113)),
        proto::RuntimeStageState::RuntimeStageSkipped => ("已跳过", rgb(245, 158, 11)),
        proto::RuntimeStageState::Unspecified => ("未知", rgb(148, 163, 184)),
    }
}

fn desktop_stage_state(state: RuntimeStageState) -> (&'static str, Color) {
    match state {
        RuntimeStageState::Waiting => ("等待", rgb(148, 163, 184)),
        RuntimeStageState::Running => ("运行中", rgb(59, 130, 246)),
        RuntimeStageState::Completed => ("已完成", rgb(34, 197, 94)),
        RuntimeStageState::Failed => ("失败", rgb(248, 113, 113)),
        RuntimeStageState::Skipped => ("已跳过", rgb(245, 158, 11)),
    }
}

fn runtime_counts(completed: u64, total: Option<u64>, failed: u64, skipped: u64) -> String {
    format!(
        "{} · 失败 {failed} · 跳过 {skipped}",
        progress_counts(completed, total)
    )
}

fn progress_counts(completed: u64, total: Option<u64>) -> String {
    total.map_or_else(
        || format!("{completed} / —"),
        |total| format!("{completed} / {total}"),
    )
}

fn percent(completed: u64, total: u64) -> i32 {
    if total == 0 {
        0
    } else {
        completed.saturating_mul(100).checked_div(total).unwrap_or(0).min(100) as i32
    }
}

fn format_speed(speed: f64, unit: &str) -> String {
    if !speed.is_finite() || speed <= 0.0 {
        return "—".into();
    }
    if unit == "bytes" {
        return format!("{}/s", bytes(speed as u64));
    }
    format!("{speed:.1} {}/秒", unit_text(unit))
}

fn unit_text(unit: &str) -> &'static str {
    match unit {
        "bytes" => "字节",
        "files" => "文件",
        "nodes" => "节点",
        "pages" => "页",
        "changes" => "条",
        "candidate_pairs" => "对",
        _ => "项",
    }
}

fn format_duration(milliseconds: u64) -> String {
    if milliseconds < 1_000 {
        return format!("{milliseconds} 毫秒");
    }
    let seconds = milliseconds as f64 / 1_000.0;
    if seconds < 60.0 {
        return format!("{seconds:.1} 秒");
    }
    format!("{} 分 {:.0} 秒", (seconds / 60.0) as u64, seconds % 60.0)
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

/// 设置页节点选择完整展示名称、机器唯一 ID、地址和连接状态。
pub(crate) fn node_config_options(state: &DesktopViewState) -> ModelRc<SharedString> {
    let rows = state
        .nodes()
        .iter()
        .enumerate()
        .map(|(index, node)| {
            let name = if index == 0 {
                "本机节点".to_owned()
            } else {
                format!("计算节点 {}", index + 1)
            };
            let status = match node.connection {
                NodeConnectionState::Offline => "离线",
                NodeConnectionState::Connecting => "连接中",
                NodeConnectionState::Online => "在线",
                NodeConnectionState::Error => "错误",
            };
            format!(
                "{} · {} · {} · {status}",
                name,
                node.machine_id.as_deref().unwrap_or("尚未握手"),
                node.endpoint,
            )
            .into()
        })
        .collect::<Vec<_>>();
    ModelRc::new(VecModel::from(rows))
}
