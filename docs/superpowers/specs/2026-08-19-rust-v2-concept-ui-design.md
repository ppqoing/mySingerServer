# Rust V2 概念图 UI 重构设计

## 1. 目标

本设计将 Rust V2 管理工具的 Slint 界面整体重构为概念图所示的浅色 Fluent 高密度桌面管理界面。重构覆盖应用外壳、七个主导航入口、结果工作区、审核删除工作区、设置与诊断工作区，以及所有空状态、禁用状态和危险操作确认。

本次只改变 UI 结构、样式和前端交互组织，不增加新的后端命令、数据库字段、网络协议或媒体计算流程。现有 Rust 属性、回调、模型和删除安全链保持兼容。

概念图基准为：

- `docs/ui-preview/rust-v2/01-overview-nodes.png`
- `docs/ui-preview/rust-v2/02-scan-tasks.png`
- `docs/ui-preview/rust-v2/03-exact-cross-machine.png`
- `docs/ui-preview/rust-v2/04-similar-media.png`
- `docs/ui-preview/rust-v2/05-review-delete.png`
- `docs/ui-preview/rust-v2/06-settings-diagnostics.png`

## 2. 设计约束

- 运行平台继续限定为 Windows x64。
- 使用 Slint 1.17.1 和内置 `fluent` 风格，不增加新的 UI 框架。
- 不兼容旧版 UI 布局，但必须兼容当前 Rust V2 的 `MainWindow` 属性和回调接口。
- 不扩大需求；概念图中没有后端数据或命令支撑的功能只能显示为 `—`、空状态或带原因的禁用状态。
- 不伪造任务速度、剩余时间、日志、删除历史、磁盘容量、版本或统计结果。
- 不为相似图片生成缩略图。成员列表显示文件信息，选中成员后才使用现有按需预览。
- 相似视频继续使用现有 JPEG 多帧联系表，按需加载到预览区域。
- 结果数据继续使用有限分页和不透明游标，不一次性加载全部组或成员。
- 删除默认进入回收站；永久删除继续使用危险色和现有显式确认门禁。
- 使用简洁代码和明确组件边界，不增加与本次界面无关的防御性逻辑。
- 新建或修改的文件、组件、属性、回调和重要布局块必须添加中文注释，说明设计目的和数据边界。

## 3. 采用方案

采用“稳定桥接契约 + 全新视觉壳 + 页面工作区”的方案。

`MainWindow` 继续作为 Rust 与 Slint 的唯一桥接入口，保留现有根属性和 21 个回调。页面不直接访问 Rust 服务，也不创建新的业务状态；它们只消费根模型、双向绑定表单字段并转发已有回调。

不采用以下方案：

- 只更换颜色：无法实现概念图的页面拆分、高密度表格和右侧详情布局。
- 同时补齐概念图全部后端能力：会引入日志、删除批次、多根扫描和新统计等未批准需求。

## 4. 应用外壳

### 4.1 窗口

- 标题：`mySingerServer · Media Dedup`。
- 首选尺寸：`1440 × 900`。
- 最小尺寸：`1080 × 700`。
- 窗口背景：`#F8FAFC`。
- 主内容区使用白色卡片和浅灰分隔线，不使用深色页面或玻璃拟态。

### 4.2 固定区域

- 左侧导航宽度：`144px`。
- 顶部命令栏高度：`58px`。
- 底部状态栏高度：`32px`。
- 内容区外边距：`20px`，允许在最小窗口下收缩到 `16px`。
- 右侧详情栏宽度：`300px`，允许在 `280–320px` 内按可用宽度调整。

### 4.3 顶部命令栏

顶部栏沿用概念图布局：菜单图标、节点范围外观、在线节点数、搜索外观和刷新入口。

- 在线节点数直接使用 `online-count`。
- 刷新按钮调用现有 `refresh()`。
- 节点范围选择器在没有真实筛选回调前显示当前范围，不发送新命令。
- 搜索框只允许对当前已经加载的行做本地文字筛选；不能表现为中心数据库全文搜索。
- `last-error` 作为右侧紧凑错误提示，不遮挡主要操作。

### 4.4 底部状态栏

底部栏展示：

- 左侧：`扫描引擎就绪` 或现有错误摘要。
- 中间：现有同步摘要 `sync-text`。
- 右侧：PostgreSQL 状态和语义色。

不显示没有数据来源的索引版本号或应用构建版本。

## 5. 视觉令牌

