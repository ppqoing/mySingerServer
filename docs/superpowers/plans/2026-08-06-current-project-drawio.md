# 当前项目模块结构图 Draw.io Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 生成一个可由 draw.io/diagrams.net 直接打开和编辑的三页当前项目模块结构图。

**Architecture:** 使用未压缩的 Draw.io `mxfile` XML，每页对应独立的 `diagram` 和 `mxGraphModel`。三个页面分别表达运行拓扑、源码模块依赖、协议与数据所有权，并通过统一颜色、线型、容器和图例保持一致语义。

**Tech Stack:** Draw.io XML、`mxGraphModel`、PowerShell XML DOM 校验、Git。

## Global Constraints

- 交付文件固定为 `docs/current-project-module-architecture.drawio`。
- 页面固定为“整体运行拓扑”“源码模块依赖”“协议与数据关系”。
- 使用未压缩 Draw.io XML，所有节点和连接线必须可编辑。
- 中央端为蓝色，媒体节点为绿色，数据存储为紫色，原生模块为橙色，本机控制为青色。
- 实线表示直接依赖或主要调用；虚线表示生命周期控制、生成/嵌入或可选关系。
- 使用 `CONTEXT.md` 中的规范名称，不修改业务代码、数据库定义或运行配置。

---

### Task 1: 创建文件骨架和整体运行拓扑页

**Files:**
- Create: `docs/current-project-module-architecture.drawio`
- Reference: `docs/current-project-architecture.md`
- Reference: `CONTEXT.md`

**Interfaces:**
- Consumes: 当前运行边界和规范领域名称。
- Produces: 合法 `mxfile`、三个具名页面，以及完整的“整体运行拓扑”页面。

- [ ] **Step 1: 验证目标文件状态**

```powershell
$path = 'docs\current-project-module-architecture.drawio'
if (Test-Path -LiteralPath $path) { 'UPDATE_EXISTING_DRAWIO' } else { 'CREATE_NEW_DRAWIO' }
```

Expected: 首次执行输出 `CREATE_NEW_DRAWIO`。

- [ ] **Step 2: 创建未压缩 XML 骨架**

```xml
<mxfile host="app.diagrams.net" modified="2026-08-06T00:00:00.000Z" agent="Codex" version="24.7.17" type="device" compressed="false">
  <diagram id="runtime-topology" name="整体运行拓扑"><mxGraphModel><root><mxCell id="0"/><mxCell id="1" parent="0"/></root></mxGraphModel></diagram>
  <diagram id="source-modules" name="源码模块依赖"><mxGraphModel><root><mxCell id="0"/><mxCell id="1" parent="0"/></root></mxGraphModel></diagram>
  <diagram id="protocol-data" name="协议与数据关系"><mxGraphModel><root><mxCell id="0"/><mxCell id="1" parent="0"/></root></mxGraphModel></diagram>
</mxfile>
```

页面内业务单元分别使用 `rt-`、`mod-`、`pd-` ID 前缀。

- [ ] **Step 3: 完成整体运行拓扑节点**

加入浏览器、中央 React Web、`gui.exe`、PostgreSQL，以及两个媒体节点容器。主媒体节点展开节点托盘程序、Agent、Worker 池、删除 Helper、SQLite、本机媒体目录；第二节点使用简化布局表达多机器连接。页面底部加入线型和颜色图例。

- [ ] **Step 4: 完成整体运行连接**

```text
浏览器 -> gui.exe
中央 React Web -.嵌入.-> gui.exe
gui.exe <-> PostgreSQL
gui.exe <-> Agent
节点托盘程序 -.控制.-> Agent / 删除 Helper
Agent -> Worker 池
Agent <-> SQLite / 删除 Helper
Worker 池 -> 本机媒体目录
删除 Helper -> 本机媒体目录
Agent -> PostgreSQL
```

- [ ] **Step 5: 校验第一页**

```powershell
[xml]$xml = Get-Content -Raw -LiteralPath 'docs\current-project-module-architecture.drawio'
$names = @($xml.mxfile.diagram | ForEach-Object { $_.name })
if ($names.Count -ne 3 -or $names[0] -ne '整体运行拓扑') { throw 'DRAWIO_PAGE_STRUCTURE_INVALID' }
if ($xml.SelectNodes("//diagram[@id='runtime-topology']//mxCell").Count -lt 25) { throw 'DRAWIO_RUNTIME_PAGE_TOO_SMALL' }
```

