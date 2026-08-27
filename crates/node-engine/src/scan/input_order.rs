//! 扫描行在进入基础计算前按规范根目录确定性轮转。

use std::{collections::VecDeque, path::Path};

use dedup_core::{DisplayPath, NormalizedPath};
use dedup_node_store::ScannedPath;

use super::ScanError;

/// 一个规范根目录及其等待轮转的扫描行。
struct RootBucket {
    /// 用于目录归属和稳定排序的规范根路径。
    normalized_root: NormalizedPath,
    /// 规范根的路径组件数量，用于重叠根选择最深归属。
    component_count: usize,
    /// 已归属此根且保持原输入顺序的扫描行。
    rows: VecDeque<ScannedPath>,
}

/// 按规范根目录交错扫描行，避免多根任务长期只供给第一个根。
pub(crate) fn interleave_rows_by_root(
    roots: &[DisplayPath],
    rows: Vec<ScannedPath>,
) -> Result<Vec<ScannedPath>, ScanError> {
    // 将显示根转换为稳定的比较键，再固定 bucket 顺序。
    let mut normalized_roots = roots
        .iter()
        .map(|root| {
            NormalizedPath::new(root.as_path())
                .map_err(|error| ScanError::InvalidResult(format!("扫描根无效: {error}")))
        })
        .collect::<Result<Vec<_>, _>>()?;
    normalized_roots.sort();
    normalized_roots.dedup();

    // 每个唯一根拥有一条保持输入相对次序的待输出队列。
    let mut buckets = normalized_roots
        .into_iter()
        .map(|normalized_root| RootBucket {
            component_count: Path::new(normalized_root.as_str()).components().count(),
            normalized_root,
            rows: VecDeque::new(),
        })
        .collect::<Vec<_>>();
    // 输出容量与输入一致，轮转期间不复制扫描行。
    let mut output = Vec::with_capacity(rows.len());

    for row in rows {
        // 记录当前最深且同深时规范路径最小的归属 bucket。
        let mut selected: Option<usize> = None;
        for (index, bucket) in buckets.iter().enumerate() {
            if !row.normalized_path.is_within(&bucket.normalized_root) {
                continue;
            }
            if selected.is_none_or(|current| {
                bucket.component_count > buckets[current].component_count
                    || (bucket.component_count == buckets[current].component_count
                        && bucket.normalized_root < buckets[current].normalized_root)
            }) {
                selected = Some(index);
            }
        }

        // 无归属表示枚举器违背了本次扫描根边界。
        let index = selected.ok_or_else(|| {
            ScanError::InvalidResult(format!("枚举行 {} 不属于任何扫描根", row.normalized_path))
        })?;
        buckets[index].rows.push_back(row);
    }

    loop {
        // 标记本轮是否仍从任一根取得行，用于识别全部队列耗尽。
        let mut advanced = false;
        for bucket in &mut buckets {
            if let Some(row) = bucket.rows.pop_front() {
                output.push(row);
                advanced = true;
            }
        }
        if !advanced {
            return Ok(output);
        }
    }
}

