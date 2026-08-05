param(
    [Parameter(Mandatory = $true)] [string]$GateScript,
    [Parameter(Mandatory = $true)] [string]$CurrentImageObjectDir,
    [Parameter(Mandatory = $true)] [string]$CurrentDllBinary,
    [Parameter(Mandatory = $true)] [string]$CurrentCompatBinary,
    [Parameter(Mandatory = $true)] [string]$Dumpbin,
    [Parameter(Mandatory = $true)] [string]$SourceRoot,
    [Parameter(Mandatory = $true)] [string]$Compiler,
    [Parameter(Mandatory = $true)] [string]$VsDevCmd,
    [Parameter(Mandatory = $true)] [string]$CompilerCommandTlog,
    [Parameter(Mandatory = $true)] [string]$BuildRoot
)

$ErrorActionPreference = 'Stop'
$tempBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
$caseRoot = Join-Path $tempBase ("videocore-object-provenance-mutation-" + [guid]::NewGuid().ToString('N'))
$caseFull = [IO.Path]::GetFullPath($caseRoot)
if (-not $caseFull.StartsWith($tempBase, [StringComparison]::OrdinalIgnoreCase)) {
    throw "refusing unsafe object-provenance mutation root: $caseFull"
}

$expectedObjects = @(
    'downscaling', 'image_decode', 'pdq', 'pdqhamming', 'pdqhashing',
    'pdqhashtypes', 'pdqutils', 'phash_parts', 'sobel_hist', 'stb_impl', 'torben')
$dllOwnNames = @('api','avio_bridge','cancel_token','contact_sheet','deadline','error','image_analysis','media_session','runtime_info','sha512','video_analysis','win_file')
$compatOwnNames = @('test_image_compat')
$currentBuildRoot = Split-Path (Split-Path ([IO.Path]::GetFullPath($CurrentImageObjectDir)) -Parent) -Parent
$currentDllTarget = Join-Path $currentBuildRoot 'videocore.dir\Release'
$currentCompatTarget = Join-Path $currentBuildRoot 'test_vc_image_compat.dir\Release'

function Write-Tlogs(
    [string]$TargetDir,
    [string[]]$ObjectPaths,
    [string]$BinaryPath,
    [string]$LinkCommand,
    [switch]$AppendIncompleteRecord) {
    $tlogDir = Join-Path $TargetDir 'synthetic.tlog'
    New-Item -ItemType Directory -Force -Path $tlogDir | Out-Null
    $all = @($ObjectPaths)
    $text = '^' + ($all -join '|') + "`n" + $LinkCommand
    if ($AppendIncompleteRecord) {
        $text += "`n^" + $all[0] + "`n" + $LinkCommand
    }
    [IO.File]::WriteAllText((Join-Path $tlogDir 'link.command.1.tlog'), $text, [Text.Encoding]::Unicode)
    [IO.File]::WriteAllText((Join-Path $tlogDir 'link.read.1.tlog'), $text, [Text.Encoding]::Unicode)
    [IO.File]::WriteAllText((Join-Path $tlogDir 'link.write.1.tlog'), [IO.Path]::GetFullPath($BinaryPath), [Text.Encoding]::Unicode)
}

function Get-CurrentLinkCommand([string]$TargetDir) {
    $matches = @(Get-ChildItem -LiteralPath $TargetDir -Recurse -File -Filter 'link.command.1.tlog')
    if ($matches.Count -ne 1) { throw "current target has ambiguous link command TLog: $TargetDir" }
    $lines = @(Get-Content -LiteralPath $matches[0].FullName)
    $lastRoot = -1
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i].StartsWith('^')) { $lastRoot = $i }
    }
    if ($lastRoot -lt 0 -or $lastRoot + 1 -ge $lines.Count) { throw "current target has no link command: $TargetDir" }
    return ($lines[($lastRoot + 1)..($lines.Count - 1)] -join ' ')
}

