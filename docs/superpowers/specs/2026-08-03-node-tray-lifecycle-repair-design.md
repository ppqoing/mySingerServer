# NodeTray 配置与生命周期修复设计

日期：2026-08-03  
状态：已确认，实施计划已生成  
适用范围：`nodetray.exe` 的配置保存、启动、取消启动、停止、重启和退出流程

实施计划：[`2026-08-03-node-tray-lifecycle-repair.md`](../plans/2026-08-03-node-tray-lifecycle-repair.md)

## 1. 背景

当前 NodeTray 的静态排查确认以下问题：

1. Wails `runtime.Quit` 会再次进入 `OnBeforeClose`，而现有回调始终返回
   `true`，导致退出请求被自身否决；
2. 前端“停止组件后退出”和托盘右键退出复用 `ExitTray(true)`，后端会在
   优雅停止失败后自动强制终止，绕过前端预期的二次确认；
3. 保存程序设置会无条件执行计划任务提权动作，普通设置修改也会弹出 UAC，
   且取消或失败时磁盘设置可能已经部分提交；
4. Supervisor 在单一命令循环中同步等待启动或停止，启动等待期间无法及时处理
   取消；原生托盘消息循环也会同步等待生命周期操作；
5. 保存组件配置后没有返回真实磁盘指纹，也没有显示运行配置是否需要重启；
6. 原子保存只在替换前复读临时文件，没有在替换后复读正式文件。

本设计修复上述流程，不改变 Agent 对 Worker 的所有权，也不增加远程管理、按
进程名清理或直接管理单个 Worker 的能力。

## 2. 已确认产品语义

### 2.1 退出

- 所有明确的“退出”入口先显示同一个确认弹窗；
- 用户取消时，不修改任何组件或 UI 进程状态；
- 用户确认后，不先执行 15 秒优雅停止，直接强制结束后台组件；
- 退出顺序为 Helper、Agent/Worker、NodeTray UI；
- UI 进程只能在已记录的后台进程全部确认退出后关闭；
- 单个后台进程终止失败时仍继续尝试其他后台组件；
- 任一后台进程最终仍存活时，NodeTray UI 保持运行并允许重试；
- 窗口右上角关闭在 `closeToTray=true` 时仍只隐藏到通知区域；
- `closeToTray=false` 时，窗口关闭请求打开同一个强制退出确认弹窗。

本应用为个人使用。强制退出时直接使用监督器当前记录的 PID，不重新检查 PID
启动时间、可执行文件路径、组件类型、配置摘要或控制管道握手。设计接受 PID
退出后被系统复用时可能结束无关进程的风险。实现仍不得按进程名扫描或结束监督
器记录之外的进程。

### 2.2 保存与重启

- “保存”只写入配置，不隐式重启；
- 保存成功返回正式配置的真实 SHA-256 和 `needsRestart`；
- “保存并重启”按“保存成功、停止旧实例、确认退出、启动新实例”执行；
- 旧实例停止失败时不启动第二实例；
- 普通“停止”和“重启”仍使用优雅停止；
- 只有整个应用的确认退出流程直接强制终止已运行组件；取消本次尚未完成的启动
  时，可以结束该次启动刚创建的进程，防止留下失去监督的实例。

### 2.3 复杂度边界

- 不引入通用事务框架、进程权限代理或复杂的退出时安全复核；
- 不改变现有本机控制协议；
- 不改变 Helper 白名单、软删除默认值和硬删除确认语义；
- 不扫描全机进程，不恢复任何按进程名批量终止行为。

## 3. 方案选择

采用“后端单一强制退出协调器”方案。前端只负责显示确认弹窗和提交一次
`ForceExitAll` 请求；后台终止顺序、等待、失败聚合和 Wails 退出授权全部由 Go
后端负责。

不采用以下方案：

- 前端依次调用多个 ForceStop API：容易在页面刷新、前端异常或入口差异下形成
  半完成状态；
- 继续扩展 `ExitTray(bool)`：布尔参数已经混淆“仅退出、优雅退出、强制退出”
  三种语义；
- 继续让托盘 Win32 消息循环同步执行生命周期操作：会造成通知区域图标无响应。

## 4. 总体架构

```text
托盘右键退出 ─┐
设置页退出 ───┼─> force-exit-requested ─> 统一确认弹窗
窗口关闭请求 ─┘                            │
                                    取消   │   确认
                                      │    ▼
                                 不做修改  ForceExitAll
                                               │
                                   强制结束记录的 Helper PID
                                               │
                                   强制结束记录的 Agent PID
                                               │
                                   等待已记录 Worker PID 退出
                                               │
                                  ┌────────────┴────────────┐
                                  │                         │
                              全部已退出                仍有存活/失败
                                  │                         │
                        授权 OnBeforeClose 放行       UI 保持并显示重试
                                  │
                           runtime.Quit 关闭 UI
```

