# mySingerServer Rust V2 媒体去重系统实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 从零实现仅面向 Windows x64 的 Rust 媒体去重系统，使单节点可用 SQLite 完成全部本地功能，并使一个 Slint 管理工具可通过 TCP + Protobuf、PostgreSQL 协调多个局域网节点完成跨机器去重。

**架构：** 工作区由 `desktop.exe`、`node.exe`、`worker.exe` 三个薄入口和十个职责单一的 crate 组成。节点串行拥有 SQLite，Worker 只处理不可信媒体，管理工具拥有 PostgreSQL 与跨节点编排；本地与中心分析共用领域类型、特征算法、不可变输入快照和代表文件分组规则。

**技术栈：** Rust 1.97.1（edition 2024）、Slint 1.17.1、Tokio 1.53.1、Prost 0.14.4、SQLite/rusqlite 0.40.1、PostgreSQL/tokio-postgres 0.7.18、Windows API、FFmpeg 8.0.1 x64 LGPL shared DLL。

**规格：** `docs/superpowers/specs/2026-08-19-rust-v2-media-dedup-design.md`

**执行方式：** 用户已选择在当前任务中使用 `superpowers:executing-plans` 自动执行；计划提交后不再等待二次确认。实施必须先通过 `superpowers:using-git-worktrees` 建立隔离工作区，再按本计划顺序执行。

## 全局约束

- 只面向 `x86_64-pc-windows-msvc`；程序不主动按 Windows 版本拒绝启动。
- 不兼容旧代码、旧协议、旧 SQLite、旧 PostgreSQL、旧配置或旧发布包。
- 不扩大需求；不增加认证、TLS、加密、节点发现、云服务、浏览器端、移动端或自动删除。
- TCP 协议不加密、不认证；界面必须明确提示该端口只能暴露在可信局域网。
- 文件缓存跳过键固定为“机器 ID + 规范路径 + 文件大小”，修改时间不参与。
- 精确重复固定按 MD5 索引后再比较文件大小，不增加 SHA 或逐字节复核。
- 特征算法参数硬编码；匹配阈值可配置，并完整快照到每个 `AnalysisRun`。
- 相似图片不生成缩略图；视频固定均匀抽六帧，联系表固定为 `3×2`、RGB24、JPG 质量 80。
- 节点创建当前 V2 SQLite；PostgreSQL 只由用户手动执行 `deploy/central-v2.sql` 创建。
- SQLite、配置、缓存和日志只放在可执行文件目录下的 `data`，不使用当前工作目录或用户目录回退。
- FFmpeg 只使用固定的 8.0.1 x64 LGPL shared DLL；不运行或发布 `ffmpeg.exe`、`ffprobe.exe`、`ffplay.exe`。
- 默认删除到回收站；永久删除只能由用户在设置中切换，并始终执行大小 + MD5 身份检查。
- 不做过多防御性编程：只在配置、协议、数据库和文件系统边界校验一次，内部使用强类型。
- 不创建只有一个生产实现的空壳 trait；只有文件枚举和媒体解码等确有测试/生产多实现的边界使用 trait。
- 所有业务源文件写中文 `//!` 职责注释，所有公开类型/函数写中文 `///`，业务 crate 启用 `#![warn(missing_docs)]`。
- 每个任务同步更新根 `AGENTS.md` 的设计目的、架构、crate 职责、数据流、不变量及构建/测试命令；它不是流水账。
- 每个任务只执行所列测试和一次最终规格覆盖检查，不追加无限审查轮次。
- 在脏主工作区中只暂存本任务明确列出的新 Rust V2 文件；绝不使用 `git add -A`。

## 固定依赖与来源

| 依赖 | 固定值 | 用途 |
|---|---|---|
| Rust | `1.97.1-x86_64-pc-windows-msvc` | 编译、rustfmt、Clippy |
| Slint | `=1.17.1` | 桌面界面与节点 `SystemTrayIcon` |
| Tokio | `=1.53.1` | TCP、任务与进程异步编排 |
| Prost | `=0.14.4` | Protobuf 生成与编解码 |
| protoc-bin-vendored | `=3.2.0` | 构建期固定 `protoc`，无需系统安装 |
| rusqlite | `=0.40.1`, `bundled` | 节点 SQLite |
| tokio-postgres | `=0.7.18`, `NoTls` | 中心 PostgreSQL |
| image | `=0.25.8`, 仅 `jpeg` | Rust 联系表 JPG 编码与测试图片解码 |
| everything-ipc | `=0.1.4` | 可选 Everything 本机 IPC 枚举 |
| cargo-about | `=0.9.1`，仅构建工具 | 生成 Rust 第三方许可证清单 |
| Meta PDQ | commit `baefb4ed67b6cdc1d4c82dbaef858d50866ac424` | Rust 等价移植与官方 golden |
| FFmpeg | `ffmpeg-n8.0.1-66-g27b8d1a017-win64-lgpl-shared-8.0.zip` | 媒体探测和解码 |
| FFmpeg URL | `https://github.com/BtbN/FFmpeg-Builds/releases/download/autobuild-2026-02-28-12-59/ffmpeg-n8.0.1-66-g27b8d1a017-win64-lgpl-shared-8.0.zip` | 构建期下载 |
| FFmpeg SHA-256 | `E7B1087C310CF8B91F5467B8ADA6D7E47CE26F2777EFA2317C7CC271087E5100` | 供应链校验 |

其余纯 Rust 依赖在根 `Cargo.toml` 使用兼容系列并由提交的 `Cargo.lock` 固定：`bytes 1`、`futures 0.3`、`serde 1`、`toml 1`、`thiserror 2`、`uuid 1`、`md-5 0.10`、`sha2 0.10`、`tracing 0.1`、`tracing-subscriber 0.3`、`tracing-appender 0.2`、`windows 0.62`、`libloading 0.9`、`tempfile 3`。

## 文件结构锁定

```text
AGENTS.md
Cargo.toml
Cargo.lock
rust-toolchain.toml
.cargo/config.toml
apps/{desktop,node,worker}/
crates/
  core/             # ID、配置、领域模型、阈值、路径值对象
  protocol/         # node.proto 生成代码和领域转换
  transport/        # 4 字节分帧、请求复用、优先级写队列
  media/            # MD5、灰度、PDQ、pHash、Sobel、视频评分、联系表
  media-ffmpeg/     # 固定 DLL 加载、FFmpeg FFI、安全探测/解码
  windows/          # 应用路径、SMBIOS、Job Object、回收站、打开目录、Walker
  node-store/       # SQLite schema、事务、查询、outbox
  node-engine/      # 扫描、Worker 池、本地分析、预览、删除
  desktop-core/     # 节点会话、PG、同步、跨机器分析、UI 状态
  desktop-ui/       # Slint 页面和绑定
proto/node.proto
deploy/central-v2.sql
scripts/{fetch-ffmpeg,build-release,verify-release}.ps1
third_party/{ffmpeg-dependency.json,pdq/UPSTREAM.md}
tests/{fixtures,windows}/
```

生成代码只放在 Cargo `OUT_DIR` 或明确标记的 `bindings_8_0_1.rs`；业务逻辑不写入三个 `apps` 入口。每个任务的 `Files` 列表是提交白名单。

## 规格覆盖矩阵

| 规格章节 | 落地任务 |
|---|---|
| 1–6：目标、边界、部署、目录、机器标识 | 任务 1–2、13、17、19 |
| 7–8：领域键、SQLite/PostgreSQL 数据模型 | 任务 2、7–8、14 |
| 9–10：扫描、缓存复用与任务状态机 | 任务 10–12、16 |
| 11–12：精确/图片/视频算法与联系表 | 任务 4–6、9–12 |
| 13–15：TCP+Protobuf、Worker、同步 | 任务 3、10、13、15 |
| 16：本地与跨机器分析编排 | 任务 12、16 |
| 17–18：UI、预览、复核与删除 | 任务 13、17–18 |
| 19–22：FFmpeg、许可证、发布与验收 | 任务 9、13、19–20 |
| 23：工程约束与文档闭环 | 全部任务及最终门禁 |

---

### 任务 1：建立可重复的 Rust x64 工作区与 Agent 架构文档

**Files:**
- Create: `AGENTS.md`
- Create: `Cargo.toml`
- Create: `Cargo.lock`
- Create: `rust-toolchain.toml`
- Create: `.cargo/config.toml`
- Create: `crates/core/Cargo.toml`
- Create: `crates/core/src/lib.rs`
- Create: `apps/desktop/{Cargo.toml,src/main.rs}`
- Create: `apps/node/{Cargo.toml,src/main.rs}`
- Create: `apps/worker/{Cargo.toml,src/main.rs}`
- Create: `crates/{protocol,transport,node-engine,node-store,desktop-core,desktop-ui,media,media-ffmpeg,windows}/Cargo.toml`
- Create: `crates/{protocol,transport,node-engine,node-store,desktop-core,desktop-ui,media,media-ffmpeg,windows}/src/lib.rs`
- Modify: `.gitignore`

**Interfaces:**
- Produces: 根工作区成员清单、统一依赖版本、`x86_64-pc-windows-msvc` 默认目标、所有后续任务必须维护的 `AGENTS.md`。

- [ ] **Step 1: 在隔离工作区确认基线**

Run: `git status --short --branch`

Expected: 当前分支为 `codex/rust-v2-media-dedup`，除计划允许的文件外没有从脏主工作区带入的未提交修改。

- [ ] **Step 2: 安装并验证固定 Rust 工具链**

若 `rustup` 不存在，从 `https://win.rustup.rs/x86_64` 下载 `rustup-init.exe`，运行：

```powershell
rustup-init.exe -y --profile minimal --default-toolchain 1.97.1-x86_64-pc-windows-msvc
rustup component add rustfmt clippy --toolchain 1.97.1-x86_64-pc-windows-msvc
```

Run: `rustc +1.97.1-x86_64-pc-windows-msvc --version`

Expected: 输出以 `rustc 1.97.1` 开头。

- [ ] **Step 3: 先写工作区冒烟测试**

在 `crates/core/src/lib.rs` 写入：

```rust
//! mySingerServer V2 的共享领域内核。
#![warn(missing_docs)]

/// 返回协议和数据库共同使用的产品代号。
pub const fn product_id() -> &'static str {
    "mysingerserver-rust-v2"
}

#[cfg(test)]
mod tests {
    #[test]
    fn product_id_is_stable() {
        assert_eq!(super::product_id(), "mysingerserver-rust-v2");
    }
}
```

- [ ] **Step 4: 创建最小工作区配置并验证失败边界**

根 `Cargo.toml` 声明上述十个 crate 和三个 app；尚未创建的成员会使 `cargo metadata` 失败。

Run: `cargo metadata --no-deps`

Expected: FAIL，错误明确指出第一个尚未创建的 workspace member；这证明成员清单已生效。

- [ ] **Step 5: 为其余成员创建只含中文 crate 文档的最小骨架**

每个库入口先只包含 `//!` 与 `#![warn(missing_docs)]`，每个 app 入口先返回 `Ok(())`。`.cargo/config.toml` 固定默认 target，并为 Slint Windows 构建传递 `/STACK:8388608`。`.gitignore` 新增 `/target/`、`/dist-rust-v2/` 和 `/downloads/`。

- [ ] **Step 6: 写入根 AGENTS.md 初始架构**

