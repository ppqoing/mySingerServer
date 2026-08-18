//! 领域与配置边界共享的精简错误类型。

use thiserror::Error;

/// 领域值、配置和路径在进入内部流程前可能返回的错误。
#[derive(Debug, Error)]
pub enum CoreError {
    /// 机器 ID 不是 64 位小写 SHA-256 十六进制字符串。
    #[error("机器 ID 必须是 64 位小写 SHA-256 十六进制字符串")]
    InvalidMachineId,
    /// SMBIOS 没有提供任何可用于计算机器身份的物理字段。
    #[error("SMBIOS 未提供系统 UUID、系统序列号或主板序列号")]
    MissingPhysicalIdentity,
    /// 相似度阈值超出算法允许范围。
    #[error("阈值 {field} 无效: {reason}")]
    InvalidThreshold {
        /// 配置中的固定字段名。
        field: &'static str,
        /// 简短的范围说明。
        reason: &'static str,
    },
    /// 普通配置字段不满足启动边界。
    #[error("配置 {field} 无效: {reason}")]
    InvalidConfig {
        /// 配置中的固定字段名。
        field: &'static str,
        /// 简短的原因说明。
        reason: &'static str,
    },
    /// TOML 文本无法解码为强类型配置。
    #[error("TOML 配置解析失败: {0}")]
    TomlDecode(#[from] toml::de::Error),
    /// 强类型配置无法编码为 TOML 文本。
    #[error("TOML 配置写入失败: {0}")]
    TomlEncode(#[from] toml::ser::Error),
    /// Windows 路径不是本系统可比较的绝对路径。
    #[error("无效的 Windows 绝对路径: {0}")]
    InvalidPath(String),
    /// 可执行文件路径没有可用的父目录。
    #[error("无效的可执行文件路径: {0}")]
    InvalidExecutablePath(String),
    /// Windows 固件表 API 调用失败。
    #[error("Windows 固件表读取失败，错误码 {code}")]
    FirmwareApi {
        /// `GetLastError` 返回的 Win32 错误码。
        code: u32,
    },
    /// SMBIOS 固件表的结构长度或字符串区不完整。
    #[error("SMBIOS 数据无效: {0}")]
    InvalidSmbios(&'static str),
}
