# Task 13A：Node 瞬态复核与逐项 TSV 删除队列实施报告

## 结论

Node 本地删除路径已切换为当前进程内复核状态和 `data/node/runtime/<run-id>` 瞬态 TSV 队列：

- 复核决定只保存在 Node 进程内的 `ReviewRegistry`，不再由产品路径写入 SQLite 复核、删除历史或重复组表。
- 本地删除计划只接受当前 `latest-analysis.result.tsv`、当前 library revision、当前活动位置和完整 ContentKey；每个组必须保留至少一个活动 `Keep`。
- 删除队列写入后重新从 TSV 解析，严格按冻结顺序逐项执行。文件系统删除成功后先调用 `deactivate_deleted_files`，SQLite 成功后才把该行从 `P` 改为 `C`；失败或跳过改为 `F`，继续后续项目。
- SQLite 提交或队列 ACK 的基础设施错误会先尝试精确清理本批 runtime 目录；清理失败会保持任务失败，不伪装成可恢复完成。任务终态使用真实 outbox 高水位，读取高水位失败不会降级为无 highwater 的 Completed。
- `NodeRuntime::start_inner` 在创建 Worker/actor 前只清理并重建精确 `data/node/runtime`，不删除 `results/latest-analysis.result.tsv`。
- 中心下发的外部删除仍返回原协议响应，但同样经过瞬态队列和逐文件事实提交；本阶段没有修改 Desktop 或中心代码。

未增加恢复、历史、JSON、`.idx`、分页、TaskCatalog 或磁盘满清理逻辑。

## TDD 证据

本任务首先使用真实文件型 SQLite 和真实文件系统夹具固定旧行为边界，再实现最小 Node 路径：

1. 旧删除提交路径会把删除历史写入 `deletion_tombstones`，且不能按当前文件事实逐项推进 revision；新增 `VerifiedDeletedFile`/`deactivate_deleted_files` 后，当前事实只写 `files.active=0`、file outbox 和 library revision，旧删除/复核/组表保持空。
2. 旧 Node 本地复核依赖 SQLite 外键删除表；真实 actor 测试在旧实现失败：`SQLite 操作失败: FOREIGN KEY constraint failed`。改为内存 `ReviewRegistry` 后，同一结果窗口能显示 Keep/Delete，复核表行数为 0，并可创建瞬态删除队列。
3. 新增 `delete_transient` 真实行为测试，旧实现缺少瞬态执行入口而无法编译；实现后固定以下行为：成功、失败、成功三项按 TSV 顺序执行，中间失败不阻塞后续项，成功项逐一提交当前事实，失败项保留文件并写入失败结果，终态清理精确 runtime 子目录。
4. 新增 Node actor 外部删除行为测试，验证外部批次仍返回 `CreateDeleteBatch` 响应并删除文件，但不写 `delete_batches`、`delete_items`、`deletion_tombstones`、`review_marks` 或 `group_members`。
5. 新增 NodeRuntime runtime 根测试：启动前遗留的旧运行子目录被清理并重建，`results/latest-analysis.result.tsv` 内容保持不变。
6. 新增 1001 行真实结果组测试，旧实现因 `MAX_LOCAL_RESULT_WINDOW_ROWS=1000` 截断而无法复核第 1001 项 Keep；结果读取器新增按偏移逐行查找，复核和删除计划只解析命中的单行，窗口外 Keep 也能通过。

本轮同时删除 NodeEngine 内旧的一次性删除执行器、committer 和 `apply_*_delete_results` 调用，原运行详情测试已改用瞬态队列；NodeStore 中的兼容 API 仍保留，但 NodeEngine 产品源码和测试不再调用它们。

执行循环中特别处理了所有拥有队列后的错误边界：领取下一行、单项执行、SQLite 当前事实提交、TSV `C/F` ACK 和最终清理都不会绕过 cleanup；创建队列失败时尚未取得队列所有权，允许直接返回错误。

## 验证结果

统一使用目标目录：

```text
C:\tmp\rust-v2-core-scope-target-task7b2d2c1
```

命令前清除了 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，并设置
`CARGO_INCREMENTAL=0`、`CARGO_PROFILE_DEV_DEBUG=0`、`CARGO_PROFILE_TEST_DEBUG=0`。

| 命令 | 结果 |
|---|---:|
| `cargo test -p dedup-node-engine --features test-hooks --test delete --locked -- --test-threads=1` | 5 passed, 0 failed |
| `cargo test -p dedup-node-engine --features test-hooks --test delete_runtime_details --locked -- --test-threads=1` | 3 passed, 0 failed |
| `cargo test -p dedup-node-engine --features test-hooks --test delete_transient --locked -- --test-threads=1` | 4 passed, 0 failed |
| `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1` | 156 passed, 0 failed |
| `cargo fmt --all -- --check` | passed |
| `git diff --check` | passed |

NodeEngine 测试库 156 项中包含 actor runtime 根、瞬态分析、任务文件、Worker、删除和调度回归；构建只报告已有 unused/dead-code 警告，没有失败。

## 修改范围

- `crates/node-engine/src/actor.rs`
- `crates/node-engine/src/analysis/result_reader.rs`
- `crates/node-engine/src/delete.rs`
- `crates/node-engine/src/lib.rs`
- `crates/node-engine/src/review_registry.rs`
- `crates/node-engine/tests/delete.rs`
- `crates/node-engine/tests/delete_runtime_details.rs`
- `crates/node-engine/tests/delete_transient.rs`
- 本报告

当前只完成 NodeEngine 侧 Task13A，未修改 `desktop-core`、`desktop-ui`、`central-store`、协议 schema 或生产部署文件；未打包、未部署、未触碰 `I:\Tool`，未提交本任务改动。
