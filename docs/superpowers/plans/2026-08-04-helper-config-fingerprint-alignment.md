# Helper 配置指纹统一实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 让 Helper 按 NodeTray Store 已有的 canonical JSON 字节合同计算有效配置 SHA-256，消除 `control handshake config fingerprint does not match`，并生成未部署的 Windows x64 `helper.exe`。

**架构：** 不修改控制握手协议与严格相等校验，只修正 Helper 端输入摘要的序列化字节：`json.MarshalIndent(..., "", "  ")` 后追加 `\n`，再计算 SHA-256。先用独立合同测试锁定 NodeTray canonical 编码，再更新控制身份集成测试；最后运行相关静态测试并构建单独产物。

**技术栈：** Go 1.26.5、Go `testing`、PowerShell 7、GNU `windres`、Windows Manifest Tool (`mt.exe`)

## 全局约束

- 只修改 Helper 指纹算法及其测试；不修改 Agent、NodeTray 握手、Helper 配置字段或进程生命周期。
- 不接受 compact JSON 指纹，不做双摘要兼容，不降低严格相等校验。
- 产物只写入 `artifacts/helper-config-fingerprint-fix/helper.exe`；不覆盖 `C:\Program Files\MySingerServer\helper.exe`，不启停任何已安装进程。
- 当前 checkout 没有 Git 元数据。版本记录统一标记为 `N/A_NO_GIT_METADATA`；不初始化 Git，也不写虚构 commit 步骤。
- 每个测试命令均使用 `-count=1`，避免测试缓存形成假阳性。
- 若任一必需静态门禁失败，停止交付“已修复”结论；真实界面启动验收保持 `BLOCKED_NOT_RUN_DYNAMIC`。

## 文件映射

- 修改 `cmd/helper/main_test.go`
  - 引入 `encoding/json`。
  - 新增 Helper 指纹与 NodeTray canonical JSON 一致的合同测试。
  - 更新控制身份测试的期望摘要，删除旧的 compact JSON 固定字符串。
- 修改 `cmd/helper/main.go`
  - 将 `effectiveHelperConfigSHA256` 改为缩进 JSON 加末尾换行后计算 SHA-256。
- 更新 `docs/superpowers/specs/2026-08-04-helper-config-fingerprint-alignment-design.md`
  - 将确认状态记录为已确认；不改变批准范围。
- 生成 `artifacts/helper-config-fingerprint-fix/helper.exe`
  - 新目录中的 Windows x64 交付物，不部署。

---

### Task 1：用失败测试固定 canonical JSON 指纹合同

**文件：**

- 修改：`cmd/helper/main_test.go`
- 参考：`internal/nodetray/config/store.go:564`
- 参考：`cmd/helper/main.go:208`

- [ ] **步骤 1：为测试引入 JSON 编码依赖**

在 `cmd/helper/main_test.go` 的 import 中加入：

```go
"encoding/json"
```

- [ ] **步骤 2：新增仅用于测试的 NodeTray canonical 摘要辅助函数**

在测试文件中加入以下辅助函数。它直接复现 NodeTray Store 的公开字节合同，不能调用待测的 `effectiveHelperConfigSHA256`：

```go
func nodeTrayCanonicalHelperConfigSHA256(t *testing.T, cfg helper.Config) string {
	t.Helper()
	canonical, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal NodeTray canonical Helper config: %v", err)
	}
	canonical = append(canonical, '\n')
	return fmt.Sprintf("%x", sha256.Sum256(canonical))
}
```

- [ ] **步骤 3：新增有代表性配置的合同测试**

将测试放在 `TestConfigPathFromArgs...` 之前，覆盖字符串、切片、布尔和数值字段，确保 compact JSON 与 canonical JSON 的差异确实可见：

