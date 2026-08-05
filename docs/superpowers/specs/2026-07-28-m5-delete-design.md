# M5 安全删除链路设计

**状态：** 已批准设计的书面版本  
**日期：** 2026-07-28  
**依据：** `docs/details/M5-delete.md`、`docs/architecture-plan.md`、现有
M1–M4 实现  
**前置：** M1–M4 已完成；第二台独立 Windows 验证由项目所有者豁免

## 1. 目标

M5 为现有重复组页面增加安全、可审计的删除闭环：

1. 用户在 GUI 中选择重复组成员；
2. GUI 服务端重新查询中心库，生成删除摘要和 60 秒一次性确认令牌；
3. 用户在确认弹窗中显式选择软删或硬删，默认软删；
4. GUI 按 Agent 路由任务；
5. 普通权限 Agent 通过本机受限命名管道转交给管理员权限 Helper；
6. Helper 在本地安全策略允许的目录内逐项处理；
7. Agent 逐项记录审计、更新 SQLite 并同步中心库；
8. GUI 展示进度和结果，已删除成员从组中消失。

验收只操作运行时生成的隔离测试文件。`I:\tmp` 和
`H:\pik\00000000000` 始终是只读媒体来源，不进入 Helper 白名单，也不
参与任何删除测试。

## 2. 方案选择

### 2.1 采用：常驻管理员 Helper + 普通 Agent 转发

Helper 是唯一持有删除权限的进程。它由用户或部署流程显式以管理员
身份启动，使用 `requireAdministrator` manifest，并在本机命名管道上
常驻服务。Agent 只负责拨号、分块转发、审计和状态同步，不触发 UAC，
也不启动 Helper。

该方案把高权限面限制在一个小进程内，白名单与拒绝目录只来自
Helper 本地配置，网络侧无法扩大权限范围。

### 2.2 未采用：Agent 按任务自动 UAC 拉起 Helper

它减少一次人工启动，但把提权触发放入网络驱动的普通 Agent 路径，
容易造成无人值守机器上的弹窗、重复拉起和不清晰的失败语义。文档中
关于“自动提权、空闲退出、自动重启”的示例与 H10/A1 的常驻、只拨号
约束互相矛盾；本设计以已批准的 H10/A1 方向为准。

### 2.3 未采用：GUI 或 Agent 直接删除

GUI 无法安全处理远端本地文件；让 Agent 本体长期以管理员身份运行会
扩大扫描、网络和 Worker 代码的权限面。两者均不采用。

## 3. 明确的适配决策

以下决策解决 `M5-delete.md` 内部不一致，并作为实施计划的权威输入：

- Helper 默认模式为 `soft`，GUI 也默认选中软删。
- 硬删必须由用户在第二次确认中显式选择；`allow_hard_delete=false`
  时 Helper 拒绝所有硬删任务。
- Agent 不调用 `ShellExecute("runas")`，不自动启动或重启 Helper。
- Helper 不因连接断开或空闲而退出；仅显式 `Shutdown`、控制台退出或
  服务管理操作可终止它。
- Helper 不在线时，Agent 在 500ms 拨号失败后为所有未处理项生成
  `E_HELPER_LOST`，提示用户以管理员身份启动 `helper.exe`。
- Helper 在任务中死亡时，当前及剩余分块全部闭环为
  `E_HELPER_LOST`；用户手动重启 Helper 后，必须重新选择并确认任务，
  系统不自动重放可能已部分执行的破坏性请求。
- M5 不承诺跨 GUI/Agent 进程重启的 exactly-once 删除。任何发送结果
  不确定的任务显示为不确定状态，禁止自动重放；`delete.log` 和实际
  文件状态是人工核对依据。

## 4. 组件边界

### 4.1 `internal/proto`：兼容扩展现有消息

现有 `MsgDeleteTask=13`、`MsgDeleteReport=25`、`DeleteTask`、
`DeleteReport` 和统一 16MB 帧实现必须复用，不能建立第二套字符串消息
协议或第二份帧编解码器。