必须包含“设计目的、进程拓扑、crate 责任表、数据所有权、同步与分析状态机、硬约束、当前构建命令、验收边界”八章，并明确旧 Go/C++ 只作参考、Rust 路径不调用旧产物。

- [ ] **Step 7: 验证工作区**

Run: `cargo test -p dedup-core`

Expected: `product_id_is_stable ... ok`。

Run: `cargo metadata --no-deps --format-version 1`

Expected: 13 个 workspace member 全部出现，默认目标为 x64 MSVC。

- [ ] **Step 8: 格式化并提交**

```powershell
cargo fmt --all -- --check
git add -- AGENTS.md Cargo.toml Cargo.lock rust-toolchain.toml .cargo/config.toml .gitignore apps crates
git commit -m "build: bootstrap Rust V2 workspace"
```

### 任务 2：实现强类型领域模型、配置、应用目录和物理机器 ID

**Files:**
- Create: `crates/core/src/error.rs`
- Create: `crates/core/src/ids.rs`
- Create: `crates/core/src/model.rs`
- Create: `crates/core/src/thresholds.rs`
- Create: `crates/core/src/config.rs`
- Create: `crates/core/src/path.rs`
- Create: `crates/windows/src/app_layout.rs`
- Create: `crates/windows/src/machine_id.rs`
- Create: `crates/windows/src/smbios.rs`
- Modify: `crates/core/src/lib.rs`
- Modify: `crates/windows/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `MachineId`, `ContentKey`, `LocationKey`, `NormalizedPath`, `TaskId`, `AnalysisRunId`, `GroupId`, `Thresholds`, `DesktopConfig`, `NodeConfig`, `AppLayout`。
- Produces: `machine_id_from_fields(&PhysicalMachineFields) -> Result<MachineId>` 与 `read_physical_machine_fields() -> Result<PhysicalMachineFields>`。

- [ ] **Step 1: 写 ID、排序和阈值失败测试**

```rust
#[test]
fn content_key_orders_by_md5_then_size() {
    let a = ContentKey::new([1; 16], 20);
    let b = ContentKey::new([2; 16], 10);
    assert!(a < b);
}

#[test]
fn thresholds_match_confirmed_defaults() {
    let t = Thresholds::default();
    assert_eq!(t.pdq_quality_min, 50);
    assert_eq!(t.phash_min_passed_parts, 8);
    assert_eq!(t.video_min_valid_frames, 4);
    assert_eq!(t.video_stage2_min, 0.80);
}
```

Run: `cargo test -p dedup-core`

Expected: FAIL，因为模块尚不存在。

- [ ] **Step 2: 实现领域值对象**

`ContentKey` 固定为 `{ md5: [u8; 16], file_size: u64 }`；`LocationKey` 固定为 `{ machine_id: MachineId, normalized_path: NormalizedPath }`。所有 UUID 业务 ID 使用 `uuid::Uuid::now_v7()`，但机器 ID 不使用 UUID。

- [ ] **Step 3: 实现阈值与 TOML 边界校验**

`Thresholds::validate()` 只检查：Quality `0..=100`、长宽比容差 `0.0..=1.0`、PDQ `0..=256`、单块 pHash `0..=64`、通过块 `1..=9`、Sobel/视频分数 `0.0..=1.0`、有效帧 `1..=6`。无效配置阻止创建分析运行，不在内部重复检查。

- [ ] **Step 4: 写应用路径测试并实现 AppLayout**

```rust
#[test]
fn layout_is_based_on_executable_not_current_directory() {
    let layout = AppLayout::from_executable(Path::new(r"C:\Portable\worker.exe")).unwrap();
    assert_eq!(layout.data_root(), Path::new(r"C:\Portable\data"));
    assert_eq!(layout.ffmpeg_root(), Path::new(r"C:\Portable\runtime\ffmpeg"));
}
```

`AppLayout` 只接受绝对可执行文件路径，返回 `data/desktop`、`data/node`、日志、缓存及固定 FFmpeg 路径；不读当前工作目录。

- [ ] **Step 5: 写机器 ID golden 测试**

```rust
#[test]
fn physical_fields_make_stable_machine_id() {
    let fields = PhysicalMachineFields {
        system_uuid: Some(" 00112233-4455-6677-8899-aabbccddeeff ".into()),
        system_serial: Some("sys-42".into()),
        baseboard_serial: Some("board-9".into()),
    };
    assert_eq!(machine_id_from_fields(&fields).unwrap().as_str().len(), 64);
    assert_eq!(machine_id_from_fields(&fields), machine_id_from_fields(&fields));
}
```

另写三个字段全为空时返回 `CoreError::MissingPhysicalIdentity` 的测试。

- [ ] **Step 6: 实现 SMBIOS 读取和哈希**

`smbios.rs` 使用 `GetSystemFirmwareTable('RSMB')` 解析 Type 1 的 UUID/系统序列号与 Type 2 的主板序列号。`machine_id_from_fields` 按确认规则 trim、转大写、跳过空字段、NUL 分隔，并计算 `SHA-256("mysingerserver-v2-machine\0" + fields)`，输出 64 个小写十六进制字符。

- [ ] **Step 7: 写路径规范化与目录边界测试并实现**

覆盖大小写无关、`C:\Media` 不包含 `C:\Media2`、尾分隔符、UNC 根和 `\\?\` 前缀。目录归属用规范化组件比较，不用字符串前缀。

Run: `cargo test -p dedup-core -p dedup-windows`

Expected: 所有 ID、配置、路径、应用目录和 SMBIOS 解析测试 PASS；真实 SMBIOS 读取测试仅在 Windows x64 运行。

- [ ] **Step 8: 更新架构文档并提交**

```powershell
git add -- AGENTS.md crates/core crates/windows
git commit -m "feat: add Rust V2 domain and Windows identity"
```

### 任务 3：定义完整 Protobuf 协议和有优先级的 TCP 传输

**Files:**
- Create: `proto/node.proto`
- Create: `crates/protocol/build.rs`
- Create: `crates/protocol/src/generated.rs`
- Create: `crates/protocol/src/convert.rs`
- Create: `crates/protocol/src/error.rs`
- Create: `crates/transport/src/frame.rs`
- Create: `crates/transport/src/pending.rs`
- Create: `crates/transport/src/priority_writer.rs`
- Create: `crates/transport/src/connection.rs`
- Modify: `crates/protocol/src/lib.rs`
- Modify: `crates/transport/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `Envelope { request_id, payload }`、`WorkerEnvelope`、`FrameReader`、`FrameWriter`、`PriorityWriter`、`ClientConnection::request`。
- Consumes: 任务 2 的 ID、`ContentKey`、`LocationKey`、`Thresholds`。

- [ ] **Step 1: 写协议编解码失败测试**

```rust
#[test]
fn content_key_round_trips_without_local_content_id() {
    let key = ContentKey::new([0x5a; 16], 1234);
    let wire = proto::ContentKey::from(&key);
    assert_eq!(ContentKey::try_from(wire).unwrap(), key);
}

#[test]
fn envelope_has_no_content_id_field() {
    let descriptor = FILE_DESCRIPTOR_SET;
    assert!(!String::from_utf8_lossy(descriptor).contains("content_id"));
}
```

Run: `cargo test -p dedup-protocol`

Expected: FAIL，因为 proto 和生成脚本尚不存在。

- [ ] **Step 2: 写 node.proto 消息清单**

`Envelope.oneof` 必须完整包含：`Hello`、`NodeStatus`、`Ping`、`Error`、`CreateScan`、`TaskAccepted`、`CancelTask`、`QueryTask`、`ListTasks`、`TaskEvent`、`BrowsePaths`、`CreateLocalAnalysis`、`QueryAnalysisRun`、`ListGroups`、`ListGroupMembers`、`SaveReviewMark`、`PrepareAnalysisInput`、`DispatchStage2`、`PullChanges`、`SyncChangeBatch`、`SyncAck`、`BeginSnapshot`、`ReadSnapshotPage`、`ReadFile`、`FileChunk`、`CreateDeleteBatch`、`RetryDeleteItems`。

`ErrorCode` 固定包含 `INVALID_REQUEST`、`NODE_BUSY`、`NOT_FOUND`、`CONFLICT`、`SNAPSHOT_REQUIRED`、`INTERNAL`。所有主动事件使用 `request_id=0` 并携带 `task_id`、单调 `event_seq`。`ContentKey` 只有 16 字节 MD5 与 `uint64 file_size`；`LocationKey` 只有机器 ID 和规范路径。

- [ ] **Step 3: 定义 WorkerEnvelope**

Worker 请求固定为 `ProbeAndStage1`、`ComputeStage2`、`BuildContactSheet`，结果固定为 `WorkerReady`、`Stage1Result`、`Stage2Result`、`ContactSheetResult`、`WorkerFailure`。每条请求携带 `task_id`、`item_id`、显示路径和所需槽位；不携带数据库连接或网络地址。

- [ ] **Step 4: 用 vendored protoc 生成代码**

`build.rs` 使用 `protoc_bin_vendored::protoc_bin_path()` 设置编译器，输出 descriptor set，并令所有 Protobuf `map` 生成为 `BTreeMap`。生成代码只在 `OUT_DIR`，`generated.rs` 使用 `include!(concat!(env!("OUT_DIR"), "/mysingerserver.v2.rs"))`。

- [ ] **Step 5: 写 4 字节大端分帧测试**

```rust
#[tokio::test]
async fn rejects_ordinary_frame_above_eight_mib() {
    let frame = vec![0_u8; 8 * 1024 * 1024 + 1];
    assert!(matches!(encode_frame(&frame, FrameClass::Ordinary), Err(FrameError::TooLarge)));
}
```

另测 `FileChunk.data.len() <= 1_048_576`，以及零长度和截断帧只在传输边界报错。

- [ ] **Step 6: 实现请求复用和断线收束**

`ClientConnection::request(payload) -> Result<Envelope>` 使用 `AtomicU64` 生成非零请求 ID、`HashMap<u64, oneshot::Sender<_>>` 分派响应；读循环断线时一次性失败全部 pending 请求。重连由 `desktop-core` 完成，transport 不内置重试。

- [ ] **Step 7: 写优先级队列测试并实现**

```rust
#[tokio::test]
async fn control_message_preempts_next_file_chunk() {
    let writer = PriorityWriter::test_writer(2, 2);
    writer.send_low(chunk(1)).await.unwrap();
    writer.send_low(chunk(2)).await.unwrap();
    writer.send_high(cancel()).await.unwrap();
    assert_eq!(writer.drain_payload_kinds().await, ["chunk", "cancel", "chunk"]);
}
```

高低队列都必须有界；每发送一个低优先级块后重新检查高优先级队列。

- [ ] **Step 8: 验证并提交**

Run: `cargo test -p dedup-protocol -p dedup-transport`

Expected: Protobuf round-trip、尺寸上限、请求分派、断线失败和优先级测试全部 PASS。

```powershell
git add -- AGENTS.md proto crates/protocol crates/transport
git commit -m "feat: define Rust V2 protobuf transport"
```

### 任务 4：实现固定像素管线与 Meta PDQ 等价移植

