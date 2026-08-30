# Task 1 实施报告：ContentKey 单项基础缓存查询

## RED

命令：`cargo test -p dedup-node-store --test content_cache lookup_base_cache_by_key_returns_one_complete_record --locked -- --test-threads=1`

实际结果：编译失败，`NodeStore` 没有 `lookup_base_cache_by_key` 方法（2 处 `E0599`）。

## GREEN

命令：`cargo test -p dedup-node-store --test content_cache lookup_base_cache_by_key_returns_one_complete_record --locked -- --test-threads=1`

实际结果：1 passed，0 failed。

全量命令：`cargo test -p dedup-node-store --locked -- --test-threads=1`

实际结果：全部测试通过；各测试目标均 0 failed。

格式与差异检查：`cargo fmt --all -- --check`、`git diff --check`

实际结果：均通过。执行前 C 盘可用 13.02 GiB，D 盘可用 15.96 GiB。

## 改动文件

- `crates/node-store/src/content.rs`：增加 `NodeStore::lookup_base_cache_by_key`，复用单项 `lookup_key_cache_batch` 装载完整记录。
- `crates/node-store/tests/content_cache.rs`：增加真实 SQLite 单项命中与未知键缺失测试。
- `crates/node-engine/src/scan/base_persistence.rs`：增加 `BaseStoreHandle::lookup_base_cache_by_key` actor 入口及测试观测。

## 提交

提交信息：`feat: add single content cache lookup`

提交 SHA：`362bcf03e05d53cfebe1c499f4582973a3c073a2`（报告写入时的提交 SHA；随后仅回填报告将产生最终提交 SHA）。

## 风险/遗留

本任务只提供底层单项查询入口，未改事件泵或批量路径缓存查询；调用方尚未切换到该入口属于后续任务范围。
