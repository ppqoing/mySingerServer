//! Slint 页面、视图模型绑定与用户交互状态。
#![warn(missing_docs)]

mod bindings;
mod models;

// 生成类型的文档由 Slint 源文件中的属性与回调说明承担；手写 Rust API 仍受 missing_docs 约束。
#[allow(missing_docs)]
mod generated {
    slint::include_modules!();
}

pub use generated::*;

pub use bindings::{UiBinding, apply_event, bind_commands};