**Files:**
- Create: `third_party/pdq/UPSTREAM.md`
- Create: `crates/media/src/image.rs`
- Create: `crates/media/src/resize.rs`
- Create: `crates/media/src/pdq/mod.rs`
- Create: `crates/media/src/pdq/downscale.rs`
- Create: `crates/media/src/pdq/transform.rs`
- Create: `crates/media/src/pdq/quality.rs`
- Create: `crates/media/src/pdq/median.rs`
- Create: `crates/media/testdata/pdq/bridge-original.jpg`
- Create: `crates/media/testdata/pdq/blur-a-little.jpg`
- Create: `crates/media/testdata/pdq/small.jpg`
- Modify: `crates/media/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `Rgb24Image`, `GrayImage`, `rgb24_to_gray`, `resize_bilinear`, `PdqHash([u8; 32])`, `PdqResult { hash, quality }`, `pdq_hash(&GrayImage)`。

- [ ] **Step 1: 写灰度公式和双线性缩放失败测试**

```rust
#[test]
fn rgb24_uses_confirmed_integer_luma() {
    let rgb = Rgb24Image::new(1, 1, vec![255, 0, 0]).unwrap();
    assert_eq!(rgb24_to_gray(&rgb).pixels(), &[77]);
}

#[test]
fn bilinear_resize_uses_pixel_centers() {
    let src = GrayImage::new(2, 1, vec![0, 100]).unwrap();
    assert_eq!(resize_bilinear(&src, 4, 1).pixels(), &[0, 25, 75, 100]);
}
```

Run: `cargo test -p dedup-media`

Expected: FAIL，因为像素类型和实现尚不存在。

- [ ] **Step 2: 实现拥有所有权的像素类型**

构造函数只在边界验证 `width * height * channels == pixels.len()`；后续算法直接使用已验证切片。灰度公式固定为 `(77R + 150G + 29B + 128) >> 8`，缩放使用像素中心坐标与边缘钳制。

- [ ] **Step 3: 记录并取得固定 PDQ 来源**

`UPSTREAM.md` 记录仓库、commit、BSD 许可证、所参考的 `pdq/cpp/common`、`downscaling`、`hashing/pdqhashing.cpp`、`torben.cpp`，以及“生产代码为独立纯 Rust 等价移植，不编译旧 C++”。三张 fixture 从同一 commit 的 `pdq/data` 复制，并保留来源路径和 SHA-256。

- [ ] **Step 4: 写官方 PDQ golden 失败测试**

通过 `image` 测试依赖读为 RGB24，再走本项目灰度与 PDQ：

```rust
#[test]
fn bridge_original_matches_meta_golden() {
    let result = pdq_fixture("bridge-original.jpg");
    assert_eq!(result.hash.to_hex(), "f8f8f0cee0f4a84f06370a22038f63f0b36e2ed596621e1d33e6b39c4e9c9b22");
    assert_eq!(result.quality, 100);
}
```

`blur-a-little.jpg` 期望 `f8f8f0cee0f4a84f06370a2a038f63f0b36e26d596621e1d33e6b39c4e9c9b22/100`；`small.jpg` 期望 `0007001f003f003f007f00ff00ff00ff01ff01ff01ff03ff03ff03ff03ff03ff/0`。

- [ ] **Step 5: 按上游阶段逐个移植 PDQ**

实现 64×64 降采样、16×64/16×16 变换、Torben 中位数、256 位阈值与 Quality。位序转换集中在 `PdqHash::from_upstream_words`：反向遍历 16 个 `u16`，每个按大端写入 32 字节；数据库和协议只使用该字节数组。

- [ ] **Step 6: 验证确定性、位序和汉明距离**

Run: `cargo test -p dedup-media pdq`

Expected: 三个官方 golden 逐位一致；相同输入汉明距离 0，翻转一个 bit 距离 1。

- [ ] **Step 7: 更新 AGENTS.md 并提交**

```powershell
git add -- AGENTS.md third_party/pdq crates/media
git commit -m "feat: port fixed PDQ image features to Rust"
```

### 任务 5：实现 9 分块 pHash、128 维 Sobel 和图片两层筛选

**Files:**
- Create: `crates/media/src/phash.rs`
- Create: `crates/media/src/sobel.rs`
- Create: `crates/media/src/image_score.rs`
- Create: `crates/media/tests/image_features.rs`
- Modify: `crates/media/src/lib.rs`
- Modify: `crates/core/src/model.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `ImageStage1`, `ImageStage2 { phash_parts: [u64; 9], sobel: [f32; 128] }`。
- Produces: `compute_image_stage2(&GrayImage) -> ImageStage2`、`screen_image_stage1`、`screen_image_stage2`。

- [ ] **Step 1: 写 pHash 位序失败测试**

构造固定 `96×96` 灰度渐变，断言九块顺序为行优先、BLOB 为 72 字节小端、bit `i` 对应左上 `8×8` DCT 的行优先系数。另断言平坦块使用“上中位数 + 严格大于”，不会因相等系数随机置位。

Run: `cargo test -p dedup-media phash`

Expected: FAIL，因为 pHash 尚不存在。

- [ ] **Step 2: 实现固定二维 DCT-II**

预计算 `cos((2x+1)uπ/64)` 的 `32×8` 表；系数固定乘 `0.25 * cu * cv`，零频 `1/sqrt(2)`。每块取包含 DC 的 64 系数，选择排序后索引 32 的上中位数，并用 `value > median` 设置 bit。

- [ ] **Step 3: 写 Sobel 零向量和方向失败测试**

```rust
#[test]
fn sobel_zero_vector_similarity_is_defined() {
    let z = [0.0_f32; 128];
    let mut nonzero = z;
    nonzero[0] = 1.0;
    assert_eq!(sobel_cosine(&z, &z), 1.0);
    assert_eq!(sobel_cosine(&z, &nonzero), 0.0);
}
```

另用水平/垂直边缘断言无符号 `[0,π)` 的 8 个硬 bin 和 `4×4` 空间格索引。

- [ ] **Step 4: 实现 Sobel**

输入固定缩放到 `128×128`；忽略一像素边界；幅值 `|gx|+|gy|`，小于 `1e-6` 跳过；每个像素只进入一个方向 bin；范数 `<=1e-9` 输出全零，否则 L2 归一化。

- [ ] **Step 5: 写两层筛选测试并实现**

一筛必须使用阈值快照的 `pdq_quality_min`、`aspect_tolerance`、`pdq_hamming_max`；二筛必须要求每块距离 `<=phash_part_hamming_max`、通过块数 `>=phash_min_passed_parts` 且 Sobel `>=sobel_min`。返回值包含一筛分数、通过块数和 Sobel 分数，便于持久化与 UI 解释。

- [ ] **Step 6: 写 PDQ band 候选索引测试**

将 32 字节 PDQ 分为四个连续 64 位大端 band；共享任一 band 才进入完整阈值检查。测试明确覆盖汉明 `4..31` 但不共享 band 时不保证召回，保持规格中的近似索引边界。

- [ ] **Step 7: 验证并提交**

Run: `cargo test -p dedup-media --test image_features`

Expected: pHash、Sobel、band 和联合筛选全部 PASS。

```powershell
git add -- AGENTS.md crates/core crates/media
git commit -m "feat: add pHash and Sobel joint screening"
```

### 任务 6：实现六帧视频评分和 JPG 联系表

**Files:**
- Create: `crates/media/src/video_score.rs`
- Create: `crates/media/src/contact_sheet.rs`
- Create: `crates/media/tests/video_features.rs`
- Modify: `crates/media/src/lib.rs`
- Modify: `crates/core/src/model.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `sample_positions(duration: Duration) -> [Duration; 6]`、`score_video_stage1`、`score_video_stage2`、`encode_contact_sheet`。
- Consumes: 任务 4/5 的 RGB24、PDQ、pHash、Sobel 和阈值快照。

- [ ] **Step 1: 写六个均匀槽位失败测试**

```rust
#[test]
fn samples_midpoints_of_six_equal_segments() {
    let p = sample_positions(Duration::from_secs(120));
    assert_eq!(p.map(|v| v.as_secs()), [10, 30, 50, 70, 90, 110]);
}
```

Run: `cargo test -p dedup-media video_score`

Expected: FAIL，因为视频评分模块尚不存在。

- [ ] **Step 2: 实现一筛有效帧语义**

同一槽位双方解码成功才进入分母；Quality、长宽比或 PDQ 不通过的有效帧计 0；解码失败槽位不进入分母。有效数低于 `video_min_valid_frames` 返回 `Incomplete`，否则平均并比较 `video_stage1_min`。

- [ ] **Step 3: 实现二筛有效帧语义**

pHash 未通过的有效帧计 0；通过时取 Sobel 余弦；有效数和平均阈值分别使用 `video_min_valid_frames`、`video_stage2_min`。缺失二筛特征返回 `Incomplete`，不把缺失当 0。

- [ ] **Step 4: 写联系表失败测试**

使用六张不同纯色 `2×2` RGB24 图，调用 `encode_contact_sheet(..., cell=2×2)`；解码结果必须为 `6×4`、三列两行、槽位按行优先，编码头为 JPEG，缺失槽位像素为固定 `#60656F`。

- [ ] **Step 5: 实现联系表**

只接收已抽取六槽位，不再请求解码；按保持长宽比居中绘制到统一单元格，画布 RGB24；使用 image JPEG encoder 质量 80，输出拥有所有权的 `Vec<u8>`。

- [ ] **Step 6: 验证并提交**

Run: `cargo test -p dedup-media`

Expected: 六帧位置、两层平均、失败槽位和 JPG 网格全部 PASS。

```powershell
git add -- AGENTS.md crates/core crates/media
git commit -m "feat: add six-frame video scoring and previews"
```

### 任务 7：创建 SQLite V2 schema、内容缓存和可靠 outbox

**Files:**
- Create: `crates/node-store/src/schema.sql`
- Create: `crates/node-store/src/open.rs`
- Create: `crates/node-store/src/content.rs`
- Create: `crates/node-store/src/features.rs`
- Create: `crates/node-store/src/outbox.rs`
- Create: `crates/node-store/src/snapshot.rs`
- Create: `crates/node-store/src/rows.rs`
- Create: `crates/node-store/tests/content_cache.rs`
- Create: `crates/node-store/tests/outbox.rs`
- Modify: `crates/node-store/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `NodeStore::open`、`NodeStore::open_in_memory`、`lookup_scanned_paths`、`upsert_content_and_location`、`load_complete_stage1`、`load_complete_stage2`、`commit_feature_result`、`pull_changes`、`ack_changes`、`begin_snapshot`。
- Consumes: 任务 2 的键和值对象、任务 3 的 `SyncChange`、任务 4–6 的特征类型。

- [ ] **Step 1: 写新库创建和旧库拒绝测试**

```rust
#[test]
fn creates_current_schema_only_for_new_database() {
    let store = NodeStore::open(&temp_db(), machine()).unwrap();
    assert_eq!(store.schema_id().unwrap(), "mysingerserver-rust-v2");
}

