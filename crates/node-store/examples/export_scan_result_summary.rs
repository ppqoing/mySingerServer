//! 结果摘要导出验收工具；固定四个参数，不进入正式发布包。

use std::{env, ffi::OsString, path::PathBuf, process::ExitCode};

use dedup_node_store::result_summary::{
    ResultSummaryError, export_scan_result_summary, validate_result_summary_pair,
};

/// 命令行固定参数，避免测试工具依赖额外解析器或隐式默认值。
#[derive(Debug)]
struct CliArguments {
    database: PathBuf,
    cache_root: PathBuf,
    task_id: String,
    output: PathBuf,
}

/// CLI 参数或库执行失败；参数错误永远映射同一个稳定 code。
#[derive(Debug)]
enum CliError {
    InvalidArgument,
    Export(ResultSummaryError),
}

impl CliError {
    /// 返回 stderr 使用的稳定错误码，不暴露动态中文诊断。
    fn code(&self) -> &'static str {
        match self {
            Self::InvalidArgument => "INVALID_ARGUMENT",
            Self::Export(error) => error_code(error),
        }
    }
}

/// 解析 `--database/--cache-root/--task-id/--output` 四组参数。
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
    let mut task_id = None;
    let mut output = None;
    for pair in values.chunks_exact(2) {
        let value = pair[1].clone();
        match pair[0].as_str() {
            "--database" if database.is_none() => database = Some(PathBuf::from(value)),
            "--cache-root" if cache_root.is_none() => cache_root = Some(PathBuf::from(value)),
            "--task-id" if task_id.is_none() => task_id = Some(value),
            "--output" if output.is_none() => output = Some(PathBuf::from(value)),
            "--database" | "--cache-root" | "--task-id" | "--output" => {
                return Err(CliError::InvalidArgument);
            }
            _ => return Err(CliError::InvalidArgument),
        }
    }
    Ok(CliArguments {
        database: database.ok_or(CliError::InvalidArgument)?,
        cache_root: cache_root.ok_or(CliError::InvalidArgument)?,
        task_id: task_id.ok_or(CliError::InvalidArgument)?,
        output: output.ok_or(CliError::InvalidArgument)?,
    })
}

/// 把库层错误映射为稳定 stderr 错误码。
fn error_code(error: &ResultSummaryError) -> &'static str {
    match error {
        ResultSummaryError::Sqlite(_) => "SQLITE_ERROR",
        ResultSummaryError::Io(_) => "IO_ERROR",
        ResultSummaryError::Json(_) => "JSON_ERROR",
        ResultSummaryError::InvalidArgument(_) => "INVALID_ARGUMENT",
        ResultSummaryError::OutputCommitIncomplete => "OUTPUT_COMMIT_INCOMPLETE",
        ResultSummaryError::UnsupportedFileIdentity => "UNSUPPORTED_FILE_IDENTITY",
        ResultSummaryError::UnsafeArtifactPath => "UNSAFE_ARTIFACT_PATH",
    }
}

/// 执行固定导出并在 stdout 输出稳定字段顺序。
fn run() -> Result<(), CliError> {
    let arguments = parse_arguments(env::args_os().skip(1))?;
    let result = export_scan_result_summary(
        &arguments.database,
        &arguments.cache_root,
        &arguments.task_id,
        &arguments.output,
    )
    .map_err(CliError::Export)?;
    // CLI 打印结果前再次验证三件套，避免消费者只拿到单独 canonical。
    validate_result_summary_pair(&result.output_path).map_err(CliError::Export)?;
    println!("RESULT_SUMMARY_STATUS={}", result.status.as_str());
    println!("RESULT_SUMMARY_PATH={}", result.output_path.display());
    println!("RESULT_SUMMARY_SHA256={}", result.sha256);
    println!("RESULT_SUMMARY_ROW_COUNT={}", result.row_count);
    println!("RESULT_SUMMARY_MISSING_COUNT={}", result.missing_count);
    println!(
        "RESULT_SUMMARY_INCONCLUSIVE_COUNT={}",
        result.inconclusive_count
    );
    println!("RESULT_SUMMARY_TASK_ID={}", result.task_id);
    Ok(())
}

/// CLI 入口；错误只输出稳定 code 并返回非零状态，不伪造摘要。
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

    /// 构造四个参数的有效前缀，单项测试只替换一个错误。
    fn valid_arguments() -> Vec<OsString> {
        [
            "--database",
            "node.db",
            "--cache-root",
            "cache",
            "--task-id",
            "task",
            "--output",
            "result.jsonl",
        ]
        .into_iter()
        .map(OsString::from)
        .collect()
    }

    /// 在当前平台构造无法转成 UTF-8 的命令行参数。
    fn invalid_utf8_argument() -> OsString {
        #[cfg(windows)]
        {
            use std::os::windows::ffi::OsStringExt;
            return OsString::from_wide(&[0xD800]);
        }
        #[cfg(not(windows))]
        {
            use std::os::unix::ffi::OsStringExt;
            return OsString::from_vec(vec![0xFF]);
        }
    }

    #[test]
    fn invalid_cli_arguments_all_use_one_stable_code() {
        let mut missing = valid_arguments();
        missing.truncate(6);
        let mut duplicate = valid_arguments();
        duplicate.extend([OsString::from("--database"), OsString::from("again.db")]);
        let mut unknown = valid_arguments();
        unknown[0] = OsString::from("--unknown");
        let mut non_utf8 = valid_arguments();
        non_utf8[1] = invalid_utf8_argument();

        for arguments in [missing, duplicate, unknown, non_utf8] {
            let error = parse_arguments(arguments).expect_err("非法参数必须拒绝");
            assert_eq!(error.code(), "INVALID_ARGUMENT");
        }
    }
}
