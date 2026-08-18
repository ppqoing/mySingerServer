//! 流式 MD5 内容键计算。

use std::{fs::File, io::Read, path::Path};

use md5::{Digest, Md5};

use super::ScanError;

/// 扫描引擎使用的文件 MD5 边界。
pub trait FileHasher {
    /// 完整读取一个缓存未命中文件并返回 16 字节 MD5。
    fn md5(&mut self, path: &Path) -> Result<[u8; 16], ScanError>;
}

/// 使用 1 MiB 缓冲区顺序读取文件的生产实现。
#[derive(Clone, Copy, Debug, Default)]
pub struct SystemMd5;

impl FileHasher for SystemMd5 {
    fn md5(&mut self, path: &Path) -> Result<[u8; 16], ScanError> {
        md5_file(path)
    }
}

/// 流式计算文件 MD5，不把媒体文件整体读入内存。
pub fn md5_file(path: &Path) -> Result<[u8; 16], ScanError> {
    let mut file = File::open(path)?;
    let mut digest = Md5::new();
    let mut buffer = vec![0_u8; 1024 * 1024];
    loop {
        let read = file.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        digest.update(&buffer[..read]);
    }
    Ok(digest.finalize().into())
}

/// 计算已在内存中的测试或协议载荷 MD5。
pub fn md5_bytes(bytes: &[u8]) -> [u8; 16] {
    Md5::digest(bytes).into()
}
