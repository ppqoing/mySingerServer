# 根 README 设计说明

## 目标

为 `mySingerServer` 生成一份中文根目录 `README.md`，让普通用户能够使用
现有 `bin` 产物完成单机或局域网部署，同时让开发者和管理员能够从源码
构建、配置、排错并找到详细设计与验收证据。

README 以普通用户的快速启动为主路径，不重复搬运各里程碑详细设计。

## 读者与使用场景

主要读者按优先级排序：

1. 已获得 `bin` 目录，希望在 Windows 上直接运行的普通用户；
2. 需要部署 PostgreSQL、多个 Agent 和可选删除 Helper 的管理员；
3. 需要从源码构建和核对验收状态的开发者。

默认场景是可信局域网内的 Windows x64 机器。GUI 与 PostgreSQL 可位于
中央机器，每台媒体机器运行一个 Agent；需要删除能力时，再在对应媒体机器
上以管理员权限运行 Helper。

## 文档组织

采用单份完整 README，按“先能运行，再理解和维护”的顺序组织：

1. 项目简介、核心功能、当前状态与平台限制；
2. Agent、GUI、PostgreSQL、Worker、Helper 的关系和数据流；
3. 使用现有 `bin` 目录的快速启动；
4. 单机部署与多 Agent 局域网部署；
5. 扫描、查看重复组、相似分析和确认删除的操作流程；
6. 三份配置文件的关键字段与数据、日志目录；
7. 从源码构建的环境、命令与产物；
8. 常见故障和安全边界；
9. M1～M6 详细设计与验收文档入口。

快速启动明确采用以下顺序：

1. 根据 `deploy/docker-compose.yml` 启动 PostgreSQL；
2. 从示例复制并修改每台机器的 `agent.json`；
3. 如需删除功能，设置窄范围 `allowed_roots` 并以管理员权限启动 Helper；
4. 启动 Agent；
5. 修改中央机器的 `gui.json`，列出所有 Agent；
6. 启动 GUI，并访问其 `listen_addr` 对应的 HTTP 地址；
7. 在 Web 页面下发扫描并查看任务、精确重复组和相似组。

## 内容依据

README 中的命令、默认值和限制只取自以下仓库事实：

- `scripts/build.ps1` 的参数、构建步骤与产物；
- `deploy/*.example.json` 和 `internal/config` 的实际字段及默认值；
- `deploy/docker-compose.yml` 与 `deploy/central.sql`；
- `cmd/agent`、`cmd/gui`、`cmd/helper` 的真实启动参数；
- `internal/gui` 的实际 HTTP 路由和内嵌 Web 页面；
- `docs/deployment/m5-helper.md` 的 Helper 权限与白名单要求；
- `docs/todolist.md` 和 `docs/acceptance` 的最终验收状态。

内部压测命令只作为开发者附录简要列出，不进入普通用户快速启动流程。

## 安全与证据边界

README 必须明确：

- 当前 TCP Agent 协议和 GUI HTTP 入口没有 TLS 或业务鉴权，只适合可信
  局域网或回环地址，不应直接暴露到互联网；
- Docker Compose 中的用户名和密码是开发示例，正式部署必须替换；
- README、示例命令和故障排查不得包含真实 DSN、密码或其他凭据；
- Helper 的 `allowed_roots` 默认必须为空，管理员只能添加明确的本地媒体
  子目录，禁止配置盘符根、系统目录或真实只读测试目录；
- 删除必须由用户明确选择并确认；普通 Agent 不负责自动提权、启动或重启
  Helper；
- M6 标记为 `M6_COMPLETE_OWNER_ACCEPTED`，同时保留其时长、HDD 和 CPU
  测量边界，不把未测项描述为技术 PASS。

## 验证标准

生成 README 后执行以下检查：

1. README 引用的仓库相对路径全部存在；
2. 示例 JSON 字段与三个示例配置及加载器一致；
3. 启动参数与三个主程序的 flag 定义一致；
4. Web 地址、API 能力和启动顺序与实现一致；
5. 构建命令与 `scripts/build.ps1` 参数一致；
6. 不出现真实凭据或受保护媒体目录；
7. 不包含未决占位符、未完成标记或互相矛盾的完成状态；
8. 普通用户主流程与开发者构建流程清晰分离。
