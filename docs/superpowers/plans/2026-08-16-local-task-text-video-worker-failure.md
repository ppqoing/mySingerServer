# 本地任务完整文字与视频 Worker 失败根因修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让任务卡完整显示全部任务信息，并从具体协议错误位置修复视频任务批量 `exit status 2`，以真实媒体内容和轨道编码完成解码，最终交付可验证的 Compute 与 Manager 包。

**Architecture:** 保持现有 `Agent -> Worker IPC -> VideoCore` 架构。父进程在缓存查询失败时返回掩码完整的“全部缺失”回复；Worker 将可预期的媒体错误按单个字段返回，使严格协议校验能够保留具体阶段和原因。真实编码解码继续由 VideoCore 根据内容签名或 FFmpeg 轨道 `codec_id` 选择实现，新增生产默认 Worker IPC 回归证明这条链路真正被调用。

**Tech Stack:** Go 1.26、MessagePack IPC、SQLite、C++20、FFmpeg/VideoCore、React 19、TypeScript、Vitest/React Testing Library、PowerShell 发布脚本。

## Global Constraints

- 图片必须读取真实文件字节，并依据内容签名选择 JPEG、PNG、WebP 解码器；扩展名不得代替内容识别。
- 视频必须解复用真实容器并依据视频轨道 `codecpar->codec_id` 选择 H.264、HEVC、VP9 等对应解码器。
- 不切回旧视频管线，不跳过 MP4/MOV，不禁用 VideoCore，不弱化 IPC、ABI、字段掩码或结果校验。
- 可预期媒体错误必须成为带具体 `FieldError.Stage` 和安全原因的文件级结果；真正协议损坏仍是硬错误。
- 不新增“查看错误”按钮或错误历史功能；本轮只修复生产任务的 72 个失败及任务卡文字截断。
- 不删除或重建 `artifacts/releases/MySingerServer-Compute/data`，保留用户任务数据库与日志。
- 真实 `I:\MiddleDir\11111111` 验收只有在当前执行身份可访问时才能判定 PASS，否则必须标记 PARTIAL。
- 任何真实编码夹具若仍失败，先记录精确 `stage`、错误和首次违约函数，停止 codec 修改并补写对应 RED/修复步骤；禁止猜测性改动。

## 文件结构与职责

- `internal/worker/supervisor.go`：父进程处理 SHA 查询及 Worker 严格协议边界。
- `internal/worker/pool_test.go`：真实 Supervisor/Worker IPC 生命周期回归。
- `internal/wproc/pipeline_session.go`：生产默认媒体会话管线及文件级错误映射。
- `internal/wproc/pipeline_session_test.go`：会话管线字段错误、图片和视频结果单元测试。
- `internal/wproc/run_test.go`：`serve` 的生产默认 IPC 与真实媒体夹具测试。
- `nodetray/frontend/src/pages/LocalTaskItem.tsx`：任务卡语义和完整文字输出。
- `nodetray/frontend/src/pages/LocalTaskItem.test.tsx`：任务卡内容与控制参数回归。
- `nodetray/frontend/src/app.css`：宽屏两层网格、完整换行和窄屏堆叠布局。
- `artifacts/stage/local-task-video-fix-*`：一次性、可校验的构建 stage，不作为源码提交。
- `artifacts/releases`：Compute/Manager ZIP、SHA-256 sidecar 与经校验的展开目录。

---

### Task 1: 修复缓存查询失败时的不完整 SHA 回复

**Files:**
- Modify: `internal/worker/supervisor.go:566-574`
- Modify: `internal/worker/pool_test.go:1592-1614,2396-2420,2588-2608`

**Interfaces:**
- Consumes: `SHAQueryMsg{RequestedFields uint32, RequestedFrames uint8}` 和现有 `missingReply(SHAQueryMsg) SHAReplyMsg`。
- Produces: 任意 `Deduper.Ask` 错误都会得到 `RequestedFields/Frames` 与 `MissingFields/Frames` 完整相等的可验证回复，Worker 可继续计算。

- [ ] **Step 1: 写出带视频字段掩码的失败回归**

在 `workerScript` 增加只用于确定性观察的通道，并在 fake Worker 收到父进程回复后发送副本：

```go
type workerScript struct {
    // existing fields...
    replyObserved chan<- SHAReplyMsg
}

// fake worker, after DecodeBody[SHAReplyMsg]
if script.replyObserved != nil {
    script.replyObserved <- reply
}
```

