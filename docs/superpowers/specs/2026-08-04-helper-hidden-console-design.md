# Helper 运行时隐藏 CMD 窗口设计

日期：2026-08-04  
状态：已确认（2026-08-04，范围修订采用方案 1）  
方案：A — 将 Helper 构建为 Windows GUI 子系统程序

## 背景与根因

Helper 是长期运行的后台管理组件，由 NodeTray 通过 `ShellExecuteExW` 的 `runas` 动词提权启动，也允许在安装目录中手动运行。当前官方构建命令使用普通 `go build`，没有设置 Go 链接器的 Windows 子系统参数。

只读检查确认：

- 已安装的 `C:\Program Files\MySingerServer\helper.exe` 的 PE Subsystem 为 `3`（`WINDOWS_CUI`）；
- 仓库现有候选产物 `artifacts/helper-config-fingerprint-fix/helper.exe` 也为 `WINDOWS_CUI`；
- `scripts/build.ps1` 构建 Helper 时没有传递 `-H=windowsgui`；
- NodeTray 的 Helper 提权启动器调用 `ShellExecuteExW` 时使用 `Show=1`。

因此 Windows 会为 Helper 创建并显示控制台窗口。该现象来自 Helper 二进制的 PE 子系统类型，不是 Helper 服务循环、控制握手或配置造成的。

## 目标

1. 无论 Helper 由 NodeTray 启动还是手动运行，都不创建或显示 CMD/控制台窗口。
2. 保留现有 UAC 确认弹窗、管理员权限、控制管道、配置指纹、日志和进程认领行为。
3. 用自动化测试固定 Helper 交付物的 PE Subsystem 为 `2`（`WINDOWS_GUI`），防止构建参数回退。
4. 生成同时包含既有配置指纹修复和隐藏窗口行为的 Windows x64 Helper 候选产物。

## 非目标

- 不隐藏或绕过 UAC 确认弹窗。
- 不修改 NodeTray 的 `ShellExecuteExW` 启动参数、提权流程或可信进程认领逻辑。
- 不修改 Helper 的服务循环、控制握手、配置字段、日志位置或退出流程。
- 不增加系统托盘、窗口、消息框或额外后台服务。
- 不自动覆盖 `C:\Program Files\MySingerServer\helper.exe`，不启停当前应用进程。
- 不为候选产物增加 Authenticode 签名；签名状态按实际结果记录。
- 不为两个既有 `OutDir` 构建测试新增生产构建模式、重构 `scripts/build.ps1` 控制流或修改测试语义；其既有失败作为独立已知门禁问题记录。

## 方案比较

### 方案 A：Windows GUI 子系统（采用）

只对 Helper 的交付构建增加 Go 链接器参数 `-H=windowsgui`。生成的 PE Subsystem 为 `WINDOWS_GUI`，因此所有启动方式都不会自动创建控制台。

优点：行为由产物自身保证，覆盖 NodeTray、命令行、资源管理器和计划任务等启动来源；无需在每个启动器重复隐藏逻辑。  
代价：手动命令行运行时也看不到标准错误输出，需要通过退出状态、NodeTray 错误提示和现有日志诊断。

### 方案 B：仅让 NodeTray 使用 `SW_HIDE`（不采用）

修改 `ShellExecuteExW` 的显示参数，只隐藏 NodeTray 启动的 Helper。

缺点：手动运行、计划任务或其他启动入口仍可能显示控制台，不能满足“所有启动方式隐藏”的确认范围。

### 方案 C：GUI 子系统与 `SW_HIDE` 同时修改（不采用）

同时修改产物和启动器。

缺点：两层重复处理，没有增加实际覆盖范围，却扩大启动器测试和回归范围。

## 详细设计

### 1. 官方 Helper 构建

修改 `scripts/build.ps1` 中唯一的官方 Helper 构建命令，仅为 `./cmd/helper` 添加：

```text
-ldflags=-H=windowsgui
```

Agent、GUI、Worker 和 NodeTray 的构建参数保持不变。Helper 的 `requireAdministrator` manifest 仍由现有 `windres` 资源流程嵌入，生成和清理 `cmd/helper/rsrc_windows_amd64.syso` 的边界不变。

### 2. Helper 运行行为

`cmd/helper/main.go` 不需要修改。GUI 子系统只改变 Windows 进程创建时的控制台行为，不改变：

- 参数解析和默认 `helper.json` 路径；
- UAC 提权和管理员令牌；
- Helper/NodeTray 控制管道；
- canonical 配置 SHA-256；
- 优雅停止、强制退出和进程身份；
- 配置加载后的文件日志。

NodeTray 的启动数据流保持：

