# Task7B2D4 实施报告

## 已完成

- 新增 `scan/task_file_cache.rs`，路径缓存先一次 SQLite 批量查询；只对本地缺失项执行一次远端批量查询。
- 远端结果只有在基础缓存完整度更高时才导入；整批导入完成后再做一次本地路径批量查询，校准本机 `content_id` 和合并字段。
- 路径输入保持枚举顺序，固定限制为最多 1,000 项；远端失败、返回长度不一致或文件大小不匹配时记录一次告警并降级 SQLite-only。
- Hash pass 增加可选远端 content 批量入口，保留原 SQLite-only wrapper；远端完整命中沿原 ACK→`C`/cache-hit 路径，部分命中继续同一任务身份进入 Media。
- Hash `ReadFailure` 的 persist 操作先写 `file_faults`，SQLite 操作成功 ACK 后才将任务文件行改为 `F`；写入失败返回任务级错误并保留 `P`。
- coordinator 增加可选远端入口，供上层把路径缓存可用性、告警和 Hash 内容缓存接入同一运行。

## 行为验证

使用固定 target `C:\tmp\rust-v2-core-scope-target-task7b2d2c`，清除外部编译器覆盖变量后运行：

- `cargo test -p dedup-node-engine --lib task_file_cache --locked -- --test-threads=1`：3/3 通过。
- `cargo test -p dedup-node-engine --lib task_file_base_compute::tests --locked -- --test-threads=1`：11/11 通过。
- 其中包含远端完整内容命中一次调用、远端失败回退 Media、路径混合顺序/导入、1,000 上限、Hash 批量查询和 `file_faults` 先写后 `F`。
- `cargo fmt --all`、`git diff --check` 通过。

## 范围说明

本次只增加瞬态任务文件的缓存/Hash/coordinator 接缝，不改 actor、任务收尾、恢复或 `TaskCatalog`；未打包、未部署、未触碰 `I:\Tool`。
