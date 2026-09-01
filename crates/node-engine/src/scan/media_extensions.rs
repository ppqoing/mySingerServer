//! 把 Node 图片、视频扩展名配置编译为枚举器共享的只读匹配集合。

use std::{collections::BTreeSet, path::Path};

use dedup_core::NodeConfig;

/// 一次扫描使用的扩展名并集，提供路径匹配和 Everything 查询片段。
#[derive(Clone, Debug)]
pub struct MediaExtensionFilter {
    extensions: BTreeSet<String>,
}

impl MediaExtensionFilter {
    /// 合并图片和视频配置；有序集合同时完成去重和稳定输出。
    pub fn from_config(config: &NodeConfig) -> Self {
        Self {
            extensions: config
                .image_extensions
                .iter()
                .chain(&config.video_extensions)
                .cloned()
                .collect(),
        }
    }

    /// 仅按最后一个扩展名进行大小写无关匹配，不读取文件内容。
    pub fn matches(&self, path: &Path) -> bool {
        path.extension()
            .and_then(|extension| extension.to_str())
            .map(|extension| extension.to_ascii_lowercase())
            .is_some_and(|extension| self.extensions.contains(&extension))
    }

    /// 返回 Everything `ext:` 使用的分号列表；空集合不产生查询。
    pub fn everything_extensions(&self) -> Option<String> {
        (!self.extensions.is_empty()).then(|| {
            self.extensions
                .iter()
                .cloned()
                .collect::<Vec<_>>()
                .join(";")
        })
    }
}
