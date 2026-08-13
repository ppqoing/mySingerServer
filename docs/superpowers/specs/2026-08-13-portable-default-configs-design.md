# 双 ZIP 默认配置设计

## 目标

Compute 与 Manager ZIP 解压后即包含可编辑的默认配置。用户不需要复制示例文件或手工创建配置，只需修改参数后启动程序。

## 配置归属

- Agent 使用 `data\agent\agent.json`。
- Worker 不新增独立配置文件；Worker 由 Agent 管理，其默认参数继续归属 `agent.json` 的 Worker 配置段，避免两个配置来源冲突。
- NodeTray 使用 `data\nodetray\tray.json`。
- GUI 使用与 `gui.exe` 同目录的 `gui.json`。
- Helper 的可编辑默认源文件为包根目录 `helper.default.json`。首次启用 Helper 时，NodeTray 自动校验该文件，并通过既有提权写入流程生成受保护的 `data\helper\helper.json`。用户不需要复制或创建运行配置。

## 打包行为

Compute ZIP 包含实际的 `data\agent\agent.json`、`data\nodetray\tray.json` 和 `helper.default.json`，不再把 Agent/Helper 示例文件作为主要配置入口。配置必须使用无凭据、无机器专属目录的安全默认值。

Manager ZIP 包含实际的 `gui.json`。默认只监听 `127.0.0.1:18081`，PostgreSQL DSN 为空，Agent 默认地址为 `127.0.0.1:9101`；PostgreSQL 或 Agent 不可用不能阻止 GUI 打开。

发布清单必须列出这些配置文件，并继续记录文件大小和 SHA-256。打包脚本在生成 ZIP 后解压复核文件清单、配置可解析性和清单哈希。

## Helper 安全边界

ZIP 不直接提供 `data\helper\helper.json`，因为 ZIP 解压不能保留所需的 Windows 受保护 DACL。NodeTray 首次启用 Helper 时读取 `helper.default.json`，执行与界面保存相同的字段校验，再调用现有提权写入器创建受保护目录及运行配置。

若默认源文件缺失或无效，NodeTray 不启动 Helper，并在界面显示稳定错误；不得退化为普通权限写入 `data\helper`。运行配置已存在时，不用默认源覆盖用户配置。

## 启动与恢复

包内默认配置应当可以直接被对应程序读取。现有“配置缺失时自动创建”的兼容能力保留，用于用户误删后的恢复，但正常新解压包不再依赖该流程。

配置中的相对路径一律相对于便携包根或既有配置加载语义解析，不绑定 `C:\Program Files` 或构建机绝对路径。

## 验证

- Compute 打包合同验证三个默认配置入口存在、JSON 可解析、安全默认值正确，并验证 `data\helper\helper.json` 不在 ZIP 中。
- Manager 打包合同验证 `gui.json` 存在且 `gui.exe -config <包根\gui.json>` 可直接启动配置链路。
- Helper 测试验证首次启用自动导入、已有运行配置不覆盖、无效默认源失败关闭，以及受保护写入失败时不启动 Helper。
- 运行现有 NodeTray 供应链、Compute/Manager 发布合同和双 ZIP 打包回归。

## 非目标

- 不为 Worker 增加第二套配置协议。
- 不把数据库、令牌、日志、缓存、SQLite 文件或机器标识打入发布包。
- 不改变 PostgreSQL、Agent 或 Everything 不可用时的既有降级启动策略。
