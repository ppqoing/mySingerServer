# Task 6A 实施报告：瞬态 TSV 任务文件与原位状态

## 范围

本次只实现 Task 6A：固定八列 UTF-8 无 BOM TSV、按物理盘 lane 建文件、有限预读、任务行身份校验和 `P/C/F` 原位状态。未修改 scheduler、dispatcher、actor、BaseCompute、pipeline、SQLite、协议或 UI；Task 6B 的加权调度留待后续提交。

## 实现

- 新增 `crates/node-engine/src/task_files.rs`，由 `TransientTaskFileSet` 单独拥有每个 lane 的追加、读取和状态句柄。
- `TaskWorkMask` 固定 bit 0..2 基础缺失、bit 3 MD5、bit 4 图片二筛、bit 5..10 视频槽位，并拒绝空值、未知位和不匹配的工作类型组合。
- 任务行严格写成 `P/C/F + UUID v7 + work_kind + normalized/display path + u64 size + lowercase MD5 + 16 位 lowercase mask`，路径拒绝 tab/CR/LF 和非 UTF-8 显示路径。
- run 目录使用规范 UUID v7 且必须新建；文件名只由排序去重的物理盘号和 `hdd/ssd/unknown` 组成；提供精确 `remove_run/cleanup_run`。
- append 先完整校验和序列化，flush 成功后才推进 published 边界；seal 后拒绝追加。预读窗口为 `max(2, per_disk_limit * 2)`，只保存行首队列对象和其余行的最小偏移元数据。
- `take_lane` 支持队首或已观察身份，领取不修改磁盘状态；`mark_completed/mark_failed` 在写前 flush，并重读完整行核对 run/lane/offset/length/item/mask，只允许 `P→C/F`，ACK 未完成时保持 `P`。
- 提供 `TaskLaneHead`、`lane_heads`、`head_identity`、`publication_epoch` 等后续 dispatcher 所需的拥有型队首和通知序号接口。

## TDD 与验证

首个 RED：模块不存在时运行

```text
cargo test -p dedup-node-engine --test transient_task_files task_rows_are_fixed_tsv_without_json_or_bom --locked -- --test-threads=1
```

实际失败为 `unresolved import dedup_node_engine::task_files`。实现后在固定 `C:\tmp\rust-v2-core-scope-target`、清除 MSVC 不兼容环境变量并关闭增量/debug 后验证：

- `transient_task_files`：11/11 通过。
- `dedup-node-engine --lib`：66/66 通过；仅保留既有 scheduler dead-code warning。
- `cargo fmt --all -- --check`：通过。
- `git diff --check`：通过。

测试覆盖固定字节、无 JSON/BOM/idx、UUID/mask/控制字符/非 UTF-8、复合和 Unknown 文件名、双 lane 重复项、sealed/published/有限预读、ACK 失败保持 P、ACK 成功 C、文件失败 F、行体字节不变、错误身份/offset/lane/run/mask、损坏行、非法状态转换以及 terminal/inflight 边界。

本轮验证前后磁盘空间约为 C 盘 17.62 GiB、D 盘 10.23 GiB；未触发清理。未运行真实媒体、打包、部署，也未触碰 `I:\Tool`。
