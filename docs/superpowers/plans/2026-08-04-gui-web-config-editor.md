# GUI Web 完整配置编辑与多 Agent 管理实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 在现有 GUI Web 工作台中增加“GUI 设置”页面，以结构化表单编辑完整 `gui.json`，支持维护多台 Agent，并将有效配置原子写回启动时 `-config` 指定的文件；新配置仅在用户手动重启 GUI 后生效。

**架构：** `internal/config.GUIConfig` 继续作为唯一配置模型，启动加载和 Web 保存共享 `ValidateGUI`。`cmd/gui` 将绝对配置路径与启动配置注入 `internal/gui.GUIConfigService`；服务负责磁盘读取、规范 JSON、同目录临时文件和 Windows 原子替换。HTTP 层提供 `GET/PUT /api/config`，React 页面通过现有 `AppApi` 读取和保存，不修改当前运行中的 HTTP Server、PostgreSQL Pool 或 Agent Pool。

**技术栈：** Go 1.26.5、Go `testing`、`net/http`、`pgxpool.ParseConfig`、Windows `MoveFileEx`、React 19、TypeScript 5.9、React Router、Vitest、Testing Library、Vite

## Global Constraints

- 这是个人项目：只保留每项功能的必要 TDD 和一次最终集中验证，不设置逐任务独立审查、双重审查、安全审批或发布门禁。
- 不增加登录、鉴权、TLS、配置版本历史、备份管理、多用户编辑锁或自动重启。
- `PUT /api/config` 只写磁盘；当前监听地址、数据库连接、Agent Pool 和分析参数在进程重启前保持不变。
- 页面必须明确提示“配置已保存，请手动重启 GUI 后生效”，不得把待重启的 Agent 显示为已连接。
- `pg_dsn` 成功读取时返回完整值，页面只做密码框视觉隐藏；错误响应和日志不得包含 DSN。
- 正式配置使用 UTF-8 无 BOM、缩进 JSON 和结尾换行；发布前必须从临时文件重新加载验证。
- 当前 checkout 没有 Git 元数据，不初始化 Git、不创建提交；版本状态统一记录为 `N/A_NO_GIT_METADATA`。
- 不启动或重启真实 GUI/Agent，不连接真实多机环境；真实多机验收记录为 `NOT_RUN_MANUAL`。
- Go 测试使用 `-count=1`。最终验证只覆盖本功能直接影响的包与 Web 构建，不扩大为 `go test ./...`。

## 文件映射

- 修改 `internal/config/gui.go`
  - 抽出共享 `ValidateGUI`，增加稳定字段错误，并校验监听地址、Agent 地址和 PostgreSQL DSN。
- 修改 `internal/config/config_test.go`
  - 固定启动加载与 Web 保存共享的验证合同和字段路径。
- 新增 `internal/gui/config_service.go`
  - 实现配置读取、规范比较、串行保存和临时文件发布。
- 新增 `internal/gui/config_replace_windows.go`
  - 使用 `MoveFileEx(REPLACE_EXISTING | WRITE_THROUGH)` 原子替换。
- 新增 `internal/gui/config_replace_other.go`
  - 为非 Windows 构建提供 `os.Rename` 实现，保持包可编译。
- 新增 `internal/gui/config_service_test.go`
  - 覆盖非默认路径、编码、无变化保存、失败保留原文件和并发完整性。
- 新增 `internal/gui/config_http.go`
  - 实现配置 HTTP 合同与安全的稳定错误响应。
- 新增 `internal/gui/config_http_test.go`
  - 覆盖 GET、PUT、严格 JSON、字段错误和 I/O 错误。
- 修改 `internal/gui/httpapi.go`
  - 注入配置服务并注册 `GET/PUT /api/config`。
- 修改 `cmd/gui/main.go`
  - 解析绝对 `-config` 路径、创建配置服务并注入 API。
- 修改 `cmd/gui/main_test.go`
  - 固定非默认配置路径的加载与注入前置合同。
