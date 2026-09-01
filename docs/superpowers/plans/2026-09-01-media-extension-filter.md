# Media Extension Filter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Rust V2 只枚举 Node 配置允许的图片和视频扩展名，并在 Desktop 远程 Node 配置表单中编辑这两个列表。

**Architecture:** `dedup-core::NodeConfig` 保存两个规范化扩展名数组；`dedup-node-engine` 在每次扫描中构造一个小型 `BTreeSet` 过滤器。Everything 把集合编译成 `ext:` 查询，Windows Walker 在遍历回调中跳过不匹配项，后续 SQLite、缓存查询和 Worker 流水线不变。

**Tech Stack:** Rust 2024、Serde/TOML、Protobuf/Prost、Everything Window Message IPC、Slint、PowerShell 发布脚本。

**Spec:** `docs/superpowers/specs/2026-09-01-media-extension-filter-design.md`

## Global Constraints

- 只通过文件扩展名决定文件是否进入扫描；枚举阶段不得读取文件头或调用 FFmpeg。
- Everything 必须在查询表达式中使用 `ext:`；Windows Walker 必须在遍历回调中跳过不匹配项。
- 空图片列表禁用图片扫描，空视频列表禁用视频扫描，两组都空时扫描返回空清单。
- 不修改 SQLite/PostgreSQL schema、任务状态或 Worker 媒体探测逻辑。
- UI 只增加两个英文逗号分隔的单行输入框和一个“恢复默认格式”按钮。
- 方法、类型、变量和新增业务文件使用中文注释说明用途与逻辑。
- 当前 worktree 已有大量未提交修改；不得 `git reset`、`git clean`、整文件暂存或提交。每个任务以目标文件 diff 和测试作为检查点，避免混入用户原有变更。
- 发布到远端时创建全新目录，不覆盖现有目录，不启动任何 EXE，并校验远端 SHA-256。

---

### Task 1: Node 配置默认值与规范化

**Files:**
- Modify: `crates/core/src/config.rs:105-170`
- Create: `crates/core/tests/node_media_extensions.rs`

**Interfaces:**
- Consumes: 现有 `NodeConfig::default`、`NodeConfig::from_toml`、`NodeConfig::to_toml` 和 `CoreError::InvalidConfig`。
- Produces: `NodeConfig.image_extensions: Vec<String>`、`NodeConfig.video_extensions: Vec<String>`、`NodeConfig::normalized(self) -> Result<Self, CoreError>`。

- [ ] **Step 1: 写配置行为失败测试**

```rust
use dedup_core::NodeConfig;

#[test]
fn missing_extension_fields_receive_complete_defaults() {
    let loaded = NodeConfig::from_toml("").unwrap();
    assert!(loaded.image_extensions.contains(&"jpg".to_owned()));
    assert!(loaded.image_extensions.contains(&"avif".to_owned()));
    assert!(loaded.image_extensions.contains(&"jxl".to_owned()));
    assert!(loaded.video_extensions.contains(&"mp4".to_owned()));
    assert!(loaded.video_extensions.contains(&"mkv".to_owned()));
    assert!(loaded.video_extensions.contains(&"mxf".to_owned()));
}

#[test]
fn save_normalizes_dots_case_order_and_duplicates() {
    let mut config = NodeConfig::default();
    config.image_extensions = vec![" PNG ".into(), ".JPG".into(), "jpg".into()];
    config.video_extensions = Vec::new();

    let loaded = NodeConfig::from_toml(&config.to_toml().unwrap()).unwrap();
    assert_eq!(
        loaded.image_extensions,
        vec!["jpg".to_owned(), "png".to_owned()],
    );
    assert!(loaded.video_extensions.is_empty());
}

#[test]
fn invalid_extension_token_is_rejected() {
    let mut config = NodeConfig::default();
    config.image_extensions = vec!["bad/path".into()];
    let error = config.to_toml().unwrap_err().to_string();
    assert!(error.contains("image_extensions"));
}
```

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```powershell
Remove-Item Env:CC,Env:CXX,Env:AR,Env:RANLIB,Env:CFLAGS,Env:CXXFLAGS,Env:RUSTFLAGS,Env:RUSTC_WRAPPER -ErrorAction SilentlyContinue
cargo test -p dedup-core --test node_media_extensions
```

