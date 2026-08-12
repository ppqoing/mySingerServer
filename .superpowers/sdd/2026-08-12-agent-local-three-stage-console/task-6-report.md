# Task 6 实现报告：拆分二筛与三筛计算

## Status

PASS。Manager 的 Stage 2/Stage 3 已映射为带 `screen_stage` 与 `source` 身份的独立 Worker 作业；图片、视频结果与缓存均按阶段裁剪，SQLite 按字段独立合并；Stage 0 继续兼容完整结果。未修改 proto 位值、wire tag、native ABI 或数据库 schema。

## Commit

基础提交：`9dccf3edd53673a31d541dc662c002d51044f41e`（`feat: split second and third screen computation`）。修复提交说明：`fix: validate staged media identity and cache replies`，哈希见任务回执。

## RED / GREEN

| 合同组 | RED 证据 | GREEN 结果 |
| --- | --- | --- |
| Agent stage-aware 信封与作业身份 | `ScreenStage`/`Source` 尚不存在，Stage 冲突和 foreign result 测试失败 | `task.Validate()`、Stage envelope equality、manager/scan source、stage/source result 校验通过 |
| 图片/视频 Stage 2、Stage 3 与 Stage 0 | 新视频字段位被旧 mask 拒绝，Stage 2/3 无法分别生成 payload | 图片 Stage 2 仅 pHash、Stage 3 仅 Sobel；视频新位在 Go 边界映射 legacy 6F 后裁剪；Stage 0 完整兼容 |
| Store 合并与缓存 | `0x100/0x200` 超出旧视频 mask，分阶段查询/保存失败 | 六帧 pHash/Sobel 完整性独立，UPSERT 使用逐列 `COALESCE`，正反写入顺序均不互清，缓存仅返回请求阶段 |
| immutable identity | 文件 SHA 改变后仍计算并返回 pHash | pre/post/commit 均核验身份，变化返回稳定 stale 且不提交 payload |
| Manager loopback / PoolRouter | Phase2 fake result 未携带身份时被拒绝；Phase1 旧结果的空 source 造成既有扫描测试超时 | 生产 Phase2 强校验 stage/source；仅 Phase1 保留旧 IPC 空身份兼容，Manager 重连回放通过 |
| computed-flight 固定帧 payload | 分阶段视频 Resolve 后复用结果只有 mask、六帧 payload 为空 | 固定 `[6]FrameResult` 被逐帧克隆到复用结果；新二筛/三筛位纳入 `ValidateVideoCoreMasks` |

## Tests

- 首轮 RED：`go test -count=1 ./internal/agent ./internal/worker ./internal/wproc ./internal/store -run 'StageTwo|StageThree|LegacyCombined|StaleIdentity|VideoSixFrame|Phase2Envelope'`
- Agent：`go test -count=1 -timeout 45s ./internal/agent` — PASS（0.662s）
- Worker：`go test -count=1 -timeout 60s ./internal/worker` — PASS（最终复验 0.349s）
- Worker 定向：`go test -count=1 -timeout 60s ./internal/worker -run 'TestDeduperComputedVideoStageReplyKeepsFixedFramePayload|TestMergedResultMapCompatibilityUsesExplicitFrameStatus'` — PASS（0.011s）
- Pipeline / Store / Worker 入口：`go test -count=1 -timeout 60s ./internal/wproc ./internal/store ./cmd/worker` — PASS（0.379s / 1.226s / no test files）
- Race：`CC=C:\Tools\WinLibs\mingw64\bin\gcc.exe go test -race -count=1 -timeout 120s ./internal/agent ./internal/worker ./internal/store` — PASS（2.575s / 4.571s / 8.294s）
- 最终受影响 race 复验：`CC=C:\Tools\WinLibs\mingw64\bin\gcc.exe go test -race -count=1 -timeout 120s ./internal/worker` — PASS（4.557s）
- `git diff --check` — PASS；仅 Git 的 LF→CRLF 工作区提示，无 whitespace error。

一次合并包命令曾因旧 Phase2 fake result 缺少 stage/source 导致 loopback 测试等待至命令超时；该问题已定位并修复，随后按包拆分运行全部通过，未把命令超时误记为测试断言失败。

## Fix round 1/5

| 修复合同 | RED 证据 | GREEN 结果 |
| --- | --- | --- |
| final identity guard | 完整缓存查询后 stat 漂移、native Analyze 后同 size/mtime 内容替换均返回成功 payload | 所有成功出口重新 stat/sameFile/size/mtime 并二次 Hash；与首次 Hash 或 KnownSHA 不同即清 payload 并返回稳定 stale |
| 日志隐私 | Worker file/crash/store 与 Agent foreign route 日志泄露完整目录和文件名 | 使用规范路径 SHA-256 截断 `path_id`，错误中的已知路径替换为 `<path>`，计算/帧错误带 `screen_stage` 和 `source` |
| cache payload | committed 完整命中被 Agent 提前跳过并产生空 FeatureItem；部分视频仅提交 missing frame mask | 每个请求提交恰好一个原始 stage/field/frame job；Worker 缓存返回完整请求 payload，完整命中不调用 native |
| 六帧全失败 | `FramesDone=0` 时固定帧错误未进入 Store、日志或 FeatureResult | 固定帧统一转换为六个 `native_status_<code>` error-only Frames；prune 只清成功 payload，不清错误帧 |
| TCP 分阶段验收 | 真实 TCP 仅覆盖旧 Stage 0 重连 | 新增 Stage 2 image pHash 与 Stage 3 image Sobel 的 accepted、严格 payload、TaskDone 与单作业验收；旧重连继续通过 |
| 旧 SavePhase2 | 视频 split 位被拒绝，分阶段保存会覆盖另一列 | 旧入口识别 split 位、逐列 `COALESCE`，2→3 与 3→2 均保留两列；legacy mask 行为不变 |

修复轮最终门禁：

- `go test -count=1 -timeout 90s ./internal/wproc ./internal/store ./cmd/worker` — PASS（0.378s / 1.383s / no test files）
- `go test -count=1 -timeout 90s ./internal/worker ./internal/agent` — PASS（0.338s / 0.681s）
- `CC=C:\Tools\WinLibs\mingw64\bin\gcc.exe go test -race -count=1 -timeout 120s ./internal/agent ./internal/worker ./internal/store` — PASS（2.677s / 4.610s / 8.247s）
- `git diff --check` — PASS；只有 LF→CRLF 提示，无 whitespace error。

Race 首轮唯一失败来自新增 Agent 日志测试直接并发读取 `bytes.Buffer`；改用 mutex-safe 捕获器后定向 race 与完整 race 均通过，生产代码未出现 race。

## Concerns

- Task 6 只提供分阶段计算、路由、缓存和持久化合同。Stage 2 通过后是否创建 Stage 3 由 Task 8 的本地 Engine 决定；本实现不会从 Stage 2 隐式追加 Stage 3。
- local/manager/scan 身份已可进入共享 Worker 抽象；来源与阶段的公平调度属于 Task 9，本任务未实现。
- `.codex-temp/` 为既有未跟踪目录，本任务未读取、修改或暂存。