#[test]
fn rejects_database_without_v2_marker() {
    let path = sqlite_with_table("legacy_files");
    assert!(matches!(NodeStore::open(&path, machine()), Err(StoreError::IncompatibleSchema)));
}
```

Run: `cargo test -p dedup-node-store open`

Expected: FAIL，因为 schema 尚不存在。

- [ ] **Step 2: 编写完整 SQLite schema.sql**

必须一次创建：`metadata`、`files`、`contents`、`image_stage1`、`image_stage2`、`video_metadata`、`video_frame_stage1`、`video_frame_stage2`、`contact_sheets`、`tasks`、`task_items`、`task_scan_roots`、`analysis_runs`、`analysis_run_inputs`、`candidate_pairs`、`duplicate_groups`、`group_members`、`review_marks`、`sync_outbox`、`sync_state`、`delete_batches`、`delete_items`。

关键约束固定为：`contents UNIQUE(md5,file_size)`；`files PRIMARY KEY(machine_id,normalized_path)`；视频帧 `UNIQUE(content_id,slot)`；候选左右 `ContentKey` 以有序字节保存；组成员 `UNIQUE(analysis_run_id,group_id,machine_id,normalized_path)`；`sync_state` 单例初值 `acked_seq=0, pruned_through_seq=0`。

- [ ] **Step 3: 实现打开和单写者约定**

`NodeStore` 拥有一个 rusqlite `Connection`，初始化 `journal_mode=WAL`、`foreign_keys=ON`、`busy_timeout=5s`。它不实现 `Clone`/`Sync`；后续由 `NodeEngine` actor 独占，确保节点所有写事务串行。

- [ ] **Step 4: 写批量缓存命中失败测试**

```rust
#[test]
fn cache_key_is_machine_path_and_size_only() {
    let mut store = seeded_store();
    let hit = store.lookup_scanned_paths(&[scan(r"D:\a.jpg", 99)]).unwrap();
    assert!(hit[0].is_reusable());
    let miss = store.lookup_scanned_paths(&[scan(r"D:\a.jpg", 100)]).unwrap();
    assert!(!miss[0].is_reusable());
}
```

同一大小不读取 mtime；命中返回 MD5/内容引用。MD5 相同但大小不同必须创建不同 `contents` 行。

- [ ] **Step 5: 实现内容、位置和特征事务**

`upsert_content_and_location` 先按 MD5 索引查，再比较大小。`commit_feature_result` 在同一事务写 stage1/stage2、任务项结果和对应 `sync_outbox`。图片二筛完整条件是 9 个 pHash 与 128 个有限 `f32` 同时存在；视频槽位二筛同理。

- [ ] **Step 6: 写一筛完整性测试**

覆盖图片缺 Quality、视频不足六个槽位记录、视频六槽位中仅三帧成功均进入 `skipped_incomplete`；六槽位有四帧成功且每帧宽高/PDQ/Quality 完整才可读取。Store 不在查询时补算。

- [ ] **Step 7: 写 outbox ACK 边界失败测试**

```rust
#[test]
fn commit_then_ack_prunes_only_committed_rows() {
    let mut store = outbox_with_sequences(1..=3);
    store.ack_changes(2).unwrap();
    assert_eq!(store.sync_state().unwrap(), SyncState { acked_seq: 2, pruned_through_seq: 2 });
    assert_eq!(store.pull_changes(2, 1000).unwrap().sequences(), vec![3]);
}
```

另测重复 ACK 幂等、ACK 大于已存在最高序号只推进到实际已提交边界、中心游标 `< pruned_through_seq` 返回 `SnapshotRequired`。

- [ ] **Step 8: 实现只读快照**

`begin_snapshot` 开启 SQLite read transaction 并记录 `snapshot_high_seq`；分页顺序固定为表序 + 主键序；页面包含当前基础行和删除墓碑；连接中断丢弃事务并从头开始，不保存分块恢复状态。

- [ ] **Step 9: 验证并提交**

Run: `cargo test -p dedup-node-store --test content_cache --test outbox`

Expected: schema、缓存、完整性、同事务 outbox、ACK/清理和快照测试全部 PASS。

```powershell
git add -- AGENTS.md crates/node-store
git commit -m "feat: add SQLite V2 content and sync store"
```

### 任务 8：持久化任务、分析运行、候选、重复组、复核与删除状态

**Files:**
- Create: `crates/node-store/src/tasks.rs`
- Create: `crates/node-store/src/analysis.rs`
- Create: `crates/node-store/src/groups.rs`
- Create: `crates/node-store/src/review.rs`
- Create: `crates/node-store/src/delete.rs`
- Create: `crates/node-store/tests/task_recovery.rs`
- Create: `crates/node-store/tests/analysis_state.rs`
- Create: `crates/node-store/tests/delete_group_update.rs`
- Modify: `crates/node-store/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `create_task`、`claim_next_item`、`complete_item`、`recover_running_items`、`create_analysis_run`、`freeze_analysis_inputs`、`replace_candidates`、`replace_groups`、`page_groups`、`save_review_mark`、`apply_delete_results`。

- [ ] **Step 1: 写任务恢复失败测试**

创建含 `queued/running/succeeded/failed/cancelled` 项的数据库，重开后只把 `running` 改回 `queued`；完成项不得重新计算。单文件失败只增加统计，任务仍可在其余项结束后标记 `completed`。

- [ ] **Step 2: 实现任务状态写入**

任务状态只允许规格中的五值；状态转换集中在 `tasks.rs`。`complete_item` 同时分配单调 `event_seq`，持久化后事件才允许发送。

- [ ] **Step 3: 写 AnalysisRun 状态机失败测试**

覆盖确认链 `collecting_stage1 -> stage1_synced -> screening -> phase2_dispatched -> phase2_synced -> finalizing -> completed`；允许任意活动态到 `cancelled`；允许 `partial -> phase2_dispatched` 的显式重试；拒绝直接从 `screening` 跳 `completed`。

- [ ] **Step 4: 实现不可变输入快照**

`freeze_analysis_inputs(run_id, selected_task_ids)` 从所选已完成任务的 `TaskItem` 连接当前文件位置，去重后按 `(ContentKey,LocationKey)` 排序写 `analysis_run_inputs`。冻结完成后不接受增量追加；后续扫描只影响下一运行。

- [ ] **Step 5: 写稳定分页测试并实现**

组游标编码 `(group_kind, representative_content_key, group_id)`，成员游标编码 `(machine_id, normalized_path)`；相同数据库状态的多次分页顺序完全一致，删除一个成员后从新游标继续不重复。

- [ ] **Step 6: 写复核持久化测试并实现**

`save_review_mark(run,group,location,Undecided|Keep|Delete)` UPSERT 到 SQLite；重开 Store 后可恢复。创建删除批次前验证每组至少一个当前活动 `Keep`，只在此边界验证一次。

- [ ] **Step 7: 写删除后组更新失败测试**

```rust
#[test]
fn successful_delete_removes_member_and_small_group() {
    let mut store = group_with_two_members();
    store.apply_delete_results(batch_with_recycled_first()).unwrap();
    assert!(store.page_groups(run(), None, 20).unwrap().items.is_empty());
}
```

另测 `failed/skipped` 保留；三成员代表被删除时，删除计划中第一个明确 `Keep` 的活动文件成为代表；不重新筛选或扩组。

- [ ] **Step 8: 实现删除事务**

成功结果在一个事务中写 `delete_items`、位置非活动、墓碑、outbox、删除 `group_members`、必要时换代表或删除少于两个成员的组与复核标记。重复提交同一成功结果幂等。

- [ ] **Step 9: 验证并提交**

Run: `cargo test -p dedup-node-store --test task_recovery --test analysis_state --test delete_group_update`

Expected: 状态恢复、不可变输入、分页、复核和删除组更新全部 PASS。

```powershell
git add -- AGENTS.md crates/node-store
git commit -m "feat: persist tasks analyses groups and deletion"
```

### 任务 9：固定 FFmpeg DLL 供应链、动态加载与安全解码接口

