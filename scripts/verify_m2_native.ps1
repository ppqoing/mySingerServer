[CmdletBinding()]
param(
    [string]$CMake = 'C:\vcpkg\downloads\tools\cmake-4.2.3-windows\cmake-4.2.3-windows-x86_64\bin\cmake.exe',
    [string]$VcpkgRoot = 'C:\vcpkg',
    [switch]$SkipBuild,
    [switch]$TimeoutContract
)

$ErrorActionPreference = 'Stop'

function Invoke-NativeProcess {
    param(
        [Parameter(Mandatory)]
        [string]$Executable,
        [Parameter(Mandatory)]
        [AllowEmptyString()]
        [string[]]$Arguments,
        [int]$TimeoutMilliseconds = 5000
    )

    $startInfo = [System.Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $Executable
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    # ProcessStartInfo.ArgumentList is unavailable in Windows PowerShell 5.1.
    # These native gate arguments are fixed tokens and local paths; quote every
    # token so both Windows PowerShell and pwsh execute the same command line.
    $startInfo.Arguments = (@(
        foreach ($argument in $Arguments) {
            '"' + $argument.Replace('"', '\"') + '"'
        }
    ) -join ' ')
    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    if (-not $process.Start()) {
        throw "failed to start $Executable"
    }
    if (-not $process.WaitForExit($TimeoutMilliseconds)) {
        $taskkillOutput = (& taskkill.exe /PID $process.Id /T /F 2>&1) -join "`n"
        $taskkillExit = $LASTEXITCODE
        if (-not $process.WaitForExit(5000)) {
            # Kill(Boolean) is not available on Windows PowerShell 5.1's
            # .NET Framework. Kill() is the compatibility fallback after the
            # tree-aware taskkill attempt.
            $process.Kill()
            if (-not $process.WaitForExit(5000)) {
                throw "native timeout cleanup did not reap PID $($process.Id)"
            }
        }
        if ($taskkillExit -ne 0) {
            throw "native timeout taskkill failed exit=$taskkillExit pid=$($process.Id): $taskkillOutput"
        }
        throw "native subprocess hung for more than ${TimeoutMilliseconds}ms: $($Arguments -join ' ')"
    }
    $stdout = $process.StandardOutput.ReadToEnd()
    $stderr = $process.StandardError.ReadToEnd()
    [pscustomobject]@{
        ExitCode = $process.ExitCode
        Stdout = $stdout
        Stderr = $stderr
    }
}

if ($TimeoutContract) {
    $pidFile = Join-Path ([System.IO.Path]::GetTempPath()) ("m2-native-timeout-" + [guid]::NewGuid().ToString('N') + '.pid')
    try {
        $escapedPIDFile = $pidFile.Replace("'", "''")
        $contractScript = '$child=Start-Process powershell.exe -PassThru -WindowStyle Hidden ' +
            "-ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 60';" +
            "Set-Content -LiteralPath '$escapedPIDFile' -Value `$child.Id;" +
            'Wait-Process -Id $child.Id'
        $timedOut = $false
        try {
            Invoke-NativeProcess `
                -Executable 'powershell.exe' `
                -Arguments @('-NoProfile', '-Command', $contractScript) `
                -TimeoutMilliseconds 1000 | Out-Null
        }
        catch {
            if (-not $_.Exception.Message.Contains('native subprocess hung')) {
                throw
            }
            $timedOut = $true
        }
        if (-not $timedOut) {
            throw 'native timeout contract did not time out'
        }
        if (-not (Test-Path -LiteralPath $pidFile -PathType Leaf)) {
            throw 'native timeout contract child PID was not published'
        }
        $childPID = [int](Get-Content -LiteralPath $pidFile -Raw)
        if (Get-Process -Id $childPID -ErrorAction SilentlyContinue) {
            throw "native timeout contract left child PID $childPID running"
        }
        Write-Host "NATIVE TIMEOUT CONTRACT PASS child_pid=$childPID residual=0"
        exit 0
    }
    finally {
        Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
    }
}

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$mediaRoot = Join-Path $repoRoot 'mediacore'
$buildRoot = Join-Path $mediaRoot 'build'
$releaseRoot = Join-Path $buildRoot 'Release'
$ctest = Join-Path (Split-Path -Parent $CMake) 'ctest.exe'

if (-not $SkipBuild) {
    & (Join-Path $PSScriptRoot 'build.ps1') `
        -MediacoreOnly `
        -CMake $CMake `
        -VcpkgRoot $VcpkgRoot
    if ($LASTEXITCODE -ne 0) {
        throw 'native build failed'
    }
}

& $ctest --test-dir $buildRoot -C Release --output-on-failure
if ($LASTEXITCODE -ne 0) {
    throw 'native CTest failed'
}

$makeLuma = Join-Path $releaseRoot 'mc_make_luma.exe'
$ourLuma = Join-Path $releaseRoot 'mc_luma_runner.exe'
$refLuma = Join-Path $releaseRoot 'ref_luma_hasher.exe'
$makeLevelB = Join-Path $releaseRoot 'mc_make_level_b.exe'
$endToEnd = Join-Path $releaseRoot 'mc_endtoend.exe'
foreach ($tool in @($makeLuma, $ourLuma, $refLuma, $makeLevelB, $endToEnd)) {
    if (-not (Test-Path -LiteralPath $tool -PathType Leaf)) {
        throw "missing native test tool: $tool"
    }
}

$lumaRoot = Join-Path $mediaRoot 'testdata\luma'
[System.IO.Directory]::CreateDirectory($lumaRoot) | Out-Null
& $makeLuma $lumaRoot
if ($LASTEXITCODE -ne 0) {
    throw 'luma corpus generation failed'
}
$lumaFiles = @(
    Get-ChildItem -LiteralPath $lumaRoot -Filter '*.lumabin' -File |
        Sort-Object Name
)
if ($lumaFiles.Count -ne 72) {
    throw "Level A expected 72 vectors, found $($lumaFiles.Count)"
}
$levelAMatched = 0
foreach ($file in $lumaFiles) {
    $ours = & $ourLuma $file.FullName
    if ($LASTEXITCODE -ne 0) {
        throw "DLL luma runner failed: $($file.Name)"
    }
    $reference = & $refLuma $file.FullName
    if ($LASTEXITCODE -ne 0) {
        throw "upstream luma runner failed: $($file.Name)"
    }
    if ($ours -cne $reference) {
        throw "Level A mismatch $($file.Name): ours=[$ours] reference=[$reference]"
    }
    $levelAMatched++
}
Write-Host "LEVEL-A PASS $levelAMatched/72"

$levelBRoot = Join-Path $mediaRoot 'testdata\level_b'
[System.IO.Directory]::CreateDirectory($levelBRoot) | Out-Null
$officialDataRoot = Join-Path $repoRoot '.superpowers\tmp\threatexchange-baefb4ed67b6cdc1d4c82dbaef858d50866ac424\ThreatExchange-baefb4ed67b6cdc1d4c82dbaef858d50866ac424\pdq\data'
if (-not (Test-Path -LiteralPath $officialDataRoot -PathType Container)) {
    throw "missing pinned official PDQ data: $officialDataRoot"
}
& $makeLevelB $levelBRoot $officialDataRoot
if ($LASTEXITCODE -ne 0) {
    throw 'Level B corpus/golden generation failed'
}
$levelBTotalSamples = 0
$levelBTotalJudged = 0
foreach ($set in @(
    @{ Name = 'local'; Path = (Join-Path $levelBRoot 'level_b.tsv'); Expected = 20 },
    @{ Name = 'official'; Path = (Join-Path $levelBRoot 'level_b_official.tsv'); Expected = 49 }
)) {
    $setSamples = 0
    $setJudged = 0
    $setMaxDistance = 0
    foreach ($line in Get-Content -LiteralPath $set.Path) {
        if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith('#')) {
            continue
        }
        $parts = $line -split "`t"
        if ($parts.Count -ne 3) {
            throw "malformed Level B line: $line"
        }
        $actual = & $endToEnd hash $parts[0]
        if ($LASTEXITCODE -ne 0) {
            throw "DLL image hash failed: $($parts[0])"
        }
        $actualHash = ($actual -split ' ')[0]
        $distanceText = & $endToEnd hd $actualHash $parts[1]
        if ($LASTEXITCODE -ne 0) {
            throw "Hamming distance failed: $($parts[0])"
        }
        $distance = [int]$distanceText
        $referenceQuality = [int]$parts[2]
        $setMaxDistance = [Math]::Max($setMaxDistance, $distance)
        if ($referenceQuality -ge 80) {
            $setJudged++
            if ($distance -gt 10) {
                throw "Level B violation $($parts[0]): distance=$distance quality=$referenceQuality"
            }
        }
        $setSamples++
    }
    if ($setSamples -ne $set.Expected) {
        throw "Level B $($set.Name) expected $($set.Expected) samples, found $setSamples"
    }
    if ($setJudged -eq 0) {
        throw "Level B $($set.Name) had no judged samples with reference quality >= 80"
    }
    Write-Host "LEVEL-B $($set.Name) PASS samples=$setSamples judged=$setJudged max_hd=$setMaxDistance"
    $levelBTotalSamples += $setSamples
    $levelBTotalJudged += $setJudged
}

$wrongExtension = Join-Path $levelBRoot 'wrongext.png'
& $endToEnd hash $wrongExtension | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'magic-byte dispatch rejected JPEG bytes with a PNG extension'
}
Write-Host "LEVEL-B PASS samples=$levelBTotalSamples judged=$levelBTotalJudged"

$corruptFiles = @(Get-ChildItem -LiteralPath (Join-Path $levelBRoot 'corrupt') -File)
$corruptPassed = 0
foreach ($file in $corruptFiles) {
    $result = Invoke-NativeProcess -Executable $endToEnd -Arguments @('hash', $file.FullName)
    if ($result.ExitCode -ne 1) {
        throw "corrupt input did not exit cleanly with decode error: $($file.Name), exit=$($result.ExitCode)"
    }
    $corruptPassed++
}
Write-Host "CORRUPT PASS $corruptPassed/$($corruptFiles.Count)"

$shaVectors = @(
    @{
        Name = 'abc'
        Input = 'abc'
        Want = 'ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f'
    },
    @{
        Name = 'empty'
        Input = ''
        Want = 'cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e'
    }
)
foreach ($vector in $shaVectors) {
    $shaResult = Invoke-NativeProcess `
        -Executable $endToEnd `
        -Arguments @('sha512str', [string]$vector.Input)
    $got = $shaResult.Stdout.Trim()
    if ($shaResult.ExitCode -ne 0 -or $got -cne $vector.Want) {
        throw "SHA-512 mismatch $($vector.Name): got=$got"
    }
}
Write-Host "SHA512 PASS $($shaVectors.Count)/$($shaVectors.Count)"
Write-Host 'M2 NATIVE VERIFY PASS'
