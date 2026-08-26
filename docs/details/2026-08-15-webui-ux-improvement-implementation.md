# WebUI UX 审计改进实施记录（2026-08-15）

对应审计：`docs/details/2026-08-14-webui-ux-audit.md`（第九节改进方案，P0–P3 全量实施，三个批次）。
本文档按任务记录实施结果、落点与验证状态。

## 总览

| 批次 | 范围 | 状态 |
|------|------|------|
| 批次一 | 纯前端修复（P1-1/1-2前端/1-3/1-5/P2-1/2-2/2-4/2-5 + P3 全档） | ✅ 完成 |
| 批次二 | 端点补充（P0-1 预览、P0-4 任务持久化、P1-2 后端幂等、P2-3 聚合、P2-6 RecycledTo） | ✅ 完成 |
| 批次三 | 协议/语义变更（P0-2 代表指定、P0-3 策略选择、P1-4 取消、P1-6 短期重试、phase2 入口） | ✅ 完成 |

## 批次一：纯前端（2026-08-15 完成）

按文件簇并行实施（簇A groups / 簇B scans+analysis / 簇C overview+agents+shell+settings+app / 簇D deletion），验证：292→270 区间全量 `npm test` 全绿、`tsc --noEmit` 干净、`eslint` 干净、`npm run build` 通过。

- **P2-1 错误码映射层**：新增 `webui/src/api/errorText.ts`（`apiErrorText`/`databaseErrorText`，覆盖 postgres 四码、server_shutting_down、delete selection conflict、delete task not found、Invalid URL 等）+ 测试。
- **P1-3 缓存一致性**：`usePagedGroups` 新增 `invalidateAll()`（清全部 LRU 页+重取当前页），`finishDelete`/`reconcileDeleteResult` 所有终态分支统一调用；固化旧行为的 `hooks.test.tsx` 用例已更新。
- **P1-1 移动端对齐**：`GroupDetail` 新增 `selectable` prop；移动端复选框/全选/选择栏整体不渲染，"能选不能删"消除。
- **P2-5 可读化**：`textScore` 结构化解析（exact→"内容完全一致"；image/video→"与代表文件的差异：距离 N（极小/小/中）"，隐藏内部字段）；`byteText` 提取至 `features/groups/format.ts` 并补 TB；选择摘要"已选 N 项，共 X GB，涉及 Y 台设备"；移除"行高估算"调试文案；分页器加载中保持上次页码。
- **P2-4 扫描信息补全**：`ScanTask.recent` 结构化 `FeatureItem[]`（注意后端为 PascalCase JSON 字段）；任务表补 scanErrors/roots/ackReason/updatedAt 列，状态中文化，speed/elapsedMs 格式化，停滞任务高亮，recent 错误明细展开，手动刷新+状态筛选+终态折叠，表单消息互斥与重复提交拦截。
- **RemotePathBrowser**：UNC 面包屑、parentPath 返回上一级、打开清旧目录、出错重试、复用 Modal 获得 Esc/焦点圈定。
- **P2-2 状态栏真实化**：`AppShell` 接 `getRuntimeStatus` 轮询，显示"数据库：正常 · 在线节点 X/Y"，异常红色链接跳概览。
- **P2-3 概览仪表盘（前端）**：卡片可点击下钻；断开保留数据加"断开前最后数据"标注；分析卡取消永久停轮询；补失败任务/scanErrors/待识别/身份冲突统计；在线口径两页对齐（online && claimed）；手动刷新；重启横幅。
- **P1-5 流程闭环**：扫描 done→"下一步：运行一筛分析"链接；分析完成→"检出 N 个重复组，前往查看"；概览顶部流程状态条。
- **P3 杂项**：分页器首页/末页/跳页；CopyButton 组件（成员路径/弹窗/任务 ID/错误详情）；minFiles 300ms 防抖；切组/离线清选择通知；AgentsPage 刷新独立态+冲突解释；AnalysisPage 单位人性化/409 清除/指标导出；GUISettingsPage 数字校验（空值非法不静默变 0、min/max、错误聚焦、dirty 防丢、参数 note）；DeleteDialog 轮询退避容错、errorCodes 中文共享（`features/deletion/errorCodes.ts`）、409 冲突中文文案、终态审计链接；DeleteStatusPanel 受控模式可查其他任务；App 404 页。