- 修改 `webui/src/api/contracts.ts`
  - 增加完整 GUI 配置、保存结果和字段错误类型。
- 修改 `webui/src/api/appApi.ts`
  - 增加 GET/PUT 适配器、snake_case 转换和配置字段错误类。
- 修改 `webui/src/api/appApi.test.ts`
  - 固定完整配置的双向转换与 400 字段错误。
- 新增 `webui/src/features/settings/GUISettingsPage.tsx`
  - 实现完整结构化配置表单与页面状态。
- 新增 `webui/src/features/settings/GUISettingsPage.css`
  - 实现桌面和窄屏布局。
- 新增 `webui/src/features/settings/GUISettingsPage.test.tsx`
  - 覆盖加载、编辑、Agent 管理、错误和保存提示。
- 修改 `webui/src/app/App.tsx`
  - 注册 `/settings` 路由并注入同一 `AppApi`。
- 修改 `webui/src/app/navigation.ts`
  - 增加“GUI 设置”导航项。
- 修改 `webui/src/app/App.test.tsx`
  - 固定第七个工作区入口和路由。
- 更新 `internal/gui/webui_dist/`
  - 由 `scripts/build-web.ps1 -VerifyEmbedded` 生成并校验新的内嵌 Web 资源。

---

### Task 1: 抽出并固定共享 GUI 配置验证

**Files:**

- Modify: `internal/config/gui.go:1-158`
- Modify: `internal/config/config_test.go:393-560`

**Interfaces:**

```go
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type GUIValidationError struct {
	Fields []FieldError
}

func (e *GUIValidationError) Error() string
func ValidateGUI(cfg *GUIConfig) error
```

- `LoadGUI(path)` 保留“先填默认值、再反序列化”的行为，完成反序列化后只调用 `ValidateGUI(cfg)`。
- `ValidateGUI` 收集全部字段错误，字段顺序与配置表单顺序一致，便于稳定测试和前端定位。
- 地址校验使用 `net.SplitHostPort`，要求非空 host、十进制端口且端口在 `1..65535`；IPv6 必须使用方括号格式。
- DSN 校验使用 `pgxpool.ParseConfig`，只解析、不建立数据库连接。
- `firstscreen` 与 `phase2` 的现有边界不改变，只把错误转换为稳定字段路径。

- [ ] **Step 1: 先写共享验证 RED 测试**

在 `internal/config/config_test.go` 增加：

- `TestValidateGUIAcceptsDefaultedLoadableConfig`
- `TestValidateGUIRejectsNetworkAndDSNFieldsWithStablePaths`
- `TestValidateGUIRejectsDuplicateAgentsWithIndexedPath`
- `TestLoadGUIAndValidateGUIShareAnalysisBoundaries`

表驱动用例至少固定以下映射：

```text
listen_addr="127.0.0.1"            -> listen_addr / invalid_address
listen_addr="127.0.0.1:70000"      -> listen_addr / invalid_address
pg_dsn="not a postgres dsn"        -> pg_dsn / invalid_dsn
agents[0].addr="192.168.1.2"       -> agents[0].addr / invalid_address
agents[1].machine_id="agent-a"     -> agents[1].machine_id / duplicate
phase2.video_frames=5               -> phase2.video_frames / fixed_value
```

测试通过 `errors.As(err, &validationError)` 读取 `*GUIValidationError`，不匹配英文 `Error()` 文本。