Expected: 编译失败，指出 `NodeConfig` 尚无两个扩展名字段。

- [ ] **Step 3: 增加精确默认列表**

在 `config.rs` 增加私有常量，并通过小函数转为 `Vec<String>`：

```rust
const DEFAULT_IMAGE_EXTENSIONS: &[&str] = &[
    "apng", "avif", "bmp", "cur", "dds", "dib", "dpx", "exr", "fits", "gif",
    "hdr", "heic", "heif", "ico", "j2c", "j2k", "jfif", "jls", "jp2", "jpc",
    "jpe", "jpeg", "jpg", "jxl", "pam", "pbm", "pcd", "pcx", "pfm", "pgm",
    "pgx", "png", "pnm", "ppm", "psd", "qoi", "ras", "sgi", "svg", "tga",
    "tif", "tiff", "webp", "xbm", "xpm", "xwd",
];

const DEFAULT_VIDEO_EXTENSIONS: &[&str] = &[
    "264", "265", "266", "3g2", "3gp", "amv", "apv", "asf", "av1", "avc",
    "avi", "bik", "bink", "cdxl", "dav", "dif", "divx", "dv", "evc", "evo",
    "f4v", "flm", "flv", "gxf", "h261", "h263", "h264", "h265", "h266", "hevc",
    "ifv", "ismv", "ivf", "kux", "lvf", "m1v", "m2t", "m2ts", "m2v", "m4v",
    "mj2", "mjpeg", "mjpg", "mk3d", "mkv", "moflex", "mov", "mp4", "mpe", "mpeg",
    "mpg", "mts", "mxf", "nsv", "nut", "nuv", "obu", "ogm", "ogv", "pdv",
    "qt", "r3d", "rm", "rmvb", "roq", "rpl", "ser", "smjpeg", "smk", "str",
    "swf", "ts", "ty", "usm", "vc1", "viv", "vivo", "vob", "vvc", "webm",
    "wmv", "wtv", "xmv", "y4m", "yop",
];

fn owned_extensions(defaults: &[&str]) -> Vec<String> {
    defaults.iter().map(|extension| (*extension).to_owned()).collect()
}
```

把字段加入 `NodeConfig`，并在 `Default` 中使用上述列表：

```rust
/// 扫描允许的图片扩展名；值不含前导点并使用小写。
pub image_extensions: Vec<String>,
/// 扫描允许的视频扩展名；值不含前导点并使用小写。
pub video_extensions: Vec<String>,
```

- [ ] **Step 4: 实现唯一规范化边界**

```rust
impl NodeConfig {
    /// 规范化扩展名并验证完整 Node 配置，供 TOML、协议和 UI 保存边界复用。
    pub fn normalized(mut self) -> Result<Self, CoreError> {
        normalize_extensions(&mut self.image_extensions, "image_extensions")?;
        normalize_extensions(&mut self.video_extensions, "video_extensions")?;
        self.validate()?;
        Ok(self)
    }
}

fn normalize_extensions(
    extensions: &mut Vec<String>,
    field: &'static str,
) -> Result<(), CoreError> {
    for extension in extensions.iter_mut() {
        *extension = extension
            .trim()
            .trim_start_matches('.')
            .to_ascii_lowercase();
        if extension.is_empty()
            || !extension
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'+' | b'-'))
        {
            return Err(CoreError::InvalidConfig {
                field,
                reason: "扩展名只能包含 ASCII 字母、数字、_、+、-",
            });
        }
    }
    extensions.sort_unstable();
    extensions.dedup();
    Ok(())
}
```

`from_toml` 在反序列化后调用 `.normalized()`；`to_toml` 对克隆值调用 `.normalized()` 后序列化。不要让 `validate()` 静默改值。

- [ ] **Step 5: 运行核心测试并确认 GREEN**

```powershell
cargo test -p dedup-core --test node_media_extensions
cargo test -p dedup-core
git diff --check -- crates/core/src/config.rs crates/core/tests/node_media_extensions.rs
```

