//! 按内容 MD5 定位并安全写入可复用的视频联系表缓存。

use std::{
    fmt::Write as _,
    fs::{self, File},
    io::{self, Write},
    path::{Path, PathBuf},
};

/// 一份由固定 16 字节 MD5 唯一确定的联系表缓存目标。
#[derive(Clone, Debug)]
pub(crate) struct ContactSheetCacheEntry {
    final_path: PathBuf,
    relative_path: String,
}

impl ContactSheetCacheEntry {
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
        let partial_path = parent.join(format!(".{file_name}.{safe_item}.partial"));
        let mut partial = File::create(&partial_path)?;
        partial.write_all(jpeg)?;
        partial.flush()?;
        partial.sync_all()?;
        Ok(partial_path)
    }
}