## 5. 强制退出设计

### 5.1 入口统一

原生托盘菜单的 `ExitTray` 命令不再直接执行后台停止。它只执行以下两步：

1. 显示并激活 NodeTray 控制台；
2. 发送 `force-exit-requested` 前端事件。

设置页“退出托盘程序”和 `closeToTray=false` 的窗口关闭请求发送同一事件。React
应用只维护一个退出弹窗实例，避免不同入口出现不同语义。

### 5.2 后端接口

新增独立结果模型：

```go
type ForceExitResult struct {
	OK               bool     `json:"ok"`
	FailedComponents []string `json:"failedComponents"`
	ErrorCode        string   `json:"errorCode"`
	ErrorSummary     string   `json:"errorSummary"`
}
```

Backend 暴露：

```go
func (b *Backend) ForceExitAll() traymodel.ForceExitResult
```

删除前端对 `ExitTray(bool)` 的调用；旧接口在全部调用方迁移后删除，避免继续产生
歧义。

### 5.3 后台终止

组件接口把 `ForceStopClaimed` 改为语义明确的 `ForceStopTracked`：

```go
ForceStopTracked(context.Context) traymodel.OperationResult
```

该方法执行以下行为：

1. 读取 Supervisor 当前记录的 PID；
2. PID 为空时返回成功；状态为 `stopped` 但仍记录非空 PID 时仍继续终止和等待；
3. 直接对记录 PID 调用 Windows 进程终止；
4. 不调用 `Inspector.Inspect`，不比较路径、启动时间或配置摘要；
5. 等待退出通知或确认 PID 不再存在；
6. 超过现有 15 秒停止上限仍未退出时返回 `force_exit_timeout`；
7. 确认退出后清理 PID、认领摘要和等待任务，并发布 `stopped` 状态。

`ForceExitAll` 先调用 Helper，再调用 Agent。Helper 失败不阻止 Agent 处理，所有
失败组件在最后统一返回。

Agent 终止时继续依赖现有 Windows Job Object 的关闭语义结束 Worker。退出开始前
保存最后一次已知 Worker PID 列表；Agent 退出后等待这些 PID 不再存在。此处只
检查退出结果，不对 Worker 做路径或身份复核，也不直接向 Worker 发送控制命令。

### 5.4 UI 最后退出

Backend 增加进程内退出授权：

```go
exitAuthorized atomic.Bool
```

`onBeforeClose` 的第一项判断为：

```go
if b.exitAuthorized.Load() {
	return false
}
```

只有以下两条路径可设置该标志：

1. `ForceExitAll` 确认所有后台 PID 已退出；
2. 第二 NodeTray 实例发现已有实例并需要立即结束自身。

设置标志后调用 `runtime.Quit`。普通窗口关闭不会设置标志，因此仍按隐藏或弹出
确认对话框处理。Wails `OnShutdown` 继续关闭托盘控制器和后台监视器。

## 6. 配置保存与生效状态

### 6.1 返回模型

Agent 和 Helper 配置保存统一返回：

```go
type ConfigApplyResult struct {
	OK           bool   `json:"ok"`
	Saved        bool   `json:"saved"`
	Restarted    bool   `json:"restarted"`
	SHA256       string `json:"sha256"`
	NeedsRestart bool   `json:"needsRestart"`
	ErrorCode    string `json:"errorCode"`
	ErrorSummary string `json:"errorSummary"`
}
```

`Saved=true` 表示正式文件已成功替换并复读。`Restarted=true` 只在新实例已经达到
就绪状态后返回。保存成功但停止或启动失败时返回 `Saved=true`、
`Restarted=false`、`NeedsRestart=true`，使前端不再把已落盘配置误报为未保存。

### 6.2 `needsRestart`

`ComponentState` 增加：

```go
RuntimeConfigSHA256 string `json:"runtimeConfigSha256"`
SavedConfigSHA256   string `json:"savedConfigSha256"`
NeedsRestart        bool   `json:"needsRestart"`
```

计算规则：

```text
NeedsRestart = 组件正在运行
               且运行握手摘要非空
               且正式磁盘摘要非空
               且两个摘要不同
```

前端只显示后端返回的真实摘要和 `NeedsRestart`。删除当前基于非敏感表单序列化
结果计算的“表单摘要 SHA-256”。摘要只显示短前缀，复制诊断时可包含完整摘要，
但不得包含配置内容。

### 6.3 原子保存补强

现有临时文件、权限限制、内容同步、临时文件复读、`.last-good` 备份、原子替换和
目录同步保留。目录同步完成后必须重新通过严格 loader 读取正式目标，并验证规范
JSON 与本次保存内容一致。正式文件复读失败时返回 `save_verify_failed`，不得启动
或重启组件。

