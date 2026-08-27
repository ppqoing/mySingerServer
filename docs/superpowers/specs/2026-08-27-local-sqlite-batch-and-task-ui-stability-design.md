# 本地 SQLite 真批量查询与任务界面稳定性设计

## 1. 目标

本次改动同时解决两个已经由源码事件链确认的问题：

1. Node 基础缓存虽然按最多 1,000 项组织批次，但本地 SQLite 仍按文件逐条执行查询，导致 SQLite actor 被大量短查询占用。
2. 桌面端任务中心和总览页共用的 `tasks` 模型被旧持久任务快照与统一运行任务快照交替覆盖，造成同一任务在 `base_compute` 与“基础计算”之间反复切换；连接 PostgreSQL 后同步事件增多，切换更加频繁。

本次不调整 Worker 数量、磁盘读取调度、远端 PostgreSQL 缓存协议、任务状态机或媒体文件一致性规则。

## 2. 已确认根因

### 2.1 SQLite 仅有批量调度，没有批量 SQL

`prepare_path_batch` 最多取 1,000 个路径，但 `NodeStore::lookup_scanned_paths` 在 Rust 循环中对每个路径调用一次 `query_row`。命中后又逐项调用 `load_base_cache_record`，图片或视频通常还会继续执行内容、元数据和一筛查询。

Hash 完成后的 content 查询同样在循环中逐项调用 `content_id_by_key` 和 `load_base_cache_record`。因此批次大小为 N 时，SQL 数量仍随 N 线性增长。

### 2.2 同一个 UI 属性存在两个写入者

`UiEvent::RuntimeTasksChanged` 将统一运行任务映射为中文标题，并写入 `window.tasks`；`UiEvent::ViewChanged` 又将旧 `TaskView` 映射结果写入同一个属性，其中任务类别仍是原始 `base_compute`。

运行任务事件、节点刷新、中心库追赶和同步完成事件会交错到达。两条路径都整体替换 Slint `VecModel`，因此总览页和任务中心必然随最后到达的事件切换，并不存在可保持的稳定行身份。

## 3. 方案选择

采用以下组合方案：

- SQLite 使用动态 `VALUES` CTE 和参数绑定，不创建临时表，也不依赖 JSON1。
- 每个 SQLite 子批固定执行两条只读 SELECT：第一条读取内容与媒体元数据，第二条读取视频一筛帧；在 Rust 内按请求序号组装结果。
- 统一运行任务快照成为 `window.tasks` 的唯一写入来源。

未采用的方案：

- 临时表方案会引入插入、清空、事务和写锁，增加 SQLite actor 的写入压力。
- 把视频帧也塞进单条巨大 JOIN 会让每个视频的内容与元数据最多重复六次，解码和错误处理更复杂。
- 在 UI 按标题、时间或刷新间隔抑制切换只能隐藏双写问题，无法建立明确的数据所有权。

## 4. SQLite 批量查询设计

### 4.1 对外接口

在 `NodeStore` 增加两个保持输入位置的接口：

```rust
lookup_base_cache_by_paths(&[ScannedPath])
    -> Result<Vec<Option<BaseCacheRecord>>, StoreError>

lookup_base_cache_by_keys(&[ContentKey])
    -> Result<Vec<Option<BaseCacheRecord>>, StoreError>
```

返回向量必须与输入严格等长、同序：

- 命中返回带本地 `content_id` 的 `BaseCacheRecord`。
- 缺失返回 `None`。
- 重复路径或重复内容键必须在各自输入位置重复返回，不能因内部去重丢失任务项。
- 空输入直接返回空向量，不执行 SQL。

现有单项接口继续保留给非基础计算调用方，避免扩大本次改动范围。基础计算流水线只改用新批量接口。

### 4.2 SQL 结构

路径批次第一条查询的请求 CTE 包含：

- `ordinal`：输入位置。
- `normalized_path`：规范路径。
- `file_size`：枚举时文件大小。

它按当前 `machine_id + normalized_path + file_size + active` 连接 `files`、`contents`、`image_stage1` 和 `video_metadata`。

内容键批次第一条查询的请求 CTE 包含：

- `ordinal`：输入位置。
- `md5`。
- `file_size`。

它连接 `contents`、`image_stage1` 和 `video_metadata`。

第二条查询使用相同请求 CTE，读取 `video_frame_stage1`，按 `ordinal, slot` 排序。Rust 组装器复用当前完整性规则：

- 图片只有宽、高、PDQ、质量全部存在时才形成完整一筛。
- 视频必须具有严格的六个槽位记录，并且至少四个成功解码槽位字段完整。
- 不完整特征仍返回内容与元数据，但 `stage1` 为 `None`。

### 4.3 参数上限与切块

正式依赖启用 rusqlite `limits` 能力，读取当前连接的 `SQLITE_LIMIT_VARIABLE_NUMBER`。每个请求项使用三个绑定参数，子批大小取：

```text
min(1,000, 当前变量上限可容纳的请求数)
```

