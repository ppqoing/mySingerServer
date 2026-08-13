# GUI 与 NodeTray 本地任务可视化路径选择 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 GUI 可以浏览目标 Agent 的文件系统并选择多个扫描根目录，同时让 NodeTray 通过 Windows 原生窗口选择多个本机根目录。

**Architecture:** GUI 通过新增 HTTP 接口和现有 GUI-Agent TCP 长连接请求 Agent 按页枚举单层目录；NodeTray 直接调用 Wails 的 Windows 原生目录选择接口。两端只在页面内维护选择草稿，最终仍提交现有任务 `roots` 字段，由 Agent 做最终校验。

**Tech Stack:** Go 1.26.5、Windows API、MessagePack TCP 协议、Wails v2.12.0、React 19、TypeScript 5.9、Vitest 4、Vite 8。

## Global Constraints

- GUI 选择的是当前目标 Agent 所在机器的路径，不是 Manager 主机路径。
- 一次任务支持多个根目录；文件必须展示但不可选择。
- GUI 中隐藏项和系统项默认不显示，用户可显式开启；NodeTray 原生窗口跟随 Windows 资源管理器设置。
- 手工输入只接受绝对 Windows 路径或 UNC 路径，不展开环境变量。
- Windows 路径比较大小写不敏感；阻止重复路径和父子范围重叠。
- 选择、保存和创建任务均不得申请 UAC；只有启动 Helper 保留原提权边界。
- 不读取文件内容，不计算哈希，不生成缩略图，不新增数据库表。
- 不新增 Manager 认证或共享令牌；明确沿用现有可信内网和防火墙边界。
- 执行时先用 `superpowers:using-git-worktrees` 创建隔离工作树，不能覆盖主工作树现有未提交文档和 `publish/`。
- 验证限于每个任务列出的聚焦测试、两次前端构建和一次人工 Windows 验收；不运行 `go test ./...`，不启动多轮审查。

---

## 文件结构映射

### 新增文件

- `internal/agent/filesystem_browser.go`：平台无关的浏览接口、排序、分页游标和安全错误映射。
- `internal/agent/filesystem_browser_windows.go`：Windows 盘符、目录和文件属性枚举。
- `internal/agent/filesystem_browser_other.go`：非 Windows 平台返回稳定的不支持错误，保证交叉编译。
- `internal/agent/filesystem_browser_windows_test.go`：真实临时目录的 Windows 枚举行为。
- `internal/gui/filesystem_browser.go`：GUI 请求关联、超时、断线和迟到响应处理。
- `internal/gui/filesystem_browser_test.go`：Broker 并发与连接生命周期测试。
- `webui/src/features/scans/RemotePathBrowser.tsx`：目标 Agent 文件浏览弹窗。
- `webui/src/features/scans/RemotePathBrowser.test.tsx`：远程浏览交互测试。
- `webui/src/features/scans/taskRoots.ts`：GUI 根目录去重和父子覆盖规则。
- `webui/src/features/scans/taskRoots.test.ts`：GUI 路径规则测试。
- `nodetray/frontend/src/pages/taskRoots.ts`：NodeTray 根目录去重和父子覆盖规则。
- `nodetray/frontend/src/pages/taskRoots.test.ts`：NodeTray 路径规则测试。

### 修改文件

- `internal/proto/message.go`、`internal/proto/message_test.go`：新增浏览消息和 DTO。
- `internal/agent/server.go`、`internal/agent/server_test.go`：接收浏览请求并回传响应。
- `cmd/agent/main.go`、`cmd/agent/main_test.go`：将生产浏览器注入 Agent Server。
- `internal/gui/pool.go`、`internal/gui/pool_test.go`：增加 Agent 断线通知。
- `internal/gui/httpapi.go`、`internal/gui/httpapi_test.go`：增加浏览 HTTP API。
- `cmd/gui/operational_runtime.go`、`cmd/gui/operational_runtime_test.go`：组合 Broker 并路由响应。
- `webui/src/api/contracts.ts`、`webui/src/api/appApi.ts`、`webui/src/api/appApi.test.ts`：增加浏览 API 类型和严格解码。
- `webui/src/features/scans/ScansPage.tsx`、`webui/src/features/scans/ScansPage.test.tsx`：接入浏览弹窗和多根目录列表。
- `webui/src/features/operational-pages.css`：浏览弹窗、文件禁用态和根目录列表样式。
- `internal/gui/web/**`：Vite 重新生成的 GUI 嵌入资源。
- `internal/nodetray/traymodel/model.go`、`internal/nodetray/traymodel/model_test.go`：NodeTray 目录选择结果 DTO。
- `nodetray/app.go`、`nodetray/app_test.go`：调用 Wails 原生目录选择。
- `nodetray/frontend/src/api/localAgent.ts`、`nodetray/frontend/src/api/localAgent.test.ts`：目录选择绑定。
- `nodetray/frontend/src/pages/LocalTasksPage.tsx`、`nodetray/frontend/src/pages/LocalTasksPage.test.tsx`：多根目录选择界面。
- `nodetray/frontend/src/app.css`：本地任务路径列表样式。
- `nodetray/frontend/dist/**`：Vite 重新生成的 NodeTray 嵌入资源。

