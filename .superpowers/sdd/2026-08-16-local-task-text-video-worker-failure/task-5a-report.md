# Task 5A：VideoCore 正式构建供应链门禁修复报告

- 执行日期：2026-08-16
- 工作树：`D:\code\mySingerServer\.worktrees\local-task-lifecycle-controls`
- 基线 HEAD：`d8a7ff89d50f26bf6829829a75632d729ef9c49f`
- 结论：**PASS**
- 范围：只修复 Task 5A；未执行 Task 5 打包、部署或运行验收。

## 1. RED 复现

在修改前运行：

```powershell
ctest --test-dir videocore\build -C Release `
  -R '^(videocore_image_object_provenance|videocore_image_object_provenance_mutation|videocore_level_b_legacy_artifact)$' `
  -V --output-on-failure
```

结果为 0/3，CTest exit 1：

| 测试 | 实际失败 | actual / expected |
|---|---|---|
| #14 provenance | `downscaling compiler command compile external include set mismatch` | TLog 的 11/11 对象都只有 `C:\vcpkg\installed\x64-windows-static\include` 与 `include\webp`；旧 expected 是 `videocore\build\vcpkg_installed\x64-windows-static` 下的两个路径。 |
| #15 mutation | clean copied baseline 被拒绝 | 在进入恶意变异前级联命中与 #14 相同的 external include mismatch。 |
| #17 Level-B artifact | `external manifest hash mismatch` | 工作树原 SHA 为 `D394579E...`、7802 bytes；expected 是既有 pin `9A552825...`、capture 同构的 7801 bytes。 |

根因与 Task 5 的只读诊断一致：标准 vcpkg 目录迁移后 provenance expected 未同步；`manifest.json text eol=crlf` 又把 capture 结尾裸 LF 改写成 CRLF。未发现第三个 external include，也未发现 golden、pin 或语义内容漂移。

## 2. 最小修复

1. CMake 对 `VCPKG_INSTALLED_DIR` 和 `VCPKG_TARGET_TRIPLET` fail closed，并把解析后的两个值显式传给 provenance 与 mutation。
2. provenance 从同一受控身份构造 vcpkg triplet 根目录：external include 集合仍严格等于 `include` 与 `include\webp` 两项；五个 vcpkg link library 也绑定到同一标准 triplet 的 `lib`，没有从 TLog 动态扩充 allowlist。
3. mutation 透传同一配置身份，并新增 `ExtraExternalInclude`，验证额外 `/external:I` 仍以 `compile external include set mismatch` 被拒绝；其余恶意变异保持不变。
4. `manifest.json` 属性改为 `-text`；按 capture 的 `ConvertTo-Json -Depth 8`、UTF-8 无 BOM、末尾单个 LF 机械重生成。JSON 语义与基线相同。

## 3. GREEN 证据

### focused

重新 configure 后运行 #14/#15/#17/#18：4/4 PASS，总用时 64.36s。

- #14：11 个对象重编译逐字节一致，DLL/EXE 重链接逐字节一致。
- #15：clean baseline 为 GREEN；forced include、response file、普通 extra include、新增 extra external include、extra input/output、错误 pathmap、TLog/PE/object 伪造与 tool shadow 等负例均未被接受。
- #17：20 行、9 个批准差异、artifact/input/result/golden 链全部通过。
- #18：`tenth_delta=RED`、`self_resign=RED`。

### 构建与完整 CTest

```powershell
cmake -S videocore -B videocore\build
cmake --build videocore\build --config Release
ctest --test-dir videocore\build -C Release --output-on-failure
```

- configure：exit 0。
- Release build：沙箱内首次被 MSBuild FileTracker `E_ACCESSDENIED` 阻断；同一命令在沙箱外复跑 exit 0，全部 VideoCore/test targets 构建成功。该异常是执行沙箱权限边界，不是产品构建错误。
- 完整 CTest：**18/18 PASS**，0 failure，总用时 70.81s。

## 4. 固定证据与范围核验

- `manifest.json`：`9A552825A4CF493A1407063CD3702176AF584D3D728780F476A1EB3CAA9EE1E3`，7801 bytes。
- `legacy-golden.tsv`：`95E019F0ADB796DBEA76AB341D5F76417A5653D444804626593CACFFC2D01517`，1728 bytes。
- CMake manifest/golden 两个独立 pin 未修改。
- `.gitattributes` 对 manifest 显示 `text: unset`，冻结字节不再被 checkout 行尾转换。
- `git -c core.whitespace=cr-at-eol diff --check`：PASS；`cr-at-eol` 只让 Git 正确认识冻结文件中 capture 产生的 CRLF，不改变文件字节或测试。
- 修改范围仅含 Task 5A 白名单文件与本报告。

## 5. 风险与边界

- manifest 字节与 PowerShell capture 规范绑定；后续不得使用通用 JSON formatter 或 Git EOL 归一化改写。
- provenance 只接受 CMake 显式配置的单一 vcpkg installed dir/triplet，配置缺失、空值、非绝对 installed dir 或带路径分隔符的 triplet 都 fail closed。
- 本报告只证明当前 Windows x64 Release 构建与 VideoCore CTest 18/18；Task 5 的包、SHA、部署、真实设备/媒体运行验收均未执行，也不在 Task 5A 范围内。
