param(
    [Parameter(Mandatory = $true)]
    [string]$SourceFile,
    [Parameter(Mandatory = $true)]
    [string]$HeaderFile,
    [Parameter(Mandatory = $true)]
    [string]$ObjectFile,
    [Parameter(Mandatory = $true)]
    [string]$Dumpbin
)

$ErrorActionPreference = 'Stop'

foreach ($path in @($SourceFile, $HeaderFile, $ObjectFile, $Dumpbin)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "required production-isolation input is missing: $path"
    }
}

$productionText = (Get-Content -Raw -LiteralPath $SourceFile) + "`n" +
    (Get-Content -Raw -LiteralPath $HeaderFile)
$forbiddenSource = @(
    'Test',
    'test_',
    'fail_next',
    'ResourceSnapshot',
    'resource_snapshot',
    'counter',
    'ImageDecodeResourceSnapshot',
    'SetImageDecodeFailNextGrayAllocation',
    'GetImageDecodeResourceSnapshot',
    'g_fail_next_gray_allocation',
    'g_turbo_acquired',
    'g_turbo_released',
    'g_png_acquired',
    'g_png_released',
    'g_stb_acquired',
    'g_stb_released'
)
foreach ($token in $forbiddenSource) {
    if ($productionText.Contains($token, [StringComparison]::Ordinal)) {
        throw "production image source contains test-only state/API: $token"
    }
}

$symbols = & $Dumpbin /symbols $ObjectFile 2>&1 | Out-String
if ($LASTEXITCODE -ne 0) {
    throw "dumpbin /symbols failed for the real production image object"
}
foreach ($token in $forbiddenSource) {
    if ($symbols.IndexOf($token, [StringComparison]::OrdinalIgnoreCase) -ge 0) {
        throw "production image object contains test-only symbol: $token"
    }
}

$requiredRaii = @(
    'using TurboHandle = std::unique_ptr',
    'struct PngOwner',
    '~PngOwner() noexcept',
    'std::unique_ptr<uint8_t, StbDeleter>'
)
foreach ($token in $requiredRaii) {
    if (-not $productionText.Contains($token, [StringComparison]::Ordinal)) {
        throw "production image source lost required RAII ownership: $token"
    }
}

Write-Output 'IMAGE_PRODUCTION_ISOLATION PASS object=image_decode.obj test_state=absent raii=turbo,png,stb'