```go
func TestEffectiveHelperConfigSHA256MatchesNodeTrayCanonicalJSON(t *testing.T) {
	cfg := helper.Config{
		PipeName:             `\\.\pipe\dedup-delete`,
		AllowedRoots:         []string{`D:\media`, `E:\archive`},
		DeniedRoots:          []string{`D:\media\private`},
		DefaultMode:          "soft",
		AllowHardDelete:      false,
		RecycleDirName:       "$DedupRecycle",
		MaxEntriesPerFrame:   2000,
		FrameReadTimeoutSec:  120,
		FrameWriteTimeoutSec: 60,
		LogDir:               `C:\ProgramData\MySingerServer\Helper\logs`,
	}

	want := nodeTrayCanonicalHelperConfigSHA256(t, cfg)
	got, err := effectiveHelperConfigSHA256(cfg)
	if err != nil {
		t.Fatalf("effectiveHelperConfigSHA256() error = %v", err)
	}
	if got != want {
		t.Fatalf("effectiveHelperConfigSHA256() = %q, want NodeTray canonical digest %q", got, want)
	}
}
```

- [ ] **步骤 4：更新控制身份测试的期望摘要**

将 `TestRunWithControlShutdownCancelsDeleteServerAndPublishesIdentity` 中旧的 compact JSON 固定字符串：

```go
wantJSON := `{"pipe_name":"","allowed_roots":null,"denied_roots":null,"default_mode":"","allow_hard_delete":false,"recycle_dir_name":"","max_entries_per_frame":0,"frame_read_timeout_sec":0,"frame_write_timeout_sec":0,"log_dir":"logs"}`
wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(wantJSON)))
```

替换为：

```go
wantDigest := nodeTrayCanonicalHelperConfigSHA256(t, helper.Config{LogDir: "logs"})
```

- [ ] **步骤 5：格式化测试文件**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w 'cmd\helper\main_test.go'
```

期望：命令退出码为 `0`，且只产生 Go 标准格式化变化。

- [ ] **步骤 6：运行新合同测试并确认 RED**

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = Join-Path $env:TEMP 'helper-fingerprint-fix-gocache'
$env:GOTMPDIR = Join-Path $env:TEMP 'helper-fingerprint-fix-gotmp'
New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR | Out-Null
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./cmd/helper -run '^TestEffectiveHelperConfigSHA256MatchesNodeTrayCanonicalJSON$'
```

期望：测试因实际 compact JSON 摘要不等于“缩进 JSON + 换行”摘要而失败。若测试意外通过，先检查测试是否错误地复用了生产函数；不能直接进入实现步骤。

- [ ] **步骤 7：记录 RED 证据**

记录测试名、退出码和摘要不匹配错误。Git 状态记录为 `N/A_NO_GIT_METADATA`，不提交。

---

### Task 2：实施最小生产修复并使目标测试通过

**文件：**

- 修改：`cmd/helper/main.go:208`
- 测试：`cmd/helper/main_test.go`

- [ ] **步骤 1：只修改 Helper 摘要输入字节**

将：

```go
func effectiveHelperConfigSHA256(cfg helper.Config) (string, error) {
	canonical, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
```

替换为：

```go
func effectiveHelperConfigSHA256(cfg helper.Config) (string, error) {
	canonical, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	canonical = append(canonical, '\n')
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
```

不改函数签名、错误传播、十六进制格式或调用位置。

- [ ] **步骤 2：格式化生产文件与测试文件**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w 'cmd\helper\main.go' 'cmd\helper\main_test.go'
```

- [ ] **步骤 3：运行两个直接相关测试并确认 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./cmd/helper -run '^(TestEffectiveHelperConfigSHA256MatchesNodeTrayCanonicalJSON|TestRunWithControlShutdownCancelsDeleteServerAndPublishesIdentity)$'
```

期望：两个测试均通过；前者固定字节合同，后者确认控制状态发布同一摘要。

- [ ] **步骤 4：检查修改范围**

```powershell
rg -n -C 12 'func effectiveHelperConfigSHA256|TestEffectiveHelperConfigSHA256MatchesNodeTrayCanonicalJSON|TestRunWithControlShutdownCancelsDeleteServerAndPublishesIdentity' 'cmd\helper\main.go' 'cmd\helper\main_test.go'
```

