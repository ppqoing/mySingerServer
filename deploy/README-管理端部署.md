# mySingerServer Rust V2 管理端部署

管理机器运行 `desktop.exe`，通过手工配置的 IP 和端口连接一个或多个 Node。远程媒体机器无需运行
`desktop.exe`；只运行同版本 `node.exe`，由 Node 自行管理 Worker、SQLite、Everything 和本地缩略图。

## 启动与连接

1. 完整解压 Rust V2 Windows x64 便携包到本地可写目录。
2. 启动 `desktop.exe`，在“节点”页添加 `IP:端口` 并连接。
3. 节点页和扫描路径节点选择框都会显示对应机器唯一 ID，避免把列表序号当成机器身份。
4. 在“设置 → 节点服务”选择 Node 后点击“加载配置”；只有“保存并重启”会把完整配置下发给
   Node。Node 本地保存后自行重启，Desktop 等待同一机器 ID 重新连接并校验配置摘要。

设置编辑缓冲区会保留 PostgreSQL 用户名和密码，运行任务的 2 秒刷新不会覆盖未保存输入。

## PostgreSQL 与运行模式

单机模式不要求 PostgreSQL：Node 使用自己的 SQLite，Desktop 通过目标 Node 创建本地重复文件清单。

多机器重复文件清单需要中心 PostgreSQL。先创建数据库和用户，再在空库手工执行包内
`schema/central-v2.sql`。当前只接受
`schema_metadata.schema_id=mysingerserver-rust-v2-central-schema-3`；脚本不使用 `IF NOT EXISTS`，
不会迁移或覆盖旧库。可使用 `scripts/New-RustV2PostgresContainer.ps1` 创建带命名卷的持久化
PostgreSQL 容器。

容器默认只发布到 `127.0.0.1:15439`。需要让可信 LAN 内的管理端连接时，显式指定节点实际 LAN 地址，
例如 `-HostAddress 192.168.1.17 -HostPort 15439`；脚本只接受明确的 IPv4/IPv6 地址，不接受主机名或
通配符。LAN 发布前应在 Windows 防火墙中仅允许管理端所在网段访问该 TCP 端口，禁止直接暴露到公网，
并在配置、日志和截图中脱敏 PostgreSQL 密码。

“数据库”页只配置主机、端口、数据库名、用户名、密码和重连间隔，支持测试连接，并显示固定业务
表的状态与数据条数。Desktop 的中心连接用于跨机器输入冻结、候选、二次派发、最终分组和结果浏览；
各 Node 是否直连中心缓存由对应 Node 的 `[postgres]` 配置独立决定。

## 三类任务

- 基础计算：`枚举文件 → 查询基础缓存 → 计算基础特征`。
- 重复文件清单：`生成候选 → 派发二次特征 → 精准判重`。
- 二次特征计算：`查询二次特征缓存 → 计算二次特征`。

每阶段在实际开始时单独计时。运行进度每 2 秒合并刷新，终态立即刷新；Worker 行显示 PID、槽位、
当前文件、物理磁盘、当前子步骤，以及 SQLite/PostgreSQL 命中、缩略图复用或原视频回退信息。

## 验收边界

构建、ZIP 白名单和哈希验证只证明发布物静态完整。真实 PostgreSQL、远端 Node、真实媒体和持续运行
必须分别验收；没有实际运行证据的项目不能标记为通过。
