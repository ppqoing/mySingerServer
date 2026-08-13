# 便携包默认配置实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** 让 Compute 与 Manager ZIP 解压后直接包含实际默认配置，同时通过 NodeTray 的既有提权边界安全创建 Helper 运行配置。

**Architecture:** 构建阶段维护四份无凭据默认配置，发布脚本将其映射到程序实际读取位置；Worker 参数只保留在 Agent 配置的 worker 段。Helper 的包根 helper.default.json 是普通用户可编辑的默认源，NodeTray 只在受保护运行配置完全不存在时，通过带 CreateOnly 标记的提权写入动作导入它。

**Tech Stack:** Go 1.23+、PowerShell 7、JSON、Windows DACL、Wails/NodeTray、Go testing。

## Global Constraints

- Worker 不新增独立配置协议，所有 Worker 参数继续归属 agent.json 的 worker 段。
- Compute 配置路径固定为 data\agent\agent.json、data\nodetray\tray.json 和包根 helper.default.json。
- Manager 配置路径固定为与 gui.exe 同目录的 gui.json。
- ZIP 不包含 data\helper\helper.json；该文件继续由提权写入器在受保护目录中创建。
- Helper 默认导入是 create-only：正式配置或 .last-good 任一存在时都不得覆盖。
- 默认配置不得包含密码、令牌、机器 ID、数据库文件、日志、缓存或构建机绝对路径。
- PostgreSQL DSN 为空表示尚未配置，不能阻止 GUI HTTP 监听和设置界面启动。
- 避开任务开始前已有的用户文档改动与 publish/ 目录。

---

### Task 1: Manager 使用包内 gui.json 并允许 PostgreSQL 尚未配置

**Files:**
- Create: deploy/gui.default.json
- Modify: internal/config/gui.go
- Modify: internal/config/config_test.go
- Modify: internal/gui/runtime_status.go
- Modify: internal/gui/runtime_status_test.go
- Modify: cmd/gui/operational_runtime.go
- Modify: cmd/gui/operational_runtime_test.go
- Modify: scripts/package-manager-release.ps1
- Modify: scripts/test-package-manager-release.ps1
- Modify: scripts/test-node-tray-supply-chain.ps1
- Modify: deploy/README-管理端部署.md

**Interfaces:**
- Produces: config.DefaultGUI() 返回 PGDSN == "" 的可验证配置。
- Produces: gui.ErrPostgresNotConfigured，供运行时快速进入稳定降级状态。
- Produces: Manager ZIP 中实际文件 gui.json，不再发布 gui.example.json。

- [ ] **Step 1: 写 GUI 空 DSN 的失败测试**

将 TestDefaultGUIIsACompletePortableFirstRunConfiguration 的关键断言改为：

~~~go
if cfg.ListenAddr != "127.0.0.1:18081" || cfg.PGDSN != "" ||
    len(cfg.Agents) != 1 || cfg.Agents[0].Addr != "127.0.0.1:9101" {
    t.Fatalf("incomplete portable defaults: %#v", cfg)
}
~~~

增加 TestValidateGUIAcceptsEmptyPostgresDSNAsUnconfigured；在 runtime_status_test.go 断言空配置错误映射为 postgres_not_configured；在 operational_runtime_test.go 断言空 DSN 在创建 pgxpool 前立即返回该错误。

- [ ] **Step 2: 运行 RED**

~~~powershell
go test -count=1 ./internal/config ./internal/gui ./cmd/gui -run 'DefaultGUI|EmptyPostgres|PostgresNotConfigured'
~~~

Expected: FAIL；默认 DSN 仍非空，空 DSN 仍被校验拒绝，错误常量尚不存在。

- [ ] **Step 3: 最小实现空 DSN 降级语义**

internal/config/gui.go：

~~~go
func DefaultGUI() *GUIConfig {
    cfg := defaultGUIOptionalFields()
    cfg.PGDSN = ""
    cfg.Agents = []AgentEndpoint{{Addr: "127.0.0.1:9101"}}
    return cfg
}

