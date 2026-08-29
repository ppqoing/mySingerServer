//! 最近本地分析结果的无索引滑动窗口行为测试。

use std::fs;

use dedup_core::{AnalysisRunId, ContentKey, LocationKey, MachineId, NormalizedPath, Thresholds};
use dedup_node_engine::analysis::{
    AnalysisResultGroupKind, AnalysisResultHeader, AnalysisResultMode, AnalysisResultRow,
    AnalysisResultWriter, LatestAnalysisReader, LocalResultWindowKind,
};
use dedup_node_store::GroupKind;
use tempfile::tempdir;

#[test]
fn reads_large_member_windows_in_order_and_reopens_without_idx() {
    let directory = tempdir().unwrap();
    let published = publish_rows(
        directory.path(),
        10_000,
        "large-group",
        AnalysisResultGroupKind::Exact,
    );
    let mut reader = LatestAnalysisReader::open_verified(&published.path).unwrap();

    let first = reader
        .read_window(
            LocalResultWindowKind::Members {
                group_id: "large-group".into(),
            },
            0,
            100,
        )
        .unwrap();
    assert_eq!(first.total_rows, 10_000);
    assert_eq!(first.members.len(), 100);
    assert_eq!(first.members[0].display_path, r"D:\Media\00000.bin");
    assert_eq!(first.members[99].display_path, r"D:\Media\00099.bin");

    let middle = reader
        .read_window(
            LocalResultWindowKind::Members {
                group_id: "large-group".into(),
            },
            5_000,
            100,
        )
        .unwrap();
    assert_eq!(middle.members[0].display_path, r"D:\Media\05000.bin");
    assert_eq!(middle.members[99].display_path, r"D:\Media\05099.bin");

    let returned = reader
        .read_window(
            LocalResultWindowKind::Members {
                group_id: "large-group".into(),
            },
            50,
            100,
        )
        .unwrap();
    assert_eq!(returned.members[0].display_path, r"D:\Media\00050.bin");
    assert_eq!(returned.members[99].display_path, r"D:\Media\00149.bin");

    let groups = reader
        .read_window(LocalResultWindowKind::Groups(GroupKind::Exact), 0, 10)
        .unwrap();
    assert_eq!(groups.total_rows, 1);
    assert_eq!(groups.groups[0].member_count, 10_000);
    assert_eq!(reader.metadata().member_count, 10_000);
    assert!(
        !directory
            .path()
            .join("latest-analysis.result.tsv.idx")
            .exists()
    );

    let reopened = LatestAnalysisReader::open_verified(&published.path).unwrap();
    assert_eq!(reopened.metadata().run_id, reader.metadata().run_id);
}

#[test]
fn filters_groups_and_keeps_members_in_their_group() {
    let directory = tempdir().unwrap();
    let mut writer = AnalysisResultWriter::begin(directory.path(), &fixture_header()).unwrap();
    for (group_index, kind) in [
        AnalysisResultGroupKind::Exact,
        AnalysisResultGroupKind::Image,
        AnalysisResultGroupKind::Video,
    ]
    .into_iter()
    .enumerate()
    {
        for member_index in 0..2 {
            writer
                .write_member(&fixture_row(
                    kind,
                    &format!("group-{group_index:?}"),
                    member_index,
                    member_index == 0,
                ))
                .unwrap();
        }
    }
    let published = writer.publish().unwrap();
    let mut reader = LatestAnalysisReader::open_verified(&published.path).unwrap();

    let image_groups = reader
        .read_window(LocalResultWindowKind::Groups(GroupKind::Image), 0, 10)
        .unwrap();
    assert_eq!(image_groups.total_rows, 1);
    assert_eq!(image_groups.groups[0].member_count, 2);

    let members = reader
        .read_window(
            LocalResultWindowKind::Members {
                group_id: "group-2".into(),
            },
            0,
            10,
        )
        .unwrap();
    assert_eq!(members.total_rows, 2);
    assert!(members.members.iter().all(|row| row.group_id == "group-2"));
}