协议继续使用 uint8 消息类型和 msgpack map：

- `DeleteTask` 保留原 `task_id` 和 `entries []string`，追加
  `seq`、`last_seq`、`mode`、`confirmed`，新字段使用向后兼容 map
  编码。
- `DeleteResult` 保留 `path`、`ok`、`err`，追加 `err_code`、
  `readonly_cleared`、`recycled_to`。
- `DeleteReport` 追加 `seq`、`last_seq` 和统计字段。
- `Hello` 追加可选 `role`、`pid`；Helper 使用现有 `MsgHello`，
  `role=delete-helper`。旧 Agent/GUI 对新增字段可忽略。
- 新增一个未占用的 `MsgShutdown` 数值和最小 `Shutdown` 载荷，仅供
  本机管道运维使用；现有消息编号不得改号或复用。

稳定错误码采用详细文档列出的 12 个值。`err_code` 用于程序判断，
`err` 只提供人读上下文，不作为控制流依据。

### 4.2 `internal/helper`：最小高权限核心

Helper 拆为清晰的独立单元：

- 配置加载与规范化；
- 路径安全验证；
- 软删/硬删执行器；
- 命名管道监听与协议处理；
- 单实例互斥体；
- `helper.log` 滚动 JSON 日志。

Helper 不连接 PostgreSQL，不访问 Agent SQLite，不接收网络连接，不
解释 GUI 令牌，也不扫描目录。它只处理已确认的单文件清单。

### 4.3 `cmd/helper`：部署入口

入口只负责：

- 加载并严格校验 `helper.json`；
- 获取 `Local\DedupDeleteHelperMutex`；
- 创建带 SDDL 的命名管道服务；
- 响应显式关闭或进程信号；
- 确保日志与管道资源按序关闭。

manifest 使用 `requireAdministrator`。构建产物必须验证 manifest
确实嵌入 `helper.exe`，而不是只检查源文件存在。

### 4.4 `internal/agent/delete`：普通权限转发器

转发器接收 GUI 发来的 `DeleteTask`，按最多 2000 项拆分，在一次任务
内建立一个 Helper 会话并顺序交换请求/回执。连接结束后立即关闭，
Helper 继续常驻。

每一回执项按以下顺序处理：

1. 写一行 `delete.log`；
2. 将成功路径在一个 SQLite 事务中标记为 `deleted` 并推进
   `sync_queue` generation；
3. 将完整分块回执发回 GUI。

数据库写失败不能改写 Helper 的物理操作结果；回执必须明确带上本地
状态同步失败，日志保留物理结果与数据库错误，避免把“文件已删但状态
未同步”误报为未删除。

### 4.5 `internal/gui`：确认、派发和结果聚合

GUI 删除模块包含三个边界：

- `ConfirmStore`：随机 128-bit 令牌、60 秒 TTL、一次性消费；
- `DeleteService`：数据库解析、摘要、按 Agent 派发和任务聚合；
- HTTP handlers：输入限制、状态码和 JSON 响应。

重复组页面仅负责选择和展示，不自行信任路径、文件大小或 Agent ID。

## 5. 安全模型

### 5.1 二次确认

`POST /api/delete/prepare` 接收所选成员 ID，而不是任意客户端路径。
服务端从 PostgreSQL 重新解析仍存活的组成员，得到
`machine_id/path/size`，去重并稳定排序，再生成：

- 文件总数；
- 总字节数；
- 每 Agent 数量；
- 最多 20 条样本路径；
- 一次性确认令牌。

令牌在服务端保存规范化清单的深拷贝和摘要，客户端不能在 execute
阶段替换路径。过期令牌返回 400，已消费令牌返回 409。

`POST /api/delete/execute` 先验证 mode，再原子消费令牌。mode 只能是
`soft` 或 `hard`；缺省采用 `soft`，永不缺省为硬删。服务端只有在令牌
有效时才构造 `confirmed=true`。

### 5.2 Helper 白名单

`allowed_roots` 必填且启动时规范化。任何空项、相对路径、UNC、设备
路径、卷相对路径或宽泛系统目录配置都应 fail closed。
`denied_roots` 优先于允许项。