Expected: 全部 PASS，diff-check 无输出。

---

### Task 2: Protobuf 配置往返

**Files:**
- Modify: `proto/node.proto:341-363`
- Modify: `crates/protocol/src/convert.rs:15-145`
- Modify: `crates/protocol/tests/node_config_wire.rs`
- Modify: `crates/desktop-core/tests/node_config_controller.rs:569-594`
- Modify: `crates/node-engine/tests/node_actor.rs:302-333,759-790`

**Interfaces:**
- Consumes: Task 1 的 `NodeConfig::normalized` 和两个公开数组字段。
- Produces: `proto::NodeConfigValue.image_extensions: Vec<String>`、`proto::NodeConfigValue.video_extensions: Vec<String>`；字段号固定为 20、21。

- [ ] **Step 1: 扩充协议失败测试**

在 `node_config_wire.rs` 的完整字段往返测试中写入非默认值并断言：

```rust
config.image_extensions = vec!["png".into(), "jpg".into()];
config.video_extensions = vec!["mkv".into()];

let wire = proto::NodeConfigValue::try_from(&config).unwrap();
assert_eq!(
    wire.image_extensions,
    vec!["jpg".to_owned(), "png".to_owned()],
);
assert_eq!(wire.video_extensions, vec!["mkv".to_owned()]);
assert_eq!(
    NodeConfig::try_from(wire).unwrap().image_extensions,
    vec!["jpg".to_owned(), "png".to_owned()],
);
```

在 descriptor 测试中断言字段号：

```rust
let value = message(messages, "NodeConfigValue").unwrap();
assert!(value.field.iter().any(|field| {
    field.name.as_deref() == Some("image_extensions") && field.number == Some(20)
}));
assert!(value.field.iter().any(|field| {
    field.name.as_deref() == Some("video_extensions") && field.number == Some(21)
}));
```

- [ ] **Step 2: 运行协议测试并确认 RED**

```powershell
cargo test -p dedup-protocol --test node_config_wire
```

Expected: 新字段不存在导致编译失败或 descriptor 断言失败。

- [ ] **Step 3: 追加协议字段并接入转换**

在 `NodeConfigValue` 末尾追加：

```proto
  repeated string image_extensions = 20;
  repeated string video_extensions = 21;
```

编码前先规范化克隆值，再写数组：

```rust
let value = value.clone().normalized()?;
// 其余现有字段保持原映射。
image_extensions: value.image_extensions,
video_extensions: value.video_extensions,
```

解码时把两个数组写入 `NodeConfig`，构造完成后调用 `.normalized()`。保持 `PROTOCOL_VERSION = 5`。

- [ ] **Step 4: 修复直接构造协议 fixture**

所有 `proto::NodeConfigValue { ... }` 字面量显式加入：

```rust
image_extensions: NodeConfig::default().image_extensions,
video_extensions: NodeConfig::default().video_extensions,
```

对于比较自定义配置的 fixture，改用该 fixture 自身的两个数组，禁止用空数组掩盖字段遗漏。

- [ ] **Step 5: 运行协议和配置控制器测试**

```powershell
cargo test -p dedup-protocol --test node_config_wire
cargo test -p dedup-desktop-core --test node_config_controller -- --test-threads=1
cargo test -p dedup-node-engine --test node_actor --features test-hooks -- --test-threads=1
git diff --check -- proto/node.proto crates/protocol/src/convert.rs crates/protocol/tests/node_config_wire.rs crates/desktop-core/tests/node_config_controller.rs crates/node-engine/tests/node_actor.rs
```

Expected: 全部 PASS，协议主版本仍为 5。

---

### Task 3: Everything 与 Windows Walker 枚举过滤

**Files:**
- Create: `crates/node-engine/src/scan/media_extensions.rs`
- Modify: `crates/node-engine/src/scan/mod.rs`
- Modify: `crates/node-engine/src/scan/enumerator.rs`
- Modify: `crates/node-engine/src/scan/everything.rs`
- Modify: `crates/node-engine/src/actor.rs:380-425,654-820,1260-1305,2275-2340`
- Modify: `crates/node-engine/tests/enumerators.rs`

