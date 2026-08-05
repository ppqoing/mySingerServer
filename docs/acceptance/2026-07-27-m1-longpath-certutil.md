# Task 1 — Real Windows path longer than 260 characters

> Archived M1 local acceptance evidence.

## Final status

`DONE`

## Scope

- Production source and permanent tests were not modified.
- Validation used the repository's real `internal/enum.WalkerEnumerator`,
  `internal/agent.GoHasher`, and resilient enumerator through the task verifier.
- Task-owned validation root:
  `D:\code\mySingerServer\.tmp\m1-acceptance-longpath`

## Validation command

Run from `D:\code\mySingerServer`:

```powershell
$env:M1_EVERYTHING_DLL='D:\code\mySingerServer\third_party\everything_sdk\Everything64.dll'
$env:GOCACHE='D:\code\mySingerServer\.tmp\gocache'
$env:GOMODCACHE='D:\code\mySingerServer\.tmp\gomodcache'
& 'C:\Users\Administrator\AppData\Local\Temp\go1.26.5-portable\go\bin\go.exe' run '.\.tmp\m1-acceptance-longpath\main.go'
$validationExit=$LASTEXITCODE
Write-Output "validation_exit_code=$validationExit"
exit $validationExit
```

## Validation evidence

Exit code: `0`

```text
target_path=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\unicode-目录-文件.txt
target_utf16_length=437
walker_record_path=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\unicode-目录-文件.txt
walker_record_count=1
sha512_expected=5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a
sha512_actual=5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a
everything_available_error=<nil>
resilient_branch=walker_fallback
resilient_fallback_cause=enumerator: primary returned no results for root
resilient_record_path=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\unicode-目录-文件.txt
resilient_record_count=1
validation_exit_code=0
```

## Acceptance assessment

