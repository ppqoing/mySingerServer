//! 中心 schema 的只读产品标记和固定列集合校验。

use std::collections::BTreeSet;

use super::{CENTRAL_SCHEMA_ID, CENTRAL_SCHEMA_SCRIPT, CentralError};

const REQUIRED_SCHEMA: &[(&str, &[&str])] = &[
    ("schema_metadata", &["key", "value"]),
    ("nodes", &["machine_id", "last_seen_at", "last_listen_addr"]),
    (
        "sync_cursors",
        &["machine_id", "committed_seq", "updated_at"],
    ),
    (
        "contents",
        &[
            "content_id",
            "md5",
            "file_size",
            "media_kind",
            "base_complete",
        ],
    ),
    (
        "file_locations",
        &[
            "machine_id",
            "normalized_path",
            "display_path",
            "file_size",
            "content_id",
            "active",
            "updated_seq",
        ],
    ),
    (
        "image_stage1",
        &["content_id", "width", "height", "pdq", "quality"],
    ),
    ("image_stage2", &["content_id", "phash_parts", "sobel"]),
    (
        "video_metadata",
        &["content_id", "duration_ms", "width", "height"],
    ),
    (
        "video_frame_stage1",
        &[
            "content_id",
            "slot",
            "time_ms",
            "decoded",
            "width",
            "height",
            "pdq",
            "quality",
        ],
    ),
    (
        "video_frame_stage2",
        &["content_id", "slot", "phash_parts", "sobel"],
    ),
    (
        "deletion_tombstones",
        &[
            "machine_id",
            "normalized_path",
            "md5",
            "file_size",
            "outcome",
            "updated_seq",
        ],
    ),
    (
        "analysis_runs",
        &[
            "analysis_run_id",
            "status",
            "thresholds_toml",
            "inputs_frozen",
            "error_text",
            "created_at",
            "updated_at",
        ],
    ),
    (
        "analysis_run_stages",
        &[
            "analysis_run_id",
            "stage_id",
            "state",
            "completed",
            "total",
            "failed",
            "skipped",
            "started_at_ms",
            "finished_at_ms",
            "warning_text",
        ],
    ),
    (
        "analysis_run_nodes",
        &[
            "analysis_run_id",
            "machine_id",
            "task_id",
            "task_highwater",
            "sync_highwater",
            "task_status",
        ],
    ),
    (
        "analysis_run_inputs",
        &[
            "analysis_run_id",
            "md5",
            "file_size",
            "machine_id",
            "normalized_path",
        ],
    ),
    (
        "analysis_stage2_dispatches",
        &[
            "analysis_run_id",
            "machine_id",
            "md5",
            "file_size",
            "node_task_id",
            "state",
            "updated_at_ms",
        ],
    ),
    (
        "candidate_pairs",
        &[
            "analysis_run_id",
            "pair_kind",
            "left_md5",
            "left_size",
            "right_md5",
            "right_size",
            "stage1_score",
            "phash_passed_parts",
            "stage2_score",
            "status",
        ],
    ),
    (
        "duplicate_groups",
        &[
            "analysis_run_id",
            "group_id",
            "group_kind",
            "representative_md5",
            "representative_size",
        ],
    ),
    (
        "group_members",
        &[
            "analysis_run_id",
            "group_id",
            "machine_id",
            "normalized_path",
            "md5",
            "file_size",
            "representative",
            "stage1_score",
            "phash_passed_parts",
            "stage2_score",
            "active",
        ],
    ),
    (
        "review_marks",
        &[
            "analysis_run_id",
            "group_id",
            "machine_id",
            "normalized_path",
            "decision",
        ],
    ),
    (
        "delete_batches",
        &[
            "delete_batch_id",
            "analysis_run_id",
            "mode",
            "status",
            "created_at",
        ],
    ),
    (
        "delete_items",
        &[
            "delete_item_id",
            "delete_batch_id",
            "group_id",
            "machine_id",
            "normalized_path",
            "expected_md5",
            "expected_size",
            "status",
            "message",
        ],
    ),
];

/// 使用当前页面连接串建立一次临时连接并校验固定 schema，不查询业务表行数。
pub async fn inspect_database(url: &str) -> Result<(), CentralError> {
    let (client, connection) = tokio_postgres::connect(url, tokio_postgres::NoTls).await?;
    let connection = tokio::spawn(async move {
        if let Err(error) = connection.await {
            tracing::error!(
                event = "background_task_failed",
                component = "central_database_diagnostics",
                task_name = "postgres_connection",
                operation = "drive_connection",
                error = %error,
                "数据库诊断连接驱动失败"
            );
        }
    });
    let result = validate_schema(&client).await;
    connection.abort();
    super::log_connection_join(connection.await, true);
    result
}
/// 验证 schema 产品标记及任务依赖的每一张固定表/列，不执行 DDL。
pub async fn validate_schema(client: &tokio_postgres::Client) -> Result<(), CentralError> {
    let exists: Option<String> = client
        .query_one("SELECT to_regclass('public.schema_metadata')::text", &[])
        .await?
        .get(0);
    if exists.is_none() {
        return Err(CentralError::SchemaMissing {
            script: CENTRAL_SCHEMA_SCRIPT,
        });
    }
    let schema_id: Option<String> = client
        .query_opt(
            "SELECT value FROM schema_metadata WHERE key='schema_id'",
            &[],
        )
        .await?
        .map(|row| row.get(0));
    validate_schema_id(schema_id.as_deref())?;
    for (table, required_columns) in REQUIRED_SCHEMA {
        let rows = client
            .query(
                "SELECT column_name FROM information_schema.columns
                 WHERE table_schema='public' AND table_name=$1",
                &[table],
            )
            .await?;
        let actual = rows
            .into_iter()
            .map(|row| row.get::<_, String>(0))
            .collect::<BTreeSet<_>>();
        validate_required_columns(table, required_columns, &actual)?;
    }
    Ok(())
}

/// 校验中心数据库只能使用当前产品 schema 标识。
fn validate_schema_id(actual: Option<&str>) -> Result<(), CentralError> {
    if actual == Some(CENTRAL_SCHEMA_ID) {
        Ok(())
    } else {
        Err(CentralError::SchemaMismatch(
            "schema_metadata.schema_id 不匹配".into(),
        ))
    }
}

/// 校验固定表的必需列，缺列时拒绝连接而不执行任何 DDL。
fn validate_required_columns(
    table: &str,
    required_columns: &[&str],
    actual: &BTreeSet<String>,
) -> Result<(), CentralError> {
    for column in required_columns {
        if !actual.contains(*column) {
            return Err(CentralError::SchemaMismatch(format!(
                "缺少 {table}.{column}"
            )));
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use super::{CENTRAL_SCHEMA_ID, CentralError, validate_required_columns, validate_schema_id};

    /// 产品标识不匹配时必须拒绝连接。
    #[test]
    fn schema_id_mismatch_is_rejected() {
        assert!(validate_schema_id(Some("legacy-schema")).is_err());
        assert!(validate_schema_id(None).is_err());
        assert!(validate_schema_id(Some(CENTRAL_SCHEMA_ID)).is_ok());
    }

    /// 固定表缺少业务必需列时必须报告精确表列名。
    #[test]
    fn missing_required_column_is_rejected() {
        let actual = BTreeSet::from(["content_id".to_owned()]);
        let error = validate_required_columns("image_stage2", &["content_id", "sobel"], &actual)
            .unwrap_err();

        assert!(matches!(
            error,
            CentralError::SchemaMismatch(message) if message == "缺少 image_stage2.sobel"
        ));
    }
}