---

### Task 1: 浏览协议与 Agent 文件系统枚举

**Files:**
- Create: `internal/agent/filesystem_browser.go`
- Create: `internal/agent/filesystem_browser_windows.go`
- Create: `internal/agent/filesystem_browser_other.go`
- Create: `internal/agent/filesystem_browser_windows_test.go`
- Modify: `internal/proto/message.go`
- Modify: `internal/proto/message_test.go`
- Modify: `internal/agent/server.go`
- Modify: `internal/agent/server_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`

**Interfaces:**
- Produces: `proto.FilesystemBrowseRequest.Validate() error`
- Produces: `proto.FilesystemBrowseResponse`
- Produces: `agent.FilesystemBrowser.Browse(context.Context, proto.FilesystemBrowseRequest) proto.FilesystemBrowseResponse`
- Produces: `agent.NewFilesystemBrowser() FilesystemBrowser`
- Produces: `(*agent.Server).SetFilesystemBrowser(FilesystemBrowser)`

- [ ] **Step 1: 为协议编号、严格校验和解码写失败测试**

在 `internal/proto/message_test.go` 增加以下合同：

```go
func TestFilesystemBrowseMessagesRoundTrip(t *testing.T) {
	request := FilesystemBrowseRequest{
		RequestID: "browse-1", Path: `D:\Media`, ShowHidden: true, Limit: 200,
	}
	response := FilesystemBrowseResponse{
		RequestID: "browse-1", CurrentPath: `D:\Media`,
		Entries: []FilesystemEntry{{Name: "Photos", Path: `D:\Media\Photos`, Kind: FilesystemEntryDirectory, Selectable: true}},
	}
	for _, item := range []struct{ typ uint8; value any; target any }{
		{MsgFilesystemBrowse, request, &FilesystemBrowseRequest{}},
		{MsgFilesystemBrowseResult, response, &FilesystemBrowseResponse{}},
	} {
		body, err := msgpack.Marshal(item.value)
		if err != nil { t.Fatal(err) }
		got, err := Decode(item.typ, body)
		if err != nil { t.Fatal(err) }
		if reflect.TypeOf(got) != reflect.TypeOf(item.target) { t.Fatalf("decoded %T", got) }
	}
}

func TestFilesystemBrowseRequestValidate(t *testing.T) {
	valid := FilesystemBrowseRequest{RequestID: "browse-1", Path: `D:\Media`, Limit: 200}
	if err := valid.Validate(); err != nil { t.Fatal(err) }
	for _, mutate := range []func(*FilesystemBrowseRequest){
		func(v *FilesystemBrowseRequest) { v.RequestID = "" },
		func(v *FilesystemBrowseRequest) { v.Path = `Media\relative` },
		func(v *FilesystemBrowseRequest) { v.Limit = 501 },
	} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); err == nil { t.Fatal("invalid request accepted") }
	}
}
```

固定新消息编号为 GUI→Agent `16` 和 Agent→GUI `27`，仅追加，不移动既有编号。

- [ ] **Step 2: 运行协议 RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/proto -run 'FilesystemBrowse'
```

Expected: FAIL，提示浏览消息常量或 DTO 尚未定义。

- [ ] **Step 3: 实现最小协议 DTO 与 Decode 分支**

在 `internal/proto/message.go` 增加：

```go
const (
	MsgFilesystemBrowse       uint8 = 16
	MsgFilesystemBrowseResult uint8 = 27
)

const (
	FilesystemEntryDrive     = "drive"
	FilesystemEntryDirectory = "directory"
	FilesystemEntryFile      = "file"
)

