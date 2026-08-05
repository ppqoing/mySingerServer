# 节点托盘安全部署与验收

本文说明节点托盘、Agent 和“删除 Helper”在 Windows 节点上的权限边界、固定系统目标及验收方法。这里的“删除 Helper”是组件名称，仅代表受控删除服务；它不会扩大删除范围，也不授权脚本清理媒体、配置、日志或其他系统对象。

配套资料：

- [工程使用说明](../../README.md)
- [节点控制面](node-control-plane.md)
- [Helper 部署说明](m5-helper.md)

## 1. 组件与权限模型

| 组件 | 常规身份 | 可执行的管理动作 | 明确禁止 |
|---|---|---|---|
| `nodetray.exe` | 当前登录普通用户，`asInvoker` | 交互式配置、状态显示、启动策略、调用受控后端 | 整体提权、携带秘密启动参数、按进程名结束进程 |
| `agent.exe` / Worker | 与 Tray 相同的普通用户 | 扫描、计算、同步及受控控制面 | 修改 Helper 管理员配置、操作计划任务 |
| `helper.exe` 手动模式 | 每次由同机 Tray 发起 one-shot `runas` | 只在一次 UAC 同意后启动或执行固定提权动作 | 常驻提权代理、接收任意路径/任意命令 |
| `helper.exe` 自动模式 | 固定 Task Scheduler 任务，最高权限 | 登录触发、运行固定 Helper 动作 | 任意任务名、任意 Action、保存密码 |

Tray/Agent 应由同一部署账号运行。Helper 仍必须与 Agent 位于同一台机器，`allowed_roots` 只配置经过人工确认的窄媒体目录，优先使用软删除并关闭硬删除。

## 2. 三个独立授权域

动态验收将高风险权限拆成三个独立开关，任一开关都不能替代另一个：

| 授权 | 影响范围 | 不包含 |
|---|---|---|
| `-AllowUAC` | 允许出现交互式 UAC，并在受控协议下写 Helper 配置或调用固定提权动作 | Task Scheduler 修改、HKCU Run 修改 |
| `-AllowTaskScheduler` | 允许检查或修改唯一固定任务 `\MySingerServer\DeleteHelper` | UAC 同意、注册表修改、其他任务 |
| `-AllowHKCUStartup` | 允许检查或修改当前用户唯一 Run 值 `MySingerServerNodeTray` | UAC、任务计划、其他用户或其他 Run 值 |

`-WhatIf` 优先级最高：即使命令行同时出现授权开关，也只能执行只读预检。当前静态验收不运行任何授权开关。

## 3. 固定路径、值与 ACL

安装器最终必须把下列逻辑位置解析为规范绝对本地路径，并把结果作为单个部署实例的固定合同。后端拒绝相对路径、重解析逃逸和相互重叠的配置目标。

