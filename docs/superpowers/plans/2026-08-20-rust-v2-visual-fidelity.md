# Rust V2 视觉精度修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 按六张已确认效果图修复 Rust V2 Desktop 的图标尺寸、视觉层级、页面密度与空状态，在不改变任何业务契约的前提下，交付可重复的双尺寸视觉证据和重新验证的 Windows x64 发布包。

**Architecture:** `MainWindow` 继续作为唯一 Rust/Slint 桥接边界；测试侧新增确定性视觉夹具与截图入口，生产侧依次收敛 Image 2 图标、主题令牌、固定应用壳、公共视觉组件和七个页面工作区。公共组件只接收显示值并转发既有回调，视觉夹具永不进入 `apps/desktop` 启动路径。

**Tech Stack:** Rust 1.97.1、Slint 1.17.1、`i-slint-backend-testing` 软件渲染器、`image` 0.25.8、GPT Image 2、Windows x64 MSVC、PowerShell、Computer Use。

**Spec:** `docs/superpowers/specs/2026-08-20-rust-v2-visual-fidelity-design.md`

## Global Constraints

- 只在 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup` 和分支 `codex/rust-v2-media-dedup` 中实施。
- 当前工作树已有未提交的 `side-navigation.slint`、`top-command-bar.slint`、`ui/assets/icons/` 和 `07-image2-icons-release.png`；实施者必须先核对内容，再由本计划对应任务精确吸收或替换，不能覆盖其他用户改动。
- 始终保留未跟踪的 `crates/desktop-core/tests/physical_two_hosts_e2e.rs`；不得修改、删除、暂存或借清理命令移除。
- 不使用 `git add -A`、`git add .`、`git clean`、`git reset`、宽泛 checkout 或递归删除；每个任务只暂存列明文件。
- 保持 `MainWindow` 现有全部根属性、四种 `Ui*Row` 模型、21 个外部业务回调和内部 `navigate-to` 根回调、参数顺序、回调次数及整数枚举语义。
- 不新增后端命令、协议字段、数据库字段、媒体算法或生产模拟数据；速度、ETA、版本、日志、容量、删除历史等缺失数据只显示结构化空态或禁用说明。
- 图片仍只在用户选择后按需加载原图；视频仍只使用已有六帧 3×2 联系表；分页、预览和删除安全门禁保持原样。
- 所有应用自有可见语义图标必须来自本轮 GPT Image 2 资产；Windows 标题栏系统按钮、原生复选框、ComboBox 箭头和纯状态圆点除外。
- 图标生成与内容修订必须使用内置 `imagegen` skill；几何后处理只做 Alpha 裁剪、缩放、居中和格式导出，不重新绘图。
- 首选窗口固定 `1440×900`，最小窗口固定 `1080×700`；侧栏 `144px`、顶栏 `58px`、底栏 `32px`、标准详情栏 `300px`。
- 视觉预览夹具只存在于 `crates/desktop-ui/tests/`，不得由环境变量、配置或调试开关进入生产可执行文件。
- 文件、组件、属性、回调和重要布局块继续使用中文注释；生产业务正文不得使用 `8px` 或 `9px`。
- 每项代码变更执行 RED → GREEN → REFACTOR；每个任务完成后先看真实命令输出，再精确提交。
- 自动截图只能作为布局证据；最终还必须打包并用已授权的 Computer Use 激活真实 Release 窗口，走 `Alt+PrintScreen` 手工截图路径。

---

## 目标文件结构

```text
crates/desktop-ui/
  Cargo.toml
  examples/normalize_image2_icons.rs
  tests/icon_assets.rs
  tests/visual_preview.rs
  tests/window_contract.rs
  tests/offscreen_layout.rs
  ui/app.slint
  ui/theme.slint
  ui/assets/icons/
    image2-manifest.md
    app.png
    app-16.png
    app-24.png
    app-32.png
    app-48.png
    app-256.png
    app.ico
    *.png
  ui/layout/*.slint
  ui/components/*.slint
  ui/pages/*.slint
apps/desktop/
  Cargo.toml
  build.rs
tests/windows/Test-RustV2VisualEvidence.ps1
docs/ui-preview/rust-v2/
  after/1440x900/*.png
  after/1080x700/*.png
  comparison/*.png
  release/*.png
docs/verification/2026-08-20-rust-v2-visual-fidelity.md
```

---

### Task 1: 建立隔离视觉夹具和可重复截图入口

**Files:**
- Modify: `crates/desktop-ui/Cargo.toml`
- Create: `crates/desktop-ui/tests/visual_preview.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`

**Interfaces:**
- Consumes: 当前真实 `MainWindow`、`UiNodeRow`、`UiTaskRow`、`UiGroupRow`、`UiMemberRow`、Slint software renderer。
- Produces: 仅测试可见的 `VisualFixture`；12 个视图的确定性导航入口；`target/visual-preview/` PNG 输出；标准/最小尺寸公共边界断言。

- [ ] **Step 1: 启用 PNG 测试编码并写夹具覆盖 RED**

把 `crates/desktop-ui/Cargo.toml` 中的 `image.workspace = true` 改为：

```toml
image = { workspace = true, features = ["png", "ico"] }
```

创建 `visual_preview.rs`，先只写入口测试并调用尚未实现的 `VisualFixture::full()` 与 `render_all_views()`：

```rust
#[test]
fn visual_fixture_covers_every_real_row_state() {
    let fixture = VisualFixture::full();
    assert_eq!(fixture.nodes.len(), 3);
    assert_eq!(fixture.tasks.len(), 3);
    assert_eq!(fixture.groups.len(), 3);
    assert_eq!(fixture.members.len(), 6);
    assert!(fixture.nodes.iter().any(|row| row.status == "在线"));
    assert!(fixture.nodes.iter().any(|row| row.status == "离线"));
    assert!(fixture.nodes.iter().any(|row| row.status == "错误"));
    assert!(fixture.tasks.iter().any(|row| row.status == "运行中"));
    assert!(fixture.tasks.iter().any(|row| row.status == "已完成"));
    assert!(fixture.tasks.iter().any(|row| row.status == "失败"));
    assert!(fixture.members.iter().any(|row| row.review == "未决定"));
    assert!(fixture.members.iter().any(|row| row.review == "保留"));
    assert!(fixture.members.iter().any(|row| row.review == "删除"));
    render_all_views(&fixture, PreviewDestination::TargetDirectory);
}
```

- [ ] **Step 2: 运行编译型 RED**

```powershell
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
cargo test -p dedup-desktop-ui --test visual_preview visual_fixture_covers_every_real_row_state --locked -- --exact --test-threads=1
```

Expected: 编译失败，错误明确指向缺失的 `VisualFixture`、`PreviewDestination` 和 `render_all_views`；不得通过删除断言或借用生产状态修复。

- [ ] **Step 3: 实现完整字面夹具**

`VisualFixture::full()` 必须使用以下精确状态矩阵，所有字段都来自现有四种行模型：

| 模型 | 行 | 必填值 |
|---|---|---|
| 节点 | 本机节点 | `index=0`、`127.0.0.1:39091`、在线、`1/2 忙碌`、`1 排队 / 1 运行`、`120 / 125` |
| 节点 | 影像节点 | `index=1`、`10.0.0.8:39091`、离线、`0/4 忙碌`、无任务、`98 / 98` |
| 节点 | 视频节点 | `index=2`、`10.0.0.9:39091`、错误、`0/8 忙碌`、`等待连接`、错误文本 `目标机器拒绝连接` |
| 任务 | 媒体扫描 | 运行中、枚举文件、35%、`7 / 20 · 失败 0 · 跳过 1` |
| 任务 | 图片分析 | 已完成、完成、100%、`18 / 18 · 失败 0 · 跳过 0` |
| 任务 | 视频分析 | 失败、提取特征、60%、`6 / 10 · 失败 1 · 跳过 0` |
| 组 | exact-001 | 精确重复、3 成员、`2.4 GiB` 可回收 |
| 组 | image-001 | 相似图片、2 成员、`38.5 MiB` 可回收 |
| 组 | video-001 | 相似视频、2 成员、`1.1 GiB` 可回收 |
| 成员 | 6 行 | 每组至少 2 行；覆盖代表/非代表、在线/离线、预览启用/禁用、未决定/保留/删除 |

夹具安装函数只调用 `set_nodes`、`set_tasks`、`set_groups`、`set_members` 和既有摘要属性；不新增生产 API。

- [ ] **Step 4: 实现 12 视图渲染和 PNG 写入**

使用下列稳定视图表：

```rust
const VIEWS: [(&str, i32, i32, i32, i32, i32); 12] = [
    ("01-overview", 0, 0, 0, 0, 0),
    ("02-nodes", 0, 1, 0, 0, 0),
    ("03-scan", 1, 0, 0, 0, 0),
    ("04-tasks", 1, 0, 1, 0, 0),
    ("05-exact", 2, 0, 0, 0, 0),
    ("06-similar-images", 3, 0, 0, 0, 0),
    ("07-similar-videos", 4, 0, 0, 0, 0),
    ("08-cross-machine", 5, 0, 0, 0, 0),
    ("09-review", 6, 0, 0, 0, 0),
    ("10-delete-center", 6, 0, 0, 1, 0),
    ("11-settings", 7, 0, 0, 0, 0),
    ("12-diagnostics", 7, 0, 0, 0, 6),
];
```

每次构造独立 `MainWindow`，先安装夹具，再设置 `current-page`、`overview-mode`、`task-mode`、`review-tab`、`settings-section`，最后显示、设尺寸、`take_snapshot()`。把 `slint::Rgba8Pixel` 转成 `image::RgbaImage`：

```rust
fn save_snapshot(
    snapshot: &slint::SharedPixelBuffer<slint::Rgba8Pixel>,
    path: &std::path::Path,
) {
    let bytes = snapshot
        .as_slice()
        .iter()
        .flat_map(|pixel| [pixel.r, pixel.g, pixel.b, pixel.a])
        .collect::<Vec<_>>();
    let image = image::RgbaImage::from_raw(snapshot.width(), snapshot.height(), bytes)
        .expect("快照字节数必须匹配宽高");
    std::fs::create_dir_all(path.parent().expect("预览路径必须有父目录"))
        .expect("应能创建视觉预览目录");
    image.save(path).expect("应能保存视觉预览 PNG");
}
```

默认输出只能是 `target/visual-preview/current/{1440x900|1080x700}`。只有显式设置 `RUST_V2_PREVIEW_OUTPUT` 时才允许写入该环境变量指向的目录；生产程序不得读取此变量。

- [ ] **Step 5: 扩展双尺寸壳边界测试**

在 `offscreen_layout.rs` 提取公共 `assert_inside_window()`，对 1440×900 与 1080×700 都断言侧栏导航、刷新和内容区已知元素不越界。现有四个测试继续保留；不使用整图哈希。

- [ ] **Step 6: 运行 GREEN 和基线产物检查**

```powershell
cargo test -p dedup-desktop-ui --test visual_preview --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
Get-ChildItem -LiteralPath target\visual-preview\current -Recurse -Filter *.png | Measure-Object
```

Expected: 两个测试目标通过；最后命令显示 24 个 PNG，标准目录 12 个、最小目录 12 个；这些是测试夹具快照，不得标为真实 Release 截图。

- [ ] **Step 7: 精确提交 Task 1**

```powershell
git add -- crates/desktop-ui/Cargo.toml Cargo.lock crates/desktop-ui/tests/visual_preview.rs crates/desktop-ui/tests/offscreen_layout.rs
git commit -m "test: add deterministic Rust V2 visual previews"
```

---

### Task 2: 重新生成、归一化并接入完整 Image 2 图标集

**Required skill:** `imagegen`，使用内置 GPT Image 2；不得改用字体图标、SVG 包或手绘替代。

**Files:**
- Create: `crates/desktop-ui/examples/normalize_image2_icons.rs`
- Create: `crates/desktop-ui/tests/icon_assets.rs`
- Create/Replace: `crates/desktop-ui/ui/assets/icons/image2-manifest.md`
- Create/Replace: `crates/desktop-ui/ui/assets/icons/*.png`
- Create: `crates/desktop-ui/ui/assets/icons/app.ico`
- Modify: `crates/desktop-ui/ui/app.slint`
- Modify: `apps/desktop/Cargo.toml`
- Modify: `apps/desktop/build.rs`
- Modify: `Cargo.lock`

**Interfaces:**
- Consumes: GPT Image 2 透明 PNG 原始输出；Task 1 PNG/ICO 编解码能力；Slint `Window.icon`。
- Produces: 27 个语义明确、几何归一的应用图标；五种品牌尺寸与 `app.ico`；Windows 标题栏和 PE 资源图标；可自动验证的 Alpha 几何契约。

- [ ] **Step 1: 先写图标尺寸、包围盒和质心 RED**

在 `icon_assets.rs` 固定两组资产：

```rust
const NAVIGATION: [&str; 11] = [
    "app.png", "menu.png", "overview.png", "nodes.png", "scan.png",
    "tasks.png", "duplicates.png", "review-delete.png", "settings.png",
    "index.png", "sync.png",
];

const INLINE: [&str; 16] = [
    "search.png", "refresh.png", "add.png", "edit.png", "remove.png",
    "connect.png", "browse.png", "info.png", "cancel.png", "filter.png",
    "preview.png", "retry.png", "keep.png", "delete.png", "save.png",
    "folder.png",
];
```

测试必须逐文件读取 RGBA，断言：导航画布 20×20、Alpha 包围盒每边 17–18px；行内画布 16×16、包围盒每边 14–15px；四边至少 1px 透明；Alpha 加权质心相对画布几何中心两轴偏差均不超过 0.5px；同组 Alpha 总量相对组中位数偏差不超过 12%；所有非透明像素的 RGB 都为纯黑。

品牌变体另外断言 `app-16/24/32/48/256.png` 尺寸与文件名一致，`app.ico` 可由 `image` ICO decoder 打开。

- [ ] **Step 2: 运行真实 RED**

```powershell
cargo test -p dedup-desktop-ui --test icon_assets --locked -- --test-threads=1 --nocapture
```

Expected: 当前九枚高分辨率资产首先因 20×20 尺寸、过量透明边距或缺少资产失败；不能放宽阈值迁就旧图。

- [ ] **Step 3: 用同一轮 Image 2 风格样张生成全部语义**

统一母提示词固定为：

```text
Create a coherent Windows desktop utility icon family. Pure black monochrome line icons on a fully transparent background, straight-on orthographic view, Fluent-inspired rounded geometry, no fill illustration, no shadow, no gradient, no texture, no lettering, no decorative dots. Every icon must remain continuous and recognizable when reduced to 16–20 pixels. Use one consistent visual stroke weight equivalent to 1.75–2 pixels at final size. Center the visual mass on the exact geometric center and leave even optical margins.
```

在同一风格样张确认后，逐枚添加且只添加以下语义尾句：

| 文件 | 语义尾句 |
|---|---|
| `app` | paired media files inside a compact rounded archive mark |
| `menu` | three balanced horizontal menu lines |
| `search` | magnifying glass |
| `refresh` | one clockwise circular refresh arrow |
| `overview` | four-cell dashboard |
| `nodes` | three connected computer nodes |
| `scan` | document with scanning corner |
| `tasks` | checklist with two lines |
| `duplicates` | two overlapping files |
| `review-delete` | reviewed file with restrained delete mark |
| `settings` | simple six-tooth gear |
| `index` | compact indexed database stack |
| `sync` | two balanced opposing arrows |
| `add` | plus inside a compact circle |
| `edit` | simple pencil |
| `remove` | minus inside a compact circle |
| `connect` | link between two endpoints |
| `browse` | open folder |
| `folder` | closed folder |
| `info` | lowercase information mark inside circle |
| `cancel` | stop square inside circle |
| `filter` | funnel |
| `preview` | eye |
| `retry` | compact clockwise retry arrow |
| `keep` | shield check |
| `delete` | restrained trash can |
| `save` | compact save disk |

原始 Image 2 输出只保存在 `target/image2-icons/raw/`，不暂存。`image2-manifest.md` 对每个最终文件记录母提示词、语义尾句、生成日期、原始输出名、最终画布和实际使用位置。

- [ ] **Step 4: 实现确定性 Alpha 后处理工具**

`normalize_image2_icons.rs` 接收 `--input target/image2-icons/raw --output crates/desktop-ui/ui/assets/icons`。算法固定为：读取 RGBA；拒绝非透明背景；计算 Alpha>0 包围盒；裁剪；Lanczos3 缩放到导航 18px 或行内 15px 的最长边；放入 20×20 或 16×16 透明画布；按 Alpha 质心选择不超过 1 个物理像素的整数偏移；输出纯黑 RGB 与原 Alpha。若另一边不足允许范围、质心仍超 0.5px 或组面积差超过 12%，工具必须报错并要求重新生成，不得拉伸图形。

品牌母图再按同样中心生成 16、24、32、48、256 PNG；`app.ico` 使用 256px 品牌 PNG 经 `image::codecs::ico::IcoEncoder` 编码。执行命令固定为：

```powershell
cargo run -p dedup-desktop-ui --example normalize_image2_icons --locked -- `
  --input target\image2-icons\raw `
  --output crates\desktop-ui\ui\assets\icons
```

- [ ] **Step 5: 接入 Slint 窗口图标和 Windows EXE 资源**

在 `app.slint` 的 `MainWindow` 中增加：

```slint
icon: @image-url("assets/icons/app-256.png");
```

在 `apps/desktop/Cargo.toml` 增加（接口依据 [winresource 0.1.31 官方文档](https://docs.rs/winresource/0.1.31/winresource/)）：

```toml
[build-dependencies]
winresource = "=0.1.31"
```

把 `apps/desktop/build.rs` 改为：

```rust
//! desktop.exe 构建脚本只固定入口重建边界，并在 Windows 嵌入应用图标。

fn main() {
    println!("cargo:rerun-if-changed=src/main.rs");
    println!("cargo:rerun-if-changed=../../crates/desktop-ui/ui/assets/icons/app.ico");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        winresource::WindowsResource::new()
            .set_icon("../../crates/desktop-ui/ui/assets/icons/app.ico")
            .compile()
            .expect("应能把 Image 2 应用图标嵌入 desktop.exe");
    }
}
```

- [ ] **Step 6: 运行图标 GREEN、Windows 构建和 PE 图标检查**

```powershell
cargo test -p dedup-desktop-ui --test icon_assets --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
cargo build -p desktop --release --locked --target x86_64-pc-windows-msvc
Get-Item -LiteralPath target\x86_64-pc-windows-msvc\release\desktop.exe | Select-Object FullName,Length,LastWriteTime
```

Expected: 所有 Alpha 几何门禁通过；Slint 窗口图标编译成功；MSVC Release 能找到 `rc.exe` 并把 ICO 嵌入 PE。若 Windows SDK 资源编译器缺失，报告环境阻塞，不得静默跳过 EXE 图标。

- [ ] **Step 7: 精确提交 Task 2**

```powershell
git add -- Cargo.lock crates/desktop-ui/Cargo.toml crates/desktop-ui/examples/normalize_image2_icons.rs crates/desktop-ui/tests/icon_assets.rs crates/desktop-ui/ui/assets/icons/image2-manifest.md crates/desktop-ui/ui/assets/icons/app.png crates/desktop-ui/ui/assets/icons/app-16.png crates/desktop-ui/ui/assets/icons/app-24.png crates/desktop-ui/ui/assets/icons/app-32.png crates/desktop-ui/ui/assets/icons/app-48.png crates/desktop-ui/ui/assets/icons/app-256.png crates/desktop-ui/ui/assets/icons/app.ico crates/desktop-ui/ui/assets/icons/menu.png crates/desktop-ui/ui/assets/icons/search.png crates/desktop-ui/ui/assets/icons/refresh.png crates/desktop-ui/ui/assets/icons/overview.png crates/desktop-ui/ui/assets/icons/nodes.png crates/desktop-ui/ui/assets/icons/scan.png crates/desktop-ui/ui/assets/icons/tasks.png crates/desktop-ui/ui/assets/icons/duplicates.png crates/desktop-ui/ui/assets/icons/review-delete.png crates/desktop-ui/ui/assets/icons/settings.png crates/desktop-ui/ui/assets/icons/index.png crates/desktop-ui/ui/assets/icons/sync.png crates/desktop-ui/ui/assets/icons/add.png crates/desktop-ui/ui/assets/icons/edit.png crates/desktop-ui/ui/assets/icons/remove.png crates/desktop-ui/ui/assets/icons/connect.png crates/desktop-ui/ui/assets/icons/browse.png crates/desktop-ui/ui/assets/icons/folder.png crates/desktop-ui/ui/assets/icons/info.png crates/desktop-ui/ui/assets/icons/cancel.png crates/desktop-ui/ui/assets/icons/filter.png crates/desktop-ui/ui/assets/icons/preview.png crates/desktop-ui/ui/assets/icons/retry.png crates/desktop-ui/ui/assets/icons/keep.png crates/desktop-ui/ui/assets/icons/delete.png crates/desktop-ui/ui/assets/icons/save.png crates/desktop-ui/ui/app.slint apps/desktop/Cargo.toml apps/desktop/build.rs
git commit -m "feat: replace desktop icons with normalized Image 2 assets"
```

---

### Task 3: 统一主题令牌并重建固定应用外壳

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/ui/theme.slint`
- Modify: `crates/desktop-ui/ui/layout/app-shell.slint`
- Modify: `crates/desktop-ui/ui/layout/side-navigation.slint`
- Modify: `crates/desktop-ui/ui/layout/top-command-bar.slint`
- Modify: `crates/desktop-ui/ui/layout/status-bar.slint`
- Create: `crates/desktop-ui/ui/components/icon-button.slint`
- Create: `crates/desktop-ui/ui/components/search-field.slint`

**Interfaces:**
- Consumes: Task 2 归一图标、既有 `AppShell` 属性与 `navigate(int)`/`refresh()` 回调。
- Produces: 144/58/32 固定壳；菜单区；七项统一导航；带搜索图标的纯视觉搜索框；独立刷新图标按钮；底栏完整错误可访问名称。

- [ ] **Step 1: 添加壳结构和真实动作 RED**

在 `window_contract.rs` 添加：

```rust
#[test]
fn shell_exposes_menu_search_and_one_refresh_action() {
    i_slint_backend_testing::init_no_event_loop();
    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let menu = accessible(&window, "应用菜单");
    assert_eq!(menu.size(), slint::LogicalSize::new(44.0, 44.0));
    assert!(accessible(&window, "本地搜索").size().width >= 220.0);
    let refresh_count = Rc::new(Cell::new(0));
    window.on_refresh({
        let refresh_count = refresh_count.clone();
        move || refresh_count.set(refresh_count.get() + 1)
    });
    accessible(&window, "刷新").invoke_accessible_default_action();
    assert_eq!(refresh_count.get(), 1);
}
```

在 `offscreen_layout.rs` 增加 `shell_landmarks_fit_both_window_sizes`，在两种尺寸下断言“应用菜单”位于 `x<144,y<58`，“总览”位于 `x<144,y>=58`，“本地搜索”和“刷新”位于顶栏且互不覆盖，底栏三段均在窗口内。

- [ ] **Step 2: 运行 RED**

```powershell
cargo test -p dedup-desktop-ui --test window_contract shell_exposes_menu_search_and_one_refresh_action --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout shell_landmarks_fit_both_window_sizes --locked -- --exact --test-threads=1
```

Expected: 首项因当前没有“应用菜单”失败；第二项随后证明旧品牌卡和刷新文字按钮不符合几何契约。

- [ ] **Step 3: 固定主题长度和文字层级**

保持四个 `Ui*Row` 结构不变，在 `Theme` 增加并统一使用：

```slint
in-out property <color> nav-icon: #475569;
in-out property <color> disabled-text: #94a3b8;
in-out property <color> danger-soft: #fef2f2;
in-out property <length> page-padding: 20px;
in-out property <length> section-gap: 16px;
in-out property <length> control-height: 34px;
in-out property <length> table-header-height: 34px;
in-out property <length> table-row-height: 38px;
in-out property <length> member-row-height: 44px;
in-out property <length> detail-width: 300px;
```

页面标题仍为 24px、详情标题 20px、区块标题 15–16px、正文/按钮 12–13px、表头/辅助文字 10–11px。

- [ ] **Step 4: 实现 IconButton 和 SearchField**

`IconButton` 固定接口：

```slint
export component IconButton inherits Rectangle {
    in property <image> icon;
    in property <string> accessible-name;
    in property <bool> action-enabled: true;
    callback clicked();
    width: 32px;
    height: 32px;
    accessible-role: AccessibleRole.button;
    accessible-label: root.accessible-name;
    accessible-enabled: root.action-enabled;
    accessible-action-default => { if root.action-enabled { root.clicked(); } }
}
```

组件内图标固定 16×16、`image-fit: contain`，按 enabled/hover 使用 `colorize`，TouchArea 在图像之后声明。`SearchField` 固定 `query: string` 双向属性和 `accessible-name`，用 Image 2 `search.png`，不创建业务回调。

- [ ] **Step 5: 重建 SideNavigation、TopCommandBar 和 StatusBar**

- 侧栏顶部 58px 只放 44×44“应用菜单”图标动作，不再显示 58px 品牌卡。
- 导航项高 46px，图标容器 20×20，图标与文字间距 10px；选中项蓝色图标、浅蓝背景和 3px 左线；未选中图标使用 `Theme.nav-icon`。
- 设置继续通过垂直伸展固定在底部，七个索引和可访问名称保持不变。
- 顶栏顺序固定为 142px 节点范围、在线摘要、伸展空白、260px SearchField、32×32 刷新 IconButton；刷新只显示图标，不显示“刷新”文字。
- `last-error` 从 TopCommandBar 移除，AppShell 把它传给 StatusBar。
- StatusBar 采用左引擎、中同步、右 PostgreSQL；长错误只显示单行 elide，但底栏根可访问名称包含完整 `last-error`。

- [ ] **Step 6: 运行 GREEN 和现有导航回归**

```powershell
cargo test -p dedup-desktop-ui --test window_contract navigation_actions_preserve_page_mapping --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract shell_exposes_menu_search_and_one_refresh_action --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
```

Expected: 菜单、搜索、刷新和三段状态栏在两种尺寸内；七项导航映射及刷新一次转发不变。

- [ ] **Step 7: 精确提交 Task 3**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/ui/theme.slint crates/desktop-ui/ui/layout/app-shell.slint crates/desktop-ui/ui/layout/side-navigation.slint crates/desktop-ui/ui/layout/top-command-bar.slint crates/desktop-ui/ui/layout/status-bar.slint crates/desktop-ui/ui/components/icon-button.slint crates/desktop-ui/ui/components/search-field.slint
git commit -m "feat: align the Rust V2 application shell"
```

---

### Task 4: 统一公共卡片、标签、表格、动作和空状态

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/ui/components/fluent-card.slint`
- Modify: `crates/desktop-ui/ui/components/metric-card.slint`
- Modify: `crates/desktop-ui/ui/components/tab-strip.slint`
- Modify: `crates/desktop-ui/ui/components/status-pill.slint`
- Modify: `crates/desktop-ui/ui/components/empty-state.slint`
- Modify: `crates/desktop-ui/ui/components/detail-panel.slint`
- Modify: `crates/desktop-ui/ui/components/group-table.slint`
- Modify: `crates/desktop-ui/ui/components/member-list.slint`
- Modify: `crates/desktop-ui/ui/components/score-panel.slint`
- Modify: `crates/desktop-ui/ui/components/delete-dialog.slint`
- Create: `crates/desktop-ui/ui/components/action-button.slint`
- Create: `crates/desktop-ui/ui/components/section-header.slint`
- Create: `crates/desktop-ui/ui/components/filter-bar.slint`
- Create: `crates/desktop-ui/ui/components/progress-bar.slint`

**Interfaces:**
- Consumes: Task 3 主题长度、Image 2 行内动作图标、当前 `UiGroupRow`/`UiMemberRow` 和既有组件回调。
- Produces: 统一视觉组件；`MetricCard.icon`；`EmptyState.density`；`ActionButton.clicked()`；`ProgressBar.value`；不改变组/成员选择、分页、预览和复核回调。

- [ ] **Step 1: 添加真实任务密度和进度可访问 RED**

在 `window_contract.rs` 复用 `install_task_center_fixture`，增加：

```rust
#[test]
fn shared_components_keep_task_rows_dense_and_progress_readable() {
    i_slint_backend_testing::init_no_event_loop();
    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_task_center_fixture(&window);
    window.invoke_navigate_to(3);
    let row = accessible(
        &window,
        "任务项：媒体扫描；节点 7；枚举文件；35%；7 / 20 · 失败 0 · 跳过 1；运行中",
    );
    assert!(
        (44.0..=64.0).contains(&row.size().height),
        "双行任务行应在 44–64px 内，实际={:?}",
        row.size(),
    );
    let progress = accessible(&window, "任务进度：35%");
    assert!(progress.size().width >= 120.0 && progress.size().height >= 8.0);
}
```

在 `offscreen_layout.rs` 为重复工作区增加 1080×700 检查：组表、成员表和详情区都必须拥有正宽度，并能通过自己的 ScrollView 到达；不要求三列所有内容同时无滚动。

- [ ] **Step 2: 运行 RED**

```powershell
cargo test -p dedup-desktop-ui --test window_contract shared_components_keep_task_rows_dense_and_progress_readable --locked -- --exact --test-threads=1
```

Expected: 当前任务行高度 88px 首先违反密度范围，且没有“任务进度：35%”可访问进度组件。

- [ ] **Step 3: 统一基础容器和文字层级**

- `FluentCard` 固定白底、1px 边框、8px 圆角；内部 padding 由使用方选择 12/16/20px，不再让子项贴边。
- `MetricCard` 保留 `title/value/detail/tone`，新增 `icon: image`，高度 96–104px；图标 20×20、数值 22–24px、标题和说明 11–12px。
- `StatusPill` 高度 24px、最小宽 72px，状态点 6px，正文 11px；危险色只用于错误/删除。
- `DetailPanel` 使用 `Theme.detail-width`、白底边框，保持 `accessible-label: "详情面板"` 和 `@children`。
- `ScorePanel` 所有业务得分使用至少 10px，禁止 8/9px。

- [ ] **Step 4: 实现四个轻量视觉组件**

`SectionHeader`：输入 `title`、`subtitle` 和可选 `icon`，高度由内容决定，标题 15–16px；只提供 `@children` 动作槽，不访问根状态。

`FilterBar`：白底、1px 边框、8px 圆角、高 56–64px，提供 `@children`；页面负责放入来源、节点、运行 ID 和动作。

`ProgressBar` 固定接口和可访问语义：

```slint
export component ProgressBar inherits Rectangle {
    in property <int> value;
    in property <color> tone: Theme.accent;
    in property <string> accessible-name: "进度";
    height: 8px;
    border-radius: 4px;
    background: Theme.border;
    accessible-role: AccessibleRole.progress-indicator;
    accessible-label: root.accessible-name + "：" + root.value + "%";
    Rectangle {
        width: parent.width * max(0, min(100, root.value)) / 100;
        height: parent.height;
        border-radius: 4px;
        background: root.tone;
    }
}
```

`ActionButton` 输入 `label`、`icon`、`tone`（0 次要、1 主要、2 危险）、`action-enabled`，转发一次 `clicked()`；高 34px，图标 16×16，正文 12px，TouchArea 后声明，中文 `accessible-label` 与 `label` 一致。

- [ ] **Step 5: 修复标签、空状态和高密度表格**

- `TabStrip` 高 42px；每个标签宽度使用文本 preferred-width + 24px，最小 72px，不再固定 112px；保留 `active-index` 和 `changed(int)`。
- `EmptyState` 新增 `density: int`，0 为紧凑（120–180px），1 为工作区（占剩余区域居中）；保留 `title/detail/disabled-feature`，新增可选图标但不创建虚假动作。
- `GroupTable` 表头 34px、普通行 38px，业务正文不小于 10px；保持 `groups/selected-id/has-more/select-group/load-more`。
- `MemberList` 表头 34px、双行成员 44px，路径和动作不小于 10px；行内预览、保留、删除使用 Task 2 图标，保持 `preview(machine,path)` 和 `review(machine,path,decision)` 参数原样。
- `DeleteDialog` 保持 520×320 根级覆盖层、`file-count/node-count/reclaimable/mode/can-execute/warning` 和 confirm/cancel 回调；只统一字体、按钮和危险色范围。

- [ ] **Step 6: 运行 GREEN、删除覆盖层和组件回归**

```powershell
cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
```

Expected: 任务进度可访问、任务行密度达标；组/成员动作参数、删除确认尺寸与根级覆盖、22 个 Rust 桥接回调全部保持。

- [ ] **Step 7: 精确提交 Task 4**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/ui/components/fluent-card.slint crates/desktop-ui/ui/components/metric-card.slint crates/desktop-ui/ui/components/tab-strip.slint crates/desktop-ui/ui/components/status-pill.slint crates/desktop-ui/ui/components/empty-state.slint crates/desktop-ui/ui/components/detail-panel.slint crates/desktop-ui/ui/components/group-table.slint crates/desktop-ui/ui/components/member-list.slint crates/desktop-ui/ui/components/score-panel.slint crates/desktop-ui/ui/components/delete-dialog.slint crates/desktop-ui/ui/components/action-button.slint crates/desktop-ui/ui/components/section-header.slint crates/desktop-ui/ui/components/filter-bar.slint crates/desktop-ui/ui/components/progress-bar.slint
git commit -m "feat: unify Rust V2 visual components"
```

---

### Task 5: 按效果图收敛总览和节点管理

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/tests/visual_preview.rs`
- Modify: `crates/desktop-ui/ui/pages/overview-dashboard.slint`
- Modify: `crates/desktop-ui/ui/pages/nodes-page.slint`

**Interfaces:**
- Consumes: 3 节点/3 任务视觉夹具、四个指标摘要、既有六个节点动作、Task 4 公共组件。
- Produces: 顶部连续的总览指标/健康/任务区；稳定的节点表+300px 详情+底部添加栏；页面动作参数与次数不变。

- [ ] **Step 1: 添加首内容锚定和节点详情几何 RED**

在 `offscreen_layout.rs` 增加 `overview_and_nodes_start_at_the_top_without_blank_stretch`：

```rust
let title = accessible(&window, "总览标题");
let main = accessible(&window, "总览主要内容");
let title_bottom = title.absolute_position().y + title.size().height;
assert!(
    main.absolute_position().y - title_bottom <= 32.0,
    "总览标题到第一组主要内容不得超过 32px",
);
```

切到节点视图后，断言“节点表”“节点详情”“添加节点栏”从左到右/从上到下不重叠，详情宽度 280–320px，添加栏位于节点表下方且仍在内容区。

在 `window_contract.rs` 保留现有节点动作测试，并新增“节点错误提示”可访问断言，完整错误文本不能只靠红色表达。

- [ ] **Step 2: 运行 RED**

```powershell
cargo test -p dedup-desktop-ui --test offscreen_layout overview_and_nodes_start_at_the_top_without_blank_stretch --locked -- --exact --test-threads=1
```

Expected: 当前页面缺少“总览标题/总览主要内容”地标，或标题到指标卡的旧伸展空白违反 32px 上限。

- [ ] **Step 3: 重排 OverviewDashboard**

- 根使用 ScrollView，但其 VerticalLayout 不放页面标题与第一组内容之间的 stretch。
- 页面标题标记 `accessible-label: "总览标题"`；紧随其后的四卡行标记“总览主要内容”，间距 16px。
- 四张 `MetricCard` 分别使用 `nodes.png`、`tasks.png`、`index.png`、`sync.png`；不新增摘要字段。
- 节点健康和最近任务使用 34px 表头、38px 行；状态继续用真实色与文本。
- 底部两个统计卡各高 150–170px，使用 `EmptyState density: 0`，标题和原因明确；不得伪造图表。
- 当生产模型为空时，节点健康/最近任务的表体各显示工作区空态；视觉夹具仍展示真实行密度。

- [ ] **Step 4: 重排 NodesPage**

- 内容边距 20px，标题与“连接全部”同一行，下面直接进入主区。
- 左侧为“节点表”卡，右侧为 `DetailPanel`；添加节点栏固定在左表下方，高 76–88px。
- 表头/行对齐名称、地址、状态、Worker、任务、同步位置；错误状态仍保留文字胶囊。
- 详情显示现有机器 ID、Worker、任务、同步位置和完整错误；错误块使用 `info.png`、标题“连接错误”和可读正文。
- “连接全部”“添加”“编辑”“同步”“移除”分别使用 `connect/add/edit/sync/remove.png`，仍只转发现有回调。
- 版本、运行时长、服务明细和存储目录没有模型字段时放入一个紧凑禁用说明，不再占据整栏大白板。

- [ ] **Step 5: 生成本任务双尺寸预览并人工对照效果图 01**

```powershell
cargo test -p dedup-desktop-ui --test visual_preview --locked -- --test-threads=1
Get-Item -LiteralPath target\visual-preview\current\1440x900\01-overview.png,target\visual-preview\current\1440x900\02-nodes.png,target\visual-preview\current\1080x700\01-overview.png,target\visual-preview\current\1080x700\02-nodes.png
```

人工检查：标题后立即出现指标/表格；节点主表、添加栏和详情有明确边界；没有无标题的大面积纯白区域；20px 导航图标在几何中心。

- [ ] **Step 6: 运行行为、布局和编译 GREEN**

```powershell
cargo test -p dedup-desktop-ui --test window_contract overview_and_nodes_consume_real_models --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract node_add_forwards_entered_ip_and_port --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract selected_node_actions_forward_existing_callbacks --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
```

Expected: 模型行、选择态、添加/编辑/同步/移除/连接参数不变；双尺寸几何通过。

- [ ] **Step 7: 精确提交 Task 5**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/tests/visual_preview.rs crates/desktop-ui/ui/pages/overview-dashboard.slint crates/desktop-ui/ui/pages/nodes-page.slint
git commit -m "feat: tighten overview and node workspaces"
```

---

### Task 6: 按效果图收敛扫描和任务中心

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/tests/visual_preview.rs`
- Modify: `crates/desktop-ui/ui/pages/scan-page.slint`
- Modify: `crates/desktop-ui/ui/pages/task-center-page.slint`

**Interfaces:**
- Consumes: 既有扫描四参数、浏览/本地分析回调、三状态任务模型、取消任务回调、Task 4 `ProgressBar`。
- Produces: 紧凑扫描表单/摘要/边界说明；连续顶部任务标签和高密度任务表；现有动作和状态筛选不变。

- [ ] **Step 1: 添加扫描/任务首屏可达 RED**

在 `offscreen_layout.rs` 增加 `scan_and_task_primary_actions_stay_above_the_fold`。1440×900 和 1080×700 下分别导航到扫描和任务，断言：

- “开始扫描”“浏览节点路径”在内容区内；
- “扫描主要内容”紧随“新建扫描标题”，间距不超过 32px；
- “任务标签栏”和“任务主表”紧随“任务中心标题”；
- 运行中行的“取消任务：task-running”在最小窗口可见或位于任务表自己的滚动区域内。

在 `window_contract.rs` 给现有真实鼠标覆盖测试保留“行选择”和“取消按钮各只命中自身一次”的断言。

- [ ] **Step 2: 运行 RED**

```powershell
cargo test -p dedup-desktop-ui --test offscreen_layout scan_and_task_primary_actions_stay_above_the_fold --locked -- --exact --test-threads=1
```

Expected: 当前缺少页面地标，扫描页三个大块说明导致最小窗口主动作密度不足，任务行仍未使用统一进度组件。

- [ ] **Step 3: 重排 ScanPage**

- 左侧 ScrollView 占剩余宽度，右侧 DetailPanel 固定 300px。
- 标题“新建扫描”后 16px 进入“扫描主要内容”。第一卡包含扫描根、Image 2 `browse.png` 浏览动作、节点、枚举器、强制重算和 `scan.png` 主动作。
- 本地分析卡紧随其后；状态胶囊、任务 UUID、类型和动作保持现有字段。
- 多根、排除目录、最小大小、后缀和高级视频选项合并成一张 120–150px 紧凑说明卡，使用 `info.png`，不绘制假输入框。
- 右侧摘要只显示当前根、节点、枚举器和四步真实流程；底部禁用说明使用紧凑密度。

- [ ] **Step 4: 重排 TaskCenterPage**

- 标题、三个自动宽度标签、任务主表按 16/12px 间距连续顶部排列。
- 任务表每行 52–60px：第一行标题、节点、阶段、状态、取消；第二行 `ProgressBar`、百分比和真实 counts。
- `ProgressBar.accessible-name` 固定为“任务进度”；运行中/排队中显示蓝色或中性色，完成绿色，失败红色。
- 只有运行中行显示 `cancel.png` 动作；完成和失败行不创建取消可访问元素。
- 右侧详情只显示已有 ID、阶段、状态、进度和计数；速度、ETA、当前文件、日志合并为一个紧凑禁用说明。
- 无任务时仅在表体显示工作区空态，标题/标签/详情结构仍保留。

- [ ] **Step 5: 生成双尺寸预览并人工对照效果图 02**

```powershell
cargo test -p dedup-desktop-ui --test visual_preview --locked -- --test-threads=1
Get-Item -LiteralPath target\visual-preview\current\1440x900\03-scan.png,target\visual-preview\current\1440x900\04-tasks.png,target\visual-preview\current\1080x700\03-scan.png,target\visual-preview\current\1080x700\04-tasks.png
```

人工检查：扫描表单和任务表从顶部开始；高级功能边界是紧凑信息卡；任务进度条、状态和取消动作在视觉上属于同一行。

- [ ] **Step 6: 运行既有回调和鼠标层级 GREEN**

```powershell
cargo test -p dedup-desktop-ui --test window_contract scan_start_forwards_four_arguments_in_order --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract scan_browse_and_local_analysis_forward_only_existing_arguments --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract task_tabs_filter_loaded_models_and_cancel_active_task --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract shared_components_keep_task_rows_dense_and_progress_readable --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1
```

Expected: 扫描四参数顺序、浏览/本地分析参数、任务标签筛选、真实鼠标取消一次回调全部保持。

- [ ] **Step 7: 精确提交 Task 6**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/tests/visual_preview.rs crates/desktop-ui/ui/pages/scan-page.slint crates/desktop-ui/ui/pages/task-center-page.slint
git commit -m "feat: tighten scan and task workspaces"
```

---

### Task 7: 按效果图收敛重复结果、审核工作台和删除中心

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/tests/visual_preview.rs`
- Modify: `crates/desktop-ui/ui/pages/duplicate-workspace.slint`
- Modify: `crates/desktop-ui/ui/pages/review-delete-workspace.slint`
- Modify: `crates/desktop-ui/ui/components/group-table.slint`
- Modify: `crates/desktop-ui/ui/components/member-list.slint`

**Interfaces:**
- Consumes: 四类重复页状态、有限游标分页、组/成员模型、按需预览、复核、快捷复核、准备删除和跨机器分析现有回调。
- Produces: 统一过滤栏；稳定三栏重复/审核布局；明确双栏删除中心；所有调用次数、参数、游标和危险门禁保持。

- [ ] **Step 1: 添加两种尺寸的命名列与过滤栏 RED**

在 `offscreen_layout.rs` 增加 `result_review_and_delete_workspaces_keep_named_regions`：

- 1440×900 的四个重复类型都必须找到“结果过滤栏”“重复组表”“成员表”“详情面板”；组表宽 360–380px，详情宽 280–320px，成员表占中间剩余宽度，三者不重叠。
- 1080×700 允许组表/成员表自己的水平滚动，但三个命名区域仍按从左到右顺序存在，详情不得覆盖成员表。
- 审核标签必须找到“审核过滤栏”“审核组队列”“审核成员列表”“复核详情”。
- 删除中心必须找到“删除批次摘要”“删除执行详情”，两个区域不重叠；没有历史模型时仍显示有标题和原因的空态。

- [ ] **Step 2: 运行 RED**

```powershell
cargo test -p dedup-desktop-ui --test offscreen_layout result_review_and_delete_workspaces_keep_named_regions --locked -- --exact --test-threads=1
```

Expected: 当前工作区缺少新的过滤栏和职责地标；旧页面在空模型时仍表现为大面积无语义白板。

- [ ] **Step 3: 重排 DuplicateWorkspace 顶部和三栏**

- 四个类型标签仍把 `current-page` 映射到 2/3/4/5，不改变已加载状态。
- 标签下使用 `FilterBar`：来源 ComboBox、节点索引、分析运行 UUID、`filter.png`/“加载结果”动作；字段和回调参数顺序保持。
- 主区固定为约 370px `GroupTable`、伸展 `MemberList`、300px `DetailPanel`，间距 12px。
- 每栏顶部都有 15–16px 标题和 10–11px 辅助文字；空模型使用工作区空态，不删除栏结构。
- 详情预览只有用户触发 `preview.png` 后才显示已有 `preview-image/preview-info`；初始预览调用次数仍为 0。
- 跨机器页的 start/poll/retry 使用 `duplicates/sync/retry.png`，仍只转发既有字符串选择和状态。

- [ ] **Step 4: 重排审核工作台和删除中心**

- 顶部主标签“审核工作台/删除中心”和其状态子标签都使用自动宽度 TabStrip。
- 审核工作台：`FilterBar` + 370px 审核组队列 + 伸展审核成员列表 + 300px 复核详情。
- 快捷复核、路径规则和确认动作分成三个带标题区块；保留 `review-filter` 和 `quick-review(rule,value)` 语义。
- 未决定/保留/删除使用 `info/keep/delete.png`，只有删除决定使用危险色。
- 删除中心：左侧删除批次摘要只展示当前 `delete-*` 根属性可表达的信息；右侧执行详情显示门禁、警告和准备动作。没有批次历史字段时显示禁用说明，不造假行。
- `prepare-delete()` 仍只准备并打开确认；实际 `confirm-delete()` 只由根级 DeleteDialog 触发，离线或 `delete-can-execute=false` 时按钮禁用。

- [ ] **Step 5: 保持组/成员动作真实鼠标层级**

`GroupTable` 和 `MemberList` 的行选择 TouchArea 必须先声明在内容下方；后声明的预览/保留/删除动作优先命中。不得用整行 TouchArea 覆盖行内动作。保留现有行为测试捕获的七类结果回调和审核/删除回调参数。

- [ ] **Step 6: 生成六个结果/审核视图的双尺寸预览**

```powershell
cargo test -p dedup-desktop-ui --test visual_preview --locked -- --test-threads=1
Get-ChildItem -LiteralPath target\visual-preview\current\1440x900 -Filter *.png | Where-Object Name -Match '05-|06-|07-|08-|09-|10-'
Get-ChildItem -LiteralPath target\visual-preview\current\1080x700 -Filter *.png | Where-Object Name -Match '05-|06-|07-|08-|09-|10-'
```

人工对照效果图 03–05：每栏有表头/标题/空态；三栏比例稳定；动作是 16px Image 2 图标；没有 8/9px 业务文字或无意义纯白板。

- [ ] **Step 7: 运行分页、预览、复核和删除门禁 GREEN**

```powershell
cargo test -p dedup-desktop-ui --test window_contract duplicate_tabs_preserve_loaded_state_and_forward_existing_callbacks --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract review_filters_loaded_members_and_delete_confirmation_obeys_gate --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout duplicate_workspace_columns_stay_ordered_inside_content_area --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout delete_confirmation_is_a_centered_root_level_light_overlay --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-core --test review_delete --locked -- --test-threads=1
```

Expected: load-groups/load-members/load-preview/save-review/start/poll/retry 参数与次数不变；初始预览仍为 0；删除确认仍受在线与 can-execute 门禁控制。

- [ ] **Step 8: 精确提交 Task 7**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/tests/visual_preview.rs crates/desktop-ui/ui/pages/duplicate-workspace.slint crates/desktop-ui/ui/pages/review-delete-workspace.slint crates/desktop-ui/ui/components/group-table.slint crates/desktop-ui/ui/components/member-list.slint
git commit -m "feat: tighten duplicate review and delete workspaces"
```

---

### Task 8: 按效果图收敛设置与日志诊断

**Files:**
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/tests/visual_preview.rs`
- Modify: `crates/desktop-ui/ui/pages/settings-workspace.slint`

**Interfaces:**
- Consumes: 七项二级菜单、真实路径/健康/最后错误、12 个双向设置字段、一次 `save-settings()`。
- Produces: 190px 二级菜单、对齐的表单卡、紧凑禁用控件和独立诊断结构；值跨菜单保持且保存一次。

- [ ] **Step 1: 添加设置表单对齐和诊断可达 RED**

在 `offscreen_layout.rs` 扩展最小尺寸设置测试：

- “设置标题”到“设置主要内容”不超过 32px；
- 二级菜单宽度严格 190px，内容卡在其右侧且不覆盖；
- 常规区找到“常规表单网格”，PostgreSQL URL、重连间隔、删除方式的标签和控件边界对齐；
- 日志与诊断区找到“诊断状态卡”“诊断路径卡”“诊断动作栏”，均在 1080×700 的内容 ScrollView 中可达；
- “保存设置”在两个尺寸中始终可见。

- [ ] **Step 2: 运行 RED**

```powershell
cargo test -p dedup-desktop-ui --test offscreen_layout settings_workspace_stays_reachable_at_minimum_size --locked -- --exact --test-threads=1
```

Expected: 现有测试在新增表单/诊断地标处失败，证明当前内容仍是概念说明主导而非对齐控件结构。

- [ ] **Step 3: 重排 SettingsWorkspace 固定骨架**

- 顶栏使用 `SectionHeader`：24px“设置”、短说明、关于 Slint 次按钮、`save.png` 主按钮。
- 主区保持 190px 二级菜单 + 伸展内容卡，间距 16px；菜单项高 40px，正文 12px。
- 内容卡统一 padding 20px；表单标签列、控件列和说明列对齐；普通控件高 34px。
- about-open 仍只切换视觉面板，不修改 `active-section` 或任何设置值。

- [ ] **Step 4: 逐区只呈现真实字段或结构化禁用控件**

- 常规：PostgreSQL URL、重连秒数、删除模式保持双向绑定；可信局域网明文提示使用紧凑警告条。
- 相似度算法：九个已有阈值按三列对齐，字段和值不变；说明只保留分析创建时快照边界。
- 存储：四个真实路径使用只读 ValueRow，不缩小长路径字体，通过 elide 和可访问全值处理。
- 节点服务、扫描与性能、外部工具：使用禁用的 34px 控件外观和一行原因，不用大段概念文字填充。
- 日志与诊断：独立“诊断状态卡”“诊断路径卡”“诊断动作栏”，展示真实 PostgreSQL 状态、四路径和最后错误；筛选/导出/清空/环境版本仍禁用。

- [ ] **Step 5: 生成设置/诊断双尺寸预览并人工对照效果图 06**

```powershell
cargo test -p dedup-desktop-ui --test visual_preview --locked -- --test-threads=1
Get-Item -LiteralPath target\visual-preview\current\1440x900\11-settings.png,target\visual-preview\current\1440x900\12-diagnostics.png,target\visual-preview\current\1080x700\11-settings.png,target\visual-preview\current\1080x700\12-diagnostics.png
```

人工检查：菜单、表单标签、控件和说明列形成稳定网格；诊断不是无标题白板；禁用项仍可读；保存动作视觉层级明确。

- [ ] **Step 6: 运行设置值、保存次数和最小尺寸 GREEN**

```powershell
cargo test -p dedup-desktop-ui --test window_contract settings_sections_preserve_real_values_and_save_once --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout settings_workspace_stays_reachable_at_minimum_size --locked -- --exact --test-threads=1
cargo test -p dedup-desktop-ui --test visual_preview --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop --locked
```

Expected: 12 个真实字段跨七分区和 About 切换不重置；保存只调用一次；1080×700 所有分区可达。

- [ ] **Step 7: 精确提交 Task 8**

```powershell
git add -- crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/tests/visual_preview.rs crates/desktop-ui/ui/pages/settings-workspace.slint
git commit -m "feat: align settings and diagnostics workspace"
```

---

### Task 9: 完整视觉回归、真实 Release 手工截图和发布包复验

**Required skill:** `computer-use:computer-use`，用户已授权控制 Windows 应用；截图必须使用真实 Release 窗口。

**Files:**
- Modify: `crates/desktop-ui/tests/visual_preview.rs`
- Create: `tests/windows/Test-RustV2VisualEvidence.ps1`
- Create: `docs/ui-preview/rust-v2/after/1440x900/*.png`
- Create: `docs/ui-preview/rust-v2/after/1080x700/*.png`
- Create: `docs/ui-preview/rust-v2/comparison/*.png`
- Create: `docs/ui-preview/rust-v2/release/1440x900/*.png`
- Create: `docs/ui-preview/rust-v2/release/1080x700/*.png`
- Create: `docs/verification/2026-08-20-rust-v2-visual-fidelity.md`

**Interfaces:**
- Consumes: 12 视图视觉入口、六张效果图、全部自动化测试、`scripts/build-release.ps1`、`scripts/verify-release.ps1 -Package`、真实 staging `desktop.exe`。
- Produces: 24 张夹具 PNG、6 张人工对照板、24 张真实 Release 手工窗口 PNG、独立验证的 ZIP 与 SHA-256、中文验收记录。

- [ ] **Step 1: 先写视觉证据完整性 RED**

`Test-RustV2VisualEvidence.ps1` 固定视图名：

```powershell
$views = @(
    '01-overview','02-nodes','03-scan','04-tasks',
    '05-exact','06-similar-images','07-similar-videos','08-cross-machine',
    '09-review','10-delete-center','11-settings','12-diagnostics'
)
$sizes = @(
    @{ Name = '1440x900'; Width = 1440; Height = 900 },
    @{ Name = '1080x700'; Width = 1080; Height = 700 }
)
```

脚本逐项验证 `after/<size>/<view>.png` 与 `release/<size>/<view>.png` 存在且 PNG 可解码。离屏 `after` 宽高必须精确匹配 1440×900 或 1080×700；真实 `Alt+PrintScreen` 图包含 Windows 非客户区，目录名表示 Slint 客户区目标，捕获宽度允许目标值到目标值+16px，高度允许目标值+24px 到目标值+48px，并把每张实际尺寸写入报告。脚本还验证 `comparison/01-overview-nodes.png` 至 `06-settings-diagnostics.png` 六文件存在，以及报告包含包绝对路径、大小、SHA-256、DPI 缩放、客户区目标、实际捕获尺寸、真实 Release 和人工结论字段。全部通过后只输出 `RUST_V2_VISUAL_EVIDENCE_PASS`。

- [ ] **Step 2: 运行 RED**

```powershell
pwsh -NoProfile -File tests\windows\Test-RustV2VisualEvidence.ps1
```

Expected: 因 `after`、`comparison`、`release` 和最终报告尚不存在而失败，输出第一个精确缺失路径。

- [ ] **Step 3: 输出 24 张隔离夹具截图和 6 张对照板**

在 `visual_preview.rs` 增加显式文档输出模式。仅当 `RUST_V2_PREVIEW_OUTPUT` 指向仓库内 `docs/ui-preview/rust-v2/after` 时写文档；普通测试仍写 target。

六张对照板映射固定为：

| 效果图 | 修复后视图 |
|---|---|
| `01-overview-nodes.png` | `01-overview` + `02-nodes` |
| `02-scan-tasks.png` | `03-scan` + `04-tasks` |
| `03-exact-cross-machine.png` | `05-exact` + `08-cross-machine` |
| `04-similar-media.png` | `06-similar-images` + `07-similar-videos` |
| `05-review-delete.png` | `09-review` + `10-delete-center` |
| `06-settings-diagnostics.png` | `11-settings` + `12-diagnostics` |

每张对照板为 2320×900：左侧效果图等比缩放至 1600×900；右侧两个 1440×900 修复后视图各缩放至 720×450 并上下排列。图像组合只改变证据画布，不改源截图。

```powershell
$env:RUST_V2_PREVIEW_OUTPUT = (Resolve-Path 'docs\ui-preview\rust-v2').Path
cargo test -p dedup-desktop-ui --test visual_preview --locked -- --test-threads=1
Remove-Item Env:RUST_V2_PREVIEW_OUTPUT
```

人工逐张对照，至少检查：图标几何与线宽、标题到首内容间距、正文/表头字号、表格行高、详情栏比例、空状态说明和 1080×700 可达性。发现差异必须回到对应页面任务修复并重跑，不在对照板上修图。

- [ ] **Step 4: 运行完整自动化与固定 Release 构建**

```powershell
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked -- --test-threads=1
cargo build --workspace --release --locked --target x86_64-pc-windows-msvc
pwsh -NoProfile -File scripts\build-release.ps1 -SkipBuild
pwsh -NoProfile -File scripts\verify-release.ps1 -Package dist-rust-v2\mySingerServer-rust-v2-win-x64.zip
Get-FileHash -LiteralPath dist-rust-v2\mySingerServer-rust-v2-win-x64.zip -Algorithm SHA256
```

Expected: fmt、严格 Clippy、workspace tests、Release build、`RUST_V2_RELEASE_BUILD_PASS` 和两次 `PACKAGE_PASS` 全部成功。若因空间、PostgreSQL、真实双机环境或 GUI 接口失败，按 `PASS/PARTIAL/BLOCKED` 如实记录，不清理其他工作树或伪造通过。

- [ ] **Step 5: 用 Computer Use 启动唯一 staging Release**

1. 用 Computer Use 列出当前应用和窗口，记录不存在 staging desktop 窗口。
2. 启动绝对路径 `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup\dist-rust-v2\staging\desktop.exe`。
3. 再次列出应用/窗口，确认只有一个该绝对路径进程，标题为 `mySingerServer · Media Dedup`。
4. 记录当前显示器 DPI 缩放；保持 Slint 首选客户区 1440×900，读取真实窗口外框约 1440×900 加 Windows 边框/标题栏的尺寸，不用离屏图代替。

- [ ] **Step 6: 手工遍历 12 视图并以 Alt+PrintScreen 保存 1440×900**

按固定顺序用可见控件点击：总览、节点、扫描、任务、精确重复、相似图片、相似视频、跨机器、审核工作台、删除中心、设置、日志与诊断。每次点击后先从真实窗口读取当前标题/标签，确认正确视图；再发送 `Alt+PrintScreen`。

用独立 STA PowerShell 读取剪贴板并保存当前文件：

```powershell
powershell.exe -STA -NoProfile -Command "Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; `$image=[System.Windows.Forms.Clipboard]::GetImage(); if (`$null -eq `$image) { throw '剪贴板没有窗口图像' }; `$path=[IO.Path]::GetFullPath(`$env:RUST_V2_SCREENSHOT_PATH); [IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName(`$path)) | Out-Null; `$image.Save(`$path,[Drawing.Imaging.ImageFormat]::Png); `$image.Dispose()"
```

每次执行前把 `RUST_V2_SCREENSHOT_PATH` 设置为 `docs/ui-preview/rust-v2/release/1440x900/<view>.png`。保存后立即读取 PNG 尺寸；客户区目标固定 1440×900，外层 PNG 必须落在证据脚本定义的 Windows 非客户区容差内，否则先修正窗口再重拍。

- [ ] **Step 7: 重复保存 1080×700 的 12 个真实视图**

把同一 Release 窗口的 Slint 客户区调整到 1080×700，再按相同顺序逐页点击、确认标题、`Alt+PrintScreen`，保存到 `release/1080x700/<view>.png`；外层 PNG 同样允许 Windows 边框/标题栏容差。重点确认：主动作可见或可通过该区域滚动到达；三栏不永久遮挡；设置保存可见；图标未变成点阵。

完成后用标准 `Alt+F4` 关闭本轮 staging 窗口，并再次列出应用/窗口，确认没有残留 staging desktop 进程。

- [ ] **Step 8: 写中文验收记录并运行证据 GREEN**

`docs/verification/2026-08-20-rust-v2-visual-fidelity.md` 必须记录：

- 分支、最终提交、工作树、Windows/Slint/Rust 版本；
- Image 2 资产数量、几何测试结果、标题栏/PE 图标检查；
- 24 张夹具截图和 24 张真实 Release 手工截图的路径、尺寸与用途边界；
- 六张对照板逐项结论，不用“感觉接近”代替具体现象；
- 自动化命令及实际 passed/failed/ignored；
- ZIP 绝对路径、字节数、SHA-256、`PACKAGE_PASS`；
- 任何 PARTIAL/BLOCKED 及真实错误原文；
- 受保护 `physical_two_hosts_e2e.rs` 未修改、未暂存。

```powershell
pwsh -NoProfile -File tests\windows\Test-RustV2VisualEvidence.ps1
git diff --check
git status --short
```

Expected: 输出 `RUST_V2_VISUAL_EVIDENCE_PASS`；diff 无空白错误；status 只包含本任务证据文件和原有受保护未跟踪测试。

- [ ] **Step 9: 精确提交 Task 9**

```powershell
git add -- crates/desktop-ui/tests/visual_preview.rs tests/windows/Test-RustV2VisualEvidence.ps1 docs/ui-preview/rust-v2/after docs/ui-preview/rust-v2/comparison docs/ui-preview/rust-v2/release docs/verification/2026-08-20-rust-v2-visual-fidelity.md
git diff --cached --name-status
git commit -m "test: record Rust V2 visual fidelity acceptance"
```

提交前索引不得出现 `crates/desktop-core/tests/physical_two_hosts_e2e.rs`、发布 ZIP、target 或原始 Image 2 输出。

---

## 依赖顺序与复验边界

```text
Task 1 视觉夹具
  -> Task 2 Image 2 资产与程序图标
  -> Task 3 主题与应用壳
  -> Task 4 公共组件
  -> Task 5 总览/节点
  -> Task 6 扫描/任务
  -> Task 7 重复/审核/删除
  -> Task 8 设置/诊断
  -> Task 9 完整门禁、Release 与手工截图
```

- Task 2 必须在页面接入图标前一次性完成全套风格，后续任务只消费已验收资产。
- Task 3/4 是所有页面任务的共享前置，不并行修改相同 `.slint` 文件。
- Task 5、6、7、8 每项结束都生成对应双尺寸预览；不把视觉问题全部推迟到 Task 9。
- Task 9 若发现页面差异，回到拥有该页面的任务文件修复并用新提交记录，不能只修证据图片。

## 最终完成定义

- 27 个应用语义图标及品牌多尺寸全部来自 GPT Image 2，几何、面积、线宽和安全边距门禁通过。
- Windows 标题栏与 `desktop.exe` 均不再显示默认通用程序图标。
- 页面标题到首个主要内容区不超过 32px；1440×900 没有无标题/原因/动作的大面积纯白区域。
- 1080×700 下主动作可见或可通过所属滚动区到达，三栏和详情栏不相互覆盖。
- 生产界面没有 8/9px 业务正文，没有 Unicode 或字体字符冒充应用语义图标。
- 视觉夹具与生产入口严格隔离；生产仍只显示真实模型、真实空态和真实禁用边界。
- 21 个外部业务回调和内部 `navigate-to` 根回调、分页游标、按需预览、任务取消、复核和删除安全门禁行为全部回归通过。
- 24 张夹具图、6 张对照板和 24 张真实 Release 手工窗口图通过证据脚本。
- 完整格式、Clippy、workspace tests、Windows x64 Release、打包和独立复验都有当前输出。
- 验收记录包含发布包绝对路径、大小、SHA-256 和诚实的 PASS/PARTIAL/BLOCKED 边界。