#[cfg(feature = "test-hooks")]
#[doc(hidden)]
/// 为集成测试公开多根扫描行轮转行为。
pub fn interleave_rows_by_root_for_test(
    roots: &[DisplayPath],
    rows: Vec<ScannedPath>,
) -> Result<Vec<ScannedPath>, ScanError> {
    interleave_rows_by_root(roots, rows)
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use super::*;

    /// 构造不访问文件系统的扫描行，专门验证纯排序行为。
    fn row(path: &str, size: u64) -> ScannedPath {
        ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            size,
        )
    }

    /// 把输出行转换为规范路径，便于冻结输入输出次序。
    fn paths(rows: &[ScannedPath]) -> Vec<String> {
        rows.iter()
            .map(|row| row.normalized_path.to_string())
            .collect()
    }

    /// 断言轮转只改变顺序，不丢失行或字节数。
    fn assert_conserved(input: &[ScannedPath], output: &[ScannedPath]) {
        assert_eq!(output.len(), input.len());
        assert_eq!(
            output.iter().map(|row| row.file_size).sum::<u64>(),
            input.iter().map(|row| row.file_size).sum::<u64>()
        );
        assert_eq!(
            output
                .iter()
                .map(|row| row.normalized_path.to_string())
                .collect::<BTreeSet<_>>(),
            input
                .iter()
                .map(|row| row.normalized_path.to_string())
                .collect::<BTreeSet<_>>()
        );
    }

    #[test]
    fn h_i_roots_round_robin_with_fixed_output_and_conservation() {
        let roots = [
            DisplayPath::new(r"H:\Media").unwrap(),
            DisplayPath::new(r"I:\Media").unwrap(),
        ];
        let input = vec![
            row(r"H:\Media\a", 11),
            row(r"H:\Media\b", 12),
            row(r"H:\Media\c", 13),
            row(r"I:\Media\a", 21),
            row(r"I:\Media\b", 22),
        ];

        let output = interleave_rows_by_root(&roots, input.clone()).unwrap();

        assert_eq!(
            paths(&output),
            [
                r"H:\MEDIA\A",
                r"I:\MEDIA\A",
                r"H:\MEDIA\B",
                r"I:\MEDIA\B",
                r"H:\MEDIA\C",
            ]
        );
        assert_conserved(&input, &output);
    }

    #[test]
    fn single_root_keeps_input_order() {
        let roots = [DisplayPath::new(r"C:\Media").unwrap()];
        let input = vec![
            row(r"C:\Media\z", 1),
            row(r"C:\Media\a", 2),
            row(r"C:\Media\Album\b", 3),
        ];

        let output = interleave_rows_by_root(&roots, input.clone()).unwrap();

        assert_eq!(paths(&output), paths(&input));
        assert_conserved(&input, &output);
    }

    #[test]
    fn three_roots_skip_empty_bucket_and_keep_turning() {
        let roots = [
            DisplayPath::new(r"F:\Media").unwrap(),
            DisplayPath::new(r"E:\Media").unwrap(),
            DisplayPath::new(r"D:\Media").unwrap(),
        ];
        let input = vec![
            row(r"D:\Media\a", 1),
            row(r"D:\Media\b", 2),
            row(r"F:\Media\a", 3),
            row(r"F:\Media\b", 4),
            row(r"F:\Media\c", 5),
        ];

        let output = interleave_rows_by_root(&roots, input.clone()).unwrap();

        assert_eq!(
            paths(&output),
            [
                r"D:\MEDIA\A",
                r"F:\MEDIA\A",
                r"D:\MEDIA\B",
                r"F:\MEDIA\B",
                r"F:\MEDIA\C",
            ]
        );
        assert_conserved(&input, &output);
    }

    #[test]
    fn duplicate_roots_are_normalized_and_deduplicated() {
        let roots = [
            DisplayPath::new(r"C:\Media\").unwrap(),
            DisplayPath::new(r"c:\media").unwrap(),
        ];
        let input = vec![row(r"C:\Media\a", 4), row(r"C:\Media\b", 5)];

        let output = interleave_rows_by_root(&roots, input.clone()).unwrap();

        assert_eq!(paths(&output), paths(&input));
        assert_conserved(&input, &output);
    }

    #[test]
    fn overlapping_roots_assign_rows_to_the_deepest_component_root() {
        let roots = [
            DisplayPath::new(r"C:\Media").unwrap(),
            DisplayPath::new(r"C:\Media\Album").unwrap(),
        ];
        let input = vec![
            row(r"C:\Media\Album\first", 1),
            row(r"C:\Media\root", 2),
            row(r"C:\Media\Album\second", 3),
        ];

        let output = interleave_rows_by_root(&roots, input.clone()).unwrap();

        assert_eq!(
            paths(&output),
            [
                r"C:\MEDIA\ROOT",
                r"C:\MEDIA\ALBUM\FIRST",
                r"C:\MEDIA\ALBUM\SECOND",
            ]
        );
        assert_conserved(&input, &output);
    }

    #[test]
    fn unc_root_is_interleaved_without_string_prefix_matching() {
        let roots = [
            DisplayPath::new(r"\\server\share\media").unwrap(),
            DisplayPath::new(r"H:\Media").unwrap(),
        ];
        let input = vec![
            row(r"\\SERVER\SHARE\MEDIA\a", 6),
            row(r"H:\Media\a", 7),
            row(r"\\server\share\media\b", 8),
        ];

        let output = interleave_rows_by_root(&roots, input.clone()).unwrap();

        assert_eq!(
            paths(&output),
            [
                r"H:\MEDIA\A",
                r"\\SERVER\SHARE\MEDIA\A",
                r"\\SERVER\SHARE\MEDIA\B",
            ]
        );
        assert_conserved(&input, &output);
    }

    #[test]
    fn row_outside_all_roots_is_invalid_result() {
        let roots = [DisplayPath::new(r"H:\Media").unwrap()];

        let error = interleave_rows_by_root(&roots, vec![row(r"I:\Media\a", 9)]).unwrap_err();

        assert!(matches!(error, ScanError::InvalidResult(_)));
    }
}
