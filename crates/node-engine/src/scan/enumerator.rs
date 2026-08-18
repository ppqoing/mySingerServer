//! 两种枚举器必须遵守的稳定输出契约。

use dedup_core::{DisplayPath, NormalizedPath};
use dedup_node_store::ScannedPath;

use super::ScanError;

/// 把一个或多个绝对扫描根完整转换为稳定排序文件列表。
pub trait FileEnumerator {
    /// 枚举失败即失败整次扫描；调用方不会在中途切换另一实现。
    fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError>;
}

impl FileEnumerator for dedup_windows::WindowsWalker {
    fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        let mut rows = self
            .walk(roots)
            .map_err(|error| ScanError::Enumeration(error.to_string()))?
            .into_iter()
            .map(|file| {
                Ok(ScannedPath::new(
                    NormalizedPath::new(&file.path)
                        .map_err(|error| ScanError::InvalidResult(error.to_string()))?,
                    DisplayPath::new(&file.path)
                        .map_err(|error| ScanError::InvalidResult(error.to_string()))?,
                    file.file_size,
                ))
            })
            .collect::<Result<Vec<_>, ScanError>>()?;
        rows.sort_by(|left, right| left.normalized_path.cmp(&right.normalized_path));
        rows.dedup_by(|left, right| left.normalized_path == right.normalized_path);
        Ok(rows)
    }
}