| 内容 | 固定逻辑位置/规则 | 权限要求 |
|---|---|---|
| 程序 | `%ProgramFiles%\MySingerServer\` | 普通用户只读执行；Administrators/SYSTEM 管理 |
| Tray 设置 | `%LOCALAPPDATA%\MySingerServer\NodeTray\tray.json` | 仅当前用户及 Administrators/SYSTEM |
| Agent 配置 | `%ProgramData%\MySingerServer\Node\agent.json` | 部署用户、Administrators、SYSTEM；不得向 Everyone/NETWORK 开放 |
| Helper 配置 | `%ProgramData%\MySingerServer\Helper\helper.json` | Owner 为 Administrators 或 SYSTEM；受保护 DACL；普通部署用户只读执行，不能写、删或替换 |
| 配置备份 | 与正式配置同目录，固定后缀 `.last-good` | 与对应正式配置相同；只保留一份可严格复验版本 |
| Agent 日志 | Agent `data_dir` 解析后的固定绝对目录 | 只允许部署账号和管理员访问；不记录完整 DSN/密码 |
| Helper 日志 | Helper `log_dir` 解析后的固定绝对目录 | 不得位于媒体白名单、TestRoot 或不受控共享目录 |
| Helper 任务 | `\MySingerServer\DeleteHelper` | 单 Principal、登录触发、单 Exec；最高权限且不保存密码 |
| 登录启动值 | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 下 `MySingerServerNodeTray` | 数据只能是规范 `nodetray.exe` 绝对路径加 `--background` |

Helper 的正式文件、`.last-good`、临时文件和锁文件均须使用受保护 DACL。SYSTEM/Administrators 可管理，当前部署用户不得拥有写、删除、`FILE_DELETE_CHILD` 或重命名替换能力。父目录同样不得向普通用户、Everyone、Authenticated Users、Users 或 Interactive Users 授予替换/删除能力。

`OpenLocation` 只允许 Agent/Helper 的日志和备份四个固定枚举，不能由前端传入任意目录。

## 4. 进程与提权安全合同

- Tray 和 Agent 使用当前用户单实例；第二个 Tray 只向受限激活通道发信号。
- 只认领 PID、创建时间、最终映像路径、控制握手和配置指纹完全匹配的实例。
- `Stop` 只向已认领控制通道发送 shutdown 并等待。`stop_timeout` 不触发隐式强杀。
- `ForceStop` 是停止超时后的独立按钮，必须再次确认；执行前重新核对 PID、创建时间和最终路径。`identity_mismatch` 或未认领实例必须拒绝。
- 不使用 `taskkill /IM`、进程名通配、全局进程扫描或自动无限重启。
- 一次性提权请求使用 64 位小写十六进制 nonce。nonce 只消费一次，第二帧、重放或响应 nonce 不匹配都拒绝。
- 提权管道 DACL 只允许当前用户、Administrators、SYSTEM；连接双方还要验证对端 PID/最终映像。
- 提权动作固定为 `write_helper_config`、`install_helper_task`、`remove_helper_task`。命令行只包含 one-shot 模式、pipe 和 nonce，不包含动作 Payload、配置、DSN、密码、token 或媒体路径。

## 5. 启动行为矩阵

登录启动仅控制 Tray 是否随当前用户登录运行；Agent/Helper 的 `manual`/`automatic` 是独立策略。

| Tray 登录启动 | Agent | Helper | 登录后的行为 |
|---|---|---|---|
| 关闭 | manual | manual | 不自动运行；操作员手动打开 Tray 后分别启动 |
| 开启 | manual | manual | Tray 启动并显示状态；不启动组件 |
| 开启 | automatic | manual | Tray 尝试认领/启动 Agent；Helper 等待手动 UAC |
| 开启 | manual | automatic | Tray 不启动 Agent；仅运行固定 Helper 任务 |
| 开启 | automatic | automatic | Tray 分别启动 Agent 和固定 Helper 任务；任一失败不阻止 UI 启动 |

关闭 Tray 默认不停止 Agent/Helper。选择“停止组件后退出”时，固定顺序受控停止 Agent、Helper；任一步骤超时必须要求操作员选择继续等待、取消退出或另行显式 ForceStop，绝不自动强杀。

## 6. 安装、升级、回滚和卸载

### 安装

1. 验证安装包哈希和发布清单，把程序写入只读安装目录。
2. 创建固定共享配置目录并先设置 Owner/DACL，再写示例配置。
3. 通过交互式表单导入现有 JSON；严格拒绝未知字段，不自动启动组件。
4. 如启用 Helper automatic，在独立授权后安装唯一固定任务并复读定义。
5. 如启用 Tray 登录启动，在独立授权后写唯一固定 Run 值并复读。
6. 先执行 `-WhatIf`，再在专用验收机获得三个权限域的明确授权后运行动态验收。

### 升级

1. 先受控停止需要替换的已认领组件；停止超时不强杀。
2. 保存程序版本、固定任务定义、Run 值和配置指纹，不复制秘密到证据。
3. 替换程序，保留正式配置和一份 `.last-good`。
4. 检查任务 Exec、Run 路径和单实例身份是否漂移；只修复本程序固定目标。
5. 重新运行 WhatIf 和经授权的动态验收。

### 回滚

1. 恢复上一版已验证程序；不要自动用备份覆盖当前正式配置。
2. 配置不兼容时由操作员查看脱敏摘要并明确选择恢复 `.last-good`。
3. 复验固定任务、Run 值和组件 identity 后再启动。

### 卸载

1. 明确选择是否受控停止组件；停止超时不自动强杀。
2. 在相应独立授权下仅删除固定任务和固定 Run 值。
3. 移除程序文件。配置、日志和媒体默认保留，另行人工决定。
4. 不枚举或清理其他任务、Run 值、进程、配置目录和媒体目录。

若发现漂移，先记录当前单一固定目标，再由操作员确认修复。脚本只恢复它实际修改过的目标，不做“清理所有同名项”。

## 7. 可复现构建与 WebView2 供应链

节点托盘发布构建固定使用 Wails `v2.12.0`、仓库 `go.sum` 中的 module sum、前端独立 `package-lock.json` 和 `npm ci`。生产命令固定使用 `-webview2 embed` 与 `-trimpath`，禁止 `-clean`、`-debug`、`-devtools` 和 source map。`-clean` 会删除版本控制中的 manifest/icon，因此不是发布参数。

WebView2 Evergreen Bootstrapper 只从 `third_party/webview2` 预下载缓存读取，构建过程不联网下载或替换它。缓存合同包括：

- `manifest.schema.json`：固定字段和格式；
- `manifest.json`：官方来源 URL、实际缓存来源、Wails module sum、SHA-256、文件大小、获取时间、NOTICE 路径和 Authenticode 签名摘要；
- `NOTICE.md`：指向 [Microsoft 官方下载页](https://developer.microsoft.com/en-us/microsoft-edge/webview2/)、[官方 Evergreen Bootstrapper](https://go.microsoft.com/fwlink/p/?LinkId=2124703) 和[官方分发说明](https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/distribution)。NOTICE 不是许可证全文，也不得被当作许可证；
- `MicrosoftEdgeWebview2Setup.exe`：必须与 manifest 的大小和 SHA-256 完全一致，且 Authenticode 必须为 Microsoft Corporation 的有效签名。

当前缓存的实际来源是固定依赖 `github.com/wailsapp/wails/v2@v2.12.0` 自带的嵌入 Bootstrapper；manifest 明确区分官方可获取地址与实际缓存来源，不能把二者混写。更新缓存时必须重新记录真实来源、时间、哈希、大小和签名，不得使用非官方镜像或手工伪造元数据。

独立构建命令：

```powershell
$env:GOCACHE = (Join-Path $PWD '.tmp-gocache')
pwsh -NoProfile -File scripts/build-nodetray.ps1 `
  -Go C:\path\to\go.exe `
  -OutDir C:\tmp\mysingerserver-nodetray-stage
