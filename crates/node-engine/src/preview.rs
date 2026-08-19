//! 节点预览读取：图片直接分块读取原文件，视频只读取已有 JPEG 联系表。

use std::{
    fs::File,
    io::{Read, Seek, SeekFrom},
    path::{Path, PathBuf},
};

use dedup_core::{LocationKey, MediaKind};
use dedup_node_store::{NodeStore, StoreError};
use thiserror::Error;

const MAX_PREVIEW_CHUNK: usize = 1024 * 1024;

/// 管理端可请求的两种预览来源。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PreviewKind {
    /// 图片原文件；节点不为图片生成缩略图。
    Original,
    /// 视频一筛时已经生成的 JPEG 多帧联系表。
    ContactSheet,
}

/// 一次有界预览读取的协议无关结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PreviewChunk {
    /// 本块在文件中的起始偏移。
    pub offset: u64,
    /// 最多 1 MiB 的原始文件字节。
    pub data: Vec<u8>,
    /// 本块结束后是否已经到达文件末尾。
    pub eof: bool,
}

/// 预览读取边界错误。
#[derive(Debug, Error)]
pub enum PreviewError {
    /// SQLite 位置或联系表查询失败。
    #[error(transparent)]
    Store(#[from] StoreError),
    /// 原文件或联系表读取失败。
    #[error(transparent)]
    Io(#[from] std::io::Error),
    /// 位置不存在或已经失活。
    #[error("文件位置不存在或已经失活")]
    NotFound,
    /// 文件类别与请求的预览来源不匹配。
    #[error("该媒体类型不支持请求的预览来源")]
    UnsupportedKind,
    /// 请求块大小超过协议固定边界。
    #[error("预览块不能超过 1 MiB")]
    ChunkTooLarge,
}

/// 无状态预览服务，只持有应用 `data/cache` 根路径。
pub struct PreviewService {
    cache_root: PathBuf,
}

impl PreviewService {
    /// 创建不主动生成任何缓存的预览读取器。
    pub fn new(cache_root: impl Into<PathBuf>) -> Self {
        Self {
            cache_root: cache_root.into(),
        }
    }

    /// 读取当前活动位置对应的原图或已存在视频联系表。
    pub fn read(
        &self,
        store: &NodeStore,
        location: &LocationKey,
        kind: PreviewKind,
        offset: u64,
        max_bytes: usize,
    ) -> Result<PreviewChunk, PreviewError> {
        if max_bytes > MAX_PREVIEW_CHUNK {
            return Err(PreviewError::ChunkTooLarge);
        }
        let active = store.active_file(location)?.ok_or(PreviewError::NotFound)?;
        let path = match (active.media_kind, kind) {
            (MediaKind::Image, PreviewKind::Original) => {
                active.display_path.as_path().to_path_buf()
            }
            (MediaKind::Video, PreviewKind::ContactSheet) => {
                let relative = store
                    .contact_sheet_path(active.content_id)?
                    .ok_or(PreviewError::NotFound)?;
                self.cache_root.join(relative)
            }
            _ => return Err(PreviewError::UnsupportedKind),
        };
        read_chunk(&path, offset, max_bytes)
    }
}

fn read_chunk(path: &Path, offset: u64, max_bytes: usize) -> Result<PreviewChunk, PreviewError> {
    let mut file = File::open(path)?;
    let length = file.metadata()?.len();
    file.seek(SeekFrom::Start(offset))?;
    let mut data = Vec::with_capacity(max_bytes);
    file.by_ref()
        .take(max_bytes as u64)
        .read_to_end(&mut data)?;
    Ok(PreviewChunk {
        offset,
        eof: offset.saturating_add(data.len() as u64) >= length,
        data,
    })
}
