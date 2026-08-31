# Task 3：双实体机 GUI 验收编排与裁决工具

## 范围

新增三个 PowerShell 文件：编排器仅通过显式 `Provider` 调用 Docker、SSH、进程、采样器和防火墙边界；报告器独立给出 Infra、Runtime、DiskSchedule、Sync、CrossAnalysis、GUI、MediaIntegrity 七个门禁。普通测试没有连接 Node、Docker、SSH 或创建防火墙/远程目录。

## RED

在基线 `40488780be2ff777899bc60aa41fdf0ee87202c8` 上先新增行为测试并运行：

```text
编排脚本缺失：...\tests\windows\Invoke-RustV2PhysicalTwoHostGuiAcceptance.ps1
exit=1
```

失败原因是待测入口不存在，不是环境或替身故障。

## GREEN

`Test-RustV2PhysicalTwoHostGuiAcceptance.ps1` 以 fake provider 记录实际 operation/参数，验证：同 ZIP 两端 SHA、固定四根到预期磁盘、不同 MachineId、每机一个双根 completed 任务、desktop GUI 退出后才运行观察器、媒体清单一致；同时覆盖 I:\Tool/媒体根子目录、非本轮对象和重复 MachineId 的拒绝，危险输入在首个外部写入前停止。

报告器要求完整截图、交互、唯一管理端、调度、同步、跨机分析和媒体证据；任一缺失输出 `INCONCLUSIVE`，不会生成 GUI PASS。

## 验证

```text
pwsh -NoProfile -File tests\windows\Test-RustV2PhysicalTwoHostGuiAcceptance.ps1
RUST_V2_PHYSICAL_TWO_HOST_GUI_ACCEPTANCE_TEST_PASS
exit=0
```

```text
pwsh -NoProfile -File tests\windows\Test-RustV2PostgresContainer.ps1
RUST_V2_POSTGRES_CONTAINER_TEST_PASS
exit=0

git diff --check
exit=0
```

## Concern

该三文件范围提供可测试的安全编排内核，真实 Docker/SSH/进程/性能 provider 必须由后续已授权的实体机执行入口显式绑定；本任务未猜测未给出的远程 shell、候选包解压和 TOML 生成契约，也没有执行任何实体机操作。
