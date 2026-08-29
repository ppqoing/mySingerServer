# Task8B2 瞬态二筛执行器实施报告

## 范围

本轮只实现已经完成缓存查询后的瞬态二筛执行器。执行器按物理硬盘把真正缺失的图片二筛字段或视频二筛槽位写入 TSV，使用同一个 `TaskFileDispatcher` 和读取许可提供者派发 Worker，并通过现有单写 actor 写入 SQLite。

本轮明确不做任务恢复、`TaskCatalog`、分页、任务表兼容、桌面分析接入或真实媒体部署；不写 `tasks`、`task_items`、`task_stages`。

## 已落地行为

实现文件：

- `crates/node-engine/src/scan/task_file_stage2_compute.rs`
- `crates/node-engine/src/scan/mod.rs`

执行边界如下：

1. 基础缓存不完整的项目留给基础计算；只有真实缺少图片二筛或视频二筛槽位的项目才进入 `P` 行，完整二筛命中不生成任务行。
2. `DispatchedTask` 的读取 permit 由活动二筛项拥有，直到同一任务 ID、任务项 ID 和 Worker slot 的 `Stage2SourceReadComplete` 才释放；身份或 slot 不匹配会停止当前任务并保留未处理 `P` 行。
3. Worker 成功结果先送入 `BaseStoreActor` 的窄写入操作，只有收到精确匹配的 `BasePersistAck` 后才把 TSV 行从 `P` 改为 `C`。
4. Worker 崩溃、取消或结果校验失败只把当前项改为 `F`，随后继续领取下一项；崩溃同时写入现有文件故障记录。
5. WorkerPool、读取 dispatcher、单写 actor 或 SQLite ACK 的基础设施错误进入任务级错误路径，收回在途 owner，未处理行保持 `P`。

## 行为测试

同一源文件模块内新增三项行为测试及一项生产过滤测试：

- `production_contains_only_missing_stage2_items`：完整二筛缓存不入 TSV，缺失项只有一条 `P`。
- `permit_and_tsv_status_follow_source_and_persist_ack_boundaries`：SourceComplete 前 permit 保持；匹配后释放；SQLite 首条写入被 gate 时仍为 `P`，ACK 后为 `C`，并确认 SQLite 已保存二筛结果。
- `crashed_item_becomes_failed_and_next_item_continues`：第一项崩溃为 `F`，第二项继续执行并为 `C`。

测试使用受控 WorkerPool、RAII permit 计数和真实 SQLite 单写 actor，不通过源码字符串匹配推断行为。

后续补充两项输入边界回归：

- `video_stage2_selection_intersects_cached_missing_slots`：视频任务只保留计划器选择与 SQLite 当前缺失槽位的交集；交集为空时不写 TSV、不启动 Worker。
- `video_stage2_missing_contact_sheet_still_creates_selected_work`：联系表缺失或损坏不阻断 probe/一筛已经完整的视频；所选槽位仍进入任务文件，Worker 可回退原视频并按目标路径重建联系表。

## 当前验证结果

- `rustfmt --edition 2024 --check crates/node-engine/src/scan/task_file_stage2_compute.rs`：通过。
- `git diff --check`（本轮 Rust 文件及 `scan/mod.rs`）：通过。
- `cargo check -p dedup-node-engine --features test-hooks --tests --locked`：通过。
- 清空外部 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER` 后，执行
  `cargo test -p dedup-node-engine --features test-hooks --lib task_file_stage2_compute --locked -- --test-threads=1`：3/3 通过。
- `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1`：128/128 通过，覆盖本模块、WorkerPool、瞬态扫描和当前任务查询协议。
- 上述两项补充回归由主代理串行复跑，均为 1/1 通过；`cargo fmt --all -- --check` 与 `git diff --check` 通过。

首次定向测试曾继承 MinGW `CC/CXX`，导致 `libsqlite3-sys` 在 MSVC 链接阶段报告
`___chkstk_ms`、`__isnan` 未解析；清空这些外部环境变量后原命令正常链接并通过，故该错误不计为产品失败。

本轮未运行真实媒体、未打包、未部署、未修改 `I:\Tool`。后续应在链接环境修复且共享 target 空闲后运行带 `test-hooks` 的定向测试，再纳入 Task8B 总回归。