if cfg.PGDSN != "" {
    if _, err := pgxpool.ParseConfig(cfg.PGDSN); err != nil {
        validation.add("pg_dsn", "invalid_dsn", "必须是可解析的 PostgreSQL DSN")
    }
}
~~~

runtime_status.go 增加 var ErrPostgresNotConfigured = errors.New("postgres_not_configured")，分类摘要为 PostgreSQL 尚未配置。newOperationalRuntimeResources 在 strings.TrimSpace(cfg.PGDSN) == "" 时直接返回该错误。

- [ ] **Step 4: 将 Manager 发布合同先改成期望实际配置**

scripts/test-package-manager-release.ps1 的精确文件清单改为：

~~~powershell
@('gui.exe', 'gui.json', 'Start-Manager.ps1',
  'README-管理端部署.md', 'release-manifest.json')
~~~

断言 gui.json 的监听地址为 127.0.0.1:18081、DSN 为空、唯一 Agent 是 127.0.0.1:9101，且 README 不再声称首次生成或复制示例。

- [ ] **Step 5: 运行 Manager 包 RED**

~~~powershell
pwsh -NoProfile -File .\scripts\test-package-manager-release.ps1
~~~

Expected: FAIL；当前 ZIP 仍包含 gui.example.json。

- [ ] **Step 6: 创建默认文件并修改 Manager 打包器**

创建完整的 deploy/gui.default.json，包含 firstscreen 和 phase2 默认段；关键字段：

~~~json
{
  "listen_addr": "127.0.0.1:18081",
  "pg_dsn": "",
  "agents": [{"addr": "127.0.0.1:9101"}],
  "heartbeat_s": 15
}
~~~

将打包器输入改为 GuiConfigPath，默认读取该文件并复制为 gui.json。敏感配置校验允许空 DSN；非空 DSN 仍必须是无凭据的 loopback PostgreSQL 地址。同步供应链合同和中文 README。

- [ ] **Step 7: 运行 GREEN 并提交**

~~~powershell
go test -count=1 ./internal/config ./internal/gui ./cmd/gui
pwsh -NoProfile -File .\scripts\test-package-manager-release.ps1
git add -- deploy/gui.default.json deploy/README-管理端部署.md internal/config/gui.go internal/config/config_test.go internal/gui/runtime_status.go internal/gui/runtime_status_test.go cmd/gui/operational_runtime.go cmd/gui/operational_runtime_test.go scripts/package-manager-release.ps1 scripts/test-package-manager-release.ps1 scripts/test-node-tray-supply-chain.ps1
git commit -m "feat: ship manager default configuration"
~~~

Expected: Go 与 Manager 合同 PASS，只提交列出的文件。

---

### Task 2: Compute ZIP 预置 Agent、Worker 参数和 NodeTray 默认配置

**Files:**
- Create: deploy/agent.default.json
- Create: deploy/nodetray.default.json
- Create: deploy/helper.default.json
- Modify: scripts/build.ps1
- Modify: scripts/package-node-release.ps1
- Modify: scripts/test-package-node-release.ps1
- Modify: scripts/test-package-portable-release.ps1
- Modify: scripts/test-node-tray-supply-chain.ps1
- Modify: deploy/README-节点部署.md

**Interfaces:**
- Produces: data/agent/agent.json、data/nodetray/tray.json、helper.default.json。
- Produces: agent.json.worker 作为 Worker 唯一配置来源。
- Preserves: ZIP 中不存在 data/helper、运行数据库、令牌、日志和机器标识。

- [ ] **Step 1: 先修改 Compute 发布合同**

将示例和两个 .gitkeep 的期望替换为：

~~~powershell
'data/agent/agent.json',
'data/nodetray/tray.json',
'helper.default.json'
~~~

增加真实 JSON 断言：

