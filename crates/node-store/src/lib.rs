//! 节点 SQLite schema、查询、事务、分析结果和同步 outbox。
#![warn(missing_docs)]

mod analysis;
mod content;
mod delete;
mod faults;
mod features;
mod groups;
mod open;
mod outbox;
mod review;
mod rows;
mod snapshot;
mod stages;
mod tasks;

#[cfg(feature = "acceptance-tools")]
pub mod result_summary;

pub use analysis::{
    AnalysisInput, AnalysisMode, AnalysisRunSnapshot, AnalysisStatus, CandidateStatus,
    CandidateWrite, PairKind,
};
pub use delete::{
    ConfirmedDeleteItem, DeleteBatchPlan, DeleteOutcome, DeleteResult, PlannedDeleteItem,
};
pub use faults::{FileFaultKind, FileFaultPage, FileFaultRecord};
pub use groups::{
    GroupKind, GroupMemberPage, GroupMemberWrite, GroupPage, GroupWrite, StoredGroup,
    StoredGroupMember,
};
pub use open::{NodeStore, StoreError};
pub use review::ReviewDecision;
pub use rows::{
    ActiveFile, BaseCacheRecord, CacheLookup, CompleteStage1, CompleteStage2, ContentId,
    ContentRecord, FeatureWrite, ImageStage1Fields, ScannedPath, SnapshotPage, SnapshotRow,
    SyncBatch, SyncState, VideoFrameStage1Fields, VideoFrameStage2Fields, VideoMetadataFields,
};
pub use snapshot::{OwnedSnapshot, Snapshot};
pub use stages::{PersistentStageState, TaskStageSnapshot, TaskStageWrite};
pub use tasks::{
    ClaimedTaskItem, NewTaskItem, TaskEvent, TaskItemApplyResult, TaskItemCompletion,
    TaskItemIdentity, TaskItemSnapshot, TaskItemStatus, TaskPage, TaskSnapshot, TaskStatus,
};
