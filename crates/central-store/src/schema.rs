//! 中心 schema 的只读产品标记和固定列集合校验。

use std::collections::BTreeSet;

use super::{CENTRAL_SCHEMA_ID, CENTRAL_SCHEMA_SCRIPT, CentralError};

/// 数据库页面展示的一张固定中心表状态。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum CentralTableStatus {
    /// 表存在且精确计数查询成功。
    Ready,
    /// 固定 schema 中声明的表尚未创建。
    Missing,
    /// 表存在，但状态或计数查询失败。
    QueryFailed(String),
}

/// 一张固定中心表的存在状态与精确行数。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralTableDiagnostic {
    /// 固定且可信的 public schema 表名。
    pub name: String,
    /// 表存在性和计数查询状态。
    pub status: CentralTableStatus,
    /// 成功时的精确 `COUNT(*)`，其他状态为空。
    pub row_count: Option<u64>,
}

/// 一次数据库连接测试返回的完整固定表诊断。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralDatabaseDiagnostics {
    /// 产品标记、固定列和全部表均通过时为真。
    pub schema_complete: bool,
    /// 按建库脚本顺序返回的全部固定表。
    pub tables: Vec<CentralTableDiagnostic>,
}

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

/// 抽象表存在性和精确计数查询，生产实现使用 PostgreSQL Client，测试可注入确定结果。
trait DatabaseCatalogQuery {
    /// 判断 public schema 中是否存在指定固定表。
    async fn table_exists(&self, table: &str) -> Result<bool, String>;
    /// 返回指定固定表的精确行数。
    async fn exact_row_count(&self, table: &str) -> Result<u64, String>;
}

impl DatabaseCatalogQuery for tokio_postgres::Client {
    async fn table_exists(&self, table: &str) -> Result<bool, String> {
        let relation = format!("public.{table}");
        self.query_one("SELECT to_regclass($1)::text", &[&relation])
            .await
            .map(|row| row.get::<_, Option<String>>(0).is_some())
            .map_err(|error| error.to_string())
    }

    async fn exact_row_count(&self, table: &str) -> Result<u64, String> {
        // table 只来自 REQUIRED_SCHEMA 常量，不接收用户输入。
        let statement = format!("SELECT COUNT(*)::bigint FROM \"{table}\"");
        let count = self
            .query_one(&statement, &[])
            .await
            .map_err(|error| error.to_string())?
            .get::<_, i64>(0);
        u64::try_from(count).map_err(|_| format!("{table} 返回了负数行数"))
    }
}

/// 查询全部固定表；一张表失败时保留其余表的可用结果。
async fn inspect_database_tables<Q: DatabaseCatalogQuery>(query: &Q) -> CentralDatabaseDiagnostics {
    let mut tables = Vec::with_capacity(REQUIRED_SCHEMA.len());
    for (table, _) in REQUIRED_SCHEMA {
        let diagnostic = match query.table_exists(table).await {
            Ok(false) => CentralTableDiagnostic {
                name: (*table).into(),
                status: CentralTableStatus::Missing,
                row_count: None,
            },
            Ok(true) => match query.exact_row_count(table).await {
                Ok(row_count) => CentralTableDiagnostic {
                    name: (*table).into(),
                    status: CentralTableStatus::Ready,
                    row_count: Some(row_count),
                },
                Err(error) => CentralTableDiagnostic {
                    name: (*table).into(),
                    status: CentralTableStatus::QueryFailed(error),
                    row_count: None,
                },
            },
            Err(error) => CentralTableDiagnostic {
                name: (*table).into(),
                status: CentralTableStatus::QueryFailed(error),
                row_count: None,
            },
        };
        tables.push(diagnostic);
    }
    CentralDatabaseDiagnostics {
        schema_complete: tables
            .iter()
            .all(|table| table.status == CentralTableStatus::Ready),
        tables,
    }
}

/// 使用当前页面连接串建立一次临时连接，并返回固定 schema 的表状态和精确行数。
pub async fn inspect_database(url: &str) -> Result<CentralDatabaseDiagnostics, CentralError> {
    let (client, connection) = tokio_postgres::connect(url, tokio_postgres::NoTls).await?;
    let connection = tokio::spawn(async move {
        let _ = connection.await;
    });
    let schema_complete = validate_schema(&client).await.is_ok();
    let mut diagnostics = inspect_database_tables(&client).await;
    diagnostics.schema_complete &= schema_complete;
    connection.abort();
    Ok(diagnostics)
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
    if schema_id.as_deref() != Some(CENTRAL_SCHEMA_ID) {
        return Err(CentralError::SchemaMismatch(
            "schema_metadata.schema_id 不匹配".into(),
        ));
    }
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
        for column in *required_columns {
            if !actual.contains(*column) {
                return Err(CentralError::SchemaMismatch(format!(
                    "缺少 {table}.{column}"
                )));
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{CentralTableStatus, DatabaseCatalogQuery, inspect_database_tables};

    /// 模拟固定数据库目录查询，分别提供正常、缺表和计数失败结果。
    struct FakeCatalog;

    impl DatabaseCatalogQuery for FakeCatalog {
        async fn table_exists(&self, table: &str) -> Result<bool, String> {
            Ok(table != "image_stage2")
        }

        async fn exact_row_count(&self, table: &str) -> Result<u64, String> {
            match table {
                "contents" => Ok(42),
                "delete_items" => Err("计数查询超时".into()),
                _ => Ok(1),
            }
        }
    }

    #[tokio::test]
    async fn database_diagnostics_reports_all_tables_missing_and_count_errors() {
        let diagnostics = inspect_database_tables(&FakeCatalog).await;

        assert_eq!(diagnostics.tables.len(), 22, "必须检查固定 schema 的全部表");
        let contents = diagnostics
            .tables
            .iter()
            .find(|table| table.name == "contents")
            .expect("应包含 contents");
        assert_eq!(contents.status, CentralTableStatus::Ready);
        assert_eq!(contents.row_count, Some(42));

        let missing = diagnostics
            .tables
            .iter()
            .find(|table| table.name == "image_stage2")
            .expect("应包含 image_stage2");
        assert_eq!(missing.status, CentralTableStatus::Missing);
        assert_eq!(missing.row_count, None);

        let failed = diagnostics
            .tables
            .iter()
            .find(|table| table.name == "delete_items")
            .expect("应包含 delete_items");
        assert_eq!(
            failed.status,
            CentralTableStatus::QueryFailed("计数查询超时".into())
        );
        assert_eq!(failed.row_count, None);
        assert!(!diagnostics.schema_complete);
    }
}