- [ ] **Step 2: 运行配置测试并确认 RED**

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = 'D:\code\mySingerServer\.superpowers\tmp\gui-config-editor-gocache'
$env:GOTMPDIR = 'D:\code\mySingerServer\.superpowers\tmp\gui-config-editor-gotmp'
New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR | Out-Null
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/config -run '^(TestValidateGUI|TestLoadGUIAndValidateGUI)'
```

Expected: FAIL，因为 `ValidateGUI`、`FieldError` 和 `GUIValidationError` 尚不存在。

- [ ] **Step 3: 实现稳定字段验证**

在 `internal/config/gui.go` 中：

1. 增加 `FieldError`、`GUIValidationError` 和 `Error()`。
2. 增加内部收集器，按基本设置、PostgreSQL、Agent、FirstScreen、Phase2 的顺序追加错误。
3. 将 `FirstScreenConfig.validate()` 与 `Phase2Config.validate()` 改为接收收集器或返回字段错误，确保每个字段有独立路径。
4. 实现 `ValidateGUI`，nil 配置返回 `config / required`。
5. 将 `LoadGUI` 中现有内联校验替换为 `ValidateGUI(cfg)`。
6. 保持默认值与所有合法零值合同不变。

字段错误消息使用简短中文，例如 `地址必须是 host:port`、`机器标识不能重复`、`必须是可解析的 PostgreSQL DSN`。

- [ ] **Step 4: 格式化并运行配置包测试至 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w 'internal\config\gui.go' 'internal\config\config_test.go'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/config
```

Expected: PASS；既有默认值、合法零值、FirstScreen 和 Phase2 边界测试仍通过。

---

### Task 2: 实现配置磁盘服务与 Windows 原子保存

**Files:**

- Create: `internal/gui/config_service.go`
- Create: `internal/gui/config_replace_windows.go`
- Create: `internal/gui/config_replace_other.go`
- Create: `internal/gui/config_service_test.go`

**Interfaces:**

```go
type GUIConfigSnapshot struct {
	Config          *config.GUIConfig `json:"config"`
	RestartRequired bool              `json:"restart_required"`
}

type GUIConfigSaveResult struct {
	Saved           bool `json:"saved"`
	RestartRequired bool `json:"restart_required"`
}

type GUIConfigService struct {
	mu               sync.Mutex
	path             string
	runtimeCanonical []byte
	replace          func(source, destination string) error
}

func NewGUIConfigService(path string, runtime *config.GUIConfig) (*GUIConfigService, error)
func (s *GUIConfigService) Load() (GUIConfigSnapshot, error)
func (s *GUIConfigService) Save(ctx context.Context, cfg *config.GUIConfig) (GUIConfigSaveResult, error)
```

- 构造函数把 `path` 解析为绝对路径，并保存启动配置的规范 JSON；不持有可变的配置指针。
- 规范 JSON 使用 `json.MarshalIndent(value, "", "  ")` 加一个 `\n`，直接得到 UTF-8 无 BOM 字节。
- `Load` 每次从磁盘调用 `config.LoadGUI`，再与 `runtimeCanonical` 比较。
- `Save` 在互斥锁内执行：验证输入、规范编码、比较当前磁盘语义、同目录临时文件写入/同步/关闭、从临时文件 `LoadGUI`、检查请求上下文、原子替换、清理临时文件。
- 当前磁盘配置语义相同时返回 `saved=false`；磁盘无效时允许用本次有效配置修复。
- Windows 文件替换函数名固定为 `replaceFileAtomically`，直接使用 `windows.MoveFileEx` 的 `MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH`。

- [ ] **Step 1: 先写配置服务 RED 测试**

在 `internal/gui/config_service_test.go` 增加：

- `TestGUIConfigServiceUsesNonDefaultAbsolutePath`
- `TestGUIConfigServiceWritesCanonicalUTF8WithoutBOM`
- `TestGUIConfigServiceReportsSavedAndRestartRequired`
- `TestGUIConfigServiceSkipsSemanticallyIdenticalSave`
- `TestGUIConfigServiceReplaceFailurePreservesOriginal`
- `TestGUIConfigServiceConcurrentSavesRemainCompleteJSON`
- `TestGUIConfigServiceRemovesTemporaryFiles`

失败注入直接在同包测试中替换 `service.replace`，返回固定错误；断言原配置字节完全不变，且目录中不存在 `.<base>.*.tmp`。

