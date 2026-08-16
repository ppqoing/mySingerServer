# 本地任务文字完整显示与视频 Worker 失败根因修复设计

## 背景与现场证据

生产目录为 `artifacts/releases/MySingerServer-Compute`，扫描任务
`local-1786866138372` 的根目录为 `I:\MiddleDir\11111111`。任务数据库和
Agent 日志给出一致证据：

- 共枚举 47,153 个文件，停止时已处理 728 个；
- 656 个成功，72 个失败；
- 72 个失败全部记录为 `crash / exit_code: exit status 2`；
- 失败文件为 71 个 MP4 和 1 个 MOV；
- 任务汇总为 `decode_calls=0`、`read_attempts=0`、`decode_attempts=0`；
- 同一分钟内多个 Worker 退出后被持续拉起。

因此失败发生在视频原生解码之前的 Worker 任务处理或 IPC 边界。它不是
Everything 枚举失败，也不能按单个损坏视频解释。任务卡的文字截断则由现有
单行 `white-space: nowrap`、`text-overflow: ellipsis` 和固定六列网格共同造成。

## 目标

1. 任务卡中的模式、创建时间、完整任务 ID、状态、阶段、进度数值、速度、
   失败数和耗时均能完整阅读。
2. 使用生产形状的 Worker IPC 测试和仓库内真实视频夹具，复现并定位
   `exit status 2` 对应的具体内部错误。
3. 修复具体错误源，使正常视频进入 VideoCore 会话管线并返回有效结果；可预期的
   文件级失败必须返回带阶段和原因的 `JobResultMsg`，不得终止 Worker。
4. 重新验证 Compute 包中的真实 `worker.exe`、`videocore.dll` 及其依赖闭包。

## 非目标与禁止方案

- 不新增“查看错误”按钮、错误弹窗或错误历史表。本轮“查看错误”是排查现场的
  72 次失败，而不是新增产品交互。
- 不切回旧视频管线，不禁用 VideoCore 会话管线。
- 不跳过 MP4/MOV，不把视频标记为不支持，不把失败计数清零。
- 不把真实 Worker 进程崩溃降级为普通文件错误来隐藏故障。
- 不修改 Everything 枚举策略，也不清理用户现有任务数据库和日志。

## 方案

### 任务卡布局

保留现有 `LocalTaskItem` 的语义和操作按钮，但把卡片调整为可重排布局：

- 宽屏时使用两层网格，第一层显示身份、状态和操作，第二层显示进度条、完整
  数值和完整指标；
- 任务 ID 不再主动截成 12 个字符，使用可断行文本显示完整值；
- 速度、失败数和耗时允许换行，不使用省略号；
- 窄屏时按身份、状态、进度、指标、操作纵向排列，按钮保持可点击且不覆盖文字；
- 保留完整任务 ID 的 `title`，作为辅助提示而非唯一查看方式。

数据类型、轮询、生命周期操作和竞态保护保持不变。

### 视频失败定位与修复

先建立能走生产默认分发路径的回归测试：

1. 使用仓库内真实 H.264 MP4 夹具构造 Phase 1 视频任务；
2. 通过真实 Worker IPC 消息顺序执行 `Job -> SHAQuery -> SHAReply -> Result`；
3. 使用生产默认的 VideoCore 会话依赖，不注入旧 `videoPipelineDeps`；
4. 断言 Worker 不退出、返回 `MsgResult`，并包含 SHA-512、时长和接触表结果；
5. 在 RED 输出中保留导致 Worker 返回代码 2 的精确内部错误，沿
   `serve -> processMediaWithDeps -> SHA 查询/回复 -> VideoCore Analyze` 逐层确定
   首个违约边界。

只修复该首个违约边界。若根因是 IPC 回复字段、掩码或缓存载荷不完整，则修正
回复构造方并维持严格验证；若根因是 VideoCore 请求或结果映射错误，则修正对应
映射并保留 ABI 与状态校验。不会以放宽校验、忽略错误或回退旧管线换取测试通过。

对于可预期的打开、哈希、探测、解码、抽帧或接触表失败，Worker 返回带
`FieldError.Stage` 和安全原因的结果，进程继续服务下一任务。只有真正的进程异常、
IPC 破坏或不可恢复的内部契约错误继续进入 crash 监督路径。

## 数据流与边界

```text
LocalTasksPage
  -> LocalTaskItem（仅调整展示，不改变控制参数）

Agent ScanManager
  -> WorkerPool
  -> worker.exe 默认分发
  -> VideoCore 会话打开/哈希
  -> SHAQuery / SHAReply
  -> VideoCore Analyze
  -> JobResultMsg
  -> Store 文件状态与任务进度
```

任务 ID、实例 ID、revision 和原有轮询合并规则不变。视频结果仍由 Store 的现有
SHA、特征和错误校验约束写入，不能绕过持久化契约。

## 错误处理

- 测试和诊断输出必须暴露 Worker 返回 2 之前的内部错误，避免只留下退出码。
- 文件路径只在本机测试断言和现有本地数据库范围内使用；日志继续使用现有路径
  脱敏规则。
- 文件级媒体错误必须保留阶段，例如 `native_open`、`native_hash`、
  `video_probe`、`video_frame` 或 `video_contact_sheet`。
- 内部协议不一致仍是硬错误，但必须由测试证明具体不一致项并从生产方修正。

## 测试与验收

### 自动化

- React Testing Library：完整任务 ID、指标不被截断；宽屏和窄屏 DOM 顺序稳定；
  生命周期按钮和回调参数不变。
- Worker 单元测试：生产默认 Phase 1 视频必须选择会话管线。
- Worker 真实夹具测试：H.264 MP4 完整 IPC 往返不退出，生成有效基础视频结果。
- 错误路径测试：可预期媒体失败返回文件级结果；真正协议错误仍被拒绝。
- 受影响 Go 包、NodeTray 前端全量测试、lint、build 和相关 race 测试通过。

### 生产包

- 重建 `worker.exe` 和发生变化的原生组件；
- 验证 `worker.exe` 只加载发布清单允许的原生依赖；
- 更新 Compute 解压目录并生成新的 Compute ZIP、Manager ZIP 与 SHA-256 sidecar；
- 用仓库真实 MP4 夹具扫描，确认 Worker 不重启、文件成功；
- 在交互用户权限允许时重新扫描 `I:\MiddleDir\11111111`，确认不再出现成批
  `exit status 2`。若 Codex 沙箱仍无 I 盘权限，明确标记该项为 PARTIAL，不以
  合成路径冒充真实目录验收。

## 完成标准

- 截图中的任务卡信息可以直接完整阅读；
- 72 次视频失败的具体代码根因有 RED 证据、修复提交和 GREEN 证据；
- 不含临时回退、跳过或弱化校验；
- 新发布包清单、文件哈希和运行时视频扫描验证一致。
