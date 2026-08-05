param(
    [string]$Go = "go",
    [string]$Npm = "npm",
    [string]$OutDir = "artifacts\stage",
    [string]$WebView2Bootstrapper = "third_party\webview2\MicrosoftEdgeWebview2Setup.exe",
    [switch]$SkipFrontendTests
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$script:WailsVersion = "v2.12.0"
$script:WailsCommand = "github.com/wailsapp/wails/v2/cmd/wails@$script:WailsVersion"
$script:ExpectedWailsModuleSum = "h1:BHO/kLNWFHYjCzucxbzAYZWUjub1Tvb4cSguQozHn5c="
$script:OfficialWebView2URL = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"
$repo = Split-Path -Parent $PSScriptRoot

function Resolve-NodeTrayApplication {
    param(
        [string]$Requested,
        [string]$Label
    )
    if (Test-Path -LiteralPath $Requested -PathType Leaf) {
        return [string](Resolve-Path -LiteralPath $Requested).Path
    }
    $selected = Get-Command $Requested -CommandType Application `
        -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $selected -or
        [string]::IsNullOrWhiteSpace([string]$selected.Source) -or
        -not (Test-Path -LiteralPath $selected.Source -PathType Leaf)) {
        throw ("NODETRAY_TOOL_NOT_FOUND label={0}" -f $Label)
    }
    return [string]$selected.Source
}

function Resolve-NodeTrayPath {
    param(
        [string]$Path,
        [string]$RepositoryRoot
    )
    $candidate = if ([IO.Path]::IsPathRooted($Path)) {
        $Path
    } else {
        Join-Path $RepositoryRoot $Path
    }
    return [IO.Path]::GetFullPath($candidate).TrimEnd('\')
}

function Assert-WebView2Cache {
    param(
        [string]$Bootstrapper,
        [string]$ManifestPath,
        [string]$RepositoryRoot
    )
    if (-not (Test-Path -LiteralPath $Bootstrapper -PathType Leaf)) {
        throw "WEBVIEW2_CACHE_MISSING"
    }
    if (-not (Test-Path -LiteralPath $ManifestPath -PathType Leaf)) {
        throw "WEBVIEW2_MANIFEST_MISSING"
    }
    if ((Get-Item -LiteralPath $Bootstrapper -Force).Attributes -band
        [IO.FileAttributes]::ReparsePoint) {
        throw "WEBVIEW2_CACHE_REPARSE_POINT_FORBIDDEN"
    }
    if ((Get-Item -LiteralPath $ManifestPath -Force).Attributes -band
        [IO.FileAttributes]::ReparsePoint) {
        throw "WEBVIEW2_MANIFEST_REPARSE_POINT_FORBIDDEN"
    }

    try {
        $manifest = Get-Content -Raw -LiteralPath $ManifestPath |
            ConvertFrom-Json -AsHashtable
    } catch {
        throw "WEBVIEW2_MANIFEST_INVALID_JSON"
    }
    $required = @(
        'schema_version',
        'filename',
        'official_source_url',
        'official_distribution_documentation_url',
        'actual_cache_origin',
        'sha256',
        'size',
        'acquired_utc',
        'notice_path',
        'authenticode'
    )
    if (@($manifest.Keys).Count -ne $required.Count -or
        @($required | Where-Object { -not $manifest.ContainsKey($_) }).Count -ne 0) {
        throw "WEBVIEW2_MANIFEST_FIELDS_INVALID"
    }
    if ([int]$manifest['schema_version'] -ne 1 -or
        [string]$manifest['filename'] -cne 'MicrosoftEdgeWebview2Setup.exe' -or
        [string]$manifest['official_source_url'] -cne $script:OfficialWebView2URL -or
        [string]$manifest['official_distribution_documentation_url'] -cne `
            'https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/distribution') {
        throw "WEBVIEW2_MANIFEST_AUTHORITY_INVALID"
    }

    $origin = $manifest['actual_cache_origin']
    $expectedOrigin = [ordered]@{
        kind = 'wails_module_embedded_asset'
        module = 'github.com/wailsapp/wails/v2'
        wails_version = $script:WailsVersion
        module_sum = $script:ExpectedWailsModuleSum
        embedded_asset_path = 'internal/webview2runtime/MicrosoftEdgeWebview2Setup.exe'
    }
    if ($origin -isnot [Collections.IDictionary] -or
        @($origin.Keys).Count -ne $expectedOrigin.Count) {
        throw "WEBVIEW2_CACHE_ORIGIN_INVALID"
    }
    foreach ($key in $expectedOrigin.Keys) {
        if (-not $origin.Contains($key) -or
            [string]$origin[$key] -cne [string]$expectedOrigin[$key]) {
            throw ("WEBVIEW2_CACHE_ORIGIN_INVALID field={0}" -f $key)
        }
    }
    $goSum = Join-Path $RepositoryRoot 'go.sum'
    if (-not (Test-Path -LiteralPath $goSum -PathType Leaf) -or
        -not (Select-String -LiteralPath $goSum -SimpleMatch `
            ("github.com/wailsapp/wails/v2 {0} {1}" -f `
                $script:WailsVersion, $script:ExpectedWailsModuleSum) -Quiet)) {
        throw "WEBVIEW2_CACHE_WAILS_MODULE_SUM_MISMATCH"
    }

    $noticeRelative = [string]$manifest['notice_path']
    if ([IO.Path]::IsPathRooted($noticeRelative) -or
        [string]::IsNullOrWhiteSpace($noticeRelative)) {
        throw "WEBVIEW2_NOTICE_PATH_INVALID"
    }
    $manifestRoot = [IO.Path]::GetFullPath(
        (Split-Path -Parent $ManifestPath)).TrimEnd('\')
    $notice = [IO.Path]::GetFullPath(
        (Join-Path $manifestRoot $noticeRelative))
    if (-not $notice.StartsWith($manifestRoot + '\',
            [StringComparison]::OrdinalIgnoreCase) -or
        -not (Test-Path -LiteralPath $notice -PathType Leaf) -or
        ((Get-Item -LiteralPath $notice -Force).Attributes -band
            [IO.FileAttributes]::ReparsePoint)) {
        throw "WEBVIEW2_NOTICE_INVALID"
    }

    $expectedHash = [string]$manifest['sha256']
    if ($expectedHash -cnotmatch '^[0-9a-f]{64}$') {
        throw "WEBVIEW2_MANIFEST_SHA256_INVALID"
    }
    $item = Get-Item -LiteralPath $Bootstrapper
    if ([long]$manifest['size'] -ne $item.Length) {
        throw "WEBVIEW2_CACHE_SIZE_MISMATCH"
    }
    $actualHash = (Get-FileHash -LiteralPath $Bootstrapper `
        -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -cne $expectedHash) {
        throw "WEBVIEW2_CACHE_SHA256_MISMATCH"
    }
    try {
        [DateTimeOffset]::Parse(
            [string]$manifest['acquired_utc'],
            [Globalization.CultureInfo]::InvariantCulture) | Out-Null
    } catch {
        throw "WEBVIEW2_ACQUIRED_UTC_INVALID"
    }

    $signatureManifest = $manifest['authenticode']
    if ($signatureManifest -isnot [Collections.IDictionary]) {
        throw "WEBVIEW2_SIGNATURE_MANIFEST_INVALID"
    }
    $signature = Get-AuthenticodeSignature -LiteralPath $Bootstrapper
    if ($signature.Status -ne [Management.Automation.SignatureStatus]::Valid -or
        $null -eq $signature.SignerCertificate -or
        $signature.SignerCertificate.Subject -notmatch `
            '^CN=Microsoft Corporation(?:,|$)') {
        throw "WEBVIEW2_AUTHENTICODE_INVALID"
    }
    if ([string]$signatureManifest['status'] -cne 'Valid' -or
        [string]$signatureManifest['signer_subject'] -cne `
            $signature.SignerCertificate.Subject -or
        [string]$signatureManifest['signer_thumbprint'] -cne `
            $signature.SignerCertificate.Thumbprint -or
        $null -eq $signature.TimeStamperCertificate -or
        [string]$signatureManifest['timestamp_subject'] -cne `
            $signature.TimeStamperCertificate.Subject) {
        throw "WEBVIEW2_SIGNATURE_MANIFEST_MISMATCH"
    }

    return [pscustomobject]$manifest
}

function Publish-FreshNodeTrayStage {
    param(
        [string]$PreparedStage,
        [string]$OutDir
    )
    $prepared = [IO.Path]::GetFullPath($PreparedStage).TrimEnd('\')
    $out = [IO.Path]::GetFullPath($OutDir).TrimEnd('\')
    if (-not (Test-Path -LiteralPath $prepared -PathType Container)) {
        throw "NODETRAY_PREPARED_STAGE_MISSING"
    }
    if (Test-Path -LiteralPath $out) {
        throw ("NODETRAY_STAGE_EXISTS path={0}" -f $out)
    }
    foreach ($name in @('nodetray.exe','MicrosoftEdgeWebview2Setup.exe')) {
        if (-not (Test-Path -LiteralPath (Join-Path $prepared $name) -PathType Leaf)) {
            throw ("NODETRAY_PREPARED_ARTIFACT_MISSING name={0}" -f $name)
        }
    }
    $parent = Split-Path -Parent $out
    if ([string]::IsNullOrWhiteSpace($parent) -or
        -not [string]::Equals(
            [IO.Path]::GetFullPath((Split-Path -Parent $prepared)).TrimEnd('\'),
            [IO.Path]::GetFullPath($parent).TrimEnd('\'),
            [StringComparison]::OrdinalIgnoreCase)) {
        throw "NODETRAY_ATOMIC_PUBLISH_REQUIRES_SAME_PARENT"
    }
    Move-Item -LiteralPath $prepared -Destination $out
    if (-not (Test-Path -LiteralPath $out -PathType Container)) {
        throw "NODETRAY_ATOMIC_PUBLISH_FAILED"
    }
}

function Get-LocalGoModuleProxy {
    param([string]$GoModuleCache)
    $full = [IO.Path]::GetFullPath($GoModuleCache).TrimEnd('\')
    $proxyRoot = Join-Path $full 'cache\download'
    $normalized = $proxyRoot.Replace('\', '/')
    if ($normalized -cnotmatch '^[A-Za-z]:/') {
        throw "NODETRAY_LOCAL_GO_PROXY_PATH_INVALID"
    }
    return 'file:///' + $normalized
}

function Resolve-MtExecutable {
    $command = Get-Command 'mt.exe' -CommandType Application `
        -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -ne $command -and
        (Test-Path -LiteralPath $command.Source -PathType Leaf)) {
        return [string]$command.Source
    }
    $kitRoot = 'C:\Program Files (x86)\Windows Kits\10\bin'
    $candidate = Get-ChildItem -LiteralPath $kitRoot -Filter 'mt.exe' `
        -File -Recurse -ErrorAction SilentlyContinue |
        Where-Object { $_.Directory.Name -ieq 'x64' } |
        Sort-Object FullName -Descending | Select-Object -First 1
    if ($null -eq $candidate) { throw "NODETRAY_MT_EXE_NOT_FOUND" }
    return [string]$candidate.FullName
}

function Assert-X64PE {
    param([string]$Path)
    $stream = [IO.File]::Open($Path, [IO.FileMode]::Open,
        [IO.FileAccess]::Read, [IO.FileShare]::Read)
    try {
        $reader = [IO.BinaryReader]::new($stream)
        if ($reader.ReadUInt16() -ne 0x5A4D) { throw "NODETRAY_PE_DOS_HEADER_INVALID" }
        $stream.Position = 0x3c
        $peOffset = $reader.ReadInt32()
        if ($peOffset -lt 0x40 -or $peOffset -gt ($stream.Length - 6)) {
            throw "NODETRAY_PE_OFFSET_INVALID"
        }
        $stream.Position = $peOffset
        if ($reader.ReadUInt32() -ne 0x00004550) { throw "NODETRAY_PE_SIGNATURE_INVALID" }
        $machine = $reader.ReadUInt16()
        if ($machine -ne 0x8664) {
            throw ("NODETRAY_PE_MACHINE_NOT_AMD64 machine=0x{0:x4}" -f $machine)
        }
        return 'amd64'
    } finally {
        $stream.Dispose()
    }
}

function Assert-EmbeddedAsInvokerManifest {
    param(
        [string]$Executable,
        [string]$Mt
    )
    $temporary = Join-Path ([IO.Path]::GetTempPath()) `
        ("nodetray-manifest-{0}.xml" -f [Guid]::NewGuid().ToString('N'))
    try {
        & $Mt ("-inputresource:{0};#1" -f $Executable) `
            ("-out:{0}" -f $temporary) | Out-Null
        if ($LASTEXITCODE -ne 0 -or
            -not (Test-Path -LiteralPath $temporary -PathType Leaf)) {
            throw "NODETRAY_PE_MANIFEST_EXTRACTION_FAILED"
        }
        $text = Get-Content -Raw -LiteralPath $temporary
        if ($text -notmatch 'requestedExecutionLevel\s+level="asInvoker"' -or
            $text -match 'requireAdministrator') {
            throw "NODETRAY_PE_MANIFEST_NOT_ASINVOKER"
        }
        return 'asInvoker'
    } finally {
        if (Test-Path -LiteralPath $temporary) {
            Remove-Item -LiteralPath $temporary -Force
        }
    }
}

if ($MyInvocation.InvocationName -eq '.') {
    return
}

$goExe = Resolve-NodeTrayApplication -Requested $Go -Label 'Go'
$npmExe = Resolve-NodeTrayApplication -Requested $Npm -Label 'npm'
$out = Resolve-NodeTrayPath -Path $OutDir -RepositoryRoot $repo
$repoRoot = [IO.Path]::GetFullPath($repo).TrimEnd('\')
$driveRoot = [IO.Path]::GetPathRoot($out).TrimEnd('\')
if ([string]::Equals($out, $repoRoot, [StringComparison]::OrdinalIgnoreCase) -or
    [string]::Equals($out, $driveRoot, [StringComparison]::OrdinalIgnoreCase) -or
    $out.StartsWith((Join-Path $repoRoot 'nodetray') + '\',
        [StringComparison]::OrdinalIgnoreCase)) {
    throw ("NODETRAY_STAGE_PATH_FORBIDDEN path={0}" -f $out)
}
if (Test-Path -LiteralPath $out) {
    throw ("NODETRAY_STAGE_EXISTS path={0}" -f $out)
}

$bootstrapper = Resolve-NodeTrayPath -Path $WebView2Bootstrapper `
    -RepositoryRoot $repo
$cacheRoot = Split-Path -Parent $bootstrapper
$cacheManifest = Join-Path $cacheRoot 'manifest.json'
$cache = Assert-WebView2Cache -Bootstrapper $bootstrapper `
    -ManifestPath $cacheManifest -RepositoryRoot $repo

$goVersion = (& $goExe version 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $goVersion -notmatch '^go version go') {
    throw "NODETRAY_GO_VERSION_FAILED"
}
$npmVersion = (& $npmExe --version 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $npmVersion -notmatch '^\d+\.\d+\.\d+') {
    throw "NODETRAY_NPM_VERSION_FAILED"
}

$goModuleCache = (& $goExe env GOMODCACHE 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($goModuleCache)) {
    throw "NODETRAY_GOMODCACHE_RESOLVE_FAILED"
}
$localGoProxy = Get-LocalGoModuleProxy -GoModuleCache $goModuleCache
$wailsBootstrapper = Join-Path $goModuleCache `
    'github.com\wailsapp\wails\v2@v2.12.0\internal\webview2runtime\MicrosoftEdgeWebview2Setup.exe'
if (-not (Test-Path -LiteralPath $wailsBootstrapper -PathType Leaf) -or
    (Get-FileHash -LiteralPath $wailsBootstrapper -Algorithm SHA256).Hash.ToLowerInvariant() `
        -cne [string]$cache.sha256) {
    throw "NODETRAY_WAILS_EMBEDDED_WEBVIEW2_MISMATCH"
}

$frontend = Join-Path $repo 'nodetray\frontend'
$nodeTrayRoot = Join-Path $repo 'nodetray'
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$oldCGO = $env:CGO_ENABLED
$oldGOTOOLCHAIN = $env:GOTOOLCHAIN
$oldGOPROXY = $env:GOPROXY
$prepared = $null
try {
    Push-Location -LiteralPath $frontend
    try {
        & $npmExe ci --ignore-scripts --no-audit --no-fund
        if ($LASTEXITCODE -ne 0) { throw "NODETRAY_NPM_CI_FAILED" }
        if (-not $SkipFrontendTests) {
            & $npmExe test
            if ($LASTEXITCODE -ne 0) { throw "NODETRAY_FRONTEND_TESTS_FAILED" }
        }
        & $npmExe run lint
        if ($LASTEXITCODE -ne 0) { throw "NODETRAY_FRONTEND_LINT_FAILED" }
        & $npmExe run build
        if ($LASTEXITCODE -ne 0) { throw "NODETRAY_FRONTEND_BUILD_FAILED" }
    } finally {
        Pop-Location
    }
    if (Get-ChildItem -LiteralPath (Join-Path $frontend 'dist') -Recurse `
        -File -Filter '*.map' | Select-Object -First 1) {
        throw "NODETRAY_FRONTEND_SOURCEMAP_FORBIDDEN"
    }

    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    $env:GOTOOLCHAIN = 'local'
    $env:GOPROXY = $localGoProxy
    Push-Location -LiteralPath $nodeTrayRoot
    try {
        & $goExe run github.com/wailsapp/wails/v2/cmd/wails@v2.12.0 generate module
        if ($LASTEXITCODE -ne 0) { throw "NODETRAY_WAILS_GENERATE_FAILED" }
    } finally {
        Pop-Location
    }
    & $goExe -C $repo test ./nodetray ./internal/nodetray/... -count=1
    if ($LASTEXITCODE -ne 0) { throw "NODETRAY_GO_TESTS_FAILED" }

    $wailsBin = Join-Path $nodeTrayRoot 'build\bin'
    $wailsResource = Join-Path $nodeTrayRoot 'nodetray-res.syso'
    if (Test-Path -LiteralPath $wailsBin) {
        Remove-Item -LiteralPath $wailsBin -Recurse -Force
    }
    if (Test-Path -LiteralPath $wailsResource -PathType Leaf) {
        Remove-Item -LiteralPath $wailsResource -Force
    }
    Push-Location -LiteralPath $nodeTrayRoot
    try {
        & $goExe run github.com/wailsapp/wails/v2/cmd/wails@v2.12.0 build `
            -platform windows/amd64 -webview2 embed -trimpath `
            -m -nosyncgomod -s -skipbindings -o nodetray.exe
        if ($LASTEXITCODE -ne 0) { throw "NODETRAY_WAILS_BUILD_FAILED" }
    } finally {
        Pop-Location
    }

    $builtExe = Join-Path $wailsBin 'nodetray.exe'
    if (-not (Test-Path -LiteralPath $builtExe -PathType Leaf)) {
        throw "NODETRAY_EXE_NOT_FOUND"
    }
    $architecture = Assert-X64PE -Path $builtExe
    $mt = Resolve-MtExecutable
    $executionLevel = Assert-EmbeddedAsInvokerManifest `
        -Executable $builtExe -Mt $mt
    $nodeTraySignature = Get-AuthenticodeSignature -LiteralPath $builtExe

    $parent = Split-Path -Parent $out
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
    $prepared = Join-Path $parent `
        (".{0}.tmp-{1}" -f (Split-Path -Leaf $out), [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $prepared | Out-Null
    Copy-Item -LiteralPath $builtExe `
        -Destination (Join-Path $prepared 'nodetray.exe')
    Copy-Item -LiteralPath $bootstrapper `
        -Destination (Join-Path $prepared 'MicrosoftEdgeWebview2Setup.exe')
    $preparedBootstrapper = Join-Path $prepared 'MicrosoftEdgeWebview2Setup.exe'
    Assert-WebView2Cache -Bootstrapper $preparedBootstrapper `
        -ManifestPath $cacheManifest -RepositoryRoot $repo | Out-Null
    Assert-X64PE -Path (Join-Path $prepared 'nodetray.exe') | Out-Null
    Assert-EmbeddedAsInvokerManifest -Executable (Join-Path $prepared 'nodetray.exe') `
        -Mt $mt | Out-Null
    Publish-FreshNodeTrayStage -PreparedStage $prepared -OutDir $out
    $prepared = $null

    $result = [ordered]@{
        status = 'PASS'
        go = $goVersion
        npm = $npmVersion
        wails = $script:WailsVersion
        wails_module_sum = $script:ExpectedWailsModuleSum
        webview2_sha256 = [string]$cache.sha256
        webview2_size = [long]$cache.size
        webview2_authenticode = 'Valid'
        pe_machine = $architecture
        execution_level = $executionLevel
        nodetray_authenticode = [string]$nodeTraySignature.Status
        stage = $out
    }
    Write-Host ($result | ConvertTo-Json -Compress)
} catch {
    if ($null -ne $prepared -and
        (Test-Path -LiteralPath $prepared) -and
        [string]::Equals(
            [IO.Path]::GetFullPath((Split-Path -Parent $prepared)).TrimEnd('\'),
            [IO.Path]::GetFullPath((Split-Path -Parent $out)).TrimEnd('\'),
            [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $prepared -Recurse -Force
    }
    if (Test-Path -LiteralPath $out) {
        throw ("NODETRAY_ATOMIC_STAGE_INVARIANT_BROKEN path={0}" -f $out)
    }
    throw
} finally {
    if ($null -eq $oldGOOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldGOOS }
    if ($null -eq $oldGOARCH) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldGOARCH }
    if ($null -eq $oldCGO) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCGO }
    if ($null -eq $oldGOTOOLCHAIN) { Remove-Item Env:GOTOOLCHAIN -ErrorAction SilentlyContinue } else { $env:GOTOOLCHAIN = $oldGOTOOLCHAIN }
    if ($null -eq $oldGOPROXY) { Remove-Item Env:GOPROXY -ErrorAction SilentlyContinue } else { $env:GOPROXY = $oldGOPROXY }
}
