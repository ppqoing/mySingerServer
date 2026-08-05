# NodeTray 禁用 Helper 状态误报修复验收记录

验收日期：2026-08-03  
工作目录：`D:\code\mySingerServer`  
总体结论：自动化行为、Go 全量与 race、前端门禁、静态模型检查和独立构建全部通过。Go race 首次受 `CGO_ENABLED=0` 环境影响而未启动测试；只读核对既有验收记录后，使用本机已有的 GCC 16.1.0、设置进程级 `CGO_ENABLED=1` 和绝对 `CC` 重跑通过。未获得真实安装替换及启动授权，动态验收保持 `BLOCKED_NOT_RUN_DYNAMIC`。

## 自动化行为

| 场景 | 结果 | 证据 |
|---|---|---|
| Helper 禁用、PID 0、配置不可用 | PASS | `TestOverviewNormalizesDisabledUnavailableHelperAndKeepsTaskDrift`；Go NodeTray 全量测试退出码 0 |
| Helper 启用、配置不可用 | PASS | `TestOverviewKeepsEnabledUnavailableHelper`；Go NodeTray 全量测试退出码 0 |
| Helper 禁用但有真实 PID | PASS | `TestOverviewKeepsDisabledHelperWhenRealPIDExists`、`Helper 禁用但有真实 PID 时显示运行态并允许停止`；Go 与 Vitest 退出码均为 0 |
| Helper 任务漂移保持可见 | PASS | `TestOverviewNormalizesDisabledUnavailableHelperAndKeepsTaskDrift`、`Helper 禁用且无 PID 时显示未启用、清空最近异常并禁用全部操作`（直接断言 Overview Helper 卡片仍显示计划任务漂移提示）；Go 与 Vitest 退出码均为 0 |
| 禁用 Helper 原始事件与旧 attention | PASS | `忽略禁用无 PID Helper 的 unavailable 和 attention，但接受真实 PID`、`刷新为禁用无 PID Overview 时清除旧 Helper attention`；Vitest 退出码 0 |
| 设置保存后刷新共享 Overview | PASS | `保存成功后等待共享 Overview 刷新再结束 pending`；Vitest 退出码 0 |

## 回归门禁

| 门禁 | 实际命令 | 退出码 | 实际结果 |
|---|---|---:|---|
| Go NodeTray 全量 | `$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-disabled-helper-go-cache'; C:\tmp\go1.26.5\go\bin\go.exe test .\internal\nodetray\... .\nodetray -count=1 -timeout 180s` | 0 | PASS；14 个包通过。补充 JSON 计数复跑退出码 0，263 个顶层测试、460 个含子测试的 PASS 事件 |
| Go race | `$env:CGO_ENABLED='1'; $env:CC='C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe'; C:\tmp\go1.26.5\go\bin\go.exe test -race .\internal\nodetray\app .\internal\nodetray\production -count=1 -timeout 180s` | 0 | PASS；2 个包通过，输出不含 `DATA RACE`。首次未设置 CGO/CC 时退出码 2，输出 `go: -race requires cgo`；确认已有 MinGW-W64 GCC 16.1.0 后安全重试成功，未安装或下载工具链 |
| Vitest | `D:\application\nodejs\npm.cmd test`（目录 `nodetray\frontend`） | 0 | PASS；19/19 个测试文件、101/101 个测试通过 |
| ESLint | `D:\application\nodejs\npm.cmd run lint`（目录 `nodetray\frontend`） | 0 | PASS；0 error、3 warning，均为 `react-refresh/only-export-components` Fast Refresh warning |
| TypeScript/Vite | `D:\application\nodejs\npm.cmd run build`（目录 `nodetray\frontend`） | 0 | PASS；TypeScript `--noEmit` 与 Vite v8.2.0 均成功，39 个模块完成转换 |
| lifecycle 与模型静态检查 | 简报 Step 3 的两条 `rg` 检查 | 0 | PASS；禁用 lifecycle 搜索按预期返回 1（无匹配），模型字段搜索返回 0；后端未扩展 lifecycle，`HelperEnabled/helperEnabled` 仍是策略字段 |
| 独立构建 | `$env:GOCACHE='D:\code\mySingerServer\.tmp\nodetray-disabled-helper-go-cache'; .\scripts\build-nodetray.ps1 -Go 'C:\tmp\go1.26.5\go\bin\go.exe' -Npm 'D:\application\nodejs\npm.cmd' -OutDir 'artifacts\nodetray-disabled-helper-state-fix'` | 0 | PASS；最终 JSON `status=PASS`，构建过程中复跑 14 个 Go 包及前端 101 个测试，未启动产物 |

未执行 `go test ./...`。

## 发布产物

绝对目录：`D:\code\mySingerServer\artifacts\nodetray-disabled-helper-state-fix`

| 文件 | 大小（字节） | SHA-256 | Authenticode |
|---|---:|---|---|
| `nodetray.exe` | 13,148,160 | `C8461D704C869A05486C5E556EC59FC62ED2C705A571EDF67D63A39EED544ADC` | `NotSigned` |
| `MicrosoftEdgeWebview2Setup.exe` | 1,793,816 | `7EBC4CE80143EF89CEA86A61EA151502868DB6CAAA678B8B43660A66ACE11C3A` | `Valid` |

构建脚本实际校验 NodeTray PE 机器类型为 `windows/amd64`，请求执行级别为 `asInvoker`；WebView2 Bootstrapper 的大小、哈希与有效签名均由本轮构建重新校验。产物目录实际文件数为 2。

## 动态验收边界

本轮没有替换或启动 `C:\Program Files\MySingerServer` 下的程序，没有启动 GUI、Agent、Worker 或 Helper，也没有触发 UAC、计划任务或 HKCU 变更。以下项目均为 `BLOCKED_NOT_RUN_DYNAMIC`：

- `BLOCKED_NOT_RUN_DYNAMIC`：实机 Helper 卡片显示“未启用”；
- `BLOCKED_NOT_RUN_DYNAMIC`：页面不再出现“组件不可用”；
- `BLOCKED_NOT_RUN_DYNAMIC`：最近异常显示“—”，三个 Helper 操作按钮禁用；
- `BLOCKED_NOT_RUN_DYNAMIC`：Agent 与 Worker 实机状态不受影响。

## 检查点

`N/A_NO_GIT_METADATA`：工作目录没有 `.git` 元数据；本轮不初始化 Git、不创建 worktree、不提交。