新增测试：

```go
func TestPoolVideoLookupFailurePreservesRequestedMasks(t *testing.T) {
    observed := make(chan SHAReplyMsg, 1)
    fields := uint32(MaskVideoDuration | MaskVideoContactSheet)
    query := SHAQueryMsg{
        JobID: 204, SHA512: make([]byte, 64), Kind: MediaVideo,
        RequestedFields: fields,
    }
    h := newLifecycleHarness(t, workerScript{
        ready: true, queryOnJob: true, queryOverride: &query,
        replyObserved: observed,
    })
    h.store.lookupErr = errors.New("sqlite temporarily unavailable")
    p := h.newPool(Config{WorkerCount: 1})
    p.Start()
    t.Cleanup(p.Close)
    h.ready(t)
    if err := p.Submit(&JobMsg{
        JobID: query.JobID, ScanTaskID: "scan-204",
        Path: `D:\media\lookup-fallback.mp4`, Kind: MediaVideo,
        Phase: Phase1, FieldsMask: MaskSHA512 | fields,
    }); err != nil {
        t.Fatal(err)
    }
    reply := <-observed
    if reply.RequestedFields != fields || reply.MissingFields != fields ||
        reply.FieldsPresent != 0 || reply.RequestedFrames != 0 ||
        reply.MissingFrames != 0 {
        t.Fatalf("fallback reply = %#v", reply)
    }
    if err := reply.ValidateMasks(); err != nil {
        t.Fatalf("fallback reply masks: %v", err)
    }
}
```

- [ ] **Step 2: 运行测试确认 RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/worker -run 'TestPool(VideoLookupFailurePreservesRequestedMasks|DeduperLookupFailureFallsBackToWorkerComputation)'
```

Expected: 新测试 FAIL，当前回复的 `requested_fields` 和 `missing_fields` 均为 `0`，证明父进程声称“Worker 将计算”时却发送了不符合原请求的回复。

- [ ] **Step 3: 在具体错误位置生成完整缺失回复**

将 `supervisor.go` 的错误分支改为复用严格掩码构造函数：

```go
reply, askErr := worker.pool.dedup.Ask(worker.pool.ctx, query)
if askErr != nil {
    worker.pool.deps.logger.Error("feature lookup failed; worker will compute",
        "job_id", query.JobID, "err", askErr)
    reply = missingReply(query)
}
```

不要改变 `Deduper.Ask`、SQLite 重试或校验器。

- [ ] **Step 4: 运行 Worker 包测试确认 GREEN**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/worker
```

Expected: PASS；查询失败仍会记录日志，但不会制造不兼容回复或 Worker 退出。

- [ ] **Step 5: 提交 Task 1**

```powershell
git add -- internal/worker/supervisor.go internal/worker/pool_test.go
git commit -m "fix: preserve video SHA fallback masks"
```

---

### Task 2: 将媒体失败映射为可验证的单字段错误

**Files:**
- Modify: `internal/wproc/pipeline_session.go:581-587`
- Modify: `internal/wproc/pipeline_session_test.go`
- Modify: `internal/worker/supervisor_test.go`

**Interfaces:**
- Consumes: `sessionPipelineFileError(result *JobResultMsg, fields uint32, stage string, err error)`。
- Produces: 每个非零请求位对应一个 `FieldError`；无字段的内部说明使用 `Field: 0`；父进程严格校验不变。

- [ ] **Step 1: 写出多字段媒体错误 RED**

在 `pipeline_session_test.go` 增加：

```go
func TestSessionPipelineFileErrorSplitsRequestedFieldBits(t *testing.T) {
    job := &worker.JobMsg{
        JobID: 801, Path: `D:\media\broken.mp4`, Kind: worker.MediaVideo,
        Phase: worker.Phase1,
        FieldsMask: worker.MaskSHA512 | worker.MaskVideoDuration | worker.MaskVideoContactSheet,
    }
    result := newSessionPipelineResult(job)
    sessionPipelineFileError(result,
        worker.MaskVideoDuration|worker.MaskVideoContactSheet,
        "video_probe", errors.New("decoder rejected stream"))
    if len(result.Errors) != 2 ||
        result.Errors[0].Field != worker.MaskVideoDuration ||
        result.Errors[1].Field != worker.MaskVideoContactSheet {
        t.Fatalf("field errors = %#v", result.Errors)
    }
    for _, fieldError := range result.Errors {
        if fieldError.Stage != "video_probe" || fieldError.Msg != "decoder rejected stream" ||
            fieldError.Field&(fieldError.Field-1) != 0 {
            t.Fatalf("invalid field error = %#v", fieldError)
        }
    }
}
```

