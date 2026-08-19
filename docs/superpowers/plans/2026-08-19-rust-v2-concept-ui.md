# Rust V2 概念图 UI 重构实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 `desktop.exe` 的 Slint 界面重构为六张概念图定义的浅色 Fluent 高密度管理界面，覆盖七个主入口和 12 个视图，同时保持现有 Rust V2 业务接口不变。

**Architecture:** `MainWindow` 继续作为 Rust/Slint 稳定桥接契约；新增独立应用壳、复用视觉组件和七个页面工作区。所有页面只消费现有模型并转发现有回调，概念图中缺少后端支持的区域使用明确空状态或禁用说明。

**Tech Stack:** Rust 1.97.1、Slint 1.17.1、Windows x64 MSVC、Cargo 集成测试、PowerShell、Computer Use。

**Spec:** `docs/superpowers/specs/2026-08-19-rust-v2-concept-ui-design.md`

## Global Constraints

- 只在 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup` 和分支 `codex/rust-v2-media-dedup` 中工作。
- 保留未跟踪的 `crates/desktop-core/tests/physical_two_hosts_e2e.rs`，不得修改、删除或混入 UI 提交。
- 运行平台固定为 Windows x64；Slint 固定使用 1.17.1 和内置 `fluent` 风格。
- 不增加后端命令、数据库字段、TCP/Protobuf 消息、媒体计算流程或生产模拟数据。
- `MainWindow` 的现有根属性、21 个回调、参数顺序和整数枚举语义必须保持。
- 图片不生成缩略图；只在用户选择后使用现有原图预览。视频只使用已有六帧 3×2 JPG 联系表。
- 结果继续使用有限分页和不透明游标，不在 UI 中一次性物化全部结果。
- 删除默认进入回收站；永久删除继续由设置显式切换，并保留 `delete-can-execute` 门禁。
- 无后端数据的速度、ETA、日志、删除历史、磁盘容量、节点版本和统计只显示 `—`、空状态或禁用原因。
- 文件、组件、属性、回调和重要布局块使用中文注释；实现保持简洁，不添加无关兼容层或重复校验。
- 每项代码变更严格执行 RED → GREEN → REFACTOR；测试先失败后才能写生产 UI。
- 只精确暂存当前任务文件，不使用 `git add -A`、`git clean`、`git reset` 或 broad checkout。

---

## 文件结构

```text
crates/desktop-ui/
  build.rs                              # 选择 Slint fluent 构建风格
  tests/window_contract.rs              # 真实 MainWindow、导航和删除门禁行为测试
  tests/bindings_contract.rs            # 21 个回调到 UiCommand 的桥接契约测试
  tests/offscreen_layout.rs             # Slint software renderer 布局与浅色区域冒烟
  ui/app.slint                          # MainWindow 稳定契约与页面转发
  ui/theme.slint                        # 浅色令牌及现有 Ui*Row 模型
  ui/layout/app-shell.slint             # 侧栏、顶栏、内容插槽、底栏
  ui/layout/top-command-bar.slint       # 在线状态、搜索外观、刷新
  ui/layout/side-navigation.slint       # 七个主导航项
  ui/layout/status-bar.slint            # 引擎、同步、数据库状态
  ui/components/fluent-card.slint       # 通用白色卡片
  ui/components/metric-card.slint       # 总览指标卡
  ui/components/tab-strip.slint         # 页面标签栏
  ui/components/empty-state.slint       # 空状态和禁用原因
  ui/components/detail-panel.slint      # 右侧详情容器
  ui/components/group-table.slint       # 高密度重复组表
  ui/components/member-list.slint       # 高密度成员表
  ui/components/delete-dialog.slint     # 浅色危险确认覆盖层
  ui/pages/overview-dashboard.slint     # 总览视图
  ui/pages/nodes-page.slint             # 节点管理视图
  ui/pages/scan-page.slint              # 新建扫描视图
  ui/pages/task-center-page.slint       # 任务中心视图
  ui/pages/duplicate-workspace.slint    # 四类重复结果视图
  ui/pages/review-delete-workspace.slint# 审核与删除中心
  ui/pages/settings-workspace.slint     # 设置与日志诊断
