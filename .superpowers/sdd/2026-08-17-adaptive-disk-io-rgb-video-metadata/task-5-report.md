# Task 5 实施报告：VideoCore source I/O governor

## 基线与范围

- 日期：2026-08-17。
- 实施前 `HEAD`：`865be7c1415065d5183693c56319d0428b081aa2`，与 brief 要求的 BASE 完全一致；实施前工作树 clean。
- 严格按 Task 5 白名单修改。计划漏列经总任务明确授权后，仅额外修改：
  - `videocore/tests/test_abi.cpp`：同步 ABI v2、2.0.0、结构大小/偏移及 governor 严格校验契约。
  - `internal/wproc/run_test.go`：机械同步 `testReadyRuntimeInfo` 及两处 Ready 断言到 `videocore.ABIVersion` / `videocore.Version`；不改变其他测试语义。
- 未执行 Task 6。

## RED 证据

先新增 native 与 Go 契约测试，再写生产实现。

1. native focused RED：
   - 命令：
     - `cmake.exe --build videocore\build --config Release --target test_vc_win_file test_vc_deadline`
     - `ctest.exe --test-dir videocore\build -C Release -R 'videocore_(win_file|deadline)' --output-on-failure`
   - 真实失败：`VC_IO_OPERATION_READ`、`vc_io_governor` 未定义；`WinFile::Read/Seek` 尚不接受同一 mutable `Deadline*`。
2. Go focused RED：
   - 命令：`go.exe test -p=1 -count=1 ./internal/wproc/videocore -run TestIOGovernor`
   - 真实失败：`OpenOptions` 不含 `IOGovernor/ioGovernorContext`，缺少 `newIOGovernorHandle`、`invokeIOAcquire`。
3. 环境型假 RED 已单独隔离：
   - 默认 Go cache 访问拒绝；改用授权目录下的独立 `GOCACHE`。
   - 沙箱内 MSBuild FileTracker 返回 `E_ACCESSDENIED`，且进程环境同时存在 `PATH`/`Path`；完整 Windows 构建在沙箱外以单一 `Path` 运行。
   - D: 一度无剩余空间；只删除本任务生成的 `.task5-gocache` 后恢复约 95 MiB，并将后续 cache/Stage 放到 C: 授权隔离目录。
   - brief 指定的 CMake 4.2 与现有 build cache 的 CMake 4.4 `CMAKE_ROOT` 不一致；RED 命令保留原工具证据，GREEN 构建使用 cache 对应的 `C:\Tools\WinLibs\mingw64\bin\cmake.exe`。

## 实现结果

- ABI 升级为 v2 / `2.0.0`；Go binding、Worker Ready 与 native ABI 常量一致，旧 v1 runtime fail closed。
- `vc_io_governor` 与 `vc_media_open_options.io_governor` 采用严格结构大小、ABI 版本、非空 acquire/report 校验。
- Go 仅把 `runtime/cgo.Handle` 的数值放入 C `uintptr_t`；C/C++ 不持有 Go 指针。open 成功/失败、取消、panic 展开、close 与重复 close 均通过单次 `Delete` 生命周期控制；回调 panic 被恢复，取消/基础设施错误脱敏。
- `WinFile` 在每次真实 `ReadFile` / `SetFilePointerEx` 前 acquire；拒绝时不触碰文件句柄；结束时 report 实际字节、真实 I/O 耗时和状态。
- governor 等待与 I/O 测量使用同一 steady clock；等待时长延长同一个 operation `Deadline`，等待不消耗 probe/frame deadline，真实 I/O 仍受原预算约束。
- AVIO、SHA-512 hash 与分析路径共享同一个 mutable `Deadline*`。governor 只传入 source `WinFile`；缓存和临时文件未设置 governor。

## GREEN 与最终验证

1. focused native：
   - `ctest.exe --test-dir videocore\build -C Release -R 'videocore_(win_file|deadline)' --output-on-failure`
   - 结果：2/2 PASS。
2. ABI + focused native：
   - `ctest.exe --test-dir videocore\build -C Release -R 'videocore_(abi|win_file|deadline)' --output-on-failure`
   - 结果：3/3 PASS。
