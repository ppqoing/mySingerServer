# mySingerServer Rust V2 最终验收记录

## 1. 验收范围与结论

本记录对应分支 `codex/rust-v2-media-dedup` 的任务 20，验收日期为 2026-08-19。
目标是验证全新 Rust V2 在 Windows x64 上的自动化闭环、真实 FFmpeg 运行时、真实
PostgreSQL 同步、Windows 删除语义、桌面程序、节点进程和最终便携包。旧 Go/C++ 产物、
旧配置和旧数据库不在验收范围内。

状态定义：

- `PASS`：已在本轮以真实命令或真实 Windows 用户界面取得可复现证据。
- `PARTIAL`：可验证部分通过，但当前工具无法完成全部实际交互；不把静态测试外推为通过。
- `BLOCKED`：缺少必要外部环境，未执行，也未以模拟结果替代。

| 验收项 | 状态 | 本轮证据 |
| --- | --- | --- |
| Rust 格式、Clippy、完整工作区测试 | PASS | `cargo fmt`、全 workspace Clippy 和测试均退出 0 |
| 固定媒体算法与 FFmpeg | PASS | PDQ、图片两层、视频六帧/联系表、真实 DLL 解码测试退出 0 |
| 单节点便携端到端 | PASS | 真实 Worker、五个 FFmpeg DLL、TCP、SQLite、扫描、三类分析、复核、删除、重启 |
| 桌面控制器自动重连 | PASS | 已成功会话关闭后同端点重启恢复；PG backend 终止后重连并再次 ACK |
| 多节点同步不阻塞 UI | PASS | 一个真实 PullChanges 持续等待时，控制器仍在 1 秒内处理 Shutdown |
| 双节点协议与隔离 | PASS | 同机两个独立便携节点、机器 ID、端口、SQLite、高水位和冻结输入互不串用 |
| PostgreSQL 自动同步 | PASS | 真实 PostgreSQL 16；1000/1000/501、ACK、stage2、失败恢复、快照和墓碑 |
| 跨节点中心编排 | PASS | 两个真实 loopback 节点完成 stage1→批量 phase2→最终图片组 |
| 默认回收站与永久删除 | PASS | 生产 `dedup-windows` API、`SHQueryRecycleBinW` 与 Explorer 三方结果一致 |
| 大小/MD5 变化保护 | PASS（自动化） | 节点删除测试覆盖失败/跳过且不失活；本轮未在 Explorer 重演篡改文件场景 |
| Desktop 启动与总览 | PASS | 实际 Release GUI、八个导航标签、一个在线节点、24 个 Worker 和手工地址控件可读 |
| Desktop 八页逐项点击与截图 | PARTIAL | Slint 可访问树无坐标；截图接口返回 `0x80004002`，未猜测屏幕坐标 |
| Node 托盘菜单实际右键/重启/退出 | PARTIAL | 无控制台、listener 和 Worker 实际通过；Computer Use 不暴露任务栏托盘目标 |
| 最终 Windows x64 ZIP | PASS | 两套独立验证通过；15 个白名单文件；哈希见第 8 节 |
| 第二台真实 Windows x64 物理机 | BLOCKED | 当前只有一台物理主机；双 loopback 节点结果不能替代真实 LAN 验收 |

## 2. 环境

| 项目 | 值 |
| --- | --- |
| 工作树 | `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup` |
| 分支 | `codex/rust-v2-media-dedup` |
| Rust | `rustc 1.97.1 (8bab26f4f 2026-07-14)` |
| Cargo | `cargo 1.97.1 (c980f4866 2026-06-30)` |
| Host/Target | `x86_64-pc-windows-msvc` |
| PowerShell | `7.6.4` |
| PostgreSQL | `postgres:16-alpine`，仅绑定 `127.0.0.1:15439` |
| FFmpeg 包 | BtbN LGPL shared 8.0.1 锁定归档；只发布五个官方 DLL |

用户级 `PATH` 已加入 `G:\Code\ffmpeg-8.0.1-full_build\bin`。该设置供命令行工具使用；
生产 `worker.exe` 按设计不搜索 `PATH`，只从自身相对路径 `runtime\ffmpeg` 加载五个 DLL。

Cargo 命令统一清除环境中遗留的 MinGW `CC`/`CXX`，避免 `rusqlite` 的 MSVC 构建与 GCC
产物混链，并使用系统临时目录下的独立 `CARGO_TARGET_DIR`。

## 3. 最终自动化门禁