## 批次二：端点补充（2026-08-15 完成）

- **P0-4 删除任务列表与持久化**：`deploy/central.sql` 新增 `delete_tasks` 表；`DeleteService` 落库（创建/HandleReport/终态 upsert，5s 超时失败降级纯内存）+ 启动 `Restore`；`GET /api/delete/tasks` 死路由激活为任务列表（进行中在前，摘要仅计数不脱敏外泄）；前端审计页默认任务列表、点击载入详情；批次一的 sessionStorage 过渡已移除。
- **P1-2 后端幂等**：`executeDelete` 对已消费 token 返回首次受理 taskId（200）而非 409；tombstone 记录 token→taskId。
- **P2-6 RecycledTo 透传**：`by_machine.*.recycled_to`（{源路径:去向} map，omitempty）透出软删去向；前端 `RecycledToList` 组件展示"已移入回收目录"。
- **P0-1 文件预览**：agent 侧 manager 通道白名单放行 `local.preview.image`/`local.review.save`（其余 op 仍 unauthorized）；`LocalImagePreviewRequest` 追加 `Sha512` 桥（Postgres file_id → sha512 → agent 本地解析，规避双 ID 空间）；GUI 新增 `PreviewBroker` + `GET /api/files/{fileId}/preview`（15s 超时、503 agent_offline、错误码映射、Cache-Control 300s）；前端 GroupDetail 真实缩略图（懒加载/onError 回退占位/video 组"暂不支持"芯片）+ 点击对比弹层（成员 vs 代表，差异高亮，仿 dupeGuru Delta Values）。
- **P2-3 聚合统计**：`GET /api/groups/stats`（复用 list 筛选 CTE，groups/total_bytes/wasted_bytes）；概览"可回收空间 X（共 N 组）"卡片；GroupsPage 筛选统计行；删除完成/手动刷新时统计同步刷新（`refreshGroupsAndStats`）。
- 验证：`go test ./internal/gui ./internal/agent ./internal/proto ./internal/localpreview ./internal/store ./cmd/gui` 全绿；前端 292 用例全绿。

## 批次三：协议/语义变更

### 后端+契约（2026-08-15 完成）

- **P0-2 指定保留副本**：`POST /api/groups/{id}/representative`（body `{"file_id"}`；组/文件 404、非本组活成员 400、并发移除兜底）；更新 `dup_groups.representative_file_id`；审计建议的"复用 local.review.save"经探明不适用（agent 本地 nodetray 审查流与 manager 代表指派是两个体系），未复用。
- **P0-3 策略批量选择**：`POST /api/groups/select-by-strategy`（strategy=newest/oldest/largest/shortest_path；并列取 file_id 小者；策略保留者与 effective 代表双保护；limit 默认/上限 50000，超出 truncated=true）。
- **P1-4 取消链路**：协议追加 `MsgScanTaskCancel=17` + `ScanTaskCancel` DTO；agent `ScanManager.Cancel`（per-task ctx，取消不计 scan error，终态 `TaskDone{Reason:"cancelled"}`，幂等）；`POST /api/tasks/{id}/cancel`（404/409/503/502，内存中间态 cancelling 不动 scan_tasks CHECK 约束，Dispatch 兼容 Reason 自描述）；分析侧 `AnalysisRunner.Run(ctx)` + `POST /api/analysis/firstscreen/cancel`（per-run ctx 叠加 shutdown 兜底）。
- 契约新增：`setGroupRepresentative`、`selectGroupsByStrategy`、`cancelTask`、`cancelAnalysis`；`ScanTask.status` 可能出现 `cancelling`，已取消任务 `status=failed + ackReason="cancelled"`；分析取消后 `lastErr="已取消"`。
- 验证：go 五包全绿；前端 296 用例全绿 + tsc/eslint 干净。

### 前端接线（2026-08-15 完成）