3. 全量 native：
   - `ctest.exe --test-dir videocore\build -C Release --output-on-failure`
   - 结果：20/20 PASS，0 failures，69.89 秒；完整脚本内复跑为 20/20 PASS，66.89 秒。
4. 完整 VideoCore gate：
   - `scripts\build.ps1 -VideoCoreOnly -StageDir <fresh-stage> -CMake C:\Tools\WinLibs\mingw64\bin\cmake.exe -Go C:\tmp\go1.26.5\go\bin\go.exe`
   - 结果：20/20 native tests、10/10 exact exports、recursive DLL closure 6 files，fresh Stage 创建成功。
5. focused Go/cgo：
   - `go.exe test -p=1 -count=1 ./internal/wproc/videocore -run TestIOGovernor`
   - 结果：PASS；另外真实 `Open`/`Hash` 经 DLL、cgo callback、native `WinFile` 的集成用例 PASS。
6. brief 指定完整 CGO：
   - `scripts\test-cgo.ps1 -Packages '.\internal\wproc\...' -DllDir '.\videocore\build\Release'`
   - 在沙箱外使用隔离 `TEMP`/`TMP`/`GOCACHE` 与 `VC_TESTDATA_ROOT=testdata\videocore\compat\videos`。
   - 结果：`internal/wproc`、`internal/wproc/mediacore`、`internal/wproc/videocore` 全部 PASS。
7. 完整相关 race：
   - `scripts\test-cgo.ps1 -Race -Packages '.\internal\wproc\...' -DllDir '.\videocore\build\Release'`
   - 结果：三包全部 PASS；主包 37.178 秒，mediacore 1.154 秒，videocore 1.559 秒。
8. Worker focused：
   - `go.exe test -p=1 -count=1 ./internal/worker -run 'Test.*VideoCore|TestMessageRoundTrip'`
   - 结果：PASS。

## SHA 与产物

- BASE SHA：`865be7c1415065d5183693c56319d0428b081aa2`。
- fresh Stage：`C:\Users\Administrator\.codex\visualizations\2026\08\15\01a006c2-7700-7bf0-91cd-7006c3aa2238\task5-stage-green-20260817`。
- `videocore.dll` SHA-256：`37F254B8F97DED1AAC80391487EA37D7995467CD9FC2F8AB7FD0F3365750349F`。
- `native-dependencies.json` SHA-256：`03ED5B22146D24C4B394A9471D3A72667820AE49C7BA2A8F41C00A4230572BDC`。
- 最终提交 SHA 由提交后的 `git rev-parse HEAD` 记录；Git commit 无法在自身内容中稳定自引用。

## 风险与边界

- D: 最终剩余空间很低（约 97 MiB）；后续全量重构建应继续把 cache/Stage 放到 C: 隔离目录。
- `internal/wproc/videocore/libvideocore.a` 会被完整构建脚本机械重生，但不在批准白名单，因此未暂存、未提交。
- Windows canonical-path 测试在沙箱内会因权限模型误报 `Access is denied`；本报告 PASS 证据来自同一 checkout、隔离临时目录下的沙箱外复跑。
- 未执行 GUI、真实慢盘或跨进程生产负载验收；本任务验收范围为 native/Go/cgo/race 自动化门禁。

## 修复轮次 1（独立审查 CHANGES_REQUESTED）

### 基线、范围与根因

- 修复基线 SHA：`aae81ec72ab4723a83c853dcd4b3099d83563498`，开始时工作树 clean。
- 完整读取 `task-5-review.md` 后逐项核验，确认 2 个 Critical 与 1 个 Important 均可稳定复现：
  1. `serve` 为 job 创建了 `localIOLeaseClient`，但 session pipeline 的 `videocore.OpenOptions` 没有携带它；只有最终 Go rehash 使用 governor，native source hash/analyze 绕过租约。
  2. Worker Ready 把 `runtimeInfo.Version` 同时写入 `DLLVersion` 和 `VideoCoreVersion`；ABI v2 后前者错误变成 `2.0.0`，而 Supervisor 正确要求 MediaCore `1.0.0`。
  3. `task17_test.go` crash replacement 夹具仍硬编码 VideoCore `1.0.0`。
- 本轮只修改原 Task 5 报告及明确追加授权的四个文件：
  - `internal/wproc/run.go`
  - `internal/wproc/pipeline_session.go`
  - `internal/wproc/run_test.go`
  - `internal/worker/task17_test.go`