在 `supervisor_test.go` 增加父进程契约证明：

```go
func TestVideoFileErrorsAcceptOneRequestedBitEach(t *testing.T) {
    job := &JobMsg{JobID: 801, Path: `D:\media\broken.mp4`, Kind: MediaVideo,
        Phase: Phase1, FieldsMask: MaskVideoDuration | MaskVideoContactSheet}
    result := &JobResultMsg{JobID: job.JobID, Path: job.Path, Kind: job.Kind,
        Errors: []FieldError{
            {Field: MaskVideoDuration, Stage: "video_probe", Msg: "decoder rejected stream"},
            {Field: MaskVideoContactSheet, Stage: "video_probe", Msg: "decoder rejected stream"},
        }}
    if err := validateWorkerResult(job, result); err != nil {
        t.Fatalf("file-level media errors rejected: %v", err)
    }
}
```

- [ ] **Step 2: 运行测试确认 RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc ./internal/worker -run 'Test(SessionPipelineFileErrorSplitsRequestedFieldBits|VideoFileErrorsAcceptOneRequestedBitEach)'
```

Expected: `SessionPipelineFileError...` FAIL，当前实现把两个位合在一个 `FieldError.Field` 中；父进程契约测试应 PASS，证明校验器无需放宽。

- [ ] **Step 3: 最小修复字段错误构造**

```go
func sessionPipelineFileError(result *worker.JobResultMsg, fields uint32, stage string, err error) *worker.JobResultMsg {
    if fields == 0 {
        result.Errors = append(result.Errors, worker.FieldError{Field: 0, Stage: stage, Msg: err.Error()})
        return result
    }
    for bit := uint32(1); fields != 0; bit <<= 1 {
        if fields&bit == 0 {
            continue
        }
        result.Errors = append(result.Errors, worker.FieldError{Field: bit, Stage: stage, Msg: err.Error()})
        fields &^= bit
    }
    return result
}
```

不修改 `validateWorkerResult`，不把硬协议错误降级。

- [ ] **Step 4: 运行会话管线和 Worker 全包测试**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc ./internal/worker
```

Expected: PASS；媒体打开、探测或解码失败会保留具体 stage，而不是因为组合字段再次触发协议崩溃。

- [ ] **Step 5: 提交 Task 2**

```powershell
git add -- internal/wproc/pipeline_session.go internal/wproc/pipeline_session_test.go internal/worker/supervisor_test.go
git commit -m "fix: preserve precise media field errors"
```

---

### Task 3: 用真实内容和具体编码验证生产默认 Worker IPC

**Files:**
- Modify: `internal/wproc/run_test.go`
- Modify: `internal/wproc/videocore/bindings.go:7-9,213-241`
- Verify: `videocore/src/video_analysis.cpp:1258-1305`
- Verify: `videocore/src/native_algorithms/image_decode.cpp:568-600`
- Fixture: `testdata/videocore/compat/videos/h264-standard.mp4`
- Fixture: `testdata/videocore/compat/videos/hevc-standard.mkv`
- Fixture: `testdata/videocore/compat/videos/vp9-portrait.webm`
- Fixture: `testdata/videocore/compat/images/synthetic-pattern.jpg`
- Fixture: `testdata/videocore/compat/images/synthetic-bars.png`
- Fixture: `testdata/videocore/compat/images/synthetic-portrait.webp`

**Interfaces:**
- Consumes: `serve(net.Conn, int, Config, pipelineDeps) int`，真实 `videocore.Open/Hash/Analyze` 和 `MsgJob -> MsgSHAQuery -> MsgSHAReply -> MsgResult`。
- Produces: 六个生产形状 IPC 用例，证明 Worker 根据真实图片内容或视频轨道编码完成处理且不退出。

- [ ] **Step 1: 写真实视频编码表驱动测试**

在 `run_test.go` 增加帮助函数，SHA 回复必须精确覆盖查询：

```go
func missingReplyForQuery(query worker.SHAQueryMsg) worker.SHAReplyMsg {
    return worker.SHAReplyMsg{
        JobID: query.JobID,
        RequestedFields: query.RequestedFields,
        MissingFields: query.RequestedFields,
        RequestedFrames: query.RequestedFrames,
        MissingFrames: query.RequestedFrames,
    }
}
```