执行：

```powershell
$env:PATH = 'C:\Users\Administrator\.cargo\bin;' + $env:PATH
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
$env:CARGO_TARGET_DIR = Join-Path $env:TEMP 'rust-v2-media-dedup-target'

cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked -- --test-threads=1
cargo test -p dedup-media -p dedup-media-ffmpeg --locked -- --test-threads=1
```

结果：四条命令均退出 0。媒体门禁包含：

- Meta PDQ 上游固定图片的 256 位 golden 与 Quality。
- 九分块 pHash、128 维 Sobel 和图片两层联合判断。
- 视频六个均匀槽位、只对成功帧求平均值及两层判断。
- 3×2、RGB24、JPG 质量 80 的视频联系表。
- 真实 JPEG/MP4 探测与解码，以及从 Worker 相对目录加载 DLL。

## 4. 单节点、重连和双节点端到端

### 4.1 单节点便携链

显式运行被标记为 `#[ignore]` 的真实运行时测试：

```powershell
cargo test -p dedup-desktop-core --test local_node_e2e `
  real_local_node_scan_analysis_review_delete_and_restart `
  -- --ignored --exact --test-threads=1
```

结果：`1 passed; 0 failed`。测试把最终 staging 的 `worker.exe` 与五个 FFmpeg DLL 复制到
全新临时便携目录，启动真实 `NodeRuntime`、Worker 池和 TCP 会话，验证：

1. 首次扫描计算并保存内容、位置和特征。
2. 第二次扫描在同一机器 ID、规范路径、文件大小命中时跳过 MD5；测试在不改变大小的情况下
   改写首字节，仍复用数据库缓存，证明跳过条件确实按需求执行。
3. 精确重复、相似图片和相似视频分析完成；组和成员分页游标稳定。
4. 复核标记写入 SQLite，并在关闭/重启后恢复。
5. Permanent 删除使用生产路径执行，成功位置立即失活并缩组。
6. 便携目录之间不共享 `data`、SQLite 或缓存。

### 4.2 Desktop 自动重连

```powershell
cargo test -p dedup-desktop-core --test controller_reconnect `
  controller_reconnects_when_manual_endpoint_comes_online `
  -- --exact --test-threads=1
```

结果：`1 passed; 0 failed`。测试先建立并观察到 Online，再关闭整个节点服务和既有连接，确认
Desktop 进入 Error；随后在同一 IP/端口重启服务，控制器删除失效会话并按配置的 1 秒间隔恢复
Online。该测试不再用“Hello 之前丢弃第一个 socket”代替已建立会话断线。

另外在真实 PostgreSQL schema 上显式运行两个 ignored 控制器测试：

```powershell
$env:DEDUP_TEST_POSTGRES_URL = 'postgresql://dedup:dedup@127.0.0.1:15439/dedup_v2'
cargo test -p dedup-desktop-core --test controller_reconnect `
  -- --ignored --test-threads=1
```

结果：`2 passed; 0 failed`。第一个测试让节点的真实 `PullChanges` 一直等待；修复前唯一控制循环
无法消费 Shutdown，测试取得 RED，修复后 1 秒内收到 `ShutdownComplete`。第二个测试在首次 ACK
之后通过 PostgreSQL `pg_terminate_backend` 终止已经建立的 desktop clients；节点级同步循环丢弃
失效 client，在后续固定触发重新连接并再次 ACK。每个节点现在拥有独立有界触发通道、
`SyncEngine` 和 PG client；连接成功、轮询首次观察到任务完成、五秒追赶与手动按钮进入同一通道，
长增量/快照不在 UI 控制循环内执行。

### 4.3 两个独立节点

```powershell
cargo test -p dedup-desktop-core --test two_nodes_e2e `
  two_portable_nodes_isolate_identity_scan_highwater_and_inputs `
  -- --ignored --exact --test-threads=1
```

结果：`1 passed; 0 failed`。两个节点分别使用固定但不同的机器身份、独立 loopback 端口、
独立 AppLayout/SQLite 和真实 Worker。并行扫描后，机器 ID、任务高水位和分析输入均保持隔离。
这证明同一管理端同时连接多个节点的协议与状态边界，但不等同于第二台物理机。

## 5. 真实 PostgreSQL 同步和跨节点编排

测试数据库只使用计划专用 Compose，不使用生产数据库：

```powershell
docker compose -f deploy\rust-v2-test-compose.yml up -d
docker compose -f deploy\rust-v2-test-compose.yml exec -T postgres `
  psql -v ON_ERROR_STOP=1 -U dedup -d dedup_v2 -f /schema/central-v2.sql

$env:DEDUP_TEST_POSTGRES_URL = 'postgresql://dedup:dedup@127.0.0.1:15439/dedup_v2'
cargo test -p dedup-desktop-core --test postgres_sync_e2e `
  -- --ignored --test-threads=1