AGENTS.md                                # 已落地 UI 架构和维护边界
docs/verification/2026-08-19-rust-v2-concept-ui.md
docs/verification/rust-v2-concept-ui/*.png
```

删除旧页面文件只发生在新页面已经编译通过后：

- `ui/pages/overview.slint`
- `ui/pages/scan-tasks.slint`
- `ui/pages/exact-cross-machine.slint`
- `ui/pages/similar-media.slint`
- `ui/pages/review-delete.slint`
- `ui/pages/settings-diagnostics.slint`
- `ui/components/navigation.slint`

## 多 Agent 执行策略

- 用户允许最多 20 个子 Agent；执行仍遵循 SDD 的单实现者写入规则，不同时派发两个实现 Agent 修改共享工作树。
- 共享工作树中的写入任务按 Task 1–8 依赖顺序执行；每个任务使用独立实现 Agent 和独立审查 Agent，避免两个 Agent 同时修改 `app.slint` 或测试文件。
- Agent 只精确提交本任务文件；主线程负责审查包、回归门禁和跨任务接口一致性。

---

### Task 1: 建立行为契约测试并切换浅色 Fluent 主题

**Files:**
- Modify: `crates/desktop-ui/Cargo.toml`
- Create: `crates/desktop-ui/tests/window_contract.rs`
- Create: `crates/desktop-ui/tests/bindings_contract.rs`
- Create: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/build.rs`
- Modify: `crates/desktop-ui/ui/theme.slint`
- Modify: `crates/desktop-ui/src/bindings.rs`

**Interfaces:**
- Consumes: 真实生成的 `MainWindow` API、`bind_commands()` 和 `UiCommand`。
- Produces: 精确锁定的 `i-slint-backend-testing = 1.17.1` 测试后端；根窗口默认值行为测试；21 个桥接回调的命令测试；软件渲染尺寸/不透明度冒烟；固定浅色令牌。

- [ ] **Step 1: 添加精确版本测试后端并编写真实行为 RED**

在 `Cargo.toml` 增加：

```toml
[dev-dependencies]
i-slint-backend-testing = { version = "=1.17.1", features = ["renderer-software"] }
```

`window_contract.rs` 必须在创建窗口之前调用 `i_slint_backend_testing::init_no_event_loop()`，然后真实构造 `MainWindow`，断言 `current-page = 0`、默认节点地址/端口、扫描根、枚举器、`delete-mode = "回收站"`。测试随后调用现有 `set_*`/`get_*` 和一个现有 `invoke_*`，证明生成 API 可用，不读取 `.slint` 源文件。

`bindings_contract.rs` 使用真实 `tokio::sync::mpsc` channel 调用 `bind_commands()`。先覆盖 `start-scan` 的参数顺序、负节点索引归零、枚举器 `1 -> Everything`/其他值 `-> WindowsWalker`；再按同样方式覆盖全部 21 个现有根回调。配置非法时断言只更新 `last-error` 且 channel 不产生 `UiCommand`。这是外部命令边界，不使用 mock 结构。

`offscreen_layout.rs` 先写会失败的浅色背景测试：创建带 `renderer-software` 的 `TestingBackend`，构造并显示 `MainWindow`，设置 `1440×900`，取得 RGBA8 snapshot，断言尺寸、像素数、非透明像素占比，以及左上/内容区的浅色亮度范围。禁止使用整图哈希或逐像素 golden。

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
cargo test -p dedup-desktop-ui --test offscreen_layout light_shell_renders_at_target_size -- --exact
```

Expected: 浅色区域断言失败，因为当前构建仍使用 `fluent-dark`；其余窗口/桥接测试先建立现有基线。

- [ ] **Step 3: 切换构建风格并重写浅色令牌**

将 `build.rs` 改为：

```rust
//! 编译管理端唯一 Slint 入口，并固定使用概念图定义的浅色 Fluent 控件风格。

fn main() {
    let configuration = slint_build::CompilerConfiguration::new().with_style("fluent".into());
    slint_build::compile_with_config("ui/app.slint", configuration)
        .expect("编译 desktop Slint 界面失败");
}
```

保持四个 `Ui*Row` 结构字段原样，将 `Theme` 更新为：

```slint
export global Theme {
    in-out property <color> window: #f8fafc;
    in-out property <color> sidebar: #ffffff;
    in-out property <color> panel: #ffffff;
    in-out property <color> panel-hover: #f1f5f9;
    in-out property <color> border: #e5e7eb;
    in-out property <color> text: #111827;
    in-out property <color> muted: #6b7280;
    in-out property <color> accent: #2563eb;
    in-out property <color> accent-soft: #eff6ff;
    in-out property <color> success: #16a34a;
    in-out property <color> warning: #f59e0b;
    in-out property <color> danger: #ef4444;
}
```

- [ ] **Step 4: 运行 GREEN 和格式门禁**

Run:

```powershell
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
cargo fmt --all -- --check
```

Expected: 真实生成 API、21 个命令桥接、软件渲染浅色冒烟和格式门禁全部通过。修正 `bindings.rs` 中过时的“17 回调”注释为“21 回调”。

- [ ] **Step 5: 精确提交 Task 1**

```powershell
git add -- crates/desktop-ui/Cargo.toml Cargo.lock crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/bindings_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/build.rs crates/desktop-ui/ui/theme.slint crates/desktop-ui/src/bindings.rs
git commit -m "test: establish Rust V2 UI behavior contracts"
```

---

### Task 2: 实现概念图应用壳、七项导航和复用基础组件

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Create: `crates/desktop-ui/ui/layout/app-shell.slint`
- Create: `crates/desktop-ui/ui/layout/top-command-bar.slint`
- Create: `crates/desktop-ui/ui/layout/side-navigation.slint`
- Create: `crates/desktop-ui/ui/layout/status-bar.slint`
- Create: `crates/desktop-ui/ui/components/fluent-card.slint`
- Create: `crates/desktop-ui/ui/components/metric-card.slint`
- Create: `crates/desktop-ui/ui/components/tab-strip.slint`
- Create: `crates/desktop-ui/ui/components/empty-state.slint`
- Create: `crates/desktop-ui/ui/components/detail-panel.slint`
- Modify: `crates/desktop-ui/ui/app.slint`

**Interfaces:**
- Consumes: Task 1 `Theme` 令牌和原 `MainWindow` 根属性/回调。
- Produces: `AppShell.active-nav`、`AppShell.navigate(int)`、`AppShell.refresh()`；`@children` 内容插槽；`TabStrip.active-index` 与 `TabStrip.changed(int)`；七项视觉导航；仅供 UI 状态机使用的根 `navigate-to(int)` 回调。

- [ ] **Step 1: 添加导航状态机和离屏布局 RED**

在 `window_contract.rs` 添加单个串行行为测试，真实构造窗口后调用将被侧栏共用的 `invoke_navigate_to()`，逐项断言：

```rust
let expected = [
    (0, 0, 0, 0), // 总览
    (0, 1, 1, 0), // 节点
    (1, 2, 1, 0), // 扫描
    (1, 3, 1, 1), // 任务
    (2, 4, 1, 1), // 重复文件
    (6, 5, 1, 1), // 审核删除
    (7, 6, 1, 1), // 设置
];
```

每个元组顺序为 `(current_page, active_nav, overview_mode, task_mode)`。先将 `current-page` 设置为 `3/4/5` 再导航到“重复文件”，断言不会重置已选重复类型。给七个侧栏按钮和刷新按钮增加稳定的中文 `accessible-label`；用 `ElementHandle::find_by_accessible_label()` 触发至少“节点”和“刷新”的默认动作，断言前者走同一映射、后者只触发一次现有 `refresh` 回调。

在 `offscreen_layout.rs` 增加区域冒烟：1440×900 时侧栏、顶栏、内容区、底栏都非透明且明度符合浅色主题；把窗口缩到 `1080×700` 后仍能渲染且关键导航按钮处于窗口边界内。只断言区域关系和可访问元素边界，不断言字体像素或整图哈希。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cargo test -p dedup-desktop-ui --test window_contract navigation_actions_preserve_page_mapping -- --exact --test-threads=1`

Expected: 编译 RED，因为根窗口尚未提供 `navigate-to` 生成 API；离屏布局测试随后因旧壳区域关系不匹配而失败。

- [ ] **Step 3: 创建固定应用壳**

`AppShell` 必须使用以下接口和层级：

```slint
export component AppShell inherits Rectangle {
    in-out property <int> active-nav: 0;
    in property <int> online-count;
    in property <string> sync-text;
    in property <string> postgres-status;
    in property <color> postgres-color;
    in property <string> last-error;
    callback navigate(int);
    callback refresh();
    background: Theme.window;

    HorizontalLayout {
        spacing: 0px;
        SideNavigation { width: 144px; active-index: root.active-nav; navigate(index) => { root.navigate(index); } }
        VerticalLayout {
            spacing: 0px;
            TopCommandBar { height: 58px; online-count: root.online-count; last-error: root.last-error; refresh => { root.refresh(); } }
            Rectangle { background: Theme.window; horizontal-stretch: 1; vertical-stretch: 1; clip: true; @children }
            StatusBar { height: 32px; sync-text: root.sync-text; postgres-status: root.postgres-status; postgres-color: root.postgres-color; }
        }
    }
}
```

`SideNavigation` 固定七个 `NavItem`，索引严格为 0–6。`TopCommandBar` 显示节点范围外观、在线数、本地搜索输入框和刷新按钮；搜索框不触发业务回调。`StatusBar` 只显示引擎就绪、同步摘要和 PostgreSQL 状态。

- [ ] **Step 4: 创建五个复用组件**

- `FluentCard`：白底、`8px` 圆角、`1px` 边框并提供 `@children`。
- `MetricCard`：`title`、`value`、`detail`、`tone` 四个输入属性。
- `TabStrip`：`labels: [string]`、`active-index: int`、`changed(int)`；选中项使用蓝色文字和 `2px` 下划线。
- `EmptyState`：`title`、`detail`、`disabled-feature`，不创建示例数据。
- `DetailPanel`：固定 `300px` 宽的白色详情容器并提供 `@children`。

组件公开接口固定为：

```slint
export component FluentCard inherits Rectangle { @children }
export component MetricCard inherits Rectangle {
    in property <string> title;
    in property <string> value;
    in property <string> detail;
    in property <color> tone: Theme.accent;
}
export component TabStrip inherits Rectangle {
    in property <[string]> labels;
    in-out property <int> active-index;
    callback changed(int);
}
export component EmptyState inherits Rectangle {
    in property <string> title;
    in property <string> detail;
    in property <bool> disabled-feature: false;
}
export component DetailPanel inherits Rectangle { width: 300px; @children }
```

- [ ] **Step 5: 在 MainWindow 中接入 AppShell 与导航适配**

新增仅限 Slint 的根状态：

```slint
in-out property <int> active-nav: 0;
in-out property <int> overview-mode: 0;
in-out property <int> task-mode: 0;
in-out property <int> review-tab: 0;
in-out property <int> settings-section: 0;
callback navigate-to(int);
```

`navigate-to(index)` 是侧栏和行为测试共同使用的唯一导航状态机；`AppShell.navigate(index)` 只转发到它。映射必须为：

```slint
if index == 0 { root.current-page = 0; root.overview-mode = 0; }
else if index == 1 { root.current-page = 0; root.overview-mode = 1; }
else if index == 2 { root.current-page = 1; root.task-mode = 0; }
else if index == 3 { root.current-page = 1; root.task-mode = 1; }
else if index == 4 { if root.current-page < 2 || root.current-page > 5 { root.current-page = 2; } }
else if index == 5 { root.current-page = 6; }
else { root.current-page = 7; }
root.active-nav = index;
```

这一任务仍可临时渲染旧页面组件；不得删除旧页面文件。

- [ ] **Step 6: 运行 GREEN、编译和 Clippy**

```powershell
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
cargo clippy -p dedup-desktop-ui -p desktop --all-targets --locked -- -D warnings
```

Expected: 导航真实动作、离屏区域关系、Slint 编译和 Clippy 全部通过。

- [ ] **Step 7: 精确提交 Task 2**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/layout crates/desktop-ui/ui/components/fluent-card.slint crates/desktop-ui/ui/components/metric-card.slint crates/desktop-ui/ui/components/tab-strip.slint crates/desktop-ui/ui/components/empty-state.slint crates/desktop-ui/ui/components/detail-panel.slint
git commit -m "feat: add Rust V2 concept application shell"
```

---

### Task 3: 拆分总览和节点管理视图

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Create: `crates/desktop-ui/ui/pages/overview-dashboard.slint`
- Create: `crates/desktop-ui/ui/pages/nodes-page.slint`
- Modify: `crates/desktop-ui/ui/app.slint`

**Interfaces:**
- Consumes: `nodes`、`tasks`、`online-count`、`running-count`、`indexed-text`、`sync-text`，以及现有六个节点回调。
- Produces: `OverviewDashboard` 和 `NodesPage`；`overview-mode` 选择总览或节点视图。

- [ ] **Step 1: 添加真实模型消费和节点操作 RED**

在 `window_contract.rs` 使用 `VecModel<UiNodeRow>` 和 `VecModel<UiTaskRow>` 注入两个节点、三种任务状态的字面 fixture。导航到总览后，通过可访问标签断言指标卡和对应节点/任务行可见；导航到节点页后断言同一节点模型仍可见且选中项可以改变。

新增 `node_add_forwards_entered_ip_and_port`：设置根 `new-node-ip`/`new-node-port`，为现有 `add-node` 安装捕获闭包，再通过“添加节点”按钮的 `accessible-label` 触发默认动作，断言参数原样转发。编辑、同步、移除、连接按钮分别捕获现有回调的节点索引或调用次数，不读取控件源码，也不测试“机器 ID/当前版本未提供”等静态文案。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cargo test -p dedup-desktop-ui --test window_contract overview_and_nodes_consume_real_models -- --exact --test-threads=1`

Expected: RED，因为新总览/节点组件及其可访问操作尚不存在。

- [ ] **Step 3: 实现 OverviewDashboard**

页面使用 `ScrollView`，内容边距 `20px`。顶部四张 `MetricCard` 分别显示在线节点、运行任务、索引摘要和同步摘要。节点健康表以 `nodes` 展示名称、地址、状态、Worker、任务和同步位置；最近任务表以 `tasks` 展示标题、节点索引、阶段、进度和状态。底部两个 `FluentCard` 保留概念图位置，但不可用统计显示 `EmptyState { title: "统计暂不可用"; detail: "当前数据源未提供容量与重复类型汇总"; }`。

```slint
export component OverviewDashboard inherits Rectangle {
    in property <[UiNodeRow]> nodes;
    in property <[UiTaskRow]> tasks;
    in property <int> online-count;
    in property <int> running-count;
    in property <string> indexed-text;
    in property <string> sync-text;
}
```

- [ ] **Step 4: 实现 NodesPage**

页面采用 `HorizontalLayout`：左侧节点表和底部添加表单，右侧 `DetailPanel`。节点行点击后更新内部 `selected-node-index`；详情只显示 `UiNodeRow` 已有字段。版本、运行时长、服务明细和存储目录使用 `EmptyState` 禁用说明。添加、编辑、同步、移除和连接全部只转发现有回调。

```slint
export component NodesPage inherits Rectangle {
    in property <[UiNodeRow]> nodes;
    in-out property <string> new-node-ip;
    in-out property <int> new-node-port;
    in-out property <int> selected-node-index: 0;
    callback connect-all();
    callback add-node(string, int);
    callback edit-node(int, string, int);
    callback sync-node(int);
    callback remove-node(int);
}
```

- [ ] **Step 5: 接入 app.slint**

当 `current-page == 0 && overview-mode == 0` 渲染 `OverviewDashboard`；当 `current-page == 0 && overview-mode == 1` 渲染 `NodesPage`。保留所有节点表单双向绑定和回调参数。

```slint
if root.current-page == 0 && root.overview-mode == 0 : OverviewDashboard { /* 只读模型绑定 */ }
if root.current-page == 0 && root.overview-mode == 1 : NodesPage { /* 节点表单与六个回调转发 */ }
```

- [ ] **Step 6: 运行 GREEN 和定向回归**

```powershell
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
cargo test -p dedup-desktop-core --locked view_state
```

- [ ] **Step 7: 精确提交 Task 3**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/pages/overview-dashboard.slint crates/desktop-ui/ui/pages/nodes-page.slint
git commit -m "feat: split overview and node management workspaces"
```