目标处理前必须：

- 是带盘符的绝对文件路径；
- 命中目录边界安全的允许根；
- 不命中任何拒绝根；
- 目标不是目录；
- 目标及允许根到目标之间的所有现有组件都不是 reparse point；
- 软删目标 `$DedupRecycle` 的既存组件也不是 reparse point。

验证在实际变更前再次执行，以缩小检查与使用之间的窗口。M5 不做目录
递归删除，也不跟随 junction、symlink 或其他 reparse point。

### 5.3 命名管道 ACL

管道仅允许 SYSTEM、Administrators 和启动 Helper 的本地用户 SID，
显式拒绝 NETWORK。Agent 通常由同一用户的非提权令牌运行，因此 SID
一致。若 Helper 由另一个管理员账户启动，普通 Agent 被拒绝是预期的
安全失败。

Helper 接受连接后先发送带角色与版本的 Hello；Agent 在 5 秒内验证
角色和协议版本，不接受任意同名管道服务。

## 6. 删除语义

### 6.1 软删

软删是默认路径。目标移动到同一卷：

`<卷>:\$DedupRecycle\<task_id>\<卷内相对路径>`

同名冲突使用确定性的 `_1`、`_2` 后缀。只允许同卷 `Rename`，不实现
“复制后删除”的跨卷退化。成功回执必须包含实际 `recycled_to`，便于
人工恢复；M5 不实现自动恢复或自动清空回收目录。

### 6.2 硬删

硬删前读取全部文件属性。若仅有只读位阻挡操作，则清除
`FILE_ATTRIBUTE_READONLY`，保留其他属性位，然后删除单个文件。
`readonly_cleared=true` 只表示本次确实清除了该位。

任一项失败不阻断同帧其他项。错误按 Windows 结果稳定映射为
`E_NOT_FOUND`、`E_IN_USE`、`E_ACCESS_DENIED`、`E_READONLY` 或
`E_DELETE_FAILED`。

### 6.3 整帧拒绝

以下情况整帧拒绝且不触碰任何目标：

- `confirmed=false`；
- mode 非法；
- hard 被本地策略禁用；
- 条目数超过本地上限；
- task/seq 结构无效；
- 同一路径在一帧中重复；
- 请求同时包含冲突的任务元数据。

路径越权或单文件错误仍按项返回，使其他安全条目可以完成。

## 7. 数据与状态流

成功删除的 Agent 本地状态在一个事务中更新：

- `files.status='deleted'`；
- 更新时间；
- 对应 `sync_queue` 行按现有 generation 语义置为待同步。

特征表不删除，因为相同 SHA-512 的其他副本仍可能使用这些特征。
中心库收到文件状态后，现有 M3/M4 查询和 groups API 已排除
`status='deleted'`。

GUI 用 `(task_id, machine_id, seq)` 聚合回执并提供轮询状态 API。前端
展示总数、成功、失败、未确定和错误码分布。M5 不新增 WebSocket；
沿用 HTTP 轮询以控制范围。

## 8. 错误与恢复

- Helper 不在线：全部未处理项 `E_HELPER_LOST`，任务闭环。
- 管道在分块中断：已收到的回执保留；当前及后续块
  `E_HELPER_LOST`。
- Agent 到 GUI 回执失败：本地 `delete.log` 和 SQLite 结果保留；不
  自动重新执行删除。
- Agent SQLite 更新失败：物理结果不回滚，记录审计并向 GUI 标出
  状态同步失败；同步可由后续对账修复。
- GUI 对部分 Agent 派发失败：成功派发的机器继续执行，失败机器显示
  未派发；令牌已消费，重试必须重新 prepare/confirm。
- Helper 收到显式 Shutdown：完成当前帧后停止接收新任务并退出。

所有日志和 HTTP 错误都不得包含确认令牌或数据库凭据。

## 9. UI

现有三组页面增加：