#[test]
fn rejects_invalid_result_records_and_partial_files() {
    let directory = tempdir().unwrap();
    let published = publish_rows(directory.path(), 2, "group", AnalysisResultGroupKind::Exact);
    let original = fs::read(&published.path).unwrap();

    for (name, bytes) in [
        ("bad-header", corrupt_at(&original, 0, b"X")),
        ("truncated", original[..original.len() - 1].to_vec()),
        (
            "bad-footer-count",
            replace_once(&original, b"F\t2\t", b"F\t3\t"),
        ),
        ("bad-footer-sha", replace_last_hex(&original)),
        ("invalid-utf8", invalid_utf8_member(&original)),
    ] {
        let path = directory.path().join(format!("{name}.tsv"));
        fs::write(&path, bytes).unwrap();
        assert!(
            LatestAnalysisReader::open_verified(&path).is_err(),
            "{name}"
        );
    }

    let partial = AnalysisResultWriter::begin(directory.path(), &fixture_header()).unwrap();
    let partial_path = directory.path().join("latest-analysis.partial.tsv");
    assert!(partial_path.exists());
    assert!(LatestAnalysisReader::open_verified(&partial_path).is_err());
    partial.discard().unwrap();
}

fn publish_rows(
    root: &std::path::Path,
    count: u32,
    group_id: &str,
    kind: AnalysisResultGroupKind,
) -> dedup_node_engine::analysis::PublishedAnalysisResult {
    let mut writer = AnalysisResultWriter::begin(root, &fixture_header()).unwrap();
    for index in 0..count {
        writer
            .write_member(&fixture_row(kind, group_id, index, index == 0))
            .unwrap();
    }
    writer.publish().unwrap()
}

fn fixture_header() -> AnalysisResultHeader {
    AnalysisResultHeader {
        format_version: 1,
        analysis_id: AnalysisRunId::new(),
        library_revision: 42,
        analysis_mode: AnalysisResultMode::Local,
        created_at_ms: 1,
        thresholds: Thresholds::default(),
    }
}

fn fixture_row(
    kind: AnalysisResultGroupKind,
    group_id: &str,
    index: u32,
    representative: bool,
) -> AnalysisResultRow {
    let machine =
        MachineId::parse("73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae")
            .unwrap();
    let content = ContentKey::new([index as u8; 16], u64::from(index) + 100);
    AnalysisResultRow {
        group_kind: kind,
        group_id: group_id.into(),
        representative,
        representative_content: ContentKey::new([0xaa; 16], 100),
        location: LocationKey::new(
            machine,
            NormalizedPath::new(format!(r"D:\Media\{index:05}.bin")).unwrap(),
        ),
        display_path: format!(r"D:\Media\{index:05}.bin"),
        content,
        stage1_score: 1.0,
        phash_passed_parts: None,
        stage2_score: None,
    }
}

fn corrupt_at(bytes: &[u8], index: usize, value: &[u8]) -> Vec<u8> {
    let mut output = bytes.to_vec();
    output.splice(index..index + value.len(), value.iter().copied());
    output
}

fn replace_once(bytes: &[u8], old: &[u8], new: &[u8]) -> Vec<u8> {
    let index = bytes
        .windows(old.len())
        .position(|window| window == old)
        .unwrap();
    let mut output = bytes.to_vec();
    output.splice(index..index + old.len(), new.iter().copied());
    output
}

fn replace_last_hex(bytes: &[u8]) -> Vec<u8> {
    let mut output = bytes.to_vec();
    let index = output.len() - 2;
    output[index] = if output[index] == b'0' { b'1' } else { b'0' };
    output
}

fn invalid_utf8_member(bytes: &[u8]) -> Vec<u8> {
    let mut output = bytes.to_vec();
    let index = output
        .windows(2)
        .position(|window| window == b"D:")
        .unwrap();
    output[index] = 0xff;
    output
}
