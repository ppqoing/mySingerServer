//! desktop-core 强类型状态到 Slint 行模型的单向映射。

use dedup_desktop_core::view_state::{
    DesktopViewState, NodeConnectionState, TaskView, ViewTaskState,
};
use slint::{Color, ModelRc, SharedString, VecModel};

use crate::{UiNodeRow, UiTaskRow};

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