Expected: exit 0。

### Task 2: 完成源码模块依赖页

**Files:**
- Modify: `docs/current-project-module-architecture.drawio`
- Reference: `cmd/`, `internal/`, `webui/`, `nodetray/frontend/`, `videocore/`, `mediacore/`

**Interfaces:**
- Consumes: Task 1 的合法三页 `mxfile`。
- Produces: 使用方指向被依赖方的源码模块依赖页。

- [ ] **Step 1: 加入生产入口和分组**

入口固定为 `cmd/gui`、`cmd/agent`、`cmd/worker`、`cmd/helper`、`nodetray`。中央分组包含 `internal/gui`、`firstscreen`、`phase2`、`proto`、`config`；节点分组包含 `agent`、`worker`、`wproc`、`store`、`syncer`、`enum`、`stats`、`diskmap`、`helper`；本机控制分组包含 `internal/nodetray/*`、`nodectl`、`agentcontrol`、`helpercontrol`、`machineid`。

- [ ] **Step 2: 加入前端和原生依赖**

```text
webui -.构建/嵌入.-> internal/gui/web -> cmd/gui
nodetray/frontend -.Wails embed.-> nodetray
internal/wproc -> VideoCore -> FFmpeg
internal/wproc -.兼容.-> MediaCore
```

- [ ] **Step 3: 加入主要 Go 包依赖**

```text
cmd/gui -> internal/gui, firstscreen, phase2, proto, config
cmd/agent -> internal/agent, worker, store, syncer, enum, nodectl
cmd/worker -> internal/wproc
cmd/helper -> internal/helper, helpercontrol, machineid, nodectl
nodetray -> internal/nodetray/*, machineid, nodectl
internal/agent -> worker, store, syncer, proto
internal/gui -> firstscreen, phase2, proto
internal/worker -> internal/wproc
```

- [ ] **Step 4: 校验模块覆盖**