type FilesystemBrowseRequest struct {
	RequestID  string `msgpack:"request_id"`
	Path       string `msgpack:"path,omitempty"`
	ShowHidden bool   `msgpack:"show_hidden"`
	Cursor     string `msgpack:"cursor,omitempty"`
	Limit      int    `msgpack:"limit"`
}

type FilesystemEntry struct {
	Name       string `msgpack:"name"`
	Path       string `msgpack:"path"`
	Kind       string `msgpack:"kind"`
	Hidden     bool   `msgpack:"hidden"`
	System     bool   `msgpack:"system"`
	Selectable bool   `msgpack:"selectable"`
}

type FilesystemBrowseResponse struct {
	RequestID   string            `msgpack:"request_id"`
	CurrentPath string            `msgpack:"current_path,omitempty"`
	ParentPath  string            `msgpack:"parent_path,omitempty"`
	Entries     []FilesystemEntry `msgpack:"entries"`
	NextCursor string            `msgpack:"next_cursor,omitempty"`
	ErrorCode  string            `msgpack:"error_code,omitempty"`
}
```

`Validate` 必须接受空 `Path` 作为磁盘入口；非空值必须是盘符绝对路径或 UNC；这里使用显式盘符和 UNC 规则校验，不能使用依赖当前宿主系统的 `filepath.IsAbs`。`Limit == 0` 规范为默认 200，显式值只允许 `1..500`；游标长度限制为 1024 字节。为两个新消息补 `Decode` 分支。

- [ ] **Step 4: 为 Windows 枚举行为写失败测试**

在 `internal/agent/filesystem_browser_windows_test.go` 使用 `t.TempDir()` 创建两个目录、普通文件和隐藏项，覆盖：

```go
func TestFilesystemBrowserShowsFilesButOnlyDirectoriesSelectable(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Photos"), 0o700); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(root, "cover.jpg"), []byte("x"), 0o600); err != nil { t.Fatal(err) }
	response := NewFilesystemBrowser().Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-1", Path: root, Limit: 200,
	})
	if response.ErrorCode != "" { t.Fatal(response.ErrorCode) }
	if len(response.Entries) != 2 { t.Fatalf("entries=%#v", response.Entries) }
	if response.Entries[0].Kind != proto.FilesystemEntryDirectory || !response.Entries[0].Selectable { t.Fatal("directory not selectable") }
	if response.Entries[1].Kind != proto.FilesystemEntryFile || response.Entries[1].Selectable { t.Fatal("file selectable") }
}
```

同文件再加入隐藏/系统过滤、目录优先排序、每页 200、无效游标、路径不存在、访问拒绝安全码和取消 context 的用例。访问拒绝只断言稳定错误码，不依赖本机管理员令牌能否绕过 DACL。

- [ ] **Step 5: 运行 Agent 枚举 RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/agent -run 'FilesystemBrowser'
```

Expected: FAIL，提示 `NewFilesystemBrowser` 尚未定义。

- [ ] **Step 6: 实现 Windows 枚举器和非 Windows 适配器**

在 `internal/agent/filesystem_browser.go` 定义：

```go
type FilesystemBrowser interface {
	Browse(context.Context, proto.FilesystemBrowseRequest) proto.FilesystemBrowseResponse
}
```

Windows 实现使用 `os.ReadDir` 枚举单层条目，并通过 `GetFileAttributesW` 判断隐藏和系统属性；空路径通过 `GetLogicalDrives` 与 `GetDriveTypeW` 列出盘符。排序键固定为“目录/盘符优先、文件其次、`strings.ToLower(name)`、原名称”。分页游标编码最后一项的类型排序值和名称，不保存服务端句柄。

错误只映射为 `invalid_path`、`path_not_found`、`access_denied`、`volume_unavailable`、`browse_cancelled`、`browse_failed`，响应和日志不得包含完整路径。非 Windows 实现返回 `browse_unsupported`。

- [ ] **Step 7: 为 Server 请求路由和生产注入写失败测试**

在 `internal/agent/server_test.go` 使用 `net.Pipe` 验证请求收到相同 `request_id` 的 `MsgFilesystemBrowseResult`，并验证文件条目原样回传。再用阻塞 fake 验证同一连接第二个并发浏览请求返回 `browse_busy`，且 Ping/Pong 不受阻塞枚举影响。

在 `cmd/agent/main_test.go` 使用注入构造器记录 `SetFilesystemBrowser` 被调用一次。

- [ ] **Step 8: 实现 Server 与 cmd/agent 接线**