~~~powershell
Assert-True ([string]$agent.data_dir -ceq './data/agent') 'unsafe Agent data root'
Assert-True ($null -ne $agent.worker -and [int]$agent.worker.image_memory_mb -eq 256) 'Worker defaults missing'
Assert-True ([string]$agent.worker.exe_path -ceq '') 'Worker path must resolve beside agent.exe'
Assert-True (-not [bool]$tray.helperEnabled -and [string]$tray.agentStartMode -ceq 'manual') 'unsafe tray defaults'
Assert-True (@($helper.allowed_roots).Count -eq 0) 'Helper default must not authorize a root'
~~~

test-package-portable-release.ps1 允许嵌套实际配置，但继续禁止包根 agent.json 和任何运行状态。

- [ ] **Step 2: 运行 Compute 包 RED**

~~~powershell
pwsh -NoProfile -File .\scripts\test-package-node-release.ps1
pwsh -NoProfile -File .\scripts\test-package-portable-release.ps1
~~~

Expected: FAIL；当前 ZIP 仍发布示例和空目录占位文件。

- [ ] **Step 3: 创建三份完整安全默认配置**

agent.default.json 完整列出 scan、sync、proto、worker、pipeline、thumb、ipc、delete、tuning；关键值：

~~~json
{
  "listen_addr": "0.0.0.0:9101",
  "data_dir": "./data/agent",
  "pg_dsn": "",
  "use_everything": true,
  "worker": {
    "count": 0,
    "exe_path": "",
    "image_timeout_s": 30,
    "video_timeout_s": 120,
    "image_memory_mb": 256,
    "respawn_delay_ms": 500,
    "crash_injection": false
  }
}
~~~

nodetray.default.json 精确对应 production.DefaultTraySettings()。helper.default.json 使用空 allowed_roots、禁用硬删除、空 log_dir，不能授权实际目录。

- [ ] **Step 4: 修改构建与 Compute 打包映射**

scripts/build.ps1 将四份 .default.json 复制到 stage 并列为必需文件。package-node-release.ps1 映射：

~~~text
agent.default.json    -> data\agent\agent.json
nodetray.default.json -> data\nodetray\tray.json
helper.default.json   -> helper.default.json
~~~

删除 .gitkeep 创建逻辑，不把 Agent/Helper 示例放入 ZIP，保留清单、SHA-256、解压复核和敏感配置拒绝测试。

- [ ] **Step 5: 更新中文说明、运行 GREEN 并提交**

README 明确 Worker 参数位于 data\agent\agent.json 的 worker 段；Helper 只修改包根默认源，NodeTray 自动安全导入。

~~~powershell
pwsh -NoProfile -File .\scripts\test-package-node-release.ps1
pwsh -NoProfile -File .\scripts\test-package-portable-release.ps1
pwsh -NoProfile -File .\scripts\test-node-tray-supply-chain.ps1
git add -- deploy/agent.default.json deploy/nodetray.default.json deploy/helper.default.json deploy/README-节点部署.md scripts/build.ps1 scripts/package-node-release.ps1 scripts/test-package-node-release.ps1 scripts/test-package-portable-release.ps1 scripts/test-node-tray-supply-chain.ps1
git commit -m "feat: ship compute default configurations"
~~~

Expected: 三个合同 PASS。

---

### Task 3: Helper 默认源通过 create-only 提权路径导入

**Files:**
- Modify: internal/nodetray/config/store.go
- Modify: internal/nodetray/config/store_test.go
- Modify: internal/nodetray/app/service.go
- Modify: internal/nodetray/app/service_test.go
- Modify: internal/nodetray/elevated/actions.go
- Modify: internal/nodetray/elevated/actions_test.go
- Modify: internal/nodetray/windows/elevation/message.go
- Modify: internal/nodetray/windows/elevation/message_test.go

**Interfaces:**
- Produces: config.ErrHelperConfigExists。
- Produces: (*config.Store).PrepareDefaultHelperWrite() (config.PreparedWrite, error)。
- Extends: config.PreparedWrite 增加 CreateOnly bool。
- Consumes: 包根 helper.default.json；路径从可信 helper.exe 的同级目录派生，不接受 UI 指定路径。

- [ ] **Step 1: 写 Store 默认源 RED**

