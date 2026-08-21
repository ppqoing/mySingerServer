//! Node 本机路径的原始字符串保留、实际解析和网络盘拒绝边界。

use std::{
    iter::once,
    os::windows::ffi::OsStrExt,
    path::{Component, Path, PathBuf},
};

use dedup_core::CoreError;
use windows::{
    Win32::Storage::FileSystem::GetDriveTypeW,
    core::PCWSTR,
};

const DRIVE_REMOTE: u32 = 4;

/// 同时保存 Node 配置原文与只供本机访问的解析路径。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct LocalNodePath {
    raw: String,
    resolved: PathBuf,
}

impl LocalNodePath {
    /// 验证 Node 路径，并只在实际访问边界将相对路径解析到 `node.exe` 目录。
    ///
    /// 原始字符串不会被大小写、分隔符或绝对化操作改写；UNC 和映射网络盘会被拒绝。
    pub fn validate(executable_dir: &Path, raw: &str) -> Result<Self, CoreError> {
        Self::validate_with_drive_type(executable_dir, raw, system_drive_type)
    }

    /// 使用调用方提供的盘类型探针验证路径；用于隔离真实 Windows API 的行为测试。
    pub fn validate_with_drive_type<F>(
        executable_dir: &Path,
        raw: &str,
        drive_type: F,
    ) -> Result<Self, CoreError>
    where
        F: FnOnce(&Path) -> u32,
    {
        if raw.trim().is_empty() {
            return Err(invalid_local_path(raw, "路径不能为空"));
        }
        if is_unc_path(raw) {
            return Err(invalid_local_path(raw, "不支持 UNC 网络路径"));
        }

        let configured = Path::new(raw);
        if !configured.is_absolute()
            && (configured.has_root()
                || configured
                    .components()
                    .any(|component| matches!(component, Component::Prefix(_))))
        {
            return Err(invalid_local_path(raw, "不支持盘符相对或根目录相对路径"));
        }
        let resolved = if configured.is_absolute() {
            configured.to_path_buf()
        } else {
            executable_dir.join(configured)
        };
        if drive_type(&resolved) == DRIVE_REMOTE {
            return Err(invalid_local_path(raw, "不支持映射网络盘"));
        }

        Ok(Self {
            raw: raw.to_owned(),
            resolved,
        })
    }

    /// 返回未经过规范化或绝对化的配置字符串。
    pub fn raw(&self) -> &str {
        &self.raw
    }

    /// 返回只供创建目录和实际文件访问使用的本机解析路径。
    pub fn resolved(&self) -> &Path {
        &self.resolved
    }
}

fn is_unc_path(raw: &str) -> bool {
    let uppercase = raw.to_ascii_uppercase();
    (raw.starts_with(r"\\") && !raw.starts_with(r"\\?\"))
        || uppercase.starts_with(r"\\?\UNC\")
}

fn system_drive_type(path: &Path) -> u32 {
    let Some(root) = path.ancestors().filter(|ancestor| ancestor.has_root()).last() else {
        return 0;
    };
    let wide = root
        .as_os_str()
        .encode_wide()
        .chain(once(0))
        .collect::<Vec<_>>();
    // SAFETY: 指针来自 NUL 结尾的 UTF-16 根路径，调用不保留该指针。
    unsafe { GetDriveTypeW(PCWSTR(wide.as_ptr())) }
}

fn invalid_local_path(raw: &str, reason: &str) -> CoreError {
    CoreError::InvalidPath(format!("{raw}: {reason}"))
}