**Interfaces:**
- Consumes: Task 1 的 `NodeConfig.image_extensions` 和 `NodeConfig.video_extensions`。
- Produces:
  - `MediaExtensionFilter::from_config(&NodeConfig) -> Self`
  - `MediaExtensionFilter::matches(&Path) -> bool`
  - `MediaExtensionFilter::everything_extensions() -> Option<String>`
  - `FilteredWindowsWalker::new(MediaExtensionFilter) -> Self`
  - `EverythingEnumerator::new(MediaExtensionFilter) -> Self`
  - `PreferredEverythingEnumerator::new(MediaExtensionFilter) -> Self`

- [ ] **Step 1: 写纯过滤器和 Walker 失败测试**

在 `enumerators.rs` 增加：

```rust
use dedup_core::{DisplayPath, NodeConfig};
use dedup_node_engine::scan::{
    FileEnumerator, FilteredWindowsWalker, MediaExtensionFilter,
};

#[test]
fn filtered_walker_only_returns_configured_extensions() {
    let directory = tempdir().unwrap();
    fs::write(directory.path().join("photo.JPG"), b"image").unwrap();
    fs::write(directory.path().join("movie.mp4"), b"video").unwrap();
    fs::write(directory.path().join("notes.txt"), b"text").unwrap();
    fs::write(directory.path().join("README"), b"none").unwrap();
    let mut config = NodeConfig::default();
    config.image_extensions = vec!["jpg".into()];
    config.video_extensions = vec!["mp4".into()];
    let walker = FilteredWindowsWalker::new(MediaExtensionFilter::from_config(&config));

    let rows = walker
        .enumerate(&[DisplayPath::new(directory.path()).unwrap()])
        .unwrap();
    let names = rows
        .iter()
        .map(|row| {
            row.display_path
                .as_path()
                .file_name()
                .unwrap()
                .to_string_lossy()
                .into_owned()
        })
        .collect::<Vec<_>>();
    assert_eq!(
        names,
        vec!["movie.mp4".to_owned(), "photo.JPG".to_owned()],
    );
}

#[test]
fn empty_filter_returns_no_walker_rows() {
    let directory = tempdir().unwrap();
    fs::write(directory.path().join("photo.jpg"), b"image").unwrap();
    let mut config = NodeConfig::default();
    config.image_extensions.clear();
    config.video_extensions.clear();
    let walker = FilteredWindowsWalker::new(MediaExtensionFilter::from_config(&config));
    assert!(walker
        .enumerate(&[DisplayPath::new(directory.path()).unwrap()])
        .unwrap()
        .is_empty());
}
```

- [ ] **Step 2: 写 Everything 查询失败测试**

在 `everything.rs` 单元测试中对私有查询构造函数断言：

```rust
#[test]
fn everything_query_contains_stable_extension_clause() {
    let mut config = NodeConfig::default();
    config.image_extensions = vec!["png".into(), "jpg".into()];
    config.video_extensions = vec!["mp4".into()];
    let filter = MediaExtensionFilter::from_config(&config);
    let root = DisplayPath::new(r"D:\Media").unwrap();
    assert_eq!(
        build_query(&root, &filter).as_deref(),
        Some(r#"file: path:"D:\Media" ext:jpg;mp4;png"#),
    );
}

#[test]
fn everything_query_is_absent_for_empty_filter() {
    let mut config = NodeConfig::default();
    config.image_extensions.clear();
    config.video_extensions.clear();
    assert!(build_query(
        &DisplayPath::new(r"D:\Media").unwrap(),
        &MediaExtensionFilter::from_config(&config),
    ).is_none());
}
```

- [ ] **Step 3: 运行枚举测试并确认 RED**

```powershell
cargo test -p dedup-node-engine --test enumerators --features test-hooks -- --test-threads=1
```

Expected: 新过滤类型和构造函数不存在导致编译失败。

- [ ] **Step 4: 实现小型扩展名集合**

`media_extensions.rs` 只承担集合构造、路径匹配和 Everything 字符串生成：

