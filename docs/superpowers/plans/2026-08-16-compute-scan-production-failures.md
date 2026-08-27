# Compute 扫描全失败修复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 Windows Compute 包中图片走旧 MediaCore、联系表缓存根目录缺失及前置失败产生空 SHA 二次存储错误的问题。

**Architecture:** 默认 Windows Worker 的一筛图片与视频统一进入现有 VideoCore session pipeline；Agent 在构造 Worker Pool 前一次性创建并验证联系表缓存根目录；Store 只接受字段覆盖 SHA 且属于明确前置阶段的无 SHA 失败结果，继续拒绝任意或带不完整特征载荷的空 SHA。

**Tech Stack:** Go 1.26、Windows named-pipe Worker、VideoCore、SQLite、PowerShell 发布脚本。

## 全局约束

- 不重新启用 `legacy_mediacore`，不向发布包增加 `mediacore.dll`。
- 不修改本地任务状态机、扫描调度或前端。
- 缓存根目录只在 Agent 启动阶段创建，单文件处理阶段继续使用现有路径越界和重解析点检查。
- Store 必须保留首个真实字段错误，且继续拒绝不属于允许阶段的空 SHA。
- 发布产物必须来自当前提交，并同时生成 Compute 与 Manager 包。

---

### Task 1: 默认图片切换到 VideoCore session pipeline

**Files:**
- Modify: `internal/wproc/run.go`
- Test: `internal/wproc/run_test.go`
- Modify: `internal/wproc/pipeline_session.go`
- Modify: `VideoCore/src/image_analysis.cpp`
- Test: `VideoCore/tests/test_image_analysis.cpp`

**Interfaces:**
- Consumes: `processMediaWithDeps(context.Context, Config, *worker.JobMsg, sessionPipelineDeps)`
- Produces: 默认 session 模式下，Phase 1 图片不再调用 `processImageWithDeps`。
- Produces: 图片分析使用 ABI 中对当前媒体类型有效的尺寸槽返回原始图片宽高，Go 映射为 `JobResultMsg.Width/Height`；视频联系表语义保持不变。

- [x] **Step 1: 写失败回归测试**

将旧的 `TestImageNoThumbnailServeUsesImagePipelineEvenWithSessionConfigured` 改为生产契约测试：发送 Phase 1 图片，返回缓存未命中答复，并断言 session 的 `open/hash/analyze/rehash/close` 各执行一次、旧图片 hasher/decode 均为零。

```go
if sessionFake.opens != 1 || sessionFake.hashes != 1 || sessionFake.analyzes != 1 ||
    sessionFake.rehashes != 1 || sessionFake.closes != 1 ||
    imageState.hashCalls != 0 || imageState.decodeCalls != 0 {
    t.Fatalf("phase-one image did not use VideoCore session")
}
```

- [x] **Step 2: 运行测试确认 RED**

Run: `go test -p=1 -count=1 ./internal/wproc -run TestImagePhaseOneServeUsesSessionPipeline`

Expected: FAIL，当前旧图片依赖被调用，session 调用计数为零。

- [x] **Step 3: 最小实现**

删除 `serve` 中 session 模式对 `Phase1 + MediaImage` 的特殊分支，使所有合法 Phase 1/2 图片和视频都进入统一的 `processMediaWithDeps` 分支。

```go
if useSessionPipeline {
    if job.Phase != worker.Phase1 && job.Phase != worker.Phase2 {
        result = invalidDispatchResult(&job, "phase", "unsupported worker phase")
    } else if job.Kind != worker.MediaImage && job.Kind != worker.MediaVideo {
        result = invalidDispatchResult(&job, "kind", "unsupported media kind")
    } else {
        result, err = processMediaWithDeps(context.Background(), cfg, &job, sessionDeps)
    }
}
```

VideoCore 图片分析成功时把 `GrayImage.width/height` 写入现有结果尺寸槽；`sessionPipelineMergeAnalysis` 在 `MediaImage` 分支把这两个值映射到图片结果宽高。该槽对视频继续表示联系表尺寸，因此不改变 ABI 布局和导出。

```cpp
out->contact_sheet_width = static_cast<uint32_t>(gray.width);
out->contact_sheet_height = static_cast<uint32_t>(gray.height);
```

- [x] **Step 4: 运行聚焦测试确认 GREEN**

Run: `go test -p=1 -count=1 ./internal/wproc -run 'Test(ImagePhaseOneServeUsesSessionPipeline|ServeDispatchesPhase2ThroughSessionPipeline|SessionPipeline)'`

Run: `ctest --test-dir VideoCore/build -C Release -R '^videocore_image_analysis$' --output-on-failure`

Expected: PASS。

### Task 2: Agent 启动时建立安全缓存根目录

**Files:**
- Modify: `internal/wproc/contact_sheet_cache.go`
- Test: `internal/wproc/contact_sheet_cache_test.go`
- Modify: `cmd/agent/main.go`
- Test: `cmd/agent/main_test.go`

**Interfaces:**
- Produces: `wproc.PrepareContactSheetRoot(root string) error`
- Consumes: `config.AgentConfig.Thumb.CacheDir`

- [x] **Step 1: 写失败回归测试**

