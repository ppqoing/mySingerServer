# 双主机六项缺陷修复设计

## 1. 目标

修复 `docs/details/2026-08-14-bug-investigation.md` 最新双主机复核中记录的六项缺陷：`DH-P0-01`、`DH-P0-02`、`DH-P0-03`、`DH-P0-04`、`DH-P1-01` 和 `DH-P2-01`，并使用相同双主机与真实媒体目录重新验收。

本设计不处理旧列表中的 B9、B11、B13–B27，也不重构与六项缺陷无关的模块。

## 2. 强制安全边界

- 本机真实媒体根为 `I:\MiddleDir\11111111`，远程真实媒体根为 `D:\tmp\-------2-4`。
- 真实媒体目录只用于扫描、特征提取、预览和只读分析，不执行删除或内容写入。
- 删除验收只能针对任务创建的隔离副本，验收结束后仅清理该副本。
- 当前主检出包含用户未提交改动。修复采用文件和代码块白名单，不执行 `git reset`、工作区清理、宽泛暂存或覆盖式恢复。
- 正式 Worker 继续使用 `CGO_ENABLED=1`、MinGW GCC 和 `-tags nodynamic`；不重新引入 `legacy_mediacore` 生产依赖。
- 实现保持简洁：优先复用现有状态、表结构和接口，只添加六项根因修复必需的分支与数据，不增加无关防御层、兼容层或大范围重构。

## 3. 总体架构

修复分成三条彼此独立、最终联合验收的链路：

1. **Worker 媒体处理链路**：消除旧 MediaCore 生产路由，并修正 VideoCore cgo 临时路径所有权。
2. **本地分析与删除安全链路**：把任务 Roots 传递到分析候选源，并在物理删除前持久化可恢复的删除意图。
3. **Manager 状态恢复链路**：从 PostgreSQL 恢复有界任务历史，并让运行状态接口读取实时 Agent 连接池快照。

每项缺陷先增加能稳定复现错误行为的回归测试，确认测试按预期失败后，再实施最小生产修改。

## 4. Worker 媒体处理链路

### 4.1 DH-P0-01：阶段一图片统一使用 Session 管线

`internal/wproc/run.go` 在 `useSessionPipeline=true` 时，不再把阶段一图片分流到 `processImageWithDeps`。阶段一、阶段二的图片和视频均通过 `processMediaWithDeps`，预览阶段仍保持独立内存编码路径。

旧图片管线继续保留给明确注入旧依赖的单元测试和非 Session 兼容路径，但正式 Worker 默认路径不能调用 `internal/wproc/mediacore` stub。`scripts/build.ps1` 不增加 `legacy_mediacore` tag，也不要求发布包携带 MediaCore DLL。

回归测试必须通过真实 `serve` IPC 调度证明：当 Session 依赖存在时，阶段一图片打开 VideoCore Session、产生 64 字节 SHA 和图片特征，并且旧 `decode` 依赖没有被调用。

### 4.2 DH-P0-02：C 内存持有临时 JPEG 路径

`internal/wproc/videocore/bindings.go` 不再把 Go `[]uint16` 的数据指针写入随后传给 C 的请求结构。非空 `TempJPEGPath` 先完成 UTF-16 校验，再使用 `C.malloc` 分配调用期内存、复制 code units，并通过 `defer C.free` 在 `vc_media_analyze` 返回后释放。

空路径保持空指针和零长度；分配失败返回文件级 native 错误，不能 panic 或退出 Worker。已有嵌入 NUL 校验保持不变。

测试分两层：

- Windows+cgo 边界测试覆盖非空 Unicode 临时路径，证明传入 C 的请求不再包含 Go 指针。
- 正式参数构建的 Worker 对真实 MP4 生成 contact sheet，证明不出现 `Go pointer to unpinned Go pointer`，Worker 处理完任务后仍存活。

## 5. 本地分析与删除安全链路

### 5.1 DH-P0-03：Roots 端到端绑定

`agentLocalTaskRunner` 在“扫描 + 本地分析”模式下复制 `request.Roots`，并调用 `RunWithProgressForRoots`。Roots 不允许通过共享切片被后续调用方修改。

`internal/localanalysis` 增加 Roots 作用域候选源：

- Roots 必须非空、为绝对路径、不能包含 `..`，且不能是盘符根目录。
- Windows 路径比较大小写不敏感。
- 使用 `filepath.Rel` 做目录边界判断，`D:\media2` 不能匹配 `D:\media`。
- `StreamActiveFiles` 只向分析器传递位于授权 Roots 内的文件；特征加载仍仅针对已通过筛选的文件哈希。
- 非法 Roots 在创建分析 run、写入候选或调用 Worker 前失败关闭。

已有提交 `2c5cdecf`、`3b4ad25d`、`85bde546` 中的已验证合同作为参考，但需要适配当前工作区接口和用户未提交修改，不能直接覆盖文件。

### 5.2 DH-P0-04：删除前持久化意图

删除流程调整为以下顺序：

