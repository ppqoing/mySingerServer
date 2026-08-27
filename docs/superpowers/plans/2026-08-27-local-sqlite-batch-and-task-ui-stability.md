# 本地 SQLite 真批量查询与任务界面稳定性实施计划

> **执行要求：** 使用 `superpowers:subagent-driven-development` 逐项实施，并在每个任务中遵循 `superpowers:test-driven-development`，先保存 RED 证据，再完成最小修复。

**目标：** 把基础计算的本地路径/内容缓存读取改为真正的批量 SQL，并消除 `ViewChanged` 与 `RuntimeTasksChanged` 对同一任务列表的交替覆盖。

**架构：** `NodeStore` 负责按 SQLite 变量上限切块，通过动态 `VALUES` CTE 每个子批执行固定两条业务 SELECT，并按输入序号还原结果；`BaseStoreHandle` 与基础计算协调器只传递整批数据并维护本地结果游标。桌面 UI 由 `RuntimeTasksChanged` 独占任务列表与运行中数量，普通视图刷新不再写入这两个属性。

**技术栈：** Rust 2024、rusqlite、Tokio actor、Slint、Cargo 集成测试。

---

## 任务 1：锁定 NodeStore 批量查询契约

**文件：**

- 修改：`crates/node-store/Cargo.toml`
- 修改：`crates/node-store/src/content.rs`
- 修改：`crates/node-store/src/rows.rs`
- 修改：`crates/node-store/src/lib.rs`
- 修改：`crates/node-store/tests/content_cache.rs`

**步骤：**

1. 在真实临时 SQLite 上新增路径批量与内容键批量测试，覆盖空输入、乱序、重复项、缺失项、同 MD5 不同大小、图片完整特征、视频六槽位与不完整特征。
2. 使用 rusqlite trace 或等价真实执行计数，断言 1,000 项正常子批只产生两条业务 SELECT；先运行测试并保存旧实现缺少接口或 SQL 数量线性增长的 RED 结果。
3. 为 rusqlite 启用 `limits` 能力；增加带中文注释的批量请求组装与结果还原辅助类型。
4. 实现 `lookup_base_cache_by_paths` 和 `lookup_base_cache_by_keys`：读取运行时变量上限、每项三个参数、路径批预留机器参数、上限不足时明确失败，不得退回逐项查询。
5. 两条 SELECT 共用同一 `VALUES(ordinal, ...)` 请求 CTE：第一条装配内容、图片和视频元数据，第二条装配视频槽位；按 `ordinal` 还原等长同序 `Vec<Option<BaseCacheRecord>>`。
6. 运行：

   `cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1`

**完成条件：** 位置、重复、缺失和现有特征完整性语义不变，SQL 数量不随 1,000 个项目线性增长。

## 任务 2：把批量 API 接入 SQLite actor

**文件：**

- 修改：`crates/node-engine/src/scan/base_persistence.rs`
- 修改：`crates/node-engine/tests/base_compute_pipeline.rs`

**步骤：**

1. 先新增 actor 契约测试：一次逻辑 path/content 批次只发起一次 `BaseStoreHandle` 调用，结果必须等长同序；保存 RED 结果。
2. 在 `BaseStoreHandle` 增加路径批量与内容键批量方法，闭包内分别只调用一次对应 `NodeStore` API。
3. 保留现有单项方法供非基础计算路径使用，不扩大本次迁移范围。
4. 运行：

   `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1`

**完成条件：** actor 边界传递整批数据，不在返回结果循环中重新进入 SQLite。

## 任务 3：迁移 path 阶段本地缓存读取

**文件：**

- 修改：`crates/node-engine/src/scan/base_compute.rs`
- 修改：`crates/node-engine/tests/base_compute_pipeline.rs`

**步骤：**

1. 新增 1,000 项、重复路径、混合命中/缺失测试，断言 item 身份、完成计数和 Worker 分派不变；先保存逐项本地查询的 RED 证据。
2. `prepare_path_batch` 一次提交保留的 `ScannedPath` 列表，并通过位置与 `ReservedScanPath` 配对。
3. 检查批量结果长度；不匹配时返回明确的 SQLite 批量契约错误。
4. 删除基础计算 path 循环中的 `load_base_cache_record` 调用，保留远端导入后的单项状态确认。
5. 运行任务 2 的 Node Engine 命令。