cargo test -p dedup-desktop-core --test two_nodes_e2e `
  coordinator_runs_stage1_batched_phase2_and_final_group_across_two_nodes `
  -- --ignored --exact --test-threads=1
```

结果：PostgreSQL 同步文件的 3 个测试全部通过；跨节点协调器测试 `1 passed; 0 failed`。
数据库元数据为 `schema_id=mysingerserver-rust-v2`、`schema_version=1`，`public` schema 共 20 张表。

覆盖闭环：

- 2501 条 outbox 固定拆成 1000、1000、501，ACK 依次到 1000、2000、2501。
- 新增图片二筛为 seq 2502，中心保存完整特征正文和新游标。
- 同步仓储 fixture 在调用真实事务前注入失败时，节点 cursor 保持 0，重试同批后收敛；该项验证
  提交边界，不冒充 TCP 断线。
- 在真实 PostgreSQL 提交之后由节点适配器注入 ACK 丢失时，下轮先用中心 cursor ACK，再得到空批次；
  已建立 PG 连接的真实终止与重连由第 4.2 节的 `pg_terminate_backend` 测试覆盖。
- 节点 outbox 已裁剪时返回 `SnapshotRequired`，中心按固定八表视图提交快照。
- 同一删除墓碑批次重放两次不产生重复墓碑，位置继续保持 inactive。
- 跨节点运行先封存 stage1 输入，第一轮批量派发两个节点的 phase2；同步完成后第二轮生成
  Passed 候选和包含两个机器位置的最终图片组。

验收完成后执行：

```powershell
docker compose -f deploy\rust-v2-test-compose.yml down
```

专用容器和网络已删除；该 Compose 使用 tmpfs，没有留下专用数据库卷。

## 6. Windows 回收站与永久删除

端到端测试只接受仓库 `tests\.runtime-delete-fixtures` 目录下由本轮预先创建的两个文件，避免误删：

```powershell
$fixtureRoot = Join-Path (Get-Location) 'tests\.runtime-delete-fixtures'
New-Item -ItemType Directory -Path $fixtureRoot -Force | Out-Null
$env:DEDUP_TEST_RECYCLE_FILE = Join-Path $fixtureRoot 'recycle-fixture.jpg'
$env:DEDUP_TEST_PERMANENT_FILE = Join-Path $fixtureRoot 'permanent-fixture.jpg'
Set-Content -LiteralPath $env:DEDUP_TEST_RECYCLE_FILE -Value 'recycle fixture'
Set-Content -LiteralPath $env:DEDUP_TEST_PERMANENT_FILE -Value 'permanent fixture'
cargo test -p dedup-windows --test shell_delete_e2e `
  recycle_bin_and_permanent_delete_have_distinct_windows_outcomes `
  -- --ignored --exact --test-threads=1