---

### Task 4: 拆分新建扫描和任务中心视图

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Create: `crates/desktop-ui/ui/pages/scan-page.slint`
- Create: `crates/desktop-ui/ui/pages/task-center-page.slint`
- Modify: `crates/desktop-ui/ui/app.slint`

**Interfaces:**
- Consumes: `tasks`、筛选门禁、扫描表单、分析表单，以及 `browse-paths`、`start-scan`、`start-local-analysis`、`cancel-task`。
- Produces: `ScanPage` 和 `TaskCenterPage`；`task-mode` 选择扫描创建或任务列表；任务标签只筛选已加载模型。

- [ ] **Step 1: 添加扫描参数和任务筛选 RED**

在 `window_contract.rs` 添加 `scan_start_forwards_four_arguments_in_order`：设置节点、路径、强制重算和 Everything 后通过“开始扫描”的可访问默认动作触发现有 `start-scan`，断言得到 `(7, "D:\\fixture", true, 1)`；同时把分析类型设为非默认值，证明它没有混入扫描参数。再测试浏览和本地分析按钮分别只转发原有参数。

添加 `task_tabs_filter_loaded_models_and_cancel_active_task`：注入运行中、完成、失败各一条 `UiTaskRow`，通过三个标签的默认动作切换根 `task-tab`，断言可访问树中只出现对应任务行；运行中任务的取消按钮触发一次 `(node_index, task_id)`，完成/失败行无可执行取消动作。速度、ETA 和日志没有模型字段，因此只在 Windows GUI 验收检查禁用呈现，不写负向源码测试。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cargo test -p dedup-desktop-ui --test window_contract scan_start_forwards_four_arguments_in_order -- --exact --test-threads=1`

Expected: RED，因为“开始扫描”可访问动作尚未接入新扫描页。

- [ ] **Step 3: 实现 ScanPage**

左侧按概念图分区显示一个扫描根、节点索引、枚举器、强制重算和开始按钮；右侧显示当前根、节点和算法流程。多根目录、排除目录、最小大小、后缀和高级视频选项使用禁用控件或 `EmptyState`，且旁注“当前版本未提供”。本地分析区域明确位于扫描创建之后，绑定 `analysis-task-ids` 和 `analysis-kind-index`。

```slint
export component ScanPage inherits Rectangle {
    in property <bool> filtering-enabled;
    in property <string> filtering-reason;
    in-out property <string> scan-root;
    in-out property <int> selected-node;
    in-out property <int> enumerator-index;
    in-out property <bool> force-recalculate;
    in-out property <string> analysis-task-ids;
    in-out property <int> analysis-kind-index;
    callback browse(int, string);
    callback start-scan(int, string, bool, int);
    callback start-analysis(int, string, int);
}
```

- [ ] **Step 4: 实现 TaskCenterPage**

`task-tab` 为 0/1/2，对应运行中、已完成、失败。每行显示任务标题、节点、阶段、进度条、计数和状态；仅排队中/运行中显示可用取消按钮。右侧详情显示选中任务的已有字段；速度、剩余时间、当前文件、事件日志和重试显示 `—` 或禁用说明，不创建硬编码示例。

```slint
export component TaskCenterPage inherits Rectangle {
    in property <[UiTaskRow]> tasks;
    in-out property <int> task-tab: 0;
    in-out property <int> selected-task-index: 0;
    callback cancel-task(int, string);
}
```

根窗口新增 `in-out property <int> task-tab: 0` 并与组件双向绑定，使标签状态可由 Rust 行为测试和后续状态恢复观察；它不改变任何后端命令。

- [ ] **Step 5: 接入 app.slint 并保持回调语义**

当 `current-page == 1 && task-mode == 0` 渲染 `ScanPage`；当 `current-page == 1 && task-mode == 1` 渲染 `TaskCenterPage`。`start-scan` 仍严格传入 `(node_index, path, force, enumerator)`，分析类型不得追加到扫描参数。

```slint
if root.current-page == 1 && root.task-mode == 0 : ScanPage { /* 扫描与分析表单双向绑定 */ }
if root.current-page == 1 && root.task-mode == 1 : TaskCenterPage { /* tasks 与 cancel-task 转发 */ }
```

- [ ] **Step 6: 运行 GREEN 和回归**

```powershell
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
cargo test -p dedup-desktop-core --locked task
```

- [ ] **Step 7: 精确提交 Task 4**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/pages/scan-page.slint crates/desktop-ui/ui/pages/task-center-page.slint
git commit -m "feat: split scan and task center workspaces"
```