只读检查这两个文件，确认没有握手兼容分支、Agent 改动或进程生命周期改动。版本记录为 `N/A_NO_GIT_METADATA`。

---

### Task 3：运行相关静态回归门禁

**文件：**

- 验证：`cmd/helper/...`
- 验证：`internal/nodetray/...`
- 验证：`nodetray/...`

- [ ] **步骤 1：固定本次验证工具与临时目录**

```powershell
$go = 'C:\tmp\go1.26.5\go\bin\go.exe'
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = Join-Path $env:TEMP 'helper-fingerprint-fix-gocache'
$env:GOTMPDIR = Join-Path $env:TEMP 'helper-fingerprint-fix-gotmp'
$env:M5_CC = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\gcc.exe'
$env:M5_WINDRES = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\windres.exe'
New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR | Out-Null
```

- [ ] **步骤 2：运行设计批准的相关测试集**

```powershell
& $go test -count=1 ./cmd/helper ./internal/nodetray/... ./nodetray
```

期望：退出码 `0`。如遇工具链、ACL 或依赖环境问题，记录原始失败并标记为环境阻塞；不可写成 PASS。

- [ ] **步骤 3：复核生产路径只有一种指纹算法**

```powershell
rg -n -C 4 'effectiveHelperConfigSHA256|MarshalIndent|json\.Marshal\(' 'cmd\helper\main.go' 'cmd\agent\main.go' 'internal\nodetray\config\store.go'
```

期望：Helper、Agent、NodeTray Store 均呈现 `MarshalIndent(..., "", "  ")` 加末尾换行；Helper 不保留 compact JSON 回退。

---

### Task 4：构建独立 Windows x64 Helper 产物

**文件：**

- 读取：`cmd/helper/helper.rc`
- 读取：`cmd/helper/helper.manifest`
- 临时生成后清理：`cmd/helper/rsrc_windows_amd64.syso`
- 生成：`artifacts/helper-config-fingerprint-fix/helper.exe`

- [ ] **步骤 1：验证输入与输出边界**

```powershell
$repo = (Resolve-Path '.').Path
$helperDir = (Resolve-Path 'cmd\helper').Path
$helperResource = Join-Path $helperDir 'rsrc_windows_amd64.syso'
$artifactDir = Join-Path $repo 'artifacts\helper-config-fingerprint-fix'
$helperExe = Join-Path $artifactDir 'helper.exe'
$go = 'C:\tmp\go1.26.5\go\bin\go.exe'
$windres = 'C:\Users\Administrator\AppData\Local\Temp\winlibs-gcc\mingw64\bin\windres.exe'
if (Test-Path -LiteralPath $helperResource) { throw "refusing to overwrite existing resource: $helperResource" }
if (Test-Path -LiteralPath $artifactDir) { throw "refusing to overwrite existing artifact directory: $artifactDir" }
New-Item -ItemType Directory -Path $artifactDir | Out-Null
```

输出目录必须是上述精确新目录。存在时停止并人工核对，不静默覆盖。

- [ ] **步骤 2：生成管理员清单资源并构建 Helper**

```powershell
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '0'
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = Join-Path $env:TEMP 'helper-fingerprint-fix-gocache'
$env:GOTMPDIR = Join-Path $env:TEMP 'helper-fingerprint-fix-gotmp'
try {
    Push-Location -LiteralPath $helperDir
    try {
        & $windres -i 'helper.rc' -O coff -o $helperResource
        if ($LASTEXITCODE -ne 0) { throw 'helper manifest resource generation failed' }
    }
    finally {
        Pop-Location
    }
    & $go -C $repo build -trimpath -o $helperExe ./cmd/helper
    if ($LASTEXITCODE -ne 0) { throw 'helper build failed' }
}
finally {
    if (Test-Path -LiteralPath $helperResource) {
        Remove-Item -LiteralPath $helperResource -Force
    }
}
```