**Files:**
- Create: `third_party/ffmpeg-dependency.json`
- Create: `scripts/fetch-ffmpeg.ps1`
- Create: `scripts/generate-ffmpeg-bindings.ps1`
- Create: `tests/fixtures/media/image.jpg`
- Create: `tests/fixtures/media/video-12s.mp4`
- Create: `tests/fixtures/media/manifest.json`
- Create: `crates/media-ffmpeg/wrapper.h`
- Create: `crates/media-ffmpeg/src/bindings_8_0_1.rs`
- Create: `crates/media-ffmpeg/src/ffi.rs`
- Create: `crates/media-ffmpeg/src/loader.rs`
- Create: `crates/media-ffmpeg/src/decode.rs`
- Create: `crates/media-ffmpeg/tests/loader_windows.rs`
- Create: `crates/media-ffmpeg/tests/decode_windows.rs`
- Modify: `crates/media-ffmpeg/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `Ffmpeg::load_from_worker_executable`、`MediaProbe`、`DecodedFrame`、`Ffmpeg::probe_media`、`Ffmpeg::decode_frame_at`。
- Produces: 固定 `runtime\ffmpeg` 中五个 DLL 和许可证；不产生任何运行时 EXE。

- [ ] **Step 1: 写依赖清单与下载脚本测试**

清单明确列出加载顺序：`avutil-60.dll`、`swresample-6.dll`、`swscale-9.dll`、`avcodec-62.dll`、`avformat-62.dll`。归档 SHA 使用全局固定值；允许输出只有这五个 DLL 与 `LICENSE.txt`，并拒绝复制三个 FFmpeg EXE、`avdevice-62.dll`、`avfilter-11.dll`。

Run: `pwsh -File scripts/fetch-ffmpeg.ps1 -Destination .tmp/ffmpeg-test -WhatIf`

Expected: 输出固定 URL、SHA、五个 DLL 和许可证，不产生文件。

- [ ] **Step 2: 实际下载并验证供应链**

Run: `pwsh -File scripts/fetch-ffmpeg.ps1 -Destination .tmp/ffmpeg-test`

Expected: SHA-256 完全匹配；`.tmp/ffmpeg-test/runtime/ffmpeg` 仅有五个允许 DLL；许可证存在；递归搜索 `ff*.exe` 返回空。

- [ ] **Step 3: 生成并固定 FFmpeg 8.0.1 bindings**

安装 `bindgen-cli 0.72.1` 与 LLVM 21.1.8 仅用于生成。`wrapper.h` 只 include `libavutil`、`libswscale`、`libavcodec`、`libavformat` 必需头；脚本使用 allowlist 生成结构体、枚举、常量，blocklist 全部函数，并把生成器版本、FFmpeg 归档 SHA 写入文件头。提交 `bindings_8_0_1.rs` 后普通构建不再依赖 LLVM 或头文件。

- [ ] **Step 4: 写 DLL 路径和搜索规则失败测试**

```rust
#[test]
fn loader_ignores_current_directory_and_path() {
    let worker = Path::new(r"C:\App\worker.exe");
    assert_eq!(dll_directory(worker).unwrap(), Path::new(r"C:\App\runtime\ffmpeg"));
}
```

真实 Windows 测试从 `DEDUP_FFMPEG_TEST_SOURCE_DIR` 复制五个 DLL 到临时的 `fixture\runtime\ffmpeg`，把全局 PATH 清空、切换当前目录，再以 `fixture\worker.exe` 作为固定路径基准加载；该变量只提供测试夹具来源，不改变生产搜索规则。缺任一五个 DLL 时必须在 Worker 启动阶段返回明确文件名。

- [ ] **Step 5: 实现受限动态加载**

调用 `SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32 | LOAD_LIBRARY_SEARCH_USER_DIRS)`、`AddDllDirectory(fixed_dir)`，再按清单顺序用相同 flags 的 `LoadLibraryExW`。`FfmpegApi` 保存全部 `HMODULE` 与函数指针，确保指针生命周期不超过库句柄；unsafe 只留在 `ffi.rs/loader.rs`。

- [ ] **Step 6: 写探测/定位/解码集成失败测试**

从旧项目已提交的 `testdata/videocore/compat/images/synthetic-pattern.jpg` 与 `testdata/videocore/compat/videos/h264-standard.mp4` 复制到新结构的独立夹具 `tests/fixtures/media/image.jpg` 与 `tests/fixtures/media/video-12s.mp4`；`manifest.json` 固定记录源路径与 SHA-256，仅作为可重复测试输入，不复用旧实现。图片探测应返回图片和尺寸；视频返回时长/尺寸；在 1/12 与 11/12 解码均输出紧凑 RGB24，像素长度等于 `width*height*3`。

- [ ] **Step 7: 实现安全 FFmpeg 解码**

动态解析并封装 `avformat_open_input`、`avformat_find_stream_info`、`av_find_best_stream`、codec context、packet/frame、`avformat_seek_file`、`avcodec_send_packet/receive_frame`、`sws_getContext/sws_scale` 及对应释放函数。RAII wrapper 分别拥有 format、codec、packet、frame、sws；`decode.rs` 以拥有所有权的 `DecodedFrame { width, height, rgb24 }` 离开 FFI crate。

- [ ] **Step 8: 验证并提交**

```powershell
$env:DEDUP_FFMPEG_TEST_SOURCE_DIR = (Resolve-Path '.tmp/ffmpeg-test/runtime/ffmpeg').Path
cargo test -p dedup-media-ffmpeg --target x86_64-pc-windows-msvc
```

Expected: loader、缺失 DLL、图片、视频、非当前目录启动全部 PASS。

```powershell
git add -- AGENTS.md third_party/ffmpeg-dependency.json scripts/fetch-ffmpeg.ps1 scripts/generate-ffmpeg-bindings.ps1 crates/media-ffmpeg tests/fixtures/media
git commit -m "feat: load FFmpeg 8 DLLs without executables"
```

### 任务 10：实现 Worker 可执行程序、匿名管道和可重启 Worker 池

**Files:**
- Create: `apps/worker/src/main.rs`
- Create: `crates/node-engine/src/worker/mod.rs`
- Create: `crates/node-engine/src/worker/process.rs`
- Create: `crates/node-engine/src/worker/pool.rs`
- Create: `crates/node-engine/src/worker/pipeline.rs`
- Create: `crates/windows/src/job.rs`
- Create: `crates/node-engine/tests/worker_pool.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Modify: `crates/windows/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `WorkerPool::start`、`WorkerPool::dispatch`、`WorkerPool::cancel_task`、`WorkerPool::prepare_planned_restart`、`WorkerPool::restart_after_requeue`、`WorkerEvent`。
- Consumes: 任务 3 的 `WorkerEnvelope`、任务 4–6 的算法、任务 9 的 `Ffmpeg`。

- [ ] **Step 1: 写 Worker pipeline 失败测试**

通过 `MediaDecoder` 测试实现提供固定 RGB24：`ProbeAndStage1` 一次解码产生媒体元数据、PDQ/Quality 和六帧槽位；`ComputeStage2` 一次解码同时产生 pHash 与 Sobel；图片不产生缩略图。

Run: `cargo test -p dedup-node-engine worker::pipeline`

Expected: FAIL，因为 pipeline 尚不存在。

- [ ] **Step 2: 实现 Worker 主循环**

Worker 启动先按固定路径加载 FFmpeg，成功后在 stdout 发送 `WorkerReady`；stdin/stdout 都使用任务 3 的 4 字节 Protobuf 分帧。日志写 `data/node/logs/worker-<pid>.log`，stdout 禁止输出非协议文本。

- [ ] **Step 3: 实现媒体 pipeline**

`MediaDecoder` trait 只有 `FfmpegDecoder` 生产实现和测试 fake。图片 stage1 解码一次；视频按六位置解码并保存每槽成功/失败记录，联系表复用成功帧；stage2 只计算请求内容，图片/每个成功视频帧在一次灰度转换中共同得到 pHash + Sobel。

- [ ] **Step 4: 写 Job Object 失败测试并实现**

创建 `KILL_ON_JOB_CLOSE` Job，把每个 Worker 进程加入。测试关闭 Job 后测试子进程在限定时间内退出；启动 flags 包含 `CREATE_NO_WINDOW`。

- [ ] **Step 5: 写计划重启与崩溃差异测试**

```rust
#[tokio::test]
async fn planned_restart_requeues_without_failure() {
    let mut pool = fake_pool_with_running_item();
    let mut store = fake_store_with_running_item();
    let running = pool.prepare_planned_restart().unwrap();
    assert_eq!(running, vec![item_id()]);
    store.requeue_items(&running).unwrap();
    pool.restart_after_requeue(&running).await.unwrap();
    assert_eq!(pool.failure_count(), 0);
    assert_eq!(store.item_state(item_id()), ItemState::Queued);
}
```

另测意外退出把当前项标记 `failed` 并补建 Worker；取消任务把等待项取消并终止/替换正在处理该任务的 Worker。

- [ ] **Step 6: 实现 WorkerPool 状态顺序**

计划重启固定为两段 API：`prepare_planned_restart` 把池置为 `restarting` 并返回运行项，但不终止进程；`NodeEngine` 先在 SQLite 事务把这些项改回 `queued`，再调用 `restart_after_requeue` 记录预期退出 PID、终止进程、补建并等待 Ready。WorkerPool 不直接访问 SQLite；意外退出路径不得命中“预期退出”集合。

- [ ] **Step 7: 运行进程级测试并提交**

Run: `cargo test -p dedup-node-engine --test worker_pool`

Expected: Ready、任务结果、崩溃替换、取消、计划重启和 Job 清理全部 PASS。

```powershell
git add -- AGENTS.md apps/worker crates/node-engine crates/windows
git commit -m "feat: add isolated Rust worker pool"
```

### 任务 11：实现文件枚举、扫描缓存、MD5 与一筛计算任务

**Files:**
- Create: `crates/windows/src/walker.rs`
- Create: `crates/node-engine/src/scan/mod.rs`
- Create: `crates/node-engine/src/scan/enumerator.rs`
- Create: `crates/node-engine/src/scan/everything.rs`
- Create: `crates/node-engine/src/scan/hash.rs`
- Create: `crates/node-engine/src/scan/engine.rs`
- Create: `crates/node-engine/tests/scan_cache.rs`
- Create: `crates/node-engine/tests/scan_roots.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Modify: `crates/node-store/src/tasks.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `FileEnumerator::enumerate`（`WindowsWalker` 与 `EverythingEnumerator`）、`ScanEngine::run`、`ScanOptions { roots, force_recompute }`。
- Consumes: `NodeStore`、`WorkerPool`、`MachineId`、规范路径、MD5。

- [ ] **Step 1: 写两个枚举器的共同契约测试**

临时目录含嵌套图片、视频、普通文件和不可匹配扩展；两个枚举器都输出规范路径、显示路径、大小并按规范路径稳定排序。Everything 测试只在本机 Everything IPC 可用时运行；不可用时返回单一明确错误，不在任务中切换 Walker。

- [ ] **Step 2: 实现 WindowsWalker 和 EverythingEnumerator**

Walker 使用 Windows 文件系统 API 递归；Everything 使用 `everything-ipc 0.1.4` 按每个根查询文件并在 Rust 端做组件边界确认。配置值只允许 `windows_walker` 或 `everything`。

- [ ] **Step 3: 写批量跳过 MD5 失败测试**

第一次扫描记录文件；第二次相同机器/路径/大小使用可计数 Reader，断言 MD5 读取次数为 0；改变大小读取一次；同大小替换普通扫描仍复用；`force_recompute=true` 必须读取并更新内容引用。

- [ ] **Step 4: 实现扫描短路径**

每批 1000 个 `ScannedPath` 查询 SQLite；命中直接完成 TaskItem。未命中流式计算 MD5，按 MD5 索引再比较大小；已有内容只添加/更新路径并复用元数据/特征；仅新 ContentKey 调度探测和 stage1。

- [ ] **Step 5: 写不完整内容不自动补算测试**

预置相同 MD5+大小但 stage1 缺失的内容，普通扫描只复用并计 `skipped_incomplete`，Worker 调用数为 0；显式“重试失败项”或强制重算才调用 Worker。

- [ ] **Step 6: 写扫描根失效边界测试**

```rust
#[tokio::test]
async fn successful_partial_root_scan_does_not_deactivate_other_roots() {
    let engine = seeded_engine([r"D:\A\old.jpg", r"D:\B\keep.jpg"]);
    engine.scan([r"D:\A"]).await.unwrap();
    assert!(!engine.is_active(r"D:\A\old.jpg"));
    assert!(engine.is_active(r"D:\B\keep.jpg"));
}
```

任务失败或取消时两个旧位置都保持活动；`D:\A` 不得影响 `D:\AB`。

- [ ] **Step 7: 实现完成事务和高水位**

只有枚举与全部可继续步骤结束后才按持久化 scan roots 失效缺失路径。任务完成响应读取事务提交后的 `outbox_high_seq`；所有文件级失败留在统计但任务状态为 `completed`。

- [ ] **Step 8: 验证并提交**

Run: `cargo test -p dedup-node-engine --test scan_cache --test scan_roots`

Expected: 枚举、缓存、MD5、内容复用、显式重试、根失效和高水位全部 PASS。

```powershell
git add -- AGENTS.md crates/windows crates/node-engine crates/node-store
git commit -m "feat: add cached scan and stage-one tasks"
```

### 任务 12：实现纯 SQLite 本地精确/相似分析和代表文件分组

**Files:**
- Create: `crates/node-engine/src/analysis/mod.rs`
- Create: `crates/node-engine/src/analysis/exact.rs`
- Create: `crates/node-engine/src/analysis/image.rs`
- Create: `crates/node-engine/src/analysis/video.rs`
- Create: `crates/node-engine/src/analysis/phase2.rs`
- Create: `crates/node-engine/src/analysis/grouping.rs`
- Create: `crates/core/src/grouping.rs`
- Create: `crates/node-engine/tests/local_analysis.rs`
- Create: `crates/node-engine/tests/representative_grouping.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Modify: `crates/node-store/src/analysis.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `LocalAnalysisEngine::start`、`LocalAnalysisEngine::retry_phase2`，以及供本地/中心共用的 `dedup_core::group_by_representative`。
- Consumes: 不可变 `analysis_run_inputs`、阈值快照、SQLite 特征、WorkerPool。

- [ ] **Step 1: 写筛选门禁失败测试**

本节点存在 `queued/running` 扫描或 stage1 任务时 `start` 返回 `AnalysisBlocked::ComputationRunning`；任务级 failed/cancelled 返回需重试/重新选择；只有 completed（允许文件级失败）才冻结输入并进入 `stage1_synced`。

- [ ] **Step 2: 写精确重复测试并实现**

按 MD5 索引、同 MD5 内按大小分组；至少两个活动位置形成组。相同内容的本机多路径全部保留为成员；不计算额外哈希。

- [ ] **Step 3: 写完整性与一筛测试**

图片缺任一宽/高/PDQ/Quality、视频缺六槽尝试记录或有效成功帧不足阈值时直接增加运行的 `skipped_incomplete`。完整图片使用 PDQ band；完整视频使用六槽 band 并集。所有候选完整持久化后才进入 `phase2_dispatched`。

- [ ] **Step 4: 写二筛复用失败测试**

候选两端 SQLite 都有完整 stage2 时 Worker 调用为 0；一端缺失时只对缺失 ContentKey 派发一次；同内容多路径不重复计算；失败候选保持 unresolved 并令运行 `partial`。

- [ ] **Step 5: 实现批量二筛和完成门禁**

先完成并提交全量一筛候选，再按 ContentKey 排序批量派发。每个结果事务写 stage2 + outbox。所有派发任务进入终态后才执行最终筛选；缺失结果不按 0 分。显式 retry 只重派 unresolved，并从 `partial` 回 `phase2_dispatched`。

- [ ] **Step 6: 写代表中心分组失败测试**

构造 A≈B、B≈C、A≉C，ContentKey 顺序 A<B<C；结果必须为 `[A,B]`，C 不经 B 链式加入。另测每个 ContentKey 只进一个组、代表位置按机器/路径升序、纯同 ContentKey 多路径只出现在精确组、少于两个不同 ContentKey 不创建相似组。

- [ ] **Step 7: 实现确定性 grouping**

在 `dedup-core/src/grouping.rs` 实现纯函数：遍历未分组 ContentKey；每个代表只查询与它直接通过最终二筛的未分组内容；成员保存相对代表的一筛分数、pHash 通过块数、Sobel/视频平均分。最终组与成员一次事务替换，顺序与分页稳定。

- [ ] **Step 8: 写本地恢复和分页测试**

分析完成后关闭/重开 SQLite；通过 store API 分页读回运行、精确组、图片组、视频组、成员和复核标记，管理工具不直接打开 SQLite。

- [ ] **Step 9: 验证并提交**

Run: `cargo test -p dedup-node-engine --test local_analysis --test representative_grouping`

Expected: 本地纯 SQLite 的门禁、两层筛选、二筛复用、partial 重试、确定性分组和恢复全部 PASS。

```powershell
git add -- AGENTS.md crates/core crates/node-engine crates/node-store
git commit -m "feat: complete local SQLite dedup analysis"
```

### 任务 13：组合 node.exe、单管理连接、托盘、预览和安全删除

**Files:**
- Create: `apps/node/build.rs`
- Create: `apps/node/src/main.rs`
- Create: `apps/node/ui/tray.slint`
- Create: `apps/node/assets/tray-icon.png`
- Create: `crates/node-engine/src/actor.rs`
- Create: `crates/node-engine/src/server.rs`
- Create: `crates/node-engine/src/preview.rs`
- Create: `crates/node-engine/src/delete.rs`
- Create: `crates/core/src/logging.rs`
- Create: `crates/windows/src/recycle.rs`
- Create: `crates/windows/src/shell.rs`
- Create: `crates/node-engine/tests/node_server.rs`
- Create: `crates/node-engine/tests/delete.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Modify: `crates/windows/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `NodeRuntime::start`、`NodeEngineHandle`、`NodeServer::serve`、协议全部节点端 handler。
- Consumes: `NodeStore`、`WorkerPool`、本地分析、transport、Windows 回收站。

- [ ] **Step 1: 写单管理连接失败测试**

启动 loopback 节点，第一条连接完成 Hello；第二条连接收到 `ErrorCode::NodeBusy` 后关闭；第一条断开后第三条可连接。节点仍能同时服务第一管理连接上的并发请求。

Run: `cargo test -p dedup-node-engine --test node_server single_manager`

Expected: FAIL，因为服务器尚不存在。

- [ ] **Step 2: 实现 NodeEngine actor**

actor 独占 `NodeStore` 与 `WorkerPool`，接收有界 `EngineCommand`，串行执行数据库写入并把耗时媒体工作派发 Worker。网络 handler 只做一次 Protobuf→强类型转换；不直接持有 SQLite。启动入口依赖 `IdentityProvider`：生产仅使用 `SmbiosIdentityProvider`，测试使用 `FixedIdentityProvider`；生产配置结构中不提供 MachineId 注入字段。

- [ ] **Step 3: 实现节点协议 handler**

覆盖状态、任务、浏览、扫描、本地分析、组/成员分页、复核、二筛、增量/快照同步、原图/联系表分块和删除。普通响应不超过 8 MiB；文件每块最多 1 MiB；事件从已持久化的 `event_seq` 发出。

- [ ] **Step 4: 写预览测试并实现**

图片预览读取原文件并分块，不写任何图片缩略图缓存；视频只读取 `contact_sheets` 已缓存 JPG。请求路径必须是当前活动 `LocationKey`；节点离线行为由管理端控制。

- [ ] **Step 5: 写永久删除身份测试**

临时文件大小或 MD5 与计划不同返回 `skipped` 且文件存在；都相同且模式为 Permanent 返回 `deleted`，文件消失，SQLite 位置非活动且组立即更新。失败/跳过仍在组中。

- [ ] **Step 6: 实现回收站和永久删除**

每项执行顺序固定为存在→大小→流式 MD5→删除。回收站在短生命周期的 STA COM 线程中使用 Windows `IFileOperation` 并设置允许撤销；永久删除使用 `std::fs::remove_file`。边界层把结果映射为四个固定状态，成功后调用任务 8 的单事务更新。

- [ ] **Step 7: 写节点配置、日志与托盘 UI**

`node.exe` Release 使用 Windows 子系统而无控制台。首次启动写 `data/node/config.toml` 默认监听 `127.0.0.1:39091`、Worker 数为可用并行度、枚举器 `windows_walker`。滚动日志为 20 MiB × 10。

节点启动时先读取 SMBIOS 并计算 MachineId；三个物理字段全空则在日志中写明原因后退出，配置文件不保存机器 ID。托盘使用 Slint `SystemTrayIcon`，图标从现有 `nodetray/build/appicon.png` 复制为新路径中的独立资产；菜单固定包含状态/地址、打开日志目录、重启计算引擎、退出节点。“重启”由 NodeEngine 严格执行“prepare → SQLite requeue transaction → terminate/recreate”路径；“退出”停止 listener、等待 Store 提交、关闭 Job 后退出事件循环。

`dedup-core::logging::SizeRotatingWriter` 在写入边界按 20 MiB 轮转并只保留 10 个文件；node 与 worker 使用同一实现但不同文件名前缀。

- [ ] **Step 8: 写托盘回调的无 GUI 状态测试**

把菜单回调映射到 `TrayCommand`，单测 Restart 只重建 Worker、Exit 触发一次有序关闭、OpenLogs 使用 `data/node/logs` 绝对路径。实际图标/右键行为留给任务 20 的 computer-use 验收。

- [ ] **Step 9: 验证并提交**

Run: `cargo test -p dedup-node-engine --test node_server --test delete`

Run: `cargo test -p dedup-windows`

Expected: 单连接、handler、分块、预览、永久删除、组更新和托盘命令全部 PASS。

```powershell
git add -- AGENTS.md apps/node crates/node-engine crates/windows
git commit -m "feat: assemble tray node and safe deletion"
```

### 任务 14：提供手动 PostgreSQL schema 和中心数据访问

**Files:**
- Create: `deploy/central-v2.sql`
- Create: `deploy/rust-v2-test-compose.yml`
- Create: `crates/desktop-core/src/central/mod.rs`
- Create: `crates/desktop-core/src/central/schema.rs`
- Create: `crates/desktop-core/src/central/content.rs`
- Create: `crates/desktop-core/src/central/analysis.rs`
- Create: `crates/desktop-core/src/central/delete.rs`
- Create: `crates/desktop-core/tests/central_schema.rs`
- Create: `crates/desktop-core/tests/central_store.rs`
- Modify: `crates/desktop-core/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `CentralStore::connect`、`validate_schema`、`apply_sync_batch`、`create_analysis_run`、`insert_analysis_inputs`、中心候选/组/复核/删除查询。
- Consumes: 外部 `ContentKey`/`LocationKey`，内部自行分配中心 `content_id`。

