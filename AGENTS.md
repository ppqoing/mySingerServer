# mySingerServer Rust V2 工程说明

本文件是 Rust V2 的长期架构约束。实现过程中只更新已经落地的结构、方案和验证命令，
不记录逐步操作流水账。旧 Go/C++ 工程保留在仓库中仅用于行为与测试夹具参考；Rust V2
不链接、不启动、不打包任何旧实现产物，也不承担旧协议、数据库或配置兼容。

## 1. 设计目的

Rust V2 面向 Windows x64，解决单机和可信局域网内多台主机的媒体重复检测：

- `node.exe` 在单机上完成扫描、特征计算、本地 SQLite 分析、结果浏览和删除。
- `desktop.exe` 手工连接多个节点，以 PostgreSQL 汇总索引并编排跨机器分析。
- `worker.exe` 隔离 FFmpeg 和媒体解析；崩溃只影响当前任务项，不破坏节点数据库。
- 精确重复固定使用 MD5 后按文件大小判断；相似图片和视频固定采用两层筛选。
- 代码优先直接、短小、可测试；只在进程、协议、配置、数据库和文件系统边界校验一次，
  不为未确认需求增加兼容层、认证、发现服务或过度防御代码。

## 2. 进程拓扑

```text
desktop.exe
  ├─ 手工 IP:port ── TCP + Protobuf ── node.exe (机器 A)
  │                                      ├─ SQLite: data/node/node.db
  │                                      └─ 匿名管道 ── worker.exe × N
  ├─ 手工 IP:port ── TCP + Protobuf ── node.exe (机器 B)
  │                                      ├─ SQLite: data/node/node.db
  │                                      └─ 匿名管道 ── worker.exe × N
  └─ NoTls ── PostgreSQL（中心索引、同步游标、跨机器分析和复核）
```

节点同时只接受一个管理连接，但该连接可以复用并发请求。TCP 明文且无认证，只允许暴露在
可信局域网。Worker 由节点创建并放入 `KILL_ON_JOB_CLOSE` Job Object；Worker 不访问
SQLite、PostgreSQL 或 TCP。三个 `apps` 目录只装配依赖和生命周期，不承载业务逻辑。

## 3. crate 责任表

| crate | 单一职责 | 不允许承担的职责 |
|---|---|---|
| `dedup-core` | 强类型 ID、领域模型、阈值、配置、路径值对象和纯分组规则 | IO、数据库、FFmpeg |
| `dedup-protocol` | `node.proto` 生成类型与领域转换 | TCP 读写、业务编排 |
| `dedup-transport` | 4 字节长度分帧、请求复用、事件和优先级写队列 | 领域决策、数据库 |
| `dedup-media` | MD5、像素管线、PDQ、9 分块 pHash、128 维 Sobel、视频评分、JPG 联系表 | FFmpeg FFI、文件删除 |
| `dedup-media-ffmpeg` | 固定 DLL 加载、FFmpeg FFI、媒体探测与 RGB24 解码 | 特征算法、数据库 |
| `dedup-windows` | 应用目录、SMBIOS、文件枚举、Job Object、回收站与 Shell | 业务状态机 |
| `dedup-node-store` | 当前 V2 SQLite schema、事务、任务、分析、结果和 outbox | 网络和媒体计算 |
| `dedup-node-engine` | 扫描、Worker 池、本地分析、预览、删除与节点 actor | 直接 PostgreSQL 访问 |
| `dedup-desktop-core` | 节点会话、中心同步、PostgreSQL 访问、跨机器分析和 UI 状态 | SQLite 直连、媒体解码 |
| `dedup-desktop-ui` | Slint 页面、视图模型和回调绑定 | TCP、SQLite、PostgreSQL、FFmpeg |

所有业务源文件用中文 `//!` 说明职责；公开类型、函数和接口用中文 `///` 说明语义与错误。
业务 crate 启用 `#![warn(missing_docs)]`。`unsafe` 只留在 FFmpeg FFI 和必要 Windows API
边界，安全接口之外不得传播裸指针或原生句柄。

## 4. 数据所有权

- 节点 actor 串行独占一个 `NodeStore` 和一个 `WorkerPool`，所有 SQLite 写入经 actor 排序。
- 节点 SQLite 是本地扫描、缓存、任务、特征、本地分析、复核、删除和 outbox 的唯一事实源。
- PostgreSQL 由 `desktop.exe` 独占访问，只存中心索引、外部键、同步游标和跨机器结果；
  `desktop.exe` 不打开节点 SQLite，节点也不连接 PostgreSQL。
