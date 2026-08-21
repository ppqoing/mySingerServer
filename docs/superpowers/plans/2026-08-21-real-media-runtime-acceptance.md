# Rust V2 真实媒体半小时运行验收实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 在前三个子计划完成后，以只读真实媒体和隔离 Node 状态连续计算 30 分钟，每 2 秒采集任务阶段、Worker、CPU、内存和物理磁盘吞吐，生成中文可审计报告。

**架构：** PowerShell harness 在 `C:\tmp` 建隔离发布布局和 Node data/config/log/cache，提升启动 Node；Rust acceptance client 只通过公开 TCP 协议反复创建强制重算扫描并轮询运行详情；PowerShell 同时采集 OS 进程/物理盘指标和源媒体前后清单，最后把 NDJSON 汇总成中文 Markdown。

**技术栈：** Windows PowerShell 7、Rust/Tokio、真实 release `node.exe`/`worker.exe`、CIM 性能类、JSON Lines。

**规格：** `docs/superpowers/specs/2026-08-21-node-runtime-scheduling-and-task-details-design.md`

**全局约束：** 本计划最后执行。唯一额外验收是一次 30 分钟真实媒体计算；不在此前运行短版真实媒体试验。媒体根从 `RUST_V2_REAL_MEDIA_ROOT` 读取，脚本缺少该环境变量时明确退出且不启动 Node。媒体文件只读，不复制、不删除、不重命名、不写旁车文件。Node 的数据库、配置、日志和缓存全部位于 `C:\tmp\rust-v2-runtime-acceptance`。不运行 workspace 全量测试。

---

### 任务 1：建立验收客户端和输入保护

**Files:**
- Create: `crates/desktop-core/examples/runtime_acceptance.rs`
- Modify: `crates/desktop-core/Cargo.toml`
- Create: `crates/desktop-core/tests/runtime_acceptance_contract.rs`

**Interfaces:**
- Consumes: `RUST_V2_ACCEPTANCE_ENDPOINT`、`RUST_V2_REAL_MEDIA_ROOT`、`RUST_V2_ACCEPTANCE_DURATION_SECONDS`、`RUST_V2_ACCEPTANCE_OUTPUT`。
- Produces: 每 2 秒一行 `RuntimeAcceptanceSample` NDJSON；最终 `RuntimeAcceptanceResult`。

- [ ] **Step 1: 写参数和采样 RED**

测试使用 fake clock/session，不读取媒体。断言 duration 默认 1800 秒且拒绝小于 1800；tick 固定 2 秒；任务在 30 分钟前完成时创建下一次 `force_recalculate=true` 扫描以保持计算；到期取消当前任务并等待终态；枚举器默认 `everything`；所有输出路径必须位于显式 evidence root。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
```

Expected: FAIL，acceptance client 不存在。

- [ ] **Step 3: 实现 client**

每条样本至少包含：UTC/elapsed、runtime task ID、state、overall counts、全部阶段、当前 Worker、最近失败、stale。client 只调用 `create_scan/list_runtime_tasks/get_runtime_task_details/cancel_task`，不调用删除、复核或文件写协议。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test runtime_acceptance_contract --locked -- --test-threads=1
git add -- crates/desktop-core/examples/runtime_acceptance.rs crates/desktop-core/Cargo.toml crates/desktop-core/tests/runtime_acceptance_contract.rs
git commit -m "test: add runtime acceptance client"
```

Expected: PASS；没有访问真实媒体。

---

### 任务 2：实现隔离 Node 启动、2 秒 OS 采样和源媒体清单

