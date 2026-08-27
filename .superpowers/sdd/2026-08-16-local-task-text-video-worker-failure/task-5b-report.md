# Task 5B 远程目录浏览会话状态与 Web lint 修复报告

- 执行日期：2026-08-16
- 工作树：`D:\code\mySingerServer\.worktrees\local-task-lifecycle-controls`
- 基线 HEAD：`db0a7f26629ba51637733f2948556dbb7394a670`
- 提交：本报告与代码同一提交，最终 SHA 见 Git 记录与交付消息

## 1. 修复结果

`RemotePathBrowser` 现在把跨开关保留的 `showHidden` 偏好放在外层组件，把路径、选择、目录条目、游标、loading、error 与请求控制器放在按 `open/machineID` 挂载的内部浏览会话中。关闭对话框会卸载内部会话；重开或切换机器会从空状态同步创建新会话，不再等待根目录请求完成后才清理旧目录。

初始 effect 只启动一次根目录 API 请求并在 cleanup 中 abort，不再同步调用 state setter。会话内切换隐藏项仍显式刷新当前目录一次；初始隐藏项使用挂载时快照，因此不会因偏好更新额外触发根请求。原有 AbortSignal、`signal.aborted` 检查和 controller identity 判断均保留。

## 2. TDD RED 证据

先在 `RemotePathBrowser.test.tsx` 加入四个确定性边界，再修改生产实现：

1. 关闭重开且新根请求 pending 时，旧路径、旧条目与旧提交能力必须立即消失。
2. 切换隐藏项只能增加一个当前目录请求。
3. 被 B 取消的 A 即使迟到完成，也不能覆盖 B 或提前结束 B 的 loading。
4. `showHidden=true` 偏好跨重开保留，且新会话只发一次携带 true 的根请求。

聚焦 RED：`npm --prefix webui test -- --run src/features/scans/RemotePathBrowser.test.tsx`，exit 1，9 项中 1 项按预期失败；失败断言显示关闭重开后旧条目 `Old` 仍在 DOM。其余三个边界通过，锁定现有单请求与迟到请求保护语义。

lint RED：`npm --prefix webui run lint`，exit 1；`RemotePathBrowser.tsx:56` 报 `react-hooks/set-state-in-effect`，并有缺失 `showHidden` 依赖 warning。

## 3. GREEN 与验证证据

| 门禁 | 结果 |
|---|---|
| 聚焦测试 `npm --prefix webui test -- --run src/features/scans/RemotePathBrowser.test.tsx` | PASS，1/1 文件，9/9 tests |
| Web 全量 `npm --prefix webui test -- --run` | PASS，17/17 文件，209/209 tests |
| Web lint `npm --prefix webui run lint` | PASS，exit 0，0 error、0 warning |
| Web build `npm --prefix webui run build` | PASS，TypeScript 与 Vite build exit 0 |
| 构建输出恢复 | 已精确恢复本轮改写的 4 个 tracked `internal/gui/web` 文件，并删除本轮唯一新增的 hash JS；源码白名单外无残留 |
| `git diff --check` | PASS，exit 0 |

## 4. 范围与剩余风险

- 生产代码与测试只修改 `RemotePathBrowser.tsx` 和 `RemotePathBrowser.test.tsx`；未修改 ESLint 配置，未添加 lint disable、sleep、setTimeout 或额外请求。
- 本 Task 未执行真实浏览器手工交互、远程 Agent 文件系统、发布打包或部署验收；这些运行边界不冒充 PASS。
- API fake 会忽略 abort 并允许迟到完成，用于确定性验证 identity 防护；真实网络取消效果仍由现有 AbortSignal 契约承担。
