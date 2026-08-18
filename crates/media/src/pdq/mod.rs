//! 固定 Meta 上游 commit 的 256 位 PDQ 纯 Rust 等价实现。

mod downscale;
mod median;
mod quality;
mod transform;

use crate::GrayImage;
use downscale::downsample_64;
use median::torben;
use quality::image_domain_quality;
use transform::dct_64_to_16;

/// 数据库与协议统一保存的 PDQ 规范字节序。
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct PdqHash([u8; 32]);

impl PdqHash {
    /// 从已经采用规范字节序的 32 字节值创建 hash。
    pub const fn from_bytes(bytes: [u8; 32]) -> Self {
        Self(bytes)
    }

    /// 返回供数据库 BLOB 和 Protobuf 直接保存的规范字节。
    pub const fn as_bytes(&self) -> &[u8; 32] {
        &self.0
    }

    /// 输出与 Meta 命令行工具一致的 64 位小写十六进制文本。
    pub fn to_hex(&self) -> String {
        const HEX: &[u8; 16] = b"0123456789abcdef";
        let mut result = String::with_capacity(64);
        for byte in self.0 {
            result.push(HEX[(byte >> 4) as usize] as char);
            result.push(HEX[(byte & 0x0f) as usize] as char);
        }
        result
    }

    /// 计算两个 256 位 hash 的逐位汉明距离。
    pub fn hamming_distance(&self, other: &Self) -> u16 {
        self.0
            .iter()
            .zip(other.0)
            .map(|(left, right)| (left ^ right).count_ones() as u16)
            .sum()
    }

    /// 集中转换上游内部的低 word 优先布局，其他模块不得再次解释字序。
    fn from_upstream_words(words: [u16; 16]) -> Self {
        let mut bytes = [0_u8; 32];
        for (index, word) in words.into_iter().rev().enumerate() {
            bytes[index * 2..index * 2 + 2].copy_from_slice(&word.to_be_bytes());
        }
        Self(bytes)
    }
}

/// 一筛使用的 PDQ 结果及上游定义的图像质量。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PdqResult {
    /// 256 位感知 hash。
    pub hash: PdqHash,
    /// `0..=100` 的图像域 Quality。
    pub quality: u8,
}

/// 使用固定灰度输入计算 Meta PDQ hash 和 Quality。
pub fn pdq_hash(image: &GrayImage) -> PdqResult {
    if image.width() < 5 || image.height() < 5 {
        return PdqResult {
            hash: PdqHash::default(),
            quality: 0,
        };
    }

    let downsampled = downsample_64(image);
    let quality = image_domain_quality(&downsampled);
    let transformed = dct_64_to_16(&downsampled);
    let threshold = torben(&transformed);
    let mut words = [0_u16; 16];

    for (index, value) in transformed.into_iter().enumerate() {
        if value > threshold {
            words[index >> 4] |= 1 << (index & 15);
        }
    }

    PdqResult {
        hash: PdqHash::from_upstream_words(words),
        quality,
    }
}