**Files:**
- Create: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`
- Create: `tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1`

**Interfaces:**
- Produces: 隔离 staging、`runtime.ndjson`、`system.ndjson`、`media-before.json`、`media-after.json`、Node/Worker 日志。

- [ ] **Step 1: 写纯 fixture harness RED**

PowerShell 测试只使用 fake 可执行文件和 3 个 fixture 文件，覆盖：缺 env 不启动；媒体根不能位于 staging；staging/data 全在 C:\tmp；配置路径为相对路径时仍解析到 staging；采样间隔参数固定 2；before/after 比较发现新增、删除、长度或 LastWriteTimeUtc 改变即失败；输出中不含 PostgreSQL 密码。

- [ ] **Step 2: 运行 RED**

```powershell
& .\tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
```

Expected: FAIL，Measure 脚本不存在。

- [ ] **Step 3: 实现安全 staging**

脚本用 GUID 创建 `C:\tmp\rust-v2-runtime-acceptance\<run-id>`，从指定 release target 复制 `node.exe`、`worker.exe`、runtime/FFmpeg 和 Everything 依赖；写 bootstrap/config，把四路径指向该 staging 下目录。使用 `Start-Process -Verb RunAs -PassThru` 启动 Node 并等待 endpoint；清理只终止本次启动 PID/子 Worker，并保留 evidence。

- [ ] **Step 4: 实现指标采样**

每 2 秒用 `Get-Process` 采集 node/worker 的 CPU delta、WorkingSet、PrivateMemory；用 `Win32_PerfFormattedData_PerfDisk_PhysicalDisk` 采集 ReadBytesPersec、DiskReadBytesPersec、AvgDiskQueueLength。采样通过 PID 和物理盘实例归属，不用本地化 Performance Counter 名称。

- [ ] **Step 5: 运行 GREEN 并提交**

```powershell
& .\tests\windows\Test-RustV2RuntimeAcceptanceHarness.ps1
git add -- tests/windows/Measure-RustV2RuntimeAcceptance.ps1 tests/windows/Test-RustV2RuntimeAcceptanceHarness.ps1
git commit -m "test: stage isolated runtime acceptance"
```

Expected: fixture harness PASS；未启动真实 Node 或访问真实媒体。

---

### 任务 3：生成中文验收报告并锁定通过条件

**Files:**
- Create: `tests/windows/New-RustV2RuntimeAcceptanceReport.ps1`
- Create: `tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1`
- Modify: `tests/windows/Measure-RustV2RuntimeAcceptance.ps1`

**Interfaces:**
- Consumes: runtime/system/media JSON。
- Produces: `docs/verification/2026-08-21-node-runtime-half-hour.md`。

- [ ] **Step 1: 写合成数据 RED**

合成 901 个 2 秒样本，断言报告包含：实际时长、机器 ID、Node 配置摘要、总文件/字节、各阶段耗时与吞吐、Worker 并行峰值/平均值、Node/Worker CPU/内存、每物理盘读吞吐/队列、最近失败、文件故障分类、联系表复用数、磁盘满清理次数、媒体清单一致性和最终结论。

- [ ] **Step 2: 固定失败条件**

以下任一项为 FAIL：实际计算窗口少于 1800 秒；采样最大间隔大于 6 秒且缺口未说明；源媒体清单变化；Node/Worker 非预期退出；任务级失败；在途 Worker 峰值小于 2 且有效 Worker 配置大于 1 且样本有足够媒体项；不同物理盘都有工作但从未重叠读取；出现无限重复同一 runtime/file 失败；evidence 无法解析。

- [ ] **Step 3: 运行 RED**

```powershell
& .\tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1
```

Expected: FAIL，报告生成器不存在。

- [ ] **Step 4: 实现中文报告**

报告明确区分“自动化门禁”“OS 采样观察”“真实媒体未修改证明”和“未触发边界”。磁盘满未发生时写“本次未触发，不能从本次实测证明清理路径”；不把无事件写成已通过实测。

- [ ] **Step 5: 运行 GREEN 并提交**

```powershell
& .\tests\windows\Test-RustV2RuntimeAcceptanceReport.ps1
git add -- tests/windows/New-RustV2RuntimeAcceptanceReport.ps1 tests/windows/Test-RustV2RuntimeAcceptanceReport.ps1 tests/windows/Measure-RustV2RuntimeAcceptance.ps1
git commit -m "test: report runtime acceptance evidence"
```

Expected: 合成 PASS/FAIL fixture 均得到对应结论。

---

### 任务 4：构建与校验最终验收所需 release 产物

**Files:**
- Modify: `scripts/build-release.ps1`
- Modify: `scripts/verify-release.ps1`
- Modify: `tests/windows/Test-RustV2Package.ps1`

**Interfaces:**
- Verifies: node/worker、FFmpeg、Everything、bootstrap 默认文件和协议 V3 依赖完整。

- [ ] **Step 1: 写包内容 RED**

现有 package test 增加断言：`node.exe`、`worker.exe`、Everything、runtime DLL、默认 `bootstrap.toml` 和 Node config 模板存在；模板默认 Everything、3 秒 timeout、2 retry、MD5 contact-sheet cache 子目录语义。不得包含真实验收媒体或 evidence。

- [ ] **Step 2: 运行 RED**

```powershell
& .\tests\windows\Test-RustV2Package.ps1
```

Expected: FAIL，缺少新默认配置/引导内容或 release 尚未构建。

- [ ] **Step 3: 实现相关打包更新**

只更新 Rust V2 release staging 所需配置和依赖复制；不改旧 Go 发布链，不清理其他 target 或 dist。

- [ ] **Step 4: 构建和验证**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo build -p node -p worker -p desktop --release --locked --target x86_64-pc-windows-msvc
& .\scripts\build-release.ps1 -CargoTargetDir 'C:\tmp\rust-v2-node-runtime-target' -SkipBuild
& .\tests\windows\Test-RustV2Package.ps1
```

