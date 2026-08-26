param(
    [Parameter(Mandatory = $true)] [string]$ImageObjectDir,
    [Parameter(Mandatory = $true)] [string]$DllTargetDir,
    [Parameter(Mandatory = $true)] [string]$CompatTargetDir,
    [Parameter(Mandatory = $true)] [string]$DllBinary,
    [Parameter(Mandatory = $true)] [string]$CompatBinary,
    [Parameter(Mandatory = $true)] [string]$Dumpbin,
    [Parameter(Mandatory = $true)] [string]$SourceRoot,
    [Parameter(Mandatory = $true)] [string]$LegacySourceRoot,
    [Parameter(Mandatory = $true)] [string]$Compiler,
    [Parameter(Mandatory = $true)] [string]$VsDevCmd,
    [Parameter(Mandatory = $true)] [string]$CompilerCommandTlog,
    [Parameter(Mandatory = $true)] [string]$CanonicalImageObjectDir,
    [Parameter(Mandatory = $true)] [string]$BuildRoot
)

$ErrorActionPreference = 'Stop'
$sourceByObject = [ordered]@{
    downscaling = 'src/pdq_upstream/pdq/cpp/downscaling/downscaling.cpp'
    image_decode = 'src/native_algorithms/image_decode.cpp'
    pdq = 'src/native_algorithms/pdq.cpp'
    pdqhamming = 'src/pdq_upstream/pdq/cpp/common/pdqhamming.cpp'
    pdqhashing = 'src/pdq_upstream/pdq/cpp/hashing/pdqhashing.cpp'
    pdqhashtypes = 'src/pdq_upstream/pdq/cpp/common/pdqhashtypes.cpp'
    pdqutils = 'src/pdq_upstream/pdq/cpp/common/pdqutils.cpp'
    phash_parts = 'src/native_algorithms/phash_parts.cpp'
    sobel_hist = 'src/native_algorithms/sobel_hist.cpp'
    stb_impl = 'src/native_algorithms/stb_impl.cpp'
    torben = 'src/pdq_upstream/pdq/cpp/hashing/torben.cpp'
}
$dllPrivateObjects = @('api','avio_bridge','cancel_token','contact_sheet','deadline','error',
    'image_analysis','media_session','runtime_info','sha512','video_analysis','win_file')
$compatPrivateObjects = @('test_image_compat')

function Require-File([string]$Path, [string]$Role) {
    $full = [IO.Path]::GetFullPath($Path)
    if (-not (Test-Path -LiteralPath $full -PathType Leaf)) {
        throw "$Role is missing: $full"
    }
    return Get-Item -LiteralPath $full
}