---

### Task 5: 统一四类重复结果工作区并改造高密度表格

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/ui/components/group-table.slint`
- Modify: `crates/desktop-ui/ui/components/member-list.slint`
- Create: `crates/desktop-ui/ui/pages/duplicate-workspace.slint`
- Modify: `crates/desktop-ui/ui/app.slint`

**Interfaces:**
- Consumes: `groups`、`members`、两个游标、结果来源/节点/运行 ID、预览、跨机器状态，以及现有结果/复核/跨机器回调。
- Produces: `DuplicateWorkspace.duplicate-tab` 0–3；四标签统一结果查询；高密度组表、成员表和 `300px` 详情面板。

- [ ] **Step 1: 添加结果状态、分页和按需预览行为 RED**

在 `window_contract.rs` 新增 `duplicate_tabs_preserve_loaded_state_and_forward_existing_callbacks`。使用字面 `VecModel<UiGroupRow>` 和 `VecModel<UiMemberRow>` 注入一组两成员数据，同时设置 `group-next-cursor`、`member-next-cursor`、`result-run-id` 和 `selected-group-id`。依次通过“精确重复 / 相似图片 / 相似视频 / 跨机器”的 `accessible-label` 触发默认动作，断言 `current-page` 精确变为 2–5，并且模型、两个不透明游标、运行 ID 和已选组都未被清空。

为现有 `load-groups`、`load-members`、`load-preview`、`save-review`、`start-cross-analysis`、`poll-cross-analysis` 和 `retry-cross-analysis` 安装捕获闭包。通过加载结果、选择组、成员预览/复核和跨机器操作的真实可访问动作触发，断言参数顺序和调用次数保持。创建窗口和注入模型后先断言预览回调计数为 0，再点击成员预览并断言只增加为 1，证明图片和视频都没有进入即加载路径。

在 `offscreen_layout.rs` 新增四标签布局冒烟：1440×900 下依次切换 2–5 页，断言组表、成员表和详情面板的可访问边界按从左到右排列且都在内容区内。图片和视频的具体预览文案留给 Task 8 的真实 GUI 验收，不写源码负向断言。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cargo test -p dedup-desktop-ui --test window_contract duplicate_tabs_preserve_loaded_state_and_forward_existing_callbacks -- --exact --test-threads=1`