Expected: 三步 PASS；输出最终 staging/ZIP 路径和 SHA-256。

- [ ] **Step 5: 提交**

```powershell
git add -- scripts/build-release.ps1 scripts/verify-release.ps1 tests/windows/Test-RustV2Package.ps1
git commit -m "build: package node runtime scheduling defaults"
```

Expected: 只包含 release 相关三文件。

---

### 任务 5：执行唯一一次真实媒体 30 分钟计算验收

**Files:**
- Create after run: `docs/verification/2026-08-21-node-runtime-half-hour.md`
- Modify: `AGENTS.md`

**Interfaces:**
- Consumes: 用户提供的 `$env:RUST_V2_REAL_MEDIA_ROOT`。
- Produces: 30 分钟证据目录和中文报告。

- [ ] **Step 1: 只读确认输入和磁盘空间**

```powershell
$mediaRoot=(Get-Item -LiteralPath $env:RUST_V2_REAL_MEDIA_ROOT).FullName
if (-not (Test-Path -LiteralPath $mediaRoot -PathType Container)) { throw 'RUST_V2_REAL_MEDIA_ROOT 必须是现有目录' }
Get-Volume | Select-Object DriveLetter,FileSystemLabel,SizeRemaining,Size
```

Expected: 媒体根存在；C: 有足够空间保存隔离 Node 数据和 evidence。此步骤不改媒体。

- [ ] **Step 2: 执行 30 分钟验收**

```powershell
$env:RUST_V2_ACCEPTANCE_DURATION_SECONDS='1800'
$env:RUST_V2_ACCEPTANCE_CARGO_TARGET='C:\tmp\rust-v2-node-runtime-target'
& .\tests\windows\Measure-RustV2RuntimeAcceptance.ps1 `
    -MediaRoot (Get-Item -LiteralPath $env:RUST_V2_REAL_MEDIA_ROOT).FullName `
    -DurationSeconds 1800 `
    -SampleSeconds 2 `
    -CargoTargetDir 'C:\tmp\rust-v2-node-runtime-target'
```

Expected: 运行至少 1800 秒，Node/Worker 计算持续推进，脚本退出 0；UAC 提示只用于本次隔离 Node。

- [ ] **Step 3: 核验报告与原始证据**

```powershell
Get-Content -LiteralPath '.\docs\verification\2026-08-21-node-runtime-half-hour.md'
Get-ChildItem -LiteralPath 'C:\tmp\rust-v2-runtime-acceptance' -Recurse -File | Select-Object FullName,Length,LastWriteTime
```

Expected: 报告结论为 PASS；runtime/system NDJSON 可解析；media before/after 完全一致；报告明确列出未触发的边界。

- [ ] **Step 4: 更新架构验证边界并精确提交**

`AGENTS.md` 记录真实验收命令、2 秒采样和“实测/未触发”区分。

```powershell
git diff --check
git add -- docs/verification/2026-08-21-node-runtime-half-hour.md AGENTS.md
git commit -m "test: verify node runtime for thirty minutes"
```

Expected: 提交只包含中文报告和架构验证边界；原始大体积 evidence 留在 C:\tmp，不提交。
