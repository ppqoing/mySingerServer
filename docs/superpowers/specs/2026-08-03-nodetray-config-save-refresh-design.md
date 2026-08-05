# NodeTray 配置保存与全界面刷新修复设计

## 背景

NodeTray 当前存在两个相互关联的问题：

1. 从“Helper 未启用、手动模式”切换为“Helper 已启用、手动模式”时，
   `SaveTraySettings` 会先执行删除 Helper 计划任务的提权操作。即使计划任务本来
   不存在，只要该无关操作失败或被取消，设置文件就不会写入，前端随后重新加载旧值，
   表现为“启用 Helper”自动取消勾选。
2. 程序设置保存成功后会刷新共享节点状态，但 Agent 配置和 Helper 配置保存后仅提交
   当前表单，没有统一刷新总览及其他依赖配置摘要、启用策略和重启状态的界面。

本项目为个人单窗口应用，本设计采用最小显式刷新方案，不引入新的全局配置缓存或
跨进程配置事件协议。

## 目标

- 可持久化“Helper 已启用、手动启动”状态；
- Helper 手动启用且固定计划任务不存在时，不弹出 UAC，不执行删除任务；
- 只有固定计划任务的实际状态与目标状态不一致时，才安装或删除任务；
- 程序设置、Agent 配置、Helper 配置发生实际保存后，立即刷新共享节点状态；
- 总览、Agent 页、Helper 页和程序设置相关状态在保存完成后保持一致；
- 共享状态刷新失败时不回滚已经写入的配置。

## 非目标

- 不建立完整的前端配置缓存；
- 不新增 `config-changed` 跨语言事件；
- 不自动启动 Helper；
- 不改变 Helper 手动/自动启动模式的含义；
- 不增加与本问题无关的安全校验或生命周期重构；
- 不修改 Agent、Helper 配置文件格式。

## 采用方案

### 后端：按计划任务实际状态协调策略

`SaveTraySettings` 计算 Helper 计划任务的目标状态：

- `HelperEnabled == true` 且 `HelperStartMode == automatic`：目标为已安装；
- 其他组合：目标为未安装。

只有 `HelperEnabled` 或 `HelperStartMode` 发生变化时，才通过现有 Task Service 读取
固定计划任务的实际状态；普通窗口、刷新间隔等设置变化不依赖任务服务。Helper 策略
发生变化后，只有 `actual.Installed != desiredInstalled` 时才调用提权操作：

| 设置变化 | 当前任务 | 行为 |
|---|---:|---|
| 未启用/手动 → 启用/手动 | 不存在 | 直接保存设置 |
| 手动 → 自动 | 不存在 | 提权安装任务，成功后保存 |
| 自动 → 手动 | 存在 | 提权删除任务，成功后保存 |
| 启用 → 禁用 | 存在 | 提权删除任务，成功后保存 |
| 任意策略 | 已与目标一致 | 不执行任务操作，直接保存 |

如果任务状态读取失败，返回稳定的任务错误，不写入可能与实际任务状态矛盾的策略。
如果确实需要的安装/删除失败，保持现有“不提交新设置并报告失败”的语义。

### 前端：实际保存后显式刷新共享状态

继续使用现有 `NodeStateContext.refresh()`，不新增第二套状态源。

- 程序设置：后端返回成功后提交表单，再等待共享状态刷新；
- Agent 配置：当 `ConfigApplyResult.saved == true` 时提交表单并刷新共享状态；
- Helper 配置：当 `ConfigApplyResult.saved == true` 时提交表单并刷新共享状态；
- 保存并重启 Agent 同样遵守 `saved == true` 的刷新规则。

使用 `saved` 而不是仅使用 `ok` 作为 Agent/Helper 刷新条件，因为配置可能已经成功
落盘，但后续摘要更新或重启失败。此时所有页面仍应显示新的保存摘要和“需要重启”
等实际状态。

当前页表单由现有 `form.commit(...)` 变为已保存状态；其他页面依赖的启用状态、配置
摘要、生命周期和漂移信息由共享 Overview 快照更新。页面切换后，配置表单继续按
现有逻辑重新读取后端配置。

## 数据流

```text
用户保存配置
    |
    +-- 程序设置 ----------+
    +-- Agent 配置 --------+--> 后端验证并写入
    +-- Helper 配置 -------+          |
                                      v
                              返回 ok / saved
                                      |
                         提交当前表单的已保存状态
                                      |
                                      v
                             NodeState.refresh()
                                      |
                                      v
                         GetOverview -> 共享快照更新
                                      |
                                      v
                      总览 / Agent / Helper / 设置相关状态
```

## 错误处理

- 程序设置未写入：重新读取磁盘中的实际设置，保持现有回退行为；
- Agent/Helper 返回 `saved == false`：不提交表单，不刷新共享状态；
- Agent/Helper 返回 `saved == true && ok == false`：保留已保存表单状态，刷新共享状态，
  同时继续显示后续应用失败；
- `NodeState.refresh()` 失败：配置保存结果不回滚，由共享 Store 显示读取错误；
- UAC 只在任务实际状态需要改变时出现；取消 UAC 时不写入相冲突的程序设置。

## 测试设计

### Go 服务测试

- 当前设置为 Helper 未启用/手动，目标为启用/手动，任务不存在：不调用 elevation，
  成功保存 `HelperEnabled=true`；
- 目标自动且任务不存在：调用安装任务后保存；
- 目标手动且任务存在：调用删除任务后保存；
- 实际任务状态已满足目标：不调用 elevation；
- 任务状态读取失败：返回任务错误且不保存设置。

### React 测试

- 程序设置保存成功后调用共享刷新，并保留启用 Helper 勾选；
- Agent 普通保存和保存并重启在 `saved=true` 后调用共享刷新；
- Helper 保存在 `saved=true` 后调用共享刷新；
- `saved=false` 时不刷新；
- `saved=true && ok=false` 时仍刷新，同时保留错误提示。

### 回归门禁

- `go test -count=1 ./internal/nodetray/app`；
- `go test -count=1 ./internal/nodetray/... ./nodetray`；
- NodeTray 前端定向 Vitest；
- NodeTray 前端完整 Vitest、ESLint、TypeScript 和 Vite build；
- Windows x64 NodeTray 构建。

## 验收标准

1. 勾选“启用 Helper”、保持“手动”、保存后勾选不回退；
2. 上述场景不因不存在的计划任务触发 UAC；
3. 返回总览后 Helper 立即显示“已启用”，启动按钮按停止态可用；
4. 保存 Agent 或 Helper 配置后，总览中的保存摘要和需要重启状态立即更新；
5. 保存已落盘但后续应用失败时，界面同时反映新保存状态和失败信息；
6. 现有自动模式任务安装、手动/禁用任务删除和 UAC 取消语义不回归；
7. 未执行真实 Windows GUI 验收时明确记录为 `BLOCKED_NOT_RUN_DYNAMIC`。

## 版本边界

源码目录没有 `.git` 元数据，设计文档状态为 `N/A_NO_GIT_METADATA`，不初始化或伪造
Git 仓库。实现完成后生成独立 NodeTray 构建产物；是否替换真实安装目录仍按单独操作
处理。