新增 `TestServeProductionSessionDecodesVideoTrackCodec`，表项固定为：

```go
tests := []struct{ name, fixture, codec string }{
    {"h264 mp4", `h264-standard.mp4`, "h264"},
    {"hevc mkv", `hevc-standard.mkv`, "hevc"},
    {"vp9 webm", `vp9-portrait.webm`, "vp9"},
}
```

每个子测试必须：

1. 以 `os.Stat` 的真实 size/mtime 构造 `Phase1 + MediaVideo + MaskAllVideo` job；
2. 使用 `pipelineDeps{runtime: testReadyRuntimeInfo}`，不能注入 `videoPipelineDeps` 或 fake `session`；
3. 读取 `MsgSHAQuery`，回复 `missingReplyForQuery(query)`；
4. 读取 `MsgResult`，断言 `FieldsDone == MaskAllVideo`、`DurationMS > 0`、接触表路径存在、PDQ 为 32 字节、尺寸为正且 `Errors` 为空；
5. 发送 `MsgShutdown` 并断言 `serve` 返回 `0`。

- [ ] **Step 2: 写真实图片内容表驱动测试**

新增 `TestServeProductionSessionDecodesImageContentFormat`：

```go
tests := []struct{ name, fixture string }{
    {"jpeg", `synthetic-pattern.jpg`},
    {"png", `synthetic-bars.png`},
    {"webp", `synthetic-portrait.webp`},
}
```

每个子测试走相同真实 IPC，job 使用 `MediaImage + MaskAllImage`，并断言 `FieldsDone == MaskAllImage`、PDQ 32 字节、宽高为正、`Errors` 为空、Worker 正常关闭。

- [ ] **Step 3: 运行真实媒体测试**

Run:

```powershell
$env:VC_TESTDATA_ROOT = (Resolve-Path 'testdata\videocore\compat\videos').Path
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc -run 'TestServeProductionSessionDecodes(VideoTrackCodec|ImageContentFormat)'
```

Expected: 六个用例 PASS。视频测试必须经过 `av_find_best_stream -> avcodec_parameters_to_context -> avcodec_open2`；图片测试必须经过 JPEG/PNG/WebP 内容分支。

若任一用例返回文件级错误，记录 `result.Errors` 的准确 stage/message 和 VideoCore 返回码并停止本任务；只在把该首个违约函数和最小修复代码补进本计划后继续，不启用旧管线或修改期望值。

- [ ] **Step 4: 修复 Analyze 请求内嵌未固定 Go 指针**

真实 H.264 RED 已确定：`cgoBridge.analyze` 把 Go `[]uint16` 数据地址写入
`C.vc_analysis_request.temporary_jpeg_path`，再传入包含该 Go 指针的 C struct，触发：

```text
panic: runtime error: argument of cgo function has Go pointer to unpinned Go pointer
internal/wproc/videocore/bindings.go:238
```

这不是 codec 失败。必须让请求结构只引用 C 拥有的 UTF-16 副本。先在 cgo preamble
加入 `<stdlib.h>` 和有溢出检查的复制/释放函数：

```c
#include <stdlib.h>

static uint16_t* go_vc_copy_utf16(const uint16_t* source, uint32_t units) {
    if (source == NULL || units == 0u ||
        (size_t)units > SIZE_MAX / sizeof(uint16_t)) {
        return NULL;
    }
    size_t bytes = (size_t)units * sizeof(uint16_t);
    uint16_t* copy = (uint16_t*)malloc(bytes);
    if (copy == NULL) return NULL;
    memcpy(copy, source, bytes);
    return copy;
}

static void go_vc_free(void* value) {
    free(value);
}
```

在 `cgoBridge.analyze` 组装 `nativeRequest` 时复制到 C heap，并在 Analyze 返回后释放：

```go
var nativeTemporaryPath *C.uint16_t
if len(temporaryPath) != 0 {
    nativeTemporaryPath = C.go_vc_copy_utf16(
        (*C.uint16_t)(unsafe.Pointer(unsafe.SliceData(temporaryPath))),
        C.uint32_t(len(temporaryPath)),
    )
    runtime.KeepAlive(temporaryPath)
    if nativeTemporaryPath == nil {
        return AnalysisResult{}, &NativeError{
            Code: StatusOOM, Message: "temporary JPEG path allocation failed",
        }
    }
    defer C.go_vc_free(unsafe.Pointer(nativeTemporaryPath))
    nativeRequest.temporary_jpeg_path = nativeTemporaryPath
    nativeRequest.temporary_jpeg_path_units = C.uint32_t(len(temporaryPath))
}
```

