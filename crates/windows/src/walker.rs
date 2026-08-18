//! 不依赖索引服务的 Windows 文件系统递归枚举。

use std::{fs, io, path::PathBuf};

use dedup_core::DisplayPath;

/// WindowsWalker 输出的原始文件路径和枚举时大小。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WalkedFile {
    /// 保留文件系统返回大小写的绝对路径。
    pub path: PathBuf,
    /// 枚举时读取的文件长度。
    pub file_size: u64,
}

/// 直接使用 Windows 文件系统目录枚举的始终可用实现。
#[derive(Clone, Copy, Debug, Default)]
pub struct WindowsWalker;

impl WindowsWalker {
    /// 递归读取每个根；目录符号链接不继续下钻，避免扫描环。
    pub fn walk(&self, roots: &[DisplayPath]) -> io::Result<Vec<WalkedFile>> {
        let mut files = Vec::new();
        let mut pending = roots
            .iter()
            .rev()
            .map(|root| root.as_path().to_path_buf())
            .collect::<Vec<_>>();
        while let Some(directory) = pending.pop() {
            let mut entries = fs::read_dir(directory)?.collect::<Result<Vec<_>, _>>()?;
            entries.sort_by_key(fs::DirEntry::path);
            for entry in entries {
                let file_type = entry.file_type()?;
                if file_type.is_dir() && !file_type.is_symlink() {
                    pending.push(entry.path());
                } else if file_type.is_file() {
                    files.push(WalkedFile {
                        path: entry.path(),
                        file_size: entry.metadata()?.len(),
                    });
                }
            }
        }
        Ok(files)
    }
}