function Sha256-File([string]$Path) {
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Test-Within([string]$Path, [string]$Root) {
    $full = [IO.Path]::GetFullPath($Path)
    $base = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    return $full.Equals($base, [StringComparison]::OrdinalIgnoreCase) -or
        $full.StartsWith($base + '\', [StringComparison]::OrdinalIgnoreCase)
}

function Resolve-BuildPath([string]$Path) {
    if ([IO.Path]::IsPathRooted($Path)) { return [IO.Path]::GetFullPath($Path) }
    return [IO.Path]::GetFullPath((Join-Path $script:BuildRootFull $Path))
}

function Require-ByteIdenticalTree(
    [string]$LegacyRoot,
    [string]$CopiedRoot,
    [string]$Role) {
    $legacyFull = [IO.Path]::GetFullPath($LegacyRoot).TrimEnd('\')
    $copiedFull = [IO.Path]::GetFullPath($CopiedRoot).TrimEnd('\')
    if (-not (Test-Path -LiteralPath $legacyFull -PathType Container) -or
        -not (Test-Path -LiteralPath $copiedFull -PathType Container)) {
        throw "vendor tree byte mismatch: $Role tree is missing"
    }
    $legacyFiles = @{}
    foreach ($file in (Get-ChildItem -LiteralPath $legacyFull -Recurse -File)) {
        $relative = [IO.Path]::GetRelativePath($legacyFull, $file.FullName).Replace('\','/')
        $legacyFiles[$relative] = Sha256-File $file.FullName
    }
    $copiedFiles = @{}
    foreach ($file in (Get-ChildItem -LiteralPath $copiedFull -Recurse -File)) {
        $relative = [IO.Path]::GetRelativePath($copiedFull, $file.FullName).Replace('\','/')
        $copiedFiles[$relative] = Sha256-File $file.FullName
    }
    $pathDiff = @(Compare-Object @($legacyFiles.Keys | Sort-Object) @($copiedFiles.Keys | Sort-Object))
    $hashDiff = @($legacyFiles.Keys | Where-Object {
        $copiedFiles.ContainsKey($_) -and $legacyFiles[$_] -cne $copiedFiles[$_]
    } | Sort-Object)
    if ($pathDiff.Count -ne 0 -or $hashDiff.Count -ne 0) {
        throw "vendor tree byte mismatch: $Role legacy_files=$($legacyFiles.Count) copied_files=$($copiedFiles.Count) hash_diff=$($hashDiff -join ',')"
    }
    return $legacyFiles.Count
}

function Read-UInt16([byte[]]$Bytes, [int]$Offset) {
    if ($Offset -lt 0 -or $Offset + 2 -gt $Bytes.Length) { throw 'truncated binary header' }
    return [BitConverter]::ToUInt16($Bytes, $Offset)
}

function Read-UInt32([byte[]]$Bytes, [int]$Offset) {
    if ($Offset -lt 0 -or $Offset + 4 -gt $Bytes.Length) { throw 'truncated binary header' }
    return [BitConverter]::ToUInt32($Bytes, $Offset)
}

function Require-X64CoffObject([IO.FileInfo]$Object, [string]$Role) {
    $bytes = [IO.File]::ReadAllBytes($Object.FullName)
    if ($bytes.Length -lt 20 -or (Read-UInt16 $bytes 0) -ne 0x8664 -or
        (Read-UInt16 $bytes 2) -eq 0 -or (Read-UInt16 $bytes 16) -ne 0) {
        throw "$Role is not an x64 COFF object: $($Object.FullName)"
    }
}

function Require-X64Pe([IO.FileInfo]$File, [bool]$ExpectDll, [string]$Role) {
    $bytes = [IO.File]::ReadAllBytes($File.FullName)
    if ($bytes.Length -lt 256 -or (Read-UInt16 $bytes 0) -ne 0x5A4D) {
        throw "$Role is not a PE image"
    }
    $pe = [int](Read-UInt32 $bytes 0x3C)
    if ($pe -lt 64 -or $pe + 26 -gt $bytes.Length -or
        (Read-UInt32 $bytes $pe) -ne 0x00004550 -or
        (Read-UInt16 $bytes ($pe + 4)) -ne 0x8664 -or
        (Read-UInt16 $bytes ($pe + 24)) -ne 0x20B) {
        throw "$Role is not an x64 PE image"
    }
    $isDll = ((Read-UInt16 $bytes ($pe + 22)) -band 0x2000) -ne 0
    if ($isDll -ne $ExpectDll) {
        throw "$Role PE kind mismatch: expected_dll=$ExpectDll actual_dll=$isDll"
    }
}

function Get-TlogRecords([string]$Path, [string]$Role) {
    $file = Require-File $Path $Role
    $records = [Collections.Generic.List[object]]::new()
    $current = $null
    foreach ($line in (Get-Content -LiteralPath $file.FullName)) {
        if ($line.StartsWith('^')) {
            if ($null -ne $current) { $records.Add($current) }
            $current = [ordered]@{
                Root = $line.Substring(1)
                Body = [Collections.Generic.List[string]]::new()
            }
        } elseif ($null -ne $current) {
            $current.Body.Add($line)
        }
    }
    if ($null -ne $current) { $records.Add($current) }
    if ($records.Count -eq 0) { throw "$Role contains no keyed records" }
    return @($records)
}

if (-not ('VcWindowsArgv' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
public static class VcWindowsArgv {
    [DllImport("shell32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    private static extern IntPtr CommandLineToArgvW(string commandLine, out int argc);
    [DllImport("kernel32.dll")]
    private static extern IntPtr LocalFree(IntPtr memory);
    public static string[] Parse(string commandLine) {
        int argc;
        IntPtr pointer = CommandLineToArgvW(commandLine, out argc);
        if (pointer == IntPtr.Zero) throw new Win32Exception();
        try {
            string[] args = new string[argc];
            for (int i = 0; i < argc; ++i)
                args[i] = Marshal.PtrToStringUni(Marshal.ReadIntPtr(pointer, i * IntPtr.Size));
            return args;
        } finally { LocalFree(pointer); }
    }
}
'@
}

function Convert-WindowsCommandLine([string]$Command, [string]$Role) {
    if ([string]::IsNullOrWhiteSpace($Command)) { throw "$Role has no command" }
    if ($Command -match '[&|<>^\r\n]') { throw "$Role unsafe shell control" }
    $parsed = [VcWindowsArgv]::Parse('vc_tlog ' + $Command)
    if ($parsed.Count -lt 2) { throw "$Role has no arguments" }
    return @($parsed[1..($parsed.Count - 1)])
}

function Compare-TextMultiset([string[]]$Actual, [string[]]$Expected) {
    $left = @($Actual | ForEach-Object { $_.ToLowerInvariant() } | Sort-Object)
    $right = @($Expected | ForEach-Object { $_.ToLowerInvariant() } | Sort-Object)
    return $left.Count -eq $right.Count -and @(Compare-Object $right $left).Count -eq 0
}

function Compare-PathMultiset([string[]]$Actual, [string[]]$Expected) {
    $left = @($Actual | ForEach-Object { [IO.Path]::GetFullPath($_).ToLowerInvariant() } | Sort-Object)
    $right = @($Expected | ForEach-Object { [IO.Path]::GetFullPath($_).ToLowerInvariant() } | Sort-Object)
    return $left.Count -eq $right.Count -and @(Compare-Object $right $left).Count -eq 0
}

function Get-SwitchValue([string]$Argument, [string]$Prefix) {
    if (-not $Argument.StartsWith($Prefix, [StringComparison]::OrdinalIgnoreCase)) { return $null }
    return $Argument.Substring($Prefix.Length)
}

function Validate-CompileRecord(
    [string]$Basename,
    [string]$Source,
    [string]$Command) {
    $role = "$Basename compiler command"
    $args = @(Convert-WindowsCommandLine $Command $role)
    $single = @{}
    $includes = [Collections.Generic.List[string]]::new()
    $externalIncludes = [Collections.Generic.List[string]]::new()
    $definitions = [Collections.Generic.List[string]]::new()
    $positionals = [Collections.Generic.List[string]]::new()
    $pathmaps = [Collections.Generic.List[object]]::new()
    $foValue = $null
    $fdValue = $null
    $requiredSingles = @('/c','/nologo','/W1','/WX-','/diagnostics:column','/EHsc','/MT','/O2','/Ob2',
        '/fp:precise','/std:c++17','/external:W0','/TP','/utf-8','/Brepro',
        '/experimental:deterministic')
    for ($i = 0; $i -lt $args.Count; $i++) {
        $arg = $args[$i]
        if ($arg.StartsWith('@')) { throw "$role compiler response files are forbidden" }
        if ($arg.StartsWith('/FI', [StringComparison]::OrdinalIgnoreCase)) {
            throw "$role forced include is forbidden"
        }
        if ($arg -match '(?i)^/(FA|Fa|Fe|Fm|Fi|FR)') {
            throw "$role compile output switch set mismatch"
        }
        if ($arg.Equals('/D', [StringComparison]::OrdinalIgnoreCase)) {
            if (++$i -ge $args.Count) { throw "$role dangling /D" }
            $definitions.Add($args[$i]); continue
        }
        if ($arg.Equals('/external:I', [StringComparison]::OrdinalIgnoreCase)) {
            if (++$i -ge $args.Count) { throw "$role dangling /external:I" }
            $externalIncludes.Add((Resolve-BuildPath $args[$i])); continue
        }
        if ($arg.StartsWith('/I', [StringComparison]::OrdinalIgnoreCase)) {
            $value = $arg.Substring(2)
            if ([string]::IsNullOrWhiteSpace($value)) { throw "$role compile include set mismatch" }
            $includes.Add((Resolve-BuildPath $value)); continue
        }
        $value = Get-SwitchValue $arg '/Fo'
        if ($null -ne $value) {
            if ($null -ne $foValue -or [string]::IsNullOrWhiteSpace($value)) {
                throw "$role compile output switch set mismatch"
            }
            $foValue = $value; continue
        }
        $value = Get-SwitchValue $arg '/Fd'
        if ($null -ne $value) {
            if ($null -ne $fdValue -or [string]::IsNullOrWhiteSpace($value)) {
                throw "$role compile output switch set mismatch"
            }
            $fdValue = $value; continue
        }
        if ($arg.StartsWith('/pathmap:', [StringComparison]::OrdinalIgnoreCase)) {
            $mapping = $arg.Substring('/pathmap:'.Length)
            $equals = $mapping.LastIndexOf('=')
            if ($equals -le 0 -or $equals -eq $mapping.Length - 1) {
                throw "$role compiler pathmap left side mismatch"
            }
            $pathmaps.Add([ordered]@{ Left=Resolve-BuildPath $mapping.Substring(0,$equals); Right=$mapping.Substring($equals+1) })
            continue
        }
        if ($arg.StartsWith('/Brepro', [StringComparison]::OrdinalIgnoreCase) -and
            -not $arg.Equals('/Brepro', [StringComparison]::OrdinalIgnoreCase)) {
            throw "$role is missing source or deterministic Task 6 flags"
        }
        if ($arg.StartsWith('/')) {
            $known = @($requiredSingles | Where-Object { $arg.Equals($_, [StringComparison]::OrdinalIgnoreCase) })
            if ($known.Count -ne 1) { throw "$role unexpected compile switch: $arg" }
            $key = $known[0].ToLowerInvariant()
            if ($single.ContainsKey($key)) { throw "$role duplicate compile switch: $arg" }
            $single[$key] = $true
            continue
        }
        $positionals.Add((Resolve-BuildPath $arg))
    }
    foreach ($required in $requiredSingles) {
        if (-not $single.ContainsKey($required.ToLowerInvariant())) {
            throw "$role is missing source or deterministic Task 6 flags"
        }
    }
    $expectedDefinitions = @('_MBCS','WIN32','_WINDOWS','NDEBUG','NOMINMAX','WIN32_LEAN_AND_MEAN','CMAKE_INTDIR="Release"')
    if (-not (Compare-TextMultiset @($definitions) $expectedDefinitions)) {
        throw "$role compile definition set mismatch"
    }
    $expectedIncludes = @('src','src\pdq_upstream','third_party') | ForEach-Object {
        [IO.Path]::GetFullPath((Join-Path $script:SourceRootFull $_))
    }
    if (-not (Compare-PathMultiset @($includes) @($expectedIncludes))) {
        throw "$role compile include set mismatch"
    }
    $expectedExternal = @(
        (Join-Path $script:VcpkgTripletRootFull 'include'),
        (Join-Path $script:VcpkgTripletRootFull 'include\webp'))
    if (-not (Compare-PathMultiset @($externalIncludes) $expectedExternal)) {
        throw "$role compile external include set mismatch"
    }
    if ($positionals.Count -ne 1 -or
        -not $positionals[0].Equals($Source, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$role compile positional input mismatch"
    }
    $expectedFo = Join-Path $script:CanonicalImageObjectDirFull "$Basename.obj"
    $expectedFd = Join-Path $script:CanonicalImageObjectDirFull 'videocore_image_algorithms.pdb'
    if ($null -eq $foValue -or $null -eq $fdValue -or
        -not (Resolve-BuildPath $foValue).Equals($expectedFo, [StringComparison]::OrdinalIgnoreCase) -or
        -not (Resolve-BuildPath $fdValue).Equals($expectedFd, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$role compile output switch set mismatch"
    }
    if ($pathmaps.Count -ne 2) { throw "$role compiler pathmap left side mismatch" }
    $objectMaps = @($pathmaps | Where-Object { $_.Right -ceq 'VC_IMAGE_OBJECTS' })
    $sourceMaps = @($pathmaps | Where-Object { $_.Right -ceq 'VC_SOURCE_ROOT' })
    if ($objectMaps.Count -ne 1 -or $sourceMaps.Count -ne 1 -or
        -not $objectMaps[0].Left.Equals($script:CanonicalImageObjectDirFull, [StringComparison]::OrdinalIgnoreCase) -or
        -not $sourceMaps[0].Left.Equals($script:SourceRootFull, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$role compiler pathmap left side mismatch"
    }
    return [ordered]@{ Basename=$Basename; Source=$Source; Args=$args; Fo=$foValue; Fd=$fdValue }
}

function Expand-LinkArguments([string]$Command, [string]$Role) {
    $initial = @(Convert-WindowsCommandLine $Command $Role)
    $expanded = [Collections.Generic.List[string]]::new()
    foreach ($arg in $initial) {
        if (-not $arg.StartsWith('@')) { $expanded.Add($arg); continue }
        $rspValue = $arg.Substring(1)
        if ([string]::IsNullOrWhiteSpace($rspValue)) { throw "$Role unresolved link response file" }
        $rsp = Resolve-BuildPath $rspValue
        if (-not (Test-Within $rsp $script:BuildRootFull)) {
            throw "$Role link response file escapes BuildRoot"
        }
        $rspFile = Require-File $rsp "$Role link response file"
        if ($rspFile.Length -gt 1048576) { throw "$Role link response file is too large" }
        $rspArgs = @(Convert-WindowsCommandLine ([IO.File]::ReadAllText($rspFile.FullName)) "$Role response file")
        foreach ($rspArg in $rspArgs) {
            if ($rspArg.StartsWith('@')) { throw "$Role nested link response files are forbidden" }
            $expanded.Add($rspArg)
        }
    }
    return @($expanded)
}

function Select-CurrentExactLinkRecord(
    [string]$TargetDir,
    [string[]]$ExpectedObjects,
    [string]$Role) {
    $matches = @(Get-ChildItem -LiteralPath $TargetDir -Recurse -File -Filter 'link.command.1.tlog')
    if ($matches.Count -ne 1) { throw "$Role expected exactly one link.command.1.tlog, got $($matches.Count)" }
    $records = @(Get-TlogRecords $matches[0].FullName "$Role link.command")
    $exact = [Collections.Generic.List[object]]::new()
    for ($i = 0; $i -lt $records.Count; $i++) {
        $rootObjects = @($records[$i].Root.Split('|') | Where-Object { $_ -match '(?i)\.obj$' } |
            ForEach-Object { Resolve-BuildPath $_ })
        if (Compare-PathMultiset $rootObjects $ExpectedObjects) {
            $exact.Add([ordered]@{ Index=$i; Record=$records[$i] })
        }
    }
    if ($exact.Count -ne 1) { throw "$Role expected one exact allowlist link record, got $($exact.Count)" }
    if ($exact[0].Index -ne ($records.Count - 1)) {
        throw "$Role exact link record is stale; the current final record is incomplete"
    }
    return $exact[0].Record
}

function Get-ExpectedLinkLibraries([bool]$IsDll) {
    $libraries = [Collections.Generic.List[string]]::new()
    if ($IsDll) {
        foreach ($name in @('avformat.lib','avcodec.lib','avutil.lib','swscale.lib')) {
            $libraries.Add('path:' + [IO.Path]::GetFullPath((Join-Path (Split-Path $script:SourceRootFull -Parent) "third_party\ffmpeg\lib\$name")).ToLowerInvariant())
        }
    }
    foreach ($name in @('turbojpeg.lib','libpng16.lib','zs.lib','libwebp.lib','libsharpyuv.lib')) {
        $libraries.Add('path:' + [IO.Path]::GetFullPath((Join-Path $script:VcpkgTripletRootFull "lib\$name")).ToLowerInvariant())
    }
    foreach ($name in @('shlwapi.lib','ole32.lib','windowscodecs.lib','kernel32.lib','user32.lib',
        'gdi32.lib','winspool.lib','shell32.lib','ole32.lib','oleaut32.lib','uuid.lib','comdlg32.lib','advapi32.lib')) {
        $libraries.Add('system:' + $name)
    }
    return @($libraries)
}

function Get-LinkLibraryIdentity([string]$Argument) {
    if ([IO.Path]::GetFileName($Argument) -ceq $Argument) {
        return 'system:' + $Argument.ToLowerInvariant()
    }
    return 'path:' + (Resolve-BuildPath $Argument).ToLowerInvariant()
}

function Validate-LinkRecord(
    [Collections.IDictionary]$Consumer,
    [object]$Record) {
    $role = $Consumer.Name
    $command = ($Record.Body -join ' ').Trim()
    $args = @(Expand-LinkArguments $command "$role link command")
    $objects = [Collections.Generic.List[string]]::new()
    $libraries = [Collections.Generic.List[string]]::new()
    $switches = @{}
    $outValue = $null; $pdbValue = $null; $implibValue = $null; $defValue = $null
    foreach ($arg in $args) {
        if ($arg.StartsWith('/')) {
            $value = Get-SwitchValue $arg '/OUT:'
            if ($null -ne $value) { if ($null -ne $outValue) { throw "$role duplicate /OUT" }; $outValue=$value; continue }
            $value = Get-SwitchValue $arg '/PDB:'
            if ($null -ne $value) { if ($null -ne $pdbValue) { throw "$role duplicate /PDB" }; $pdbValue=$value; continue }
            $value = Get-SwitchValue $arg '/IMPLIB:'
            if ($null -ne $value) { if ($null -ne $implibValue) { throw "$role duplicate /IMPLIB" }; $implibValue=$value; continue }
            $value = Get-SwitchValue $arg '/DEF:'
            if ($null -ne $value) { if ($null -ne $defValue) { throw "$role duplicate /DEF" }; $defValue=$value; continue }
            if ($arg -match '(?i)^/(WHOLEARCHIVE|DEFAULTLIB|ASSEMBLYMODULE|ASSEMBLYRESOURCE|INCLUDE|EXPORT|ENTRY|LIBPATH):') {
                throw "$role unresolved object-bearing link switch: $arg"
            }
            $allowed = @('/INCREMENTAL:NO','/NOLOGO','/MANIFEST',
                "/MANIFESTUAC:level='asInvoker' uiAccess='false'",'/manifest:embed',
                '/SUBSYSTEM:CONSOLE','/TLBID:1','/MACHINE:X64','/Brepro')
            if ($Consumer.IsDll) { $allowed += '/DLL' }
            $known = @($allowed | Where-Object { $arg.Equals($_, [StringComparison]::OrdinalIgnoreCase) })
            if ($known.Count -ne 1) { throw "$role unexpected link switch: $arg" }
            $key = $known[0].ToLowerInvariant()
            if ($switches.ContainsKey($key)) { throw "$role duplicate link switch: $arg" }
            $switches[$key] = $true
            continue
        }
        if ($arg -match '(?i)\.obj$') { $objects.Add((Resolve-BuildPath $arg)); continue }
        if ($arg -match '(?i)\.lib$') { $libraries.Add((Get-LinkLibraryIdentity $arg)); continue }
        if ($arg -match '(?i)\.(res|exp|netmodule|winmd|dll)$') {
            throw "$role unresolved object-bearing link input: $arg"
        }
        throw "$role unexpected positional link input: $arg"
    }
    if (-not (Compare-PathMultiset @($objects) @($Consumer.Expected))) {
        throw "$role actual object input set mismatch"
    }
    $expectedLibraries = @(Get-ExpectedLinkLibraries $Consumer.IsDll)
    if (-not (Compare-TextMultiset @($libraries) $expectedLibraries)) {
        throw "$role actual library input set mismatch"
    }
    $required = @('/incremental:no','/nologo','/manifest',
        "/manifestuac:level='asinvoker' uiaccess='false'",'/manifest:embed',
        '/subsystem:console','/tlbid:1','/machine:x64','/brepro')
    if ($Consumer.IsDll) { $required += '/dll' }
    foreach ($item in $required) {
        if (-not $switches.ContainsKey($item)) { throw "$role link command is missing $item" }
    }
    if ($null -eq $outValue -or $null -eq $pdbValue -or $null -eq $implibValue -or
        -not (Resolve-BuildPath $outValue).Equals($Consumer.Pe.FullName, [StringComparison]::OrdinalIgnoreCase) -or
        -not (Resolve-BuildPath $pdbValue).Equals($Consumer.ExpectedPdb, [StringComparison]::OrdinalIgnoreCase) -or
        -not (Resolve-BuildPath $implibValue).Equals($Consumer.ExpectedImplib, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$role link output switch binding mismatch"
    }
    if ($Consumer.IsDll) {
        $expectedDef = Join-Path $script:SourceRootFull 'exports.def'
        if ($null -eq $defValue -or
            -not (Resolve-BuildPath $defValue).Equals($expectedDef, [StringComparison]::OrdinalIgnoreCase)) {
            throw "$role link DEF input mismatch"
        }
    } elseif ($null -ne $defValue) { throw "$role unexpected link DEF input" }
    return [ordered]@{ Args=$args; Objects=@($objects); Libraries=@($libraries) }
}

function Require-MicrosoftTool([string]$Path, [string]$ExpectedName, [string]$Role) {
    if (-not [IO.Path]::IsPathRooted($Path)) { throw "$Role must be an absolute path" }
    $file = Require-File $Path $Role
    if (-not $file.Name.Equals($ExpectedName, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Role basename mismatch"
    }
    $signature = Get-AuthenticodeSignature -LiteralPath $file.FullName
    if ($signature.Status -ne [Management.Automation.SignatureStatus]::Valid -or
        $signature.SignerCertificate.Subject -notmatch '(?i)\bMicrosoft Corporation\b') {
        throw "$Role is not a valid Microsoft-signed tool"
    }
    return $file
}

function Get-TrustedVsEnvironment([string]$CmdPath) {
    $cmd = Require-MicrosoftTool $CmdPath 'cmd.exe' 'Windows command processor'
    $raw = 'call "' + $script:VsDevCmdFull + '" -arch=x64 -host_arch=x64 >nul && set'
    $lines = @(& $cmd.FullName /d /s /c $raw 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "VsDevCmd environment capture failed with exit=$LASTEXITCODE" }
    $environment = [Collections.Generic.Dictionary[string,string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($line in $lines) {
        $text = [string]$line
        $equals = $text.IndexOf('=')
        if ($equals -le 0) { continue }
        $name = $text.Substring(0,$equals)
        $value = $text.Substring($equals+1)
        # cmd.exe can expose both inherited `Path` and VsDevCmd's enriched
        # `PATH`.  Keep the richer value instead of allowing case-folding to
        # overwrite the trusted toolchain path with the shorter inherited one.
        if (-not $environment.ContainsKey($name) -or
            $value.Length -gt $environment[$name].Length) {
            $environment[$name] = $value
        }
    }
    foreach ($name in @('CL','_CL_','LINK','_LINK_')) { [void]$environment.Remove($name) }
    foreach ($name in @('PATH','INCLUDE','LIB','LIBPATH')) {
        if (-not $environment.ContainsKey($name)) { throw "VsDevCmd did not provide $name" }
        $trusted = [Collections.Generic.List[string]]::new()
        foreach ($entry in $environment[$name].Split(';')) {
            $candidate = $entry.Trim().Trim('"')
            if ([string]::IsNullOrWhiteSpace($candidate) -or -not [IO.Path]::IsPathRooted($candidate)) { continue }
            $full = [IO.Path]::GetFullPath($candidate).TrimEnd('\')
            if (-not (Test-Path -LiteralPath $full -PathType Container)) { continue }
            $trustedRoots = @(
                $script:VsInstallRoot,
                [IO.Path]::GetFullPath($env:SystemRoot),
                [IO.Path]::GetFullPath(${env:ProgramFiles(x86)} + '\Windows Kits'),
                [IO.Path]::GetFullPath(${env:ProgramFiles(x86)} + '\Microsoft SDKs'))
            if (-not @($trustedRoots | Where-Object { Test-Within $full $_ }).Count) { continue }
            if (-not $trusted.Contains($full)) { $trusted.Add($full) }
        }
        if ($trusted.Count -eq 0) { throw "VsDevCmd $name has no trusted absolute entries" }
        $environment[$name] = $trusted -join ';'
    }
    if (@($environment['PATH'].Split(';') | Where-Object {
        $_.Equals($script:CompilerDirectory, [StringComparison]::OrdinalIgnoreCase)
    }).Count -eq 0) { throw 'trusted PATH does not contain the validated compiler directory' }
    return $environment
}

function Resolve-SystemLibrary([string]$Name, [Collections.IDictionary]$Environment, [string]$Role) {
    $matches = [Collections.Generic.List[string]]::new()
    foreach ($directory in $Environment['LIB'].Split(';')) {
        $candidate = Join-Path $directory $Name
        if (Test-Path -LiteralPath $candidate -PathType Leaf) { $matches.Add([IO.Path]::GetFullPath($candidate)) }
    }
    if ($matches.Count -ne 1) { throw "$Role expected one trusted resolution for $Name, got $($matches.Count)" }
    return $matches[0]
}

function Invoke-DirectTool(
    [IO.FileInfo]$Tool,
    [string[]]$Arguments,
    [string]$Role,
    [string]$WorkingDirectory,
    [Collections.IDictionary]$Environment) {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $Tool.FullName
    $info.WorkingDirectory = $WorkingDirectory
    $info.UseShellExecute = $false
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $info.CreateNoWindow = $true
    $info.Environment.Clear()
    foreach ($entry in $Environment.GetEnumerator()) { $info.Environment[$entry.Key] = $entry.Value }
    $processTemp = Join-Path $WorkingDirectory 'process-temp'
    New-Item -ItemType Directory -Force -Path $processTemp | Out-Null
    $info.Environment['TEMP'] = $processTemp
    $info.Environment['TMP'] = $processTemp
    foreach ($argument in $Arguments) { [void]$info.ArgumentList.Add($argument) }
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $info
    if (-not $process.Start()) { throw "$Role failed to start" }
    $stdout = $process.StandardOutput.ReadToEndAsync()
    $stderr = $process.StandardError.ReadToEndAsync()
    $process.WaitForExit()
    $output = $stdout.Result + $stderr.Result
    if ($process.ExitCode -ne 0) { throw "$Role failed with exit=$($process.ExitCode)`n$output" }
}

function New-CompileReplay(
    [Collections.IDictionary]$Spec,
    [string]$ReplayRoot) {
    $args = [Collections.Generic.List[string]]::new()
    $rawFo = $Spec.Fo
    $rawFd = $Spec.Fd
    if ([IO.Path]::IsPathRooted($rawFo)) {
        $objectPath = Join-Path $ReplayRoot ($Spec.Basename + '.obj')
        $foArg = '/Fo' + $objectPath
    } else {
        # MSBuild upper-cases its TLog serialization, but cl.exe receives the
        # VS_SETTINGS spelling.  Reconstruct that reviewed relative spelling
        # so __FILE__/COFF debug paths and lambda identities match production.
        $configuration = Split-Path $script:CanonicalImageObjectDirFull -Leaf
        $canonicalRelativeFo = "videocore_image_algorithms.dir\$configuration\$($Spec.Basename).obj"
        $objectPath = [IO.Path]::GetFullPath((Join-Path $ReplayRoot $canonicalRelativeFo))
        $foArg = '/Fo' + $canonicalRelativeFo
    }
    if ([IO.Path]::IsPathRooted($rawFd)) {
        $pdbPath = Join-Path $ReplayRoot 'videocore_image_algorithms.pdb'
        $fdArg = '/Fd' + $pdbPath
    } else {
        $configuration = Split-Path $script:CanonicalImageObjectDirFull -Leaf
        $canonicalRelativeFd = "videocore_image_algorithms.dir\$configuration\videocore_image_algorithms.pdb"
        $pdbPath = [IO.Path]::GetFullPath((Join-Path $ReplayRoot $canonicalRelativeFd))
        $fdArg = '/Fd' + $canonicalRelativeFd
    }
    New-Item -ItemType Directory -Force -Path (Split-Path $objectPath -Parent),(Split-Path $pdbPath -Parent) | Out-Null
    for ($i = 0; $i -lt $Spec.Args.Count; $i++) {
        $arg = $Spec.Args[$i]
        if ($arg.StartsWith('/Fo', [StringComparison]::OrdinalIgnoreCase)) { $args.Add($foArg); continue }
        if ($arg.StartsWith('/Fd', [StringComparison]::OrdinalIgnoreCase)) { $args.Add($fdArg); continue }
        if ($arg.StartsWith('/pathmap:', [StringComparison]::OrdinalIgnoreCase) -and
            $arg.EndsWith('=VC_IMAGE_OBJECTS', [StringComparison]::Ordinal)) {
            $args.Add('/pathmap:' + (Split-Path $objectPath -Parent) + '=VC_IMAGE_OBJECTS'); continue
        }
        if ($arg.StartsWith('/I', [StringComparison]::OrdinalIgnoreCase) -and
            -not $arg.Equals('/external:I', [StringComparison]::OrdinalIgnoreCase)) {
            $resolved = Resolve-BuildPath $arg.Substring(2)
            foreach ($relative in @('src','src\pdq_upstream','third_party')) {
                $canonical = [IO.Path]::GetFullPath((Join-Path $script:SourceRootFull $relative))
                if ($resolved.Equals($canonical, [StringComparison]::OrdinalIgnoreCase)) {
                    $arg = '/I' + $canonical; break
                }
            }
            $args.Add($arg); continue
        }
        if (-not $arg.StartsWith('/') -and -not $arg.StartsWith('@') -and
            (Resolve-BuildPath $arg).Equals($Spec.Source, [StringComparison]::OrdinalIgnoreCase)) {
            $args.Add($Spec.Source); continue
        }
        $args.Add($arg)
    }
    return [ordered]@{ Args=@($args); ObjectPath=$objectPath }
}

function New-LinkReplay(
    [Collections.IDictionary]$Spec,
    [Collections.IDictionary]$Consumer,
    [string]$ReplayRoot,
    [Collections.IDictionary]$Environment) {
    $args = [Collections.Generic.List[string]]::new()
    $out = Join-Path $ReplayRoot $Consumer.OutName
    $pdb = Join-Path $ReplayRoot $Consumer.PdbName
    $implib = Join-Path $ReplayRoot $Consumer.ImplibName
    foreach ($arg in $Spec.Args) {
        if ($arg.StartsWith('/OUT:', [StringComparison]::OrdinalIgnoreCase)) { $args.Add('/OUT:' + $out); continue }
        if ($arg.StartsWith('/PDB:', [StringComparison]::OrdinalIgnoreCase)) { $args.Add('/PDB:' + $pdb); continue }
        if ($arg.StartsWith('/IMPLIB:', [StringComparison]::OrdinalIgnoreCase)) { $args.Add('/IMPLIB:' + $implib); continue }
        if ($arg.StartsWith('/DEF:', [StringComparison]::OrdinalIgnoreCase)) {
            $args.Add('/DEF:' + (Resolve-BuildPath $arg.Substring('/DEF:'.Length))); continue
        }
        if ($arg -match '(?i)\.obj$') { $args.Add((Resolve-BuildPath $arg)); continue }
        if ($arg -match '(?i)\.lib$') {
            if ([IO.Path]::GetFileName($arg) -ceq $arg) {
                $args.Add((Resolve-SystemLibrary $arg $Environment "$($Consumer.Name) link replay"))
            } else { $args.Add((Resolve-BuildPath $arg)) }
            continue
        }
        $args.Add($arg)
    }
    return [ordered]@{ Args=@($args); Output=$out }
}

$script:SourceRootFull = [IO.Path]::GetFullPath($SourceRoot)
$legacySourceRootFull = [IO.Path]::GetFullPath($LegacySourceRoot)
$script:BuildRootFull = [IO.Path]::GetFullPath($BuildRoot)
# vcpkg 依赖根以构建时 CMakeCache 记录的 VCPKG_INSTALLED_DIR 为准（标准缓存布局为
# 共享的 C:\vcpkg\installed）；缓存条目缺失时回退到 build 本地 vcpkg_installed（manifest 布局）。
$script:VcpkgInstalledRootFull = Join-Path $script:BuildRootFull 'vcpkg_installed'
$script:VcpkgTriplet = 'x64-windows-static'
$cmakeCachePath = Join-Path $script:BuildRootFull 'CMakeCache.txt'
if (Test-Path -LiteralPath $cmakeCachePath -PathType Leaf) {
    $installedMatch = Select-String -LiteralPath $cmakeCachePath -Pattern '^VCPKG_INSTALLED_DIR:PATH=(.+)$' | Select-Object -First 1
    if ($installedMatch) {
        $script:VcpkgInstalledRootFull = [IO.Path]::GetFullPath($installedMatch.Matches[0].Groups[1].Value.Trim())
    }
    $tripletMatch = Select-String -LiteralPath $cmakeCachePath -Pattern '^VCPKG_TARGET_TRIPLET:STRING=(.+)$' | Select-Object -First 1
    if ($tripletMatch) {
        $script:VcpkgTriplet = $tripletMatch.Matches[0].Groups[1].Value.Trim()
    }
}
$script:VcpkgTripletRootFull = Join-Path $script:VcpkgInstalledRootFull $script:VcpkgTriplet
$script:CanonicalImageObjectDirFull = [IO.Path]::GetFullPath($CanonicalImageObjectDir)
$compilerFile = Require-MicrosoftTool $Compiler 'cl.exe' 'MSVC compiler'
$script:CompilerDirectory = $compilerFile.Directory.FullName.TrimEnd('\')
$linkerFile = Require-MicrosoftTool (Join-Path $script:CompilerDirectory 'link.exe') 'link.exe' 'MSVC linker'
[void](Require-MicrosoftTool $Dumpbin 'dumpbin.exe' 'MSVC dumpbin')
$vsDevCmdFile = Require-File $VsDevCmd 'VsDevCmd'
if (-not $vsDevCmdFile.Name.Equals('VsDevCmd.bat', [StringComparison]::OrdinalIgnoreCase)) {
    throw 'VsDevCmd basename mismatch'
}
$vsMarker = $compilerFile.FullName.IndexOf('\VC\Tools\', [StringComparison]::OrdinalIgnoreCase)
if ($vsMarker -lt 0) { throw 'validated compiler path is outside a Visual Studio VC Tools tree' }
$vsInstallRoot = $compilerFile.FullName.Substring(0,$vsMarker)
if (-not (Test-Within $vsDevCmdFile.FullName $vsInstallRoot)) { throw 'VsDevCmd is outside the validated Visual Studio installation' }
$script:VsInstallRoot = $vsInstallRoot
$script:VsDevCmdFull = $vsDevCmdFile.FullName

if (-not (Test-Path -LiteralPath $ImageObjectDir -PathType Container)) {
    throw "shared image OBJECT library directory is missing: $ImageObjectDir"
}
$objects = @(Get-ChildItem -LiteralPath $ImageObjectDir -File -Filter '*.obj' | Sort-Object BaseName)
$actualBasenames = @($objects.BaseName | ForEach-Object { $_.ToLowerInvariant() } | Sort-Object)
$expectedBasenames = @($sourceByObject.Keys | Sort-Object)
if ($objects.Count -ne 11 -or @(Compare-Object $expectedBasenames $actualBasenames).Count -ne 0) {
    throw "exact shared image object set mismatch: expected=$($expectedBasenames -join ',') actual=$($actualBasenames -join ',')"
}
foreach ($object in $objects) { Require-X64CoffObject $object "shared image object $($object.BaseName)" }

$vendorCounts = [ordered]@{
    pdq = Require-ByteIdenticalTree (Join-Path $legacySourceRootFull 'src\pdq_upstream') (Join-Path $script:SourceRootFull 'src\pdq_upstream') 'pdq_upstream'
    stb = Require-ByteIdenticalTree (Join-Path $legacySourceRootFull 'third_party\stb') (Join-Path $script:SourceRootFull 'third_party\stb') 'stb'
    luma = Require-ByteIdenticalTree (Join-Path $legacySourceRootFull 'testdata\luma') (Join-Path $script:SourceRootFull 'testdata\luma') 'luma'
    level_b = Require-ByteIdenticalTree (Join-Path $legacySourceRootFull 'testdata\level_b') (Join-Path $script:SourceRootFull 'testdata\level_b') 'level_b'
}

$compilerTlogPath = $CompilerCommandTlog
if (Test-Path -LiteralPath $compilerTlogPath -PathType Container) {
    $compilerTlogs = @(Get-ChildItem -LiteralPath $compilerTlogPath -Recurse -File -Filter 'CL.command.1.tlog')
    if ($compilerTlogs.Count -ne 1) { throw "expected exactly one image CL.command.1.tlog, got $($compilerTlogs.Count)" }
    $compilerTlogPath = $compilerTlogs[0].FullName
}
$compileRecords = @(Get-TlogRecords $compilerTlogPath 'image compiler command TLog')
$compileBySource = @{}
foreach ($record in $compileRecords) {
    $rootParts = @($record.Root.Split('|') | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    if ($rootParts.Count -ne 1) { throw 'image compiler record must have exactly one rooted source' }
    $root = Resolve-BuildPath $rootParts[0]
    $key = $root.ToLowerInvariant()
    if ($compileBySource.ContainsKey($key)) { throw "duplicate compiler record for source: $root" }
    $compileBySource[$key] = ($record.Body -join ' ').Trim()
}
if ($compileBySource.Count -ne 11) { throw "image compiler TLog exact source set mismatch: count=$($compileBySource.Count)" }
$compileSpecs = [Collections.Generic.List[object]]::new()
foreach ($basename in $sourceByObject.Keys) {
    $source = [IO.Path]::GetFullPath((Join-Path $script:SourceRootFull $sourceByObject[$basename]))
    $key = $source.ToLowerInvariant()
    if (-not $compileBySource.ContainsKey($key)) { throw "compiler TLog has no exact record for $source" }
    $compileSpecs.Add((Validate-CompileRecord $basename $source $compileBySource[$key]))
}

$dllPe = Require-File $DllBinary 'videocore.dll final PE'
$compatPe = Require-File $CompatBinary 'compatibility executable final PE'
Require-X64Pe $dllPe $true 'videocore.dll'
Require-X64Pe $compatPe $false 'test_vc_image_compat.exe'
$sharedPaths = @($objects.FullName)
$consumers = @(
    [ordered]@{
        Name='videocore.dll'; IsDll=$true; TargetDir=$DllTargetDir; Pe=$dllPe
        Expected=@($sharedPaths) + @($dllPrivateObjects | ForEach-Object { Join-Path $DllTargetDir "$_.obj" })
        ExpectedPdb=[IO.Path]::ChangeExtension($dllPe.FullName,'.pdb')
        ExpectedImplib=[IO.Path]::ChangeExtension($dllPe.FullName,'.lib')
        OutName='videocore.dll'; PdbName='videocore.pdb'; ImplibName='videocore.lib'
    },
    [ordered]@{
        Name='test_vc_image_compat.exe'; IsDll=$false; TargetDir=$CompatTargetDir; Pe=$compatPe
        Expected=@($sharedPaths) + @($compatPrivateObjects | ForEach-Object { Join-Path $CompatTargetDir "$_.obj" })
        ExpectedPdb=[IO.Path]::ChangeExtension($compatPe.FullName,'.pdb')
        ExpectedImplib=[IO.Path]::ChangeExtension($compatPe.FullName,'.lib')
        OutName='test_vc_image_compat.exe'; PdbName='test_vc_image_compat.pdb'; ImplibName='test_vc_image_compat.lib'
    })
$linkSpecs = [Collections.Generic.List[object]]::new()
foreach ($consumer in $consumers) {
    foreach ($path in $consumer.Expected) {
        $input = Require-File $path "$($consumer.Name) allowlisted input"
        Require-X64CoffObject $input "$($consumer.Name) allowlisted input"
    }
    $record = Select-CurrentExactLinkRecord $consumer.TargetDir $consumer.Expected $consumer.Name
    $linkSpecs.Add((Validate-LinkRecord $consumer $record))
}

$tempBase = $script:BuildRootFull.TrimEnd('\') + '\'
$tempFull = [IO.Path]::GetFullPath((Join-Path $script:BuildRootFull ('videocore-deterministic-provenance-' + [guid]::NewGuid().ToString('N'))))
if (-not $tempFull.StartsWith($tempBase, [StringComparison]::OrdinalIgnoreCase)) {
    throw "refusing unsafe deterministic provenance temp root: $tempFull"
}

try {
    New-Item -ItemType Directory -Path $tempFull | Out-Null
    $environment = Get-TrustedVsEnvironment (Join-Path $env:SystemRoot 'System32\cmd.exe')
    $compileRoot = Join-Path $tempFull 'compile'
    $linkRoot = Join-Path $tempFull 'link'
    New-Item -ItemType Directory -Path $compileRoot,$linkRoot | Out-Null
    $compileMismatches = [Collections.Generic.List[string]]::new()
    foreach ($spec in $compileSpecs) {
        $replay = New-CompileReplay $spec $compileRoot
        Invoke-DirectTool $compilerFile $replay.Args "$($spec.Basename) deterministic compile replay" $compileRoot $environment
        $currentObject = Join-Path $ImageObjectDir "$($spec.Basename).obj"
        $rebuiltHash = Sha256-File $replay.ObjectPath
        $currentHash = Sha256-File $currentObject
        if ($rebuiltHash -cne $currentHash) {
            $compileMismatches.Add("$($spec.Basename)(current=$currentHash rebuilt=$rebuiltHash)")
        }
    }
    if ($compileMismatches.Count -ne 0) {
        throw "deterministic compile replay differs from current objects: $($compileMismatches -join '; ')"
    }
    for ($i = 0; $i -lt $consumers.Count; $i++) {
        $consumer = $consumers[$i]
        $replay = New-LinkReplay $linkSpecs[$i] $consumer $linkRoot $environment
        Invoke-DirectTool $linkerFile $replay.Args "$($consumer.Name) deterministic link replay" $linkRoot $environment
        $rebuilt = Require-File $replay.Output "$($consumer.Name) replay output"
        Require-X64Pe $rebuilt $consumer.IsDll "$($consumer.Name) replay output"
        if ((Sha256-File $rebuilt.FullName) -cne (Sha256-File $consumer.Pe.FullName)) {
            throw "$($consumer.Name) deterministic link replay differs from current PE"
        }
    }
    $objectHashLines = @($objects | ForEach-Object {
        "$($_.BaseName.ToLowerInvariant())=$(Sha256-File $_.FullName)" })
    $objectSetHash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData(
        [Text.UTF8Encoding]::new($false).GetBytes(($objectHashLines -join "`n") + "`n"))).ToLowerInvariant()
    Write-Output "IMAGE_OBJECT_PROVENANCE PASS objects=11 source_to_coff=recompiled_byte_identical pe_relinked_byte_identical=dll,exe exact_current_link_records=true safe_argv=compile,link direct_tools=absolute_cl,sibling_link vendor_tree_byte_identical=pdq:$($vendorCounts.pdq),stb:$($vendorCounts.stb),luma:$($vendorCounts.luma),level_b:$($vendorCounts.level_b) object_set_sha256=$objectSetHash dll_sha256=$(Sha256-File $dllPe.FullName) compat_sha256=$(Sha256-File $compatPe.FullName)"
} finally {
    if ((Test-Path -LiteralPath $tempFull) -and
        $tempFull.StartsWith($tempBase, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $tempFull -Recurse -Force
    }
}