## 7. 程序设置差异应用

`SaveTraySettings` 首先加载旧设置并计算差异：

- `LoginStartTray` 未变化时不访问 HKCU；
- `HelperEnabled` 和 `HelperStartMode` 均未改变时不调用计划任务提权；
- 刷新间隔、通知级别和关闭到托盘等普通设置变化时不弹 UAC。

执行顺序：

1. 校验新设置；
2. 加载旧设置并计算登录启动、Helper 策略和普通偏好的差异；
3. Helper 策略变化时执行一次安装或删除固定计划任务操作；
4. UAC 取消或计划任务失败时立即返回，不保存新设置，也不继续修改登录启动；
5. 登录启动变化时启用或禁用当前用户登录启动项；
6. 保存完整托盘设置；
7. 重新读取磁盘设置、登录启动和任务状态，刷新界面。

不实现跨文件、注册表和计划任务的通用回滚事务。如果第 5 或第 6 步失败，返回
`settings_partially_applied`，前端立即重新加载实际状态并显示未应用项；不显示
虚假的保存成功。

## 8. 可取消生命周期监督器

### 8.1 非阻塞 Actor

Supervisor 仍由单一 actor 拥有状态，但耗时启动、停止和等待不再直接占用命令
循环。actor 保存一个活动操作：

```go
type activeOperation struct {
	kind   operationKind
	cancel context.CancelFunc
	done   chan traymodel.OperationResult
}
```

启动命令在 actor 内完成状态检查、设置 `starting` 和创建取消上下文，然后把启动
与就绪等待交给操作 goroutine。goroutine 只通过内部完成事件把结果交还 actor，
不能直接修改 Supervisor 状态。

### 8.2 启动取消

在 `starting` 状态收到停止请求时：

1. 调用活动启动操作的 `cancel`；
2. 若已经记录本次启动 PID，直接终止该 PID；
3. 等待退出或达到 15 秒上限；
4. 清理活动操作和组件记录；
5. 发布 `stopped`，并向原启动调用返回 `start_cancelled`。

重复启动和启动期间的重启返回 `operation_conflict`。停止用于取消启动，不需要
额外新增前端“取消等待”API；按钮文案在 `starting` 状态显示为“取消启动”。

### 8.3 重启

重启是 actor 内部编排的单一活动操作：

1. 优雅停止当前实例；
2. 只有确认旧实例退出后才启动新实例；
3. 停止失败或超时时返回失败，不创建新进程；
4. 重启过程中拒绝第二次启动或重启；
5. 停止阶段允许用户取消重启并保留当前可确定状态。

`SaveAndRestartAgent` 继续使用配置工作流互斥锁，防止另一个保存操作插入“保存、
停止、启动”中间。

## 9. 托盘消息循环

Win32 `windowProc` 和 `dispatchNativeEvent` 只负责菜单显示、命令校验和投递。选中
生命周期命令后，把命令提交到独立 Go 执行器并立即返回消息循环；不得在锁定的
OS 线程上等待 Wails、Supervisor、UAC 或进程退出。

执行器允许不同组件操作并发投递，但最终冲突判断由各自 Supervisor actor 完成。
同一组件的按钮和托盘菜单根据 `starting/running/stopping` 状态禁用不合法操作。

退出菜单是例外入口：它只显示控制台并发送前端事件，真正的强制退出从确认弹窗
调用后端，因此不会在托盘消息线程中执行。

## 10. 前端交互

### 10.1 强制退出弹窗

删除现有“仅退出托盘程序”“停止组件后退出”和停止超时后的多分支选择，统一为：

- `取消`；
- `强制退出全部后台组件并关闭界面`。

存在未保存草稿时明确提示草稿会丢失。确认按钮只调用一次 `ForceExitAll`。请求
期间按钮禁用；失败时弹窗保持打开并显示失败组件和“重试强制退出”。

### 10.2 配置状态

- 保存成功显示真实磁盘摘要短前缀；
- `NeedsRestart=true` 时显示“配置已保存，需要重启后生效”；
- 保存并重启的停止或启动失败时显示“配置已保存，但重启失败”；
- 程序设置应用失败后立即重载实际设置，不保留错误的已提交表单状态；
- Agent、Helper 和总览页共用后端生命周期状态禁用冲突按钮。

## 11. 错误处理

新增或固定以下稳定错误码：

| 错误码 | 含义 |
|---|---|
| `force_exit_failed` | 至少一个记录组件终止失败 |
| `force_exit_timeout` | 强制终止后后台 PID 在 15 秒内仍存在 |
| `save_verify_failed` | 原子替换后正式文件严格复读失败 |
| `settings_partially_applied` | 外部设置已部分应用但完整保存未完成 |
| `start_cancelled` | 用户在 `starting` 阶段取消本次启动 |
| `operation_conflict` | 同一组件已有不兼容的活动操作 |

