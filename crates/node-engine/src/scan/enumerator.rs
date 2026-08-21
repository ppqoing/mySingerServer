//! 两种枚举器必须遵守的稳定输出契约。

use std::{collections::BTreeSet, io};

use dedup_core::{DisplayPath, NormalizedPath};
use dedup_node_store::ScannedPath;

use super::ScanError;

/// 把一个或多个绝对扫描根完整转换为稳定排序文件列表。
pub trait FileEnumerator {
    /// 枚举失败即失败整次扫描；调用方不会在中途切换另一实现。
    fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError>;

    /// 逐项交给有界下游；默认适配旧的完整 `Vec` 实现。
    ///
    /// 实现若能在遍历时产生文件，应覆盖此方法，让 `emit` 阻塞时停止继续枚举。
    fn enumerate_into(
        &self,
        roots: &[DisplayPath],
        emit: &mut dyn FnMut(ScannedPath) -> Result<(), ScanError>,
    ) -> Result<(), ScanError> {
        for row in self.enumerate(roots)? {
            emit(row)?;
        }
        Ok(())
    }
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

    fn enumerate_into(
        &self,
        roots: &[DisplayPath],
        emit: &mut dyn FnMut(ScannedPath) -> Result<(), ScanError>,
    ) -> Result<(), ScanError> {
        let mut emitted_error = None;
        let mut seen = BTreeSet::new();
        let result = self.walk_into(roots, |file| {
            let row = NormalizedPath::new(&file.path)
                .map_err(|error| ScanError::InvalidResult(error.to_string()))
                .and_then(|normalized| {
                    Ok(ScannedPath::new(
                        normalized,
                        DisplayPath::new(&file.path)
                            .map_err(|error| ScanError::InvalidResult(error.to_string()))?,
                        file.file_size,
                    ))
                })
                .and_then(|row| {
                    if seen.insert(row.normalized_path.clone()) {
                        emit(row)
                    } else {
                        Ok(())
                    }
                });
            match row {
                Ok(()) => Ok(()),
                Err(error) => {
                    emitted_error = Some(error);
                    Err(io::Error::new(io::ErrorKind::Interrupted, "枚举下游已停止"))
                }
            }
        });
        if let Some(error) = emitted_error {
            return Err(error);
        }
        result.map_err(|error| ScanError::Enumeration(error.to_string()))
    }
}