Expected: RED，因为四个结果标签和统一工作区的可访问动作尚不存在。

- [ ] **Step 3: 将 GroupTable 改为高密度列式组表**

保留属性和回调名称。新增固定表头：类型、组 ID/MD5、代表大小、成员、可回收空间。行高 `44px`，选中行使用 `Theme.accent-soft`。空模型使用 `EmptyState`。`has-more` 只控制加载下一页按钮，不预取全部数据。

```slint
export component GroupTable inherits Rectangle {
    in property <[UiGroupRow]> groups;
    in property <string> selected-id;
    in property <bool> has-more;
    callback select-group(string);
    callback load-more();
}
```

- [ ] **Step 4: 将 MemberList 改为高密度成员表**

保留 `members`、`preview`、`review` 接口。每行显示代表/在线、路径、机器、大小/元数据、Stage1、pHash、Stage2、复核状态及三个动作。不得创建图片缩略图、缓存或新的数据模型。

```slint
export component MemberList inherits Rectangle {
    in property <[UiMemberRow]> members;
    callback preview(string, string);
    callback review(string, string, int);
}
```

- [ ] **Step 5: 实现 DuplicateWorkspace**

固定四标签与 `current-page` 映射：

```slint
export component DuplicateWorkspace inherits Rectangle {
    in-out property <int> duplicate-tab;
    in property <[UiGroupRow]> groups;
    in property <[UiMemberRow]> members;
    in property <string> group-next-cursor;
    in property <string> member-next-cursor;
    in property <image> preview-image;
    in property <string> preview-info;
    in-out property <int> source-index;
    in-out property <int> node-index;
    in-out property <string> run-id;
    in-out property <string> selected-group-id;
    in-out property <string> cross-selections;
    in property <string> cross-status;
    in property <string> cross-summary;
    callback select-tab(int);
    callback load-groups(bool, int, string, int, string);
    callback load-members(bool, int, string, string, int, string);
    callback save-review(string, string, int);
    callback preview(string, string);
    callback start-cross(string);
    callback poll-cross();
    callback retry-cross();
}
```