- Target path length was `437` UTF-16 code units, strictly greater than 260.
- The path contains Unicode in `unicode-目录-文件.txt`.
- Walker returned exactly one record and the target path was clean: it had no
  leaked `\\?\` prefix.
- Expected SHA-512:
  `5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a`
- Actual SHA-512:
  `5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a`
- Everything availability returned `<nil>`, but the current index returned no
  result for the newly created path. The resilient enumerator therefore used
  the accepted `walker_fallback` branch and returned the same clean target path.

## Cleanup command

```powershell
$taskRoot=[System.IO.Path]::GetFullPath('D:\code\mySingerServer\.tmp\m1-acceptance-longpath')
$treeTarget=[System.IO.Path]::GetFullPath((Join-Path -Path $taskRoot -ChildPath 'tree'))
$verifierTarget=[System.IO.Path]::GetFullPath((Join-Path -Path $taskRoot -ChildPath 'main.go'))
$expectedTree='D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree'
$expectedVerifier='D:\code\mySingerServer\.tmp\m1-acceptance-longpath\main.go'
Write-Output "resolved_task_root=$taskRoot"
Write-Output "resolved_tree_target=$treeTarget"
Write-Output "resolved_verifier_target=$verifierTarget"
if (-not [string]::Equals($treeTarget,$expectedTree,[System.StringComparison]::OrdinalIgnoreCase)) { throw "Refusing tree cleanup: resolved target mismatch" }
if (-not [string]::Equals($verifierTarget,$expectedVerifier,[System.StringComparison]::OrdinalIgnoreCase)) { throw "Refusing verifier cleanup: resolved target mismatch" }
if (-not $treeTarget.StartsWith($taskRoot + [System.IO.Path]::DirectorySeparatorChar,[System.StringComparison]::OrdinalIgnoreCase)) { throw "Refusing tree cleanup: outside task root" }
if (-not $verifierTarget.StartsWith($taskRoot + [System.IO.Path]::DirectorySeparatorChar,[System.StringComparison]::OrdinalIgnoreCase)) { throw "Refusing verifier cleanup: outside task root" }
Remove-Item -LiteralPath $treeTarget -Recurse -Force
Remove-Item -LiteralPath $verifierTarget -Force
Write-Output "tree_exists_after_cleanup=$(Test-Path -LiteralPath $treeTarget)"
Write-Output "verifier_exists_after_cleanup=$(Test-Path -LiteralPath $verifierTarget)"
Write-Output "task_root_exists_after_cleanup=$(Test-Path -LiteralPath $taskRoot)"
```

## Cleanup evidence

Exit code: `0`

```text
resolved_task_root=D:\code\mySingerServer\.tmp\m1-acceptance-longpath
resolved_tree_target=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree
resolved_verifier_target=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\main.go
tree_exists_after_cleanup=False
verifier_exists_after_cleanup=False
task_root_exists_after_cleanup=True
```

The cleanup removed only the validated task-owned `tree` and `main.go`
targets. The task root remains in place.

---

## Fix round 1 — independent digest and call-level evidence

Final status after fix round: `DONE`

This round recreated and reran only the task-owned verifier. Production source
and permanent tests were not modified.

### Verifier and creation evidence

Temporary verifier source used to create and validate the tree:

`D:\code\mySingerServer\.tmp\m1-acceptance-longpath\main.go`

The complete verifier source was preserved before cleanup at:

`D:\code\mySingerServer\docs\acceptance\evidence\m1-longpath-verifier.go.txt`

The temporary and preserved verifier files were byte-identical:

```text
temp_verifier_sha256=180d6e9f6348c6680a4c9075062c8230041dd6565f7a1f1b5f0edff6c76d1063
preserved_verifier_sha256=180d6e9f6348c6680a4c9075062c8230041dd6565f7a1f1b5f0edff6c76d1063
verifier_sources_identical=True
```

The exact command that ran the verifier and thereby created the directory tree
and target file was:

```powershell
$env:M1_EVERYTHING_DLL='D:\code\mySingerServer\third_party\everything_sdk\Everything64.dll'
$env:GOCACHE='D:\code\mySingerServer\.tmp\gocache'
$env:GOMODCACHE='D:\code\mySingerServer\.tmp\gomodcache'
& 'C:\Users\Administrator\AppData\Local\Temp\go1.26.5-portable\go\bin\go.exe' run '.\.tmp\m1-acceptance-longpath\main.go'
$validationExit=$LASTEXITCODE
Write-Output "validation_exit_code=$validationExit"
exit $validationExit
```

Within the preserved source, target creation is performed by the real
filesystem calls:

```go
if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
	fatalf("MkdirAll: %v", err)
}
if err := os.WriteFile(target, []byte(literalContents), 0o600); err != nil {
	fatalf("WriteFile: %v", err)
}
```

The exact literal was:

```text
Go-quoted text: "M1 long-path acceptance literal contents\n"
UTF-8 bytes (hex): 4d31206c6f6e672d7061746820616363657074616e6365206c69746572616c20636f6e74656e74730a
Byte count: 41
```

### Exact repository package calls

The preserved verifier imports the repository packages:

```go
import (
	"dedup/internal/agent"
	fileenum "dedup/internal/enum"
)
```

It invokes the real implementations directly:

```go
(fileenum.WalkerEnumerator{}).Enum(...)
(agent.GoHasher{}).HashFile(absoluteTarget)
fileenum.NewEverythingEnumeratorAt(dll)
fileenum.NewResilientEnumerator(everything, fileenum.WalkerEnumerator{}, ...)
resilient.Enum(...)
```

No Walker, hashing, Everything, or resilient-enumerator logic was copied into
the verifier.

### Fresh verifier output

Exit code: `0`

```text
literal_contents_go_quoted="M1 long-path acceptance literal contents\n"
literal_contents_utf8_hex=4d31206c6f6e672d7061746820616363657074616e6365206c69746572616c20636f6e74656e74730a
literal_contents_byte_count=41
target_path=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\unicode-目录-文件.txt
target_utf16_length=437
walker_record_path=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\unicode-目录-文件.txt
walker_record_count=1
sha512_expected=5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a
sha512_actual=5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a
everything_available_error=<nil>
resilient_branch=walker_fallback
resilient_fallback_cause=enumerator: primary returned no results for root
resilient_record_path=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\nested-segment-0123456789\unicode-目录-文件.txt
resilient_record_count=1
validation_exit_code=0
```

### Independent Windows certutil SHA-512

Before cleanup, Windows `certutil` was invoked on the same target using the
long-path-safe `\\?\` prefix:

```powershell
$taskRoot=[System.IO.Path]::GetFullPath('D:\code\mySingerServer\.tmp\m1-acceptance-longpath')
$targetDir=Join-Path -Path $taskRoot -ChildPath 'tree'
for ($i=0; $i -lt 14; $i++) {
    $targetDir=Join-Path -Path $targetDir -ChildPath 'nested-segment-0123456789'
}
$target=[System.IO.Path]::GetFullPath((Join-Path -Path $targetDir -ChildPath 'unicode-目录-文件.txt'))
$extendedTarget='\\?\' + $target
Write-Output "certutil_target=$target"
Write-Output "certutil_extended_target=$extendedTarget"
Write-Output "certutil_target_exists=$(Test-Path -LiteralPath $target -PathType Leaf)"
& certutil.exe -hashfile $extendedTarget SHA512
$certutilExit=$LASTEXITCODE
Write-Output "certutil_exit_code=$certutilExit"
exit $certutilExit
```

Evidence:

```text
certutil_target_exists=True
5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a
certutil_exit_code=0
```

The localized `certutil` status line and Unicode filename were mojibake in the
captured console, but the pre-call `Test-Path` was `True`, the exact extended
path was passed to `certutil`, its digest was unambiguous, and its exit code was
zero.

Digest comparison:

```text
GoHasher SHA-512 = 5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a
certutil SHA-512 = 5cd7106951cbfd05c3a6cf6061b94be001c6736fef31c353f441bfd02555ba57913e20cf12a93337a2fa8b8415fd65d0a53fb244bef2bb8e6345e9340e165d0a
match = True
```

This independently demonstrates that the hand-known expected SHA-512 matches
the exact 41 bytes written to the real long-path file.

### Guarded cleanup evidence

Cleanup again resolved both targets, required exact absolute-path equality, and
required each target to be a child of the exact task root before removal.

```text
resolved_task_root=D:\code\mySingerServer\.tmp\m1-acceptance-longpath
resolved_tree_target=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\tree
resolved_verifier_target=D:\code\mySingerServer\.tmp\m1-acceptance-longpath\main.go
tree_exists_after_cleanup=False
verifier_exists_after_cleanup=False
preserved_source_exists_after_cleanup=True
```

Only the task-owned tree and temporary verifier were deleted. The full verifier
source evidence and this report remain under
`D:\code\mySingerServer\docs\acceptance`.
