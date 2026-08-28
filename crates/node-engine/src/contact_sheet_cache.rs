//! 按内容 MD5 定位并安全写入可复用的视频联系表缓存。

use std::{
    fmt::Write as _,
    fs::{self, File},
    io::{self, Write},
    path::{Path, PathBuf},
};

use dedup_media::decode_contact_sheet;
use dedup_node_store::{ContentId, FeatureWrite, NodeStore};

use crate::{
    artifact_registry::{ArtifactKind, ArtifactLease, RegenerableArtifactRegistry},
    disk_full_cleanup::{DiskFullCleaner, write_planned_artifact_with_disk_full_cleanup},
};

/// 一份由固定 16 字节 MD5 唯一确定的联系表缓存目标。
#[derive(Clone, Debug)]
pub(crate) struct ContactSheetCacheEntry {
    final_path: PathBuf,
    relative_path: String,
}

impl ContactSheetCacheEntry {
    /// 根据内容 MD5 创建固定的两级联系表缓存路径。
    pub(crate) fn from_md5(contact_sheet_root: &Path, md5: [u8; 16]) -> Self {
        let mut digest = String::with_capacity(32);
        for byte in md5 {
            write!(&mut digest, "{byte:02x}").expect("writing to String cannot fail");
        }
        let prefix = &digest[..2];
        Self {
            final_path: contact_sheet_root
                .join(prefix)
                .join(format!("{digest}.jpg")),
            relative_path: format!("contact-sheets/{prefix}/{digest}.jpg"),
        }
    }

    /// 校验 SQLite 引用、根目录边界和固定六槽 JPEG，避免仅凭文件存在误判命中。
    pub(crate) fn is_valid(&self, stored_relative_path: Option<&str>) -> bool {
        stored_relative_path == Some(self.relative_path.as_str())
            && self
                .final_path
                .parent()
                .and_then(Path::parent)
                .is_some_and(|root| {
                    let Ok(canonical_root) = fs::canonicalize(root) else {
                        return false;
                    };
                    let Ok(canonical_file) = fs::canonicalize(&self.final_path) else {
                        return false;
                    };
                    canonical_file.starts_with(canonical_root)
                })
            && Self::is_valid_file(&self.final_path)
    }

    /// 判断文件是可解码的固定联系表 JPEG；损坏或非 JPEG 均视为缺失。
    pub(crate) fn is_valid_file(path: &Path) -> bool {
        fs::read(path)
            .ok()
            .and_then(|bytes| decode_contact_sheet(&bytes).ok())
            .is_some()
    }

    pub(crate) fn same_target(&self, other: &Self) -> bool {
        self.final_path == other.final_path
    }

    pub(crate) fn final_path(&self) -> &Path {
        &self.final_path
    }

    pub(crate) fn relative_path(&self) -> &str {
        &self.relative_path
    }

    pub(crate) fn write_partial(&self, item_id: &str, jpeg: &[u8]) -> io::Result<PathBuf> {
        let partial_path = self.partial_path(item_id)?;
        write_jpeg(&partial_path, jpeg)?;
        Ok(partial_path)
    }

    pub(crate) fn write_partial_with_disk_full_cleanup(
        &self,
        item_id: &str,
        jpeg: &[u8],
        registry: &RegenerableArtifactRegistry,
        cleaner: &DiskFullCleaner,
        store: &mut NodeStore,
    ) -> io::Result<(PathBuf, ArtifactLease)> {
        let partial_path = self.partial_path(item_id)?;
        let (_, lease) = write_planned_artifact_with_disk_full_cleanup(
            cleaner,
            store,
            registry,
            &partial_path,
            ArtifactKind::OrphanTemporary,
            || write_jpeg(&partial_path, jpeg),
        )?;
        Ok((partial_path, lease))
    }