**完成条件：** path 批次本地读取没有 item 级 SELECT，完整命中仍直接持久化且不启动 Worker。

## 任务 4：迁移 content 阶段并保持游标/许可守恒

**文件：**

- 修改：`crates/node-engine/src/scan/base_compute.rs`
- 修改：`crates/node-engine/src/scan/cache_resolver.rs`（仅在现有请求结构需要承载本地批量上下文时）
- 修改：`crates/node-engine/tests/base_compute_pipeline.rs`

**步骤：**

1. 增加 ready Hash 混合命中/缺失、重复内容键、decode credit 已满、远端容量已满及 1,000 项边界测试；先保存 RED 结果。
2. 冻结 FIFO 队首最多 1,000 个 `ContentKey`，一次取得本地结果，建立与冻结项严格对齐的本地结果游标。
3. 游标未消费完时不重复查询同一批，也不查询下一批；credit 不足时同时保留 Hash item、本地结果、完成许可和远端容量所有权。
4. 删除基础计算 content 循环中的 `content_id_by_key + load_base_cache_record` 组合查询。
5. 运行任务 2 的 Node Engine 命令。

**完成条件：** content 批量读取固定，远端降级、Worker 分派和所有 ownership/容量守恒测试继续通过。

## 任务 5：让运行任务成为 UI 唯一数据源

**文件：**

- 修改：`crates/desktop-ui/src/bindings.rs`
- 修改：`crates/desktop-ui/src/models.rs`（仅在运行中计数尚无统一映射时）
- 修改：`crates/desktop-ui/tests/bindings_contract.rs`
- 修改：`crates/desktop-core/src/app.rs`（仅在启动时未发布初始快照时）
- 修改：`crates/desktop-core/tests/controller_runtime_tasks.rs`

**步骤：**

1. 用真实 `MainWindow` 新增事件交错测试：先 `RuntimeTasksChanged` 后 `ViewChanged`、反向顺序并重复多轮；断言 Node owner、runtime ID、中文“基础计算”标题和运行中数量不变。先运行并保存旧双写行为的 RED 结果。
2. 从 `UiEvent::ViewChanged` 分支移除 `window.tasks` 和 `running_count` 写入。
3. 在 `UiEvent::RuntimeTasksChanged` 同一次映射中更新任务列表和运行中数量，添加中文注释说明该分支独占数据所有权。
4. 核对控制器启动时会发布空的统一运行任务快照；若没有，仅补最小初始发布及契约测试。
5. 运行：

   `cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1`

   `cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked -- --test-threads=1`

**完成条件：** PostgreSQL/节点/普通视图刷新再频繁也不能改写任务行身份、标题或运行中数量。

## 任务 6：集成验证、审查与提交

**文件：**

- 修改：本计划涉及的源码和测试
- 新增：`docs/verification/2026-08-27-local-sqlite-batch-and-task-ui-stability.md`

**步骤：**

1. 运行 `cargo fmt --all -- --check` 与 `git diff --check`。
2. 运行 NodeStore、Node Engine、Desktop Core、Desktop UI 的上述定向测试，再运行受影响 crate 的完整测试；PostgreSQL 环境用例若未配置，明确记录跳过原因。
3. 在中文验证文档记录 RED 证据、最终测试数量、SQL trace 数量及已知环境限制。
4. 使用 `gpt-5.6-sol`、`max` 推理强度做一次独立最终审查，只处理可复现的范围内问题。
5. 精确暂存计划涉及文件，提交修复并正常推送 Rust V2 分支；禁止 force push，禁止触碰 `I:\Tool`。
6. 修复分支验证通过后，将 Rust V2 合并到 `main`，处理真实冲突、复跑核心门禁并正常推送 `main`。

**完成条件：** 批量 SQL 和 UI 单写者的行为测试通过，审查无未解决阻塞项，两个远端分支 SHA 与本地一致。