路径查询还需预留机器 ID 参数。若运行时变量上限无法容纳一个请求，返回明确 `StoreError`，不得退回逐项 SQL。

当前固定依赖的 bundled SQLite 默认变量上限为 32,766，正常发行构建可一次容纳 1,000 项；运行时切块用于防止自定义构建参数降低该上限。

### 4.4 Engine 接入

`BaseStoreHandle` 增加两个对应的 actor 调用。一次 actor call 完成一个逻辑批次，调用方不得在返回结果的循环中再次执行本地缓存 SELECT。

- path 阶段将已保留的 `ScannedPath` 一次传入，然后按位置与 `ReservedScanPath` 配对。
- content 阶段先冻结当前 ready Hash 队首最多 1,000 个 `ContentKey`，一次查询本地结果，再按原 FIFO 顺序执行远端判定和 decode credit 门禁。
- content 阶段使用一个与冻结队首严格对齐的本地结果游标；游标未消费完之前不重复查询同一批，也不查询下一批。
- 若 decode credit 暂不可用或远端容量已满，未消费的 Hash item 和对应本地结果都保留在各自队首；不得改变 item 身份、完成许可或远端请求容量守恒。

远端缓存导入后的单项重新读取属于写入后的状态确认，不在本次输入批量查询范围内。

## 5. UI 单一任务数据源设计

### 5.1 数据所有权

`RuntimeTaskControllerState` 是任务中心和总览最近任务的唯一数据源：

- `UiEvent::RuntimeTasksChanged` 独占 `window.tasks` 的写入。
- `UiEvent::ViewChanged` 继续更新节点、设置、PostgreSQL 状态、故障诊断等属性，但不再写任务列表。
- `running_count` 由统一运行任务摘要计算，并随 `RuntimeTasksChanged` 一起更新，避免任务列表与计数来自不同模型。

旧 `DesktopViewState::tasks` 和 `TaskView` 暂时保留给现有控制逻辑及兼容调用方，但不再直接驱动当前两个页面。若以后需要单独展示历史持久任务，应新增独立模型和页面，不得重新共用 `window.tasks`。

### 5.2 初始状态与刷新

控制器启动时发布一个初始统一运行任务快照，保证 Slint 的任务属性从启动开始就只有一个所有者。随后仍按现有两秒轮询、运行任务事件和会话变化刷新。

列表排序、选择键和详情加载继续使用 `(RuntimeTaskOwner, runtime_task_id)`，本次不改变运行任务 wire 协议。

## 6. 错误处理

- 任一 SQLite 子批查询或行解码失败时，整个逻辑批次返回错误，不返回部分成功结果。
- 查询结果出现非法媒体类型、负文件大小、无效 MD5 长度或视频槽位结构异常时，保持当前 `StoreError` 语义。
- Engine 收到长度不匹配的批量结果时立即终止当前基础计算，并报告明确的 SQLite 批量契约错误。
- UI 的普通 View 刷新即使失败或高频到达，也无权改变已经发布的运行任务模型。

## 7. TDD 与验收

### 7.1 SQLite RED 测试

使用真实临时 SQLite 数据库和 SQL trace，而不是检查源码文本：

- 约 1,000 个混合图片、视频、普通内容和缺失项。
- 乱序输入、重复路径、重复内容键、同 MD5 不同大小。
- 严格断言输出长度、位置、内容 ID、媒体元数据和一筛完整性。
- 断言正常 1,000 项子批只执行两条业务 SELECT，不随项目数量增长。

### 7.2 UI RED 测试

使用真实 `MainWindow` 连续应用事件：

1. 发布标题为“基础计算”的 `RuntimeTasksChanged`。
2. 紧接着发布包含原始 `base_compute` 的 `ViewChanged`。
3. 断言任务行仍为 Node runtime 身份、标题仍为“基础计算”、运行 ID 不变。
4. 反向交错多轮，断言任务列表和运行数量不随事件顺序变化。

### 7.3 Engine 回归

- path 本地完整命中仍直接完成。
- content 本地命中、远端缺失和 Worker 计算保持精确 item 身份。
- 1,000 项边界、重复输入、decode credit 不足和远端批容量守恒不变。
- 运行 node-store、node-engine、desktop-core 和 desktop-ui 定向测试，再执行与本次文件相关的完整 crate 测试、格式检查和 `git diff --check`。

## 8. 完成标准

满足以下条件才算完成：

1. 基础计算 path/content 本地缓存查询不再在 item 循环中执行 SELECT。
2. 正常 1,000 项输入由固定两条业务 SELECT 完成一个 SQLite 子批。
3. 缓存结果与输入严格等长同序，重复项、缺失项和特征完整性行为不变。
4. 任意 `ViewChanged` 与 `RuntimeTasksChanged` 到达顺序都不会改变统一任务列表的数据来源。
5. 总览和任务中心稳定显示中文任务标题，不再在 `base_compute` 与“基础计算”之间切换。
6. 所有新增 RED 测试先在旧实现上按预期失败，修复后通过，相关回归无新增失败。
