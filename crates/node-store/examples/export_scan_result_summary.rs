//! 结果摘要导出验收工具；只读 SQLite/cache，并输出固定 `result-summary.tsv`。

use std::{env, ffi::OsString, path::PathBuf, process::ExitCode};

use dedup_node_store::result_summary::{
    ResultSummaryError, export_scan_result_summary, validate_result_summary,
};

/// 命令行参数；`--media-root` 可重复，至少需要一项。
#[derive(Debug, PartialEq, Eq)]
struct CliArguments {
    database: PathBuf,
    cache_root: PathBuf,
    media_roots: Vec<PathBuf>,
    output: PathBuf,
}

/// CLI 参数错误或导出失败；错误码保持稳定供 PowerShell 读取。
#[derive(Debug)]
enum CliError {
    InvalidArgument,
    Export(ResultSummaryError),
}

impl CliError {
    /// 返回 stderr 使用的稳定错误码，不泄露动态路径诊断。
    fn code(&self) -> &'static str {
        match self {
            Self::InvalidArgument => "INVALID_ARGUMENT",
            Self::Export(error) => error_code(error),
        }
    }
}

/// 解析固定参数；每个 `--media-root value` 都追加到根列表，其他选项不可重复。
fn parse_arguments<I>(arguments: I) -> Result<CliArguments, CliError>
where
    I: IntoIterator<Item = OsString>,
{
    let values = arguments
        .into_iter()
        .map(|value| value.into_string().map_err(|_| CliError::InvalidArgument))
        .collect::<Result<Vec<_>, _>>()?;
    if values.len() % 2 != 0 {
        return Err(CliError::InvalidArgument);
    }
    let mut database = None;
    let mut cache_root = None;
    let mut media_roots = Vec::new();
    let mut output = None;
    for pair in values.chunks_exact(2) {
        let value = PathBuf::from(&pair[1]);
        match pair[0].as_str() {
            "--database" if database.is_none() => database = Some(value),
            "--cache-root" if cache_root.is_none() => cache_root = Some(value),
            "--media-root" => media_roots.push(value),
            "--output" if output.is_none() => output = Some(value),
            "--database" | "--cache-root" | "--output" => {
                return Err(CliError::InvalidArgument);
            }
            _ => return Err(CliError::InvalidArgument),
        }
    }
    Ok(CliArguments {
        database: database.ok_or(CliError::InvalidArgument)?,
        cache_root: cache_root.ok_or(CliError::InvalidArgument)?,
        media_roots: if media_roots.is_empty() {
            return Err(CliError::InvalidArgument);
        } else {
            media_roots
        },
        output: output.ok_or(CliError::InvalidArgument)?,
    })
}

/// 把库层错误映射为稳定 stderr 错误码。
fn error_code(error: &ResultSummaryError) -> &'static str {
    match error {
        ResultSummaryError::Sqlite(_) => "SQLITE_ERROR",
        ResultSummaryError::Io(_) => "IO_ERROR",
        ResultSummaryError::InvalidArgument(_) => "INVALID_ARGUMENT",
        ResultSummaryError::InvalidOutput(_) => "INVALID_OUTPUT",
        ResultSummaryError::UnsupportedFileIdentity => "UNSUPPORTED_FILE_IDENTITY",
        ResultSummaryError::UnsafeArtifactPath => "UNSAFE_ARTIFACT_PATH",
    }
}

/// 执行只读导出并输出固定字段，绝不输出 task ID 或 JSON metadata。
fn run() -> Result<(), CliError> {
    let arguments = parse_arguments(env::args_os().skip(1))?;
    let result = export_scan_result_summary(
        &arguments.database,
        &arguments.cache_root,
        &arguments.media_roots,
        &arguments.output,
    )
    .map_err(CliError::Export)?;
    validate_result_summary(&result.output_path).map_err(CliError::Export)?;
    println!("RESULT_SUMMARY_STATUS={}", result.status.as_str());
    println!("RESULT_SUMMARY_PATH={}", result.output_path.display());
    println!("RESULT_SUMMARY_SHA256={}", result.sha256);
    println!("RESULT_SUMMARY_ROW_COUNT={}", result.row_count);
    println!("RESULT_SUMMARY_MISSING_COUNT={}", result.missing_count);
    println!(
        "RESULT_SUMMARY_INCONCLUSIVE_COUNT={}",
        result.inconclusive_count
    );
    Ok(())
}

/// CLI 入口；失败只打印稳定码并返回非零状态。
fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("RESULT_SUMMARY_ERROR_CODE={}", error.code());
            ExitCode::from(2)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造两个媒体根，确认重复参数按出现顺序保留。
    #[test]
    fn repeated_media_root_arguments_are_preserved() {
        let arguments = [
            "--database",
            "node.db",
            "--cache-root",
            "cache",
            "--media-root",
            r"C:\Media",
            "--media-root",
            r"D:\Media",
            "--output",
            "result-summary.tsv",
        ]
        .into_iter()
        .map(OsString::from);
        let parsed = parse_arguments(arguments).expect("重复 media-root 应接受");
        assert_eq!(
            parsed.media_roots,
            [PathBuf::from(r"C:\Media"), PathBuf::from(r"D:\Media")]
        );
    }

    /// 缺少 media root、重复单值参数和未知参数都使用同一稳定错误码。
    #[test]
    fn invalid_cli_arguments_use_one_stable_code() {
        let cases = [
            vec![
                "--database",
                "node.db",
                "--cache-root",
                "cache",
                "--output",
                "result-summary.tsv",
            ],
            vec![
                "--database",
                "node.db",
                "--database",
                "again.db",
                "--cache-root",
                "cache",
                "--media-root",
                r"C:\Media",
                "--output",
                "result-summary.tsv",
            ],
            vec!["--unknown", "value"],
        ];
        for values in cases {
            let arguments = values.into_iter().map(OsString::from);
            assert_eq!(
                parse_arguments(arguments)
                    .expect_err("非法参数必须拒绝")
                    .code(),
                "INVALID_ARGUMENT"
            );
        }
    }
}