删除旧的 `nativeRequest` 内嵌 Go slice 指针赋值和调用后的对应 `KeepAlive`。不要使用
`cgocheck=0`、环境开关或旧管线绕过；C 副本的生命周期必须精确覆盖单次
`vc_media_analyze`。

- [ ] **Step 5: 重跑六夹具确认具体根因 GREEN**

Run:

```powershell
$env:VC_TESTDATA_ROOT = (Resolve-Path 'testdata\videocore\compat\videos').Path
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/wproc -run 'TestServeProductionSessionDecodes(VideoTrackCodec|ImageContentFormat)'
```

Expected: H.264 不再触发 cgo panic，H.264/HEVC/VP9 与 JPEG/PNG/WebP 六项全部 PASS；
每个用例都收到 `MsgResult` 并以 `MsgShutdown` 令 `serve` 返回 `0`。

- [ ] **Step 6: 运行原生编码兼容测试与 Go race**

Run:

```powershell
& 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\ctest.exe' --test-dir videocore/build -C Release --output-on-failure
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/wproc ./internal/worker
```

Expected: PASS；H.264、HEVC、VP9 现有 native golden 继续一致，无数据竞争。

- [ ] **Step 7: 提交 Task 3**

```powershell
git add -- internal/wproc/run_test.go internal/wproc/videocore/bindings.go
git commit -m "fix: keep VideoCore analyze paths in C memory"
```

---

### Task 4: 让任务卡完整显示全部文字

**Files:**
- Modify: `nodetray/frontend/src/pages/LocalTaskItem.tsx:15-16,63-84`
- Modify: `nodetray/frontend/src/pages/LocalTaskItem.test.tsx:25-48`
- Modify: `nodetray/frontend/src/app.css:221-284,409-421`

**Interfaces:**
- Consumes: 现有 `LocalTask` 与 `LocalTaskControl`，不改变 API、轮询或生命周期参数。
- Produces: 完整 task ID、进度、速度、失败数和耗时；宽屏两层布局，窄屏纵向重排。

- [ ] **Step 1: 把任务卡测试改为要求完整任务 ID**

```tsx
it('完整显示任务 ID、进度和指标', () => {
  render(<LocalTaskItem task={runningTask} locked={false} onAction={vi.fn()} />)
  const item = screen.getByRole('listitem')
  expect(screen.getByTitle(runningTask.taskId)).toHaveTextContent(runningTask.taskId)
  expect(item).toHaveTextContent('40 / 100')
  expect(item).toHaveTextContent('12 文件/秒 · 失败 2 · 00:03:12')
  expect([...item.querySelectorAll('[data-local-task-field]')].map((node) => node.getAttribute('data-local-task-field')))
    .toEqual(['identity', 'status', 'progress', 'count', 'metrics', 'actions'])
})
```

保留现有操作回调测试，确保 `taskId/instanceId/expectedRevision` 不变。

- [ ] **Step 2: 运行 RTL 测试确认 RED**

Run:

```powershell
npm --prefix nodetray/frontend test -- --run src/pages/LocalTaskItem.test.tsx
```

Expected: FAIL；当前 `visibleTaskId` 只输出前 12 个字符和省略号。

- [ ] **Step 3: 删除主动截断并输出完整 ID**

删除 `visibleTaskId`，把 ID 节点改为：

```tsx
<span className="local-task-item__id" title={task.taskId}>{task.taskId}</span>
```

- [ ] **Step 4: 调整两层网格和换行 CSS**

核心规则改为：

```css
.local-task-item {
  display: grid;
  grid-template-columns: minmax(220px, 1.4fr) minmax(150px, 0.8fr) auto;
  grid-template-areas:
    'identity status actions'
    'progress count metrics';
}

.local-task-item__identity { grid-area: identity; }
.local-task-item__status { grid-area: status; }
.local-task-item__progress { grid-area: progress; }
.local-task-item__count { grid-area: count; }
.local-task-item__metrics { grid-area: metrics; }
.local-task-item__actions { grid-area: actions; }

.local-task-item__id,
.local-task-item__count,
.local-task-item__metrics {
  overflow-wrap: anywhere;
  white-space: normal;
}
```

在现有窄屏 media query 中使用单列区域：

