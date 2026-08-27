//! 不依赖后台线程的定长滚动日志 writer，供三个进程复用。

use std::{
    fs::{self, File, OpenOptions},
    io::{self, Write},
    path::{Path, PathBuf},
};

/// 生产日志单文件固定上限 20 MiB。
pub const DEFAULT_LOG_FILE_BYTES: u64 = 20 * 1024 * 1024;
/// 生产日志包含当前文件在内固定保留 10 个文件。
pub const DEFAULT_LOG_FILE_COUNT: usize = 10;

/// 在下一次写入越过边界前轮转的同步文件 writer。
///
/// 文件名为 `<prefix>.log`、`<prefix>.1.log` 至 `<prefix>.(keep-1).log`。
/// 一个大于上限的单条日志仍完整写入当前文件，下一次写入前再轮转。
pub struct SizeRotatingWriter {
    directory: PathBuf,
    prefix: String,
    max_bytes: u64,
    keep_files: usize,
    current: Option<File>,
    current_bytes: u64,
}

impl SizeRotatingWriter {
    /// 创建生产默认的 20 MiB × 10 日志 writer。
    pub fn production(
        directory: impl Into<PathBuf>,
        prefix: impl Into<String>,
    ) -> io::Result<Self> {
        Self::new(
            directory,
            prefix,
            DEFAULT_LOG_FILE_BYTES,
            DEFAULT_LOG_FILE_COUNT,
        )
    }

    /// 创建显式边界的 writer；较小数值只用于单元测试。
    pub fn new(
        directory: impl Into<PathBuf>,
        prefix: impl Into<String>,
        max_bytes: u64,
        keep_files: usize,
    ) -> io::Result<Self> {
        if max_bytes == 0 || keep_files == 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "日志大小和保留数量必须大于零",
            ));
        }
        let directory = directory.into();
        fs::create_dir_all(&directory)?;
        let prefix = prefix.into();
        let path = log_path(&directory, &prefix, 0);
        let current = OpenOptions::new().create(true).append(true).open(path)?;
        let current_bytes = current.metadata()?.len();
        Ok(Self {
            directory,
            prefix,
            max_bytes,
            keep_files,
            current: Some(current),
            current_bytes,
        })
    }

    fn rotate(&mut self) -> io::Result<()> {
        if let Some(mut current) = self.current.take() {
            current.flush()?;
        }
        if self.keep_files > 1 {
            let oldest = log_path(&self.directory, &self.prefix, self.keep_files - 1);
            if oldest.exists() {
                fs::remove_file(oldest)?;
            }
            for index in (1..self.keep_files - 1).rev() {
                let source = log_path(&self.directory, &self.prefix, index);
                if source.exists() {
                    fs::rename(source, log_path(&self.directory, &self.prefix, index + 1))?;
                }
            }
            let current_path = log_path(&self.directory, &self.prefix, 0);
            if current_path.exists() {
                fs::rename(current_path, log_path(&self.directory, &self.prefix, 1))?;
            }
        } else {
            let current_path = log_path(&self.directory, &self.prefix, 0);
            if current_path.exists() {
                fs::remove_file(current_path)?;
            }
        }
        self.current = Some(OpenOptions::new().create(true).append(true).open(log_path(
            &self.directory,
            &self.prefix,
            0,
        ))?);
        self.current_bytes = 0;
        Ok(())
    }
}

impl Write for SizeRotatingWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        if self.current_bytes > 0
            && self.current_bytes.saturating_add(buffer.len() as u64) > self.max_bytes
        {
            self.rotate()?;
        }
        let written = self
            .current
            .as_mut()
            .expect("日志 writer 始终持有当前文件")
            .write(buffer)?;
        self.current_bytes = self.current_bytes.saturating_add(written as u64);
        Ok(written)
    }

    fn flush(&mut self) -> io::Result<()> {
        self.current
            .as_mut()
            .expect("日志 writer 始终持有当前文件")
            .flush()
    }
}

fn log_path(directory: &Path, prefix: &str, index: usize) -> PathBuf {
    if index == 0 {
        directory.join(format!("{prefix}.log"))
    } else {
        directory.join(format!("{prefix}.{index}.log"))
    }
}

#[cfg(test)]
mod tests {
    use std::{fs, io::Write};

    use super::SizeRotatingWriter;

    /// 防止轮转后继续增长或保留超过固定文件数量。
    #[test]
    fn rotates_before_crossing_boundary_and_keeps_fixed_count() {
        let directory = tempfile::tempdir().unwrap();
        let mut writer = SizeRotatingWriter::new(directory.path(), "node", 4, 3).unwrap();
        writer.write_all(b"1111").unwrap();
        writer.write_all(b"2222").unwrap();
        writer.write_all(b"3333").unwrap();
        writer.write_all(b"4444").unwrap();
        writer.flush().unwrap();

        assert_eq!(
            fs::read(directory.path().join("node.log")).unwrap(),
            b"4444"
        );
        assert_eq!(
            fs::read(directory.path().join("node.1.log")).unwrap(),
            b"3333"
        );
        assert_eq!(
            fs::read(directory.path().join("node.2.log")).unwrap(),
            b"2222"
        );
        assert!(!directory.path().join("node.3.log").exists());
    }
}