```rust
//! 把 Node 图片、视频扩展名配置编译为枚举器共享的只读匹配集合。

use std::{collections::BTreeSet, path::Path};
use dedup_core::NodeConfig;

#[derive(Clone, Debug)]
pub struct MediaExtensionFilter {
    extensions: BTreeSet<String>,
}

impl MediaExtensionFilter {
    /// 合并图片和视频配置；`BTreeSet` 同时提供去重、查询和稳定 Everything 顺序。
    pub fn from_config(config: &NodeConfig) -> Self {
        Self {
            extensions: config
                .image_extensions
                .iter()
                .chain(&config.video_extensions)
                .cloned()
                .collect(),
        }
    }

    /// 仅按最后一个扩展名进行大小写无关匹配，不读取文件内容。
    pub fn matches(&self, path: &Path) -> bool {
        path.extension()
            .and_then(|extension| extension.to_str())
            .map(|extension| extension.to_ascii_lowercase())
            .is_some_and(|extension| self.extensions.contains(&extension))
    }

    /// 返回 Everything `ext:` 使用的分号列表；空集合不产生查询。
    pub fn everything_extensions(&self) -> Option<String> {
        (!self.extensions.is_empty())
            .then(|| self.extensions.iter().cloned().collect::<Vec<_>>().join(";"))
    }
}
```

在 `scan/mod.rs` 导出 `MediaExtensionFilter` 和 `FilteredWindowsWalker`。

- [ ] **Step 5: 实现过滤 Walker 包装器**

保留现有未过滤 `WindowsWalker` 供旧单元夹具使用；新增生产包装器，内部调用 `walk_into`，在任何路径规范化前检查：

```rust
#[derive(Clone, Debug)]
pub struct FilteredWindowsWalker {
    filter: MediaExtensionFilter,
}

impl FilteredWindowsWalker {
    /// 创建使用一个 Node 配置快照的 Walker。
    pub fn new(filter: MediaExtensionFilter) -> Self {
        Self { filter }
    }
}
```

`FileEnumerator::enumerate` 收集匹配行后保持现有规范路径排序和去重；`enumerate_into` 的回调开头固定为：

```rust
if !self.filter.matches(&file.path) {
    return Ok(());
}
```

- [ ] **Step 6: 让 Everything 在 IPC 查询中筛选**

把两个 unit struct 改为持有过滤器的结构，并新增 `new`。查询函数固定为：

```rust
fn build_query(root: &DisplayPath, filter: &MediaExtensionFilter) -> Option<String> {
    filter.everything_extensions().map(|extensions| {
        format!(
            r#"file: path:"{}" ext:{extensions}"#,
            root.as_path().display(),
        )
    })
}
```

`EverythingEnumerator::enumerate` 在过滤集合为空时直接 `Ok(Vec::new())`；否则每个根只调用一次 `query_wait`。`PreferredEverythingEnumerator` 的 Everything 主路径和 Walker 回退路径都由同一个过滤器克隆构造。

- [ ] **Step 7: 接入唯一生产 actor**

在 `NodeRuntime::start_inner` 构造一次：

```rust
let media_extensions = MediaExtensionFilter::from_config(config);
```

把它作为 `spawn_actor` 参数保存到 `EngineState`，创建 `BackgroundJob::Scan` 时克隆进入后台任务。测试工厂统一传：

```rust
MediaExtensionFilter::from_config(&NodeConfig::default())
```

后台枚举分支固定为：

```rust
let rows = match enumerator {
    EnumeratorKind::WindowsWalker => {
        FilteredWindowsWalker::new(media_extensions).enumerate(&options.roots)
    }
    EnumeratorKind::Everything => {
        PreferredEverythingEnumerator::new(media_extensions).enumerate(&options.roots)
    }
};
```

不得在 `run_enumerated_scan_to_base_compute`、SQLite 或缓存解析器再加第二层过滤。

- [ ] **Step 8: 运行 Node Engine 定向测试**

