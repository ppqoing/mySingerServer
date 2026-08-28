//! 节点文件库版本号的校验和事务内递增边界。

use rusqlite::{Connection, OptionalExtension, Transaction};

use crate::StoreError;

/// 确保合法 V3 数据库拥有初始文件库版本，并立即拒绝格式错误的既有值。
pub(crate) fn ensure_library_revision(connection: &Connection) -> Result<(), StoreError> {
    connection.execute(
        "INSERT INTO metadata(key,value) VALUES('library_revision','0')
         ON CONFLICT(key) DO NOTHING",
        [],
    )?;
    let _ = read_library_revision(connection)?;
    Ok(())
}

/// 严格读取 metadata 中的文件库版本；缺失或非 `u64` 十进制值视为不兼容 schema。
pub(crate) fn read_library_revision(connection: &Connection) -> Result<u64, StoreError> {
    let value: Option<String> = connection
        .query_row(
            "SELECT value FROM metadata WHERE key='library_revision'",
            [],
            |row| row.get(0),
        )
        .optional()?;
    let value = value.ok_or(StoreError::IncompatibleSchema)?;
    if value.is_empty() || !value.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(StoreError::IncompatibleSchema);
    }
    value
        .parse::<u64>()
        .map_err(|_| StoreError::IncompatibleSchema)
}

/// 在调用方业务事务中递增文件库版本，并返回递增后的版本号。
///
/// 后续扫描成功收尾或至少一项成功删除必须复用各自的业务事务调用此函数；本模块不自行提交。
#[allow(dead_code)]
pub(crate) fn bump_library_revision(transaction: &Transaction<'_>) -> Result<u64, StoreError> {
    let revision = read_library_revision(transaction)?;
    let next = revision
        .checked_add(1)
        .ok_or(StoreError::IncompatibleSchema)?;
    transaction.execute(
        "UPDATE metadata SET value=?1 WHERE key='library_revision'",
        [next.to_string()],
    )?;
    Ok(next)
}

#[cfg(test)]
mod tests {
    use rusqlite::Connection;

    use super::{bump_library_revision, read_library_revision};

    /// 事务边界返回新版本，且只有调用方提交后才持久化。
    #[test]
    fn bump_library_revision_advances_only_inside_caller_transaction() {
        let mut connection = Connection::open_in_memory().unwrap();
        connection
            .execute_batch(
                "CREATE TABLE metadata(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
                 INSERT INTO metadata(key,value) VALUES('library_revision','0');",
            )
            .unwrap();

        let transaction = connection.transaction().unwrap();
        assert_eq!(bump_library_revision(&transaction).unwrap(), 1);
        assert_eq!(read_library_revision(&transaction).unwrap(), 1);
        transaction.commit().unwrap();

        assert_eq!(read_library_revision(&connection).unwrap(), 1);
    }
}