- **3.1 设为保留**：GroupDetail 成员行"设为保留"按钮（代表/离线成员不显示，confirm 确认，成功后 detailReload 重取详情；已勾选成员自动移出选择）。
- **3.2 策略批量选择**：新增 `features/groups/strategy.ts`（策略文案 + `pickStrategySelection` 纯函数）；GroupDetail"自动选择"下拉（组内，已加载成员范围，>100 提示仅覆盖当前页）；GroupsPage"批量选择"对话框（带当前筛选调 select-by-strategy，多组 scope `kind:multi`，truncated 提示上限）；GroupTable 行内"选中其余"快捷按钮（仅桌面）。
- **3.3 取消按钮**：ScansPage 任务行"停止"（乐观 cancelling 中间态，404/409/503 中文映射）；AnalysisPage"取消分析"（禁用至空闲，409 提示）；`cancelling`→"正在停止"、`failed+cancelled`→"已取消"中文化。
- **3.4 审计页一键重试**：DeleteStatusPanel 对 uncertain/E_HELPER_LOST 项显示"重试这些项"，经 App 级 `retryFileIds` 导航回 /groups 恢复选择并自动打开 prepare；快照缺失时禁用并提示（短期方案固有边界）。
- **3.5 phase 2 入口**：创建表单"扫描阶段"单选（一筛/二筛），去掉硬编码 phase:1；任务表新增"阶段"列；只读展示 phase2.autoDispatch 配置状态。**注意：agent 侧 ScanManager.Prepare 仍拒 phase≠1，二筛提交会被拒为 failed——契约/UI 已就绪，待 agent 支持后生效。**
- **DeleteDialog 确认页**新增"本次保留的文件"区（口径：当前已加载成员页的未选中项与代表，详见代码注释）。

## 总验证（2026-08-15）

- 前端：`npm test` 19 文件 **330 用例全绿**；`eslint` 0 问题；`tsc --noEmit` 通过；`npm run build` 通过（产物入 internal/gui/web embed）。
- 后端：`go test -count=1 ./internal/... ./cmd/...` 全绿，**除以下与本次改动无关的既有环境问题**（涉及文件均未在本次或工作区改动之列）：
  - `internal/shared/finalpath`：8.3 短路径名解析差异（环境文件系统配置）。
  - `internal/wproc`、`internal/wproc/videocore`：`exit status 0xc0000135`（测试环境缺 DLL）。
  - `integration`：`TestBuildScriptPackagesHelperWithoutOverwritingOperatorConfig` 需 videocore stage 前置产物（VIDEOCORE_STAGE_REQUIRED）；`TestVideoCoreBuildStaticContract` 对 build.ps1 的既有断言。scripts/build.ps1 自 a925d5f1 起未变。
- OverviewPage 一条用例（pipeline status strip + manual refresh）在全量并行运行时偶发失败，单跑稳定通过，标记为 flaky 待加固。

## 实施方式备注

- Go 1.23.12 便携工具链位于 `.tmp/go-toolchain/go`（系统 PATH 无 Go；`.tmp/go1.23.12.zip` 为安装包，可删）。
- 全部工作未做任何 git 提交/推送；工作区既有未提交改动保持原样。

## 编译打包（2026-08-15）

- 构建：`scripts/build.ps1 -StageDir artifacts/stage-webui-ux-20260815-r3`（Go 用 `.tmp` 便携工具链且注入进程 PATH——wails 以子进程调 `go`；MinGW 用 PATH 上的 WinLibs；cmake/vcpkg 用 C:\vcpkg 标准缓存）。产物：agent.exe、gui.exe（内嵌新 webui）、helper.exe、worker.exe、videocore.dll、nodetray.exe + FFmpeg DLL 闭包 + WebView2 Bootstrapper + release manifest。VideoCore 18 项 CTest 全过。
- **途中修复的仓库既有问题**：`videocore/tests/test_image_object_provenance.ps1` 硬编码 build 本地 `vcpkg_installed` 布局，与 2026-08-11 标准依赖缓存布局（共享 `C:\vcpkg\installed`）冲突；已改为从 `CMakeCache.txt` 读 `VCPKG_INSTALLED_DIR`/`VCPKG_TARGET_TRIPLET`，缓存缺失回退旧布局（编译外部包含与链接库两处断言同步）。mutation 用例级联自愈。
- 发布包（`artifacts/releases/`，ReleaseId 标 `-dirty` 因工作区含未提交改动，SourceRevision=68458a37）：
  - `MySingerServer-compute-win-x64-20260815-main-68458a37-dirty.zip`（25 文件）sha256 `feefbacef67963b0a5a99a14728c45cebd6690d4278ff02506009536abad6a78`
  - `MySingerServer-manager-win-x64-20260815-main-68458a37-dirty.zip`（5 文件）sha256 `203d49d09c856b7e9ff844f1a97d53ee07612596b8ab1bc882e85e64980179d9`
  - 两个打包脚本均输出 `PACKAGE PASS`（含解压回验与清单哈希校验）。