```

初始真实测试暴露一个产品缺陷：仅设置 `FOF_ALLOWUNDO` 时，`IFileOperation` 返回成功且源文件消失，
但实际回收站项目数没有增加。修复后生产实现同时设置 `FOF_ALLOWUNDO` 与
`FOFX_RECYCLEONDELETE`；前者保留旧 Windows 语义，后者明确请求 Windows 8+ 回收。

最终测试在宿主 Windows 桌面用户上下文执行并通过：

- 夹具所在卷的 `SHQueryRecycleBinW` 项目数精确增加 1，不再用全部卷计数承受外部活动抖动。
- Explorer 的前轮验收显示回收项；最终复跑又精确定位
  `rust-v2-final-recycle-20260819-161454.jpg`。
- 该项目的 Shell verbs 包含“还原”。
- Permanent 文件消失，但回收站项目数不再增加。
- 验收后只还原上述精确文件，确认落回专用 fixture 根，再永久清理；fixture 根最终不存在。

受限 sandbox 无权读取桌面用户的回收站，因此其 `Access denied`/计数 0 没有被当作通过证据。

## 7. 实际 GUI 与托盘边界

从最终 staging 启动真实 `node.exe` 和 `desktop.exe`。进程证据：节点监听
`127.0.0.1:39091`，Desktop 可访问树中显示：

- 窗口标题 `mySingerServer · Media Dedup` 与 `Rust V2 x64`。
- 八个导航入口：总览与节点、扫描任务、精确重复、相似图片、相似视频、跨机器、删除复核、
  设置诊断。
- `1 个节点在线`，本地手工端点为 `127.0.0.1:39091`。
- 节点返回真实物理机器 ID，显示 `Worker 0/24 忙碌`。
- 任务 0 queued/0 running，中心/本地 outbox 0/0。
- IP、端口输入框和连接、刷新、同步、移除按钮存在。

因此 Release GUI 启动、默认手工端点连接和总览状态标为 `PASS`。

当前 Computer Use 的截图调用返回
`SetIsBorderRequired failed: 不支持此接口 (0x80004002)`；无截图模式能读取 Slint 文本，但这些
文本节点不提供几何坐标，不能可靠点击八页。窗口列表也不暴露 Windows 任务栏/通知区域为目标，
所以不能真实右键托盘菜单。按工具恢复规则停止坐标猜测，以下保持 `PARTIAL`：

- 八页内的逐项交互与实际视觉截图。
- 托盘状态、打开日志、重启计算引擎和退出的实际右键操作。

静态托盘命令测试已通过；实际节点无顶层控制台窗口、listener 正常、24 个 Worker 已创建，但这些
只能作为进程行为证据，不能代替托盘点击。验收后按 staging 绝对路径停止全部相关进程，结果
`STAGING_PROCESSES_REMAINING=0`。

## 8. 最终便携发布包

回收站实现修复后，从新的 staging 完整重建：

```powershell
$releaseTarget = Join-Path $env:TEMP 'rust-v2-media-dedup-target'
pwsh -NoProfile -File scripts\build-release.ps1 -CargoTargetDir $releaseTarget
pwsh -NoProfile -File scripts\verify-release.ps1 `
  -Package dist-rust-v2\mySingerServer-rust-v2-win-x64.zip
pwsh -NoProfile -File tests\windows\Test-RustV2Package.ps1
```

三条路径分别输出 `RUST_V2_RELEASE_BUILD_PASS`、`PACKAGE_PASS` 和
`RUST_V2_PACKAGE_TEST_PASS`。`cargo-about 0.9.1` 核对 699 个解析依赖；Slint 的
`LicenseRef` 警告由生成器显式追加 11 个 Slint 包条目与完整 Royalty-free 2.0 正文闭合。

| 属性 | 值 |
| --- | --- |
| 绝对路径 | `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup\dist-rust-v2\mySingerServer-rust-v2-win-x64.zip` |
| 大小 | `64,804,379` 字节 |
| SHA-256 | `b99f71d79f51aab92092360cde32b6d9d13f887a7729b008927232fe9c7b4c9e` |
| 普通文件数 | 15 |

白名单文件：

```text
desktop.exe
node.exe
worker.exe
runtime/ffmpeg/avcodec-62.dll
runtime/ffmpeg/avformat-62.dll
runtime/ffmpeg/avutil-60.dll
runtime/ffmpeg/swresample-6.dll
runtime/ffmpeg/swscale-9.dll
schema/central-v2.sql
licenses/FFmpeg-LGPL-3.0.txt
licenses/PDQ-BSD-3-Clause.txt
licenses/Project-MIT.txt
licenses/Rust-Third-Party-Licenses.html
licenses/Slint-Royalty-Free-2.0.txt
manifest/files.sha256
```

包内没有 FFmpeg EXE、数据库、配置、缓存、旧 Go/C++ 可执行文件或非 x64 程序。中心 SQL 仅随包
提供，应用不会替用户执行 DDL。

## 9. 尚未闭合的外部验收

只有以下外部边界没有冒充 PASS：

1. 第二台真实 Windows x64 物理主机当前不可用，故真实 LAN 上的跨机器精确/图片/视频和分布式
   删除为 `BLOCKED`。现有双节点测试只证明同机不同端口的协议、存储和编排闭环。
2. 当前 Computer Use 无法定位 Slint 控件坐标或任务栏托盘，GUI 八页逐项交互和托盘菜单实际操作
   为 `PARTIAL`。这不是代码测试失败，也不是已经完成的实际 UI 验收。

除此之外，测试 PostgreSQL、临时 fixture 和 staging 进程均已清理；主工作树
`D:\code\mySingerServer` 的既有脏文件未被清理、还原或覆盖。
