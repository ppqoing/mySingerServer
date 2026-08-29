# Task 13C：结果摘要导出边界

## 结论

`dedup-node-store` 的验收导出器已切换为只读当前文件事实：从 `files.active=1` 连接
`contents` 和媒体特征表，按一个或多个规范媒体根做路径组件筛选，再按
`normalized_path、machine_id、display_path` 稳定排序。导出固定为 UTF-8、LF、无 BOM 的
`result-summary.tsv`，不读取 `tasks`、`task_items`，不生成 JSON/JSONL、metadata、pair lock
或 `.idx`，也不写 SQLite、cache 和媒体文件。

## 新接口和 CLI

库接口为：

```text
export_scan_result_summary(database_path, cache_root, media_roots, output_path)
```

`media_roots` 至少一项，每项使用 `NormalizedPath` 校验；重叠根不会复制同一 active 文件。
输出文件名固定为 `result-summary.tsv`，输出已存在、位于 cache root 或覆盖数据库时拒绝。

验收 CLI 固定接受：

```text
--database <node.db>
--cache-root <cache>
--media-root <root>        # 可重复，按出现顺序收集后规范化去重
--output <directory\result-summary.tsv>
```

CLI 不再接受 `--task-id`，也不再打印 task ID、metadata 路径或 JSON 状态文件。

## TSV 列和状态

头行固定保存以下字段：

```text
record_type status machine_id normalized_path display_path file_size md5 media_type base_complete
feature_payload_sha256 image_stage1_sha256 image_stage2_sha256 video_metadata_sha256
video_frame_stage1_0_sha256 ... video_frame_stage1_5_sha256
video_frame_stage2_0_sha256 ... video_frame_stage2_5_sha256
thumbnail_sha256 thumbnail_state contact_sheet_sha256 status_reason
```

实际文件使用 TAB 分隔。每个文件一条 `R` 行；footer 为 `F、row_count、前置头和数据行字节
SHA-256`。每个已存在的 payload 都按固定字段编码计算 SHA-256，不经过 JSON 序列化；未存在
的表行留空。当前 schema 没有缩略图表，`thumbnail_sha256` 留空并以
`unsupported_no_thumbnail_artifact` 明示；视频联系表存在时写入 SHA-256，缺失时该行标记
`MISSING`。内容/必需基础特征缺失为 `MISSING`，已存在但字段不完整或长度错误为
`INCONCLUSIVE`，不确定状态优先级高于缺失，避免把损坏数据当作普通缺失。

## SQLite sidecar 边界

导出顺序固定为：只读打开 SQLite → 开启只读事务并执行一次真实 `files` 查询，令 SQLite
完成首次 WAL-index 初始化 → 捕获主库/WAL/SHM 内容 hash → 查询 active 文件和特征 → 提交
只读事务并关闭连接 → 复核主库/WAL/SHM 内容 hash → 发布 TSV。这样首次只读打开产生的
SHM 初始化不会被误报为外部变化；sidecar 在快照后被修改时拒绝发布，且不会留下半成品
`result-summary.tsv`。只比较内容 hash，不把 SHM 的 mtime 更新误判为数据变化。

## TDD 证据

### RED

在旧实现上新增真实文件型 SQLite 测试后运行：

```text
cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export exports_active_files_from_media_roots_without_task_rows_as_tsv --locked -- --test-threads=1
```

旧接口真实编译失败：第三参数仍要求 `&str task_id`，测试传入媒体根列表时报
`expected &str, found &[PathBuf; 1]`。该 RED 证明旧导出器无法脱离 task/task_items，也没有
TSV 根筛选接口。

### GREEN

```text
cargo test -p dedup-node-store --features acceptance-tools --test result_summary_export --locked -- --test-threads=1
```

结果：`8 passed; 0 failed`。覆盖 active 文件与根组件边界、稳定排序、重叠根去重、完整/缺失/
不确定特征、图片和视频所有 payload hash、联系表 hash、旧 task 行隔离、首次 WAL/SHM 打开、
sidecar 变化拒绝、BOM/footer 篡改拒绝，以及不写数据库任务事实。

CLI 编译检查：

```text
cargo check -p dedup-node-store --features acceptance-tools --example export_scan_result_summary --locked
```

结果：exit 0。格式检查：`cargo fmt --all -- --check` 和指定文件 `git diff --check` 均通过。

## 移除/保留的兼容边界

- 移除导出器对 `task_id、tasks、task_items` 的生产查询和任务计数裁决。
- 移除 JSONL canonical、JSON metadata、三件套 pair lease/提交逻辑及 JSON 解析依赖。
- `validate_result_summary_pair` 仅保留旧函数名作为薄别名，实际只校验单个 TSV，不创建 pair
  文件；新调用应使用 `validate_result_summary`。
- SQLite schema 中既有运行态表定义暂不删除，后续产品路径不得写入；本报告只改变验收导出
  边界，不改 NodeEngine、Desktop、Central 或 inventory/lib。

本阶段未提交、未打包、未部署，未访问 `I:\Tool`。
