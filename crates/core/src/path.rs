//! Windows 绝对路径的稳定比较形式与目录组件边界。

use std::{
    fmt,
    path::{Path, PathBuf},
};

use serde::{Deserialize, Serialize};

use crate::CoreError;

/// 用于 SQLite 索引、排序和跨协议键的大小写无关 Windows 绝对路径。
#[derive(Clone, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(transparent)]
pub struct NormalizedPath(String);

/// 保留原始大小写并供界面显示和文件系统访问的 Windows 绝对路径。
#[derive(Clone, Debug, Deserialize, Eq, Hash, PartialEq, Serialize)]
#[serde(transparent)]
pub struct DisplayPath(PathBuf);

impl DisplayPath {
    /// 保存原始路径拼写；相对路径会在文件系统边界被拒绝。
    pub fn new(path: impl AsRef<Path>) -> Result<Self, CoreError> {
        let path = path.as_ref();
        if !path.is_absolute() {
            return Err(CoreError::InvalidPath(path.display().to_string()));
        }
        Ok(Self(path.to_path_buf()))
    }

    /// 返回供实际文件访问使用的原始路径。
    pub fn as_path(&self) -> &Path {
        &self.0
    }
}

impl NormalizedPath {
    /// 把盘符路径、UNC 路径或对应的 `\\?\` 形式规范为统一比较值。
    pub fn new(path: impl AsRef<Path>) -> Result<Self, CoreError> {
        let original = path.as_ref();
        let text = original
            .to_str()
            .ok_or_else(|| CoreError::InvalidPath(original.display().to_string()))?;
        normalize_windows_path(text).map(Self)
    }

    /// 返回供数据库和协议保存的规范字符串。
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// 按路径组件判断当前路径是否等于或位于给定目录下。
    pub fn is_within(&self, directory: &Self) -> bool {
        let path_components = Path::new(&self.0).components().collect::<Vec<_>>();
        let directory_components = Path::new(&directory.0).components().collect::<Vec<_>>();
        path_components.starts_with(&directory_components)
    }
}

impl fmt::Display for NormalizedPath {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

fn normalize_windows_path(input: &str) -> Result<String, CoreError> {
    let replaced = input.replace('/', "\\");
    let has_verbatim_unc_prefix = replaced
        .get(..8)
        .is_some_and(|prefix| prefix.eq_ignore_ascii_case(r"\\?\UNC\"));
    let without_verbatim = if has_verbatim_unc_prefix {
        format!(r"\\{}", &replaced[8..])
    } else if let Some(stripped) = replaced.strip_prefix(r"\\?\") {
        stripped.to_owned()
    } else {
        replaced
    };

    if is_drive_absolute(&without_verbatim) {
        normalize_drive_path(&without_verbatim)
    } else if without_verbatim.starts_with(r"\\") {
        normalize_unc_path(&without_verbatim)
    } else {
        Err(CoreError::InvalidPath(input.to_owned()))
    }
}

fn is_drive_absolute(path: &str) -> bool {
    let bytes = path.as_bytes();
    bytes.len() >= 3 && bytes[0].is_ascii_alphabetic() && bytes[1] == b':' && bytes[2] == b'\\'
}

fn normalize_drive_path(path: &str) -> Result<String, CoreError> {
    let drive = path[..2].to_ascii_uppercase();
    let components = collapse_components(&path[3..], path)?;
    if components.is_empty() {
        return Ok(format!(r"{drive}\"));
    }
    Ok(format!(r"{drive}\{}", components.join(r"\")))
}

fn normalize_unc_path(path: &str) -> Result<String, CoreError> {
    let mut raw = path[2..].split('\\').filter(|part| !part.is_empty());
    let server = raw
        .next()
        .ok_or_else(|| CoreError::InvalidPath(path.to_owned()))?;
    let share = raw
        .next()
        .ok_or_else(|| CoreError::InvalidPath(path.to_owned()))?;
    let remaining = raw.collect::<Vec<_>>().join(r"\");
    let components = collapse_components(&remaining, path)?;
    let root = format!(r"\\{}\{}", server.to_uppercase(), share.to_uppercase());
    if components.is_empty() {
        return Ok(format!(r"{root}\"));
    }
    Ok(format!(r"{root}\{}", components.join(r"\")))
}

fn collapse_components(raw: &str, original: &str) -> Result<Vec<String>, CoreError> {
    let mut components = Vec::new();
    for component in raw.split('\\').filter(|part| !part.is_empty()) {
        match component {
            "." => {}
            ".." => {
                if components.pop().is_none() {
                    return Err(CoreError::InvalidPath(original.to_owned()));
                }
            }
            value => components.push(value.to_uppercase()),
        }
    }
    Ok(components)
}