- 跨边界键固定为 `MachineId`、`ContentKey(md5,file_size)` 和
  `LocationKey(machine_id,normalized_path,file_size)`；SQLite 自增 ID 不通过网络或同步传播。
- 配置、SQLite、日志和缓存只写可执行程序同目录下的 `data`。当前工作目录和用户目录不作
  运行时回退。

## 5. 同步与分析状态机

扫描先生成文件列表，再以“机器 ID + 规范路径 + 文件大小”批量查询 SQLite。命中位置缓存
时跳过 MD5；否则计算 MD5 后以 `ContentKey` 查找已有元数据和特征，已有数据不重复计算。
媒体数据不完整时记为 `skipped_incomplete`，不进入一筛。

本地分析完全在 SQLite 内执行。每次分析冻结输入和阈值；只有相关计算任务全部结束后才能
开始筛选。精确重复为 MD5 索引后比较文件大小；图片一筛为 PDQ/Quality，二筛联合 9 分块
pHash 与 128 维 Sobel；视频均匀抽六帧，每帧走同一图片判定并取平均。分组以最小稳定键
作为代表，只加入与代表直接通过的成员，不做传递闭包。

中心同步每批最多 1000 条：先以 PostgreSQL 已提交 cursor 向节点 ACK，再拉取增量；中心
事务提交后才 ACK 新 cursor。节点 outbox 被裁剪而中心落后时执行整次快照。自动同步只由
连接成功、任务完成和每 5 秒追赶检查触发；手动同步进入同一队列，每节点最多一个同步循环。

跨机器分析先冻结节点集合、各节点 task highwater 和 sync highwater。中心使用完整 stage1
数据批量生成候选，一筛结束后才批量派发缺失 stage2；数据库已有二筛结果时不派发。所有节点
计算完成且 stage2 同步过高水位后才最终筛选。失败运行保持 `partial`，显式重试只补缺失项。

## 6. 不可破坏的硬约束

- 只构建 `x86_64-pc-windows-msvc`，不主动按 Windows 版本号拒绝启动。
- 不添加旧代码兼容、TLS、认证、自动发现、云服务、Web 前端、移动端或自动删除。
- 算法定义和采样位置硬编码；九个匹配阈值可配置并快照到分析运行。
- 图片不生成缩略图。视频联系表固定三列两行、RGB24、JPG 质量 80，复用六个成功抽帧。
- FFmpeg 固定从 `worker.exe` 相对路径 `runtime/ffmpeg` 加载五个 8.0.1 x64 LGPL DLL；
  不搜索当前目录或 PATH，不运行或发布 FFmpeg EXE。
- 删除默认进入回收站；永久删除必须由设置切换。每项删除前重新检查存在、大小和流式 MD5；
  只有成功删除才立即从重复组移除，失败或跳过仍保留。
- PostgreSQL schema 只由用户手动运行 `deploy/central-v2.sql` 创建；应用只校验，不隐式 DDL。
- 文件、模块、公开函数和接口保持详细中文职责注释；实现保持简洁，不重复校验内部强类型。

## 7. 构建与测试命令

固定工具链为 Rust `1.97.1-x86_64-pc-windows-msvc`，根 `.cargo/config.toml` 固定默认目标。

```powershell
cargo fmt --all -- --check
cargo clippy --workspace --all-targets --locked -- -D warnings
cargo test --workspace --locked -- --test-threads=1
cargo build --workspace --release --locked --target x86_64-pc-windows-msvc
```

真实 PostgreSQL 测试默认 `#[ignore]`，只在固定测试容器和
`DEDUP_TEST_POSTGRES_URL` 存在时显式运行。FFmpeg 集成测试使用已校验的测试 DLL来源，
但仍通过生产相对路径加载。每项实现遵循 RED→GREEN→REFACTOR，并只运行计划指定门禁和一次
最终综合门禁，不追加无休止审查。

## 8. 验收边界

当前已建立 Rust 1.97.1 工具链和 13 成员工作区骨架；业务模块将在后续任务按本文件架构填充。
静态测试、集成测试、发布包验证和 Windows 实际 GUI/托盘/回收站验收必须分开记录。没有实际
运行的 GUI、托盘、回收站、第二台物理主机或 PostgreSQL 项不得标记 PASS；可用双节点进程
集成测试证明协议与编排，但真实双物理机不可用时仍标 `BLOCKED`。
