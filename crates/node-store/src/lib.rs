//! 节点 SQLite schema、查询、事务、分析结果和同步 outbox。
#![warn(missing_docs)]

mod content;
mod features;
mod open;
mod outbox;
mod rows;
mod snapshot;

pub use open::{NodeStore, StoreError};
pub use rows::{
    CacheLookup, CompleteStage1, CompleteStage2, ContentId, ContentRecord, FeatureWrite,
    ImageStage1Fields, ScannedPath, SnapshotPage, SnapshotRow, SyncBatch, SyncState,
    VideoFrameStage1Fields, VideoFrameStage2Fields, VideoMetadataFields,
};
pub use snapshot::Snapshot;