- [ ] **Step 2: 运行服务测试并确认 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/gui -run '^TestGUIConfigService'
```

Expected: FAIL，因为服务及平台替换函数尚不存在。

- [ ] **Step 3: 实现配置服务**

实现顺序：

1. `canonicalGUIConfig` 先调用 `config.ValidateGUI`，再输出规范 JSON。
2. `NewGUIConfigService` 调用 `filepath.Abs`，校验启动配置并设置生产 `replaceFileAtomically`。
3. `Load` 返回磁盘配置和规范比较结果。
4. `Save` 使用 `os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")`，显式 `Chmod(0600)`、`Write`、`Sync`、`Close`。
5. 临时文件关闭后调用 `config.LoadGUI(tempPath)`；只有通过验证且 `ctx.Err()==nil` 才替换目标。
6. 所有失败路径通过 `defer os.Remove(tempPath)` 清理，错误仅包含操作名和目标基名，不拼入配置内容。
7. Windows 和非 Windows 文件分别使用 build tag，避免重复定义。

- [ ] **Step 4: 格式化并运行服务测试至 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w `
  'internal\gui\config_service.go' `
  'internal\gui\config_replace_windows.go' `
  'internal\gui\config_replace_other.go' `
  'internal\gui\config_service_test.go'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/gui -run '^TestGUIConfigService'
```

Expected: PASS；并发测试循环读取最终文件时始终得到完整合法配置。

---

### Task 3: 暴露 GET/PUT 配置 API 并接入实际 `-config` 路径

**Files:**

- Create: `internal/gui/config_http.go`
- Create: `internal/gui/config_http_test.go`
- Modify: `internal/gui/httpapi.go:18-70`
- Modify: `cmd/gui/main.go:539-670`
- Modify: `cmd/gui/main_test.go`

**Interfaces:**

```go
type guiConfigStore interface {
	Load() (GUIConfigSnapshot, error)
	Save(context.Context, *config.GUIConfig) (GUIConfigSaveResult, error)
}

func (api *API) SetConfigService(service guiConfigStore)
func (api *API) handleConfigGet(http.ResponseWriter, *http.Request)
func (api *API) handleConfigPut(http.ResponseWriter, *http.Request)

func loadGUIRuntime(path string) (string, *config.GUIConfig, error)
```

HTTP 合同：

```json
GET /api/config
{
  "config": {
    "listen_addr": "127.0.0.1:18080",
    "pg_dsn": "postgres://dedup@127.0.0.1:5432/dedup",
    "agents": [{"machine_id": "agent-a", "addr": "192.168.1.10:9101"}],
    "heartbeat_s": 15,
    "firstscreen": {
      "hamming_max": 31,
      "aspect_tolerance": 0.1,
      "video_duration_window_ms": 2000,
      "image_quality_min": 50,
      "read_page_size": 50000,
      "group_insert_batch": 1000,
      "sha_resolve_chunk": 10000
    },
    "phase2": {
      "phash_pass_t2": 0.8,
      "phash_part_threshold": 10,
      "sobel_t3": 0.85,
      "video_frames": 6,
      "video_avg_t4": 0.8,
      "video_min_passed": 4,
      "video_min_valid": 4,
      "video_file_timeout_s": 120,
      "video_frame_command_timeout_s": 20,
      "image_file_timeout_s": 30,
      "task_shard_size": 5000,
      "auto_dispatch": true
    }
  },
  "restart_required": false
}
```

`PUT /api/config` 请求体直接是完整 `GUIConfig` 对象，成功响应为：

```json
{"saved":true,"restart_required":true}
```

字段错误固定为：

```json
{
  "error": "config_invalid",
  "fields": [
    {"field": "agents[1].machine_id", "code": "duplicate", "message": "机器标识不能重复"}
  ]
}
```

- [ ] **Step 1: 先写 HTTP 与路径接线 RED 测试**

在 `internal/gui/config_http_test.go` 增加：

- `TestGUIConfigHTTPGetReturnsDiskSnapshot`
- `TestGUIConfigHTTPPutSavesCompleteConfig`
- `TestGUIConfigHTTPPutReturnsFieldErrors`
- `TestGUIConfigHTTPPutRejectsUnknownFieldsAndTrailingJSON`
- `TestGUIConfigHTTPReturnsStableReadAndWriteErrorsWithoutDSN`
- `TestGUIConfigHTTPUnavailableWithoutInjectedService`

使用内存 fake `guiConfigStore`，记录传入配置并返回固定结果。错误响应只断言稳定 `error` 码和字段数组，同时断言响应体不包含测试 DSN 密码。

在 `cmd/gui/main_test.go` 增加 `TestLoadGUIRuntimeReturnsAbsoluteNonDefaultPath`，用 `t.TempDir()` 下的 `custom-gui.json` 证明加载路径不是默认 `gui.json`。

- [ ] **Step 2: 运行 HTTP 与路径测试并确认 RED**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/gui ./cmd/gui -run '^(TestGUIConfigHTTP|TestLoadGUIRuntime)'
```