- [ ] **Step 1: 编写完整 central-v2.sql**

脚本只面向空库并创建 `schema_metadata`、`nodes`、`sync_cursors`、`contents`、`file_locations`、图片/视频 stage1/stage2、删除墓碑、`analysis_runs`、`analysis_run_nodes`、`analysis_run_inputs`、`candidate_pairs`、`duplicate_groups`、`group_members`、`review_marks`、`delete_batches`、`delete_items`。

关键约束与 SQLite 对齐：`contents UNIQUE(md5,file_size)`；所有跨边界引用使用内容键/位置键；`analysis_run_inputs` 在运行内唯一且不可更新；候选左右键规范排序；中心游标按 machine_id 唯一。

- [ ] **Step 2: 写“桌面不建表”失败测试**

连接空 PostgreSQL 后 `CentralStore::connect` 返回 `CentralError::SchemaMissing { script: "schema/central-v2.sql" }`，查询系统目录确认没有创建业务表。所有依赖真实 PostgreSQL 的测试使用 `#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]`，普通 workspace 门禁不隐式依赖 Docker。

- [ ] **Step 3: 启动测试 PostgreSQL 并手动执行 SQL**

`deploy/rust-v2-test-compose.yml` 固定映射 `127.0.0.1:15439`，数据库/用户/密码为 `dedup_v2` / `dedup` / `dedup`，仅服务本计划的隔离测试。

Run: `docker compose -f deploy/rust-v2-test-compose.yml up -d`

```powershell
$env:DEDUP_TEST_POSTGRES_URL = 'postgresql://dedup:dedup@127.0.0.1:15439/dedup_v2'
psql $env:DEDUP_TEST_POSTGRES_URL -v ON_ERROR_STOP=1 -f deploy/central-v2.sql
```

Expected: 脚本一次执行成功；第二次执行明确失败，证明它不是隐式迁移器。

- [ ] **Step 4: 实现 schema 校验**

只读 `schema_metadata` 和 `information_schema.columns`，验证固定表/列；不执行 DDL。失败时中心模式禁用，但本地节点功能仍可用。

- [ ] **Step 5: 写外部键 UPSERT 测试**

同步两个机器的相同 `ContentKey`，中心只有一条 contents、两个 location；相同 MD5 不同大小生成两条 contents。任何公开 `CentralStore` 方法不得要求节点本地 `content_id`。

- [ ] **Step 6: 实现中心事务和稳定分页**

`apply_sync_batch` 在单事务按依赖顺序 UPSERT 内容、位置、媒体/特征、墓碑并推进 cursor。分析/组/复核/删除 SQL 使用任务 8 相同游标排序和状态语义。

- [ ] **Step 7: 验证并提交**

```powershell
$env:DEDUP_TEST_POSTGRES_URL = 'postgresql://dedup:dedup@127.0.0.1:15439/dedup_v2'
cargo test -p dedup-desktop-core --test central_schema --test central_store -- --ignored --test-threads=1
```

Expected: 手动 schema、只读校验、外部键映射、事务和分页全部 PASS。

```powershell
git add -- AGENTS.md deploy/central-v2.sql deploy/rust-v2-test-compose.yml crates/desktop-core
git commit -m "feat: add manual PostgreSQL central store"
```

### 任务 15：实现每批 1000 条的自动/手动同步、ACK 清理和全量快照