查询类型映射：精确 `kind = 0`，图片 `kind = 1`，视频 `kind = 2`；跨机器的结果类型仍由现有加载控件选择，不创建第四种 `GroupKind`。普通标签显示来源、节点、运行 ID；跨机器标签显示 selections、创建、轮询、partial 重试和摘要。页面主体固定为组表、成员表和 `DetailPanel`。图片只在 `preview()` 后显示原图；视频只显示现有联系表。

- [ ] **Step 6: 在 app.slint 合并 current-page 2–5**

使用单个条件 `current-page >= 2 && current-page <= 5` 创建 `DuplicateWorkspace`，转发所有原有属性和回调。切换标签只改变 `current-page`，不得清空 `groups`、`members`、游标、运行 ID或选中组。

```slint
if root.current-page >= 2 && root.current-page <= 5 : DuplicateWorkspace {
    duplicate-tab: root.current-page - 2;
    select-tab(tab) => { root.current-page = 2 + tab; }
}
```

- [ ] **Step 7: 运行 GREEN、UI 编译和结果回归**

```powershell
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
cargo test -p dedup-desktop-core --locked results
cargo test -p dedup-desktop-core --locked review
```

- [ ] **Step 8: 精确提交 Task 5**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/components/group-table.slint crates/desktop-ui/ui/components/member-list.slint crates/desktop-ui/ui/pages/duplicate-workspace.slint
git commit -m "feat: unify concept duplicate result workspace"
```

---

### Task 6: 实现审核工作台和删除中心

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/ui/components/delete-dialog.slint`
- Create: `crates/desktop-ui/ui/pages/review-delete-workspace.slint`
- Modify: `crates/desktop-ui/ui/app.slint`

**Interfaces:**
- Consumes: 当前成员、选中组、预览、路径规则、删除确认属性和现有审核删除回调。
- Produces: 审核/删除双模式；待审核/已决定/已忽略本地过滤；待执行/执行中/历史记录删除标签；浅色删除覆盖层。

- [ ] **Step 1: 添加审核筛选和删除门禁行为 RED**

在 `window_contract.rs` 新增 `review_filters_loaded_members_and_delete_confirmation_obeys_gate`。注入未决定、保留、删除三种 `UiMemberRow.review` 的字面 fixture，导航到审核删除页后，通过“待审核 / 已决定 / 已忽略”可访问动作切换根 `review-filter`，断言可访问树只暴露当前筛选的成员；通过“审核工作台 / 删除中心”和“待执行 / 执行中 / 历史记录”动作断言根 `review-tab`、`delete-filter` 的状态映射。

测试为 `prepare-delete` 和 `confirm-delete` 安装捕获闭包：点击“准备删除”只调用一次现有准备回调；打开根删除覆盖层并令 `delete-can-execute = false` 时，“确认执行”的默认动作不得调用确认回调；改为 `true` 后同一动作只调用一次。分别设置 `delete-mode = "回收站"` 和 `"永久删除"`，断言可访问名称/描述反映当前模式，永久删除描述包含“不可恢复”。

在 `offscreen_layout.rs` 增加浅色根级覆盖层冒烟，断言确认卡片位于窗口中央并覆盖内容区，但不使用像素哈希。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cargo test -p dedup-desktop-ui --test window_contract review_filters_loaded_members_and_delete_confirmation_obeys_gate -- --exact --test-threads=1`

Expected: RED，因为新审核/删除状态动作和可观察筛选状态尚不存在。

- [ ] **Step 3: 实现 ReviewDeleteWorkspace**

`review-tab` 选择审核工作台或删除中心。审核标签只按当前 `members` 的 `review` 值筛选并明确“当前组范围”；快捷规则继续传 0–3。删除中心的待执行只调用 `prepare-delete`；执行中和历史记录使用 `EmptyState`，文案固定为“当前版本没有持久删除批次”。

根窗口新增 `in-out property <int> review-filter: 0` 和 `in-out property <int> delete-filter: 0`，与工作区双向绑定，供真实行为测试和后续状态恢复观察；不得映射到新的后端命令。

```slint
export component ReviewDeleteWorkspace inherits Rectangle {
    in property <[UiMemberRow]> members;
    in property <string> selected-group-id;
    in property <image> preview-image;
    in property <string> preview-info;
    in-out property <string> path-rule;
    in-out property <int> review-tab: 0;
    in-out property <int> review-filter: 0;
    in-out property <int> delete-filter: 0;
    callback save-review(string, string, int);
    callback preview(string, string);
    callback quick-review(int, string);
    callback prepare-delete();
}
```

- [ ] **Step 4: 重绘 DeleteDialog**

改为浅色覆盖层和 `520×320` 白色卡片；永久删除时边框、标题和警告使用 `Theme.danger`，警告文本包含“不可恢复”。保留 `can-execute` 按钮门禁、`cancel()` 和 `confirm()`，不增加二次字符串输入。

```slint
Button {
    text: "确认执行";
    enabled: root.can-execute;
    clicked => { root.confirm(); }
}
```

- [ ] **Step 5: 接入 app.slint**

`current-page == 6` 渲染新工作区。继续转发 `save-review`、`load-preview`、`quick-review` 和 `prepare-delete`。根删除覆盖层继续位于 AppShell 外层，确保覆盖所有页面。

```slint
if root.current-page == 6 : ReviewDeleteWorkspace {
    review-tab <=> root.review-tab;
    /* 现有审核与准备删除回调转发 */
}
if root.delete-dialog-open : DeleteDialog { /* 保持根级覆盖层 */ }
```

- [ ] **Step 6: 运行 GREEN 和删除链回归**

```powershell
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
cargo test -p dedup-desktop-core --locked delete
```

- [ ] **Step 7: 精确提交 Task 6**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/components/delete-dialog.slint crates/desktop-ui/ui/pages/review-delete-workspace.slint
git commit -m "feat: add concept review and delete workspaces"
```