```powershell
cargo test -p dedup-node-engine --test enumerators --features test-hooks -- --test-threads=1
cargo test -p dedup-node-engine everything --features test-hooks -- --test-threads=1
cargo test -p dedup-node-engine --test node_actor --features test-hooks -- --test-threads=1
git diff --check -- crates/node-engine/src/scan/media_extensions.rs crates/node-engine/src/scan/mod.rs crates/node-engine/src/scan/enumerator.rs crates/node-engine/src/scan/everything.rs crates/node-engine/src/actor.rs crates/node-engine/tests/enumerators.rs
```

Expected: 混合目录只返回 JPG/MP4，Everything 查询包含排序后的 `ext:`，actor 测试全部 PASS。

---

### Task 4: Desktop Node 配置表单与发布默认配置

**Files:**
- Modify: `crates/desktop-ui/ui/app.slint:134-180,360-395`
- Modify: `crates/desktop-ui/ui/pages/settings-workspace.slint:110-185,480-590`
- Modify: `crates/desktop-ui/src/bindings.rs:280-380,480-680,1107-1220`
- Modify: `crates/desktop-ui/tests/bindings_contract.rs`
- Modify: `crates/desktop-ui/tests/window_contract.rs:123-250`
- Modify: `scripts/build-release.ps1:38-80`

**Interfaces:**
- Consumes: Task 1 的两个 `NodeConfig` 字段和 `NodeConfig::normalized`，Task 2 的两个协议数组。
- Produces: MainWindow 属性 `node-config-image-extensions`、`node-config-video-extensions` 和回调 `restore-node-extension-defaults()`。

- [ ] **Step 1: 写 UI 绑定失败测试**

在现有远程 Node 配置保存测试中设置：

```rust
window.set_node_config_image_extensions(" PNG, .jpg, jpg ".into());
window.set_node_config_video_extensions("mp4, MKV".into());
```

并在 `SaveNodeConfigAndRestart` 断言：

```rust
assert_eq!(
    config.image_extensions,
    vec!["jpg".to_owned(), "png".to_owned()],
);
assert_eq!(
    config.video_extensions,
    vec!["mkv".to_owned(), "mp4".to_owned()],
);
```

再增加恢复默认行为：

```rust
window.set_node_config_image_extensions(SharedString::default());
window.set_node_config_video_extensions(SharedString::default());
window.invoke_restore_node_extension_defaults();
assert!(window.get_node_config_image_extensions().contains("jpg"));
assert!(window.get_node_config_video_extensions().contains("mp4"));
```

在 `window_contract.rs` 的可滚动字段集合中加入“图片扩展名”“视频扩展名”“恢复默认格式”。

- [ ] **Step 2: 运行 UI 测试并确认 RED**

```powershell
cargo test -p dedup-desktop-ui --test bindings_contract -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract -- --test-threads=1
```

Expected: Slint 新属性、回调和可访问控件尚不存在。

- [ ] **Step 3: 增加最小 Slint 属性和控件**

`app.slint` 增加并转发：

```slint
in-out property <string> node-config-image-extensions;
in-out property <string> node-config-video-extensions;
callback restore-node-extension-defaults();
```

`SettingsWorkspace` 在“节点服务”滚动内容中增加：

```slint
Text { text: "扫描文件类型"; height: 22px; color: Theme.text; font-size: 14px; font-weight: 650; }
NodeTextField {
    label: "图片扩展名";
    value <=> root.node-image-extensions;
    field-enabled: root.node-form-enabled;
    edited => { root.node-edited(); }
}
NodeTextField {
    label: "视频扩展名";
    value <=> root.node-video-extensions;
    field-enabled: root.node-form-enabled;
    edited => { root.node-edited(); }
}
ActionButton {
    label: "恢复默认格式";
    action-enabled: root.node-form-enabled;
    clicked => { root.restore-node-extension-defaults(); }
}
```

输入提示文字说明英文逗号分隔、清空即禁用；不增加标签控件或弹窗。

- [ ] **Step 4: 完成 Rust UI 往返与恢复默认**

增加两个小函数：

```rust
fn extension_list_from_text(text: &str) -> Vec<String> {
    text.split(',')
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .collect()
}

fn extension_list_text(extensions: &[String]) -> String {
    extensions.join(", ")
}
```