```powershell
[xml]$xml = Get-Content -Raw -LiteralPath 'docs\current-project-module-architecture.drawio'
$text = ($xml.SelectNodes("//diagram[@id='source-modules']//mxCell") | ForEach-Object { $_.value }) -join "`n"
$required = @('cmd/gui','cmd/agent','cmd/worker','cmd/helper','nodetray','internal/gui','internal/agent','internal/worker','internal/wproc','webui','nodetray/frontend','VideoCore','MediaCore','FFmpeg')
$missing = @($required | Where-Object { $text -notmatch [regex]::Escape($_) })
if ($missing.Count) { throw "DRAWIO_MODULES_MISSING=$($missing -join ',')" }
```

Expected: exit 0，缺失模块数为 0。

### Task 3: 完成协议与数据关系页

**Files:**
- Modify: `docs/current-project-module-architecture.drawio`
- Reference: `internal/proto/message.go`, `internal/nodectl/message.go`, `internal/worker/messages.go`, `deploy/central.sql`, `internal/store/`

**Interfaces:**
- Consumes: Task 1 和 Task 2 的页面与统一样式。
- Produces: 跨进程协议、数据库所有权和原生 ABI 关系页。

- [ ] **Step 1: 建立端点和协议连接**

```text
浏览器 -> GUI: HTTP / JSON
NodeTray React -> NodeTray Go: Wails Bridge
NodeTray Go -> Agent/删除 Helper: nodectl / MessagePack v1
GUI <-> Agent: TCP / MessagePack v1
Agent <-> Worker: Windows 命名管道 / Worker IPC
Agent <-> 删除 Helper: Windows 命名管道 / Delete IPC
Worker -> VideoCore: C ABI
VideoCore -> FFmpeg: MSVC SDK / runtime DLL
```

- [ ] **Step 2: 建立数据所有权**

SQLite 标注 `files`、`image_features`、`video_features`、`video_frames`、`sync_queue`；PostgreSQL 标注中央文件、特征、任务、`dup_groups`、`dup_members`、`pair_scores`。Agent 到 PostgreSQL 标注“增量同步”，GUI 到 PostgreSQL 标注“查询 / 分析 / 调度”。

- [ ] **Step 3: 添加身份和边界备注**

必须包含：GUI 配置 Agent endpoint；机器唯一 ID 由 Agent 握手上报；浏览器不直连 Agent/数据库；Worker 只由 Agent 管理；删除 Helper 为可选组件。

- [ ] **Step 4: 校验协议和数据覆盖**

```powershell
[xml]$xml = Get-Content -Raw -LiteralPath 'docs\current-project-module-architecture.drawio'
$text = ($xml.SelectNodes("//diagram[@id='protocol-data']//mxCell") | ForEach-Object { $_.value }) -join "`n"
$required = @('HTTP / JSON','MessagePack v1','Windows 命名管道','Wails Bridge','C ABI','SQLite','PostgreSQL','sync_queue','dup_groups','Agent endpoint','机器唯一 ID')
$missing = @($required | Where-Object { $text -notmatch [regex]::Escape($_) })
if ($missing.Count) { throw "DRAWIO_PROTOCOLS_MISSING=$($missing -join ',')" }
```

Expected: exit 0，缺失协议或数据项数为 0。

### Task 4: 完整文件验收

**Files:**
- Verify: `docs/current-project-module-architecture.drawio`
- Verify: `docs/superpowers/specs/2026-08-06-current-project-drawio-design.md`

**Interfaces:**
- Consumes: 完整三页 Draw.io 文件。
- Produces: XML、页面、关键模块、关键协议和连接端点的静态验收结果。

- [ ] **Step 1: 验证 XML、页面名称和可编辑结构**

```powershell
$path = 'docs\current-project-module-architecture.drawio'
[xml]$xml = Get-Content -Raw -LiteralPath $path
if ($xml.mxfile.compressed -ne 'false') { throw 'DRAWIO_MUST_BE_UNCOMPRESSED' }
$names = @($xml.mxfile.diagram | ForEach-Object { $_.name })
$expected = @('整体运行拓扑','源码模块依赖','协议与数据关系')
if (Compare-Object $expected $names) { throw "DRAWIO_PAGE_NAMES=$($names -join ',')" }
foreach ($page in $xml.mxfile.diagram) {
    if ($null -eq $page.mxGraphModel.root) { throw "DRAWIO_ROOT_MISSING=$($page.name)" }
    if (@($page.mxGraphModel.root.mxCell).Count -lt 10) { throw "DRAWIO_PAGE_TOO_SMALL=$($page.name)" }
}
```

- [ ] **Step 2: 验证所有连接端点存在**

```powershell
[xml]$xml = Get-Content -Raw -LiteralPath 'docs\current-project-module-architecture.drawio'
$ids = @{}
$xml.SelectNodes('//mxCell[@id]') | ForEach-Object { $ids[$_.id] = $true }
foreach ($edge in $xml.SelectNodes('//mxCell[@edge="1"]')) {
    if ($edge.source -and -not $ids.ContainsKey($edge.source)) { throw "DRAWIO_BAD_SOURCE=$($edge.id)" }
    if ($edge.target -and -not $ids.ContainsKey($edge.target)) { throw "DRAWIO_BAD_TARGET=$($edge.id)" }
}
```

- [ ] **Step 3: 验证编码和变更范围**

```powershell
$path = (Resolve-Path 'docs\current-project-module-architecture.drawio').Path
$bytes = [IO.File]::ReadAllBytes($path)
$hasBom = $bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF
if ($hasBom) { throw 'DRAWIO_UTF8_BOM_NOT_ALLOWED' }
git diff --check
git status --short
```

Expected: UTF-8 无 BOM；除计划内文档和 Draw.io 文件外没有新增变更。

- [ ] **Step 4: 提交交付物**

```powershell
git add docs/current-project-module-architecture.drawio docs/superpowers/plans/2026-08-06-current-project-drawio.md
git commit -m "docs: add project module architecture drawio"
git log -2 --oneline --decorate
git status --short --branch
```

Expected: Draw.io 文件和实施计划可追踪，工作区无未提交的计划内文件。
