# mySingerServer Rust V2 节点部署

Rust V2 Windows x64 便携包包含 `node.exe`、`worker.exe`、`Everything.exe`、固定 FFmpeg DLL、
`bootstrap.toml`、默认 `config/node.toml` 和中心建库脚本。请完整解压到本地物理磁盘目录，
不要只复制单个 EXE，也不要从 ZIP 内直接运行。

## 启动

1. 运行 `node.exe`。程序清单要求管理员权限，Windows 会显示 UAC 提示。
2. Node 按 `bootstrap.toml` 找到完整配置，默认监听 `127.0.0.1:39091`。
3. Node 启动 Worker 池；`worker.exe` 不访问 SQLite、PostgreSQL 或 TCP。
4. 管理端连接后，可在“设置 → 节点服务”选择此 Node，点击“加载配置”。修改后点击
   “保存并重启”，配置由 Node 写入本机，随后 Node 自行重启并重新连接。

Node 的机器唯一 ID 由 SMBIOS System UUID、System Serial 和 Baseboard Serial 生成，只读显示，
不写入配置。远程计算机只需要运行 `node.exe` 和它启动的 Worker，不需要运行 `desktop.exe`。

## Everything 与扫描路径

默认枚举器为 Everything。Node 收到扫描任务后会检查当前会话中的 Everything IPC 和索引数据库；
若未就绪，则启动 `node.exe` 同目录的 `Everything.exe -startup` 并等待初始化。启动、等待或首次
完整枚举失败时，本次扫描从头使用 Windows Walker，绝不混合两种枚举结果。

扫描页可以添加多个路径项；每项可选择 Node、选择该 Node 上的路径并单独删除。切换 Node 会清空
已选路径。当前只支持本地物理磁盘路径，不保证 UNC 或映射网络盘的物理盘并发语义。

## Node 配置

配置路径支持绝对路径和相对路径。相对字符串保存时保持原样，运行时以 `node.exe` 所在目录解析。
可配置数据、配置、日志、缓存目录，以及 HDD、SSD、未知磁盘的每盘读取线程数、全局读取线程数、
读取块大小、单块超时和重试次数。默认单块超时为 3 秒。

单机只使用 SQLite 时保持：

```toml
[postgres]
enabled = false
host = "127.0.0.1"
port = 5432
database = "media_dedup"
username = "postgres"
password = ""
connect_timeout_seconds = 3
```

多机器模式把 `enabled` 改为 `true`，并填写可达的 PostgreSQL 基础连接参数。Node 会先把结果事务
提交到本地 SQLite，再发布 outbox；基础和二次特征缓存会先查 SQLite，再查 PostgreSQL。
PostgreSQL 连接或查询失败时，本次任务记录警告并降级为 SQLite-only，不会把本地计算标为失败。

## 数据与 schema

- SQLite 当前固定为 `PRAGMA user_version=3`，只自动初始化空数据库。
- 旧 SQLite 不自动迁移；升级不兼容版本时请手工备份并重建数据目录。
- 视频联系表保存在 `<cache_path>/contact-sheets/<md5前两位>/<md5>.jpg`，存在且有效时直接复用。
- 读取超时和 Worker 崩溃按机器 ID、路径及故障详情记录；同一次运行不无限重试崩溃文件。
- 磁盘空间不足时会触发清理 `mySingerServer` 项目路径下全部符合条件的临时文件和可再生产物。

## 完整性与验收边界

发布 ZIP 位于 `dist-rust-v2/mySingerServer-rust-v2-win-x64.zip`，同目录 `.zip.sha256` 保存本轮归档
SHA-256。包内 `manifest/files.sha256` 覆盖解压文件。静态打包验证不等于真实媒体运行验收；
实际验收结果必须单独记录运行目录、媒体根、持续时间、任务终态和日志证据。
