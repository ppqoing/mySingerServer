//! 单调同步 outbox 的写入、拉取、ACK 和已确认裁剪边界。

use dedup_protocol::proto;
use rusqlite::{Connection, Transaction, params};

use crate::{NodeStore, StoreError, SyncBatch, SyncState};

impl NodeStore {
    /// 记录一个与业务写事务无关的同步变更，主要供任务/删除模块复用。
    pub fn record_sync_change(
        &mut self,
        entity_kind: &str,
        payload: Vec<u8>,
    ) -> Result<u64, StoreError> {
        let transaction = self.connection.transaction()?;
        let sequence = append_sync_change(&transaction, entity_kind, payload)?;
        transaction.commit()?;
        Ok(sequence)
    }

    /// 返回节点已经提交到 SQLite 的最高 outbox 序号。
    pub fn outbox_high_seq(&self) -> Result<u64, StoreError> {
        outbox_high_seq_from(&self.connection)
    }

    /// 从中心已提交序号之后拉取一批有序变更。
    pub fn pull_changes(&self, after_seq: u64, limit: usize) -> Result<SyncBatch, StoreError> {
        let state = self.sync_state()?;
        if after_seq < state.pruned_through_seq {
            return Err(StoreError::SnapshotRequired {
                requested_seq: after_seq,
                pruned_through_seq: state.pruned_through_seq,
            });
        }
        let mut statement = self.connection.prepare_cached(
            "SELECT seq,entity_kind,payload FROM sync_outbox
             WHERE seq>?1 ORDER BY seq LIMIT ?2",
        )?;
        let changes = statement
            .query_map(params![after_seq as i64, limit as i64], |row| {
                Ok(proto::SyncChange {
                    seq: row.get::<_, i64>(0)? as u64,
                    entity_kind: row.get(1)?,
                    payload: row.get(2)?,
                })
            })?
            .collect::<Result<Vec<_>, _>>()?;
        Ok(SyncBatch {
            changes,
            high_seq: self.outbox_high_seq()?,
            pruned_through_seq: state.pruned_through_seq,
        })
    }

    /// 幂等接受 PostgreSQL 已提交游标，并只裁剪本地实际产生过的序号范围。
    pub fn ack_changes(&mut self, committed_seq: u64) -> Result<(), StoreError> {
        let transaction = self.connection.transaction()?;
        let high_seq = outbox_high_seq_from(&transaction)?;
        let current = sync_state_from(&transaction)?;
        let acked_seq = current.acked_seq.max(committed_seq.min(high_seq));
        transaction.execute("DELETE FROM sync_outbox WHERE seq<=?1", [acked_seq as i64])?;
        let pruned_through_seq = current.pruned_through_seq.max(acked_seq);
        transaction.execute(
            "UPDATE sync_state SET acked_seq=?1,pruned_through_seq=?2 WHERE singleton=1",
            params![acked_seq as i64, pruned_through_seq as i64],
        )?;
        transaction.commit()?;
        Ok(())
    }

    /// 读取节点当前 ACK 和裁剪边界。
    pub fn sync_state(&self) -> Result<SyncState, StoreError> {
        sync_state_from(&self.connection)
    }
}

pub(crate) fn append_sync_change(
    transaction: &Transaction<'_>,
    entity_kind: &str,
    payload: Vec<u8>,
) -> Result<u64, StoreError> {
    transaction.execute(
        "INSERT INTO sync_outbox(entity_kind,payload) VALUES(?1,?2)",
        params![entity_kind, payload],
    )?;
    Ok(transaction.last_insert_rowid() as u64)
}

pub(crate) fn outbox_high_seq_from(connection: &Connection) -> Result<u64, StoreError> {
    let high_seq: i64 = connection.query_row(
        "SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='sync_outbox'),0)",
        [],
        |row| row.get(0),
    )?;
    Ok(high_seq as u64)
}

fn sync_state_from(connection: &Connection) -> Result<SyncState, StoreError> {
    let (acked_seq, pruned_through_seq): (i64, i64) = connection.query_row(
        "SELECT acked_seq,pruned_through_seq FROM sync_state WHERE singleton=1",
        [],
        |row| Ok((row.get(0)?, row.get(1)?)),
    )?;
    Ok(SyncState {
        acked_seq: acked_seq as u64,
        pruned_through_seq: pruned_through_seq as u64,
    })
}