增加四类测试：

1. 运行配置和备份均不存在时，LoadHelperForm 严格解码 helper.default.json 并显示可编辑字段，但允许尚未填写的空 allowed_roots，且不创建 data\helper。
2. PrepareDefaultHelperWrite 将空 log_dir 固定为 data\helper\logs，调用共享校验并返回 CreateOnly: true。
3. 正式配置或备份任一存在时返回 ErrHelperConfigExists，不覆盖。
4. 未知字段、尾随 JSON、空 allowed_roots 在准备导入时失败，错误不泄漏路径或内容。

- [ ] **Step 2: 运行 Store RED**

~~~powershell
go test -count=1 ./internal/nodetray/config -run 'HelperDefault|PrepareDefaultHelper'
~~~

Expected: FAIL；接口与字段尚不存在。

- [ ] **Step 3: 实现 Store 窄接口**

~~~go
var ErrHelperConfigExists = errors.New("helper_config_exists")

type PreparedWrite struct {
    TargetPath    string
    CanonicalJSON []byte
    SHA256        string
    CreateOnly    bool
}

func (s *Store) helperDefaultPath() string {
    return filepath.Join(filepath.Dir(s.paths.HelperExecutable), "helper.default.json")
}
~~~

PrepareDefaultHelperWrite 先检查正式配置和备份，再严格解码默认源；空 LogDir 替换为 filepath.Join(filepath.Dir(s.paths.HelperConfig), "logs")；此时必须完整调用 helper.ValidateConfig，因此未填写 allowed_roots 的默认源只能编辑、不能启用。验证通过后返回 canonical JSON。普通 PrepareHelperWrite 保持 CreateOnly: false。

- [ ] **Step 4: 写提权端 create-only RED**

覆盖：

- 目标或备份已经存在时，CreateOnly: true 必须失败且逐字节不变。
- 普通进程准备后、提权端拿锁前出现目标时，仍不得覆盖。
- 完全不存在时创建受保护正式配置和备份。

- [ ] **Step 5: 运行提权端 RED**

~~~powershell
go test -count=1 ./internal/nodetray/elevated -run 'CreateOnly|DefaultHelper'
~~~

Expected: FAIL；旧写入器会覆盖现有配置。

- [ ] **Step 6: 在锁内实现 create-only 二次校验**

savePreparedHelper 获得受保护目录锁后把 CreateOnly 传给 saveLocked。在任何写入前执行：

~~~go
if createOnly && (pathExists(target) || pathExists(backup)) {
    return trayconfig.ErrHelperConfigExists
}
~~~

任一 Stat 非 os.ErrNotExist 错误都失败关闭。在 elevation/message.go 中新增并允许稳定错误代码 helper_config_exists；Service 收到该代码时按“另一条受保护写入已先完成”处理，绝不重试覆盖。

- [ ] **Step 7: 写 Service 自动导入 RED**

覆盖：

- disabled 改为 manual enabled：prepare-default-helper -> elevate-write_helper_config -> helper-sha -> save-settings。
- automatic enabled：导入完成后才安装任务。
- 已有配置：跳过默认写入并继续。
- 默认源无效、UAC 取消或写入失败：不保存启用状态、不启动 Helper、不安装任务。
- StartHelper 配置缺失时先导入；失败时不调用 Start。

- [ ] **Step 8: 运行 Service RED**

~~~powershell
go test -count=1 ./internal/nodetray/app -run 'DefaultHelper|HelperEnable|StartHelper'
~~~

Expected: FAIL；Service 尚未请求默认导入。

- [ ] **Step 9: 实现 Service 自动导入**

扩展 app.Store：

~~~go
PrepareDefaultHelperWrite() (config.PreparedWrite, error)
~~~

增加私有 ensureDefaultHelperConfig(ctx)。ErrHelperConfigExists 表示已存在，不写入；其他准备错误返回 helper_config_invalid；提权成功后更新 Helper 期望 SHA。SaveTraySettings 在启用 Helper 且策略变化时先导入，再处理任务策略；StartHelper 在任何启动动作前调用它。

