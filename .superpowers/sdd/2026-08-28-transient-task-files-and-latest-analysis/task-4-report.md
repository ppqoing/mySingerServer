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

## Follow-up：审查 Important 修复

本轮仍按先 RED、后 GREEN 执行，测试均为真实 SQLite/NodeEngine 行为断言，没有读取源码字符串。

### RED

- 中心二筛批次夹具先证明旧实现把已有 `[0,1,4,5]` 与请求成功槽 `[0,1,2,3,4,5]` 全量下发，观察到 Worker 请求 `[0,1,2,3,4,5]`，而不是只请求 `[2,3]`。
- raw SQLite 把图片 stage1 的 `width` 改为 `0` 后，旧解码仍返回 `Some`，本地分析 `skipped_incomplete` 为 `0`。
- 已有完整图片一筛再次提交 `ImageStage1Fields::default()` 时，旧 outbox 载荷把宽高、PDQ、Quality 编成空值，未反映 SQL 合并后的有效行。

### GREEN

- `run_stage2_batch_internal` 使用 `video_stage2_missing_slots` 与中心要求成功槽位的交集；phase2 的重发/派发与本地分析统一检查 `BASE_MISSING_PROBE | BASE_MISSING_STAGE1`，畸形或未完成基础记录只取消当前任务项，不启动 Worker。
- `decode_stage1_fields` 拒绝零尺寸、Quality>100、错误 PDQ 长度；分析候选改用一次批量 `BaseCacheRecord` 查询和集中完整性分类，视频探测尺寸/时长不合法时跳过该内容。
- 图片 stage1、视频元数据、视频槽位 stage1 的 outbox 编码均在同一 SQLite 事务中回读 SQL 合并结果，再生成同步载荷；中心无需新增兼容合并规则。
- 为新门禁补齐一个既有 actor Worker 屏障测试夹具的 `base_complete` 前置，未放宽生产门禁。

### Follow-up 验证

所有 Cargo 命令均显式使用 `CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`、`CARGO_INCREMENTAL=0`、dev/test debug=0，并清除 C/C++ 工具链和 wrapper 环境变量。各次重型命令前检查 C/D 空间；本轮记录 C=18.20--18.21 GiB、D=10.31 GiB，未触及 10 GiB 停止线。

| 验证 | 结果 |
|---|---|
| `cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1` | 18/18 通过 |
| `cargo test -p dedup-node-store --locked -- --test-threads=1` | 全量通过（含 4 个库测试与全部集成测试） |
| `cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1` | 10/10 通过 |
| `cargo test -p dedup-node-engine --test base_compute_pipeline --features test-hooks --locked -- --test-threads=1` | 59/59 通过 |
| `cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1` | 23/23 通过 |
| `cargo test -p dedup-node-engine --lib central_cache --locked -- --test-threads=1` | 1/1 通过 |
| `cargo test -p dedup-central-store --locked -- --test-threads=1` | 3 通过，1 个 PostgreSQL 环境测试按要求忽略 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 64/64 通过；含补齐夹具后的 actor 测试 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

未执行真实媒体、打包、部署、TSV/lane/任务恢复/协议/UI 改动，也未访问 `I:\Tool`。

## Follow-up 3：快照畸形行与远端二筛精确一次

本轮针对复审剩余两个 Important 继续遵循真实行为 RED→最小 GREEN；断言只观察
SQLite 快照、任务状态、Worker 调用和 outbox 实体/槽位，不使用源码字符串匹配。

### RED

- 快照 raw SQLite 夹具把一个视频元数据的 `duration_ms` 和一个视频槽位的 `time_ms` 改为负数。
  旧实现先把负 duration 编为缺失字段，负 frame time 再直接返回错误；分页无法稳定跳过该内容，
  `snapshot_skips_malformed_video_rows_without_null_overwrite` 真实失败于首个有效邻居断言。
- 中心二筛测试缓存返回完整六槽，而本机仅请求 `[0]` 或请求 `[0,1]` 且预存槽 0。旧实现
  将六槽全部写入 outbox，并在缓存完整后再次全量重发，真实观察到
  `[0,1,2,3,4,5,0,1,2,3,4,5]`，违反 Worker=0、请求交集和精确一次。

### GREEN

- `snapshot.rs` 在视频元数据和视频一筛槽位读取边界过滤负 duration/time；畸形 raw 行不进入
  payload、不以 `None` 覆盖中心旧值，稳定游标仍可分页发出有效邻居并完成。其他快照表和协议
  载荷保持不变。