```css
.local-task-item {
  grid-template-columns: minmax(0, 1fr);
  grid-template-areas: 'identity' 'status' 'progress' 'count' 'metrics' 'actions';
}
```

- [ ] **Step 5: 运行前端完整验证**

Run:

```powershell
npm --prefix nodetray/frontend test -- --run
npm --prefix nodetray/frontend run lint
npm --prefix nodetray/frontend run build
```

Expected: 全量测试和 build PASS；lint 为 0 error，允许仓库既有的 3 条 `react-refresh` warning。build 后精确恢复本轮生成的 tracked `nodetray/frontend/dist` 文件并删除仅由本轮产生的新 hash 文件，不触碰其他用户改动。

- [ ] **Step 6: 提交 Task 4**

```powershell
git add -- nodetray/frontend/src/pages/LocalTaskItem.tsx nodetray/frontend/src/pages/LocalTaskItem.test.tsx nodetray/frontend/src/app.css
git commit -m "fix: show complete local task details"
```

---

### Task 5A: 修复正式构建的 VideoCore 供应链门禁基线

**阻断证据（Task 5 首轮）：**
- `videocore_image_object_provenance` 与 mutation baseline 的 11 个对象都只包含配置批准的 `C:\vcpkg\installed\x64-windows-static\include` 和 `...\include\webp`；测试仍硬编码旧 `videocore\build\vcpkg_installed`，没有发现额外 include 注入。
- Level-B `legacy-golden.tsv` 的 SHA 与固定值一致，20 行和 9 个批准差异的内部验证一致；`manifest.json` 的 capture 规范序列化 SHA 精确等于既有独立固定值 `9a5528...`，但 `.gitattributes eol=crlf` 把 capture 输出末尾的裸 LF 转换成 CRLF，破坏了冻结字节。
- `internal/wproc` 在 `scripts/test-cgo.ps1` 提供正确 VideoCore DLL PATH、fixture root 与规范 TEMP 后，普通和 race 均 PASS；直接 `go test` 的 `0xc0000135` 是 DLL 装载环境错误，不是产品回归。

**Files:**
- Modify: `videocore/CMakeLists.txt`
- Modify: `videocore/tests/test_image_object_provenance.ps1`
- Modify: `videocore/tests/test_image_object_provenance_mutation.ps1`
- Modify: `.gitattributes`
- Regenerate exactly: `videocore/testdata/legacy_level_b/manifest.json`

**Interfaces:**
- `test_image_object_provenance.ps1` 接收 CMake 配置解析出的 `VCPKG_INSTALLED_DIR` 和 `VCPKG_TARGET_TRIPLET`，仍只允许 `<installed>/<triplet>/include` 与其 `webp` 子目录，集合必须精确相等。
- mutation gate 必须把同一配置透传给每个合成案例，现有 extra-include 等负例继续失败。
- Level-B manifest 按 capture 脚本的规范序列化字节保存；`.gitattributes` 对该冻结证据使用 `-text`，禁止 Git checkout 改写字节。既有外部 pin 不变，禁止用当前错误哈希替换固定值。

- [ ] **Step 1: 记录真实 RED**

在当前标准依赖配置上运行 CTest #14/#15/#17，断言分别以旧 build-local include mismatch、manifest hash mismatch 失败；同时记录真实 TLog 仅含两个批准的标准 include。

- [ ] **Step 2: 让 provenance gate 使用显式配置身份**

从 CMake 将 `VCPKG_INSTALLED_DIR` 与 `VCPKG_TARGET_TRIPLET` 传给 provenance 与 mutation 脚本。脚本解析为绝对路径后构造两个且仅两个 expected external includes；缺失/空配置必须 fail closed。不得把任意实际 TLog 路径自动加入 allowlist。

- [ ] **Step 3: 恢复冻结 manifest 的 capture 原始字节**

把 `.gitattributes` 中该文件规则改为 `-text`，用与 `capture_videocore_level_b_legacy.ps1` 相同的 `ConvertTo-Json -Depth 8`、UTF-8 无 BOM、末尾单个 LF 机械重生成 `manifest.json`。断言文件 SHA 仍为既有 `9A552825...`，golden SHA 仍为 `95E019F0...`；不得修改两个 pin。

- [ ] **Step 4: GREEN 与负例回归**

重新 configure/build 后运行 #14/#15/#17/#18；预期通过。确认 mutation 测试仍拒绝 extra include、forced include、response file、工具 shadow 与自签 Level-B 变异。随后 fresh 全量 CTest 必须 18/18。