为 `Server` 增加浏览器字段和 `SetFilesystemBrowser`。每条连接使用容量为 1 的浏览 gate；实际枚举在受连接 context 管理的 goroutine 中执行，完成后通过现有并发安全 `proto.Conn.WriteFrame` 回传。gate 已占用时立即返回同 `request_id` 的 `browse_busy`，不能阻塞消息读取循环。

在 `cmd/agent/main.go` 创建一次 `agent.NewFilesystemBrowser()` 并注入 Server。不新增配置项，不改变监听端口。

- [ ] **Step 9: 运行 Task 1 聚焦 GREEN**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/proto ./internal/agent ./cmd/agent -run 'FilesystemBrowse|FilesystemBrowser'
```

Expected: PASS。

- [ ] **Step 10: 提交 Task 1**

```powershell
git add -- internal/proto/message.go internal/proto/message_test.go internal/agent/filesystem_browser.go internal/agent/filesystem_browser_windows.go internal/agent/filesystem_browser_other.go internal/agent/filesystem_browser_windows_test.go internal/agent/server.go internal/agent/server_test.go cmd/agent/main.go cmd/agent/main_test.go
git commit -m "feat: browse agent filesystem paths"
```

---

### Task 2: GUI 请求关联与 HTTP 浏览接口

**Files:**
- Create: `internal/gui/filesystem_browser.go`
- Create: `internal/gui/filesystem_browser_test.go`
- Modify: `internal/gui/pool.go`
- Modify: `internal/gui/pool_test.go`
- Modify: `internal/gui/httpapi.go`
- Modify: `internal/gui/httpapi_test.go`
- Modify: `cmd/gui/operational_runtime.go`
- Modify: `cmd/gui/operational_runtime_test.go`

**Interfaces:**
- Consumes: `proto.MsgFilesystemBrowse`、`proto.MsgFilesystemBrowseResult`
- Produces: `(*gui.FilesystemBroker).Browse(context.Context, string, proto.FilesystemBrowseRequest) (proto.FilesystemBrowseResponse, error)`
- Produces: `(*gui.FilesystemBroker).Dispatch(string, any) bool`
- Produces: `(*gui.FilesystemBroker).FailMachine(string)`
- Produces: `(*gui.API).SetFilesystemBrowser(filesystemBrowseService)`
- Produces: `POST /api/agents/{machine_id}/filesystem/browse`

- [ ] **Step 1: 为 Broker 关联、取消和断线写失败测试**

在 `internal/gui/filesystem_browser_test.go` 定义记录发送内容的 fake transport，并覆盖：

```go
func TestFilesystemBrokerPairsResponseByMachineAndRequestID(t *testing.T) {
	transport := &fakeFilesystemTransport{online: true}
	broker := NewFilesystemBroker(transport)
	result := make(chan proto.FilesystemBrowseResponse, 1)
	go func() {
		response, _ := broker.Browse(context.Background(), "machine-a", proto.FilesystemBrowseRequest{Path: `D:\Media`, Limit: 200})
		result <- response
	}()
	sent := <-transport.sent
	if !broker.Dispatch("machine-a", &proto.FilesystemBrowseResponse{RequestID: sent.RequestID, CurrentPath: `D:\Media`}) { t.Fatal("response not claimed") }
	if got := <-result; got.CurrentPath != `D:\Media` { t.Fatalf("response=%#v", got) }
}
```

再覆盖错误 machine 不得配对、context 取消清 pending、`FailMachine` 立即返回 `agent_disconnected`、迟到响应被忽略、离线 Agent 不发送。

- [ ] **Step 2: 运行 Broker RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/gui -run 'FilesystemBroker'
```

Expected: FAIL，提示 `NewFilesystemBroker` 尚未定义。

- [ ] **Step 3: 实现 Broker 和 Pool 断线通知**

在 `internal/gui/filesystem_browser.go` 使用 `crypto/rand` 生成 128-bit 十六进制 `request_id`，pending key 固定为 `machineID + "\x00" + requestID`。`Browse` 先检查 `IsOnline`，注册 pending 后发送，任何发送失败或 context 结束都删除 pending。

为 `Pool` 增加：

```go
func (pool *Pool) SetOnDisconnect(callback func(machineID string))
```

`AgentConn.Run` 仅在已认领连接从 online 变为 offline 时通知一次。回调调用 `broker.FailMachine(machineID)`，不得等待 HTTP 调用完成。

- [ ] **Step 4: 为 HTTP 严格输入和错误映射写失败测试**