`node_config_from_window` 写入两个数组后调用 `.normalized()`；`apply_node_config` 使用 `extension_list_text`；`clear_node_config_form` 清空两项。恢复默认回调使用 `NodeConfig::default()` 设置两个文本值，再复用现有 dirty 比较逻辑，默认值与已加载值相同时不得误标 dirty。

- [ ] **Step 5: 更新发布包首次配置**

在 `scripts/build-release.ps1` 的 `$defaultNodeConfig` 顶层、`[paths]` 前加入以下两个 TOML 数组，确保首次运行生成的配置直接展示完整默认值：

```toml
image_extensions = [
  "apng", "avif", "bmp", "cur", "dds", "dib", "dpx", "exr", "fits", "gif",
  "hdr", "heic", "heif", "ico", "j2c", "j2k", "jfif", "jls", "jp2", "jpc",
  "jpe", "jpeg", "jpg", "jxl", "pam", "pbm", "pcd", "pcx", "pfm", "pgm",
  "pgx", "png", "pnm", "ppm", "psd", "qoi", "ras", "sgi", "svg", "tga",
  "tif", "tiff", "webp", "xbm", "xpm", "xwd"
]
video_extensions = [
  "264", "265", "266", "3g2", "3gp", "amv", "apv", "asf", "av1", "avc",
  "avi", "bik", "bink", "cdxl", "dav", "dif", "divx", "dv", "evc", "evo",
  "f4v", "flm", "flv", "gxf", "h261", "h263", "h264", "h265", "h266", "hevc",
  "ifv", "ismv", "ivf", "kux", "lvf", "m1v", "m2t", "m2ts", "m2v", "m4v",
  "mj2", "mjpeg", "mjpg", "mk3d", "mkv", "moflex", "mov", "mp4", "mpe", "mpeg",
  "mpg", "mts", "mxf", "nsv", "nut", "nuv", "obu", "ogm", "ogv", "pdv",
  "qt", "r3d", "rm", "rmvb", "roq", "rpl", "ser", "smjpeg", "smk", "str",
  "swf", "ts", "ty", "usm", "vc1", "viv", "vivo", "vob", "vvc", "webm",
  "wmv", "wtv", "xmv", "y4m", "yop"
]
```

- [ ] **Step 6: 运行 UI 和发布脚本静态测试**

```powershell
cargo test -p dedup-desktop-ui --test bindings_contract -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout -- --test-threads=1
cargo test -p dedup-protocol --test node_config_wire
git diff --check -- crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/pages/settings-workspace.slint crates/desktop-ui/src/bindings.rs crates/desktop-ui/tests/bindings_contract.rs crates/desktop-ui/tests/window_contract.rs scripts/build-release.ps1
```

Expected: UI 行为与最小尺寸滚动测试 PASS，发布默认 TOML 可由 `NodeConfig::from_toml` 读取。

---

### Task 5: 架构文档、全量验证、打包与远端交付

**Files:**
- Modify: `AGENTS.md` 的扫描枚举和基础计算说明段落
- Verify: `docs/superpowers/specs/2026-09-01-media-extension-filter-design.md`
- Verify: `docs/superpowers/plans/2026-09-01-media-extension-filter.md`
- Generate: `dist-rust-v2/mySingerServer-rust-v2-win-x64.zip`

**Interfaces:**
- Consumes: Tasks 1-4 的完整配置、协议、枚举和 UI 行为。
- Produces: 可复现的 Release ZIP、新的远端隔离目录和本地/远端 SHA-256 证据。

- [ ] **Step 1: 更新长期架构约束**

把“枚举器返回全部普通文件”改为：两种枚举器只返回 Node 配置扩展名并集命中的普通文件；Everything 查询使用 `ext:`，Windows Walker 在遍历时跳过。保留“扩展名不写入领域类型，Worker 仍以 FFmpeg 实际 probe 决定 `MediaKind`”。

- [ ] **Step 2: 执行格式与核心回归**

