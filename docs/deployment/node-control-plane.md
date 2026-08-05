# 节点本机控制面

节点控制面是 Agent 和删除 Helper 暴露给同机管理程序的轻量生命周期接口。
它只负责读取状态和请求受控关闭，不提供远程控制，不替代 Agent 的业务 TCP
协议，也不允许管理程序直接操作 Worker。

当前仓库已经实现控制面后端；常驻托盘程序、交互式配置页和开机启动选项仍是
后续工作，不能把本文当作托盘 UI 已交付的说明。后续实现计划见
[托盘后端计划](../superpowers/plans/2026-08-02-node-tray-backend.md)和
[托盘 UI 与发布计划](../superpowers/plans/2026-08-02-node-tray-ui-release.md)。

## 固定端点

| 组件 | 固定命名管道 | 命令 |
|---|---|---|
| Agent | `\\.\pipe\mysingerserver-agent-control-v1` | `status`、`shutdown` |
| 删除 Helper | `\\.\pipe\mysingerserver-helper-control-v1` | `status`、`shutdown` |

Helper 的控制管道与 `helper.json` 中的删除事务管道是两个端点。删除事务继续使用
既有协议；历史 Agent 发送的 `MsgShutdown` 仍只在删除协议上保持兼容。不得把
两根管道合并，也不得从 Agent 的业务 TCP 分派器增加生命周期命令。

命名管道 DACL 显式拒绝网络登录 SID，只允许启动进程的当前用户、
`Administrators` 和 `SYSTEM`。因此控制面边界是同一 Windows 主机；防火墙、
端口转发或反向代理都不能把它变成远程管理接口。同一组件的第二个控制 listener
还会被本机会话互斥量拒绝。

## 协议与兼容性

控制协议当前版本是 `1`，使用四字节大端长度前缀和 MessagePack 负载，单帧上限
为 1 MiB。每个连接只处理一个请求；请求必须包含版本、随机请求 ID 和命令，响应
必须回显请求 ID。`shutdown` 会先返回成功响应，再触发组件已有的受控退出和排空
流程。

`status` 的稳定用途是识别组件、进程、可执行文件、配置指纹、生命周期和就绪
状态。Agent 还报告 Worker 汇总与同步健康；Helper 只报告删除 listener 和活动
事务数，不报告 Worker。错误摘要会去除常见 DSN、密码、令牌和媒体路径。

版本号、固定管道名称、命令语义和现有字段含义属于兼容边界。托盘以外的客户端
不得依赖字段顺序、未写入本文的内部状态、错误文案或未来可能追加的字段；新增
字段只能向后追加，不能复用旧字段表达不同语义。

## 构建与静态验收

普通 `scripts/build.ps1` 在构建既有 `agent.exe` 和 `helper.exe` 前，会运行
`internal/nodectl`、`internal/agentcontrol` 和 `internal/helpercontrol` 的测试。
控制面不会增加新的发布二进制。

不启动任何进程的静态门禁：

```powershell
pwsh -NoProfile -File .\tests\windows\Test-NodeControlPlane.ps1 -WhatIf
```

`-WhatIf` 只读取仓库内的控制面源码和构建脚本，不创建目录、不启动或停止进程、
不触发 UAC、不访问 PostgreSQL，也不读取媒体目录。其输出中的动态状态固定为
`BLOCKED_NOT_RUN_DYNAMIC`，不能解释为动态验收通过。

## 动态验收边界

动态验收只能在得到运行进程和测试 PostgreSQL 的明确授权后执行，并且所有配置、
二进制和测试根都要显式传入：

```powershell
pwsh -NoProfile -File .\tests\windows\Test-NodeControlPlane.ps1 `
  -AgentConfig C:\tmp\agent.control-test.json `
  -HelperConfig C:\tmp\helper.control-test.json `
  -AgentExe D:\staging\agent.exe `
  -WorkerExe D:\staging\worker.exe `
  -HelperExe D:\staging\helper.exe `
  -VideoCoreDll D:\staging\videocore.dll `
  -TestRoot C:\tmp\mysingerserver-node-control-acceptance
```

测试配置必须让 Agent 仅监听 `127.0.0.1`，把 Agent 数据目录和 Helper 的窄范围
`allowed_roots` 指向该测试根，把 Helper 的非空绝对 `log_dir` 也放在测试根内，
显式配置传入的 Worker 路径，并关闭 Helper 硬删除。脚本拒绝既存测试根、根外
数据/日志目录、宽泛删除根和名称不符合
`C:\tmp\mysingerserver-node-control-*` 的目标；退出时只清理本次创建且重新验证
过的根。真实媒体目录不得写入测试配置。

动态脚本验证状态、Agent 单实例、受控退出与 Worker 进程树、TCP 控制隔离、
Helper 双管道、活动事务排空和凭据扫描。没有上述授权时，验收结论必须保持
`BLOCKED_NOT_RUN_DYNAMIC`，不得用静态通过替代。

## 运维注意事项

- `shutdown` 是受控退出请求，不是强制终止；调用方应等待进程退出并处理超时；
- Helper 有已接受删除事务时会停止接收新事务，并等待当前事务完成；
- 状态和日志不得包含 PostgreSQL DSN、密码、令牌、确认值或完整媒体路径；
- Agent/Helper 配置仍由受控部署流程管理，当前控制面没有交互式配置写入能力；
- Helper 的提权、白名单和软删除要求仍以 [Helper 部署说明](m5-helper.md)为准。