- [ ] **Step 10: 运行 Helper 全量 GREEN 并提交**

~~~powershell
go test -count=1 ./internal/nodetray/config ./internal/nodetray/elevated ./internal/nodetray/app ./internal/nodetray/production ./nodetray
git add -- internal/nodetray/config/store.go internal/nodetray/config/store_test.go internal/nodetray/app/service.go internal/nodetray/app/service_test.go internal/nodetray/elevated/actions.go internal/nodetray/elevated/actions_test.go internal/nodetray/windows/elevation/message.go internal/nodetray/windows/elevation/message_test.go
git commit -m "feat: import helper defaults through elevation"
~~~

Expected: 全部 PASS。

---

### Task 4: 完整回归并生成新的双 ZIP

**Files:**
- Verify only: scripts/package-portable-release.ps1
- Output only: D:\code\mySingerServer\publish\MySingerServer-compute-win-x64-<release>.zip
- Output only: D:\code\mySingerServer\publish\MySingerServer-manager-win-x64-<release>.zip

**Interfaces:**
- Consumes: Tasks 1-3 的默认配置和 Helper 导入合同。
- Produces: 两个 ZIP、各自 .zip.sha256、包内 release-manifest.json。

- [ ] **Step 1: 格式与 Go 回归**

~~~powershell
gofmt -w internal/config/gui.go internal/config/config_test.go internal/gui/runtime_status.go internal/gui/runtime_status_test.go cmd/gui/operational_runtime.go cmd/gui/operational_runtime_test.go internal/nodetray/config/store.go internal/nodetray/config/store_test.go internal/nodetray/app/service.go internal/nodetray/app/service_test.go internal/nodetray/elevated/actions.go internal/nodetray/elevated/actions_test.go
git diff --check
go test -count=1 ./internal/config ./internal/gui ./cmd/gui ./internal/nodetray/config ./internal/nodetray/elevated ./internal/nodetray/app ./internal/nodetray/production ./nodetray
~~~

Expected: 全部 PASS，git diff --check 无输出。

- [ ] **Step 2: 发布合同与供应链门禁**

~~~powershell
pwsh -NoProfile -File .\scripts\test-package-node-release.ps1
pwsh -NoProfile -File .\scripts\test-package-manager-release.ps1
pwsh -NoProfile -File .\scripts\test-package-portable-release.ps1
pwsh -NoProfile -File .\scripts\test-node-tray-supply-chain.ps1
~~~

Expected: 四个发布与供应链 PASS 标记全部出现。

- [ ] **Step 3: 构建 fresh stage**

~~~powershell
pwsh -NoProfile -File .\scripts\build.ps1
~~~

Expected: stage 包含五个 EXE、原生依赖、Everything/WebView2 和四份默认配置；不得复用修改前 stage。

- [ ] **Step 4: 生成双 ZIP**

Run: scripts/package-portable-release.ps1，参数如下：

~~~text
StageDir       = .\artifacts\stage
OutputDir      = D:\code\mySingerServer\publish
ReleaseId      = <yyyyMMdd>-main-<short-head>
BuildDate      = <yyyy-MM-dd>
SourceRevision = <full-head>
~~~

Expected: 新增 Compute/Manager ZIP 和两个 SHA-256 sidecar，不覆盖旧产物。

- [ ] **Step 5: 解压复核最终内容**

Compute 必须含：

~~~text
MySingerServer-Compute/data/agent/agent.json
MySingerServer-Compute/data/nodetray/tray.json
MySingerServer-Compute/helper.default.json
~~~

且不含 data/helper/helper.json。Manager 必须含 MySingerServer-Manager/gui.json。逐一核对 sidecar 和 release-manifest.json 的大小及 SHA-256。

- [ ] **Step 6: 最终状态核验**

~~~powershell
git status --short
git log -4 --oneline
~~~

Expected: 只保留任务开始前已有的用户文档改动和 publish/ 产物，不出现临时缓存或意外暂存文件。