强制退出错误聚合组件名称，不包含路径、配置内容、命令行、密码或控制令牌。
失败后 UI 保持可操作，用户可重试或取消弹窗。

## 12. 测试设计

### 12.1 Go 单元测试

必须覆盖：

1. 未授权时 `OnBeforeClose` 继续阻止退出；授权后返回 `false`；
2. 第二实例设置退出授权并可完成 Wails 退出；
3. `ForceExitAll` 按 Helper、Agent、Worker、UI 顺序完成；
4. Helper 失败后仍处理 Agent，失败组件正确聚合；
5. 任一后台 PID 仍存在时不调用 Wails `Quit`；
6. `ForceStopTracked` 不调用 Inspector 的身份复核；
7. 强制终止只有在退出确认后才把状态置为 `stopped`；
8. 正式配置替换后复读成功、复读失败和摘要不一致；
9. 保存返回真实摘要及运行中、已停止两种 `NeedsRestart`；
10. 保存成功但重启失败返回 `Saved=true` 和 `NeedsRestart=true`；
11. 普通设置变化不访问 UAC、计划任务或无关 HKCU；
12. Helper 策略变化只调用一次提权，UAC 取消时不保存新设置；
13. `starting` 状态的停止可以取消启动并清理已创建进程；
14. 重启停止失败时不调用启动器；
15. 托盘消息线程投递命令后立即返回，不等待生命周期结果。

涉及 actor 并发的包使用 `go test -race` 运行；Windows 固定管道测试必须在没有
真实 Agent 占用管道的隔离会话中运行，或作为独立串行门禁运行。

### 12.2 前端测试

必须覆盖：

1. 三种退出入口只打开一个强制退出弹窗；
2. 取消不调用后端；
3. 确认只调用一次 `ForceExitAll`；
4. 后台失败时 UI 不关闭并显示失败组件；
5. 未保存草稿提示；
6. 保存后真实摘要和 `NeedsRestart` 文案；
7. 启动中按钮变为“取消启动”，冲突按钮禁用；
8. 程序设置失败后重载实际值。

前端门禁包括 Vitest、ESLint、TypeScript 检查和 Vite 生产构建。

### 12.3 Windows 动态验收

动态验收在明确授权的隔离 Windows 会话中执行：

1. 修改并保存 Agent/Helper 配置；
2. 启动、启动中取消、停止和重启；
3. 托盘操作期间通知区域仍响应；
4. 普通设置保存不弹 UAC；
5. Helper 策略变化的 UAC 取消和同意；
6. 强制退出确认后 Helper、Agent、Worker 先退出，NodeTray UI 最后退出；
7. 模拟一个后台终止失败，确认 UI 保持并可重试；
8. 第二实例可以立即退出且不留下隐藏进程。

未执行真实进程、UAC、计划任务或 HKCU 场景前，对应状态继续记录为
`BLOCKED_NOT_RUN_DYNAMIC`，不得用单元测试结果替代。

## 13. 验收标准

- 明确退出必须经过弹窗确认；
- 确认后直接强制结束监督器记录的后台进程；
- Helper、Agent、Worker 全部确认退出后才结束 NodeTray UI；
- 取消退出不改变任何进程；
- 普通配置设置不触发无关 UAC；
- 保存结果包含真实摘要和准确的 `NeedsRestart`；
- 启动可取消，启动、停止、重启不创建并行实例；
- 托盘消息循环不被生命周期等待阻塞；
- Wails 程序化退出不再被 `OnBeforeClose` 自身否决；
- 现有原子保存、备份、Worker 所有权、Helper 删除边界和脱敏合同不回归。

## 14. 对既有设计的覆盖关系

本设计对以下既有行为作定向覆盖：

- 覆盖 `2026-08-03-node-tray-exit-all-design.md` 中“托盘右键退出直接执行后台
  停止”的入口，改为先打开统一确认弹窗；
- 覆盖 `2026-08-02-node-tray-design.md` 中退出时先优雅停止、超时后另行确认的
  流程，改为一次确认后直接强制结束全部记录后台进程；
- 覆盖退出时必须再次复核 PID、启动时间和路径的要求，改为直接使用监督器记录
  的 PID；
- 不覆盖普通停止和重启的优雅停止要求；
- 不覆盖窗口关闭到托盘、Agent 管理 Worker、固定 Helper 任务以及配置脱敏要求。

仓库当前没有可用 Git 元数据，因此本规格不记录或伪造提交号；版本标识统一为
`N/A_NO_GIT_METADATA`。