在 `internal/gui/httpapi_test.go` 覆盖：

```go
func TestFilesystemBrowseHTTPUsesBodyPathAndReturnsFilesDisabled(t *testing.T) {
	service := &fakeFilesystemBrowseService{response: proto.FilesystemBrowseResponse{
		CurrentPath: `D:\Media`,
		Entries: []proto.FilesystemEntry{{Name: "cover.jpg", Path: `D:\Media\cover.jpg`, Kind: proto.FilesystemEntryFile, Selectable: false}},
	}}
	api := NewAPI(nil, nil, nil)
	api.SetFilesystemBrowser(service)
	request := httptest.NewRequest(http.MethodPost, "/api/agents/machine-a/filesystem/browse", strings.NewReader(`{"path":"D:\\Media","show_hidden":false,"limit":200}`))
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK { t.Fatalf("status=%d body=%s", response.Code, response.Body.String()) }
}
```

再覆盖未知 JSON 字段为 400、路径不进入 URL、离线为 503、超时为 504、访问拒绝为 403、路径不存在为 404、未配置服务为 503。

- [ ] **Step 5: 实现 HTTP API 与运行时组合**

`internal/gui/httpapi.go` 增加严格 JSON handler，使用请求 context 加一次固定浏览超时；HTTP DTO 使用 snake_case，并将协议 DTO 转换为 JSON。不要把请求路径写入错误文本或日志。

`cmd/gui/operational_runtime.go` 在 Pool 创建后创建 Broker；Agent 消息回调先调用：

```go
if resources.filesystemBrowser.Dispatch(machineID, message) {
	return
}
```

未被 Broker 消费的消息继续进入现有 phase2/delete/task 路由。Pool 断线回调连接到 `FailMachine`，API 通过 `SetFilesystemBrowser` 获取同一 Broker。