Expected: FAIL，因为配置路由、setter 和 `loadGUIRuntime` 尚不存在。

- [ ] **Step 3: 实现严格 HTTP 解码和稳定错误**

1. 在 `API` 增加 `config guiConfigStore` 字段和 `SetConfigService`。
2. 在 `Routes()` 注册：

```go
legacy.HandleFunc("GET /api/config", api.handleConfigGet)
legacy.HandleFunc("PUT /api/config", api.handleConfigPut)
```

3. PUT 使用 `json.Decoder.DisallowUnknownFields()`，第一次解码完整对象，第二次解码必须得到 `io.EOF`。
4. `*config.GUIValidationError` 返回 400 `config_invalid`；非法 JSON 返回 400 `invalid_request`。
5. 未注入服务返回 503 `config_unavailable`；读取失败返回 500 `config_read_failed`；保存失败返回 500 `config_save_failed`。
6. 500 响应不返回底层错误文本；本功能不记录请求体、配置对象或 DSN。

- [ ] **Step 4: 将服务接到启动时实际配置路径**

在 `cmd/gui/main.go` 实现：

```go
func loadGUIRuntime(path string) (string, *config.GUIConfig, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := config.LoadGUI(absolute)
	if err != nil {
		return "", nil, err
	}
	return absolute, cfg, nil
}
```

`run` 使用返回的 `absoluteConfigPath` 创建服务：

```go
configService, err := gui.NewGUIConfigService(absoluteConfigPath, cfg)
if err != nil {
	return fmt.Errorf("initialize GUI config service: %w", err)
}
```

创建 API 后调用：

```go
api.SetConfigService(configService)
```

服务创建失败必须发生在 HTTP Server 启动前；现有 PostgreSQL 和 Agent Pool 构造仍使用 `cfg` 启动快照。

- [ ] **Step 5: 格式化并运行后端相关包至 GREEN**

```powershell
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w `
  'internal\gui\config_http.go' `
  'internal\gui\config_http_test.go' `
  'internal\gui\httpapi.go' `
  'cmd\gui\main.go' `
  'cmd\gui\main_test.go'
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/config ./internal/gui ./cmd/gui
```

Expected: PASS；现有 GUI API、任务、分析和删除测试不回归。

---

### Task 4: 扩展前端 AppApi 的完整配置合同

**Files:**

- Modify: `webui/src/api/contracts.ts:1-185`
- Modify: `webui/src/api/appApi.ts:1-350`
- Modify: `webui/src/api/appApi.test.ts`

**Interfaces:**

```ts
export interface GUIAgentConfig {
  machineId: string;
  addr: string;
}

export interface GUIFirstScreenConfig {
  hammingMax: number;
  aspectTolerance: number;
  videoDurationWindowMs: number;
  imageQualityMin: number;
  readPageSize: number;
  groupInsertBatch: number;
  shaResolveChunk: number;
}

