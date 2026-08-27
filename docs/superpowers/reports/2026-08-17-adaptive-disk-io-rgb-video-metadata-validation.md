# 自适应磁盘 I/O、RGB 视频缩略图与完整视频元数据验收报告

日期：2026-08-17

工作树：`D:\code\mySingerServer\.worktrees\local-task-lifecycle-controls`

验收基线：`f833d69f8ec9f4919fa06a69458d6059e6e29a92`

## 总结

本轮结论为 **PARTIAL / BLOCKED**，不得声明完整性能或发布 PASS。

- benchmark 与发布 manifest 契约完成 RED→GREEN；脚本在任一运行失败、结果不一致或性能回退超过 3% 时均不会输出性能 PASS。
- 指定 race 四包、前端 test/lint/build、fresh VideoCore native 构建和两个 I 盘真实视频只读抽检通过。
- 正式 Baseline/Adaptive 全量扫描因 D 盘可用空间仅 `993947648` bytes，低于固定前置要求 `5368709120` bytes，均在访问源媒体前标记 `BLOCKED`。没有降低阈值，没有删除数据或旧 cache。
- full build 首次在 Agent 的 cgo 链接边界失败。白名单内完成 Agent 专属 cgo 最小修复后，Agent 构建通过，但 full build 随后被既有 NodeTray 配置夹具 `config: IO policy value out of bounds` 阻断。按授权边界未继续修第二个跨范围门禁，因此没有生成 fresh 全产品 stage 和正式 Compute/Manager ZIP。
- 未部署、未替换现场目录、未启动 GUI，未复制或重建 `local-control.token`，未修改 ACL。

## 1. benchmark 与构建脚本契约

状态：**PASS**

RED 证据：

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File '.\scripts\benchmark-scan-io.test.ps1'
```

- 首个 RED：`BENCHMARK_SCAN_IO_SCRIPT_MISSING`，exit code `1`。
- manifest RED：`compute manifest lacks Agent/Worker IPC ABI 2`，exit code `1`。
- Agent 构建 RED：`INVOKE_AGENT_BUILD_MISSING`，exit code `1`。

GREEN 证据：同一命令 fresh exit code `0`，输出 `BENCHMARK SCAN IO CONTRACT TEST PASS`。覆盖：

- 生产 root 固定为 `I:\MiddleDir\11111111`；Baseline/Adaptive 使用相同 `fields_mask=2043` 与 Worker 数。
- 记录 Git build SHA、完整配置、起止 UTC、墙钟、每盘轨迹、资源采样、生命周期和结果集合摘要。
- D 盘空间不足时先写 `BLOCKED`，不调用扫描 runner。
- 文件集合、SHA-256、图片特征、六帧特征和失败集合以规范化 digest 比较。
- 任一 runner/结果/生命周期失败不得产生性能 PASS；自适应回退 `>3%` 为 FAIL，未回退但缩短 `<20%` 为 `TARGET_NOT_MET`。
- Compute manifest 记录 Agent/Worker IPC version `2`、VideoCore ABI `2`、media metadata schema `5`；Manager manifest 记录 media metadata schema `5`。
- Agent 构建只在自身命令期间设置 `CGO_ENABLED=1` 和已解析的 MinGW `CC`，随后恢复环境；GUI/Helper 继续使用既有 CGO0，Worker 继续使用既有 CGO1。

## 2. Go 全量与 race

### 全量 Go

状态：**FAIL / PARTIAL**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./...
```

exit code `1`。首轮环境和既有门禁失败包括：缺少 `bin\tools\ffmpeg.exe/ffprobe.exe`、Windows PowerShell 5 ExecutionPolicy、沙箱短路径临时目录 ACL、旧 VideoCore stage DLL，以及已经漂移的 build/export 静态夹具。`internal/agent` 另有一次等待 I/O snapshot 超时。未把部分包通过冒充全量 PASS，未跨 Task 12 白名单修这些问题。

### 指定 race

状态：**PASS**

```powershell
$env:PATH='C:\tmp\mysinger-task12-videocore-stage;' + $env:PATH
$env:VC_TESTDATA_ROOT=(Resolve-Path '.\testdata\videocore\compat\videos').Path
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 `
  ./internal/diskio ./internal/agent ./internal/worker ./internal/wproc
```

fresh exit code `0`：四个包全部 `ok`。首次使用旧 stage 时 `internal/wproc` 以 `0xc0000139` 失败；换用本轮 fresh DLL 并补齐固定夹具 root 后复跑通过。

## 3. 前端

状态：**PASS（lint 有既有 warning）**

```powershell
npm --prefix nodetray/frontend test -- --run
npm --prefix nodetray/frontend run lint
npm --prefix nodetray/frontend run build
```

- Vitest：27 个 test files、182 个 tests 全部通过，exit code `0`。
- ESLint：exit code `0`，0 errors、3 个 `react-refresh/only-export-components` warnings。
- TypeScript/Vite build：exit code `0`。
- build 生成的 `nodetray/frontend/dist` 已精确恢复到本轮前状态，没有提交生成产物。

full build 内的 `webui` 也完成 17 个 test files、209 个 tests，lint/build exit code `0`；其生成的 embedded web 文件同样已精确恢复。

## 4. Native / VideoCore

状态：**PASS**

```powershell
& '.\scripts\build.ps1' -Go 'C:\tmp\go1.26.5\go\bin\go.exe' `
  -VideoCoreOnly -StageDir 'C:\tmp\mysinger-task12-videocore-stage'
```

exit code `0`：