    /// 用 Worker 返回的 JPEG 替换缺失或损坏联系表，并同步修复 SQLite 引用。
    pub(crate) fn publish_rebuilt(
        &self,
        item_id: &str,
        jpeg: &[u8],
        store: &mut NodeStore,
        content_id: ContentId,
    ) -> io::Result<()> {
        let partial_path = self.write_partial(item_id, jpeg)?;
        let stale_path = self
            .final_path
            .with_extension(format!("jpg.{item_id}.stale"));
        let had_existing = self.final_path.is_file();
        if had_existing {
            if stale_path.exists() {
                fs::remove_file(&stale_path)?;
            }
            fs::rename(&self.final_path, &stale_path)?;
        }
        if let Err(error) = fs::rename(&partial_path, &self.final_path) {
            let _ = fs::remove_file(&partial_path);
            if had_existing {
                let _ = fs::rename(&stale_path, &self.final_path);
            }
            return Err(error);
        }
        let committed = store
            .commit_feature_result(
                content_id,
                None,
                FeatureWrite::ContactSheet(self.relative_path.clone()),
            )
            .map_err(io::Error::other);
        if let Err(error) = committed {
            let _ = fs::remove_file(&self.final_path);
            if had_existing {
                let _ = fs::rename(&stale_path, &self.final_path);
            }
            return Err(error);
        }
        if had_existing {
            let _ = fs::remove_file(stale_path);
        }
        Ok(())
    }

    /// 已有有效文件时只修复 SQLite 联系表引用，不重写 JPEG。
    pub(crate) fn repair_reference(
        &self,
        store: &mut NodeStore,
        content_id: ContentId,
    ) -> io::Result<()> {
        if !Self::is_valid_file(&self.final_path) {
            return Err(io::Error::new(io::ErrorKind::NotFound, "联系表文件不存在"));
        }
        store
            .commit_feature_result(
                content_id,
                None,
                FeatureWrite::ContactSheet(self.relative_path.clone()),
            )
            .map(|_| ())
            .map_err(io::Error::other)
    }

    fn partial_path(&self, item_id: &str) -> io::Result<PathBuf> {
        let parent = self
            .final_path
            .parent()
            .expect("MD5 contact sheet target always has a parent");
        fs::create_dir_all(parent)?;
        let safe_item = item_id
            .chars()
            .map(|character| {
                if character.is_ascii_alphanumeric() || matches!(character, '-' | '_') {
                    character
                } else {
                    '_'
                }
            })
            .collect::<String>();
        let file_name = self
            .final_path
            .file_name()
            .expect("MD5 contact sheet target always has a file name")
            .to_string_lossy();
        Ok(parent.join(format!(".{file_name}.{safe_item}.partial")))
    }
}

fn write_jpeg(path: &Path, jpeg: &[u8]) -> io::Result<()> {
    let mut partial = File::create(path)?;
    partial.write_all(jpeg)?;
    partial.flush()?;
    partial.sync_all()
}

#[cfg(test)]
mod tests {
    use super::*;
    use dedup_media::{Rgb24Image, encode_contact_sheet};
    use tempfile::tempdir;

    /// 生成固定尺寸的可解码联系表，作为本地 artifact 校验夹具。
    fn valid_jpeg() -> Vec<u8> {
        let frames: [Option<Rgb24Image>; 6] = std::array::from_fn(|slot| {
            Some(Rgb24Image::new(8, 8, vec![slot as u8; 8 * 8 * 3]).unwrap())
        });
        encode_contact_sheet(&frames, 320, 180).unwrap()
    }

    /// 联系表命中必须同时满足派生相对路径和可解码 JPEG。
    #[test]
    fn validates_derived_path_and_jpeg_contents() {
        let directory = tempdir().unwrap();
        let entry = ContactSheetCacheEntry::from_md5(directory.path(), [0xAB; 16]);
        fs::create_dir_all(entry.final_path().parent().unwrap()).unwrap();
        fs::write(entry.final_path(), valid_jpeg()).unwrap();
        assert!(entry.is_valid(Some(entry.relative_path())));
        assert!(!entry.is_valid(Some("contact-sheets/ab/not-the-md5.jpg")));

        fs::write(entry.final_path(), b"damaged-jpeg").unwrap();
        assert!(!entry.is_valid(Some(entry.relative_path())));
    }

    /// 联系表目录中的派生文件缺失时不能被空路径或普通文件误判为命中。
    #[test]
    fn missing_or_empty_contact_sheet_is_not_valid() {
        let directory = tempdir().unwrap();
        let entry = ContactSheetCacheEntry::from_md5(directory.path(), [0xCD; 16]);
        assert!(!entry.is_valid(Some(entry.relative_path())));
        fs::create_dir_all(entry.final_path().parent().unwrap()).unwrap();
        fs::write(entry.final_path(), []).unwrap();
        assert!(!entry.is_valid(Some(entry.relative_path())));
    }
}