## 热修复：nodetray 本地任务页不显示任务与进度（2026-08-16）

- **现象**：nodetray 本地任务页创建扫描任务后只显示"任务已提交"，下方无任务条目与进度。
- **根因**：`nodetray/frontend/src/pages/LocalTasksPage.tsx` 仅在挂载时加载一次任务列表，`submit()` 创建成功后从不刷新；且仅渲染 taskId+status，后端（`traymodel.LocalTask`）早已返回的进度字段未进前端类型。
- **修复**：
  - 创建成功后立即刷新列表；存在非终态任务（pending/running/waiting_recovery）时每 2 秒轮询，全部终态自动停止。
  - 创建失败展示 `errorSummary`（原来忽略结果恒显示已提交）。
  - `LocalTask` 前端类型补 `progressComplete/progressTotal/mode/roots/errorCode/errorSummary`；列表渲染状态中文、阶段、进度条与完成/总量、速度/失败数/耗时/同步状态、错误摘要；加空态"暂无本地任务"。
- **验证**：`nodetray/frontend` 130 用例全绿（新增 3 例：创建后刷新展示进度、失败展示错误不刷新、非终态轮询到终态停止）、lint 0 错误、`npm run build` 通过。
- **重打包**：重编 nodetray.exe（wails v2.12.0 PASS），r3 stage 复制为 `artifacts/stage-webui-ux-20260816-r4` 并替换 nodetray.exe（r3 保持原样），重新打 compute 包：
  - `MySingerServer-compute-win-x64-20260816-main-68458a37-dirty.zip` sha256 `aba3ea9ec57cd13a8f637eba71177c3624767d8603006485fdd39d09af86d165`（25 文件，PACKAGE PASS）。manager 包不受此修复影响，沿用 20260815 版。

## 主分区合规复验（2026-08-18）

对主工作区按本文档逐项复验：

- **落点抽查**：`delete_tasks` 表（deploy/central.sql:153）、`MsgScanTaskCancel=17`（internal/proto/message.go:28）、新路由 representative / select-by-strategy / tasks cancel / groups/stats / files preview / delete/tasks（internal/gui/httpapi.go:114-122）、`webui/src/api/errorText.ts` 均在。
- **webui 前端**：`npm test` 19 文件 **330 用例全绿**（与记录一致）；`tsc --noEmit` 通过；`eslint` 0 问题；`npm run build` 通过（embed 产物 hash 文件名刷新属预期行为）。
- **nodetray 前端**：`npm ci` 补装依赖后 25 文件 **130 用例全绿**（与记录一致）；lint 0 错误（3 个 react-refresh 风格警告）；build 通过；LocalTasksPage 热修复（创建后刷新、非终态 2 秒轮询、progressComplete 进度条）确认在码。
- **发布包哈希复验**：manager-20260815 与 compute-20260816 的 sha256 与记录一致；compute-20260815 zip 已不在 releases/（被 20260816 热修复版取代，符合热修复记录逻辑）。
- **Go 后端**：52 包中 46 PASS；`finalpath`（8.3 短路径）、`wproc`/`wproc/videocore`（0xc0000135 缺 DLL）三处与记录在案的既有环境失败一致。
- **偏差 1（既有仓库问题，非本批次回归）**：`cmd/helper` 两个 build.ps1 测试（`TestBuildScriptPackagesHelperWithoutOverwritingOperatorConfig`、`TestBuildScriptFailsClosedWhenExactResourceCleanupFails`）因 `VIDEOCORE_STAGE_REQUIRED` 失败。定性：测试自初始导入（65a30c30）起存在，调用 build.ps1 时从不传 `-StageDir`；build.ps1 自 a925d5f1 起强制 `-StageDir`（build.ps1:94-95），两者均未随对方更新，故在当前环境为确定性失败，与记录在 integration 名下的同名测试同属一类。前置缺失时辅助函数为 `t.Fatal` 而非 skip。cmd/helper 与 scripts/build.ps1 均不在本批次改动之列。**（2026-08-18 已随下文"既有测试债务修复"一并修复。）**
- **偏差 2（已修复，2026-08-18）**：`internal/agent/scan.go` 于 2026-08-17 08:32 被清空为 0 字节（全仓唯一受损文件；本批次改动未提交，HEAD 版无 `Cancel`），曾导致 `internal/agent`、`internal/agent/delete`、`cmd/agent` 三包无法编译。**重建方案（用户选定）**：以 HEAD 版 1110 行为基底，按 `scan_test.go` 的 4 个 `TestScanManagerCancel*` 用例与 `server.go` 的 `ScanCancelHandler` 接口重新实现取消增量——`ScanState` 挂 per-task `ctx/cancel`（Prepare 时创建）；新增 `Cancel(taskID) (bool, *proto.TaskStats)`（未知任务 (false,nil)；运行中 cancel ctx 返回 (true,nil) 且幂等；已完成 (false,stats)）；`run` 入口/根目录间/枚举回调逐记录查 ctx（取消不计 scan error、在途记录不计数）；`processDisk` worker 取消后停止处理但继续排空 jobs 防死锁；`finish` 从 ctx 推导终态 `Reason="cancelled"`。**验证**：4 个取消用例全过；`internal/agent`、`internal/agent/delete`、`cmd/agent` 全绿且 `-race` 干净；全量 `go test -count=1 ./internal/... ./cmd/...` 除记录在案的三处环境失败与偏差 1 外无新失败；gofmt/go vet 干净。清空来源不明（疑似并行会话，仓库存在 codex worktree 活动痕迹），已建议用户留意。