**Files:**
- Create: `crates/desktop-core/src/node_session.rs`
- Create: `crates/desktop-core/src/sync.rs`
- Create: `crates/desktop-core/tests/sync_batches.rs`
- Create: `crates/desktop-core/tests/sync_snapshot.rs`
- Modify: `crates/desktop-core/src/lib.rs`
- Modify: `crates/node-engine/src/server.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `NodeSession::connect`、`SyncEngine::sync_node`、`SyncTrigger::Automatic|Manual`、同步进度模型。
- Consumes: 节点 `PullChanges/SyncAck/Snapshot` 协议和 `CentralStore::apply_sync_batch`。

- [ ] **Step 1: 写 2501 条批次失败测试**

fake 节点提供序号 1..=2501，期望 PG 事务批大小依次 `[1000,1000,501]`，ACK 序号 `[1000,2000,2501]`；Automatic 与 Manual 调用同一个 `sync_node`，不得有两套逻辑。

Run: `cargo test -p dedup-desktop-core --test sync_batches`

Expected: FAIL，因为 SyncEngine 尚不存在。

- [ ] **Step 2: 实现节点会话与重连边界**

会话建立时 Hello 校验同一 V2 协议；一个 `desktop.exe` 为每个手工 `IP:port` 保持一条连接。断线结束当前请求并按固定间隔重连；任务不在客户端重建或重试。

- [ ] **Step 3: 实现先 ACK 中心游标**

每轮先读 PG `center_cursor` 并发 `SyncAck(center_cursor)`，闭合“PG 已提交但 ACK 丢失”。随后只拉 `seq > center_cursor`；PG 事务提交后才 ACK 新序号；ACK 失败则停止该轮，重连后幂等恢复。

- [ ] **Step 4: 写提交中断测试**

在 PG 事务提交前断开：cursor 不推进、节点不收到 ACK、重连重放同批。PG 提交后 ACK 前断开：重连第一步 ACK PG cursor，节点清理旧行且不重复写业务结果。

- [ ] **Step 5: 写 SnapshotRequired 失败测试**

中心 cursor=5、节点 `pruned_through_seq=8` 时增量返回 `SNAPSHOT_REQUIRED`；快照页写入同一 PG 事务，提交时 cursor=`snapshot_high_seq`，随后 ACK 并从更大序号继续增量。

- [ ] **Step 6: 实现快照整次重来**

快照固定按表/主键分页，低优先级传输；连接中断回滚 PG snapshot transaction，并丢弃 token，下一连接重新 BeginSnapshot，不增加页级恢复状态。

自动同步只有三个固定触发点：节点连接成功、任务完成事件、每 5 秒一次的追赶检查；每个节点同一时间最多运行一个同步循环。手动“立即同步”只向同一触发通道排队，不建立第二套同步路径。

- [ ] **Step 7: 覆盖同步数据范围**

测试位置/活动状态、内容、元数据、图片/视频 stage1+stage2、删除结果/墓碑均同步；原媒体和联系表 JPG 永不写 PG。

- [ ] **Step 8: 验证并提交**

Run: `cargo test -p dedup-desktop-core --test sync_batches --test sync_snapshot -- --test-threads=1`

Expected: 1000 批次、ACK 丢失、事务回滚、快照和数据范围全部 PASS。

```powershell
git add -- AGENTS.md crates/desktop-core crates/node-engine
git commit -m "feat: synchronize SQLite to PostgreSQL reliably"
```

### 任务 16：实现固定高水位的跨机器两层去重编排

**Files:**
- Create: `crates/desktop-core/src/analysis/mod.rs`
- Create: `crates/desktop-core/src/analysis/gate.rs`
- Create: `crates/desktop-core/src/analysis/screen.rs`
- Create: `crates/desktop-core/src/analysis/dispatch.rs`
- Create: `crates/desktop-core/src/analysis/finalize.rs`
- Create: `crates/desktop-core/tests/cross_analysis.rs`
- Create: `crates/desktop-core/tests/cross_phase2.rs`
- Modify: `crates/desktop-core/src/lib.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `CrossAnalysisCoordinator::start`、`poll`、`retry_unresolved`。
- Consumes: 多个 `NodeSession`、`CentralStore`、`SyncEngine`、共享 `group_by_representative`。

- [ ] **Step 1: 写 stage1 双门禁失败测试**

三个选定节点中任一相关任务 queued/running、failed/cancelled，或任一 PG cursor 未达该任务 `outbox_high_seq`，状态都停在 `collecting_stage1` 且不执行一筛。全部 task completed 且 cursor 达标后才进入 `stage1_synced`。

- [ ] **Step 2: 实现不可变中心输入**

协调器向每节点分页读取由所选 TaskItem 生成的 `(ContentKey,LocationKey)`，在一个 PG 事务写 `analysis_run_inputs` 并封存。随后自动同步的新位置不进入本运行；当前活动状态只控制操作可用性。

- [ ] **Step 3: 写中心精确与相似一筛测试**

精确按 MD5→大小；相似只连接运行输入和完整 stage1。图片用四 band，视频用六槽 band 并集；完整候选集合先提交到 PG，提交前 `DispatchStage2` 调用数为 0。

- [ ] **Step 4: 写两级二筛缓存复用测试**

PG 已有完整 stage2：不发节点请求。PG 缺失但所选节点 SQLite 已有：节点返回复用并补 outbox，不启动 Worker。两边都缺失：按在线且有活动位置的节点分组，一筛全部结束后批量派发。

- [ ] **Step 5: 实现节点选择和批量派发**

同一内容多节点持有时按“在线优先，然后 MachineId/路径升序”选择一个。每个节点一个有界批次列表；任务完成返回新 `outbox_high_seq`。不在一筛过程中逐对派发。

- [ ] **Step 6: 写 phase2 高水位失败测试**

任一已派发任务还在 queued/running 或 PG cursor 未达新的 highwater 时不得最终筛选。任务终态但候选特征缺失时运行 `partial`，unresolved 保留且不按 0 分；显式 retry 回 `phase2_dispatched` 并只派缺失内容。

- [ ] **Step 7: 复用共享代表分组并更新中心结果**

中心 finalization 调用任务 12 已实现的 `dedup-core` 纯函数，确保本地/跨机器 A-B-C 行为、ContentKey 唯一性和排序完全相同。最终组、成员、直接分数一次事务替换。

- [ ] **Step 8: 验证并提交**

Run: `cargo test -p dedup-desktop-core --test cross_analysis --test cross_phase2 -- --test-threads=1`

Expected: 多节点门禁、输入冻结、批量派发、两级复用、phase2 高水位、partial 重试和代表分组全部 PASS。

```powershell
git add -- AGENTS.md crates/core crates/desktop-core
git commit -m "feat: orchestrate cross-machine dedup analysis"
```

### 任务 17：实现 Slint 桌面外壳、节点、扫描、设置与诊断界面

**Files:**
- Create: `apps/desktop/build.rs`
- Create: `apps/desktop/src/main.rs`
- Create: `crates/desktop-ui/build.rs`
- Create: `crates/desktop-ui/src/lib.rs`
- Create: `crates/desktop-ui/src/models.rs`
- Create: `crates/desktop-ui/src/bindings.rs`
- Create: `crates/desktop-ui/ui/app.slint`
- Create: `crates/desktop-ui/ui/theme.slint`
- Create: `crates/desktop-ui/ui/components/navigation.slint`
- Create: `crates/desktop-ui/ui/components/status-pill.slint`
- Create: `crates/desktop-ui/ui/pages/overview.slint`
- Create: `crates/desktop-ui/ui/pages/scan-tasks.slint`
- Create: `crates/desktop-ui/ui/pages/settings-diagnostics.slint`
- Create: `crates/desktop-core/src/app.rs`
- Create: `crates/desktop-core/src/view_state.rs`
- Create: `crates/desktop-core/tests/view_state.rs`
- Modify: `crates/desktop-core/src/lib.rs`
- Modify: `crates/core/src/logging.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `DesktopApp` 命令通道、Slint `MainWindow`、节点/任务/设置 view models。
- Consumes: 节点会话、同步/分析命令和 `data/desktop/config.toml`。

- [ ] **Step 1: 写纯状态模型失败测试**

覆盖手工增加/编辑/移除 `IP:port`、连接状态、任务进度、计算中禁用筛选、PG schema 缺失仅禁用中心模式、阈值无效阻止启动、默认删除模式 RecycleBin。

Run: `cargo test -p dedup-desktop-core --test view_state`

Expected: FAIL，因为 app/view state 尚不存在。

- [ ] **Step 2: 实现 DesktopApp 命令/事件单向流**

Slint callback 只发送 `UiCommand`；异步 core 处理后发 `UiEvent`；UI 线程只应用 immutable view model。UI 不直接调用 TCP、SQLite、PostgreSQL 或 FFmpeg。

- [ ] **Step 3: 建立主题和导航**

按六张已确认预览图实现深色中性背景、低干扰强调色、左侧导航、顶部运行状态、卡片/表格/空状态。页面固定为总览、扫描任务、精确重复、相似图片、相似视频、跨机器、删除复核、设置诊断。

- [ ] **Step 4: 实现总览与节点页**

显示手工节点列表、在线/离线/忙、监听地址、Worker/任务统计、最后同步游标；提供连接、编辑、移除和立即同步。多节点连接并行，但每节点只有一个 session。

- [ ] **Step 5: 实现扫描与任务页**

路径浏览通过节点协议；显示根、枚举器、强制重算、创建/取消/失败项重试、逐文件进度与 `skipped_incomplete`。相关计算 queued/running 时“开始筛选”禁用并显示原因。

- [ ] **Step 6: 实现设置与诊断页**

编辑 PostgreSQL NoTls 连接、九个匹配阈值、删除模式、节点重连间隔；保存前只做一次边界校验。显示数据/日志/缓存绝对路径、PG schema 状态和可信局域网明文警告。

同页提供顶层可访问“关于”入口并嵌入 Slint `AboutSlint`，满足 Slint Royalty-free 2.0 桌面归属要求；发布包同时携带该许可证文本。

desktop 首次启动只在可执行文件旁创建 `data/desktop/config.toml`、`data/desktop/cache` 与 `data/desktop/logs`，默认节点为 `127.0.0.1:39091`；复用 `dedup-core::logging::SizeRotatingWriter`，桌面日志同样固定为 20 MiB × 10。

- [ ] **Step 7: 编译 Slint 并验证模型**

Run: `cargo test -p dedup-desktop-core -p dedup-desktop-ui`

Run: `cargo build -p desktop --target x86_64-pc-windows-msvc`

Expected: 状态测试 PASS，Slint 编译无错误，desktop.exe 生成。

- [ ] **Step 8: 更新架构文档并提交**

```powershell
git add -- AGENTS.md apps/desktop crates/desktop-core crates/desktop-ui
git commit -m "feat: add Slint desktop control interface"
```

### 任务 18：实现结果分页、按需预览、复核与删除交互

**Files:**
- Create: `crates/desktop-ui/ui/components/group-table.slint`
- Create: `crates/desktop-ui/ui/components/member-list.slint`
- Create: `crates/desktop-ui/ui/components/score-panel.slint`
- Create: `crates/desktop-ui/ui/components/delete-dialog.slint`
- Create: `crates/desktop-ui/ui/pages/exact-cross-machine.slint`
- Create: `crates/desktop-ui/ui/pages/similar-media.slint`
- Create: `crates/desktop-ui/ui/pages/review-delete.slint`
- Create: `crates/desktop-core/src/results.rs`
- Create: `crates/desktop-core/src/review.rs`
- Create: `crates/desktop-core/src/delete.rs`
- Create: `crates/desktop-core/tests/review_delete.rs`
- Modify: `crates/desktop-ui/ui/app.slint`
- Modify: `crates/desktop-ui/src/bindings.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: 本地/中心统一 `GroupView`、`MemberView`、稳定游标加载、复核命令、删除计划/进度模型。

