# 便携双 ZIP 默认发布路径设计

## 目标

将 `scripts/package-portable-release.ps1` 的默认发布目录从仓库内的
`artifacts\releases` 改为固定路径 `D:\code\mySingerServer\publish`。

## 行为

- 调用脚本时未传 `-OutputDir`，Compute 与 Manager ZIP 及各自的
  `.sha256` 文件发布到 `D:\code\mySingerServer\publish`。
- 显式传入 `-OutputDir` 时继续使用调用方指定的目录，保持现有测试、自动化和
  自定义发布流程兼容。
- 不改变 ZIP 文件名、包内容、候选目录、原子发布、冲突拒绝或回滚逻辑。
- README 的标准发布示例使用默认目录，不再传旧的
  `-OutputDir .\artifacts\releases`。

## 验证

- 合同测试静态读取脚本，断言默认值精确为
  `D:\code\mySingerServer\publish`。
- 现有动态合同继续显式传入测试临时目录，确保测试不会写入真实发布目录。
- 运行便携发布合同、`git diff --check`，并核对只修改规格、脚本、合同测试和
  README。
