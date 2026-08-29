# Task 13A：Node 瞬态复核与逐项 TSV 删除队列

## 目标

把 Node 本地复核、删除计划、删除进度和删除结果从 SQLite 运行态表迁出。删除确认后先生成一个当前进程专用 TSV 队列，再由该文件顺序驱动逐项重验和删除；进程退出不恢复，启动时由既有 runtime 根清理一起删除。

## 硬边界

- 不增加 TaskCatalog、恢复、历史、JSON、`.idx`、分页或磁盘满清理。
- `review_marks/delete_batches/delete_items/deletion_tombstones/group_members` 不发生产品写入。
- 旧 schema 和旧 NodeStore API 可暂时留作兼容测试，但产品路径不得调用。
- 删除前逐项重新核对活动 `LocationKey`、实际大小和 1 MiB 缓冲流式 MD5。
- 同一 TSV 按冻结顺序逐行执行；单项失败、跳过不阻塞后续项。
- 每个成功项立即单独提交 `files.active=0`、file outbox 和 `library_revision + 1`；SQLite ACK 前队列状态必须保持 `P`，ACK 后才改为 `C`。失败或跳过改为 `F`。
- 队列只存在于解析后的 `data/node/runtime` 精确子目录，任务终态后删除；`NodeRuntime::start_inner` 在启动 Worker/actor 前精确删除并重建该 runtime 根，重启不查询、不恢复旧队列或其他旧任务文件。`data/node/results/latest-analysis.result.tsv` 不在清理范围。
- 本阶段不修改 Desktop/PostgreSQL 中心复核和删除；该边界留给 Task 13B，避免共享 `desktop-core/src/app.rs` 冲突。

## 设计

### NodeStore 当前事实提交

新增 `VerifiedDeletedFile` 和 `NodeStore::deactivate_deleted_files`。输入必须仍对应当前活动位置和相同 ContentKey；事务只失活文件、追加 `file` outbox、推进 revision 并返回 outbox 高水位。不得写墓碑、删除批次/项目、复核或重复组表。

Task 13A 的队列执行器每次只向该批量 API 传一个成功项，从而保证逐项提交和逐项 revision。

### Node 当前进程复核

新增简单 `ReviewRegistry`，键为 `(AnalysisRunId, group_id, LocationKey)`。只接受当前 `latest_analysis`、同一 result revision 且位置确实属于该组的决定；新分析成功、Node 启动或进程退出自然清空。读取本地结果成员窗口时只从该 registry 合并决定，不访问 SQLite `review_marks`。

本地删除计划必须验证：当前 latest run、结果 revision 等于 SQLite revision、每个确认项属于结果且 registry 为 Delete、每个涉及组至少有一个当前活动 Keep。中心下发的外部冻结项不依赖 Node 本地复核，但仍走同一 TSV 和逐文件安全重验。

### TSV 队列

固定 UTF-8、LF、无 BOM、无 JSON。每行至少保存单字节状态、item ID、group ID、MachineId、NormalizedPath、MD5 十六进制、size 和 mode。写入后 flush/sync，再由 reader 解析回 `PlannedDeleteItem`；执行器不得直接遍历原请求 Vec 作为第二份调度源。

状态规则：`P` 待处理；文件失败/身份变化后 `F`；文件系统成功但 SQLite 未 ACK 仍为 `P`；ACK 后 `C`。状态原位更新并 sync。终态精确删除本批目录；清理失败使任务失败，不保留为可恢复任务。

## TDD 门禁

1. 旧 `apply_external_delete_results` 路径先以真实 SQLite 夹具证明会写 `deletion_tombstones` 且不推进 revision；新 API 后断言只写当前 file fact/outbox/revision。
2. 队列真实文件测试证明写入顺序、实际从 TSV 解析、P/F/C 状态、ACK 前 P、完成精确清理和非法字段拒绝。
3. 受控文件系统按“成功、大小/MD5 变化、删除失败、成功”执行，证明后续项继续；成功项逐项提交，失败只在 RuntimeTask/返回值/日志。
4. Node actor 本地复核测试证明 SQLite `review_marks` 为 0；stale revision、缺 Keep、非当前 run 均在创建队列前失败。
5. NodeRuntime 启动前留下旧 runtime 子目录，启动后该目录被精确清空重建；results 下最近成功文件保持不变。
6. 外部中心批次行为保持协议响应，但 Node SQLite 的 `delete_batches/delete_items/deletion_tombstones/review_marks/group_members` 均无新增写入。

## 验证

- `cargo test -p dedup-node-store --test delete_group_update --locked -- --test-threads=1`
- `cargo test -p dedup-node-engine --features test-hooks --test delete --locked -- --test-threads=1`
- `cargo test -p dedup-node-engine --features test-hooks --test delete_runtime_details --locked -- --test-threads=1`
- `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`
- `cargo fmt --all -- --check`
- `git diff --check`

统一复用 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`，运行前清除继承的 C/C++/Rust wrapper 环境变量。未触及停止线时不创建新 target。
