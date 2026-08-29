//! 最近一次分析结果 TSV 的发布和验真行为测试。

use std::{fs, os::windows::ffi::OsStrExt, path::Path};

use dedup_core::{AnalysisRunId, ContentKey, LocationKey, MachineId, NormalizedPath, Thresholds};
use dedup_node_engine::analysis::{
    AnalysisResultError, AnalysisResultGroupKind, AnalysisResultHeader, AnalysisResultMode,
    AnalysisResultRow, AnalysisResultWriter, verify_result_file,
};
use tempfile::tempdir;
use windows::{
    Win32::{
        Foundation::{CloseHandle, GENERIC_READ, HANDLE},
        Storage::FileSystem::{CreateFileW, FILE_ATTRIBUTE_NORMAL, FILE_SHARE_MODE, OPEN_EXISTING},
    },
    core::PCWSTR,
};

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

/// 防止调用方提前返回且漏调 discard 时把未完成 partial 留给下次分析。
#[test]
fn dropping_unfinished_writer_removes_partial_and_keeps_previous_result() {
    let directory = tempdir().unwrap();
    let result = directory.path().join("latest-analysis.result.tsv");
    let partial = directory.path().join("latest-analysis.partial.tsv");
    fs::write(&result, b"previous-good-result").unwrap();

    let writer = AnalysisResultWriter::begin(directory.path(), &fixture_header()).unwrap();
    assert!(partial.exists());
    drop(writer);

    assert!(!partial.exists());
    assert_eq!(fs::read(&result).unwrap(), b"previous-good-result");
}

/// 防止非连续成员重复同一组时把可供 UI 直取的分组计数算错。
#[test]
fn published_and_verified_metadata_expose_unique_group_count() {
    let directory = tempdir().unwrap();
    let header = fixture_header();
    let mut rows = fixture_rows();
    rows[0].group_id = "group-a".into();
    rows[1].group_id = "group-b".into();
    rows[2].group_id = "group-a".into();
    let mut writer = AnalysisResultWriter::begin(directory.path(), &header).unwrap();
    for row in &rows {
        writer.write_member(row).unwrap();
    }

    let published = writer.publish().unwrap();
    let verified = verify_result_file(&published.path).unwrap();

    assert_eq!(published.run_id, header.analysis_id);
    assert_eq!(published.library_revision, 42);
    assert_eq!(published.group_count, 2);
    assert_eq!(verified.run_id, header.analysis_id);
    assert_eq!(verified.library_revision, 42);
    assert_eq!(verified.group_count, 2);
}

/// 防止旧 result 被独占读取时发布失败后遗留 partial 或覆盖旧字节。
#[test]
fn locked_previous_result_makes_publish_remove_partial_and_keep_old_bytes() {
    let directory = tempdir().unwrap();
    let result = directory.path().join("latest-analysis.result.tsv");
    let partial = directory.path().join("latest-analysis.partial.tsv");
    fs::write(&result, b"previous-good-result").unwrap();
    let mut writer = AnalysisResultWriter::begin(directory.path(), &fixture_header()).unwrap();
    writer.write_member(&fixture_rows()[0]).unwrap();
    let handle = open_without_sharing(&result);

    assert!(matches!(writer.publish(), Err(AnalysisResultError::Io(_))));

    unsafe {
        CloseHandle(handle).unwrap();
    }
    assert!(!partial.exists());
    assert_eq!(fs::read(&result).unwrap(), b"previous-good-result");
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

/// 使用 share=0 模拟 UI 或杀毒软件独占打开上一次成功结果的场景。
fn open_without_sharing(path: &Path) -> HANDLE {
    let wide = path
        .as_os_str()
        .encode_wide()
        .chain(std::iter::once(0))
        .collect::<Vec<_>>();
    unsafe {
        CreateFileW(
            PCWSTR(wide.as_ptr()),
            GENERIC_READ.0,
            FILE_SHARE_MODE(0),
            None,
            OPEN_EXISTING,
            FILE_ATTRIBUTE_NORMAL,
            None,
        )
        .unwrap()
    }
}
