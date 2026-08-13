## Task 3 报告：GUI 远程目录弹窗与多根目录列表

### 范围

- 增加 Agent 文件系统浏览 API 的严格 snake_case 解码与请求编码。
- 增加纯字符串 Windows/UNC 根目录规范化、大小写无关去重和父子目录覆盖决策。
- 增加远程目录弹窗：磁盘入口、面包屑、目录导航、文件禁用、隐藏/系统开关、分页、错误保留选择和请求取消。
- 将扫描表单改为多根目录列表与逐个手工绝对路径输入；切换 Agent 清空草稿，父目录覆盖子目录前确认。

### TDD 证据

RED 1：

```powershell
Set-Location webui
npm.cmd exec vitest run src/api/appApi.test.ts src/features/scans/taskRoots.test.ts
```

结果：失败，`browseAgentFilesystem is not a function`，且 `taskRoots` 模块尚不存在；这是新增 API 与路径规则缺失导致的预期失败。

RED 2：增加真实组件行为测试后运行四个定向文件，失败原因是 `RemotePathBrowser` 尚不存在、`ScansPage` 尚为旧的竖线文本输入流程；这是新增弹窗及多根目录 UI 缺失导致的预期失败。

GREEN：

```powershell
Set-Location webui
npm.cmd exec vitest run src/api/appApi.test.ts src/features/scans/taskRoots.test.ts src/features/scans/RemotePathBrowser.test.tsx src/features/scans/ScansPage.test.tsx
```

结果：4 个测试文件、47 项测试全部通过。

新增/修改测试包括：

- `browses an encoded Agent filesystem path exclusively through the JSON body`
- `rejects malformed filesystem entries instead of accepting unknown kinds or omitted selection`
- `addTaskRoot` 的重复、覆盖、替换和相对路径拒绝行为
- `shows drive entries and navigates directories through breadcrumbs`
- `disables files while allowing a selected directory to be added`
- `reloads hidden entries and appends the next page`
- `keeps the selected directory after a browse error`
- `aborts the active browse request when the dialog closes`
- `browses the selected Agent and submits multiple roots`
- 未选/离线 Agent 禁用浏览、切换 Agent 清空草稿、父目录替换确认。

### 构建证据

首次 `npm.cmd run build` 在 `tsc --noEmit` 报 `RemotePathBrowser.tsx(22,22): TS2554`：React 19 类型要求 `useRef` 显式初始值。修复为 `useRef<AbortController | undefined>(undefined)` 后，经父任务授权重试一次：

```powershell
npm.cmd run build
```

结果：通过。Vite 转换 57 个模块，生成 `internal/gui/web/assets/main-CNNLWO0N.js`（350.46 kB）和 `main-Wafl2AZ_.css`（20.04 kB），并更新两个 HTML 入口。

### 自审

- 浏览路径仅在 POST JSON body 内；machine ID 使用 `encodeURIComponent` 作为 URL segment。
- 响应中的未知 `kind`、缺失 `selectable` 与其他必需字段均被严格拒绝。
- 前端不读取远程文件，不新增认证/UAC；所有浏览请求均使用新 `AbortController`，切页或关闭时取消旧请求。
- `git diff --check` 对 Vite 生成的 `internal/gui/web/*.html` 报告 CRLF 行尾为 trailing whitespace；这是生成器产物的换行格式，未手工改写生成文件。
- 未执行全量前端测试、GUI 真实设备/真实 Agent 运行时验收；本任务仅完成定向自动化测试和 Vite 静态构建验证。