- [ ] **Step 6: 运行 Task 2 聚焦 GREEN**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/gui ./cmd/gui -run 'FilesystemBrowse|FilesystemBroker|Disconnect'
```

Expected: PASS。

- [ ] **Step 7: 提交 Task 2**

```powershell
git add -- internal/gui/filesystem_browser.go internal/gui/filesystem_browser_test.go internal/gui/pool.go internal/gui/pool_test.go internal/gui/httpapi.go internal/gui/httpapi_test.go cmd/gui/operational_runtime.go cmd/gui/operational_runtime_test.go
git commit -m "feat: expose remote agent path browser"
```

---

### Task 3: GUI 远程目录弹窗与多根目录列表

**Files:**
- Create: `webui/src/features/scans/RemotePathBrowser.tsx`
- Create: `webui/src/features/scans/RemotePathBrowser.test.tsx`
- Create: `webui/src/features/scans/taskRoots.ts`
- Create: `webui/src/features/scans/taskRoots.test.ts`
- Modify: `webui/src/api/contracts.ts`
- Modify: `webui/src/api/appApi.ts`
- Modify: `webui/src/api/appApi.test.ts`
- Modify: `webui/src/features/scans/ScansPage.tsx`
- Modify: `webui/src/features/scans/ScansPage.test.tsx`
- Modify: `webui/src/features/operational-pages.css`
- Modify: `internal/gui/web/**`（由 Vite 生成）

**Interfaces:**
- Consumes: `POST /api/agents/{machine_id}/filesystem/browse`
- Produces: `AppApi.browseAgentFilesystem(machineID, input, signal)`
- Produces: `addTaskRoot(current, candidate): RootChange`
- Produces: `RemotePathBrowser` React 组件

- [ ] **Step 1: 为 API 严格解码和路径规则写失败测试**

在 `webui/src/api/contracts.ts` 定义：

```ts
export interface FilesystemEntry {
  name: string;
  path: string;
  kind: "drive" | "directory" | "file";
  hidden: boolean;
  system: boolean;
  selectable: boolean;
}

export interface FilesystemPage {
  currentPath: string;
  parentPath: string;
  entries: FilesystemEntry[];
  nextCursor: string;
}
```

`taskRoots.test.ts` 必须覆盖：

```ts
expect(addTaskRoot(["D:\\Media"], "d:/media").kind).toBe("duplicate");
expect(addTaskRoot(["D:\\Media"], "D:\\Media\\Photos").kind).toBe("covered");
expect(addTaskRoot(["D:\\Media\\Photos"], "D:\\Media")).toEqual({
  kind: "replace", roots: ["D:\\Media"], covered: ["D:\\Media\\Photos"]
});
expect(addTaskRoot([], "Media\\relative").kind).toBe("invalid");
```

在 `appApi.test.ts` 验证路径只出现在 JSON body、machine ID 经过 URL segment 编码、未知 `kind` 或缺失 `selectable` 时严格拒绝响应。

- [ ] **Step 2: 运行 GUI API 与路径规则 RED**

Run:

```powershell
Set-Location webui
npm.cmd exec vitest run src/api/appApi.test.ts src/features/scans/taskRoots.test.ts
```

Expected: FAIL，提示 API 方法和 `addTaskRoot` 尚未定义。

- [ ] **Step 3: 实现 API、规范化和父子覆盖决策**

`taskRoots.ts` 只做字符串层面的 Windows 路径规范化，不访问文件系统。将 `/` 转为 `\`，保留盘符根 `D:\`，其他路径去掉末尾分隔符，使用小写 key 比较。组件在 `replace` 时调用确认框；用户拒绝则保留原列表。

`appApi.ts` 新增：

```ts
browseAgentFilesystem: (machineID, input, signal) => requestJson(
  `/api/agents/${encodeURIComponent(requiredText(machineID, "machine id"))}/filesystem/browse`,
  jsonPost({ path: input.path, show_hidden: input.showHidden, cursor: input.cursor, limit: input.limit }, signal),
  filesystemPage
)
```

- [ ] **Step 4: 为弹窗和 ScansPage 写失败测试**

`RemotePathBrowser.test.tsx` 覆盖磁盘入口、面包屑、目录导航、文件禁用、隐藏开关、加载下一页、错误后已选目录不丢失和取消请求。

`ScansPage.test.tsx` 增加：

```tsx
test("browses the selected Agent and submits multiple roots", async () => {
  const api = apiFor({
    browseAgentFilesystem: vi.fn().mockResolvedValue({
      currentPath: "D:\\Media", parentPath: "D:\\", nextCursor: "",
      entries: [{ name: "Photos", path: "D:\\Media\\Photos", kind: "directory", hidden: false, system: false, selectable: true }]
    })
  });
  render(<ScansPage api={api} />);
  const user = userEvent.setup();
  await user.selectOptions(await screen.findByLabelText("扫描 Agent"), "agent-a");
  await user.click(screen.getByRole("button", { name: "选择目录…" }));
  await user.click(await screen.findByRole("button", { name: /Photos/ }));
  await user.click(screen.getByRole("button", { name: "添加当前目录" }));
  expect(screen.getByText("D:\\Media\\Photos")).toBeVisible();
});
```

还要验证未选/离线 Agent 时按钮禁用、切换 Agent 清空草稿并提示、文件按钮不可选择、父目录替换需要确认。

- [ ] **Step 5: 实现 GUI 页面**

`RemotePathBrowser` 只接受 `machineID`、`api`、`open`、`onAdd`、`onClose`；每次请求使用新的 `AbortController`，切页或关闭前取消旧请求。当前目录使用单选高亮，文件条目设置 `disabled` 和 `aria-disabled="true"`。

`ScansPage` 将 `rootsText` 改为 `roots: string[]` 与单独的手工输入框；任务提交直接传 `roots`。不再解析竖线，但手工添加按钮仍支持逐个录入路径。

- [ ] **Step 6: 运行 Task 3 聚焦测试和一次构建**

Run:

```powershell
Set-Location webui
npm.cmd exec vitest run src/api/appApi.test.ts src/features/scans/taskRoots.test.ts src/features/scans/RemotePathBrowser.test.tsx src/features/scans/ScansPage.test.tsx
npm.cmd run build
```

Expected: 聚焦测试 PASS；TypeScript/Vite build PASS，并更新 `internal/gui/web` 嵌入资源。

- [ ] **Step 7: 提交 Task 3**

```powershell
Set-Location ..
git add -- webui/src/api/contracts.ts webui/src/api/appApi.ts webui/src/api/appApi.test.ts webui/src/features/scans/RemotePathBrowser.tsx webui/src/features/scans/RemotePathBrowser.test.tsx webui/src/features/scans/taskRoots.ts webui/src/features/scans/taskRoots.test.ts webui/src/features/scans/ScansPage.tsx webui/src/features/scans/ScansPage.test.tsx webui/src/features/operational-pages.css internal/gui/web
git commit -m "feat: select remote agent scan roots"
```

---

### Task 4: NodeTray Windows 原生目录选择与多根目录列表

**Files:**
- Create: `nodetray/frontend/src/pages/taskRoots.ts`
- Create: `nodetray/frontend/src/pages/taskRoots.test.ts`
- Modify: `internal/nodetray/traymodel/model.go`
- Modify: `internal/nodetray/traymodel/model_test.go`
- Modify: `nodetray/app.go`
- Modify: `nodetray/app_test.go`
- Modify: `nodetray/frontend/src/api/localAgent.ts`
- Modify: `nodetray/frontend/src/api/localAgent.test.ts`
- Modify: `nodetray/frontend/src/pages/LocalTasksPage.tsx`
- Modify: `nodetray/frontend/src/pages/LocalTasksPage.test.tsx`
- Modify: `nodetray/frontend/src/app.css`
- Modify: `nodetray/frontend/dist/**`（由 Vite 生成）

**Interfaces:**
- Produces: `traymodel.PathSelectionResult`
- Produces: `(*Backend).ChooseLocalTaskRoot(currentPath string) traymodel.PathSelectionResult`
- Produces: `chooseLocalTaskRoot(currentPath)` TypeScript API

- [ ] **Step 1: 为 Wails 后端目录选择写失败测试**

在 `traymodel/model.go` 定义结果：

```go
type PathSelectionResult struct {
	OK           bool   `json:"ok"`
	Path         string `json:"path"`
	Cancelled    bool   `json:"cancelled"`
	ErrorCode    string `json:"errorCode"`
	ErrorSummary string `json:"errorSummary"`
}
```

`nodetray/app_test.go` 注入 `openDirectoryDialogAdapter`，验证默认目录、取消返回和错误脱敏：

```go
func TestChooseLocalTaskRootUsesWindowsDirectoryDialog(t *testing.T) {
	openDirectoryDialogAdapter = func(_ context.Context, options runtime.OpenDialogOptions) (string, error) {
		if options.DefaultDirectory != `D:\Media` { t.Fatalf("options=%#v", options) }
		return `D:\Media\Photos`, nil
	}
	backend := NewBackend(nil)
	backend.Startup(context.Background())
	result := backend.ChooseLocalTaskRoot(`D:\Media`)
	if !result.OK || result.Path != `D:\Media\Photos` { t.Fatalf("result=%#v", result) }
}
```

- [ ] **Step 2: 运行 NodeTray Go RED**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./nodetray ./internal/nodetray/traymodel -run 'ChooseLocalTaskRoot|PathSelectionResult'
```

Expected: FAIL，提示 DTO、适配器或 Backend 方法尚未定义。

- [ ] **Step 3: 实现 NodeTray 后端选择方法**

在 `nodetray/app.go` 增加：

```go
var openDirectoryDialogAdapter = runtime.OpenDirectoryDialog

func (b *Backend) ChooseLocalTaskRoot(currentPath string) traymodel.PathSelectionResult
```

该方法从活动 Wails context 调用 `runtime.OpenDirectoryDialog`，标题为“选择本地任务扫描目录”。只有当 `currentPath` 是现有绝对目录时才作为 `DefaultDirectory`；取消返回 `OK: true, Cancelled: true`；平台错误返回稳定 `directory_dialog_failed`，不返回原始路径。Wails 2.12 的 Windows 实现不转发 `ShowHiddenFiles`，因此 NodeTray 的隐藏项显示跟随 Windows 资源管理器设置，不为此改写自定义浏览器。不得调用任何 elevation 客户端。

- [ ] **Step 4: 为 NodeTray 前端路径规则和多选流程写失败测试**

`taskRoots.test.ts` 使用与 GUI 相同的用例和值，锁定重复、父子覆盖和 UNC 行为，但不跨包导入 GUI 源码。

`LocalTasksPage.test.tsx` 覆盖：

```tsx
it("重复打开原生窗口添加多个目录后提交", async () => {
  const choose = vi.fn()
    .mockResolvedValueOnce({ ok: true, path: "D:\\Media", cancelled: false })
    .mockResolvedValueOnce({ ok: true, path: "E:\\Photos", cancelled: false });
  const create = vi.fn(async () => ({ ok: true, task: { taskId: "t1" } }));
  render(<LocalTasksPage api={{ choose, create, list: vi.fn(async () => ({ ok: true, tasks: [] })) }} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "选择目录…" }));
  await user.click(screen.getByRole("button", { name: "选择目录…" }));
  await user.click(screen.getByRole("button", { name: "创建任务" }));
  expect(create).toHaveBeenCalledWith(expect.objectContaining({ roots: ["D:\\Media", "E:\\Photos"] }));
});
```

再覆盖取消不改变列表、文件不会由后端返回为选择结果、手工添加、移除、父目录替换确认。

- [ ] **Step 5: 实现 NodeTray 前端与动态 Wails 绑定**

`localAgent.ts` 增加：

```ts
export type PathSelectionResult = { ok: boolean; path: string; cancelled: boolean; errorCode?: string; errorSummary?: string };
export const chooseLocalTaskRoot = (currentPath: string) =>
  call<PathSelectionResult>("ChooseLocalTaskRoot", { ok: false, path: "", cancelled: false, errorCode: "backend_unavailable" }, currentPath);
```

`LocalTasksPage` 改用 `roots: string[]`、手工输入和逐项移除。原生窗口每次只返回一个目录，用户重复点击完成多选；隐藏项显示跟随 Windows 资源管理器设置。父目录覆盖已有子目录时使用可注入确认函数，测试不直接依赖 `window.confirm`。

- [ ] **Step 6: 运行 Task 4 聚焦测试和一次构建**

Run:

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./nodetray ./internal/nodetray/traymodel -run 'ChooseLocalTaskRoot|PathSelectionResult'
Set-Location nodetray/frontend
npm.cmd exec vitest run src/api/localAgent.test.ts src/pages/taskRoots.test.ts src/pages/LocalTasksPage.test.tsx
npm.cmd run build
```

Expected: 聚焦 Go/React 测试 PASS；TypeScript/Vite build PASS，并更新 `nodetray/frontend/dist`。

- [ ] **Step 7: 提交 Task 4**

```powershell
Set-Location ../..
git add -- internal/nodetray/traymodel/model.go internal/nodetray/traymodel/model_test.go nodetray/app.go nodetray/app_test.go nodetray/frontend/src/api/localAgent.ts nodetray/frontend/src/api/localAgent.test.ts nodetray/frontend/src/pages/taskRoots.ts nodetray/frontend/src/pages/taskRoots.test.ts nodetray/frontend/src/pages/LocalTasksPage.tsx nodetray/frontend/src/pages/LocalTasksPage.test.tsx nodetray/frontend/src/app.css nodetray/frontend/dist
git commit -m "feat: select local task roots in nodetray"
```

---

### Task 5: 一次性聚焦验收与交付记录

**Files:**
- Modify only if needed: `docs/acceptance/node-tray-acceptance.md`（仅记录实际人工结果，不预填 PASS）

**Interfaces:**
- Consumes: Task 1–4 完成的 Agent、GUI 和 NodeTray 路径选择链路。
- Produces: 一份明确区分自动检查与人工 Windows 结果的交付结论。

- [ ] **Step 1: 只运行一次组合聚焦检查**

不要运行全仓测试。只运行：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 ./internal/proto ./internal/agent ./internal/gui ./cmd/agent ./cmd/gui ./nodetray ./internal/nodetray/traymodel -run 'FilesystemBrowse|FilesystemBrowser|ChooseLocalTaskRoot|PathSelectionResult'
git diff --check
```

Expected: 聚焦 Go 测试 PASS；`git diff --check` PASS。前端不重复测试或构建，因为 Task 3、Task 4 已各执行一次。

- [ ] **Step 2: 做一次 Windows 人工验收，不循环扩大**

只检查两个流程：

1. NodeTray：打开本地任务，原生窗口中确认文件可见但不可选；连续添加两个目录；创建任务后 Agent 收到两个 `roots`。
2. GUI：选择在线 Agent；浏览目标 Agent 磁盘；确认目录可进入、文件不可选、隐藏开关有效；添加两个不重叠目录并创建任务。

同时各触发一次 Agent 离线或目录消失错误，确认已选根目录没有被清空。若当前环境没有第二台 Windows Agent，只把 GUI 远程实机项记录为 `PARTIAL_NOT_RUN`，不得用单元测试冒充实机 PASS。

- [ ] **Step 3: 检查提交范围并结束**

```powershell
git status --short
git log --oneline -5
```

Expected: 只存在 Task 1–4 的实现提交和经用户允许的验收记录；没有 `.tmp`、构建缓存、个人配置、令牌或运行数据库进入提交。不要为了获得干净状态删除用户文件。

本任务到此结束，不再追加全仓测试、重复前端测试或额外审查轮次。
