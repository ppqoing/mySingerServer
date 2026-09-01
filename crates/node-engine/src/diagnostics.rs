//! Node Engine 内部被消费错误的统一日志边界。

use std::fmt::Display;

/// 记录必须继续执行、但不能静默丢失的真实错误。
pub(crate) fn record_error<T, E: Display>(
    result: Result<T, E>,
    component: &'static str,
    operation: &'static str,
) {
    match result {
        Ok(value) => drop(value),
        Err(error) => tracing::error!(
            event = "background_task_failed",
            component,
            operation,
            error = %error,
            "后台操作失败"
        ),
    }
}

/// 记录局部清理或诊断更新失败；主业务结果已经确定，调用方继续收束。
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
            "后台收束操作失败"
        ),
    }
}

/// 记录由正常关闭、取消或接收端先结束所产生的预期错误。
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
            reason = "receiver_closed",
            error = %error,
            "接收端已结束，结果不再投递"
        ),
    }
}

/// 记录 oneshot 或标准通道发送失败；错误载荷是未送达的值，不是可格式化错误。
pub(crate) fn record_closed<T>(
    result: Result<(), T>,
    component: &'static str,
    operation: &'static str,
) {
    if let Err(unsent_value) = result {
        drop(unsent_value);
        tracing::info!(
            event = "expected_condition",
            component,
            operation,
            reason = "receiver_closed",
            error = "channel receiver closed",
            "接收端已结束，结果不再投递"
        );
    }
}