export interface GUIPhase2Config {
  phashPassT2: number;
  phashPartThreshold: number;
  sobelT3: number;
  videoFrames: number;
  videoAvgT4: number;
  videoMinPassed: number;
  videoMinValid: number;
  videoFileTimeoutS: number;
  videoFrameCommandTimeoutS: number;
  imageFileTimeoutS: number;
  taskShardSize: number;
  autoDispatch: boolean;
}

export interface GUIConfig {
  listenAddr: string;
  pgDsn: string;
  agents: GUIAgentConfig[];
  heartbeatS: number;
  firstScreen: GUIFirstScreenConfig;
  phase2: GUIPhase2Config;
}

export interface GUIConfigSnapshot {
  config: GUIConfig;
  restartRequired: boolean;
}

export interface GUIConfigSaveResult {
  saved: boolean;
  restartRequired: boolean;
}

export interface ConfigFieldError {
  field: string;
  code: string;
  message: string;
}

export interface AppApi {
  loadGUIConfig(signal?: AbortSignal): Promise<GUIConfigSnapshot>;
  saveGUIConfig(config: GUIConfig, signal?: AbortSignal): Promise<GUIConfigSaveResult>;
}
```

`AppApi` 保留全部现有方法，上述两个方法追加在接口末尾。

在 `appApi.ts` 导出：

```ts
export class GUIConfigValidationError extends ApiError {
  readonly fields: readonly ConfigFieldError[];
}
```

- [ ] **Step 1: 先写配置 API 适配 RED 测试**

在 `webui/src/api/appApi.test.ts` 增加：

- `loads and decodes the complete GUI configuration`
- `encodes the complete GUI configuration with snake_case fields`
- `preserves structured GUI configuration field errors`
- `rejects malformed GUI configuration responses`

PUT 测试必须断言 `method: "PUT"`、`Content-Type: application/json` 和完整请求体；400 测试断言异常为 `GUIConfigValidationError` 且字段路径保持 `agents[1].machine_id`。

- [ ] **Step 2: 运行 AppApi 测试并确认 RED**

```powershell
Set-Location 'D:\code\mySingerServer\webui'
npm.cmd test -- src/api/appApi.test.ts
```

Expected: FAIL，因为配置类型和 API 方法尚不存在。

- [ ] **Step 3: 实现双向类型转换**

1. 在 `contracts.ts` 增加完整类型和 `AppApi` 方法。
2. 在 `appApi.ts` 增加 `guiConfigSnapshot(value)`、`guiConfig(value)`、`guiConfigInput(value)`、`configFieldErrors(value)` 解码/编码函数。
3. `loadGUIConfig` 使用 GET `/api/config`。
4. `saveGUIConfig` 使用 PUT `/api/config`，并设置 `decodeStatuses: [400]`。
5. 当 400 响应为 `config_invalid` 时抛出 `GUIConfigValidationError`；其他 400 继续转换为普通 `ApiError`。
6. 不修改 `client.ts` 的通用响应正文暴露策略；若 TypeScript 需要访问状态，复用现有 `ApiError` 即可。

- [ ] **Step 4: 运行 API 测试、类型检查至 GREEN**

```powershell
npm.cmd test -- src/api/appApi.test.ts
npm.cmd run build
Set-Location 'D:\code\mySingerServer'
```

Expected: PASS；snake_case 与 camelCase 完整往返，既有 AppApi 方法行为不变。

---

### Task 5: 实现“GUI 设置”结构化表单与导航

**Files:**

- Create: `webui/src/features/settings/GUISettingsPage.tsx`
- Create: `webui/src/features/settings/GUISettingsPage.css`
- Create: `webui/src/features/settings/GUISettingsPage.test.tsx`
- Modify: `webui/src/app/App.tsx:1-48`
- Modify: `webui/src/app/navigation.ts:1-15`
- Modify: `webui/src/app/App.test.tsx:1-210`

**Component interface:**

```ts
export interface GUISettingsPageProps {
  readonly api?: AppApi;
}