## 既有测试债务修复（2026-08-18）

复验后应用户要求清理三处与 UX 批次无关的既有失败：

- **artifacts 目录双包冲突**：`artifacts/live_agent_status_test.go`（package main）与 `artifacts/live_workerpool_integration_test.go`（package workerpool_test）同目录共存导致 `go build ./...` 报 "found packages main and workerpool_test"；两文件均引用已不存在的 `internal/modules/...` 旧布局，属失效诊断脚本。已移至 `artifacts/_legacy/`（`_` 前缀目录 Go 工具链不扫描），内容保留未删。`go build ./...` 复验干净。
- **integration 三项失败**：
  - `TestVideoCoreOnlyRejectsExistingStageBeforeToolResolution`：`build.ps1` 的 stage 校验（VIDEOCORE_STAGE_REQUIRED/EXISTS 等）原先排在 `Resolve-StandardDependencyPaths`（vcpkg 路径解析）之后，测试以缺失 vcpkg + 已存在 stage 调用时期望稳定的 `VIDEOCORE_STAGE_EXISTS`，实际先抛 `VCPKG_STANDARD_PATH_MISSING`。已将 stage 校验块前移至工具链解析之前（dot-source 守卫之后、语义不变）。
  - `TestVideoCoreBuildStaticContract` 两个 mutation（"helper GUI subsystem linker flag moved to unused variable"、"helper build command moved into block comment"）锚点是 helper 构建命令的两行原文，因 build.ps1 缩进/换行变迁失配。修复：测试读取源码后先归一化 `\r\n`→`\n`（兼容 core.autocrlf 双形态检出），锚点更新为当前源（反引号续行 + 12 空格）。
  - 验证：两项测试及全部 mutation 子用例通过。
- **cmd/helper 两项失败**（`TestBuildScriptPackagesHelperWithoutOverwritingOperatorConfig`、`TestBuildScriptFailsClosedWhenExactResourceCleanupFails`）：两测试自初始导入起以 `-OutDir` 调 build.ps1 且不传 `-StageDir`，与 a925d5f1 起的 stage 强制要求脱节，命中 `VIDEOCORE_STAGE_REQUIRED` 后级联失败（后者表现为 rsrc_windows_amd64.syso 未生成）。修复：改用 `-StageDir` 全新目录（stage 语义下 OutDir 不再生效）；前者重构为"已占用 stage（含 operator helper.json）→ 期望 VIDEOCORE_STAGE_EXISTS 拒绝且配置不动 + 全新 stage → 完整构建出 helper.exe 且不生成 helper.json、不残留 .syso"，保持测试原名与意图。
- **总验证（2026-08-18）**：`go test -count=1 ./internal/... ./cmd/... ./integration/` 全绿，仅余两个记录在案的环境性失败——`internal/shared/finalpath`（8.3 短路径）与 `internal/wproc`、`internal/wproc/videocore`（0xc0000135 缺 DLL）。cmd/helper 两项各实跑一次完整构建通过（213s/146s），integration 全包通过，`go build ./...` 干净（artifacts 双包冲突消除）。

