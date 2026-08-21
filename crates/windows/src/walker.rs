//! 不依赖索引服务的 Windows 文件系统递归枚举。

use std::{collections::VecDeque, fs, io, path::PathBuf};

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
        let mut root_paths = roots
            .iter()
            .map(|root| root.as_path().to_path_buf())
            .collect::<Vec<_>>();
        root_paths.sort_by_key(|path| path.to_string_lossy().to_uppercase());
        let mut pending_roots = VecDeque::from(root_paths);
        let mut stack = Vec::new();
        loop {
            if stack.is_empty() {
                let Some(root) = pending_roots.pop_front() else {
                    break;
                };
                stack.push(fs::read_dir(root)?);
            }
            let next = stack.last_mut().expect("非空目录迭代器栈").next();
            let Some(entry) = next else {
                stack.pop();
                continue;
            };
            let entry = entry?;
            let file_type = entry.file_type()?;
            if file_type.is_dir() && !file_type.is_symlink() {
                stack.push(fs::read_dir(entry.path())?);
            } else if file_type.is_file() {
                emit(WalkedFile {
                    path: entry.path(),
                    file_size: entry.metadata()?.len(),
                })?;
            }
        }
        Ok(())
    }
}