- [ ] **Step 1: 写本地/中心同模型分页测试**

本地数据从 NodeSession，中心数据从 CentralStore；两者映射为同一 `GroupPage`。滚动加载只保留有限窗口，不一次物化百万行；游标不使用 offset。

- [ ] **Step 2: 实现精确与跨机器页**

精确组显示 MD5/大小、机器分布和位置；跨机器页显示运行状态链、节点 task/highwater/cursor、未解决候选和显式 retry。节点计算或同步未过门禁时不显示可点击“最终筛选”。

- [ ] **Step 3: 实现相似图片/视频页**

显示代表、成员、一筛分数、pHash 通过块、Sobel 或视频平均分。图片只有选中时读取原文件到内存，不写缩略图；视频按需拉联系表 JPG。离线位置仍显示记录与分数，但预览、打开和删除禁用。

- [ ] **Step 4: 写复核持久化失败测试**

本地标记调用节点 SQLite，中心标记调用 PG；重启 view state 后恢复。按大小/分辨率/Quality/路径快捷选择只更新标记，不生成删除请求。

- [ ] **Step 5: 实现删除确认模型**

确认对话固定显示文件数、节点数、预计释放空间、RecycleBin/Permanent、每组 Keep。任何组无活动 Keep 时按钮禁用；Permanent 使用明显警示但不增加二次口令。

- [ ] **Step 6: 写混合删除结果测试**

一组返回 recycled/failed/skipped：只移除 recycled；失败/跳过仍显示并可经用户重新确认后重试；少于两个成员的组从列表消失；代表删除后使用明确 Keep 的第一活动成员。本地结果由节点事务立即更新 SQLite；中心结果在响应成功后立即调用 PostgreSQL `apply_delete_results`，后续同步到达的相同 tombstone 必须幂等，不得重复改变组或统计。

- [ ] **Step 7: 构建 UI 并提交**

Run: `cargo test -p dedup-desktop-core --test review_delete`

Run: `cargo build -p desktop --release --target x86_64-pc-windows-msvc`

Expected: 分页、离线动作、复核、确认和混合结果测试 PASS；Slint 页面全部编译。

```powershell
git add -- AGENTS.md crates/desktop-core crates/desktop-ui
git commit -m "feat: add review preview and deletion workflows"
```

### 任务 19：构建 x64 便携发布包、许可证与 PostgreSQL 建库脚本

**Files:**
- Create: `scripts/build-release.ps1`
- Create: `scripts/verify-release.ps1`
- Create: `scripts/generate-third-party-notices.ps1`
- Create: `tests/windows/Test-RustV2Package.ps1`
- Create: `licenses/Slint-Royalty-Free-2.0.txt`
- Create: `licenses/PDQ-BSD-3-Clause.txt`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `dist-rust-v2/mySingerServer-rust-v2-win-x64.zip`、`.sha256`、包内 `schema/central-v2.sql` 与许可证闭包。

- [ ] **Step 1: 写发布验证失败测试**

对一个故意包含 `ffmpeg.exe`、缺 `worker.exe`、或 PE Machine 非 `0x8664` 的 fixture 运行验证脚本，分别得到明确失败。对缺 Slint/FFmpeg/PDQ/Rust 第三方声明也失败。

- [ ] **Step 2: 实现第三方 notices 生成**

脚本从 `Cargo.lock` 和 `cargo metadata` 生成 Rust 依赖清单，使用固定 `cargo-about 0.9.1` 生成许可证正文；追加 Slint Royalty-free 2.0、Meta PDQ BSD 与 BtbN FFmpeg `LICENSE.txt`。生成物进入包内 `licenses`，不修改根 MIT `LICENSE`。

- [ ] **Step 3: 实现 build-release.ps1**

固定运行 `cargo build --workspace --release --locked --target x86_64-pc-windows-msvc`，调用 fetch-ffmpeg，创建全新 staging，只复制 `desktop.exe/node.exe/worker.exe`、五个 DLL、licenses、`deploy/central-v2.sql`→`schema/central-v2.sql`。不复制 `data`、旧 EXE/DLL、配置或 FFmpeg EXE。

- [ ] **Step 4: 实现 verify-release.ps1**

检查三个 EXE 唯一存在并为 x64；FFmpeg DLL 名称集合与清单完全相等；归档/单文件 SHA 和许可证存在；递归禁止 `ffmpeg.exe|ffprobe.exe|ffplay.exe`；包内无 `*.db`、`config.toml`、旧 Go/C++ 二进制。

- [ ] **Step 5: 生成并验证发布包**

Run: `pwsh -File scripts/build-release.ps1`

Run: `pwsh -File scripts/verify-release.ps1 -Package dist-rust-v2/mySingerServer-rust-v2-win-x64.zip`

Expected: `PACKAGE_PASS`，输出 ZIP 路径、大小和 SHA-256。

- [ ] **Step 6: 从任意当前目录运行冒烟检查**

把 staging 放到含空格的临时路径，从另一个 cwd 启动 `worker.exe` 做 Ready 探测；首次启动 node/desktop 只在 staging 的 `data` 创建目录。

- [ ] **Step 7: 更新 AGENTS.md 并提交**

```powershell
git add -- AGENTS.md scripts/build-release.ps1 scripts/verify-release.ps1 scripts/generate-third-party-notices.ps1 tests/windows/Test-RustV2Package.ps1 licenses
git commit -m "build: package Rust V2 Windows x64 release"
```

### 任务 20：执行自动化、PostgreSQL、FFmpeg、GUI、托盘和删除验收

**Files:**
- Create: `crates/desktop-core/tests/local_node_e2e.rs`
- Create: `crates/desktop-core/tests/two_nodes_e2e.rs`
- Create: `crates/desktop-core/tests/postgres_sync_e2e.rs`
- Create: `docs/verification/2026-08-19-rust-v2-acceptance.md`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: 最终可复跑命令、自动化结果、实际 Windows UI/托盘/删除证据与明确的 PASS/PARTIAL/BLOCKED 边界。

- [ ] **Step 1: 写单节点端到端测试**

在临时便携目录启动真实 worker/node，经 TCP 创建扫描，验证第二次跳过 MD5、精确/相似图片/相似视频、本地分页、复核、永久删除和重启恢复；不设置 PostgreSQL。

- [ ] **Step 2: 写双节点协议端到端测试**

测试启动两个进程内真实 `NodeRuntime`，通过 `FixedIdentityProvider` 注入不同的物理字段，并分别使用独立端口、`AppLayout` 与 SQLite；一个 desktop-core 同时连接，执行 stage1、高水位、批量 phase2、中心分组。生产应用固定使用 `SmbiosIdentityProvider`，配置仍不允许手工 MachineId。

- [ ] **Step 3: 写真实 PostgreSQL 同步测试**

测试标记为默认忽略；harness 手动执行 `deploy/central-v2.sql` 后运行 2501 条增量、提交前断线、ACK 丢失、outbox 已清理触发 snapshot、stage2 高水位和删除墓碑。显式运行命令如下，测试结束只删除专用 Docker volume：

```powershell
$env:DEDUP_TEST_POSTGRES_URL = 'postgresql://dedup:dedup@127.0.0.1:15439/dedup_v2'
cargo test -p dedup-desktop-core --test postgres_sync_e2e -- --ignored --test-threads=1
```

- [ ] **Step 4: 运行统一 Rust 门禁一次**

```powershell
cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked -- --test-threads=1
```

Expected: 全部 PASS；不重复发起额外审查轮次。

- [ ] **Step 5: 运行固定媒体与 FFmpeg 门禁**

Run: `cargo test -p dedup-media -p dedup-media-ffmpeg --locked -- --test-threads=1`

Expected: PDQ 官方 golden、pHash/Sobel golden、六帧位置、JPG 3×2/RGB24/q80、FFmpeg 图片/视频解码和相对 DLL 路径全部 PASS。

- [ ] **Step 6: 使用 computer-use 验收桌面与托盘**

先读取并遵守 `computer-use:computer-use` skill。启动发布 staging：确认 node 无控制台、托盘图标出现、右键状态/打开日志/重启/退出可用；desktop 六组页面与已确认预览一致，手工 IP/端口可编辑，计算中筛选禁用，离线预览/删除禁用。

- [ ] **Step 7: 使用临时文件验收回收站和永久删除**

只操作 `tests/.runtime-delete-fixtures` 下本轮创建的文件。默认删除后通过 Windows 回收站界面确认文件存在且可恢复；切换 Permanent 后文件不进入回收站；大小/MD5 改变项显示 skipped。记录文件名、时间和结果，验收后只清理本轮 fixture。

- [ ] **Step 8: 运行发布验证并记录 SHA**

Run: `pwsh -File scripts/build-release.ps1`

Run: `pwsh -File scripts/verify-release.ps1 -Package dist-rust-v2/mySingerServer-rust-v2-win-x64.zip`

Expected: `PACKAGE_PASS`；把绝对路径、SHA-256、三个 EXE 与五 DLL 清单写入验收文档。

- [ ] **Step 9: 如实记录真实多机边界**

若能访问第二台 Windows x64 主机，按手工 IP/端口运行真实跨机器精确、图片、视频与删除；否则把“真实双物理机”标为 `BLOCKED`，同时保留双节点自动集成 PASS。未运行的 GUI、回收站、真实多机项目不得标 PASS。

- [ ] **Step 10: 最终更新 Agent 文档和提交**

`AGENTS.md` 必须反映最终 crate/进程结构、SQLite/PG schema、任务与分析状态机、同步/删除不变量、FFmpeg 加载、实际构建/测试命令和当前验收边界。

```powershell
git add -- AGENTS.md crates/desktop-core/tests/local_node_e2e.rs crates/desktop-core/tests/two_nodes_e2e.rs crates/desktop-core/tests/postgres_sync_e2e.rs docs/verification/2026-08-19-rust-v2-acceptance.md
git commit -m "test: verify Rust V2 end-to-end workflows"
```

## 最终完成门禁

- [ ] `git status --short` 只显示明确保留的非本任务用户改动；Rust V2 工作区无遗漏文件。
- [ ] `cargo fmt --all -- --check` PASS。
- [ ] `cargo clippy --workspace --all-targets --locked -- -D warnings` PASS。
- [ ] `cargo test --workspace --locked -- --test-threads=1` PASS。
- [ ] PostgreSQL 专用集成测试 PASS，且 desktop 未自动执行 DDL。
- [ ] 发布 ZIP 验证 PASS、SHA-256 已记录、无 FFmpeg EXE、仅 x64。
- [ ] 实际执行过的 Windows GUI/托盘/回收站检查有证据；未执行项明确标 `PARTIAL` 或 `BLOCKED`。
- [ ] 根 `AGENTS.md` 足以让后续实现者理解设计目的、整体架构、实现方案和不可破坏的不变量。
