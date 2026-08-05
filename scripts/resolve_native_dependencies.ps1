param(
    [Parameter(Mandatory = $true)]
    [string[]]$RootDll,
    [Parameter(Mandatory = $true)]
    [string[]]$SearchRoot,
    [Parameter(Mandatory = $true)]
    [string]$RepositoryRoot,
    [string]$Dumpbin = "",
    [Parameter(Mandatory = $true)]
    [string]$OutFile
)

$ErrorActionPreference = "Stop"

function Resolve-NativeDumpbin {
    param([string]$Requested)

    if ($Requested) {
        if (-not (Test-Path -LiteralPath $Requested -PathType Leaf)) {
            throw "NATIVE_DEPENDENCY_TOOL_NOT_FOUND path=$Requested"
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
    throw "NATIVE_DEPENDENCY_TOOL_NOT_FOUND"
}

function Test-NativePathInsideRoot {
    param(
        [string]$Path,
        [string]$Root
    )

    $pathFull = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $rootFull = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    return [string]::Equals(
               $pathFull, $rootFull,
               [StringComparison]::OrdinalIgnoreCase) -or
           $pathFull.StartsWith(
               $rootFull + '\',
               [StringComparison]::OrdinalIgnoreCase)
}

function Test-NativeSystemDll {
    param([string]$Name)

    $lower = $Name.ToLowerInvariant()
    if ($lower.StartsWith("api-ms-win-") -or
        $lower.StartsWith("ext-ms-win-")) {
        return $true
    }
    return $lower -in @(
        "advapi32.dll", "bcrypt.dll", "bcryptprimitives.dll",
        "cabinet.dll", "cfgmgr32.dll", "combase.dll", "comctl32.dll",
        "comdlg32.dll", "crypt32.dll", "dwmapi.dll", "gdi32.dll",
        "d2d1.dll", "dwrite.dll", "gdi32full.dll", "imm32.dll",
        "iphlpapi.dll", "kernel32.dll",
        "kernelbase.dll", "mf.dll", "mfplat.dll", "mfuuid.dll",
        "msvcp_win.dll", "msvcrt.dll", "ncrypt.dll", "ntdll.dll",
        "ole32.dll", "oleaut32.dll", "powrprof.dll", "rpcrt4.dll",
        "sechost.dll", "secur32.dll", "setupapi.dll", "shell32.dll",
        "shlwapi.dll", "ucrtbase.dll", "user32.dll", "userenv.dll",
        "usp10.dll", "version.dll", "winmm.dll", "wintrust.dll", "ws2_32.dll"
    )
}

function Get-NativeDllImports {
    param(
        [string]$DllPath,
        [string]$DumpbinPath
    )

    $output = @(& $DumpbinPath /dependents $DllPath 2>&1)
    if ($null -ne $LASTEXITCODE -and $LASTEXITCODE -ne 0) {
        throw "NATIVE_DEPENDENCY_INSPECTION_FAILED path=$DllPath exit=$LASTEXITCODE"
    }
    $imports = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase)
    foreach ($line in $output) {
        $text = ([string]$line).Trim()
        if ($text -match '^[A-Za-z0-9_.+-]+\.dll$') {
            [void]$imports.Add($text)
        }
    }
    return @($imports | Sort-Object)
}

function Resolve-NativeDependencyClosure {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$RootDll,
        [Parameter(Mandatory = $true)]
        [string[]]$SearchRoot,
        [Parameter(Mandatory = $true)]
        [string]$RepositoryRoot,
        [Parameter(Mandatory = $true)]
        [string]$DumpbinPath
    )

    if (-not (Test-Path -LiteralPath $RepositoryRoot -PathType Container)) {
        throw "NATIVE_DEPENDENCY_REPOSITORY_NOT_FOUND path=$RepositoryRoot"
    }
    $repository = (Resolve-Path -LiteralPath $RepositoryRoot).Path.TrimEnd('\')
    $candidateByName = @{}
    foreach ($root in $SearchRoot) {
        if (-not (Test-Path -LiteralPath $root -PathType Container)) {
            throw "NATIVE_DEPENDENCY_SEARCH_ROOT_NOT_FOUND path=$root"
        }
        $search = (Resolve-Path -LiteralPath $root).Path
        if (-not (Test-NativePathInsideRoot -Path $search -Root $repository)) {
            throw "NATIVE_DEPENDENCY_OUTSIDE_REPOSITORY path=$search"
        }
        foreach ($item in Get-ChildItem -LiteralPath $search -Filter "*.dll" `
                     -File -Recurse -Force) {
            $full = $item.FullName
            if (-not (Test-NativePathInsideRoot -Path $full -Root $repository)) {
                throw "NATIVE_DEPENDENCY_OUTSIDE_REPOSITORY path=$full"
            }
            $key = $item.Name.ToLowerInvariant()
            if (-not $candidateByName.ContainsKey($key)) {
                $candidateByName[$key] = @()
            }
            $existing = @($candidateByName[$key])
            if ($full -notin $existing) {
                $candidateByName[$key] = @($existing + $full)
            }
        }
    }

    $resolved = @{}
    $importsByName = @{}
    $queue = [Collections.Generic.Queue[string]]::new()
    foreach ($root in $RootDll) {
        if (-not (Test-Path -LiteralPath $root -PathType Leaf)) {
            throw "NATIVE_DEPENDENCY_ROOT_NOT_FOUND path=$root"
        }
        $full = (Resolve-Path -LiteralPath $root).Path
        if (-not (Test-NativePathInsideRoot -Path $full -Root $repository)) {
            throw "NATIVE_DEPENDENCY_OUTSIDE_REPOSITORY path=$full"
        }
        $key = [IO.Path]::GetFileName($full).ToLowerInvariant()
        if ($candidateByName.ContainsKey($key) -and
            @($candidateByName[$key]).Count -gt 1) {
            throw "NATIVE_DEPENDENCY_AMBIGUOUS name=$key candidates=$(@($candidateByName[$key]).Count)"
        }
        if (-not $resolved.ContainsKey($key)) {
            $resolved[$key] = $full
            $queue.Enqueue($key)
        }
    }

    while ($queue.Count -gt 0) {
        $key = $queue.Dequeue()
        $path = [string]$resolved[$key]
        $imports = @(Get-NativeDllImports -DllPath $path -DumpbinPath $DumpbinPath)
        $importsByName[$key] = $imports
        foreach ($import in $imports) {
            if (Test-NativeSystemDll -Name $import) {
                continue
            }
            $importKey = $import.ToLowerInvariant()
            if (-not $candidateByName.ContainsKey($importKey)) {
                throw "NATIVE_DEPENDENCY_UNRESOLVED importer=$key name=$import"
            }
            $candidates = @($candidateByName[$importKey])
            if ($candidates.Count -ne 1) {
                throw "NATIVE_DEPENDENCY_AMBIGUOUS name=$import candidates=$($candidates.Count)"
            }
            $candidate = [string]$candidates[0]
            if (-not (Test-NativePathInsideRoot -Path $candidate -Root $repository)) {
                throw "NATIVE_DEPENDENCY_OUTSIDE_REPOSITORY path=$candidate"
            }
            if (-not $resolved.ContainsKey($importKey)) {
                $resolved[$importKey] = $candidate
                $queue.Enqueue($importKey)
            }
        }
    }

    $files = @(
        foreach ($key in @($resolved.Keys | Sort-Object)) {
            $path = [string]$resolved[$key]
            $relative = [IO.Path]::GetRelativePath($repository, $path) `
                -replace '\\', '/'
            [ordered]@{
                name = [IO.Path]::GetFileName($path)
                path = $relative
                sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
                imports = @($importsByName[$key])
            }
        }
    )
    return [ordered]@{
        schema_version = 1
        files = $files
    }
}

$temporary = $null
try {
    $dumpbinPath = Resolve-NativeDumpbin -Requested $Dumpbin
    $manifest = Resolve-NativeDependencyClosure `
        -RootDll $RootDll `
        -SearchRoot $SearchRoot `
        -RepositoryRoot $RepositoryRoot `
        -DumpbinPath $dumpbinPath
    $outFull = [IO.Path]::GetFullPath($OutFile)
    $outParent = [IO.Path]::GetDirectoryName($outFull)
    if (-not (Test-Path -LiteralPath $outParent -PathType Container)) {
        throw "NATIVE_DEPENDENCY_OUTPUT_PARENT_NOT_FOUND path=$outParent"
    }
    if (Test-Path -LiteralPath $outFull) {
        throw "NATIVE_DEPENDENCY_OUTPUT_EXISTS path=$outFull"
    }
    $temporary = Join-Path $outParent `
        (".{0}.tmp-{1}" -f [IO.Path]::GetFileName($outFull), [Guid]::NewGuid().ToString("N"))
    $json = $manifest | ConvertTo-Json -Depth 8
    [IO.File]::WriteAllText(
        $temporary,
        $json + [Environment]::NewLine,
        [Text.UTF8Encoding]::new($false))
    Move-Item -LiteralPath $temporary -Destination $outFull
    $temporary = $null
    Write-Host "NATIVE DEPENDENCY CLOSURE PASS files=$($manifest.files.Count)"
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