- 未执行 Task 6，未修改其他生产路径。

### 修复 RED

1. serve → native source governor 集成 RED：
   - 新增真实 `serve → videocore.Open → native Hash` fixture，允许 SHAQuery 前出现租约消息，并用 native 特有 seek 区分最终 Go rehash。
   - 命令：`go.exe test -p=1 -count=1 .\internal\wproc -run '^TestServeProductionSessionGovernsNativeSourceIO$'`
   - 失败：`acquires=1 seek_acquires=0 reports=1 reported_seeks=0`，证明只有最终 rehash 受控，native source 没有接线。
2. Ready 字段 RED：
   - 命令：`go.exe test -p=1 -count=1 .\internal\wproc -run 'TestServeReadyReportsVideoCoreRuntime|TestServeSendsReadyAndHandlesShutdown'`
   - 两项均失败；实际 `DLLVersion="2.0.0"`，期望 `worker.MediaCoreDLLVersion`（`1.0.0`）。
3. task17 RED：
   - 命令：`go.exe test -p=1 -count=1 .\internal\worker -run '^TestPoolReplacementReadyAfterNativeCrash$'`
   - 稳定失败；replacement Ready 实际 `VideoCoreVersion="2.0.0"`，旧断言期待 `1.0.0`。

### 最小修复

- `sessionPipelineDeps` 增加当前 job 的 `videocore.IOGovernor`；`serve` 只在 session pipeline source open 前注入该 job 的 `localIOLeaseClient`，`processMediaWithDeps` 只把它放入 source `videocore.OpenOptions`。contact-sheet cache/temp/sidecar 路径没有该 OpenOptions，不注入 governor。
- Ready 的 `DLLVersion` 固定为 `worker.MediaCoreDLLVersion`；`VideoCoreABI` / `VideoCoreVersion` 仍来自真实 VideoCore runtime。
- production fixture 处理 native hash 在 SHAQuery 前产生的 acquire/report，要求 seek acquire 与最终 seek report 均非零，并校验 job identity、完成状态及报告字节。
- Ready gate 测试覆盖当前 v2 接受与显式 v1 拒绝；crash replacement 使用 `VideoCoreVersion` 单一常量来源。

### 修复 GREEN

1. focused production/Ready：
   - `go.exe test -p=1 -count=1 .\internal\wproc -run 'TestServeReadyReportsVideoCoreRuntime|TestServeSendsReadyAndHandlesShutdown|TestServeProductionSessionGovernsNativeSourceIO'`
   - 结果：PASS。
2. focused Ready gate/task17：
   - `go.exe test -p=1 -count=1 .\internal\worker -run 'TestTask5VideoCoreReadyGateAcceptsV2AndRejectsV1|TestPoolReplacementReadyAfterNativeCrash'`
   - 结果：PASS。
3. 完整 Worker：
   - `go.exe test -p=1 -count=1 .\internal\worker`
   - 结果：PASS，1.271 秒。
4. 完整 native：
   - `ctest.exe --test-dir videocore\build -C Release --output-on-failure`
   - 结果：20/20 PASS，0 failures，69.94 秒。
5. 完整 CGO 普通：
   - `scripts\test-cgo.ps1 -Packages '.\internal\wproc\...' -DllDir '.\videocore\build\Release'`
   - 结果：wproc / mediacore / videocore 三包全部 PASS（3.210 秒 / 0.236 秒 / 0.394 秒）。
6. 完整 CGO race：
   - 同命令增加 `-Race`，使用独立 `GOCACHE`。
   - 结果：三包全部 PASS（37.305 秒 / 1.161 秒 / 2.041 秒）。

### 修复 SHA 与风险

- 审查基线 SHA：`aae81ec72ab4723a83c853dcd4b3099d83563498`。
- 本轮最终提交 SHA 由提交后的 `git rev-parse HEAD` 记录；提交不能在自身报告内容中稳定自引用。
- Windows canonical-path 测试继续在沙箱外、隔离 `TEMP`/`TMP`/`GOCACHE` 与仓库 `VC_TESTDATA_ROOT` 下运行；无产品失败或剩余自动化失败。
