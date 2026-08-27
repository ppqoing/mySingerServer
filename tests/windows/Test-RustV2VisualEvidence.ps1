<#
.SYNOPSIS
验证 Rust V2 视觉夹具、真实 Release 截图、对照板和中文验收报告的完整性。

.DESCRIPTION
本脚本只读取证据，不生成、修补或替换任何截图。离屏夹具必须精确匹配客户区尺寸；
真实窗口截图允许固定的 Windows 非客户区范围，并要求报告逐张记录真实捕获尺寸。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
Add-Type -AssemblyName System.Drawing

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$previewRoot = Join-Path $repositoryRoot 'docs\ui-preview\rust-v2'
$reportPath = Join-Path $repositoryRoot 'docs\verification\2026-08-20-rust-v2-visual-fidelity.md'
$views = @(
    '01-overview', '02-nodes', '03-scan', '04-tasks',
    '05-exact', '06-similar-images', '07-similar-videos', '08-cross-machine',
    '09-review', '10-delete-center', '11-settings', '12-diagnostics'
)
$sizes = @(
    [pscustomobject]@{ Name = '1440x900'; Width = 1440; Height = 900 },
    [pscustomobject]@{ Name = '1080x700'; Width = 1080; Height = 700 }
)
$comparisons = @(
    '01-overview-nodes.png',
    '02-scan-tasks.png',
    '03-exact-cross-machine.png',
    '04-similar-media.png',
    '05-review-delete.png',
    '06-settings-diagnostics.png'
)

function Stop-VisualEvidenceValidation {
    param(
        [Parameter(Mandatory)] [string] $Code,
        [Parameter(Mandatory)] [string] $Path,
        [string] $Detail = ''
    )

    $suffix = if ([string]::IsNullOrWhiteSpace($Detail)) { '' } else { " $Detail" }
    throw "$Code path=$Path$suffix"
}