期望：只生成新目录中的 `helper.exe`；无论构建成功或失败，都清理本任务生成的精确 `.syso` 文件。

- [ ] **步骤 3：验证 PE 架构为 x64**

```powershell
$stream = [System.IO.File]::OpenRead($helperExe)
$reader = [System.IO.BinaryReader]::new($stream)
try {
    $stream.Position = 0x3c
    $peOffset = $reader.ReadInt32()
    $stream.Position = $peOffset
    if ($reader.ReadUInt32() -ne 0x00004550) { throw 'invalid PE signature' }
    $machine = $reader.ReadUInt16()
    if ($machine -ne 0x8664) { throw ('unexpected PE machine: 0x{0:X4}' -f $machine) }
}
finally {
    $reader.Dispose()
    $stream.Dispose()
}
```

- [ ] **步骤 4：验证嵌入的 UAC 清单**

```powershell
$mt = 'C:\Program Files (x86)\Windows Kits\10\bin\10.0.26100.0\x64\mt.exe'
$manifestOut = Join-Path $env:TEMP 'helper-config-fingerprint-fix.manifest'
try {
    & $mt '-nologo' "-inputresource:$helperExe;#1" "-out:$manifestOut"
    if ($LASTEXITCODE -ne 0) { throw 'extract helper manifest failed' }
    if (-not (Select-String -LiteralPath $manifestOut -Pattern 'requestedExecutionLevel\s+level="requireAdministrator"' -Quiet)) {
        throw 'requireAdministrator manifest not found'
    }
}
finally {
    if (Test-Path -LiteralPath $manifestOut) { Remove-Item -LiteralPath $manifestOut -Force }
}
```

- [ ] **步骤 5：记录交付物证据**

```powershell
Get-Item -LiteralPath $helperExe | Select-Object FullName, Length, LastWriteTime
Get-FileHash -Algorithm SHA256 -LiteralPath $helperExe
Get-AuthenticodeSignature -LiteralPath $helperExe | Select-Object Status, StatusMessage
```

记录绝对路径、大小、修改时间、SHA-256 和签名状态。未签名应按实际结果报告，不把它描述为已签名。

---

### Task 5：最终核验与交付边界说明

**文件：**

- 复核：`cmd/helper/main.go`
- 复核：`cmd/helper/main_test.go`
- 复核：`artifacts/helper-config-fingerprint-fix/helper.exe`

- [ ] **步骤 1：重新运行最终新鲜验证**

在任何“已修复”表述之前，重新执行：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./cmd/helper ./internal/nodetray/... ./nodetray
Get-FileHash -Algorithm SHA256 -LiteralPath 'artifacts\helper-config-fingerprint-fix\helper.exe'
```

只依据这次命令的实际输出报告结果。

- [ ] **步骤 2：检查未越界部署**

```powershell
Get-Item -LiteralPath 'C:\Program Files\MySingerServer\helper.exe' | Select-Object FullName, Length, LastWriteTime
Get-Process -Name 'nodetray','agent','worker','helper' -ErrorAction SilentlyContinue | Select-Object ProcessName, Id, StartTime
```

这两项只读检查只记录交付时的安装文件与进程状态，用于复核部署边界；不要将现有进程状态解释为本次动态验收。

- [ ] **步骤 3：按门禁状态交付**

最终报告必须分别列出：

- `PASS`：新合同测试、控制身份测试、相关静态测试、x64/manifest/哈希验证中实际通过的项目。
- `FAIL` 或 `BLOCKED`：任何未通过或环境阻塞的静态门禁。
- `BLOCKED_NOT_RUN_DYNAMIC`：未替换已安装 Helper，未从界面点击启动，因此尚不能声称本机运行时问题已动态解决。
- 版本状态：`N/A_NO_GIT_METADATA`。

只有后续获得用户对真实安装目录替换及启停验证的明确授权后，才另行执行动态验收。
