//! 中心 schema 的只读产品标记和固定列集合校验。

use std::collections::BTreeSet;

use dedup_core::product_id;

use super::{CENTRAL_SCHEMA_SCRIPT, CentralError};

const REQUIRED_SCHEMA: &[(&str, &[&str])] = &[
    ("schema_metadata", &["key", "value"]),
    ("nodes", &["machine_id", "last_seen_at", "last_listen_addr"]),
    (
        "sync_cursors",
        &["machine_id", "committed_seq", "updated_at"],
    ),
    (
        "contents",
        &["content_id", "md5", "file_size", "media_kind"],
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
    if schema_id.as_deref() != Some(product_id()) {
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
