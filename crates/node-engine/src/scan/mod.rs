//! 文件列表进入 SQLite 内容模型和一筛结果的单一扫描流程。

mod engine;
mod enumerator;
mod everything;
mod hash;

pub use dedup_windows::WindowsWalker;
pub use engine::{
    ScanEngine, ScanOptions, ScanSummary, Stage1Processor, Stage1Request,
    WorkerPoolStage1Processor, begin_scan_task,
};
pub use enumerator::FileEnumerator;
pub use everything::EverythingEnumerator;
pub(crate) use everything::{PreferredEverythingEnumerator, ensure_everything_ready};
pub use hash::{FileHasher, SystemMd5, md5_bytes, md5_file};

use thiserror::Error;

/// 枚举、文件读取、SQLite 或一筛编排失败。
#[derive(Debug, Error)]
pub enum ScanError {
    /// 当前枚举器无法完成整次文件列表。
    #[error("文件枚举失败: {0}")]
    Enumeration(String),
    /// 文件读取或联系表写入失败。
    #[error("文件 IO 失败: {0}")]
    Io(#[from] std::io::Error),
    /// SQLite 扫描任务或内容事务失败。
    #[error(transparent)]
    Store(#[from] dedup_node_store::StoreError),
    /// 枚举器返回了无效路径或字段。
    #[error("枚举结果无效: {0}")]
    InvalidResult(String),
    /// Worker 一筛结果与请求不匹配。
    #[error("一筛处理失败: {0}")]
    Stage1(String),
}
