# 完全便携双发布包设计

## 目标

将现有 Windows x64 发布物改为两个职责独立、可解压到任意本地可写目录运行的 ZIP：

1. 通用计算端：负责扫描、哈希、缩略图、相似度计算和受控删除。
2. 远端管理端：负责连接外部 PostgreSQL 和多个计算节点，派发任务、观察进度、查看去重结果与错误。

两个产品均不依赖 `C:\Program Files`、当前工作目录、`ProgramData` 或 `LocalAppData`。本设计不提供安装器、不捆绑 PostgreSQL，也不迁移旧版本数据。

## 已确认范围

- 采用完全便携模式，程序、配置、数据库、日志和 WebView2 用户数据均位于各自发布目录。
- 不自动迁移或兼容读取旧 `ProgramData`、`LocalAppData` 数据。
- 同一台机器仍只允许一个计算端实例运行，不支持多个计算端副本同时运行。
- 计算包保留 NodeTray 本地托盘管理。
- 管理包连接用户已有的 PostgreSQL，不包含数据库运行环境或安装脚本。
- 管理配置由用户手工把模板复制为正式配置后编辑。
- 双击 `gui.exe` 可以直接启动管理端，监听成功后自动打开默认浏览器。
- 按用户明确选择，普通用户可写目录也允许启用管理员 Helper；接受其可执行文件可能被替换而造成的本地提权风险。
- 支持本地磁盘与可移动磁盘中的绝对路径，不承诺 UNC 网络目录。

## 方案选择

采用“保留现有组件边界，增加统一便携根和双包发布”的方案。

未采用以下方案：

- 以当前工作目录作为根：快捷方式、登录启动、计划任务和外部终端可能提供不同工作目录，行为不可靠。
- 将计算端合并成单一 EXE：会破坏 Worker 隔离、管理员 Helper、Everything IPC 和原生 DLL 边界，超出本次目标。
- 制作两个安装器：不符合解压即用和任意目录运行要求。
- 用命令行或环境变量指定便携根：普通启动、UAC 子进程、登录启动和计划任务需要同步传递参数，容易产生路径分歧。

## 产品边界

### 通用计算端

计算端继续使用现有进程分工：

- `nodetray.exe`：本机配置、启停、状态和托盘交互。
- `agent.exe`：接收管理端任务、维护扫描任务和本地状态。
- `worker.exe`：执行哈希、缩略图和媒体计算。
- `helper.exe`：执行需要管理员权限的受控删除与计划任务操作。
- `Everything.exe`、`Everything64.dll`：路径索引和枚举。
- VideoCore、FFmpeg、WebView2 及其许可证和运行依赖。

计算端不包含 `gui.exe`、`gui.json` 或管理端启动脚本。

### 远端管理端

管理端包含：

- `gui.exe`：内嵌 Web 管理页面和 HTTP API。
- `gui.example.json`：无密码、无令牌、无真实节点地址的配置模板。
- `Start-Manager.ps1`：从脚本自身目录启动 `gui.exe` 的可选启动入口。
- `README-管理端部署.md`：中文部署和排障说明。
- `release-manifest.json`：发布身份和逐文件哈希。

管理端不包含 Agent、Worker、Helper、Everything、FFmpeg、VideoCore、WebView2 或 PostgreSQL。

## 目录布局

### 计算端

```text
MySingerServer-Compute\
├─ nodetray.exe
├─ agent.exe
├─ worker.exe
├─ helper.exe
├─ Everything.exe
├─ Everything64.dll
├─ MicrosoftEdgeWebview2Setup.exe
├─ 原生 DLL 和工具
├─ licenses\
├─ release-manifest.json
└─ data\                    # 发布包不预建 data\helper
   ├─ nodetray\
   │  ├─ tray.json
   │  └─ webview2\
   └─ agent\
   │  ├─ agent.json
   │  ├─ agent.db
   │  ├─ logs\
   │  └─ stats.log
```

`data\helper` 不作为空目录、`.gitkeep` 或 ZIP 目录项发布。只有用户通过 NodeTray UI
保存 Helper 配置时，提权写入器才首次创建受保护的 `data\helper`、`helper.json` 和日志目录。

