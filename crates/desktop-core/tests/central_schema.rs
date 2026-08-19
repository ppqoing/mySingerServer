use dedup_desktop_core::central::{CENTRAL_SCHEMA_SCRIPT, CentralError, CentralStore};

const SCHEMA: &str = include_str!("../../../deploy/central-v2.sql");

#[test]
fn manual_schema_has_all_fixed_tables_and_no_implicit_migration_syntax() {
    let tables = [
        "schema_metadata",
        "nodes",
        "sync_cursors",
        "contents",
        "file_locations",
        "image_stage1",
        "image_stage2",
        "video_metadata",
        "video_frame_stage1",
        "video_frame_stage2",
        "deletion_tombstones",
        "analysis_runs",
        "analysis_run_nodes",
        "analysis_run_inputs",
        "candidate_pairs",
        "duplicate_groups",
        "group_members",
        "review_marks",
        "delete_batches",
        "delete_items",
    ];
    for table in tables {
        assert!(
            SCHEMA.contains(&format!("CREATE TABLE {table}")),
            "missing table {table}"
        );
    }
    assert!(!SCHEMA.contains("IF NOT EXISTS"));
    assert!(SCHEMA.contains("UNIQUE(md5, file_size)"));
    assert!(
        SCHEMA
            .contains("PRIMARY KEY(analysis_run_id, md5, file_size, machine_id, normalized_path)")
    );
    assert!(SCHEMA.contains("CHECK(left_md5 < right_md5 OR"));
    assert!(!SCHEMA.contains("contact_sheets"));
    assert_eq!(CENTRAL_SCHEMA_SCRIPT, "schema/central-v2.sql");
}

#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_EMPTY_URL"]
async fn connecting_to_empty_database_reports_schema_missing_without_creating_tables() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_EMPTY_URL").unwrap();
    let error = CentralStore::connect(&url).await.unwrap_err();
    assert!(matches!(
        error,
        CentralError::SchemaMissing {
            script: "schema/central-v2.sql"
        }
    ));

    let (client, connection) = tokio_postgres::connect(&url, tokio_postgres::NoTls)
        .await
        .unwrap();
    tokio::spawn(async move { connection.await.unwrap() });
    let count: i64 = client
        .query_one(
            "SELECT COUNT(*) FROM information_schema.tables
             WHERE table_schema='public' AND table_name='schema_metadata'",
            &[],
        )
        .await
        .unwrap()
        .get(0);
    assert_eq!(count, 0);
}
