//! 最近一次分析结果 TSV 的发布和验真行为测试。

use std::fs;

use dedup_core::{AnalysisRunId, ContentKey, LocationKey, MachineId, NormalizedPath, Thresholds};
use dedup_node_engine::analysis::{
    AnalysisResultError, AnalysisResultGroupKind, AnalysisResultHeader, AnalysisResultMode,
    AnalysisResultRow, AnalysisResultWriter, verify_result_file,
};
use tempfile::tempdir;

/// 防止发布结果缺失 H/M/F 固定记录，或尾部校验覆盖错误字节范围。
#[test]
fn published_result_has_header_member_rows_and_verified_footer() {
    let directory = tempdir().unwrap();
    let published = publish_fixture_result(directory.path());
    let bytes = fs::read(&published.path).unwrap();
    let text = std::str::from_utf8(&bytes).unwrap();
    let lines = text.lines().collect::<Vec<_>>();

    assert!(!bytes.starts_with(&[0xef, 0xbb, 0xbf]));
    assert_eq!(lines.len(), 5);
    assert_eq!(lines[0].split('\t').count(), 15);
    assert!(lines[0].starts_with("H\t1\t"));
    assert!(lines[1].starts_with("M\texact\tgroup-1\t1\t"));
    assert_eq!(lines[1].split('\t').count(), 14);
    assert_eq!(lines[4].split('\t').count(), 3);

    let parsed = verify_result_file(&published.path).unwrap();
    assert_eq!(parsed.member_count, 3);
    assert_eq!(parsed.sha256, sha256_before_footer(&bytes));
}

/// 防止取消或失败的新分析清掉上一份已经完整发布的结果。
#[test]
fn discarding_new_partial_keeps_previous_published_result() {
    let directory = tempdir().unwrap();
    let result = directory.path().join("latest-analysis.result.tsv");
    fs::write(&result, b"previous-good-result").unwrap();
    let writer = AnalysisResultWriter::begin(directory.path(), &fixture_header()).unwrap();

    writer.discard().unwrap();

    assert_eq!(fs::read(&result).unwrap(), b"previous-good-result");
    assert!(
        !directory
            .path()
            .join("latest-analysis.partial.tsv")
            .exists()
    );
}

/// 防止损坏、非有限分数和带 TSV 控制字符的显示路径作为可用结果被接收。
#[test]
fn rejects_corruption_non_finite_scores_and_tsv_control_characters() {
    let directory = tempdir().unwrap();
    let mut invalid_score = fixture_rows()[0].clone();
    invalid_score.stage1_score = f64::NAN;
    let mut score_writer =
        AnalysisResultWriter::begin(directory.path(), &fixture_header()).unwrap();
    assert!(matches!(
        score_writer.write_member(&invalid_score),
        Err(AnalysisResultError::InvalidRow(_))
    ));
    assert!(
        !directory
            .path()
            .join("latest-analysis.partial.tsv")
            .exists()
    );

    let mut invalid_path = fixture_rows()[0].clone();
    invalid_path.display_path.push('\t');
    let mut path_writer = AnalysisResultWriter::begin(directory.path(), &fixture_header()).unwrap();
    assert!(matches!(
        path_writer.write_member(&invalid_path),
        Err(AnalysisResultError::InvalidRow(_))
    ));
    assert!(
        !directory
            .path()
            .join("latest-analysis.partial.tsv")
            .exists()
    );
    let mut writer = AnalysisResultWriter::begin(directory.path(), &fixture_header()).unwrap();
    writer.write_member(&fixture_rows()[0]).unwrap();
    let published = writer.publish().unwrap();
    let mut bytes = fs::read(&published.path).unwrap();
    let footer_offset = bytes.iter().rposition(|byte| *byte == b'F').unwrap();
    bytes[footer_offset + 2] = b'9';
    fs::write(&published.path, bytes).unwrap();

    assert!(matches!(
        verify_result_file(&published.path),
        Err(AnalysisResultError::InvalidFormat(_))
    ));
}

/// 用固定输入发布三条成员记录，避免测试从被测编码逻辑推导期望值。
fn publish_fixture_result(
    root: &std::path::Path,
) -> dedup_node_engine::analysis::PublishedAnalysisResult {
    let mut writer = AnalysisResultWriter::begin(root, &fixture_header()).unwrap();
    for row in fixture_rows() {
        writer.write_member(&row).unwrap();
    }
    writer.publish().unwrap()
}

/// 构造有效、可手工检查的头记录。
fn fixture_header() -> AnalysisResultHeader {
    AnalysisResultHeader {
        format_version: 1,
        analysis_id: AnalysisRunId::new(),
        library_revision: 42,
        analysis_mode: AnalysisResultMode::Local,
        created_at_ms: 1_725_000_123,
        thresholds: Thresholds::default(),
    }
}

/// 构造三条不同可选字段组合的成员记录。
fn fixture_rows() -> Vec<AnalysisResultRow> {
    let representative = ContentKey::new([0x0a; 16], 512);
    let machine =
        MachineId::parse("73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae")
            .unwrap();
    [
        (true, ContentKey::new([0x0a; 16], 512), None, None),
        (
            false,
            ContentKey::new([0x0b; 16], 1024),
            Some(8),
            Some(0.987_654_321_f64),
        ),
        (
            false,
            ContentKey::new([0x0c; 16], 2048),
            Some(7),
            Some(0.123_456_789_f64),
        ),
    ]
    .into_iter()
    .enumerate()
    .map(
        |(index, (representative_flag, content, phash, stage2))| AnalysisResultRow {
            group_kind: AnalysisResultGroupKind::Exact,
            group_id: "group-1".into(),
            representative: representative_flag,
            representative_content: representative,
            location: LocationKey::new(
                machine.clone(),
                NormalizedPath::new(format!(r"C:\Media\Track-{index}.flac")).unwrap(),
            ),
            display_path: format!(r"C:\Media\Track-{index}.flac"),
            content,
            stage1_score: 1.0,
            phash_passed_parts: phash,
            stage2_score: stage2,
        },
    )
    .collect()
}

/// 用 SHA-256 独立重算 F 行之前全部字节，验证校验范围不包含尾记录。
fn sha256_before_footer(bytes: &[u8]) -> [u8; 32] {
    use sha2::{Digest, Sha256};

    let footer_start = bytes
        .windows(3)
        .rposition(|window| window == b"\nF\t")
        .map_or(0, |index| index + 1);
    Sha256::digest(&bytes[..footer_start]).into()
}