- phase2 在远端查询前冻结每个内容的本地有效图片二筛或请求视频槽位掩码；远端完整六槽返回
  只按 `expected ∩ missing` 持久化，选择性重发只按冻结的 preexisting 集合执行，导入结果不会
  再次被当成本地旧缓存重发。`dispatch_missing` 保留全部成功一筛槽位作为中心要求，刷新后
  仍只把集中缺失掩码交给 Worker。
- 增加真实 NodeStore outbox 类型/槽位/计数行为测试：仅请求槽 0 时 Worker 调用为 0 且只产生
  一个 `video_frame_stage2` 槽 0；预存槽 0、导入槽 1 时两槽各一次且不产生槽 2..5。

### Follow-up 3 验证

所有 Cargo 命令均在同一 PowerShell 命令中显式使用
`CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`、`CARGO_INCREMENTAL=0`、dev/test debug=0，
并清除 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`；每条重型命令前检查 C/D
空间。本轮记录 C 约 18.2 GiB、D 约 10.3 GiB，均未低于 10 GiB 停止线。

| 验证 | 结果 |
|---|---|
| 快照畸形行定向 `cargo test -p dedup-node-store --test outbox snapshot_skips_malformed_video_rows_without_null_overwrite --locked -- --test-threads=1` | 1/1 通过 |
| 远端导入/选择性重发定向 `cargo test -p dedup-node-engine --lib remote_stage2_ --locked -- --test-threads=1` | 2/2 通过 |
| `cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1` | 22/22 通过 |
| `cargo test -p dedup-node-store --locked -- --test-threads=1` | 全量通过 |
| `cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1` | 11/11 通过 |
| `cargo test -p dedup-node-engine --test base_compute_pipeline --features test-hooks --locked -- --test-threads=1` | 59/59 通过 |
| `cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1` | 23/23 通过 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 66/66 通过 |
| `cargo test -p dedup-central-store --locked -- --test-threads=1` | 3 通过，1 个 PostgreSQL 环境测试忽略 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

未执行真实媒体、打包、部署、TSV/lane/任务恢复/协议/UI 改动，也未访问 `I:\Tool`。

## Follow-up 2：阶段二空交集、Quality 覆盖与重发门禁

本轮针对复审指出的三个 Important 继续按真实行为测试先 RED、再最小 GREEN；测试只观察
SQLite、outbox、任务状态和 Worker 调用，不使用源码字符串匹配。

### RED

- 中心仅请求视频槽 `[0]`、本机已有 `[0,1,4,5]` 时，旧 phase2 先将交集算为空但仍创建一个 Worker 请求；行为测试观察到 `calls=1`，期望为 `0`。
- 已有合法图片/视频槽位 Quality 分别为 `90/80` 时，传入 `101/255` 直接触发 SQLite Quality CHECK，批次不能完成；负视频时长被 `as u64` 解码为 `18446744073709551615`；畸形图片 stage1 仍能通过公开二筛重发入口新增 outbox。

### GREEN

- phase2 先以集中 `CacheCompleteness::video_stage2_missing_slots` 与中心请求槽位求交集；对交集为空但请求槽已在本机完整的情况，通过 `republish_stage2_slots_from_cache` 只重发这些请求槽，再成功提交任务项，不启动 Worker。视频空槽请求在批次入口拒绝；图片空槽仍保留既有图片 Worker 请求语义。
- `republish_complete_stage2_from_cache` 和按槽重发入口统一经过完整性分类，非法尺寸、Quality、BLOB、Sobel、视频探测字段和缺失槽位均拒绝重发。
- 图片与视频槽位 stage1 的 SQL 合并使用 Quality `CASE`，写入参数先将 `>100` 变为缺失；合法 `0` 仍可写入。事务提交前回读合并行后编码 outbox，因此旧合法 Quality 不会被部分输入降级。
- 单条内容加载和只读快照均拒绝把负视频时长/槽位时间转换成巨大无符号值；负 duration 作为探测缺失，负 frame time 使快照读取返回明确存储错误。

### Follow-up 验证

所有 Cargo 命令均在同一 PowerShell 命令中显式使用 `CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`、`CARGO_INCREMENTAL=0`、dev/test debug=0，并清除 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`；每条重型命令前检查 C/D 空间。本轮记录 C=18.06--18.17 GiB、D=10.31 GiB，未触及 10 GiB 停止线。

