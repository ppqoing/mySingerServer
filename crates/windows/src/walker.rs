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
        self.walk_into(roots, |file| {
            files.push(file);
            Ok(())
        })?;
        Ok(files)
    }

    /// 遍历时逐项交给调用方；回调阻塞时不继续读取后续目录项。
    pub fn walk_into(
        &self,
        roots: &[DisplayPath],
        mut emit: impl FnMut(WalkedFile) -> io::Result<()>,
    ) -> io::Result<()> {
        enum Pending {
            Directory(PathBuf),
            File(WalkedFile),
        }
        let mut root_paths = roots
            .iter()
            .map(|root| root.as_path().to_path_buf())
            .collect::<Vec<_>>();
        root_paths.sort_by_key(|path| path.to_string_lossy().to_uppercase());
        let mut pending = root_paths
            .into_iter()
            .rev()
            .map(Pending::Directory)
            .collect::<Vec<_>>();
        while let Some(next) = pending.pop() {
            let directory = match next {
                Pending::Directory(directory) => directory,
                Pending::File(file) => {
                    emit(file)?;
                    continue;
                }
            };
            let mut entries = fs::read_dir(directory)?.collect::<Result<Vec<_>, _>>()?;
            entries.sort_by_key(|entry| entry.path().to_string_lossy().to_uppercase());
            for entry in entries.into_iter().rev() {
                let file_type = entry.file_type()?;
                if file_type.is_dir() && !file_type.is_symlink() {
                    pending.push(Pending::Directory(entry.path()));
                } else if file_type.is_file() {
                    pending.push(Pending::File(WalkedFile {
                        path: entry.path(),
                        file_size: entry.metadata()?.len(),
                    }));
                }
            }
        }
        Ok(())
    }
}