## 死代码清理（2026-08-18）

方法：`deadcode -test ./...` + staticcheck U1000 双工具扫描，候选逐项全仓 grep 核实（排除 wails 绑定反射面、接口实现、build tag 变体等误报）后分级删除；前端用 ts-prune + 引用图核实（两边 eslint 的 no-unused-vars 已覆盖局部死变量，不重复）。

- **Go 删除 23 处**（均未导出或全仓零调用）：
  - 函数/方法：`cmd/agent` `runWithDeleteLogger`；`config` `FirstScreenConfig.validate`；`gui` `handleConfigGet/handleConfigPut`（路由走 newConfigHTTP，从未注册）；`nodetray/app` `ensureDefaultHelperConfig`+`reconcileHelperTaskPolicy`+`applyHelperTaskPolicy`+`taskOperation` 死链；`nodetray/config` `platformValidateProtectedHelper`+`validateProtectedHelperDACL`（含 stub；`validateProtectedHelperSecurityDescriptor` 有测试在用，保留）；`store` `committedFrameMask`+`analysisPhase1Done`；`worker` `validFrameLength`、`attemptedPhase2Fields`+`phase1StoreResult`+`phase2StoreResult`、`validatePhase1/2WorkerResult`+`validatePhase1FieldErrors`+`validatePhase2ImageResult`；`wproc` `mediacoreVersion`。
  - 导出但零调用：`agent` `NewScanManagerWithPool`（生产用 WithPoolRouter）；`elevation` `ServeOnce`（含 stub；生产用 ServeOnceWithHandlerFactory）。
  - 只写不读字段：`nodetray/app` `Service.taskDefinition`（Dependencies.TaskDefinition 仍被 composition 校验与注入使用，保留）。
  - 测试文件：`localtask` `blockedSchedulerPool.jobIDs`、`recordingTaskRunner.once` 字段。
  - 连带清理：service.go 的 msgpack import、store_windows.go 的 filepath import。
- **前端删除 2 处**：`nodetray/frontend/src/bindings/backend.ts` 整文件（一行 re-export，零引用，目录一并移除）；`api/localAgent.ts` 的 `startLocalAnalysis`（零引用）。
- **明确保留**：`NewTrustedTerminator`（计划文档声明保留供历史/测试）、`deleteHandlerCompileCheck.Handle`（编译期接口断言）、`ConfirmDialog.tsx`（有完整测试的基础组件资产）、webui 两个 legacy.html（路由与文档在用）、全部 wailsjs 生成绑定、各 "used in module" 导出（仅 export 关键字多余，删之无收益）。
- **验证**：`go build ./...` 干净；受影响包 `go test` 全绿；全量 `go test ./internal/...` 仅余记录在案的两个环境性失败（finalpath 8.3、wproc 缺 DLL）；nodetray 前端 130 用例全绿 + tsc + lint 通过。
- 工具原始输出存档：`.tmp/deadcode-full.txt`、`.tmp/staticcheck-u1000.txt`、ts-prune 两份（/tmp，会话临时）。

## 已知边界与遗留

- 删除任务恢复快照为背书式（member 明细未入 status_json），迟到报告丢弃、deadline 后 pending 翻转 uncertain；真正的中断重派（设计 §12.1 稳定操作 ID）未实施，3.4 仅落地短期"一键重试"入口。
- 视频预览未做：contact-sheet 代理会暴露任意路径读，且 agent 无视频 preview 管线；前端 video 组显示占位。审计"PhasePreview 取关键帧"的描述与实际不符。
- cancelling 期间 agent 重启丢任务时 GUI 任务停在 cancelling（无重派），代码已注释该边界。
- 批次二中 DeleteStatusPanel 任务列表点进详情后无"返回列表"按钮（清空查询可回）。
- `go build ./...` 会扫到 artifacts/ 下既有遗留测试包报错（既有问题，与本次无关）。
- 多个既有 Go 文件为 CRLF/混合换行，`gofmt -l` 报警为仓库既有状态，未做整文件格式化。
