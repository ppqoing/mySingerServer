param(
    [Parameter(Mandatory = $true)]
    [string]$Dll,
    [Parameter(Mandatory = $true)]
    [string]$Def,
    [string]$Dumpbin = "",
    [string]$OutFile = ""
)

$ErrorActionPreference = "Stop"

function Resolve-DumpbinExecutable {
    param([string]$Requested)

    if ($Requested) {
        if (-not (Test-Path -LiteralPath $Requested -PathType Leaf)) {
            throw "VIDEOCORE_DUMPBIN_NOT_FOUND path=$Requested"
        }
        return (Resolve-Path -LiteralPath $Requested).Path
    }
    $command = Get-Command "dumpbin.exe" -CommandType Application `
        -ErrorAction SilentlyContinue
    if ($null -ne $command) {
        return $command.Source
    }
    $vswhere = "C:\Program Files (x86)\Microsoft Visual Studio\Installer\vswhere.exe"
    if (Test-Path -LiteralPath $vswhere -PathType Leaf) {
        $installation = & $vswhere -latest -products * `
            -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 `
            -property installationPath
        if ($LASTEXITCODE -eq 0 -and $installation) {
            $candidate = Get-ChildItem -LiteralPath `
                (Join-Path $installation "VC\Tools\MSVC") `
                -Filter "dumpbin.exe" -File -Recurse |
                Where-Object { $_.FullName -like "*\bin\Hostx64\x64\dumpbin.exe" } |
                Sort-Object FullName -Descending |
                Select-Object -First 1
            if ($null -ne $candidate) {
                return $candidate.FullName
            }
        }
    }
    throw "VIDEOCORE_DUMPBIN_NOT_FOUND"
}

$temporary = $null
try {
    if (-not (Test-Path -LiteralPath $Dll -PathType Leaf)) {
        throw "VIDEOCORE_EXPORT_DLL_NOT_FOUND path=$Dll"
    }
    if (-not (Test-Path -LiteralPath $Def -PathType Leaf)) {
        throw "VIDEOCORE_EXPORT_DEF_NOT_FOUND path=$Def"
    }
    $dllPath = (Resolve-Path -LiteralPath $Dll).Path
    $defPath = (Resolve-Path -LiteralPath $Def).Path
    $dumpbinPath = Resolve-DumpbinExecutable -Requested $Dumpbin

    $expected = @(
        Get-Content -LiteralPath $defPath |
            ForEach-Object { $_.Trim() } |
            Where-Object {
                $_ -and
                -not $_.StartsWith(";") -and
                $_ -notmatch '^(?i:LIBRARY|EXPORTS)(\s|$)'
            } |
            ForEach-Object {
                (($_ -split '\s+')[0] -split '=')[0]
            }
    )
    $expectedUnique = @($expected | Sort-Object -Unique -CaseSensitive)
    if ($expected.Count -ne 14 -or $expectedUnique.Count -ne 14) {
        throw "VIDEOCORE_EXPORT_DEF_INVALID expected=14 actual=$($expected.Count) unique=$($expectedUnique.Count)"
    }

    $dumpOutput = @(& $dumpbinPath /exports $dllPath 2>&1)
    if ($null -ne $LASTEXITCODE -and $LASTEXITCODE -ne 0) {
        throw "VIDEOCORE_DUMPBIN_FAILED exit=$LASTEXITCODE"
    }
    $actual = @(
        foreach ($line in $dumpOutput) {
            $text = [string]$line
            if ($text -match '^\s*\d+\s+[0-9A-Fa-f]+\s+[0-9A-Fa-f]+\s+(\S+)') {
                $Matches[1]
            }
        }
    )
    $actualUnique = @($actual | Sort-Object -Unique -CaseSensitive)
    $missing = @($expectedUnique | Where-Object { $_ -cnotin $actualUnique })
    $extra = @($actualUnique | Where-Object { $_ -cnotin $expectedUnique })
    if ($actual.Count -ne 14 -or $actualUnique.Count -ne 14 -or
        $missing.Count -ne 0 -or $extra.Count -ne 0) {
        Write-Error ("VIDEOCORE_EXPORT_MISMATCH expected=14 actual={0} missing=[{1}] extra=[{2}]" -f `
            $actualUnique.Count,
            ($missing -join ','),
            ($extra -join ','))
        exit 1
    }

    if ($OutFile) {
        $outFull = [IO.Path]::GetFullPath($OutFile)
        $outParent = [IO.Path]::GetDirectoryName($outFull)
        if (-not (Test-Path -LiteralPath $outParent -PathType Container)) {
            throw "VIDEOCORE_EXPORT_OUTPUT_PARENT_NOT_FOUND path=$outParent"
        }
        if (Test-Path -LiteralPath $outFull) {
            throw "VIDEOCORE_EXPORT_OUTPUT_EXISTS path=$outFull"
        }
        $temporary = Join-Path $outParent `
            (".{0}.tmp-{1}" -f [IO.Path]::GetFileName($outFull), [Guid]::NewGuid().ToString("N"))
        $text = (@($actualUnique | Sort-Object -CaseSensitive) -join [Environment]::NewLine) +
            [Environment]::NewLine
        [IO.File]::WriteAllText(
            $temporary, $text, [Text.UTF8Encoding]::new($false))
        Move-Item -LiteralPath $temporary -Destination $outFull
        $temporary = $null
    }
    Write-Host "14/14 exact exports"
    exit 0
}
catch {
    Write-Error $_.Exception.Message
    exit 1
}
finally {
    if ($temporary -and (Test-Path -LiteralPath $temporary)) {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }
}