1. 再次读取已提交复核结果并验证文件身份。
2. 在一个 SQLite 事务中创建 `local_delete_batches` 的 `running` 记录和每个文件的 `pending` item；事务同时验证 current analysis、review、文件 SHA、大小和 mtime。
3. 只有该事务成功后，才调用 Helper 执行物理删除。
4. 根据 Helper 报告更新既有 batch/items；只有明确且确定的成功项才把 `files.status` 标记为 `deleted` 并写入 outbox。
5. Helper 失联、报告缺失或结果提交失败时，已持久化项目保持 `pending` 或转为 `uncertain`，不得伪装成成功。

未收口的 `pending`/`uncertain` 文件必须从新的活动分析候选和重复删除选择中隔离，避免崩溃后再次把同一文件当作普通 active 文件处理。状态接口需返回这些记录，供后续人工或自动对账；本轮不根据“路径不存在”自动宣判删除成功。

存储接口拆分为“开始删除批次”和“完成删除批次”两个事务边界。开始事务失败时必须证明 Helper 调用次数为零；完成事务失败时必须证明预写 journal 仍存在。

## 6. Manager 状态恢复链路

### 6.1 DH-P1-01：恢复有界终态任务历史

`TaskRegistry.Restore` 从 PostgreSQL 恢复：

- 所有 `sent`、`acked`、`running` 扫描任务；
- 按 `updated_at DESC, id DESC` 排序的最近 200 条 `done`、`failed` 扫描任务。

只恢复 `target.type` 缺省或等于 `scan` 的记录，不能混入 phase2/analysis 任务。恢复时解析 `stats_json`，重新填充 done、total、skipped、failed、scan_errors、elapsed 和 recent 等用户可见字段。活动任务仍参与 `PendingScans` 重派，终态历史只参与 `/api/tasks` 展示。

该设计给内存列表设置固定上限，同时保留全部活动任务，不把每次 HTTP 查询改成无界数据库扫描。

### 6.2 DH-P2-01：运行状态使用实时 Agent 快照

`RuntimeHost.ServeHTTP` 为单次请求固定当前 API 快照。处理 `/api/runtime/status` 时：

- API 已安装且包含 Pool：使用 `Pool.Status()` 作为 `status.Agents`。
- API 未安装、数据库连接中或数据库不可用：使用构造时的离线/等待快照。
- DatabaseState、DatabaseErrorCode、Restarting 和 RecoveryURL 仍由 RuntimeHost 自身状态提供。

这样 `/api/runtime/status` 与 `/api/agents` 在同一运行实例中共享 Agent 状态来源，同时保留降级启动期间无需数据库的可用性。

## 7. 错误处理与可观察性

- 单个媒体损坏、native open/analyze 失败或 C 内存分配失败必须返回文件级错误，不能让 Worker 进程退出。
- Roots 校验失败必须在分析持久化和 Worker 调用前返回明确错误。
- 删除开始事务失败不得触发 Helper；删除完成事务失败必须保留可查询 journal。
- Manager 恢复一条结构非法的扫描任务时继续保持失败关闭，并给出包含 task ID 的错误；不静默伪造任务。
- 双机复测需分别记录任务 ID、Worker/Agent/VideoCore SHA-256、成功/失败/跳过计数、Worker 重生次数及数据库特征计数。

## 8. 测试与验收

### 8.1 自动化回归

1. `internal/wproc`：Session 阶段一图片调度、非空 contact-sheet 临时路径、Worker 存活。
2. `internal/localanalysis` 与 `cmd/agent`：Roots 复制、非法 Roots、大小写、目录边界、跨盘路径和 scan-then-analysis 传递。
3. `internal/localdelete` 与 `internal/store`：journal 先于 Helper、开始事务失败、完成事务失败、成功/失败/uncertain 混合结果以及未收口候选隔离。
4. `internal/gui`：活动任务与最近终态恢复、统计恢复、非扫描任务隔离、200 条上限和实时 runtime status。
5. 运行相关 Go 包普通测试和 race 测试，再执行仓库既有可运行的完整回归。

### 8.2 构建与本机黑盒

- 使用正式构建脚本生成 Worker、Agent 和 Manager 运行件。
- 对隔离图片和 MP4 执行完整 IPC 流程，确认 SHA、image_features、video_features 和 contact sheet 均有有效输出。
- 对隔离删除副本注入开始事务失败、Helper 后完成事务失败和正常成功，核对文件系统、SQLite journal、files 状态和 outbox。

### 8.3 双主机真实媒体复测

- 本机扫描 `I:\MiddleDir\11111111`，远程扫描 `D:\tmp\-------2-4`。
- 两端部署相同构建哈希；保持 SSH 和现有安全传输方案。
- 扫描运行到图片、视频均产生非零成功样本后可以取消，不要求完整处理数十万文件。
- 验收期间不得出现 `mediacore: cgo Windows binding unavailable`、cgo pointer panic 或系统性 Worker 重生。
- PostgreSQL 中真实媒体必须出现有效 SHA，以及非零 `image_features` 和 `video_features`。
- 本地分析只能读取任务 Roots；预览需返回有效图片内容。
- 删除仅在隔离副本完成，不对两个真实根执行。

## 9. 完成定义

六项缺陷各自的回归测试通过，正式构建通过，双主机动态验收达到对应门槛，并把最新状态、证据、残余边界和优先级写回 `docs/details/2026-08-14-bug-investigation.md`。任何正式测试未运行或受环境阻断的项目必须标记为 `PARTIAL` 或 `BLOCKED`，不能以静态检查替代动态通过。