---

### Task 7: 实现设置与日志诊断并更新 AGENTS 架构说明

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Create: `crates/desktop-ui/ui/pages/settings-workspace.slint`
- Modify: `crates/desktop-ui/ui/app.slint`
- Delete: `crates/desktop-ui/ui/pages/overview.slint`
- Delete: `crates/desktop-ui/ui/pages/scan-tasks.slint`
- Delete: `crates/desktop-ui/ui/pages/exact-cross-machine.slint`
- Delete: `crates/desktop-ui/ui/pages/similar-media.slint`
- Delete: `crates/desktop-ui/ui/pages/review-delete.slint`
- Delete: `crates/desktop-ui/ui/pages/settings-diagnostics.slint`
- Delete: `crates/desktop-ui/ui/components/navigation.slint`
- Modify: `AGENTS.md`

**Interfaces:**
- Consumes: PostgreSQL 状态、四个路径、所有设置字段和 `save-settings()`。
- Produces: 七项设置二级菜单；常规/算法/存储真实表单；节点服务、扫描性能、外部工具、日志诊断禁用说明；最终新 UI 文件拓扑。

- [ ] **Step 1: 添加设置状态和保存行为 RED**

在 `window_contract.rs` 新增 `settings_sections_preserve_real_values_and_save_once`。导航到设置页后依次通过七个二级菜单的中文 `accessible-label` 触发默认动作，断言根 `settings-section` 精确变为 0–6。通过生成 API 设置 PostgreSQL URL、重连秒数、删除模式索引和九个阈值，再跨常规、相似度算法、存储、日志与诊断往返，断言所有真实值保持不变。

为现有 `save-settings` 安装捕获闭包，通过“保存设置”可访问动作触发并断言只调用一次。将四个应用路径、PostgreSQL 状态和 `last-error` 设置为字面 fixture，在存储/诊断页通过可访问文本确认这些真实值可见；禁用项目只要求存在 `accessible-disabled = true` 的代表性控件，不断言源码文案或旧文件名。