function Rewrite-ObjectPaths(
    [string]$Command,
    [Collections.IDictionary]$PathMap) {
    $rewritten = $Command
    foreach ($sourcePath in $PathMap.Keys) {
        $sourceFull = [IO.Path]::GetFullPath($sourcePath)
        $destination = [IO.Path]::GetFullPath($PathMap[$sourcePath])
        $relative = [IO.Path]::GetRelativePath([IO.Path]::GetFullPath($BuildRoot), $sourceFull)
        foreach ($form in @($sourceFull, $relative)) {
            $rewritten = [regex]::Replace(
                $rewritten,
                [regex]::Escape($form),
                ('"' + $destination + '"'),
                [Text.RegularExpressions.RegexOptions]::IgnoreCase)
        }
    }
    return $rewritten
}

function Rewrite-LinkSwitch([string]$Command, [string]$Switch, [string]$Value) {
    $pattern = '(?i)(?:^|\s)/' + [regex]::Escape($Switch) + ':(?:"[^"]*"|\S+)'
    $matches = [regex]::Matches($Command, $pattern)
    if ($matches.Count -ne 1) { throw "expected one /${Switch}: link switch, got $($matches.Count)" }
    return [regex]::Replace($Command, $pattern, (' /' + $Switch + ':"' + $Value + '"'), 1)
}

$currentDllLinkCommand = Get-CurrentLinkCommand $currentDllTarget
$currentCompatLinkCommand = Get-CurrentLinkCommand $currentCompatTarget

function Mutate-PeBody([string]$PePath) {
    $bytes = [IO.File]::ReadAllBytes($PePath)
    if ($bytes.Length -lt 1024) { throw "PE is unexpectedly small: $PePath" }
    $offset = [Math]::Floor($bytes.Length * 0.75)
    $bytes[$offset] = $bytes[$offset] -bxor 0x5A
    [IO.File]::WriteAllBytes($PePath, $bytes)
}