- [ ] **Step 5: 提交 Task 5A**

```powershell
git add -- .gitattributes videocore/CMakeLists.txt videocore/tests/test_image_object_provenance.ps1 videocore/tests/test_image_object_provenance_mutation.ps1 videocore/testdata/legacy_level_b/manifest.json
git commit -m "fix: align VideoCore provenance with standard dependencies"
```

---

### Task 5B: 修复远程目录浏览会话状态与 Web lint 门禁

**阻断证据（Task 5 第二轮）：**
- Task 5A 后正式构建已通过 VideoCore CTest 18/18、exports 10/10、native closure 6/6、Web 测试 205/205；`build-web.ps1` 在 `RemotePathBrowser.tsx:56` 的 `react-hooks/set-state-in-effect` fail closed。
- 当前 effect 只同步清空 `selectedPath`，不会立即清空 `currentPath/entries/cursor/error`；关闭重开或切换机器后，新根请求完成前仍会显示并允许添加旧会话路径。
- 不能简单把 `showHidden` 加到 effect 依赖：toggle handler 的当前目录请求会被 cleanup 取消，继而多发一次根目录请求并丢失当前导航。

**Files:**
- Modify: `webui/src/features/scans/RemotePathBrowser.tsx`
- Modify: `webui/src/features/scans/RemotePathBrowser.test.tsx`

**Interfaces:**
- `showHidden` 是跨开关对话框保留的用户偏好。
- 路径、选择、条目、游标、错误、loading 和请求控制器属于一次 `(open session, machineID)` 会话；关闭或机器变化必须通过卸载旧会话同步清除，旧值不得在新请求等待期间可见/可提交。
- 初始根请求每个新会话只发一次，并使用当时的隐藏项偏好；会话内切换隐藏项只刷新当前目录一次。
- 保留现有 AbortSignal、aborted 检查和 controller identity，迟到旧请求不得覆盖新结果或错误地清除新 loading。

- [ ] **Step 1: 建立行为与 lint RED**

新增确定性测试：进入子目录后关闭再打开并延迟根响应时，旧路径/条目不可见且添加禁用；隐藏项切换只发一次当前目录请求；旧 A 被取消后迟到不得覆盖已完成 B；隐藏偏好为 true 时重开只发一次 true 根请求。运行聚焦测试并保留当前 stale-session 失败；运行 lint 保留现有 effect 错误。

- [ ] **Step 2: 分离持久偏好和浏览会话**

外层组件仅持有 `showHidden` 偏好；`open=false` 不挂载会话，`open=true` 按 `machineID` 挂载新的内部 session。内部所有浏览状态从空值初始化，effect 只发初始根请求和 cleanup，不同步 setState。不得加 eslint disable。

- [ ] **Step 3: GREEN 与提交**

运行 RemotePathBrowser 聚焦测试、Web 全量测试、lint 和 build；预期测试全绿、lint 0 error（现有无关 warning 可保留）、build PASS。只提交两个白名单文件：

```powershell
git add -- webui/src/features/scans/RemotePathBrowser.tsx webui/src/features/scans/RemotePathBrowser.test.tsx
git commit -m "fix: isolate remote browser sessions"
```

---

### Task 5: 全量验证、构建和发布 Compute/Manager 包

**Files:**
- Verify: `scripts/build.ps1`
- Verify: `scripts/build-nodetray.ps1`
- Verify: `scripts/package-node-release.ps1`
- Verify: `scripts/package-manager-release.ps1`
- Output: `artifacts/releases/MySingerServer-compute-win-x64-20260816-local-task-video-fix.zip`
- Output: `artifacts/releases/MySingerServer-compute-win-x64-20260816-local-task-video-fix.zip.sha256`
- Output: `artifacts/releases/MySingerServer-manager-win-x64-20260816-local-task-video-fix.zip`
- Output: `artifacts/releases/MySingerServer-manager-win-x64-20260816-local-task-video-fix.zip.sha256`
- Deploy: `artifacts/releases/MySingerServer-Compute`

**Interfaces:**
- Consumes: Tasks 1–4 的已提交 HEAD、标准 Go/CMake/vcpkg/npm 工具链和发布脚本。
- Produces: manifest 与 SHA-256 自洽的 Compute/Manager 包，以及保留原 `data` 的经校验 Compute 展开目录。