### 管理端

```text
MySingerServer-Manager\
├─ gui.exe
├─ gui.example.json
├─ gui.json                  # 用户从模板复制；发布包不预置
├─ Start-Manager.ps1
├─ README-管理端部署.md
├─ release-manifest.json
└─ data\                    # 首次运行时创建
   └─ logs\
      └─ gui.log
```

## 便携根与路径解析

### 统一规则

所有默认路径以当前入口 EXE 的最终真实路径所在目录为根，不使用进程当前工作目录。解析流程为：

1. 读取当前可执行文件绝对路径。
2. 解析 Windows 最终路径，避免短路径、大小写和重解析点造成身份分歧。
3. 以最终路径的父目录作为便携根。
4. 只在该根目录下生成组件路径和 `data` 路径。

目录不可写、目标路径逃逸便携根或必要文件缺失时明确失败，不静默回退到系统目录。

### 计算端路径

NodeTray 不再查询 `FOLDERID_ProgramFiles`、`FOLDERID_ProgramData` 或 `FOLDERID_LocalAppData`，也不再要求自身路径等于 `C:\Program Files\MySingerServer\nodetray.exe`。

NodeTray 从自身便携根解析 Agent、Worker、Helper、配置、日志和 WebView2 用户数据。Agent 继续从自身目录解析 Worker、Everything 和原生 DLL。登录启动、UAC 一次性进程和 Helper 计划任务均保存当前便携根中的绝对路径。

移动计算目录后，NodeTray 将现有登录启动项或 Helper 计划任务视为漂移；用户保存相应设置或执行修复操作时，以新目录中的绝对路径更新它们。

### 管理端路径

Windows 上先打开当前 GUI 映像并取得最终路径，再以最终 `gui.exe` 的父目录为便携根；最终路径为 UNC 时拒绝启动。`gui.exe` 无 `-config` 参数时默认读取该目录下的 `gui.json`。显式 `-config` 仍按既有语义解析并覆盖默认路径。GUI 自身日志固定写入 `<exe-root>\data\logs\gui.log`，并保留控制台日志输出。

`Start-Manager.ps1` 使用 `$PSScriptRoot` 定位 `gui.exe` 和 `gui.json`，不得依赖调用者的工作目录。

## GUI 启动和浏览器行为

管理端启动顺序为：

1. 解析便携根。
2. 创建并验证便携日志目录，初始化文件日志。
3. 读取并校验 `gui.json`，连接外部 PostgreSQL。
4. 绑定 HTTP 监听地址。
5. 记录实际监听成功状态。
6. 默认打开本机可访问的管理页面地址。
7. 开始接受 HTTP 请求和远端 Agent 连接。

当监听地址为 `0.0.0.0` 或 `::` 时，浏览器地址分别映射为 `127.0.0.1` 或 `[::1]`，不把通配监听地址直接交给浏览器。增加 `-no-browser` 参数，供服务化或脚本化启动时禁用自动打开。

配置缺失、JSON 无效、目录不可写、端口占用或 PostgreSQL 不可达时，错误必须写入 `data\logs\gui.log`。Windows 交互启动同时显示可理解的错误信息，避免双击后只出现瞬时关闭的控制台窗口。

## 数据和兼容性

- 不探测、不读取、不复制旧系统目录中的配置、数据库、日志或 WebView2 数据。
- 新便携目录首次启动形成全新计算端状态；旧数据如需保留，由用户手工复制并承担一致性检查。
- 计算端继续使用现有机器身份和单实例机制；移动目录不创建第二套并行管道或第二个活动计算端。
- 管理端的业务数据仍以外部 PostgreSQL 为准，本地只保存配置和运行日志。
- 显式配置文件参数保持兼容，便于诊断和自动化。

## 安全边界

完全便携目录可能允许普通用户替换 `helper.exe`、`nodetray.exe` 或配置，而 Helper 可以通过 UAC 或计划任务以管理员权限运行。根据用户明确选择，本实现不因目录普通用户可写而禁止 Helper。

