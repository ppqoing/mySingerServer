# Image 2 图标资产清单

- 生成方式：Codex 内置 `image_gen`（GPT Image 2），每个语义单独调用。
- 生成日期：2026-08-20；`add`、`preview` 于同日按独立审查结论重新生成并目视确认单枚语义。
- 原始输出保存位置：`C:\tmp\rust-v2-visual-fidelity-target\image2-icons\raw`，不进入 Git。
- 后处理：Alpha 包围盒最小裁剪、Lanczos3 等比缩放、整数像素质心定位、非透明像素 RGB 归一为纯黑；不拉伸语义轮廓。
- 最终几何：导航画布 20×20，最长边优先 18px、必要时回退 17px；行内画布 16×16，最长边优先 14px、质心无法满足时依次回退 13px/12px；四边至少 1px 透明，Alpha 质心两轴偏差不超过 0.5px。

## 统一母提示词

```text
Create a coherent Windows desktop utility icon family. Pure black monochrome line icons on a fully transparent background, straight-on orthographic view, Fluent-inspired rounded geometry, no fill illustration, no shadow, no gradient, no texture, no lettering, no decorative dots. Every icon must remain continuous and recognizable when reduced to 16–20 pixels. Use one consistent visual stroke weight equivalent to 1.75–2 pixels at final size. Center the visual mass on the exact geometric center and leave even optical margins.
```

## 语义资产

每轮调用只在母提示词后追加表中的单一语义尾句。

| 最终文件 | 语义尾句 | 选用的内置原始输出名 | 最终画布 | 实际使用位置 |
|---|---|---|---:|---|
| `app.png` | `paired media files inside a compact rounded archive mark` | `exec-a2dd7263-58a7-4e18-a154-5ac1d6f7bef1.png` | 20×20 | 应用品牌与导航品牌母图 |
| `menu.png` | `three balanced horizontal menu lines` | `exec-4a2493dc-dc26-4fda-8668-02bc8b09a69c.png` | 20×20 | 侧边导航展开/收起入口 |
| `overview.png` | `four-cell dashboard` | `exec-60a88c66-a449-454d-af2d-6bb4b7128791.png` | 20×20 | 总览导航项 |
| `nodes.png` | `three connected computer nodes` | `exec-603b2bf2-e3ee-41ce-a6f0-3ff20000db42.png` | 20×20 | 节点导航项 |
| `scan.png` | `document with scanning corner` | `exec-77709943-63ea-4613-b477-a0fb0bc533ff.png` | 20×20 | 扫描导航项 |
| `tasks.png` | `checklist with two lines` | `exec-58110229-40dc-426a-a82d-89ce867ad726.png` | 20×20 | 任务导航项 |
| `duplicates.png` | `two overlapping files` | `exec-f482f9c7-e7c6-4202-b907-5f6d8d65478c.png` | 20×20 | 重复文件导航项 |
| `review-delete.png` | `reviewed file with restrained delete mark` | `exec-45851483-d083-440e-ba95-69b03fcfa3de.png` | 20×20 | 审核删除导航项 |
| `settings.png` | `simple six-tooth gear` | `exec-7988c8d3-f8df-4422-b282-f8e90859c91e.png` | 20×20 | 设置导航项 |
| `index.png` | `compact indexed database stack` | `exec-95ae76be-5b33-4b63-818a-555be712d958.png` | 20×20 | 中心索引状态 |
| `sync.png` | `two balanced opposing arrows` | `exec-d65630a8-cf61-4369-a4e1-bb07aa98c526.png` | 20×20 | 节点同步状态与动作 |
| `search.png` | `magnifying glass` | `exec-06690bfc-188c-4c7b-b14c-9f83ab66c0f3.png` | 16×16 | 搜索输入与搜索动作 |
| `refresh.png` | `one clockwise circular refresh arrow` | `exec-9d7bcbe2-3e59-4d32-94d2-89839d6aa16b.png` | 16×16 | 顶部命令栏刷新动作 |
| `add.png` | `plus inside a compact circle` | `exec-8028193c-f40d-4c45-bf3d-f2ee20f1e228.png` | 16×16 | 新增节点/扫描根动作 |
| `edit.png` | `simple pencil` | `exec-70c9fc76-a540-4bb2-bd97-57c45957a12b.png` | 16×16 | 编辑节点与设置动作 |
| `remove.png` | `minus inside a compact circle` | `exec-3e30e82c-3f40-4355-a916-eba1452bc3b8.png` | 16×16 | 移除配置项动作 |
| `connect.png` | `link between two endpoints` | `exec-26210c2a-f473-4d86-879f-a154c9c1f1f1.png` | 16×16 | 节点连接动作 |
| `browse.png` | `open folder` | `exec-1cced2f9-2be0-4c7f-b19e-100802eefc03.png` | 16×16 | 路径浏览动作 |
| `folder.png` | `closed folder` | `exec-f19c76ca-c1b9-4eb3-91c0-bb48384262fc.png` | 16×16 | 文件夹/扫描根条目 |
| `info.png` | `lowercase information mark inside circle` | `exec-81e794b1-ce72-4cb7-ad2f-f607387d1eb1.png` | 16×16 | 信息与诊断提示 |
| `cancel.png` | `stop square inside circle` | `exec-943a86e4-247b-49b6-be9d-55cef13974f0.png` | 16×16 | 取消任务动作 |
| `filter.png` | `funnel` | `exec-1b75000e-cb29-42db-8918-478307b3f4bc.png` | 16×16 | 结果筛选动作 |
| `preview.png` | `eye` | `exec-a48dfcd4-7a1c-4362-84a1-39afad67d60b.png` | 16×16 | 媒体预览动作 |
| `retry.png` | `compact clockwise retry arrow` | `exec-a3bce2a5-04d4-42fa-85c9-0498bbcd2709.png` | 16×16 | 任务/二筛重试动作 |
| `keep.png` | `shield check` | `exec-9f3954cf-efc7-4f27-ab6f-e2ea84226b47.png` | 16×16 | 保留复核决定 |
| `delete.png` | `restrained trash can` | `exec-f3028f77-5438-4236-a021-029280442e78.png` | 16×16 | 删除复核决定与删除动作 |
| `save.png` | `compact save disk` | `exec-82d148c0-5876-4434-94af-560d4df18fd3.png` | 16×16 | 保存设置动作 |

## 品牌派生资产

以下文件均由最终选用的 `app` 原始输出通过同一 Alpha 裁剪、等比缩放和质心定位生成。

| 最终文件 | 原始输出名 | 最终画布 | 实际使用位置 |
|---|---|---:|---|
| `app-16.png` | `exec-a2dd7263-58a7-4e18-a154-5ac1d6f7bef1.png` | 16×16 | Windows 小图标规格 |
| `app-24.png` | `exec-a2dd7263-58a7-4e18-a154-5ac1d6f7bef1.png` | 24×24 | Windows 紧凑图标规格 |
| `app-32.png` | `exec-a2dd7263-58a7-4e18-a154-5ac1d6f7bef1.png` | 32×32 | Windows 标准图标规格 |
| `app-48.png` | `exec-a2dd7263-58a7-4e18-a154-5ac1d6f7bef1.png` | 48×48 | Windows 大图标规格 |
| `app-256.png` | `exec-a2dd7263-58a7-4e18-a154-5ac1d6f7bef1.png` | 256×256 | Slint `MainWindow.icon` |
| `app.ico` | `app-256.png` 经 `IcoEncoder` 编码 | 256×256 ICO | `desktop.exe` Windows PE 图标资源 |