在 `offscreen_layout.rs` 增加设置页冒烟，断言七项二级菜单边界纵向排列、右侧内容卡在其右侧，最小窗口 `1080×700` 下“保存设置”仍处于窗口边界内。旧页面删除由新页面编译和完整 UI 回归证明，不为文件名写变更检测器。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cargo test -p dedup-desktop-ui --test window_contract settings_sections_preserve_real_values_and_save_once -- --exact --test-threads=1`

Expected: RED，因为七项设置菜单和保存可访问动作尚未接入新工作区。

- [ ] **Step 3: 实现 SettingsWorkspace**

左侧二级菜单固定七项，右侧只在对应 section 显示内容：

- 常规：PostgreSQL URL、重连秒数、删除模式。
- 相似度算法：九个阈值和“分析创建时快照”说明。
- 存储：data、config、logs、cache 四个只读路径。
- 节点服务、扫描与性能、外部工具：显示概念图布局和“当前版本未提供”，所有控件禁用。
- 日志与诊断：显示 PostgreSQL 健康、路径和 `last-error`；筛选、导出、清空和环境版本禁用。

继续保留 `AboutSlint` 归属入口和可信局域网明文提示。

```slint
export component SettingsWorkspace inherits Rectangle {
    in-out property <int> active-section: 0;
    in property <string> data-path;
    in property <string> logs-path;
    in property <string> cache-path;
    in property <string> config-path;
    in property <string> postgres-status;
    in property <color> postgres-color;
    in property <string> last-error;
    in-out property <string> postgres-url;
    in-out property <int> reconnect-seconds;
    in-out property <int> delete-mode-index;
    in-out property <string> pdq-quality;
    in-out property <string> aspect-tolerance;
    in-out property <string> pdq-hamming;
    in-out property <string> phash-hamming;
    in-out property <string> phash-parts;
    in-out property <string> sobel-min;
    in-out property <string> video-valid;
    in-out property <string> video-stage1;
    in-out property <string> video-stage2;
    callback save-settings();
}
```

- [ ] **Step 4: 接入 app.slint 并删除旧 UI 文件**

`current-page == 7` 渲染 `SettingsWorkspace`。确认 `cargo check` 已能使用全部新页面后，再删除七个旧页面/导航文件。不得删除仍被新页面复用的 `status-pill.slint`、`score-panel.slint`、`group-table.slint`、`member-list.slint` 或 `delete-dialog.slint`。

```slint
if root.current-page == 7 : SettingsWorkspace {
    active-section <=> root.settings-section;
    save-settings => { root.save-settings(); }
}
```

- [ ] **Step 5: 更新 AGENTS.md 的 UI 架构段落**

将旧“fluent-dark、八个导航入口”描述替换为已落地结构：浅色 `fluent`、七个主导航、四类重复标签、审核删除双模式、设置二级菜单、有限分页、按需预览、缺失能力禁用状态、`MainWindow` 21 回调不变。写明图片不生成缩略图、视频使用六帧联系表，以及页面不得直接访问 TCP/数据库/FFmpeg。

- [ ] **Step 6: 运行 GREEN、文档和全 UI 回归**

```powershell
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --locked
cargo check -p dedup-desktop-ui -p desktop --locked
cargo clippy -p dedup-desktop-ui -p desktop --all-targets --locked -- -D warnings
git diff --check
```

- [ ] **Step 7: 精确提交 Task 7**

```powershell
git add -- AGENTS.md crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/pages/settings-workspace.slint crates/desktop-ui/ui/pages/overview.slint crates/desktop-ui/ui/pages/scan-tasks.slint crates/desktop-ui/ui/pages/exact-cross-machine.slint crates/desktop-ui/ui/pages/similar-media.slint crates/desktop-ui/ui/pages/review-delete.slint crates/desktop-ui/ui/pages/settings-diagnostics.slint crates/desktop-ui/ui/components/navigation.slint
git commit -m "feat: complete Rust V2 concept UI architecture"
```

---

### Task 8: 完整门禁、Windows GUI 逐页截图和验收记录

**Files:**
- Create: `docs/verification/2026-08-19-rust-v2-concept-ui.md`
- Create: `docs/verification/rust-v2-concept-ui/01-overview.png`
- Create: `docs/verification/rust-v2-concept-ui/02-nodes.png`
- Create: `docs/verification/rust-v2-concept-ui/03-scan.png`
- Create: `docs/verification/rust-v2-concept-ui/04-tasks.png`
- Create: `docs/verification/rust-v2-concept-ui/05-exact.png`
- Create: `docs/verification/rust-v2-concept-ui/06-similar-images.png`
- Create: `docs/verification/rust-v2-concept-ui/07-similar-videos.png`
- Create: `docs/verification/rust-v2-concept-ui/08-cross-machine.png`
- Create: `docs/verification/rust-v2-concept-ui/09-review.png`
- Create: `docs/verification/rust-v2-concept-ui/10-delete-center.png`
- Create: `docs/verification/rust-v2-concept-ui/11-settings.png`
- Create: `docs/verification/rust-v2-concept-ui/12-diagnostics.png`

**Interfaces:**
- Consumes: Tasks 1–7 完整 UI、现有发布脚本和 Windows 桌面会话。
- Produces: 自动化门禁证据、12 张真实应用截图和明确 PASS/PARTIAL 验收结论。

- [ ] **Step 1: 运行完整 Rust UI 门禁**

```powershell
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked -- --test-threads=1
cargo build --workspace --release --locked --target x86_64-pc-windows-msvc
```

Expected: 四条命令均退出 0；现有 `physical_two_hosts_e2e.rs` 若仍未跟踪，不纳入提交范围，但 Cargo 可能自动发现并编译该测试文件，失败时只修复本次 UI 造成的回归。

- [ ] **Step 2: 构建并验证新的 Windows x64 便携包**

```powershell
& .\scripts\build-release.ps1
& .\scripts\verify-release.ps1 -PackagePath .\dist-rust-v2\mySingerServer-rust-v2-win-x64.zip
```

Expected: 新包构建和静态验证均 PASS；不得沿用 UI 重构前的 ZIP 哈希冒充新结果。

- [ ] **Step 3: 启动真实 Release 界面并逐页检查**

从新的 `dist-rust-v2/staging` 启动 `desktop.exe`。使用 Computer Use 依次进入七个主导航、四个重复标签、审核/删除模式和设置/诊断二级菜单。每次截图前确认窗口为目标 Release 进程，尺寸接近 `1440×900`，没有其他窗口遮挡。

- [ ] **Step 4: 保存 12 张真实截图**

截图文件名必须与本任务 Files 列表完全一致。截图不得使用原概念图、设计稿拼图或静态 HTML 冒充；必须来自本轮实际 `desktop.exe`。相似图片截图确认列表没有自动缩略图，视频截图确认预览区文案为六帧联系表。

- [ ] **Step 5: 编写中文验收记录**

文档逐项记录：命令、退出码、包路径和 SHA-256、12 张截图相对链接、七项导航、四类重复、审核删除、设置诊断、默认回收站和禁用占位状态。Computer Use 若无法点击、截图或识别 Slint 控件，只把未执行项目写为 `PARTIAL`，并保留成功的编译/测试/包验证证据，不能用静态结果替代 GUI PASS。

- [ ] **Step 6: 最终范围核对并提交验收材料**

```powershell
git diff --check
git status --short
git add -- docs/verification/2026-08-19-rust-v2-concept-ui.md docs/verification/rust-v2-concept-ui
git commit -m "docs: record Rust V2 concept UI acceptance"
```

提交前确认 `crates/desktop-core/tests/physical_two_hosts_e2e.rs` 未被暂存。

---

## 最终完成条件

- 计划八个任务全部具有 RED、GREEN、提交和独立审查记录。
- `MainWindow` 21 个回调和现有 Rust 绑定编译保持不变。
- 六张概念图对应的 12 个实际视图全部落地。
- 相似图片没有缩略图生成路径；相似视频仍使用六帧 JPG 联系表。
- 所有概念稿未支持能力都使用 `—`、空状态或禁用说明，没有伪造数据。
- 完整 workspace fmt、clippy、test、release build 和发布包验证按真实结果记录。
- Windows GUI 截图与交互按实际执行状态报告；无法执行的项目明确标为 `PARTIAL`。
- `AGENTS.md` 已记录设计目的、实现方案、组件边界和维护约束。
- 未跟踪的物理双主机测试文件保持原样且不进入 UI 提交。