- 每个非代表成员的复选框；
- 跨组已选数量与总大小；
- “删除所选”按钮；
- 二次确认弹窗；
- 默认选中的“软删”和需主动选择的“硬删”；
- 硬删红色不可恢复警告；
- 任务进度、逐机器/逐分块状态和失败清单。

路径与错误文本继续只用 `textContent` 渲染。代表文件默认不可选；
若允许删除代表文件，服务端必须在 prepare 时重新选择仍存活代表，
不能仅靠前端禁用状态保证。

## 10. 配置与部署

`helper.example.json` 默认：

- `default_mode=soft`；
- `allow_hard_delete=true`；
- `allowed_roots` 为空示例并要求部署者填写；
- `denied_roots` 示例包含系统卷信息和系统回收站；
- `max_entries_per_frame=2000`；
- 帧读超时 120 秒、回执写超时 60 秒。

Agent 只配置 pipe name、分块上限和审计日志位置，不需要
`helper.exe` 路径或提权参数。发布目录包含 `helper.exe`、配置示例和
启动说明，但不包含真实白名单、密码或令牌。

## 11. 测试与验收

### 11.1 测试层次

- 协议：旧/new msgpack map 兼容、16MB 边界、字段类型和错误码。
- Helper 单元：配置、目录边界、UNC/设备路径、所有祖先 reparse、
  只读属性、软删冲突和逐项错误映射。
- Agent 单元：2000 分块、Hello、管道中断、逐项审计、SQLite 原子
  更新和回执闭环。
- GUI 单元/API：成员 ID 解析、令牌 TTL/单用/并发消费、mode、
  部分派发、轮询状态、XSS。
- Windows 集成：真实命名管道、SDDL、单实例、manifest、Helper
  死亡、只读硬删和软删。
- E2E：真实 GUI→Agent→Helper→SQLite→PostgreSQL→GUI 状态闭环。

### 11.2 安全验收目录

删除 E2E 使用运行唯一的临时目录和动态 `subst` 盘符，使
`$DedupRecycle` 也落在测试根内。控制器在执行前后记录文件哈希、属性、
reparse 状态和目录清单，并在结束后验证：

- 测试根之外没有文件变化；
- `I:\tmp`、`H:\pik\00000000000` 未被枚举或写入；
- 临时盘符、管道、Helper/Agent/GUI 进程和测试目录残留为 0；
- PostgreSQL scoped schema 残留为 0。

### 11.3 对详细文档 TC 的适配

- TC-01～TC-08 保留，其中默认流程先验软删，TC-01 明确选择 hard。
- TC-09 改为“Helper 未运行”：不出现 UAC；全部
  `E_HELPER_LOST`；用户手动启动 Helper 后重新确认可成功。
- TC-10 在同机验证合法 SID、拒绝 SID 和 NETWORK deny；第二台
  Windows 状态记录为 `USER_WAIVED`。
- TC-11 改为“常驻”：任务结束和连接断开后 Helper 仍在线，显式
  Shutdown 后退出。
- TC-12 保留任务中 Helper 死亡，但下一任务仅在用户手动重启 Helper
  后执行，不自动提权重启。

## 12. 非目标

M5 不实现：

- 目录递归删除；
- 自动清理或自动恢复 `$DedupRecycle`；
- Agent 自动 UAC；
- Helper Windows Service 安装器；
- 远程文件预览；
- 跨进程 exactly-once 删除重放；
- 对用户提供的 I/H 媒体集执行删除；
- M6 性能指标或压测功能。

## 13. 完成条件

M5 只有在以下条件全部满足后才能标记完成：

- P1–P3、H1–H11、A1–A7、G1–G5、T1–T3 全部有可追溯证据；
- TC-01～TC-12 按本设计适配后通过或明确记录仅第二 Windows 豁免；
- 独立审查无未关闭 Critical/Important；
- final fail-closed controller 全部门 PASS；
- 删除测试只触及隔离生成文件，用户媒体未修改；
- scoped schema、临时盘符、进程、管道和测试目录残留均为 0；
- `SECOND_WINDOWS_STATUS=USER_WAIVED` 写入验收记录。