- CTest 当前实际为 20/20（brief 中的 18/18 已被后续测试扩展）。
- exact exports 14/14。
- recursive native dependency closure 6 files PASS。
- fresh stage：`C:\tmp\mysinger-task12-videocore-stage`。

## 5. 真实媒体只读抽检

状态：**PARTIAL**

对 `I:\MiddleDir\11111111` 沙箱外只读统计得到 47150 个文件，其中 JPG 39169、MP4 4589、MKV 1。ffprobe 只读选择了：

- H.264：stream 0=`h264`，stream 1=`aac`，双轨容器。
- HEVC：stream 0=`hevc`，stream 1=`aac`，双轨容器。

使用 fresh VideoCore DLL 分别运行：

```powershell
$env:VC_REAL_MEDIA_SAMPLE='<只读样本>'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 `
  ./internal/wproc/videocore `
  -run '^TestRealMediaContactSheetCompletesWithinProductionFrameDeadline$' -v
```

两个样本均 exit code `0`，联系表六帧完成。源文件 SHA-256 前后保持不变：

- H.264：`df8c0dc819e6f96bc6fd3f528906a407600e28013f188784f1cc5565cbe2e00a`。
- HEVC：`f0a9eb32e68b47f6beda3a9bfe3905e8da1907276fb59a75bd8783ca5278d31e`。

native 20/20 覆盖 RGB contact-sheet 编码和既有灰度 golden；但正式任务被 D 空间门禁阻断，因此本轮没有取得以下运行时证据：实际任务 DB 中主视频 decoder/stream 与 FFmpeg 的逐行比对、全部轨道原子落库、正式任务 cache 目录的 `.json/.lock/vc-grid-v1` 闭包。上述项标记 **BLOCKED**，不能由单元测试替代。

## 6. Baseline / Adaptive 全量性能与生命周期

状态：**BLOCKED**

```powershell
& '.\scripts\benchmark-scan-io.ps1' -Root 'I:\MiddleDir\11111111' `
  -Mode Baseline -OutputDir '.\artifacts\benchmarks\io-baseline'
& '.\scripts\benchmark-scan-io.ps1' -Root 'I:\MiddleDir\11111111' `
  -Mode Adaptive -OutputDir '.\artifacts\benchmarks\io-adaptive'
```

两次命令均先写入 `D_DRIVE_SPACE_INSUFFICIENT`：观测 `993947648` bytes，小于要求 `5368709120` bytes。原始证据：

- `artifacts\benchmarks\io-baseline\benchmark-summary.json`
- `artifacts\benchmarks\io-adaptive\benchmark-summary.json`

因此没有执行正式扫描，未取得完整结果集合/SHA/图片特征/六帧特征/失败集合对比，也没有墙钟回退或 20% 目标数据；性能状态为 `BLOCKED`，绝不输出 PASS。pause/resume/stop、lease 等待取消、in-flight 排空、进度不超前及 CPU/磁盘波动也因同一前置门禁 `BLOCKED`。

## 7. fresh 全产品 build/package

状态：**BLOCKED**

full build 第一次在 Agent CGO0 链接时报 `_cgo_*` relocation 未定义。根因证据：

- `go list -deps` 显示 `cmd/agent -> internal/wproc -> internal/wproc/videocore`。
- `CGO_ENABLED=0 go build -tags nodynamic ./cmd/agent` 可重复失败。
- `CGO_ENABLED=1` 时依赖列出 `runtime/cgo` 与 `videocore bindings.go/io_governor.go`，同一 Agent build exit code `0`。

白名单修复后再次运行 full build，Agent 已通过；随后 `scripts/build-nodetray.ps1` 的 Go tests 因既有 `internal/nodetray/config` 夹具报 `config: IO policy value out of bounds` 而 exit code `1`。根据“只修 Agent 必要 cgo”授权，没有继续修改配置或测试。

构建脚本失败时回收了未完成 stage，故以下正式包命令未执行：

- Compute release ZIP：**BLOCKED / 无 artifact / 无 SHA-256**。
- Manager release ZIP：**BLOCKED / 无 artifact / 无 SHA-256**。
- 正式 ZIP 独立展开、ABI2/schema5/manifest/native closure/SHA 验证：**BLOCKED**。

发布脚本的独立最小 fixture contract 已 fresh exit code `0`，证明 manifest 新字段和 ZIP/sidecar 自校验可运行；这不等同于正式 fresh 产品包验收。

## 8. 部署与 GUI 边界

状态：**BLOCKED（按授权未执行）**

- 未移动或替换任何现场目录。
- 未启动 `gui.exe`、NodeTray、Agent 或 Worker 真实进程。
- 未部署本轮任何产物。
- 未复制、读取、重建 `local-control.token`，未改变 token ACL。
- 未修改目录 ACL，未删除源媒体、旧 cache 或 `thumbcache\vc-grid-v1`。

## 最终判定

- 代码/契约：PASS。
- 指定 race：PASS。
- 前端：PASS（3 warnings）。
- Native：PASS（20/20、14/14、closure 6）。
- 真实媒体：PARTIAL；两种视频 codec 和双轨只读分析通过，正式 DB/cache 闭包 BLOCKED。
- 全量 Go：FAIL/PARTIAL。
- 正式 Baseline/Adaptive 性能与生命周期：BLOCKED（D 盘空间不足）。
- fresh 全产品 build/package：BLOCKED（NodeTray 配置夹具失败）。
- 部署/GUI：BLOCKED（无授权且无完整包）。

因此 Task 12 只能以“契约和若干分层门禁完成、完整性能与发布闭包未完成”交付，不具备部署批准条件。