export function GUISettingsPage({ api = appApi }: GUISettingsPageProps)
```

页面结构固定为：

1. 标题“GUI 设置”和状态文本。
2. “基本设置”：`listen_addr`、`heartbeat_s`。
3. “PostgreSQL”：`pg_dsn` 密码框与“显示/隐藏”按钮。
4. “Agent”：每行 `machine_id`、`addr`、“上移”、“下移”、“删除”，以及“添加 Agent”。
5. “一筛参数”：完整七个 `firstscreen` 字段。
6. “二筛参数”：完整十二个 `phase2` 字段与 `auto_dispatch` 复选框。
7. 底部“重新加载”和“保存配置”。

页面状态：

```ts
type LoadState = "loading" | "ready" | "error";

const [config, setConfig] = useState<GUIConfig>();
const [baseline, setBaseline] = useState<GUIConfig>();
const [loadState, setLoadState] = useState<LoadState>("loading");
const [saving, setSaving] = useState(false);
const [showDSN, setShowDSN] = useState(false);
const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
const [notice, setNotice] = useState<string>();
```

脏状态使用配置值的深比较，Agent 数组顺序参与比较。重新加载直接再次调用 `loadGUIConfig`，成功后同时替换 `config` 和 `baseline`。保存失败保留 `config`；保存成功将当前值设为 `baseline` 并根据 `restartRequired` 显示固定提示。

- [ ] **Step 1: 先扩展路由 RED 测试**

在 `webui/src/app/App.test.tsx`：

1. mock `GUISettingsPage`，断言收到相同 `AppApi`。
2. 将 `[/settings, settings-route]` 加入 `routes`。
3. 将工作区链接断言从六项改为七项并加入“GUI 设置”。

运行：

```powershell
Set-Location 'D:\code\mySingerServer\webui'
npm.cmd test -- src/app/App.test.tsx
```

Expected: FAIL，因为设置页面、导航和路由尚不存在。

- [ ] **Step 2: 先写设置页面 RED 测试**

在 `GUISettingsPage.test.tsx` 增加：

- `loads and renders every GUI configuration section`
- `keeps the DSN hidden until the user chooses to show it`
- `adds edits reorders and removes agents without changing runtime status`
- `keeps edited values and binds indexed field errors after save failure`
- `reloads disk configuration and clears dirty state`
- `shows the manual restart message after a changed save`
- `shows no-restart message when saved configuration matches runtime`

测试使用完整 `GUIConfig` fixture 和 `Partial<AppApi> as AppApi`，不访问真实网络。

```powershell
npm.cmd test -- src/features/settings/GUISettingsPage.test.tsx
```

Expected: FAIL，因为页面尚不存在。

- [ ] **Step 3: 实现页面、布局和可访问字段绑定**

1. 页面挂载时创建 `AbortController`，卸载时取消加载/保存后的状态更新。
2. 每个输入框使用 `name`/`id` 对应后端路径；Agent 行使用 `agents[${index}].machine_id` 和 `agents[${index}].addr` 映射显示错误。
3. 数字输入使用 `type="number"`，更新时以 `Number(event.currentTarget.value)` 写回；`step` 对阈值设为 `0.01`，整数设为 `1`。
4. `video_frames` 保持可见，现有后端固定值校验决定是否可保存；页面旁标注“当前必须为 6”。
5. 删除时至少保留一行；最后一行删除按钮禁用，并显示“至少需要一个 Agent”。
6. 上移/下移通过复制数组后交换元素；首行上移和末行下移按钮禁用。
7. DSN 输入默认 `type="password"`，按钮仅在 `password/text` 间切换，不清空值。
8. 捕获 `GUIConfigValidationError` 后将 `fields` 转为 `Record<string,string>`；其他错误显示页面级可重试提示。
9. 保存期间禁用保存按钮；脏状态显示“有未保存更改”。
10. 复用 `operational-pages.css` 基础样式，在 `GUISettingsPage.css` 只补充分组网格、Agent 行、字段错误和窄屏布局。

- [ ] **Step 4: 注册导航与路由**

在 `navigation.ts` 末尾加入：

```ts
{ label: "GUI 设置", to: "/settings" }
```

在 `App.tsx` 导入并注册：

```tsx
<Route path="/settings" element={<GUISettingsPage api={api} />} />
```

- [ ] **Step 5: 运行页面、路由和完整前端测试至 GREEN**

```powershell
npm.cmd test -- src/features/settings/GUISettingsPage.test.tsx src/app/App.test.tsx
npm.cmd test
npm.cmd run lint
npm.cmd run build
Set-Location 'D:\code\mySingerServer'
```

Expected: 全部 PASS；设置页在窄屏不产生页面级横向溢出，Agent 行在自身区域内正常换行。

---

### Task 6: 更新内嵌 Web 资源并做一次集中验证

**Files:**

- Update generated output: `internal/gui/webui_dist/`
- Verify: all files listed above

本任务只做一次集中验证，不追加逐任务审查或额外安全检查。

- [ ] **Step 1: 统一格式化修改过的 Go 文件**

```powershell
Set-Location 'D:\code\mySingerServer'
& 'C:\tmp\go1.26.5\go\bin\gofmt.exe' -w `
  'internal\config\gui.go' `
  'internal\config\config_test.go' `
  'internal\gui\config_service.go' `
  'internal\gui\config_service_test.go' `
  'internal\gui\config_replace_windows.go' `
  'internal\gui\config_replace_other.go' `
  'internal\gui\config_http.go' `
  'internal\gui\config_http_test.go' `
  'internal\gui\httpapi.go' `
  'cmd\gui\main.go' `
  'cmd\gui\main_test.go'
```