```powershell
Remove-Item Env:CC,Env:CXX,Env:AR,Env:RANLIB,Env:CFLAGS,Env:CXXFLAGS,Env:RUSTFLAGS,Env:RUSTC_WRAPPER -ErrorAction SilentlyContinue
cargo fmt --all -- --check
cargo test -p dedup-core
cargo test -p dedup-protocol
cargo test -p dedup-node-engine --features test-hooks -- --test-threads=1
cargo test -p dedup-desktop-core --tests -- --test-threads=1
cargo test -p dedup-desktop-ui --tests -- --test-threads=1
```

Expected: 所有命令退出码 0；不得以过滤测试通过替代 Node/UI 既有回归。

- [ ] **Step 3: 检查范围和脏树**

```powershell
git status --short
git diff --check
git diff -- crates/core/src/config.rs proto/node.proto crates/protocol/src/convert.rs crates/node-engine/src/scan crates/node-engine/src/actor.rs crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/pages/settings-workspace.slint crates/desktop-ui/src/bindings.rs scripts/build-release.ps1 AGENTS.md
```

Expected: 无空白错误；只有本计划字段、过滤、UI 和文档相关差异。不要暂存或提交已有用户修改。

- [ ] **Step 4: 构建并验证 Release 包**

```powershell
& .\scripts\build-release.ps1
```

若 Release 二进制已由同一源码成功构建但依赖下载阶段需要复测，仅使用：

```powershell
& .\scripts\build-release.ps1 -SkipBuild
```

Expected: 输出 `RUST_V2_RELEASE_BUILD_PASS`、`PACKAGE_PATH` 和 `PACKAGE_SHA256`；ZIP 内不得包含 data、旧 EXE 或 FFmpeg EXE。

- [ ] **Step 5: 创建远端隔离目录并上传**

本地计算稳定目录名：

```powershell
$package = (Resolve-Path '.\dist-rust-v2\mySingerServer-rust-v2-win-x64.zip').Path
$packageHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $package).Hash
$releaseName = "media-extension-filter-$(Get-Date -Format yyyyMMdd)-$($packageHash.Substring(0, 8).ToLowerInvariant())"
$remoteRoot = "C:\Users\zjh\Downloads\mySingerServer\$releaseName"
$remoteApp = "$remoteRoot\mySingerServer-rust-v2-win-x64"
$remoteZip = "$remoteRoot\mySingerServer-rust-v2-win-x64.zip"
$remoteScpZip = "C:/Users/zjh/Downloads/mySingerServer/$releaseName/mySingerServer-rust-v2-win-x64.zip"
```

先在远端拒绝覆盖已有目录，再上传并解压：

```powershell
ssh codex-192-168-1-6 powershell -NoProfile -Command "if (Test-Path -LiteralPath '$remoteRoot') { throw '目标目录已存在' }"
ssh codex-192-168-1-6 powershell -NoProfile -Command "New-Item -ItemType Directory -Path '$remoteRoot'"
scp $package "codex-192-168-1-6:$remoteScpZip"
ssh codex-192-168-1-6 powershell -NoProfile -Command "New-Item -ItemType Directory -Path '$remoteApp'"
ssh codex-192-168-1-6 powershell -NoProfile -Command "Expand-Archive -LiteralPath '$remoteZip' -DestinationPath '$remoteApp'"
```

不要启动 `desktop.exe`、`node.exe` 或 `worker.exe`。

- [ ] **Step 6: 校验远端 ZIP 与 EXE**

```powershell
$localDesktopHash = (Get-FileHash -Algorithm SHA256 -LiteralPath '.\dist-rust-v2\staging\desktop.exe').Hash
ssh codex-192-168-1-6 powershell -NoProfile -Command "Get-FileHash -Algorithm SHA256 -LiteralPath '$remoteRoot\mySingerServer-rust-v2-win-x64.zip'; Get-FileHash -Algorithm SHA256 -LiteralPath '$remoteRoot\mySingerServer-rust-v2-win-x64\desktop.exe'"
```

Expected: 远端 ZIP 哈希等于 `$packageHash`，远端 `desktop.exe` 哈希等于 `$localDesktopHash`。最终交付明确报告远端完整目录、ZIP 哈希、Desktop 哈希和“未启动 EXE”。