- [ ] **Step 1: 运行受影响与仓库级自动化门禁**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -p=1 -count=1 ./internal/worker ./internal/wproc ./internal/store ./internal/agent ./cmd/agent ./internal/nodetray/app ./nodetray
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -race -p=1 -count=1 ./internal/worker ./internal/wproc ./internal/agent
npm --prefix nodetray/frontend test -- --run
npm --prefix nodetray/frontend run lint
npm --prefix nodetray/frontend run build
git diff --check
```

Expected: Go、race、前端测试和 build PASS；lint 0 error；`git diff --check` exit 0。

- [ ] **Step 2: 使用全新 stage 构建全部 Windows 组件**

```powershell
$stage = 'artifacts\stage\local-task-video-fix-20260816'
& pwsh -NoProfile -File scripts\build.ps1 `
  -Go 'C:\tmp\go1.26.5\go\bin\go.exe' `
  -StageDir $stage
```

Expected: build、VideoCore CTests、导出表、原生依赖解析和 NodeTray build 全部 PASS；stage 中至少存在 `agent.exe`、`worker.exe`、`videocore.dll`、`nodetray.exe`、`gui.exe`。

- [ ] **Step 3: 生成 Compute 与 Manager ZIP 和 sidecar**

```powershell
$revision = (git rev-parse HEAD).Trim()
& pwsh -NoProfile -File scripts\package-node-release.ps1 `
  -StageDir 'artifacts\stage\local-task-video-fix-20260816' `
  -OutputDir 'artifacts\releases' `
  -ReleaseId '20260816-local-task-video-fix' `
  -BuildDate '2026-08-16' `
  -SourceRevision $revision
& pwsh -NoProfile -File scripts\package-manager-release.ps1 `
  -StageDir 'artifacts\stage\local-task-video-fix-20260816' `
  -OutputDir 'artifacts\releases' `
  -ReleaseId '20260816-local-task-video-fix' `
  -BuildDate '2026-08-16' `
  -SourceRevision $revision
```

Expected: 两个脚本均完成内部解压、manifest 文件清单和哈希闭包验证后发布 ZIP 与 sidecar。

- [ ] **Step 4: 校验归档并安全更新 Compute 展开目录**

先确认 `MySingerServer-Compute` 下的 NodeTray、Agent、Worker 进程身份并通过产品退出流程停止，不能在进程持有二进制时覆盖。将新 Compute ZIP 解压到同级新目录，核对 `release-manifest.json` 后，仅把旧目录的 `data` 移入新目录；用目录重命名原子切换，并保留旧目录为带 HEAD 后缀的可恢复备份，直到运行验收通过。

必须断言：

```powershell
$zip = 'artifacts\releases\MySingerServer-compute-win-x64-20260816-local-task-video-fix.zip'
$sidecar = "$zip.sha256"
$expected = ((Get-Content -LiteralPath $sidecar) -split '\s+')[0]
$actual = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actual -cne $expected) { throw "COMPUTE_ZIP_SHA256_MISMATCH" }
```

Manager ZIP执行相同校验。`data/agent/agent.db` 和日志的 SHA/大小在切换前后必须一致。

- [ ] **Step 5: 用发布包扫描真实编码夹具**

启动更新后的 Compute 包，创建仅扫描任务覆盖六个仓库夹具的复制目录。验收：

- H.264/MP4、HEVC/MKV、VP9/WebM 均成功；
- JPEG、PNG、WebP 均成功；
- `worker ready` 数量不随每个视频重复增长；
- 数据库无 `exit_code: exit status 2`；
- 任务 `failures=0`，结果含真实时长、帧或图片特征。

- [ ] **Step 6: 复测用户目录并记录精确结果**

在交互用户权限可访问时创建 `I:\MiddleDir\11111111` 的扫描任务，至少等到之前失败的首批视频完成。对比：任务 ID、成功/失败数、`files.status/error`、Agent Worker 重启数和首个错误 stage。

Expected: 不再出现批量 `exit status 2`。若仍有媒体失败，必须显示具体 `native_open`、`video_probe`、`video_frame` 或 `video_contact_sheet` 原因，并回到对应函数修复后重新构建；不得把该项标记 PASS。

- [ ] **Step 7: 记录发布证据**

在最终交付消息中列出：HEAD、Compute/Manager ZIP 绝对路径、两个 SHA-256、stage 依赖闭包结果、真实夹具扫描结果、I 盘扫描结果，以及任何明确的 PARTIAL/BLOCKED 边界。发布产物不提交 Git。