- [ ] **Step 2: 运行后端相关包**

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOCACHE = 'D:\code\mySingerServer\.superpowers\tmp\gui-config-editor-gocache'
$env:GOTMPDIR = 'D:\code\mySingerServer\.superpowers\tmp\gui-config-editor-gotmp'
New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR | Out-Null
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/config ./internal/gui ./cmd/gui
```

Expected: PASS。

- [ ] **Step 3: 运行前端测试、lint 和构建**

```powershell
Set-Location 'D:\code\mySingerServer\webui'
npm.cmd test
npm.cmd run lint
npm.cmd run build
Set-Location 'D:\code\mySingerServer'
```

Expected: 三个命令退出码均为 0。

- [ ] **Step 4: 生成并验证内嵌 Web 资源**

```powershell
& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File `
  'scripts\build-web.ps1' -VerifyEmbedded
```

Expected: 脚本完成前端构建，更新 `internal/gui/webui_dist/`，并通过本地资源引用和嵌入校验。

- [ ] **Step 5: 复跑受嵌入资源影响的 GUI 测试**

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/gui ./cmd/gui
```

Expected: PASS，包含内嵌 React 入口和本地资源检查。

- [ ] **Step 6: 记录交付状态**

最终报告只记录：

- 后端相关包测试结果；
- 前端测试、lint、build 结果；
- `build-web.ps1 -VerifyEmbedded` 结果；
- `N/A_NO_GIT_METADATA`；
- 真实 GUI 重启与多机连接为 `NOT_RUN_MANUAL`。

不把未执行的真实多机连接写成 PASS，也不增加额外审查清单。

## 完成判定

满足以下条件即可交付：

1. `/api/config` 可读取并原子保存启动时实际配置文件。
2. 无效配置不会替换正式文件，字段错误可定位到具体表单控件。
3. “GUI 设置”可编辑完整配置并管理多台 Agent。
4. 保存成功后明确提示手动重启，当前运行时状态不热更新。
5. Task 6 的集中验证全部通过；真实多机连接由用户重启后手工确认。

## 执行方式

建议选择当前任务内直接执行：按 Task 1 到 Task 6 顺序实施，使用必要 TDD，最后只做一次集中验证。若需要改为单独任务执行，可使用 `superpowers:executing-plans` 按同一顺序继续；两种方式都不增加逐任务独立审查。