部署文档必须明确警告：在多人使用或不可信账户可写的机器上，应由管理员把计算目录 ACL 限制为仅 Administrators 和 SYSTEM 可修改。程序仍须保留进程身份、最终路径、父子进程和请求协议验证，但这些检查不能消除可执行文件本身被替换的风险。

发布模板不得包含 PostgreSQL 密码、访问令牌、真实主机名或真实 Agent 地址。Manager 模板的 DSN scheme 只能是 `postgres` 或 `postgresql`，host 只能是 `127.0.0.1` 或 `localhost` 安全占位；发布脚本继续 fail closed 地检查模板。

## 构建与发布

完整构建阶段继续生成所有可执行文件和依赖。发布阶段从同一个经过验证的阶段目录生成两个产品：

```text
MySingerServer-compute-win-x64-<版本>.zip
MySingerServer-compute-win-x64-<版本>.zip.sha256
MySingerServer-manager-win-x64-<版本>.zip
MySingerServer-manager-win-x64-<版本>.zip.sha256
```

两个 ZIP 各自包含 `release-manifest.json`，字段至少包括：

- schema 版本；
- 产品和 `release_kind`；
- Windows x64 目标；
- 构建日期；
- 源码提交；
- `portable_root` 为 `.`；
- 文件相对路径、大小和 SHA-256。

新增总发布入口，在任务专用临时目录中生成和验证两个候选包。只有两个包的文件集合、manifest、解压校验和 sidecar 全部通过，才把四个最终文件发布到输出目录。任一候选失败时清理候选文件，不留下半套最终发布物。目标文件已存在时拒绝覆盖。发布阶段失败后的回滚必须打开并持续锁定每个已发布文件，在同一文件句柄上完成 SHA-256 校验和删除；若路径在校验后被替换，只删除原先锁定的发布对象，保留替换后的用户文件。句柄打开、校验、删除或关闭失败时保留对象、记录 cleanup warning，并继续处理其余回滚项。

## 测试策略

### 单元和组件测试

- 便携根从 EXE 最终路径派生，不受当前工作目录影响。
- 含空格、中文和不同盘符的本地绝对路径可正确生成布局。
- 生成路径无法逃逸便携根。
- NodeTray 普通和 UAC 入口使用相同布局。
- Agent、Worker、Helper、Everything 和 DLL 均从计算包解析。
- GUI 默认读取 EXE 同目录 `gui.json`，显式 `-config` 保持优先。
- GUI 监听成功后才打开浏览器；`-no-browser` 不调用浏览器启动器。
- 通配监听地址映射为回环浏览器地址。
- GUI 启动错误写入便携日志并进入交互错误通道。
- 登录启动和 Helper 计划任务可以检测并修复移动目录后的路径漂移。

### 发布契约测试

- 计算包包含全部计算运行文件，不包含 `gui.exe`、GUI 配置或预建的 `data\helper` 目录项。
- 管理包只包含 GUI、模板、启动脚本、部署说明和发布 manifest，不包含计算依赖；`data\logs` 由首次运行创建。
- 两个配置模板不包含密码、令牌或真实地址。
- 两个 manifest 与解压文件集合、大小和 SHA-256 完全一致。
- 两个 sidecar 与最终 ZIP SHA-256 一致。
- 模拟第二个候选包失败时，输出目录不出现任何本次发布的最终文件。

### 动态验收

- 分别解压到非 `C:\Program Files`、含空格和中文的本地可写目录。
- 从另一个当前工作目录启动计算端和管理端。
- 计算端完成配置、Agent 启动、Everything 首次索引等待和真实扫描。
- 移动计算目录后修复登录启动项和 Helper 计划任务，并完成启停。
- 手工复制并编辑 `gui.json`，双击 `gui.exe` 后自动打开管理页面。
- 使用 `-no-browser` 启动时不打开浏览器。
- 管理端连接外部 PostgreSQL 和至少一个远端计算端，完成任务派发、进度观察、去重结果和错误页面检查。

没有实际完成 GUI、UAC、计划任务、外部 PostgreSQL、远端 Agent 或真实媒体扫描的验收项必须标记为 `PARTIAL` 或 `BLOCKED`，不得用静态测试代替。
