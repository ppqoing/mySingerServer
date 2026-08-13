# 便携双 ZIP 默认发布路径实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 未传 `-OutputDir` 时，将 Compute 与 Manager 双 ZIP 发布到 `D:\code\mySingerServer\publish`。

**Architecture:** 保持现有 `OutputDir` 参数和解析流程，只替换参数默认值。合同测试静态验证默认值，同时继续用显式临时目录执行完整动态发布合同，避免测试污染真实发布目录。

**Tech Stack:** PowerShell 7、Git、现有便携发布合同脚本。

## Global Constraints

- 默认发布目录精确为 `D:\code\mySingerServer\publish`。
- 显式 `-OutputDir` 继续覆盖默认值。
- 不改变文件名、包内容、候选目录、原子发布、冲突拒绝或回滚逻辑。
- 测试不得向真实默认发布目录写文件。

---

### Task 1: 修改默认发布目录并验证合同

**Files:**
- Modify: `scripts/test-package-portable-release.ps1`
- Modify: `scripts/package-portable-release.ps1`
- Modify: `README.md`

**Interfaces:**
- Consumes: `package-portable-release.ps1 -StageDir <path> [-OutputDir <path>]`
- Produces: 未提供 `-OutputDir` 时使用 `D:\code\mySingerServer\publish`；显式值保持原行为。

- [ ] **Step 1: 写默认路径失败合同**

在 `scripts/test-package-portable-release.ps1` 读取发布脚本文本并断言参数默认值：

```powershell
$packageSource = Get-Content -Raw -LiteralPath $packageScript
Assert-True ($packageSource -match "(?m)\\[string\\]\\`$OutputDir\\s*=\\s*'D:\\\\code\\\\mySingerServer\\\\publish'") `
    'portable release default output directory is not D:\code\mySingerServer\publish'
```

- [ ] **Step 2: 运行合同并确认 RED**

Run: `pwsh -NoProfile -File scripts/test-package-portable-release.ps1`

Expected: FAIL，错误包含 `portable release default output directory is not D:\code\mySingerServer\publish`。

- [ ] **Step 3: 最小修改生产脚本和 README**

将 `scripts/package-portable-release.ps1` 参数改为：

```powershell
[string]$OutputDir = 'D:\code\mySingerServer\publish',
```

README 标准命令不再显式传旧的 `-OutputDir .\artifacts\releases`，并说明默认输出目录。

- [ ] **Step 4: 运行 GREEN 和静态检查**

Run: `pwsh -NoProfile -File scripts/test-package-portable-release.ps1`

Expected: `PORTABLE RELEASE PACKAGE CONTRACT PASS`。

Run: `git diff --check`

Expected: PASS。

- [ ] **Step 5: 精确提交**

```powershell
git add -- scripts/test-package-portable-release.ps1 scripts/package-portable-release.ps1 README.md docs/superpowers/plans/2026-08-13-portable-release-publish-path.md
git diff --cached --check
git commit -m "build: publish portable zips to project output"
```