```text
用户点击启动 Helper
  -> ShellExecuteExW(runas)
  -> 显示 UAC 确认
  -> 启动 WINDOWS_GUI helper.exe
  -> 不创建 CMD 窗口
  -> Helper 加载配置并启动控制管道
  -> NodeTray 严格验证身份和配置指纹
```

### 3. 错误处理

Helper 的返回码和现有错误传播不变。构建为 GUI 子系统后，启动早期写入 `stderr` 的错误不再有可见控制台承载；这是选择“所有启动方式隐藏”的直接结果。

不为此增加消息框或新日志系统：

- NodeTray 启动路径继续显示启动或握手失败；
- Helper 成功加载日志配置后继续写现有文件日志；
- 手动运行失败可通过进程退出状态及现有日志定位。

### 4. 既有构建测试夹具边界

当前以下两个测试调用 `scripts/build.ps1` 时只传 `-OutDir`：

- `TestBuildScriptPackagesHelperWithoutOverwritingOperatorConfig`；
- `TestBuildScriptFailsClosedWhenExactResourceCleanupFails`。

当前脚本默认进入 VideoCore 模式并要求 `-StageDir`，使测试在到达 Helper 构建前就以 `VIDEOCORE_STAGE_REQUIRED` 退出。实际验证进一步确认：`-MediacoreOnly` 分支会在 MediCore 构建后直接返回，同样无法到达 Helper 打包和 fake `windres` 路径。因此只给测试增加 `-MediacoreOnly` 不是有效修复。

用户已选择方案 1：本功能不扩展生产构建接口，也不重构构建脚本。两个测试保持原状，其失败作为与“隐藏 Helper CMD 窗口”无关的已知门禁问题单独报告，不阻塞本功能的直接合同测试和候选产物生成。

### 5. 回归测试

测试分为三层：

1. 构建脚本合同测试：固定官方 Helper 构建命令必须包含 `-ldflags=-H=windowsgui`，删除或替换该参数时测试失败。
2. 实际 PE 测试：构建带管理员 manifest 的 Windows x64 Helper，解析 PE Optional Header，断言 Machine 为 `0x8664` 且 Subsystem 为 `2`；同时继续提取并验证 `requireAdministrator`。
3. 相关回归测试：运行 Helper 的 PE/manifest、配置指纹、控制身份和构建脚本合同测试。完整相关门禁仍执行一次，但两个既有 `OutDir` 夹具若以已确认方式失败，应按已知失败记录，不得描述为本功能回归。

测试不得仅搜索最终产物文件名，也不得用 NodeTray 的隐藏窗口参数代替 PE Subsystem 断言。

## 构建与产物

使用仓库固定 Go、`windres` 和现有 Helper manifest 构建新的独立产物：

```text
artifacts/helper-hidden-console/helper.exe
```

构建时显式使用 `GOOS=windows`、`GOARCH=amd64`、`CGO_ENABLED=0` 和 `-ldflags=-H=windowsgui`。输出目录必须是新的独立目录，不覆盖既有指纹修复产物。

记录：

- 绝对路径、文件大小、修改时间和 SHA-256；
- PE Machine=`0x8664`；
- PE Subsystem=`2`（`WINDOWS_GUI`）；
- manifest=`requireAdministrator`；
- Authenticode 实际状态。

## 验收标准

静态验收：

- 官方 Helper 构建命令包含且只对 Helper 使用 `-H=windowsgui`；
- 实际候选产物为 Windows x64、`WINDOWS_GUI`，并保留 `requireAdministrator`；
- Helper 指纹合同测试与控制身份测试继续通过；
- 本功能的直接合同测试通过；
- `go test -count=1 ./cmd/helper ./integration ./internal/nodetray/... ./nodetray` 仍作为完整诊断门禁执行。两个既有 `OutDir` 夹具的已知失败按方案 1 单独记录为 `KNOWN_FAIL_OUT_OF_SCOPE`，其他失败按实际状态报告，不能写成 PASS。

动态验收：

- 本任务默认不部署、不启停，所以真实安装后的窗口行为和控制握手保持 `BLOCKED_NOT_RUN_DYNAMIC`；
- 后续获得明确部署授权后，从 NodeTray 启动 Helper：UAC 弹窗正常出现，确认后没有 CMD 窗口，Helper 控制握手成功；
- 后续手动运行已部署 Helper：不显示 CMD 窗口。

## 交付边界

当前 checkout 没有 Git 元数据，版本状态记录为 `N/A_NO_GIT_METADATA`；不初始化 Git，不伪造提交或分支状态。候选产物只保存在仓库 `artifacts` 目录，真实安装替换和动态验收必须另行获得用户明确授权。
