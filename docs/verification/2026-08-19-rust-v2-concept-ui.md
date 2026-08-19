# Rust V2 概念界面验收记录

> 计划日期：2026-08-19
> 实际验收日期：2026-08-20
> 自动化与发布基线：`5f89e5e2a7126171118b481c96605cdd5e4fb5d7`

## 结论

- Rust 工作区格式、严格 Clippy、完整测试和 Windows x64 Release 构建：**PASS**。
- 新便携包构建与独立静态复验：**PASS**。
- 真实 Release 进程启动和总览页只读观察：**PASS**。
- Computer Use 逐页点击与 12 张截图：**PARTIAL**。系统截图接口返回 `SetIsBorderRequired failed: 不支持此接口 (0x80004002)`；文本可访问树可读，但元素点击又因 `coordinate input geometry is unavailable` 无法执行，键盘 `Tab` 后焦点仍停留在根窗口。未使用离屏图、设计稿或静态页面替代真实截图。

## 自动化门禁

运行前均清除了当前 PowerShell 进程的 `CC`、`CXX`，避免 MinGW 环境变量污染 MSVC 目标。

| 门禁 | 结果 | 真实证据 |
|---|---|---|
| `cargo fmt --all -- --check` | PASS | 退出码 0 |
| `cargo clippy --workspace --all-targets --locked -- -D warnings` | PASS | 退出码 0，严格警告门禁通过 |
| `cargo test --workspace --locked -- --test-threads=1` | PASS | 退出码 0；63 个 test-result 区块合计 154 passed、0 failed、16 ignored |
| `cargo build --workspace --release --locked --target x86_64-pc-windows-msvc` | PASS | 退出码 0；冷构建完成于 1m51s |

首次完整测试在执行任何测试前因 D 盘无剩余空间失败，错误包括 OS 112、LLVM `no space on device`、LNK1180 和 Slint `SaveError(StorageFull)`。只读解析 `cargo metadata.target_directory` 后，确认目标为当前隔离工作树内的 `target`；仅执行一次 `cargo clean`，删除 56,296 个可再生文件（Cargo 报告 38.3 GiB），随后从零重跑并取得上述 PASS。未删除源码、发布目录、其他工作树或用户文件。

未跟踪的 `crates/desktop-core/tests/physical_two_hosts_e2e.rs` 被 Cargo 自动发现并参与编译/测试，但始终没有修改或暂存；其环境条件测试仍按测试自身声明忽略。

## Windows x64 便携包

| 项目 | 结果 |
|---|---|
| `scripts/build-release.ps1` | `RUST_V2_RELEASE_BUILD_PASS`、`PACKAGE_PASS`，退出码 0 |
| 包路径 | `dist-rust-v2/mySingerServer-rust-v2-win-x64.zip` |
| 大小 | 65,464,920 bytes |
| SHA-256 | `2f8045411e6d4b838c7320b94320544a51afaf193685472f41cbfc2f673ac2e5` |
| `scripts/verify-release.ps1 -Package <zip>` | `PACKAGE_PASS`，退出码 0 |

计划中的 `-PackagePath` 参数已与当前脚本接口漂移：脚本实际只有一个必填参数 `-Package`。第一次使用 `-PackagePath` 时 PowerShell 报参数不存在；未修改脚本，随后按当前源码接口使用 `-Package` 完成独立复验。

## 真实 Windows GUI 验收

本轮从新建包的 `dist-rust-v2/staging/desktop.exe` 启动真实 Release 进程。进程路径、窗口句柄和标题均已核对：窗口标题为 `mySingerServer · Media Dedup`。Computer Use 的 `list_apps()` 返回唯一应用标识和唯一窗口。

文本模式的真实窗口观察成功，当前总览页可访问树确认：

- 七个主导航按钮：总览、节点、扫描、任务、重复文件、审核删除、设置；
- 顶栏本地搜索、刷新和“0 个节点在线”；
- 总览指标、真实本机节点 `127.0.0.1:39091`；
- 未提供能力以“功能暂不可用 / 统计暂不可用 / —”表达；
- 状态栏显示引擎就绪、同步位置和 PostgreSQL 未配置说明。

随后两次真实窗口截图均在 Windows Graphics Capture 边框接口失败。可访问点击无法获得坐标，键盘焦点链也没有进入侧栏按钮，因此没有继续猜测坐标、没有触发删除动作、外部通信、登录或权限操作。四类重复、审核/删除、设置/诊断和默认回收站仍有本轮真实 `MainWindow` 行为测试与离屏布局测试作为自动化证据，但这些证据不替代 GUI PASS。

## 截图交付状态

以下均为计划中的预期相对路径；由于真实截图 API 不可用，文件**未生成**，状态统一为 **PARTIAL**，没有放置占位图或伪造图。

| 序号 | 视图 | 预期文件 | 状态 |
|---:|---|---|---|
| 1 | 总览 | `rust-v2-concept-ui/01-overview.png` | PARTIAL：窗口文本观察成功，截图失败 |
| 2 | 节点 | `rust-v2-concept-ui/02-nodes.png` | PARTIAL |
| 3 | 扫描 | `rust-v2-concept-ui/03-scan.png` | PARTIAL |
| 4 | 任务 | `rust-v2-concept-ui/04-tasks.png` | PARTIAL |
| 5 | 精确重复 | `rust-v2-concept-ui/05-exact.png` | PARTIAL |
| 6 | 相似图片 | `rust-v2-concept-ui/06-similar-images.png` | PARTIAL |
| 7 | 相似视频 | `rust-v2-concept-ui/07-similar-videos.png` | PARTIAL |
| 8 | 跨机器重复 | `rust-v2-concept-ui/08-cross-machine.png` | PARTIAL |
| 9 | 审核工作台 | `rust-v2-concept-ui/09-review.png` | PARTIAL |
| 10 | 删除中心 | `rust-v2-concept-ui/10-delete-center.png` | PARTIAL |
| 11 | 设置 | `rust-v2-concept-ui/11-settings.png` | PARTIAL |
| 12 | 日志与诊断 | `rust-v2-concept-ui/12-diagnostics.png` | PARTIAL |

## 验收边界

- 自动化 PASS 证明：七主导航与四类重复的状态映射、审核删除双模式、设置七节、默认回收站、删除执行门禁、有限分页、按需预览、视频六帧联系表文案和禁用占位契约均未回归。
- 包验证 PASS 证明：本轮新 ZIP 的文件闭包、架构、许可证和哈希清单符合发布脚本要求。
- 本记录没有把软件离屏渲染、可访问树或包验证冒充真实逐页截图；12 图与对应人工视觉检查仍需在支持 Windows Graphics Capture 的交互桌面补验。
