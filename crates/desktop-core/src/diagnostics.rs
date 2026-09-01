//! Desktop Core 内部被消费结果的统一日志边界。

use std::fmt::Display;

/// 记录运行状态收束失败；业务结果继续返回给 UI，但错误不能静默丢失。
pub(crate) fn record_warning<T, E: Display>(
    result: Result<T, E>,
    component: &'static str,
    operation: &'static str,
) {
    match result {
        Ok(value) => drop(value),
        Err(error) => tracing::warn!(
            event = "background_task_failed",
            component,
            operation,
            error = %error,
            "Desktop 后台收束操作失败"
        ),
    }
}

/// 记录 UI 接收端已经结束产生的预期发送错误。
pub(crate) fn record_expected<T, E: Display>(
    result: Result<T, E>,
    component: &'static str,
    operation: &'static str,
) {
    match result {
        Ok(value) => drop(value),
        Err(error) => tracing::info!(
            event = "expected_condition",
            component,
            operation,
            reason = "ui_receiver_closed",
            error = %error,
            "UI 事件接收端已经关闭"
        ),
    }
}