| 用途 | 值 |
|---|---|
| 窗口背景 | `#F8FAFC` |
| 侧栏、卡片、表格背景 | `#FFFFFF` |
| 悬停背景 | `#F1F5F9` |
| 边框 | `#E5E7EB` |
| 主文字 | `#111827` |
| 次文字 | `#6B7280` |
| 主色 | `#2563EB` |
| 主色浅背景 | `#EFF6FF` |
| 成功 | `#16A34A` |
| 警告 | `#F59E0B` |
| 危险 | `#EF4444` |

- 卡片圆角：`8px`。
- 输入框、按钮圆角：`6px`。
- 表格行高：`40px`；允许结果成员行使用 `44px`。
- 状态胶囊高度：`24px`。
- 主标题字号：`24px`；区块标题 `15–16px`；正文 `13–14px`；辅助文字 `11–12px`。
- 主操作按钮使用蓝色实底；次操作使用白底蓝框；删除和永久删除使用红色。

## 6. 导航与状态映射

侧栏固定为七个主入口：

1. 总览
2. 节点
3. 扫描
4. 任务
5. 重复文件
6. 审核删除
7. 设置

为了保持现有 Rust/Slint 接口，`current-page` 的既有数值语义不改变。新增纯 Slint 内部状态负责七个视觉入口：

| 视觉入口 | 现有 `current-page` | 内部状态 |
|---|---:|---|
| 总览 | 0 | `overview-mode = 0` |
| 节点 | 0 | `overview-mode = 1` |
| 扫描 | 1 | `task-mode = 0` |
| 任务 | 1 | `task-mode = 1` |
| 重复文件 / 精确重复 | 2 | `duplicate-tab = 0` |
| 重复文件 / 相似图片 | 3 | `duplicate-tab = 1` |
| 重复文件 / 相似视频 | 4 | `duplicate-tab = 2` |
| 重复文件 / 跨机器 | 5 | `duplicate-tab = 3` |
| 审核删除 | 6 | `review-tab` |
| 设置 | 7 | `settings-section` |

内部状态必须定义在不会因条件页面销毁而丢失的位置。切换重复类型时继续使用现有共享 `groups`、`members`、运行 ID、选择组和游标，不在 UI 中建立第二份结果缓存。

## 7. 页面设计

### 7.1 总览

总览与概念图左图一致，包含：

- 四张指标卡：在线节点、运行任务、索引摘要、同步摘要。
- 节点健康表：名称、地址、状态、Worker、任务、同步位置。
- 最近任务表：名称、节点、阶段、进度、状态。
- 底部两个概览卡：节点状态分布和重复类型入口。

数据对应：

- `online-count`、`running-count`、`indexed-text`、`sync-text`。
- `nodes` 和 `tasks` 的已加载内容。

没有可计算统计时，卡片显示 `—` 和“当前数据源未提供”，不绘制伪造饼图。

### 7.2 节点

节点页采用“主表格 + 右侧详情 + 底部添加节点”的结构。

- 主表格显示名称、地址、状态、Worker、任务、同步位置。
- 选中行后，右侧显示机器 ID、错误文本和已有运行统计。
- 底部表单继续使用 `new-node-ip` 和 `new-node-port`。
- 操作继续调用 `add-node`、`edit-node`、`remove-node`、`connect-all` 和 `sync-node`。
- 概念图中的版本、运行时长、服务明细和存储根目录没有后端字段，按对应详情区布局显示禁用说明。

### 7.3 扫描

扫描页采用“新建扫描表单 + 右侧预估信息”的结构。

- 单个扫描根继续绑定 `scan-root`。
- 节点选择绑定 `scan-node-index`。
- 枚举器选择绑定 `enumerator-index`。
- 强制重算绑定 `force-recalculate`。
- 扫描创建仍只负责枚举文件；精确重复、相似图片和相似视频类型只用于扫描完成后的本地分析，不能作为 `start-scan` 的新参数。
- 开始扫描调用 `start-scan`；路径入口调用现有 `browse-paths`。

概念图中的多根目录、排除目录、最小文件大小、扩展名和高级视频参数不表现为已生效控件。右侧预估文件数、容量和算法阶段没有数据时显示 `—`。

### 7.4 任务

任务页包含：

- `运行中 / 已完成 / 失败` 三个本地筛选标签。
- 任务主表：任务、节点、阶段、进度、状态、完成/失败/跳过计数。
- 右侧任务详情：任务 ID、阶段、状态、进度和已有计数。
- 当前可取消任务显示取消按钮并调用 `cancel-task`。
- 本地分析表单继续使用 `analysis-task-ids` 和 `analysis-kind-index`，调用 `start-local-analysis`。

