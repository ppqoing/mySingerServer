# Task 11：本地分析结果滑动窗口报告

## RED

先新增真实行为测试，再运行定向命令确认接口尚不存在：

```text
cargo test -p dedup-node-engine --features test-hooks --test analysis_result_window --locked -- --test-threads=1
```

预期失败为 `LatestAnalysisReader` 与 `LocalResultWindowKind` 未定义（`E0432`）。

```text
cargo test -p dedup-protocol --test local_result_window_wire --locked -- --test-threads=1
```

预期失败为本地窗口消息、Envelope payload、`GroupMember.display_path` 和读取类别未定义。

随后把 actor 行为测试接入后，窗口请求仍预期失败；实际在接线前得到“最近结果窗口必须返回窗口响应”的断言失败，证明测试走的是 actor 行为而不是源码字符串检查。

## GREEN

- `LatestAnalysisReader::open_verified` 使用 `BufReader<File>` 顺序校验 UTF-8/LF、H/M/F、成员数和 H/M SHA-256，仅保存元数据、组摘要和 `u64` 偏移；成员窗口按偏移 seek，不创建 `.idx`，组类别由请求显式过滤。
- Node actor 启动时校验并安装最近 result；成功发布后先打开 reader，再安装结果并发送 Runtime Completed；分析失败或取消不替换旧 reader。成员窗口对唯一 `ContentKey` 执行一次批量基础缓存查询，并直接返回结果文件中的 `display_path`。
- V5 Envelope 新增 tag 46；请求/响应共用 `ReadLocalResultWindow`，携带 `group_kind`；`GroupMember.display_path` 追加为 field 12。

通过的验证：

```text
cargo test -p dedup-node-engine --features test-hooks --test analysis_result_window --locked -- --test-threads=1  # 3 passed
cargo test -p dedup-protocol --locked -- --test-threads=1                                      # 24 passed
cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1       # 154 passed
cargo fmt --all -- --check
git diff --check
```

另有 actor 专项窗口测试 1 passed，覆盖启动加载、组/成员窗口、stale 和错误 run ID；协议专项 wire 测试 2 passed，覆盖组/成员往返、tag 46 和 field 12。

## 文件清单

- `crates/node-engine/src/analysis/result_reader.rs`
- `crates/node-engine/src/analysis/mod.rs`
- `crates/node-engine/src/actor.rs`
- `proto/node.proto`
- `crates/protocol/src/lib.rs`
- `crates/protocol/tests/local_result_window_wire.rs`
- `crates/node-engine/tests/analysis_result_window.rs`
- 本报告文件

## 自审与未触碰范围

- 结果文件仍为无 BOM、LF 的 UTF-8 TSV；未增加 JSON、JSONL、持久化 `.idx`、TaskCatalog、恢复、历史或临时 SQLite。
- reader 不保存完整 `AnalysisResultRow` 集合；窗口只返回请求范围。结果损坏不会回退旧 SQLite 分组表，revision 不同仍可读并标记 stale。
- 未修改 `desktop-core`、`desktop-ui`，未接入 Desktop 本地分析；未触碰真实媒体根或 `I:\Tool`。
- 保留旧 Node SQLite 组/复核 API，避免把 Task12/后续清理混入本任务；新增窗口协议是独立只读入口。

## Concerns

- 现有工程仍有若干与本任务无关的 unused/dead-code 编译警告；本次验证无失败。
- `AnalysisResultError` 仍复用格式/IO 变体，协议层新增末尾 `InvalidResult=7`；损坏窗口由 actor 明确映射为 `InvalidResult`，文件 IO 仍映射为 `Internal`。

## Fix round 1/5：审查 finding 修复

### Finding 1 RED

新增 `actor_reports_invalid_result_for_corrupt_startup_file`：启动时在最近结果固定路径写入损坏字节，再通过真实 Node actor 请求窗口，要求协议错误码为新增 `InvalidResult=7`，且不能是 `Internal` 或 `NotFound`。修复前实际失败为 `left: 3 (NotFound), right: 7`，证明启动校验失败被丢成空状态。

命令：

```text
cargo test -p dedup-node-engine --features test-hooks --lib actor::tests::actor_reports_invalid_result_for_corrupt_startup_file --locked -- --test-threads=1
```

### Finding 2 RED

新增 `reader_keeps_previous_file_identity_after_result_replacement`：打开上一份结果 reader 后发布同路径的新结果，窗口必须仍返回旧组类型。修复前因固定路径重开读到了新文件；切换为稳定句柄后首次运行暴露 Windows `MoveFileExW` 在旧句柄存在时返回 `Io(code=5, PermissionDenied)`。

新增 `failed_pre_publish_verification_keeps_previous_result`：验证回调显式失败时，旧 result 字节必须不变且 partial 必须清理。接口尚未实现时 focused 编译先实际失败为 `E0599: no method named publish_with_verifier`。

### Fix

- V5 `ErrorCode` 增加末尾 `INVALID_RESULT=7`。Node actor 启动保留“结果存在但验真失败”的 `LatestAnalysis::Invalid` 状态；所有窗口统一返回 `InvalidResult`，不回退 SQLite 旧分组，也把窗口内格式/摘要错误映射为该码，文件 IO 仍保持 `Internal`。
- `LatestAnalysisReader` 改为保存共享删除模式打开的 `BufReader<File>`，成员窗口在同一句柄上 seek，不再按固定路径重新打开；结果替换后旧 reader 仍绑定旧文件身份。
- writer 增加 `publish_with_verifier`：写完并同步 partial 后先调用完整 reader 验证，验证失败清理 partial 并保留旧 result。提交边界使用 prepared 索引，释放 partial 的验证句柄后再原子替换；替换后只把新固定路径文件句柄绑定到已验真的索引并交给 actor。Windows 目标替换改用 `ReplaceFileW`，首次发布仍使用 `MoveFileExW`，因此旧 reader 与新发布可以并存。

### GREEN

```text
cargo test -p dedup-node-engine --features test-hooks --test analysis_result_window --locked -- --test-threads=1
# 6 passed

cargo test -p dedup-node-engine --features test-hooks --lib actor::tests::actor_reports_invalid_result_for_corrupt_startup_file --locked -- --test-threads=1
# 1 passed

cargo test -p dedup-node-engine --features test-hooks --lib actor::tests::local_analysis_installs_latest_result_before_runtime_completed --locked -- --test-threads=1
# 1 passed

cargo test -p dedup-protocol --locked -- --test-threads=1
# 24 passed

cargo test -p dedup-windows --test atomic_file --locked -- --test-threads=1
# 4 passed

cargo test -p dedup-node-engine --features test-hooks --test analysis_result_file --locked -- --test-threads=1
# 6 passed

cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1
# 155 passed

cargo fmt --all -- --check
git diff --check
```

### 修复轮自审

- 两个 finding 均有真实行为 RED 与对应 GREEN；没有使用源码字符串匹配测试。
- 固定结果仍为 UTF-8/LF TSV，无 `.idx`、历史/备份服务或额外 SQLite；旧 reader 和旧磁盘结果在验证失败时保持不变。
- 本轮唯一超出原 Task11 文件清单的改动是 `crates/windows/src/atomic_file.rs`：Windows `MoveFileExW` 无法在稳定旧 reader 存在时替换目标，实测 `ReplaceFileW` 才满足相同原子替换边界；现有原子文件回归 4/4 通过。
