<#
.SYNOPSIS
构建并冻结 Rust V2 CPU/I/O A/B 专用测试便携包。

.DESCRIPTION
脚本复用正式 build/verify 流程，然后立即把指定 variant 的 ZIP、sidecar 和展开目录
归档到独立目录。测试客户端和结果导出器只记录在 ZIP 外的 metadata，永不进入正式包。
#>
[CmdletBinding()]
param(
    [ValidateSet('A', 'B')]
    [Parameter(Mandatory)]
    [string] $Variant,
    [string] $CargoTargetDir,
    [string] $OutputRoot = 'C:\tmp\rust-v2-cpu-io-ab\packages',
    [string] $SourceRevision,
    [string] $SourceTreeSha256,
    [string] $AcceptanceClientPath,
    [string] $ResultExporterPath
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repositoryRoot = Split-Path -Parent $PSScriptRoot
# 正式路径固定调用同级脚本；仅行为夹具可把同名脚本放入隔离目录，验证调用顺序。
$formalScriptsRoot = [Environment]::GetEnvironmentVariable('RUST_V2_TEST_FORMAL_SCRIPTS_ROOT', 'Process')
$formalScriptsBase = if ([string]::IsNullOrWhiteSpace($formalScriptsRoot)) { Join-Path $repositoryRoot 'scripts' } else { [IO.Path]::GetFullPath($formalScriptsRoot).TrimEnd('\') }
$formalBuilder = Join-Path $formalScriptsBase 'build-release.ps1'
$formalVerifier = Join-Path $formalScriptsBase 'verify-release.ps1'
$targetTriple = 'x86_64-pc-windows-msvc'

function Get-FullPathSafe {
    <# 将路径转成稳定绝对路径；空值保持空字符串。 #>
    param([string] $Path)
    if ([string]::IsNullOrWhiteSpace($Path)) { return '' }
    return [IO.Path]::GetFullPath($Path).TrimEnd('\')
}

function Test-PathWithin {
    <# 判断路径是否落在根目录内，防止工具与正式 release 混放。 #>
    param([string] $Candidate, [string] $Root)
    $candidatePath = Get-FullPathSafe $Candidate
    $rootPath = Get-FullPathSafe $Root
    if (-not $candidatePath -or -not $rootPath) { return $false }
    return $candidatePath.Equals($rootPath, [StringComparison]::OrdinalIgnoreCase) -or
        $candidatePath.StartsWith(($rootPath + '\'), [StringComparison]::OrdinalIgnoreCase)
}

function Get-FileSha256OrNull {
    <# 返回文件 SHA-256；文件缺失时返回 null，供 metadata 明确表达证据缺失。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Get-TextSha256 {
    <# 对 UTF-8 文本计算确定性 SHA-256，用于配置和源码清单指纹。 #>
    param([AllowNull()][string] $Text)
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes([string]$Text)
    $algorithm = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($algorithm.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant() }
    finally { $algorithm.Dispose() }
}

function Get-NormalizedFileSha256 {
    <# 规范化配置文本的换行后计算 SHA，保证不同 Windows 写入方式得到同一指纹。 #>
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    $text = [IO.File]::ReadAllText($Path)
    $normalized = ($text -replace "`r`n", "`n") -replace "`r", "`n"
    return Get-TextSha256 -Text $normalized
}

function Write-Utf8NoBom {
    <# 以无 BOM UTF-8 写 metadata，避免不同 PowerShell 版本改变哈希。 #>
    param([string] $Path, [string] $Text)
    [IO.File]::WriteAllText($Path, $Text, [Text.UTF8Encoding]::new($false))
}

function Invoke-PowerShellScript {
    <# 调用正式脚本并保留输出，外层只根据退出码和固定 marker 判断成功。 #>
    param([string] $ScriptPath, [string[]] $Arguments)
    $pwsh = (Get-Command pwsh -ErrorAction Stop).Source
    $output = & $pwsh -NoProfile -File $ScriptPath @Arguments 2>&1
    $exitCode = $LASTEXITCODE
    [pscustomobject]@{ ExitCode = $exitCode; Output = ($output | Out-String) }
}

function Resolve-PackagePathFromOutput {
    <# 从正式 builder 的 PACKAGE_PATH marker 读取 ZIP，禁止猜测其他产物。 #>
    param([string] $Output)
    $line = @($Output -split "`r?`n" | Where-Object { $_ -match '^PACKAGE_PATH=' } | Select-Object -Last 1)
    if ($line.Count -eq 0) { return '' }
    return ($line[0] -replace '^PACKAGE_PATH=', '').Trim()
}

function Get-SourceTreeHash {
    <# 计算 HEAD、tracked working-tree 内容和 untracked 内容的实际指纹；生成物与 SDD 证据固定排除。 #>
    param([string] $Root, [string] $ExcludedRoot = '')
    $head = ((& git -C $Root rev-parse HEAD 2>$null) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $head) { return $null }
    $excluded = if ($ExcludedRoot) { Get-FullPathSafe $ExcludedRoot } else { '' }
    $lines = [Collections.Generic.List[string]]::new()
    [void]$lines.Add("HEAD`t$head")
    $patterns = @('^\.git(?:/|$)', '^\.superpowers/sdd(?:/|$)', '^docs/verification(?:/|$)', '^dist-rust-v2(?:/|$)', '^target(?:/|$)')
    $paths = @((& git -C $Root ls-files 2>$null) + (& git -C $Root ls-files --others --exclude-standard 2>$null)) |
        Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Sort-Object -Unique
    foreach ($rawPath in $paths) {
        $relative = ([string]$rawPath).Replace('\', '/')
        if ($patterns | Where-Object { $relative -match $_ }) { continue }
        $full = Get-FullPathSafe (Join-Path $Root $relative)
        if ($excluded -and (Test-PathWithin -Candidate $full -Root $excluded)) { continue }
        if (Test-Path -LiteralPath $full -PathType Leaf) {
            $hash = (Get-FileHash -LiteralPath $full -Algorithm SHA256).Hash.ToLowerInvariant()
            $size = (Get-Item -LiteralPath $full).Length
            [void]$lines.Add("FILE`t$relative`t$size`t$hash")
        } else {
            [void]$lines.Add("DELETED`t$relative")
        }
    }
    return Get-TextSha256 -Text (($lines -join "`n") + "`n")
}

function Get-ToolVersion {
    <# 保存 rustc/cargo 的完整版本文本，作为 A/B 可比构建环境证据。 #>
    param([string] $Command)
    $resolved = Get-Command $Command -ErrorAction SilentlyContinue
    if ($null -eq $resolved) { return $null }
    return ((& $resolved.Source -Vv 2>$null) | Out-String).Trim()
}

function Assert-FormalPackage {
    <# 对正式 ZIP 运行唯一允许的正式 verifier；test-only 脚本不复制测试 EXE。 #>
    param([string] $PackagePath)
    $result = Invoke-PowerShellScript -ScriptPath $formalVerifier -Arguments @('-Package', $PackagePath)
    if ($result.ExitCode -ne 0 -or $result.Output -notmatch 'PACKAGE_PASS') {
        throw "RUST_V2_TEST_PACKAGE_FORMAL_VERIFY_FAILED: $($result.Output.Trim())"
    }
}

function Assert-ZipSidecar {
    <# 校验 sidecar 的哈希和文件名，防止归档时绑定到旧 ZIP。 #>
    param([string] $PackagePath, [string] $SidecarPath)
    if (-not (Test-Path -LiteralPath $SidecarPath -PathType Leaf)) { throw 'RUST_V2_TEST_PACKAGE_FORMAL_SIDECAR_MISSING' }
    $line = @(Get-Content -LiteralPath $SidecarPath | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Select-Object -First 1)
    if ($line.Count -ne 1 -or $line[0] -notmatch '^([0-9a-fA-F]{64})\s+(.+)$') { throw 'RUST_V2_TEST_PACKAGE_FORMAL_SIDECAR_INVALID' }
    $name = [IO.Path]::GetFileName($PackagePath)
    if ([IO.Path]::GetFileName([string]$Matches[2]) -ne $name -or $Matches[1].ToLowerInvariant() -ne (Get-FileSha256OrNull $PackagePath)) {
        throw 'RUST_V2_TEST_PACKAGE_FORMAL_SIDECAR_MISMATCH'
    }
}

function Get-ManifestHash {
    <# 读取展开正式目录的 manifest SHA，作为 package 绑定的一部分。 #>
    param([string] $ReleaseRoot)
    $manifest = Join-Path $ReleaseRoot 'manifest\files.sha256'
    $hash = Get-FileSha256OrNull $manifest
    if (-not $hash) { throw 'RUST_V2_TEST_PACKAGE_MANIFEST_MISSING' }
    return $hash
}

function Assert-ToolPath {
    <# 校验外置工具必须是绝对路径、存在且不在 formal release 内。 #>
    param([string] $Path, [string] $ReleaseRoot, [string] $Name)
    if ([string]::IsNullOrWhiteSpace($Path)) { return [ordered]@{ path = $null; sha256 = $null } }
    $absolute = Get-FullPathSafe $Path
    if (-not [IO.Path]::IsPathRooted($Path) -or -not (Test-Path -LiteralPath $absolute -PathType Leaf)) {
        throw "RUST_V2_TEST_PACKAGE_${Name}_INVALID"
    }
    if (Test-PathWithin -Candidate $absolute -Root $ReleaseRoot) {
        throw "RUST_V2_TEST_PACKAGE_${Name}_INSIDE_RELEASE"
    }
    return [ordered]@{ path = $absolute; sha256 = (Get-FileHash -LiteralPath $absolute -Algorithm SHA256).Hash.ToLowerInvariant() }
}

function New-TestPackageMetadata {
    <# 生成有序 test-only metadata，并明确 deployable=false。 #>
    param(
        [string] $Path, [string] $ReleaseRoot, [string] $PackagePath, [string] $SidecarPath,
        [string] $SourceRevisionValue, [string] $SourceTreeValue, [string] $ManifestSha,
        [hashtable] $AcceptanceTool, [hashtable] $ExporterTool, [string] $ConfigSha
    )
    $cargoLock = Join-Path $repositoryRoot 'Cargo.lock'
    $cargoLockSha = Get-FileSha256OrNull $cargoLock
    $rustc = Get-ToolVersion 'rustc'
    $cargo = Get-ToolVersion 'cargo'
    $buildConfigPath = Join-Path $repositoryRoot '.cargo\config.toml'
    $nodeConfig = Join-Path $ReleaseRoot 'config\node.toml'
    $metadata = [ordered]@{
        schema = 'rust-v2-cpu-io-test-package/v1'
        variant = $Variant
        test_only = $true
        deployable = $false
        source_revision = $SourceRevisionValue
        source_tree_sha256 = $SourceTreeValue
        cargo_lock_sha256 = $cargoLockSha
        rustc = $rustc
        cargo = $cargo
        target_triple = $targetTriple
        build_config_sha256 = Get-FileSha256OrNull $buildConfigPath
        formal_zip_path = Get-FullPathSafe $PackagePath
        formal_zip_sha256 = Get-FileSha256OrNull $PackagePath
        formal_zip_sidecar_path = Get-FullPathSafe $SidecarPath
        formal_manifest_sha256 = $ManifestSha
        release_root = Get-FullPathSafe $ReleaseRoot
        runtime_config_path = Get-FullPathSafe $nodeConfig
        runtime_config_sha256 = $ConfigSha
        # builder 不接收媒体根，明确由 orchestrator 启动前生成并绑定真实清单。
        media_manifest_binding = 'orchestrator_runtime'
        media_manifest_source = 'orchestrator_before_run'
        media_manifest_sha256 = 'BOUND_AT_RUN'
        node_exe_sha256 = Get-FileSha256OrNull (Join-Path $ReleaseRoot 'node.exe')
        worker_exe_sha256 = Get-FileSha256OrNull (Join-Path $ReleaseRoot 'worker.exe')
        acceptance_client_path = $AcceptanceTool.path
        acceptance_client_sha256 = $AcceptanceTool.sha256
        result_exporter_path = $ExporterTool.path
        result_exporter_sha256 = $ExporterTool.sha256
        tools_are_external = $true
    }
    Write-Utf8NoBom -Path $Path -Text (($metadata | ConvertTo-Json -Depth 12) + "`n")
    return $metadata
}

function Assert-MetadataBinding {
    <# 写入前后复算包、sidecar、manifest 和 test-only 标志，避免 metadata 自相矛盾。 #>
    param([string] $MetadataPath, [string] $ReleaseRoot, [string] $PackagePath, [string] $SidecarPath)
    $metadata = Get-Content -LiteralPath $MetadataPath -Raw | ConvertFrom-Json
    if ($metadata.schema -ne 'rust-v2-cpu-io-test-package/v1' -or
        $metadata.variant -ne $Variant -or $metadata.test_only -ne $true -or $metadata.deployable -ne $false) {
        throw 'RUST_V2_TEST_PACKAGE_METADATA_FLAGS_INVALID'
    }
    if ([IO.Path]::GetFullPath([string]$metadata.formal_zip_path) -ne [IO.Path]::GetFullPath($PackagePath)) {
        throw 'RUST_V2_TEST_PACKAGE_METADATA_ZIP_PATH_MISMATCH'
    }
    if ([string]$metadata.formal_zip_sha256 -ne (Get-FileSha256OrNull $PackagePath)) {
        throw 'RUST_V2_TEST_PACKAGE_METADATA_ZIP_HASH_MISMATCH'
    }
    if ([string]$metadata.formal_manifest_sha256 -ne (Get-ManifestHash $ReleaseRoot)) {
        throw 'RUST_V2_TEST_PACKAGE_METADATA_MANIFEST_HASH_MISMATCH'
    }
    if ([string]$metadata.media_manifest_binding -ne 'orchestrator_runtime' -or [string]$metadata.media_manifest_sha256 -ne 'BOUND_AT_RUN') {
        throw 'RUST_V2_TEST_PACKAGE_METADATA_MEDIA_BINDING_INVALID'
    }
    foreach ($binary in @('node.exe', 'worker.exe')) {
        $property = if ($binary -eq 'node.exe') { 'node_exe_sha256' } else { 'worker_exe_sha256' }
        $actualBinarySha = Get-FileSha256OrNull (Join-Path $ReleaseRoot $binary)
        if (-not $actualBinarySha -or [string]$metadata.$property -ne $actualBinarySha) {
            throw "RUST_V2_TEST_PACKAGE_METADATA_EXE_SHA_MISMATCH:$binary"
        }
    }
    $configPath = Get-FullPathSafe ([string]$metadata.runtime_config_path)
    if (-not $configPath -or (Get-NormalizedFileSha256 $configPath) -ne ([string]$metadata.runtime_config_sha256).ToLowerInvariant()) {
        throw 'RUST_V2_TEST_PACKAGE_METADATA_RUNTIME_CONFIG_SHA_MISMATCH'
    }
    $actualRevision = ((& git -C $repositoryRoot rev-parse HEAD 2>$null) | Out-String).Trim()
    if ([string]$metadata.source_revision -ne $actualRevision) { throw 'RUST_V2_TEST_PACKAGE_METADATA_SOURCE_REVISION_MISMATCH' }
    if ([string]$metadata.source_tree_sha256 -ne (Get-SourceTreeHash -Root $repositoryRoot -ExcludedRoot (Split-Path -Parent $ReleaseRoot))) {
        throw 'RUST_V2_TEST_PACKAGE_METADATA_SOURCE_TREE_SHA_MISMATCH'
    }
    if ((Get-FullPathSafe ([string]$metadata.formal_zip_sidecar_path)) -ne (Get-FullPathSafe $SidecarPath)) {
        throw 'RUST_V2_TEST_PACKAGE_METADATA_SIDECAR_PATH_MISMATCH'
    }
    foreach ($tool in @('acceptance_client_path', 'result_exporter_path')) {
        $toolPath = [string]$metadata.$tool
        if ($toolPath -and (Test-PathWithin -Candidate $toolPath -Root $ReleaseRoot)) {
            throw "RUST_V2_TEST_PACKAGE_TOOL_INSIDE_RELEASE:$tool"
        }
    }
}

try {
    if (-not (Test-Path -LiteralPath $formalBuilder -PathType Leaf) -or
        -not (Test-Path -LiteralPath $formalVerifier -PathType Leaf)) {
        throw 'RUST_V2_TEST_PACKAGE_FORMAL_SCRIPT_MISSING'
    }
    $outputRootAbsolute = Get-FullPathSafe $OutputRoot
    $variantRoot = Join-Path $outputRootAbsolute $Variant
    if (Test-Path -LiteralPath $variantRoot) {
        throw 'RUST_V2_TEST_PACKAGE_VARIANT_ALREADY_EXISTS'
    }
    $actualRevision = ((& git -C $repositoryRoot rev-parse HEAD 2>$null) | Out-String).Trim()
    if (-not $actualRevision) { throw 'RUST_V2_TEST_PACKAGE_SOURCE_REVISION_UNAVAILABLE' }
    if (-not [string]::IsNullOrWhiteSpace($SourceRevision) -and $SourceRevision -ne $actualRevision) {
        throw 'RUST_V2_TEST_PACKAGE_SOURCE_REVISION_MISMATCH'
    }
    if ([string]::IsNullOrWhiteSpace($SourceRevision)) { $SourceRevision = $actualRevision }
    $actualTreeHash = Get-SourceTreeHash -Root $repositoryRoot -ExcludedRoot $outputRootAbsolute
    if (-not $actualTreeHash) { throw 'RUST_V2_TEST_PACKAGE_SOURCE_TREE_UNAVAILABLE' }
    if (-not [string]::IsNullOrWhiteSpace($SourceTreeSha256) -and $SourceTreeSha256.ToLowerInvariant() -ne $actualTreeHash) {
        throw 'RUST_V2_TEST_PACKAGE_SOURCE_TREE_MISMATCH'
    }
    if ([string]::IsNullOrWhiteSpace($SourceTreeSha256)) { $SourceTreeSha256 = $actualTreeHash }
    if ([string]::IsNullOrWhiteSpace($SourceRevision) -or [string]::IsNullOrWhiteSpace($SourceTreeSha256)) {
        throw 'RUST_V2_TEST_PACKAGE_SOURCE_FINGERPRINT_MISSING'
    }

    # 无论是否为夹具都先调用 build-release，再解析其输出；不允许 ZIP 注入绕过真实生产路径。
    $builderArgs = @()
    if ($CargoTargetDir) { $builderArgs += @('-CargoTargetDir', $CargoTargetDir) }
    $buildResult = Invoke-PowerShellScript -ScriptPath $formalBuilder -Arguments $builderArgs
    if ($buildResult.ExitCode -ne 0 -or $buildResult.Output -notmatch 'RUST_V2_RELEASE_BUILD_PASS') {
        throw "RUST_V2_TEST_PACKAGE_FORMAL_BUILD_FAILED: $($buildResult.Output.Trim())"
    }
    $formalPackage = Resolve-PackagePathFromOutput -Output $buildResult.Output
    if (-not $formalPackage) { throw 'RUST_V2_TEST_PACKAGE_FORMAL_ZIP_MARKER_MISSING' }
    $formalPackage = Get-FullPathSafe $formalPackage
    if (-not (Test-Path -LiteralPath $formalPackage -PathType Leaf)) { throw 'RUST_V2_TEST_PACKAGE_FORMAL_ZIP_MISSING' }
    if (Test-PathWithin -Candidate $formalPackage -Root $outputRootAbsolute) { throw 'RUST_V2_TEST_PACKAGE_FORMAL_ZIP_INSIDE_OUTPUT' }
    Assert-ZipSidecar -PackagePath $formalPackage -SidecarPath "$formalPackage.sha256"
    Assert-FormalPackage -PackagePath $formalPackage

    # A/B variant 目录尚未存在，复制和解压在同一次执行内完成，避免下一次 formal build 覆盖旧包。
    $archiveRoot = Join-Path $variantRoot 'formal'
    $releaseRoot = Join-Path $variantRoot 'release'
    New-Item -ItemType Directory -Path $archiveRoot, $releaseRoot -Force | Out-Null
    $archiveZip = Join-Path $archiveRoot "$Variant-formal.zip"
    $archiveSidecar = "$archiveZip.sha256"
    Copy-Item -LiteralPath $formalPackage -Destination $archiveZip -Force:$false
    $sourceSidecar = "$formalPackage.sha256"
    if (-not (Test-Path -LiteralPath $sourceSidecar -PathType Leaf)) { throw 'RUST_V2_TEST_PACKAGE_FORMAL_SIDECAR_MISSING' }
    Assert-ZipSidecar -PackagePath $formalPackage -SidecarPath $sourceSidecar
    # 归档文件可能改名，重新生成 sidecar，不能复用源 ZIP 的文件名绑定。
    $archiveHash = Get-FileSha256OrNull $archiveZip
    if (-not $archiveHash) { throw 'RUST_V2_TEST_PACKAGE_ARCHIVE_SHA256_UNAVAILABLE' }
    Write-Utf8NoBom -Path $archiveSidecar -Text "$archiveHash  $(Split-Path -Leaf $archiveZip)`n"
    Assert-ZipSidecar -PackagePath $archiveZip -SidecarPath $archiveSidecar
    Expand-Archive -LiteralPath $archiveZip -DestinationPath $releaseRoot -Force
    Assert-FormalPackage -PackagePath $releaseRoot

    $toolsRoot = Join-Path $variantRoot 'tools'
    $acceptanceTool = Assert-ToolPath -Path $AcceptanceClientPath -ReleaseRoot $releaseRoot -Name 'ACCEPTANCE_CLIENT'
    $exporterTool = Assert-ToolPath -Path $ResultExporterPath -ReleaseRoot $releaseRoot -Name 'RESULT_EXPORTER'
    $configPath = Join-Path $releaseRoot 'config\node.toml'
    $configSha = Get-NormalizedFileSha256 $configPath
    $metadataPath = Join-Path $variantRoot 'test-package.json'
    $metadata = New-TestPackageMetadata -Path $metadataPath -ReleaseRoot $releaseRoot -PackagePath $archiveZip `
        -SidecarPath $archiveSidecar -SourceRevisionValue $SourceRevision -SourceTreeValue $SourceTreeSha256 `
        -ManifestSha (Get-ManifestHash $releaseRoot) -AcceptanceTool $acceptanceTool -ExporterTool $exporterTool -ConfigSha $configSha
    Assert-MetadataBinding -MetadataPath $metadataPath -ReleaseRoot $releaseRoot -PackagePath $archiveZip -SidecarPath $archiveSidecar

    $metadataSha = Get-FileSha256OrNull $metadataPath
    Write-Output 'RUST_V2_CPU_IO_TEST_PACKAGE_PASS'
    Write-Output "VARIANT=$Variant"
    Write-Output "TEST_PACKAGE_ROOT=$variantRoot"
    Write-Output "TEST_PACKAGE_METADATA=$metadataPath"
    Write-Output "FORMAL_ZIP=$archiveZip"
    Write-Output "FORMAL_ZIP_SHA256=$(Get-FileSha256OrNull $archiveZip)"
    Write-Output "METADATA_SHA256=$metadataSha"
}
catch {
    Write-Error $_
    exit 1
}
