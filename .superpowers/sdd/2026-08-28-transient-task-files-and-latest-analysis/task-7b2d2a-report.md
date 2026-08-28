# Task 7B2D2A：基础任务文件上下文所有权

## 结果

Task 7B2D2A 已完成。本次只为 `BaseTaskProducer` 增加内存中的
`TaskFileIdentity → TaskFileBaseContext` 映射，保存批量缓存查询得到的 `content_id`、缓存
快照、联系表有效性和强制重算标记。上下文不写入 TSV、SQLite 或其他持久文件；`seal` 成功
后与 dispatcher、清单一起移交给后续 BaseCompute 接入层。

## TDD 证据

先加入三个真实行为测试并执行 RED。旧的半成品接口无法编译：上下文缺少 `content_id`，
生产者没有 pending context 所有权，且 `seal` 没有移交上下文。这些失败直接对应本任务的
上下文契约缺口。

实现后定向测试全部通过：

- `base_contexts_match_partial_and_missing_rows_by_identity`：部分缓存和未命中行的上下文
  与 dispatcher 返回的完整身份逐一相等。
- `full_cache_hits_have_no_task_file_context`：完整缓存命中不产生任务行或上下文。
- `contexts_keep_their_original_lane_identity`：跨 HDD/SSD lane 的身份和上下文没有串线。

## 实现边界

- `TaskFileBaseContext` 是公开只读语义的数据值，字段包含 `content_id`、`cached`、
  `contact_sheet_valid` 和 `force_recompute`。
- `BaseTaskProduction` 使用按身份排序的 `BTreeMap` 保存上下文，稳定支持后续按完整
  `TaskFileIdentity` 查找。
- 每个 lane 的 `TaskFileDispatcher::append_batch` 返回身份后，按返回顺序与该 lane 的行和
  上下文配对；完整命中没有身份，也不生成上下文。
- 所有 lane 追加成功后才提交 staged 上下文、seen、resolved 和统计；追加或输入校验失败
  不提交本批上下文，原 dispatcher owner 仍可 `discard` 精确清理运行目录。
- `scan/mod.rs` 仅增加类型导出；没有接入 Worker、actor、SQLite schema、任务恢复或
  `progress.md`。

## 修改文件

- `crates/node-engine/src/scan/base_task_producer.rs`
- `crates/node-engine/src/scan/mod.rs`
- `crates/node-engine/tests/base_task_producer.rs`

## 验证

所有 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，关闭增量/debug 信息并清除
CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER。验证期间 C/D 可用空间约
14.67/11.95 GiB，未触发 10 GiB 停止线。

| 验证 | 结果 |
|---|---:|
| `cargo test -p dedup-node-engine --test base_task_producer --locked -- --test-threads=1` | 14/14 通过 |
| `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1` | 19/19 通过 |
| `cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1` | 25/25 通过 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 69/69 通过 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

现有 `BasePersistIdentity::TaskFile` 尚未接入主循环，因此构建仍有既有 dead-code 警告；
本任务未改变该边界，也未运行真实媒体、打包、部署或访问 `I:\Tool`。
