# Task 10 实现报告：本机结果查询、审核和不落盘图片预览

## 结果

已实现 Agent 独占 SQLite 的本机结果查询、组详情、审核提交，以及只以 `file_id` 为入口的 JPEG/WebP 内存预览。当前结果查询排除 `files.status='deleted'`，指定历史 run 的查询保留已删除成员；分页上限 200，按 `generation DESC, group_id ASC` 稳定排序。

审核要求每组显式至少一个 `keep`，未传成员写为 `undecided`；`delete` 仅允许 exact 组或最终 verdict=`duplicate` 的组。`local_reviews` 与 `local_outbox` 在同一 SQLite 事务中写入，outbox 失败时审核记录整体回滚，只有保存操作会生成 `local.review` outbox。

图片预览 wire 请求只包含 `file_id`、目标尺寸、格式和质量，不含 path，msgpack 对未知字段严格拒绝。Agent 先按 machine/file 读取数据库 canonical path、图片类型、非 deleted 状态、SHA-512、size、mtime，再把 path-bearing job 交给 Worker。实际图片读取、二次不可变身份校验、解码、缩放以及 JPEG/WebP 编码全部位于 Windows Worker 的 `internal/wproc`；Agent 依赖图不包含 `github.com/gen2brain/webp`。预览字节只在内存中返回，不创建源目录或 TEMP 缩略图，编码响应预留 wire 开销并限制总 payload 不超过 4 MiB。

视频查询只返回 Stage 1 已持久化的 contact sheet 路径，本任务不重建视频预览缓存。生产错误和日志不输出 canonical path；Worker 协议错误使用 PathID。

## RED → GREEN 记录

第一轮 RED 分组及对应 GREEN：

- Worker wire：缺少 preview phase/job/result 字段；增加专用 phase、格式/尺寸/质量、结果字节及安全错误码，并完成 msgpack round-trip。
- Supervisor/Pool：preview phase 被拒且结果会进入 `SaveAnalysis`；增加严格 preview 身份/尺寸/格式/4 MiB 校验，preview 旁路分析持久化。
- Worker 编码：缺少内存图片预览实现；增加真实 JPEG/WebP 解码、缩放、限长编码、stale identity 和源目录/TEMP 无副作用测试。
- WProc：preview job 返回 unsupported phase；增加 Worker 进程内 dispatch，真实 IPC 回归通过。
- Proto：缺少本地查询/审核/预览 DTO；增加上限验证、严格未知字段拒绝、`path` 注入拒绝和总 payload 上限。
- Store：缺少当前/历史查询、审核事务与 preview identity 读取；增加真实 SQLite 查询、筛选、分页、事务回滚和 machine-owned source 测试。
- Service/Handler：缺少 localreview/localpreview 服务及 socket 操作；增加 NodeTray loopback-only handler，Manager 与非回环 NodeTray 均拒绝。
- Router/Pool：preview bytes 存在切片别名且会持久化分析；改为深拷贝并验证 `SaveAnalysis` 未被调用。
- cmd/agent：生产组合根未注入 results handler；增加 LocalResultHandler 注入和操作转发测试。

收口阶段新增三组真实 SQLite RED → GREEN：

- 文件名筛选曾误命中目录组件；改为在 SQLite 中递归拆分 canonical path，只对 basename 筛选。
- 组详情曾通过首个 200 行列表页查找，第 201 行以后误报不存在；改为按 machine/current-or-run/group 直接查询。
- `reviewed` 曾表示任一成员已决定；改为当前/历史范围内全部成员均已决定，并保留 `undecided` 的组级语义。

## 验证摘要

- 默认标签完整相关包：`go test -count=1 ./internal/localreview ./internal/localpreview ./internal/store ./internal/proto ./internal/agent ./internal/worker ./internal/wproc ./cmd/worker ./cmd/agent`：PASS。
- 强制 fallback：同一矩阵加 `-tags nodynamic`：PASS；真实 JPEG 与 WebP 编解码、MIME、尺寸、stale identity、无文件副作用和 4 MiB 限制均通过。
- Race：`localreview/localpreview/store/proto/agent/worker/wproc/cmd/agent`：PASS；WProc 使用发布目录 native runtime 闭包和显式 WinLibs `CC`。
- Windows 构建：`go build -tags nodynamic ./cmd/agent ./cmd/worker`：PASS；`go list -tags nodynamic -deps ./cmd/agent` 不含 WebP codec。
- 静态门禁：`git diff --check` PASS；生产 preview 代码无 `Create/CreateTemp/WriteFile/Mkdir/Rename`；相关生产代码无 path 日志。
- Linux/CGO=0：`localreview` 与 `proto` 可编译；包含 `localpreview` 的组合被既有 `internal/worker` Windows-only `supervisorDeps/workerProc` 边界阻断，属于非目标平台 PARTIAL，未扩范围修改。

## 依赖与发布边界

`go` directive 从 1.22 调整为 1.23，仅满足 `github.com/gen2brain/webp v0.6.4` 的依赖要求，不再升级；当前 Go 1.26.5 Windows 工具链兼容。新增依赖为固定 tagged 版本 `github.com/gen2brain/webp v0.6.4`（MIT），间接使用 `github.com/ebitengine/purego v0.10.1`，不新增外部 DLL 或运行时文件。

Task 14 的 Windows release build 必须统一传入 `-tags nodynamic`，确保 ZIP 内的 Worker 始终使用 bundled CGo-free fallback，且不会探测宿主机 `libwebp`。Agent 已通过包边界与依赖图证明不链接或执行编码器。

## 文件与提交

实现涉及：

- `internal/store/local_results.go`、`internal/store/local_results_test.go`
- `internal/proto/local.go`、`internal/proto/local_test.go`
- `internal/localreview/service.go`、`internal/localreview/service_test.go`
- `internal/localpreview/service.go`、`internal/localpreview/service_test.go`
- `internal/wproc/run.go`、`internal/wproc/run_test.go`、`internal/wproc/image_preview.go`、`internal/wproc/image_preview_test.go`
- `internal/worker/messages.go`、`internal/worker/messages_test.go`、`internal/worker/supervisor.go`、`internal/worker/supervisor_test.go`、`internal/worker/pool.go`、`internal/worker/pool_test.go`
- `internal/agent/local_handler.go`、`internal/agent/local_handler_test.go`、`internal/agent/pool_router.go`、`internal/agent/pool_router_test.go`
- `cmd/agent/main.go`、`cmd/agent/main_test.go`、`go.mod`、`go.sum`
- 本报告及 `progress.md` ledger。

提交信息固定为 `feat: review local results with memory previews`，本报告随该提交一并落库。

## Concerns

- Linux Worker/Agent 仍受项目既有 Windows-only Worker supervisor 实现约束；Task 10 的交付目标和发布门禁为 Windows portable。
- Task 14 若漏传 `-tags nodynamic`，默认 WebP 包允许探测宿主动态库；因此 release 脚本必须把该 tag 作为强制条件。
- 未执行 NodeTray GUI 人工操作验收；socket 认证、strict payload、查询/审核/预览生产组合链已由自动化测试覆盖。