测试不存在的嵌套缓存根目录经过准备后成为普通目录，并可被 `contactSheetRoot` 校验；测试普通文件占位时返回错误。Agent 启动测试断言进入后续启动失败前缓存根已经建立。

```go
root := filepath.Join(t.TempDir(), "data", "thumbcache")
if err := PrepareContactSheetRoot(root); err != nil { t.Fatal(err) }
if _, err := contactSheetRoot(root); err != nil { t.Fatal(err) }
```

- [x] **Step 2: 运行测试确认 RED**

Run: `go test -p=1 -count=1 ./internal/wproc ./cmd/agent -run 'TestPrepareContactSheetRoot|TestAgentStartupCreatesThumbCacheRoot'`

Expected: FAIL，准备函数不存在且 Agent 未创建目录。

- [x] **Step 3: 最小实现**

`PrepareContactSheetRoot` 对绝对清理后的路径调用 `os.MkdirAll`，随后复用 `contactSheetRoot` 验证普通目录、符号链接/重解析点和 canonical path；`runWithDependencies` 在 Worker Pool 构造前调用该函数并包装为 `prepare thumb cache root` 错误。

```go
if err := wproc.PrepareContactSheetRoot(cfg.Thumb.CacheDir); err != nil {
    return fmt.Errorf("prepare thumb cache root: %w", err)
}
```

- [x] **Step 4: 运行聚焦测试确认 GREEN**

Run: `go test -p=1 -count=1 ./internal/wproc ./cmd/agent -run 'TestPrepareContactSheetRoot|TestAgentStartupCreatesThumbCacheRoot'`

Expected: PASS。

### Task 3: 保留前置失败并阻止空 SHA 二次错误

**Files:**
- Modify: `internal/store/features.go`
- Test: `internal/store/features_test.go`

**Interfaces:**
- Consumes: `Phase1Result.Errors []FieldError`
- Produces: `validPreSHAFailure(Phase1Result) bool` 的收紧允许阶段集合。

- [x] **Step 1: 写失败回归测试**

扩展表驱动测试，允许 `sha512`、`native_open`、`native_hash`、`stale` 四个明确可发生在 SHA 完成前的阶段；继续断言 `decode`、`thumb_cache`、任意阶段和不覆盖 SHA 的错误被拒绝。

```go
{name: "native hash", stage: "native_hash", allow: true},
{name: "arbitrary", stage: "network", allow: false},
```

- [x] **Step 2: 运行测试确认 RED**

Run: `go test -p=1 -count=1 ./internal/store -run 'TestSavePhase1(PersistsPreSHAFailure|PreSHAFailureWhitelist)'`

Expected: FAIL，新增生产前置阶段仍返回 `SHA-512 must be exactly 64 bytes`。

- [x] **Step 3: 最小实现**

只扩展 `validPreSHAFailure` 的阶段白名单：

```go
case "stat", "open", "read", "sha512", "native_open", "native_hash", "stale":
```

- [x] **Step 4: 运行聚焦与包级测试确认 GREEN**

Run: `go test -p=1 -count=1 ./internal/store -run 'TestSavePhase1(PersistsPreSHAFailure|PreSHAFailureWhitelist)'`

Run: `go test -p=1 -count=1 ./internal/wproc ./internal/store ./internal/agent ./cmd/agent`

Expected: 全部 PASS。

### Task 4: 构建与发布验证

**Files:**
- Verify: `scripts/build.ps1`
- Verify: `scripts/package-portable-release.ps1`
- Output: `artifacts/releases/MySingerServer-Compute`
- Output: `artifacts/releases/MySingerServer-Manager`

**Interfaces:**
- Consumes: 当前 Git HEAD、已验证 VideoCore/native stage。
- Produces: Compute/Manager 便携目录、ZIP、SHA-256 sidecar 和 release manifest。

- [x] **Step 1: 运行完整受影响测试与竞态测试**

Run: `go test -p=1 -count=1 ./internal/wproc ./internal/store ./internal/agent ./cmd/agent`

Run: `go test -race -p=1 -count=1 ./internal/agent`

Expected: PASS。

- [ ] **Step 2: 构建当前源码并生成便携发布包**

使用仓库标准 `scripts/build.ps1` 构建五个 EXE 和 NodeTray，再以 `scripts/package-portable-release.ps1` 同时生成 Compute 与 Manager 发布物；复用 native stage 之前必须核对其 manifest 和依赖闭包。

- [ ] **Step 3: 验证产物**

确认 Worker 仍只导入 `videocore.dll`，Compute 包含新的 `agent.exe/worker.exe`，Manager 包存在；验证 ZIP sidecar、release manifest、文件白名单和包内 `data/agent/agent.json`。

- [ ] **Step 4: 提交**

```powershell
git add -- docs/superpowers/plans/2026-08-16-compute-scan-production-failures.md internal/wproc/run.go internal/wproc/run_test.go internal/wproc/pipeline_session.go internal/wproc/contact_sheet_cache.go internal/wproc/contact_sheet_cache_test.go cmd/agent/main.go cmd/agent/main_test.go internal/store/features.go internal/store/features_test.go VideoCore/src/image_analysis.cpp VideoCore/tests/test_image_analysis.cpp
git commit -m "fix: restore Compute scan processing"
```
