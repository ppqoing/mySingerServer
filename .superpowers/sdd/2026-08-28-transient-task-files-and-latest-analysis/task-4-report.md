# Task 4：缓存完整性与批量查询验收报告

## 范围

本任务集中实现 SQLite 基础/二筛缓存的结构完整性判断、真实批量载荷和二筛缺失消费。没有修改协议编号或 V5 既有 `BASE_MISSING_PROBE`、`BASE_MISSING_STAGE1`、`BASE_MISSING_CONTACT_SHEET` 位，也没有运行真实媒体、打包、部署或接触 `I:\Tool`。

## TDD 证据

1. 初始字段矩阵 RED：`valid_zero_features_are_hits_but_structural_gaps_are_missing` 在分类器和 `BaseCacheRecord` 扩展尚不存在时真实编译失败。
2. 字段保护 RED：新增 `invalid_stage1_dimensions_do_not_overwrite_existing_feature` 在旧 SQL 下真实运行失败；已有 640×480 被 `Some(0)` 覆盖，`load_complete_stage1` 不能再返回完整结果。
3. 最小 GREEN：把一筛尺寸/视频时长更新改为正值 `CASE`，并继续用 `COALESCE` 合并可选字段；新行仍保留非法字段供分类器报告缺失，既有有效字段不被部分结果覆盖。

测试使用真实 SQLite 行为、`Connection::trace_v2`、临时 JPEG 和 Worker 处理器，不使用 `read_source`、`contains` 或其他源码字符串测试。

## 实现结果

- 新增 `CacheCompleteness` 和 `classify_cache_completeness`，统一校验非零尺寸、视频时长、固定 BLOB 长度、有限 Sobel、六槽/至少四个成功一筛帧；全零 PDQ、pHash 和 Sobel 仍是合法命中。
- `BaseCacheRecord` 携带图片二筛结构、视频六槽二筛数组和联系表相对路径。path/key 批量入口各用三条固定业务 SELECT，一次载入基础字段、视频一筛槽和视频二筛槽，按 ordinal 保留输入顺序、重复项和文件大小区分。
- path/key 1000 项 SQLite trace 分别断言三条业务 SELECT、无任务表 INSERT/UPDATE/DELETE；变量上限切块行为断言三批共九条 SELECT，禁止退化为逐项查询。
- 非法 NULL、尺寸、长度、浮点和视频槽位只使当前记录/槽位成为缺失，不拖垮邻项；视频二筛掩码只覆盖一筛成功但二筛缺失的槽位。
- 本机联系表必须匹配 MD5 派生相对路径、位于联系表根目录内并能解码固定六槽 JPEG；损坏文件会回退原视频并重建。远端基础记录只初始化可导入字段，不伪造本机联系表 artifact。
- `BaseCompute` 和 phase2 使用集中分类结果；完整二筛从批量原始记录重发 outbox，部分视频只派发真实缺失槽位。Worker 失败不写特征占位，部分写入保留既有有效字段。
- 为覆盖“同一进程后台连接”的现行 transient runtime 边界，`local_analysis` 的 reopen 夹具改用 `NodeStore::reopen()`，不恢复跨进程启动清理后的运行态。

## 验证命令与结果

所有 Cargo 命令均在同一 PowerShell 进程显式设置 `CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`、`CARGO_INCREMENTAL=0`、dev/test debug=0，并清除 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`。每次重型命令前检查到 C=18.26--18.27 GiB、D=10.31 GiB，未低于 10 GiB。

| 验证 | 结果 |
|---|---|
| `cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1` | 17/17 通过 |
| `cargo test -p dedup-node-store --locked -- --test-threads=1` | 50 通过，0 失败 |
| `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1` | 59/59 通过 |
| `cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1` | 8/8 通过 |
| `cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1` | 23/23 通过 |
| phase2 partial retry 定向测试 | 1/1 通过 |
| 视频二筛缺失槽位定向测试 | 1/1 通过；两项均只收到 `[2,3]` |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

另有一次非 brief 验证命令 `cargo check -p dedup-node-engine --tests --features test-hooks` 触发基线 `actor.rs:2535` 未限定 `NormalizedPath` 的 E0433；该引用在基线 `196575c3` 已存在，本任务未修改 actor，也未将其作为 Task 4 验收门禁。按 brief 的 `base_compute_pipeline --features test-hooks` 集成验证已完整通过。

## 风险与边界

- 联系表校验复用媒体 crate 的固定三列两行六槽 JPEG 解码规则；任意可解码但不符合联系表尺寸约束的 JPEG 仍会按缺失处理，这是当前联系表格式契约。
- `scan/engine.rs` 也有最小接入改动，因为基础结果持久化和已有联系表检查必须遵守同一缺失掩码与 artifact 校验；没有引入物理盘 lane、TSV、任务恢复或协议/UI 行为。
