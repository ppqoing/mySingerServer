# Task 1：PostgreSQL 容器 LAN 地址绑定

## 实现

- `New-RustV2PostgresContainer.ps1` 新增 `HostAddress`，默认 `127.0.0.1`。
- 使用 `System.Net.IPAddress.TryParse`，仅接受 IPv4/IPv6 明确地址，并拒绝 IPv4/IPv6 通配地址；主机名、通配符和带 Docker 参数的值在调用 Docker 前拒绝。
- Docker `--publish` 使用规范化地址和已校验端口；IPv6 使用 Docker 所需的方括号形式。
- 输出连接串使用实际绑定地址；同名容器/卷拒绝和 schema 3/22 表校验保持不变。
- 部署文档补充可信 LAN、防火墙最小范围和密码脱敏要求。

## TDD 证据

1. RED：先在替身行为测试中加入显式 LAN 地址场景。基线运行：
   `pwsh -NoProfile -File tests\windows\Test-RustV2PostgresContainer.ps1`
   失败：`A parameter cannot be found that matches parameter name 'HostAddress'.`
2. GREEN：完成最小脚本实现并加入非法地址行为断言；同一测试命令输出：
   `RUST_V2_POSTGRES_CONTAINER_TEST_PASS`
3. 测试替身记录每次 Docker argv，断言默认 loopback、显式 `192.168.1.17`、IPv4/IPv6 校验和非法值零 Docker 调用。

## 验证

- 聚焦 PowerShell 行为测试通过。
- `git diff --check` 通过。

## 审查修复 Round 1

审查发现原校验仍允许 IPv4 广播/多播、IPv6 多播和带 scope 的 IPv6，且测试缺少成功 IPv6 发布场景。

### RED

先加入 `::1` 的 Docker argv/连接串断言，以及 `255.255.255.255`、`224.0.0.1`、`::`、`ff02::1`、
`fe80::1%12` 等非法值的零调用断言。使用旧逻辑运行：

```text
pwsh -NoProfile -File tests\windows\Test-RustV2PostgresContainer.ps1
Exception: ...:119
非法主机地址必须拒绝：255.255.255.255
exit=1
```

### GREEN

生产校验现在拒绝 IPv4 广播、IPv4 多播、IPv6 多播和非零 IPv6 `ScopeId`；IPv6 发布和连接串使用
`[::1]` 方括号格式。修复后覆盖测试完整输出：

```text
pwsh -NoProfile -File tests\windows\Test-RustV2PostgresContainer.ps1
RUST_V2_POSTGRES_CONTAINER_TEST_PASS
exit=0
```

并执行：

```text
git diff --check
exit=0
```