| 验证 | 结果 |
|---|---|
| 复审 RED：旧实现中心已有槽空交集行为测试 | `calls=1`，期望 `0` |
| 复审 RED：旧实现 `content_cache` | 22 个测试中 4 个失败（Quality CHECK×2、负 duration 转换、畸形重发门禁） |
| `cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1` | 22/22 通过 |
| `cargo test -p dedup-node-store --locked -- --test-threads=1` | 55/55 通过 |
| `cargo test -p dedup-node-engine --test base_compute_pipeline --features test-hooks --locked -- --test-threads=1` | 59/59 通过 |
| `cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1` | 11/11 通过 |
| `cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1` | 23/23 通过 |
| `cargo test -p dedup-node-engine --lib central_cache --locked -- --test-threads=1` | 1/1 通过 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 64/64 通过 |
| `cargo test -p dedup-central-store --locked -- --test-threads=1` | 3 通过，1 个 PostgreSQL 环境测试忽略 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

NodeEngine 全量初次回归曾因图片请求的既有空 `frame_slots` 夹具未到 Worker 屏障；已将“空交集直接完成”收窄为视频路径，图片行为测试和随后 64/64 全量均通过。未执行真实媒体、打包、部署、TSV/lane/任务恢复/协议/UI 改动，也未访问 `I:\Tool`。

## Follow-up 4：中心特征合并与 phase2 正式载荷校验

本轮针对复审剩余的中心 UPSERT 和 phase2 outbox 断言继续执行真实行为测试优先、最小实现；
没有使用源码字符串作为行为测试。

### RED 与环境边界

- 新增 `crates/central-store/tests/content_upsert.rs` 表级 PostgreSQL 行为夹具：先写完整图片、
  视频元数据、视频槽位一筛/二筛，再发送 NULL、空二筛字段、`decoded=false`、非法 Quality
  `101/255` 和 `base_complete=false`，同时覆盖初次部分行后续完整补齐与合法 Quality `0`。
- 本机未设置 `DEDUP_TEST_POSTGRES_URL`，且 5432 无 PostgreSQL 服务；该真实表级测试按现有契约
  保持 `ignored`，没有将环境缺失伪报为 RED/GREEN 或 PASS。测试体在环境存在时实际调用
  `CentralStore::connect`、`apply_sync_batch` 和 SQL 查询，不是源码扫描测试。
- 原 phase2 测试只读取 payload 固定偏移，属于断言缺口；已先替换为正式载荷解码器并使用不同本地/远端
  特征值，保留两个精确一次场景的行为边界。

### GREEN 实现

- 中心 `contents.base_complete` 只允许从 false 变 true；图片 stage1 的尺寸、PDQ、Quality，
  视频元数据的时长/尺寸，视频 frame stage1 的尺寸、PDQ、Quality 均使用条件合并或旧值保护；
  非法 Quality 先转为缺失，合法 `0` 保留，`decoded=false` 不覆盖旧的 true。
- 图片与视频 frame stage2 的空字节载荷解码为缺失并通过 `COALESCE` 保留旧列；初次空二筛行
  可保存，后续完整 payload 可补齐。中心仍由数据库固定长度约束保护真实特征长度。
- phase2 outbox 测试按版本、MD5、文件大小、槽位、长度和完整字段解码；预存槽使用本地特征，远端
  导入槽使用不同特征，断言 expected 槽各且仅一次，未请求槽不产生载荷。

### Follow-up 4 验证

所有 Cargo 命令在同一 PowerShell 命令中显式设置 `CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`、
`CARGO_INCREMENTAL=0`、dev/test debug=0，并清除 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`；
每条重型命令前检查 C/D，记录 C=17.95 GiB、D=10.31 GiB，未低于 10 GiB 停止线。

| 验证 | 结果 |
|---|---|
| `cargo test -p dedup-central-store --test content_upsert --locked -- --test-threads=1` | 编译通过，1 个 PostgreSQL 集成测试因环境变量缺失 ignored |
| `cargo test -p dedup-central-store --locked -- --test-threads=1` | 3 通过，2 个 PostgreSQL 集成测试按契约 ignored |
| `cargo test -p dedup-node-engine --lib analysis::phase2::tests::remote_stage2 --locked -- --test-threads=1` | 2/2 通过 |
| `cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1` | 22/22 通过 |
| `cargo test -p dedup-node-store --locked -- --test-threads=1` | 全量通过 |
| `cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1` | 11/11 通过 |
| `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1` | 59/59 通过 |
| `cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1` | 23/23 通过 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 66/66 通过 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

未执行真实媒体、打包、部署、TSV/lane/任务恢复/协议/UI 改动，也未访问 `I:\Tool`。
