# 当前项目模块结构图 Draw.io 设计

日期：2026-08-06  
状态：三页分层方案已确认

## 1. 目标与交付物

基于当前 `main` 工作区源码、`CONTEXT.md` 和 `docs/current-project-architecture.md`，生成可由 draw.io/diagrams.net 直接打开和继续编辑的多页文件：

`docs/current-project-module-architecture.drawio`

图稿只描述当前已经存在的模块、依赖和运行关系，不引入新的系统设计，也不修改业务代码。

## 2. 页面结构

### 页面一：整体运行拓扑

展示系统部署和进程所有权：

- 中央端：浏览器、中央 React Web、`gui.exe`、PostgreSQL。
- 媒体节点：`nodetray.exe`、`agent.exe`、`worker.exe × N`、可选 `helper.exe`、SQLite、本机媒体目录。
- 主要运行关系：浏览器访问 GUI，GUI 连接多个 Agent，Agent 创建 Worker、访问 SQLite 并同步 PostgreSQL，Helper 执行受控删除，NodeTray 管理本机组件生命周期。
- 强调 Worker 只由 Agent 管理，浏览器不直接连接 Agent 或数据库。

### 页面二：源码模块依赖

按源码层次展示可执行入口、内部包、前端和原生模块：

- 可执行入口：`cmd/gui`、`cmd/agent`、`cmd/worker`、`cmd/helper`、`nodetray`。
- 中央模块：`internal/gui`、`firstscreen`、`phase2`、`proto`、`config`。
- 节点模块：`internal/agent`、`worker`、`wproc`、`store`、`syncer`、`enum`、`stats`、`diskmap`。
- 本机控制模块：`nodectl`、`agentcontrol`、`helpercontrol`、`internal/nodetray/*`、`machineid`。
- 前端：`webui` 构建到 `internal/gui/web`；`nodetray/frontend` 构建到 Wails 嵌入资源。
- 原生模块：Worker 当前默认依赖 VideoCore 和 FFmpeg；MediaCore 标记为兼容/旧路径。

依赖箭头从使用方指向被依赖方；编译/源码依赖使用实线，生成物或嵌入关系使用点线。

### 页面三：协议与数据关系

展示跨进程通信和数据所有权：

- HTTP/JSON：浏览器与 GUI。
- TCP/MessagePack v1：GUI 与 Agent。
- Windows 命名管道：Agent 与 Worker、Agent 与 Helper。
- MessagePack 本机控制面：NodeTray 与 Agent/Helper。
- Wails Bridge：NodeTray React 与 NodeTray Go 后端。
- SQLite：由 Agent 持有本地文件、特征和同步队列。
- PostgreSQL：由 GUI/Agent 使用，保存中央文件、特征、任务和重复组。
- C ABI：Worker 调用 VideoCore，VideoCore 使用 FFmpeg SDK/运行库。

## 3. 视觉规范

- 中央端使用蓝色，媒体节点使用绿色，数据存储使用紫色，原生媒体模块使用橙色，本机控制模块使用青色。
- 容器采用浅色背景和深色标题，模块采用圆角矩形，数据库采用圆柱形。
- 实线表示直接依赖或主要调用；虚线表示生命周期控制、生成/嵌入或可选关系。
- 每页均包含标题、简短说明和图例；标签使用中文，源码路径和协议名称保留英文。
- 采用未压缩 Draw.io XML，便于版本控制和人工审阅。

## 4. 范围边界

- 不逐一展开基准测试、语料生成和驻留测试工具。
- 不展开每个数据库字段、HTTP 路由或 MessagePack 消息字段。
- 不把生成目录、缓存和发布产物画成独立业务模块。
- 不改变 `CONTEXT.md` 中已经确定的领域名称。

## 5. 验收标准

1. 文件是合法 XML，根元素为 `mxfile`，包含三个可独立切换的 `diagram` 页面。
2. 三页名称分别为“整体运行拓扑”“源码模块依赖”“协议与数据关系”。
3. GUI、Agent、Worker、删除 Helper、NodeTray、两套前端、SQLite、PostgreSQL、VideoCore 和 MediaCore 均有明确位置。
4. 关键依赖方向与当前源码及现有架构文档一致。
5. 文件中不存在 `TODO`、`TBD`、临时占位节点或无法解释的孤立模块。
6. Draw.io 打开后所有节点、分组、连接线和文字均可编辑。