速度、ETA、当前文件、开始时间、事件日志、重试和队列重排没有现有接口，显示 `—` 或禁用说明。

### 7.5 重复文件

重复文件页固定四个标签：`精确重复 / 相似图片 / 相似视频 / 跨机器`。

所有标签共用：

- 顶部筛选栏：来源、节点、运行 ID、加载结果。
- 左侧组表：类型、组 ID、代表 MD5、代表大小、成员数、可回收空间。
- 中间成员表：机器、路径、大小、代表项、在线状态、复核状态和已有算法证据。
- 右侧详情/预览：选中成员信息、按需预览和复核按钮。
- 服务端游标分页及“加载更多”。

精确重复显示 MD5 和文件大小证据。相似图片显示 PDQ、一组 9 分块 pHash 和 Sobel 二筛结果；列表不生成缩略图，选中后才加载原图预览。相似视频显示候选摘要，选中后加载 JPEG 六帧联系表。跨机器标签保留创建、轮询和重试操作，显示 `cross-status` 与 `cross-summary`。

概念图中的修改时间、盘符、全量图库缩略图、相似度时间轴、副本分布统计和导出清单没有现有字段或回调，只能占位或禁用。

### 7.6 审核删除

审核删除页分为 `审核工作台` 和 `删除中心` 两种内部模式。

审核工作台：

- 标签为 `未决定 / 保留 / 删除`，与实际复核领域状态及删除语义一致。
- 只筛选当前已经加载的成员；界面明确标注作用域为当前组。
- 继续使用 `save-review`、`quick-review`、`load-preview` 和 `prepare-delete`。
- 代表文件、保留、删除、未决定和在线门禁沿用现有模型。

删除中心：

- `待执行` 展示当前删除确认摘要并打开现有确认覆盖层。
- `执行中` 和 `历史记录` 没有持久批次模型，显示不可用空状态。
- 不使用 `last-error` 冒充删除审计日志。

现有安全链必须保持：

`prepare-delete → DeleteConfirmationChanged → confirm-delete`

`delete-can-execute` 为 `false` 时确认按钮必须禁用；永久删除必须显示明确危险提示。

### 7.7 设置

设置页使用左侧二级菜单和右侧表单卡片，分为：

- 常规
- 节点服务
- 扫描与性能
- 相似度算法
- 外部工具
- 存储
- 日志与诊断

当前真正可编辑的内容为：

- PostgreSQL URL。
- 自动重连秒数。
- 删除模式。
- PDQ、长宽比、pHash、Sobel 和视频阈值。

当前只读内容为：

- PostgreSQL 健康状态。
- data、logs、cache、config 路径。
- 当前 UI 错误摘要。

语言、主题、托盘设置、节点服务启停、扫描并发、FFmpeg 路径、日志筛选/导出/清空和运行环境版本没有现有接口，保持概念图布局但禁用，并标注“当前版本未提供”。保存继续只调用 `save-settings()`。

## 8. 组件结构

计划将 UI 组织为以下职责明确的文件：

```text
crates/desktop-ui/ui/
  app.slint
  theme.slint
  layout/
    app-shell.slint
    top-command-bar.slint
    side-navigation.slint
    status-bar.slint
  components/
    fluent-card.slint
    metric-card.slint
    tab-strip.slint
    empty-state.slint
    detail-panel.slint
    group-table.slint
    member-list.slint
    delete-dialog.slint
  pages/
    overview-dashboard.slint
    nodes-page.slint
    scan-page.slint
    task-center-page.slint
    duplicate-workspace.slint
    review-delete-workspace.slint
    settings-workspace.slint
```

职责：

- `app.slint`：保留 `MainWindow` 对外契约并完成属性、回调转发。
- `theme.slint`：保存所有浅色视觉令牌和现有行模型结构。
- `layout/*`：只负责固定外壳，不包含业务查询。
- `components/*`：保存跨页面复用的小组件和状态表达。
- `pages/*`：按七个视觉入口组织现有业务操作。

现有 `bindings.rs`、`models.rs` 和 `apps/desktop/src/main.rs` 原则上不改接口；只有编译暴露出确实需要的最小适配时才能修改，并必须由契约测试覆盖。

## 9. Rust/Slint 契约

以下根属性名称和类型必须保持：

- `nodes`、`tasks`、`groups`、`members`。
- `online-count`、`running-count`、`indexed-text`、`sync-text`。
- `filtering-enabled`、`filtering-reason`。
- `postgres-status`、`postgres-color` 和四个应用路径。
- 所有节点、扫描、分析、结果、预览、审核、删除和设置表单属性。

