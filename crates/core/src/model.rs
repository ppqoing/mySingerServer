//! 配置、协议和数据库共同使用的简单领域枚举。

use serde::{Deserialize, Serialize};

/// 用户确认删除时选择的实际文件处理方式。
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum DeleteMode {
    /// 默认把文件送入 Windows 回收站，允许用户恢复。
    #[default]
    RecycleBin,
    /// 在身份复核通过后永久删除文件。
    Permanent,
}

/// 节点扫描目录时使用的文件枚举实现。
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum EnumeratorKind {
    /// 使用 Rust Windows 文件系统遍历器。
    #[default]
    WindowsWalker,
    /// 使用 Everything 本机 IPC 查询。
    Everything,
}