```

脚本顺序为 `npm ci` → test/lint/build → Wails 绑定生成 → Go 测试 → Wails 生产构建 → AMD64 PE 与内嵌 `asInvoker` manifest 复验 → 临时 stage 同父目录原子发布。正式 `OutDir` 必须预先不存在；任一步失败都不能留下正式 stage。脚本只删除 `nodetray/build/bin` 和固定生成资源 `nodetray-res.syso`，不会启动产物、安装 WebView2、触发 UAC、写注册表或计划任务。

完整发布的 `scripts/build.ps1` 默认加入 `nodetray.exe`、经校验的 `MicrosoftEdgeWebview2Setup.exe`、`agent.example.json` 和 `helper.example.json`，并把它们写入 `release-manifest.json`。`-VideoCoreOnly`、`-MediacoreOnly` 自动跳过托盘；开发者可显式传 `-SkipNodeTrayBuild`，但该产物不是完整节点发布包。现有 VideoCore/FFmpeg fresh-stage、递归 DLL 闭包和禁止 CLI FFmpeg 工具的边界保持不变。

静态供应链门禁：

```powershell
pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1
```

门禁还可接收 `-StageDir`，检查真实 stage 不含 `node_modules`、TypeScript 源码、source map 或疑似测试凭据。`Get-AuthenticodeSignature` 对 `nodetray.exe` 的结果必须如实记录；个人内部构建通常为 `NotSigned`，不能因此宣称已签名。是否要求代码签名由后续正式发布策略决定。

## 8. WhatIf 与动态验收

只读预检：

```powershell
pwsh -NoProfile -File tests/windows/Test-NodeTrayBackend.ps1 -WhatIf
```

预期输出是 JSON：`mode=what-if-read-only`、`status=PASS`，并显示当前 SID、Task Scheduler 只读查询结果、规范 TestRoot、三个二进制状态、固定 Task/Run 目标、七个动态场景和证据目录。二进制未构建时对应项为 `BLOCKED_NOT_RUN_DYNAMIC`，不影响 WhatIf 完成。WhatIf 不创建 TestRoot/证据，不查询或修改固定任务/Run 值，不启动进程或触发 UAC。

授权后动态命令必须使用绝对路径，例如：

```powershell
$root = 'C:\tmp\mysingerserver-node-tray-backend'
$stage = (Resolve-Path '.\artifacts\stage').Path
pwsh -NoProfile -File tests/windows/Test-NodeTrayBackend.ps1 `
  -NodeTrayExe (Join-Path $stage 'nodetray.exe') `
  -AgentExe (Join-Path $stage 'agent.exe') `
  -HelperExe (Join-Path $stage 'helper.exe') `
  -TestRoot $root `
  -AllowUAC -AllowTaskScheduler -AllowHKCUStartup
```