function Get-PngDimensions {
    param([Parameter(Mandatory)] [string] $Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        Stop-VisualEvidenceValidation -Code 'RUST_V2_VISUAL_EVIDENCE_MISSING' -Path $Path
    }

    $stream = $null
    $image = $null
    try {
        $stream = [IO.File]::Open($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
        $image = [Drawing.Image]::FromStream($stream, $true, $true)
        if ($image.RawFormat.Guid -ne [Drawing.Imaging.ImageFormat]::Png.Guid) {
            Stop-VisualEvidenceValidation -Code 'RUST_V2_VISUAL_EVIDENCE_NOT_PNG' -Path $Path
        }
        return [pscustomobject]@{ Width = $image.Width; Height = $image.Height }
    }
    catch {
        if ($_.Exception.Message.StartsWith('RUST_V2_VISUAL_EVIDENCE_')) {
            throw
        }
        Stop-VisualEvidenceValidation `
            -Code 'RUST_V2_VISUAL_EVIDENCE_DECODE_FAILED' `
            -Path $Path `
            -Detail "error=$($_.Exception.Message)"
    }
    finally {
        if ($null -ne $image) {
            $image.Dispose()
        }
        if ($null -ne $stream) {
            $stream.Dispose()
        }
    }
}

$releaseCaptures = @()
foreach ($size in $sizes) {
    foreach ($view in $views) {
        $afterPath = Join-Path $previewRoot "after\$($size.Name)\$view.png"
        $after = Get-PngDimensions -Path $afterPath
        if ($after.Width -ne $size.Width -or $after.Height -ne $size.Height) {
            Stop-VisualEvidenceValidation `
                -Code 'RUST_V2_AFTER_DIMENSIONS_INVALID' `
                -Path $afterPath `
                -Detail "expected=$($size.Width)x$($size.Height) actual=$($after.Width)x$($after.Height)"
        }

        $releasePath = Join-Path $previewRoot "release\$($size.Name)\$view.png"
        $release = Get-PngDimensions -Path $releasePath
        $widthMaximum = $size.Width + 16
        $heightMinimum = $size.Height + 24
        $heightMaximum = $size.Height + 48
        if ($release.Width -lt $size.Width -or
            $release.Width -gt $widthMaximum -or
            $release.Height -lt $heightMinimum -or
            $release.Height -gt $heightMaximum) {
            Stop-VisualEvidenceValidation `
                -Code 'RUST_V2_RELEASE_DIMENSIONS_INVALID' `
                -Path $releasePath `
                -Detail "expected_width=$($size.Width)..$widthMaximum expected_height=$heightMinimum..$heightMaximum actual=$($release.Width)x$($release.Height)"
        }
        $releaseCaptures += [pscustomobject]@{
            RelativePath = "release/$($size.Name)/$view.png"
            Width = $release.Width
            Height = $release.Height
        }
    }
}

foreach ($comparison in $comparisons) {
    $comparisonPath = Join-Path $previewRoot "comparison\$comparison"
    $dimensions = Get-PngDimensions -Path $comparisonPath
    if ($dimensions.Width -ne 2320 -or $dimensions.Height -ne 900) {
        Stop-VisualEvidenceValidation `
            -Code 'RUST_V2_COMPARISON_DIMENSIONS_INVALID' `
            -Path $comparisonPath `
            -Detail "expected=2320x900 actual=$($dimensions.Width)x$($dimensions.Height)"
    }
}

if (-not (Test-Path -LiteralPath $reportPath -PathType Leaf)) {
    Stop-VisualEvidenceValidation -Code 'RUST_V2_VISUAL_EVIDENCE_MISSING' -Path $reportPath
}
$report = Get-Content -LiteralPath $reportPath -Raw
$requiredReportFields = @(
    [pscustomobject]@{ Name = '包绝对路径'; Pattern = '(?im)^\s*(?:-\s*)?包绝对路径\s*[:：]\s*[A-Za-z]:\\.+$' },
    [pscustomobject]@{ Name = '包字节数'; Pattern = '(?im)^\s*(?:-\s*)?包字节数\s*[:：]\s*[1-9][0-9,]*\s*(?:字节)?\s*$' },
    [pscustomobject]@{ Name = '包 SHA-256'; Pattern = '(?im)^\s*(?:-\s*)?包\s*SHA-256\s*[:：]\s*[0-9a-f]{64}\s*$' },
    [pscustomobject]@{ Name = 'DPI 缩放'; Pattern = '(?im)^\s*(?:-\s*)?DPI\s*缩放\s*[:：]\s*\d+%\s*$' },
    [pscustomobject]@{ Name = '客户区目标'; Pattern = '(?im)^\s*(?:-\s*)?客户区目标\s*[:：].*1440\s*[x×]\s*900.*1080\s*[x×]\s*700.*$' },
    [pscustomobject]@{ Name = '真实 Release'; Pattern = '(?im)^\s*(?:-\s*)?真实\s*Release\s*[:：]\s*\S.+$' },
    [pscustomobject]@{ Name = '人工结论'; Pattern = '(?im)^\s*(?:-\s*)?人工结论\s*[:：]\s*\S.+$' }
)
foreach ($field in $requiredReportFields) {
    if (-not [regex]::IsMatch($report, $field.Pattern)) {
        Stop-VisualEvidenceValidation `
            -Code 'RUST_V2_VISUAL_REPORT_FIELD_MISSING' `
            -Path $reportPath `
            -Detail "field=$($field.Name)"
    }
}

foreach ($capture in $releaseCaptures) {
    $pathPattern = [regex]::Escape($capture.RelativePath).Replace('/', '[/\\]')
    $dimensionPattern = "$($capture.Width)\s*[x×]\s*$($capture.Height)"
    if (-not [regex]::IsMatch(
            $report,
            "$pathPattern[\s\S]{0,200}?$dimensionPattern",
            [Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
        Stop-VisualEvidenceValidation `
            -Code 'RUST_V2_VISUAL_REPORT_CAPTURE_MISSING' `
            -Path $reportPath `
            -Detail "capture=$($capture.RelativePath) actual=$($capture.Width)x$($capture.Height)"
    }
}

Write-Output 'RUST_V2_VISUAL_EVIDENCE_PASS'