以下回调名称、参数顺序和整数枚举语义必须保持：

- 节点：`add-node`、`edit-node`、`remove-node`、`connect-all`、`refresh`、`sync-node`。
- 扫描任务：`browse-paths`、`start-scan`、`cancel-task`。
- 分析：`start-local-analysis`、`start-cross-analysis`、`poll-cross-analysis`、`retry-cross-analysis`。
- 结果：`load-groups`、`load-members`、`load-preview`。
- 审核删除：`save-review`、`quick-review`、`prepare-delete`、`confirm-delete`。
- 设置：`save-settings`。

整数语义继续为：

- 枚举器：`0 = Walker`，`1 = Everything`。
- 分析类型：`0 = 精确重复`，`1 = 相似图片`，`2 = 相似视频`。
- 复核决定：`0 = 未决定`，`1 = 保留`，`2 = 删除`。
- 删除模式：`0 = 回收站`，`1 = 永久删除`。

## 10. 数据流

```text
desktop-core 状态/事件
        ↓
bindings.rs 映射与回调注册
        ↓
MainWindow 根属性和根回调
        ↓
页面工作区与复用组件
```

- 状态只能从 Rust 单向进入只读行模型。
- 表单字段使用已有双向绑定。
- 用户操作只能通过现有回调回到 Rust。
- 页面组件不得直接读 SQLite、PostgreSQL、TCP 或本地文件。
- 页面切换不得清空现有结果、游标、预览和表单状态。

## 11. 状态和错误表达

所有页面统一支持：

- 空数据：白色卡片内居中说明，没有虚构示例行。
- 不可用功能：禁用控件并在邻近位置说明原因。
- 在线、运行、完成：绿色。
- 连接中、排队、警告：橙色。
- 错误、失败、永久删除：红色。
- 已取消、离线、未知：灰色。
- 当前错误：顶部栏单行截断显示，不弹出重复模态框。

不增加重试循环、自动修复或新的错误恢复策略。

## 12. 测试与验收

### 12.1 自动化契约

先建立失败测试，证明旧 UI 不满足以下约束：

- 构建风格必须为 `fluent`，不得为 `fluent-dark`。
- 主题必须使用本设计的浅色令牌。
- 七个主导航标签和四个重复文件标签必须存在。
- `MainWindow` 的根属性、21 个回调及整数语义必须保持。
- 删除确认门禁和默认回收站语义必须保持。
- 所有六张概念图对应的页面工作区均必须存在。
- UI 源文件不得出现伪造的速度、ETA、删除历史或日志行数据。

测试只验证稳定结构、契约和语义，不使用逐像素截图作为唯一门禁。

### 12.2 编译和回归

- `cargo fmt --all -- --check`
- `cargo check -p dedup-desktop-ui -p desktop --locked`
- `cargo clippy -p dedup-desktop-ui -p desktop --all-targets --locked -- -D warnings`
- `cargo test -p dedup-desktop-ui --locked`
- 与 UI 绑定相关的现有 desktop-core 定向测试。

### 12.3 Windows 视觉验收

构建并启动 Release `desktop.exe`，逐页核对：

1. 七个导航入口均可进入。
2. 顶栏、侧栏、底栏和内容区比例与概念图一致。
3. 总览、节点、扫描、任务、四类重复、审核、删除、设置、诊断布局均存在。
4. 最小窗口下没有关键按钮被遮挡。
5. 空数据、离线节点、运行任务、错误提示和禁用功能状态清晰。
6. 删除默认为回收站，永久删除具有红色警告且门禁有效。
7. 相似图片列表没有自动生成缩略图；预览只在选择后加载。
8. 相似视频预览显示 JPEG 六帧联系表。

如 GUI 自动化或桌面捕获环境不可用，只能将动态视觉验收标记为 `PARTIAL`，不得用编译通过替代运行时验收。

## 13. 完成标准

- 视觉主题、外壳比例、导航、标签、表格、详情栏和状态表达与六张概念图保持同一设计语言和布局层级。
- 七个主入口和六张概念图中的 12 个视图全部落实。
- 所有已有 Rust 属性、回调和删除安全语义通过契约测试与编译回归。
- 没有新增后端功能、协议字段、数据库迁移或媒体算法。
- 没有生成相似图片缩略图，也没有伪造概念图数据。
- 新增组件边界清晰，关键文件和功能块包含中文注释。
- 自动化门禁通过；Windows GUI 视觉验收结果按真实执行状态报告。