function New-Case([string]$Name, [switch]$TextObjects, [switch]$TextPe, [switch]$SubstituteValidObject,
                  [switch]$RenamedPrivateDuplicate, [switch]$TamperPeContent,
                  [switch]$ChangeObjectContent, [switch]$AppendPeOverlay,
                  [switch]$AppendIncompleteLinkRecord, [switch]$ForgeCompilerTlog,
                   [switch]$ResponseFileShadow, [switch]$ShellInjection,
                   [switch]$OmitImageAnalysis,
                  [ValidateSet('', 'ForcedInclude', 'ResponseFile', 'ExtraInclude', 'ExtraInput',
                      'ExtraOutput', 'WrongPathMapLeft')]
                  [string]$CompileMutation = '', [switch]$ToolShadow) {
    $root = Join-Path $caseFull $Name
    $objectDir = Join-Path $root 'objects'
    $dllDir = Join-Path $root 'dll-target'
    $compatDir = Join-Path $root 'compat-target'
    New-Item -ItemType Directory -Force -Path $objectDir,$dllDir,$compatDir | Out-Null

    foreach ($basename in $expectedObjects) {
        $destination = Join-Path $objectDir "$basename.obj"
        if ($TextObjects) {
            [IO.File]::WriteAllText($destination, "not-coff-$basename", [Text.UTF8Encoding]::new($false))
        } else {
            Copy-Item -LiteralPath (Join-Path $CurrentImageObjectDir "$basename.obj") -Destination $destination
        }
    }
    foreach ($basename in $dllOwnNames) {
        $destination = Join-Path $dllDir "$basename.obj"
        if ($TextObjects) { [IO.File]::WriteAllText($destination, "not-coff-own-$basename") }
        else { Copy-Item -LiteralPath (Join-Path $currentDllTarget "$basename.obj") -Destination $destination }
    }
    foreach ($basename in $compatOwnNames) {
        $destination = Join-Path $compatDir "$basename.obj"
        if ($TextObjects) { [IO.File]::WriteAllText($destination, "not-coff-own-$basename") }
        else { Copy-Item -LiteralPath (Join-Path $currentCompatTarget "$basename.obj") -Destination $destination }
    }
    if ($SubstituteValidObject) {
        Copy-Item -LiteralPath (Join-Path $objectDir 'downscaling.obj') -Destination (Join-Path $objectDir 'torben.obj') -Force
    }
    if ($ChangeObjectContent) {
        # Preserve the old source marker/anchor and COFF header while changing
        # the actual object bytes.  A source-to-object proof must reject this
        # even when the final PE and text TLogs are copied unchanged.
        $changed = Join-Path $objectDir 'pdq.obj'
        $stream = [IO.File]::Open($changed, [IO.FileMode]::Append, [IO.FileAccess]::Write, [IO.FileShare]::None)
        try { $stream.WriteByte(0xA5) } finally { $stream.Dispose() }
    }

    $dllBinary = Join-Path $root 'videocore.dll'
    $compatBinary = Join-Path $root 'test_vc_image_compat.exe'
    if ($TextPe) {
        [IO.File]::WriteAllText($dllBinary, 'not a PE DLL', [Text.UTF8Encoding]::new($false))
        [IO.File]::WriteAllText($compatBinary, 'not a PE EXE', [Text.UTF8Encoding]::new($false))
    } else {
        Copy-Item -LiteralPath $CurrentDllBinary -Destination $dllBinary
        Copy-Item -LiteralPath $CurrentCompatBinary -Destination $compatBinary
    }
    if ($TamperPeContent) {
        Mutate-PeBody $dllBinary
        Mutate-PeBody $compatBinary
    }
    if ($AppendPeOverlay) {
        foreach ($binary in @($dllBinary, $compatBinary)) {
            $stream = [IO.File]::Open($binary, [IO.FileMode]::Append, [IO.FileAccess]::Write, [IO.FileShare]::None)
            try { $stream.Write([Text.Encoding]::ASCII.GetBytes('FORGED-PE-OVERLAY')) } finally { $stream.Dispose() }
        }
    }

    $objectPaths = @(Get-ChildItem -LiteralPath $objectDir -File -Filter '*.obj' |
        Sort-Object Name | ForEach-Object { [IO.Path]::GetFullPath($_.FullName) })
    if ($RenamedPrivateDuplicate) {
        $shadow = Join-Path $dllDir 'pdq_shadow.obj'
        if ($TextObjects) {
            [IO.File]::WriteAllText($shadow, 'private shadow', [Text.UTF8Encoding]::new($false))
        } else {
            Copy-Item -LiteralPath (Join-Path $objectDir 'pdq.obj') -Destination $shadow
        }
    }
    $dllOwnPaths = @($dllOwnNames | ForEach-Object { [IO.Path]::GetFullPath((Join-Path $dllDir "$_.obj")) })
    $compatOwnPaths = @($compatOwnNames | ForEach-Object { [IO.Path]::GetFullPath((Join-Path $compatDir "$_.obj")) })
    $dllMap = [ordered]@{}
    $compatMap = [ordered]@{}
    foreach ($basename in $expectedObjects) {
        $current = Join-Path $CurrentImageObjectDir "$basename.obj"
        $replacement = Join-Path $objectDir "$basename.obj"
        $dllMap[$current] = $replacement
        $compatMap[$current] = $replacement
    }
    foreach ($basename in $dllOwnNames) {
        $dllMap[(Join-Path $currentDllTarget "$basename.obj")] = (Join-Path $dllDir "$basename.obj")
    }
    foreach ($basename in $compatOwnNames) {
        $compatMap[(Join-Path $currentCompatTarget "$basename.obj")] = (Join-Path $compatDir "$basename.obj")
    }
    $dllCommand = Rewrite-ObjectPaths $currentDllLinkCommand $dllMap
    $compatCommand = Rewrite-ObjectPaths $currentCompatLinkCommand $compatMap
    $dllCommand = Rewrite-LinkSwitch $dllCommand 'OUT' $dllBinary
    $dllCommand = Rewrite-LinkSwitch $dllCommand 'PDB' ([IO.Path]::ChangeExtension($dllBinary, '.pdb'))
    $dllCommand = Rewrite-LinkSwitch $dllCommand 'IMPLIB' ([IO.Path]::ChangeExtension($dllBinary, '.lib'))
    $compatCommand = Rewrite-LinkSwitch $compatCommand 'OUT' $compatBinary
    $compatCommand = Rewrite-LinkSwitch $compatCommand 'PDB' ([IO.Path]::ChangeExtension($compatBinary, '.pdb'))
    $compatCommand = Rewrite-LinkSwitch $compatCommand 'IMPLIB' ([IO.Path]::ChangeExtension($compatBinary, '.lib'))
    if ($OmitImageAnalysis) {
        $imageAnalysis = [IO.Path]::GetFullPath(
            (Join-Path $dllDir 'image_analysis.obj'))
        $matches = [regex]::Matches(
            $dllCommand, '(?i)"?' + [regex]::Escape($imageAnalysis) + '"?')
        if ($matches.Count -ne 1) {
            throw "$Name expected one DLL image_analysis body input, got $($matches.Count)"
        }
        $dllCommand = [regex]::Replace(
            $dllCommand,
            '(?i)"?' + [regex]::Escape($imageAnalysis) + '"?',
            '',
            1)
    }
    if ($RenamedPrivateDuplicate -or $ResponseFileShadow) {
        $canonicalPdq = [IO.Path]::GetFullPath((Join-Path $objectDir 'pdq.obj'))
        $shadow = Join-Path $dllDir 'pdq_shadow.obj'
        if (-not (Test-Path -LiteralPath $shadow)) {
            Copy-Item -LiteralPath $canonicalPdq -Destination $shadow
        }
        $shadowFull = [IO.Path]::GetFullPath($shadow)
        if ($ResponseFileShadow) {
            $rsp = Join-Path $root 'shadow-inputs.rsp'
            [IO.File]::WriteAllText($rsp, ('"' + $shadowFull + '"'), [Text.UTF8Encoding]::new($false))
            $replacement = '@"' + [IO.Path]::GetFullPath($rsp) + '"'
        } else {
            $replacement = '"' + $shadowFull + '"'
        }
        $matches = [regex]::Matches($dllCommand, '(?i)"?' + [regex]::Escape($canonicalPdq) + '"?')
        if ($matches.Count -ne 1) { throw "$Name expected one DLL pdq body input, got $($matches.Count)" }
        $dllCommand = [regex]::Replace(
            $dllCommand, '(?i)"?' + [regex]::Escape($canonicalPdq) + '"?', $replacement, 1)
    }
    $shellSentinel = $null
    if ($ShellInjection) {
        $shellSentinel = Join-Path $root 'shell-injection-sentinel.txt'
        $dllCommand += ' & echo injected>"' + $shellSentinel + '"'
    }
    if ($ToolShadow) {
        # The old gate asks cmd.exe to resolve cl.exe/link.exe in BuildRoot.
        # A forwarding executable keeps the replay coherent while recording
        # whether the writable working directory shadow was executed.
        $shadowSentinel = Join-Path $root 'tool-shadow-sentinel.txt'
        $shadowSource = @'
#include <windows.h>
#include <cwchar>
#include <string>

static std::wstring env(const wchar_t* name) {
    wchar_t value[32768];
    DWORD length = GetEnvironmentVariableW(name, value, 32768);
    return length == 0 || length >= 32768 ? std::wstring() : std::wstring(value, length);
}

static std::wstring switch_value(int argc, wchar_t** argv, const wchar_t* prefix) {
    size_t length = std::wcslen(prefix);
    for (int i = 1; i < argc; ++i) {
        if (_wcsnicmp(argv[i], prefix, length) == 0) return std::wstring(argv[i] + length);
    }
    return std::wstring();
}

int wmain(int argc, wchar_t** argv) {
    std::wstring sentinel = env(L"VC_R5_SHADOW_SENTINEL");
    HANDLE log = CreateFileW(sentinel.c_str(), FILE_APPEND_DATA, FILE_SHARE_READ, nullptr,
        OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, nullptr);
    if (log != INVALID_HANDLE_VALUE) {
        const char marker[] = "executed\n";
        DWORD written = 0;
        WriteFile(log, marker, sizeof(marker) - 1, &written, nullptr);
        CloseHandle(log);
    }
    std::wstring self = argv[0];
    bool compile = self.size() >= 6 && _wcsicmp(self.c_str() + self.size() - 6, L"cl.exe") == 0;
    std::wstring destination = switch_value(argc, argv, compile ? L"/Fo" : L"/OUT:");
    std::wstring source;
    if (compile) {
        for (int i = 1; i < argc; ++i) {
            std::wstring arg = argv[i];
            if (arg.size() > 4 && _wcsicmp(arg.c_str() + arg.size() - 4, L".cpp") == 0) {
                size_t slash = arg.find_last_of(L"\\/");
                size_t dot = arg.find_last_of(L'.');
                source = env(L"VC_R5_SHADOW_OBJECT_DIR") + L"\\" +
                    arg.substr(slash + 1, dot - slash - 1) + L".obj";
                break;
            }
        }
    } else {
        source = destination.size() >= 4 && _wcsicmp(destination.c_str() + destination.size() - 4, L".dll") == 0
            ? env(L"VC_R5_SHADOW_DLL") : env(L"VC_R5_SHADOW_EXE");
    }
    if (source.empty() || destination.empty()) return 91;
    return CopyFileW(source.c_str(), destination.c_str(), FALSE) ? 0 : 92;
}
'@
        $shadowSourcePath = Join-Path $root 'tool-shadow.cpp'
        $shadowExe = Join-Path $root 'tool-shadow.exe'
        [IO.File]::WriteAllText($shadowSourcePath, $shadowSource, [Text.UTF8Encoding]::new($false))
        $compileRaw = 'call "' + [IO.Path]::GetFullPath($VsDevCmd) +
            '" -arch=x64 -host_arch=x64 >nul && "' + [IO.Path]::GetFullPath($Compiler) +
            '" /nologo /EHsc /utf-8 /Fe:"' + $shadowExe + '" "' + $shadowSourcePath + '"'
        $compileOutput = (& $env:ComSpec /d /s /c $compileRaw 2>&1 | Out-String)
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $shadowExe -PathType Leaf)) {
            throw "failed to compile coherent tool-shadow fixture:`n$compileOutput"
        }
    }
    Write-Tlogs $dllDir (@($objectPaths) + $dllOwnPaths) $dllBinary $dllCommand `
        -AppendIncompleteRecord:$AppendIncompleteLinkRecord
    Write-Tlogs $compatDir (@($objectPaths) + $compatOwnPaths) $compatBinary $compatCommand `
        -AppendIncompleteRecord:$AppendIncompleteLinkRecord

    $caseCompilerTlog = $CompilerCommandTlog
    if ($ForgeCompilerTlog -or $CompileMutation) {
        $sourceTlogs = if (Test-Path -LiteralPath $CompilerCommandTlog -PathType Container) {
            @(Get-ChildItem -LiteralPath $CompilerCommandTlog -Recurse -File -Filter 'CL.command.1.tlog')
        } else { @(Get-Item -LiteralPath $CompilerCommandTlog) }
        if ($sourceTlogs.Count -ne 1) { throw "expected one source compiler TLog" }
        $compilerDir = Join-Path $root 'compiler-tlog'
        New-Item -ItemType Directory -Path $compilerDir | Out-Null
        $compilerText = [IO.File]::ReadAllText($sourceTlogs[0].FullName, [Text.Encoding]::Unicode)
        if ($ForgeCompilerTlog) {
            $compilerText = [regex]::Replace(
                $compilerText, '(?i)(?:^|\s)/Brepro(?=\s|$)', ' /BreproFORGED', 1)
        } else {
            $emptyDir = Join-Path $root 'empty-include'
            New-Item -ItemType Directory -Path $emptyDir | Out-Null
            $emptyHeader = Join-Path $root 'empty-forced-include.h'
            [IO.File]::WriteAllText($emptyHeader, '', [Text.UTF8Encoding]::new($false))
            $emptyRsp = Join-Path $root 'empty-compile.rsp'
            [IO.File]::WriteAllText($emptyRsp, '', [Text.UTF8Encoding]::new($false))
            $extraAsm = Join-Path $root 'extra-output.asm'
            $extraInput = [IO.Path]::GetFullPath((Join-Path $CurrentImageObjectDir 'downscaling.obj'))
            switch ($CompileMutation) {
                'ForcedInclude' {
                    $insertion = ' /FI"' + $emptyHeader + '"'
                    $compilerText = [regex]::Replace($compilerText, '(?im)^/c(?=\s)', ('/c' + $insertion), 1)
                }
                'ResponseFile' {
                    $insertion = ' @"' + $emptyRsp + '"'
                    $compilerText = [regex]::Replace($compilerText, '(?im)^/c(?=\s)', ('/c' + $insertion), 1)
                }
                'ExtraInclude' {
                    $insertion = ' /I"' + $emptyDir + '"'
                    $compilerText = [regex]::Replace($compilerText, '(?im)^/c(?=\s)', ('/c' + $insertion), 1)
                }
                'ExtraInput' {
                    $insertion = ' "' + $extraInput + '"'
                    $compilerText = [regex]::Replace($compilerText, '(?im)^/c(?=\s)', ('/c' + $insertion), 1)
                }
                'ExtraOutput' {
                    $insertion = ' /FA /Fa"' + $extraAsm + '"'
                    $compilerText = [regex]::Replace($compilerText, '(?im)^/c(?=\s)', ('/c' + $insertion), 1)
                }
                'WrongPathMapLeft' {
                    $wrong = Join-Path $root 'irrelevant-object-path'
                    $compilerText = [regex]::Replace(
                        $compilerText,
                        '(?i)/pathmap:(?:"[^"]*"|[^\s=]+)=VC_IMAGE_OBJECTS',
                        ('/pathmap:"' + $wrong + '"=VC_IMAGE_OBJECTS'), 1)
                }
            }
            if ($CompileMutation -eq 'ExtraInput' -and
                $compilerText.IndexOf($extraInput, [StringComparison]::OrdinalIgnoreCase) -lt 0) {
                throw 'compile-extra-input mutation fixture did not inject its object argument'
            }
        }
        [IO.File]::WriteAllText(
            (Join-Path $compilerDir 'CL.command.1.tlog'), $compilerText, [Text.Encoding]::Unicode)
        $caseCompilerTlog = $compilerDir
    }

    $old = [DateTime]::UtcNow.AddMinutes(-10)
    $new = [DateTime]::UtcNow.AddMinutes(-1)
    Get-ChildItem -LiteralPath $objectDir -File | ForEach-Object { $_.LastWriteTimeUtc = $old }
    (Get-Item -LiteralPath $dllBinary).LastWriteTimeUtc = $new
    (Get-Item -LiteralPath $compatBinary).LastWriteTimeUtc = $new
    Get-ChildItem -LiteralPath $dllDir,$compatDir -Recurse -File -Filter '*.tlog' |
        ForEach-Object { $_.LastWriteTimeUtc = $new }

    return [ordered]@{ ObjectDir=$objectDir; DllDir=$dllDir; CompatDir=$compatDir;
        DllBinary=$dllBinary; CompatBinary=$compatBinary; CompilerTlog=$caseCompilerTlog;
        BuildRoot=$BuildRoot; ShellSentinel=$shellSentinel; ShadowSentinel=$shadowSentinel;
        ShadowExecutable=$shadowExe }
}

