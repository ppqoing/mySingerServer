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
- `AnalysisResultError` 现有公开错误枚举没有单独的 `InvalidResult` 变体，损坏窗口在 actor 层按既有协议错误边界返回 Internal 并附带校验信息；未扩展错误枚举以避免改变既有 V5 错误编号。