该命令只能在具备交互式桌面的专用 Windows 验收机上、经逐项授权后运行。当前脚本为 fail-closed 骨架：真实二进制尚未提供只限制在 TestRoot 的受控验收协议，因此七项都必须返回 `BLOCKED_NOT_RUN_DYNAMIC`，进程退出码非零。`blocked` 表示未运行，不是 PASS；不能用单元测试或 WhatIf PASS 替代动态证据。

证据只能包含稳定错误码、状态、时间和固定测试目标的脱敏摘要，严禁 DSN、password、token、完整配置内容或媒体路径。

## 9. 事故处置

| 现象 | 处置 |
|---|---|
| UAC 取消 | 记录 `uac_cancelled`，保留旧配置、旧任务和旧组件状态；不要循环弹窗 |
| 任务漂移 | 停止自动运行；只比较固定任务定义，获 `AllowTaskScheduler` 与所需 UAC 授权后重建固定任务 |
| Run 漂移 | 显示当前固定值的脱敏状态；获 `AllowHKCUStartup` 后仅修复 `MySingerServerNodeTray` |
| `unclaimed_instance` | 不发送 shutdown、不接管；核对 PID、创建时间、映像路径、握手和配置指纹 |
| `identity_mismatch` | 拒绝 ForceStop；重新刷新身份，不按进程名结束 |
| `ready_timeout` | 保留失败态并展示脱敏原因；人工修复后再启动，不自动无限重启 |
| `stop_timeout` | 继续等待、取消退出或由操作员独立确认 ForceStop；绝不隐式强杀 |
| 配置复读/恢复失败 | 正式文件不自动覆盖；验证 `.last-good` 摘要后由操作员确认恢复 |

## 10. 真实媒体目录边界

真实媒体目录始终只读，绝不能作为 TestRoot、生成语料目录、证据目录、输出目录或清理目标。尤其不得操作 `I:\tmp`、`H:\pik\00000000000`、`G:\pik`、`D:\webdev` 和 `D:\m6-generated-corpus`。验收只允许 `C:\tmp\mysingerserver-node-tray-backend*` 或仓库 `.tmp\mysingerserver-node-tray-backend*` 的专用目录；清理前还要再次验证规范路径、重解析属性和目录身份。