function Invoke-Gate([Collections.IDictionary]$Case) {
    $output = (& 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -File $GateScript `
        -ImageObjectDir $Case.ObjectDir -DllTargetDir $Case.DllDir `
        -CompatTargetDir $Case.CompatDir -DllBinary $Case.DllBinary `
        -CompatBinary $Case.CompatBinary -Dumpbin $Dumpbin `
        -SourceRoot $SourceRoot -LegacySourceRoot (Join-Path (Split-Path $SourceRoot -Parent) 'mediacore') `
        -Compiler $Compiler -VsDevCmd $VsDevCmd `
        -CompilerCommandTlog $Case.CompilerTlog `
        -CanonicalImageObjectDir $CurrentImageObjectDir `
        -BuildRoot $Case.BuildRoot 2>&1 | Out-String)
    return [ordered]@{ ExitCode=$LASTEXITCODE; Output=$output }
}

try {
    $baseline = New-Case 'clean-copied-baseline'
    $baselineResult = Invoke-Gate $baseline
    if ($baselineResult.ExitCode -ne 0) {
        throw "clean copied provenance baseline was rejected:`n$($baselineResult.Output)"
    }
    $cases = [ordered]@{
        'non-coff' = @{ Case=New-Case 'non-coff' -TextObjects; Diagnostic='not an x64 COFF object' }
        'non-pe' = @{ Case=New-Case 'non-pe' -TextPe; Diagnostic='not a PE image' }
        'valid-object-substitution' = @{ Case=New-Case 'valid-object-substitution' -SubstituteValidObject; Diagnostic='deterministic compile replay differs' }
        'renamed-private-duplicate' = @{ Case=New-Case 'renamed-private-duplicate' -RenamedPrivateDuplicate; Diagnostic='actual object input set mismatch' }
        'missing-image-analysis-private-tu' = @{ Case=New-Case 'missing-image-analysis-private-tu' -OmitImageAnalysis; Diagnostic='actual object input set mismatch' }
        'link-response-shadow' = @{ Case=New-Case 'link-response-shadow' -ResponseFileShadow; Diagnostic='link response file escapes BuildRoot' }
        'link-shell-injection' = @{ Case=New-Case 'link-shell-injection' -ShellInjection; Diagnostic='unsafe shell control' }
        'compile-forced-include' = @{ Case=New-Case 'compile-forced-include' -CompileMutation ForcedInclude; Diagnostic='forced include is forbidden' }
        'compile-response-file' = @{ Case=New-Case 'compile-response-file' -CompileMutation ResponseFile; Diagnostic='compiler response files are forbidden' }
        'compile-extra-include' = @{ Case=New-Case 'compile-extra-include' -CompileMutation ExtraInclude; Diagnostic='compile include set mismatch' }
        'compile-extra-input' = @{ Case=New-Case 'compile-extra-input' -CompileMutation ExtraInput; Diagnostic='compile positional input mismatch' }
        'compile-extra-output' = @{ Case=New-Case 'compile-extra-output' -CompileMutation ExtraOutput; Diagnostic='compile output switch set mismatch' }
        'compile-pathmap-left' = @{ Case=New-Case 'compile-pathmap-left' -CompileMutation WrongPathMapLeft; Diagnostic='compiler pathmap left side mismatch' }
        'stale-pe-forged-complete-tlog' = @{ Case=New-Case 'stale-pe-forged-complete-tlog' -TamperPeContent; Diagnostic='deterministic link replay differs' }
        'object-content-change' = @{ Case=New-Case 'object-content-change' -ChangeObjectContent; Diagnostic='deterministic compile replay differs' }
        'pe-overlay' = @{ Case=New-Case 'pe-overlay' -AppendPeOverlay; Diagnostic='deterministic link replay differs' }
        'stale-complete-link-record' = @{ Case=New-Case 'stale-complete-link-record' -AppendIncompleteLinkRecord; Diagnostic='exact link record is stale' }
        'forged-compiler-command-tlog' = @{ Case=New-Case 'forged-compiler-command-tlog' -ForgeCompilerTlog; Diagnostic='missing source or deterministic Task 6 flags' }
    }
    $accepted = [Collections.Generic.List[string]]::new()
    $wrongDiagnostics = [Collections.Generic.List[string]]::new()
    foreach ($name in $cases.Keys) {
        $spec = $cases[$name]
        $result = Invoke-Gate $spec.Case
        if ($result.ExitCode -eq 0) {
            $accepted.Add($name)
        } elseif ($result.Output.IndexOf($spec.Diagnostic, [StringComparison]::OrdinalIgnoreCase) -lt 0) {
            $wrongDiagnostics.Add("$name(expected=$($spec.Diagnostic))")
        }
        if ($name -eq 'link-shell-injection' -and
            (Test-Path -LiteralPath $spec.Case.ShellSentinel)) {
            $accepted.Add('link-shell-injection-executed')
        }
    }
    $toolShadow = New-Case 'tool-shadow' -ToolShadow
    $shadowCl = Join-Path ([IO.Path]::GetFullPath($BuildRoot)) 'cl.exe'
    $shadowLink = Join-Path ([IO.Path]::GetFullPath($BuildRoot)) 'link.exe'
    if ((Test-Path -LiteralPath $shadowCl) -or (Test-Path -LiteralPath $shadowLink)) {
        throw 'tool-shadow fixture refuses to overwrite existing BuildRoot tools'
    }
    $oldShadowObjectDir = $env:VC_R5_SHADOW_OBJECT_DIR
    $oldShadowDll = $env:VC_R5_SHADOW_DLL
    $oldShadowExe = $env:VC_R5_SHADOW_EXE
    $oldShadowSentinel = $env:VC_R5_SHADOW_SENTINEL
    try {
        Copy-Item -LiteralPath $toolShadow.ShadowExecutable -Destination $shadowCl
        Copy-Item -LiteralPath $toolShadow.ShadowExecutable -Destination $shadowLink
        $env:VC_R5_SHADOW_OBJECT_DIR = [IO.Path]::GetFullPath($CurrentImageObjectDir)
        $env:VC_R5_SHADOW_DLL = [IO.Path]::GetFullPath($CurrentDllBinary)
        $env:VC_R5_SHADOW_EXE = [IO.Path]::GetFullPath($CurrentCompatBinary)
        $env:VC_R5_SHADOW_SENTINEL = $toolShadow.ShadowSentinel
        $toolResult = Invoke-Gate $toolShadow
    } finally {
        Remove-Item -LiteralPath $shadowCl,$shadowLink -Force -ErrorAction SilentlyContinue
        $env:VC_R5_SHADOW_OBJECT_DIR = $oldShadowObjectDir
        $env:VC_R5_SHADOW_DLL = $oldShadowDll
        $env:VC_R5_SHADOW_EXE = $oldShadowExe
        $env:VC_R5_SHADOW_SENTINEL = $oldShadowSentinel
    }
    if ($toolResult.ExitCode -ne 0) {
        $wrongDiagnostics.Add('tool-shadow(clean direct replay rejected)')
    }
    if (Test-Path -LiteralPath $toolShadow.ShadowSentinel) {
        $accepted.Add('tool-shadow-executed')
    }
    if ($accepted.Count -ne 0 -or $wrongDiagnostics.Count -ne 0) {
        throw "object provenance mutation failures: accepted=$($accepted -join ',') wrong_diagnostic=$($wrongDiagnostics -join ',')"
    }
    Write-Output 'IMAGE_OBJECT_PROVENANCE_MUTATION PASS clean_copied_baseline=GREEN non_coff=RED non_pe=RED valid_object_substitution=RED renamed_private_duplicate=RED missing_image_analysis_private_tu=RED link_response_shadow=RED link_shell_injection=RED compile_forced_include=RED compile_response_file=RED compile_extra_include=RED compile_extra_input=RED compile_extra_output=RED compile_pathmap_left=RED stale_pe_forged_complete_tlog=RED object_content_change=RED pe_overlay=RED stale_complete_link_record=RED forged_compiler_command_tlog=RED tool_shadow=IGNORED temp_only=true dedicated_diagnostics=true'
} finally {
    if ((Test-Path -LiteralPath $caseFull) -and
        $caseFull.StartsWith($tempBase, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $caseFull -Recurse -Force
    }
}
