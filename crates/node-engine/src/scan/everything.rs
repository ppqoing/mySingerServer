//! Everything 1.4/1.5 Window Message IPC 枚举实现。

use std::path::PathBuf;

use dedup_core::{DisplayPath, NormalizedPath};
use dedup_node_store::ScannedPath;
use everything_ipc::wm::{EverythingClient, RequestFlags};

use super::{FileEnumerator, ScanError};

/// 用户明确选择 Everything 时使用的枚举器；不可用会直接返回错误。
#[derive(Clone, Copy, Debug, Default)]
pub struct EverythingEnumerator;

impl FileEnumerator for EverythingEnumerator {
    fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        let client = EverythingClient::new()
            .map_err(|error| ScanError::Enumeration(format!("Everything IPC 不可用: {error}")))?;
        let mut rows = Vec::new();
        for root in roots {
            let normalized_root = NormalizedPath::new(root.as_path())
                .map_err(|error| ScanError::InvalidResult(error.to_string()))?;
            let query = format!(r#"file: path:"{}""#, root.as_path().display());
            let list = client
                .query_wait(&query)
                .request_flags(RequestFlags::FullPathAndFileName | RequestFlags::Size)
                .call()
                .map_err(|error| ScanError::Enumeration(format!("Everything 查询失败: {error}")))?;
            for item in list.iter() {
                let path = item
                    .get_string(RequestFlags::FullPathAndFileName)
                    .map(PathBuf::from)
                    .ok_or_else(|| ScanError::InvalidResult("Everything 缺少完整路径".into()))?;
                let normalized = NormalizedPath::new(&path)
                    .map_err(|error| ScanError::InvalidResult(error.to_string()))?;
                if !normalized.is_within(&normalized_root) {
                    continue;
                }
                rows.push(ScannedPath::new(
                    normalized,
                    DisplayPath::new(path)
                        .map_err(|error| ScanError::InvalidResult(error.to_string()))?,
                    item.get_size(RequestFlags::Size).ok_or_else(|| {
                        ScanError::InvalidResult("Everything 缺少文件大小".into())
                    })?,
                ));
            }
        }
        rows.sort_by(|left, right| left.normalized_path.cmp(&right.normalized_path));
        rows.dedup_by(|left, right| left.normalized_path == right.normalized_path);
        Ok(rows)
    }
}
