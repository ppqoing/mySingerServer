//! 按内容 MD5 定位并安全写入可复用的视频联系表缓存。

use std::{
    fmt::Write as _,
    fs::{self, File},
    io::{self, Write},
    path::{Path, PathBuf},
};

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

    pub(crate) fn exists(&self) -> bool {
        self.final_path.is_file()
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
        if !self.exists() {
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
