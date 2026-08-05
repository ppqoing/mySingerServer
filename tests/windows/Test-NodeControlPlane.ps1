[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$AgentConfig,
    [string]$HelperConfig,
    [string]$AgentExe,
    [string]$WorkerExe,
    [string]$HelperExe,
    [string]$VideoCoreDll,
    [string]$TestRoot
)

$ErrorActionPreference = "Stop"
$script:AgentControlPipe = 'mysingerserver-agent-control-v1'
$script:HelperControlPipe = 'mysingerserver-helper-control-v1'
$script:MaximumControlFrame = 1MB
$script:StartedProcesses = [System.Collections.Generic.List[System.Diagnostics.Process]]::new()

function Assert-NodeControlCondition {
    param([bool]$Condition, [string]$Code)
    if (-not $Condition) {
        throw $Code
    }
}

function Invoke-StaticNodeControlChecks {
    $repository = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
    $required = @(
        'internal\nodectl\message.go',
        'internal\nodectl\pipe_windows.go',
        'internal\agentcontrol\service.go',
        'internal\helpercontrol\service.go',
        'cmd\agent\main.go',
        'cmd\helper\main.go',
        'scripts\build.ps1'
    )
    foreach ($relative in $required) {
        $path = Join-Path $repository $relative
        Assert-NodeControlCondition `
            (Test-Path -LiteralPath $path -PathType Leaf) `
            "STATIC_REQUIRED_FILE_MISSING"
    }

    $pipeSource = Get-Content -Raw -LiteralPath `
        (Join-Path $repository 'internal\nodectl\pipe_windows.go')
    Assert-NodeControlCondition `
        $pipeSource.Contains('mysingerserver-agent-control-v1') `
        'STATIC_AGENT_CONTROL_PIPE_MISSING'
    Assert-NodeControlCondition `
        $pipeSource.Contains('mysingerserver-helper-control-v1') `
        'STATIC_HELPER_CONTROL_PIPE_MISSING'
    Assert-NodeControlCondition `
        ($script:AgentControlPipe -ne $script:HelperControlPipe) `
        'STATIC_CONTROL_PIPES_COLLIDE'

    $buildSource = Get-Content -Raw -LiteralPath `
        (Join-Path $repository 'scripts\build.ps1')
    foreach ($package in @(
            './internal/nodectl',
            './internal/agentcontrol',
            './internal/helpercontrol'
        )) {
        Assert-NodeControlCondition `
            $buildSource.Contains($package) `
            'STATIC_BUILD_CONTROL_PACKAGE_GATE_MISSING'
    }

    [ordered]@{
        schema_version = 1
        mode = 'static'
        status = 'PASS'
        dynamic_acceptance = 'BLOCKED_NOT_RUN_DYNAMIC'
        process_actions = 0
        temporary_roots_created = 0
    } | ConvertTo-Json -Depth 4
}

function Resolve-TestRootCandidate {
    param([string]$Candidate)
    if ([string]::IsNullOrWhiteSpace($Candidate)) {
        $Candidate = 'C:\tmp\mysingerserver-node-control-{0}' -f `
            [Guid]::NewGuid().ToString('N')
    }
    $full = [IO.Path]::GetFullPath($Candidate).TrimEnd('\')
    $prefix = [IO.Path]::GetFullPath('C:\tmp').TrimEnd('\') + '\'
    $leaf = Split-Path -Leaf $full
    Assert-NodeControlCondition `
        ($full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase) -and
            $leaf.StartsWith(
                'mysingerserver-node-control-',
                [StringComparison]::OrdinalIgnoreCase
            )) `
        'UNSAFE_TEST_ROOT'
    return $full
}

function Test-PathInsideRoot {
    param([string]$Path, [string]$Root)
    $fullPath = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $fullRoot = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    return $fullPath.Equals($fullRoot, [StringComparison]::OrdinalIgnoreCase) -or
        $fullPath.StartsWith(
            $fullRoot + '\',
            [StringComparison]::OrdinalIgnoreCase
        )
}

function Resolve-HelperLogDirectoryForTest {
    param([string]$Value, [string]$Root)
    Assert-NodeControlCondition `
        (-not [string]::IsNullOrWhiteSpace($Value)) `
        'HELPER_LOG_DIR_REQUIRED'
    Assert-NodeControlCondition ([IO.Path]::IsPathRooted($Value)) `
        'HELPER_LOG_DIR_MUST_BE_ABSOLUTE'
    $resolved = [IO.Path]::GetFullPath($Value).TrimEnd('\')
    Assert-NodeControlCondition (Test-PathInsideRoot $resolved $Root) `
        'HELPER_LOG_DIR_OUTSIDE_TEST_ROOT'
    return $resolved
}

function Test-CleanupRootIdentity {
    param(
        [string]$ExpectedPath,
        [string]$CurrentPath,
        [string]$ExpectedIdentity,
        [string]$CurrentIdentity,
        [bool]$CurrentIsReparse
    )
    if ($CurrentIsReparse -or
        [string]::IsNullOrWhiteSpace($ExpectedIdentity) -or
        [string]::IsNullOrWhiteSpace($CurrentIdentity)) {
        return $false
    }
    return $ExpectedIdentity.Equals(
            $CurrentIdentity,
            [StringComparison]::Ordinal
        ) -and
        ([IO.Path]::GetFullPath($ExpectedPath).TrimEnd('\')).Equals(
            [IO.Path]::GetFullPath($CurrentPath).TrimEnd('\'),
            [StringComparison]::OrdinalIgnoreCase
        )
}

function Initialize-DirectoryIdentityInterop {
    if ('NodeControlDirectoryIdentity' -as [type]) { return }
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;
using System.Text;
using Microsoft.Win32.SafeHandles;

public sealed class NodeControlDirectoryIdentityInfo
{
    public string Identity { get; set; }
    public string FinalPath { get; set; }
    public bool IsReparsePoint { get; set; }
}

public static class NodeControlDirectoryIdentity
{
    private const uint FILE_READ_ATTRIBUTES = 0x80;
    private const uint FILE_SHARE_READ = 0x1;
    private const uint FILE_SHARE_WRITE = 0x2;
    private const uint FILE_SHARE_DELETE = 0x4;
    private const uint OPEN_EXISTING = 3;
    private const uint FILE_FLAG_BACKUP_SEMANTICS = 0x02000000;
    private const uint FILE_FLAG_OPEN_REPARSE_POINT = 0x00200000;
    private const uint FILE_ATTRIBUTE_REPARSE_POINT = 0x400;

    [StructLayout(LayoutKind.Sequential)]
    private struct BY_HANDLE_FILE_INFORMATION
    {
        public uint FileAttributes;
        public FILETIME CreationTime;
        public FILETIME LastAccessTime;
        public FILETIME LastWriteTime;
        public uint VolumeSerialNumber;
        public uint FileSizeHigh;
        public uint FileSizeLow;
        public uint NumberOfLinks;
        public uint FileIndexHigh;
        public uint FileIndexLow;
    }

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern SafeFileHandle CreateFile(
        string name,
        uint desiredAccess,
        uint shareMode,
        IntPtr securityAttributes,
        uint creationDisposition,
        uint flagsAndAttributes,
        IntPtr templateFile);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetFileInformationByHandle(
        SafeFileHandle handle,
        out BY_HANDLE_FILE_INFORMATION information);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern uint GetFinalPathNameByHandle(
        SafeFileHandle handle,
        StringBuilder path,
        uint pathLength,
        uint flags);

    public static NodeControlDirectoryIdentityInfo Get(string path)
    {
        using (SafeFileHandle handle = CreateFile(
            path,
            FILE_READ_ATTRIBUTES,
            FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
            IntPtr.Zero,
            OPEN_EXISTING,
            FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OPEN_REPARSE_POINT,
            IntPtr.Zero))
        {
            if (handle.IsInvalid)
                throw new Win32Exception(Marshal.GetLastWin32Error());
            BY_HANDLE_FILE_INFORMATION information;
            if (!GetFileInformationByHandle(handle, out information))
                throw new Win32Exception(Marshal.GetLastWin32Error());
            StringBuilder finalPath = new StringBuilder(32768);
            uint length = GetFinalPathNameByHandle(
                handle, finalPath, (uint)finalPath.Capacity, 0);
            if (length == 0 || length >= finalPath.Capacity)
                throw new Win32Exception(Marshal.GetLastWin32Error());
            return new NodeControlDirectoryIdentityInfo {
                Identity = String.Format(
                    "{0:x8}:{1:x8}:{2:x8}",
                    information.VolumeSerialNumber,
                    information.FileIndexHigh,
                    information.FileIndexLow),
                FinalPath = finalPath.ToString(),
                IsReparsePoint =
                    (information.FileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0
            };
        }
    }
}
'@
}

function Get-DirectoryIdentity {
    param([string]$Path)
    Initialize-DirectoryIdentityInterop
    return [NodeControlDirectoryIdentity]::Get($Path)
}

function Resolve-RequiredLeaf {
    param([string]$Path, [string]$Code)
    Assert-NodeControlCondition `
        (-not [string]::IsNullOrWhiteSpace($Path)) `
        $Code
    Assert-NodeControlCondition `
        (Test-Path -LiteralPath $Path -PathType Leaf) `
        $Code
    return [string](Resolve-Path -LiteralPath $Path).Path
}

function Add-ByteSequence {
    param(
        [System.IO.MemoryStream]$Stream,
        [byte[]]$Bytes
    )
    $Stream.Write($Bytes, 0, $Bytes.Length)
}

function ConvertTo-BigEndianBytes {
    param([uint64]$Value, [int]$Count)
    $bytes = [byte[]]::new($Count)
    for ($index = $Count - 1; $index -ge 0; $index--) {
        $bytes[$index] = [byte]($Value -band 0xff)
        $Value = $Value -shr 8
    }
    return $bytes
}

function ConvertTo-MessagePackInteger {
    param([long]$Value)
    if ($Value -ge 0 -and $Value -le 127) {
        return [byte[]]@([byte]$Value)
    }
    if ($Value -ge 0 -and $Value -le 255) {
        return [byte[]]@([byte]0xcc, [byte]$Value)
    }
    if ($Value -ge 0 -and $Value -le 65535) {
        return [byte[]](@([byte]0xcd) + `
            (ConvertTo-BigEndianBytes -Value ([uint64]$Value) -Count 2))
    }
    if ($Value -ge 0 -and $Value -le [uint32]::MaxValue) {
        return [byte[]](@([byte]0xce) + `
            (ConvertTo-BigEndianBytes -Value ([uint64]$Value) -Count 4))
    }
    if ($Value -ge 0) {
        return [byte[]](@([byte]0xcf) + `
            (ConvertTo-BigEndianBytes -Value ([uint64]$Value) -Count 8))
    }
    if ($Value -ge -32) {
        return [byte[]]@([byte](256 + $Value))
    }
    if ($Value -ge [sbyte]::MinValue) {
        return [byte[]]@([byte]0xd0, [byte]([sbyte]$Value))
    }
    if ($Value -ge [int16]::MinValue) {
        return [byte[]](@([byte]0xd1) + `
            (ConvertTo-BigEndianBytes -Value ([uint16][int16]$Value) -Count 2))
    }
    if ($Value -ge [int32]::MinValue) {
        return [byte[]](@([byte]0xd2) + `
            (ConvertTo-BigEndianBytes -Value ([uint32][int32]$Value) -Count 4))
    }
    return [byte[]](@([byte]0xd3) + `
        (ConvertTo-BigEndianBytes -Value ([uint64]$Value) -Count 8))
}

function ConvertTo-MessagePackString {
    param([string]$Value)
    $body = [Text.Encoding]::UTF8.GetBytes($Value)
    if ($body.Length -le 31) {
        return [byte[]](@([byte](0xa0 + $body.Length)) + $body)
    }
    if ($body.Length -le 255) {
        return [byte[]](@([byte]0xd9, [byte]$body.Length) + $body)
    }
    if ($body.Length -le 65535) {
        return [byte[]](@([byte]0xda) + `
            (ConvertTo-BigEndianBytes -Value $body.Length -Count 2) + $body)
    }
    return [byte[]](@([byte]0xdb) + `
        (ConvertTo-BigEndianBytes -Value $body.Length -Count 4) + $body)
}

function ConvertTo-MessagePackValue {
    param($Value)
    if ($null -eq $Value) {
        return [byte[]]@([byte]0xc0)
    }
    if ($Value -is [bool]) {
        return [byte[]]@([byte]($(if ($Value) { 0xc3 } else { 0xc2 })))
    }
    if ($Value -is [string]) {
        return ConvertTo-MessagePackString -Value $Value
    }
    if ($Value -is [System.Collections.IDictionary]) {
        $stream = [IO.MemoryStream]::new()
        try {
            $count = $Value.Count
            if ($count -le 15) {
                $stream.WriteByte([byte](0x80 + $count))
            } else {
                $stream.WriteByte(0xde)
                Add-ByteSequence $stream `
                    (ConvertTo-BigEndianBytes -Value $count -Count 2)
            }
            foreach ($key in $Value.Keys) {
                Add-ByteSequence $stream `
                    (ConvertTo-MessagePackString -Value ([string]$key))
                Add-ByteSequence $stream `
                    (ConvertTo-MessagePackValue -Value $Value[$key])
            }
            return $stream.ToArray()
        } finally {
            $stream.Dispose()
        }
    }
    if ($Value -is [System.Collections.IEnumerable]) {
        $items = @($Value)
        $stream = [IO.MemoryStream]::new()
        try {
            if ($items.Count -le 15) {
                $stream.WriteByte([byte](0x90 + $items.Count))
            } else {
                $stream.WriteByte(0xdc)
                Add-ByteSequence $stream `
                    (ConvertTo-BigEndianBytes -Value $items.Count -Count 2)
            }
            foreach ($item in $items) {
                Add-ByteSequence $stream `
                    (ConvertTo-MessagePackValue -Value $item)
            }
            return $stream.ToArray()
        } finally {
            $stream.Dispose()
        }
    }
    return ConvertTo-MessagePackInteger -Value ([long]$Value)
}

function Read-MessagePackUnsigned {
    param([byte[]]$Bytes, [ref]$Offset, [int]$Count)
    [uint64]$value = 0
    for ($index = 0; $index -lt $Count; $index++) {
        Assert-NodeControlCondition ($Offset.Value -lt $Bytes.Length) `
            'MSGPACK_TRUNCATED'
        $value = ($value -shl 8) -bor $Bytes[$Offset.Value]
        $Offset.Value++
    }
    return $value
}

function Read-MessagePackStringBody {
    param([byte[]]$Bytes, [ref]$Offset, [int]$Length)
    Assert-NodeControlCondition `
        (($Length -ge 0) -and ($Offset.Value + $Length -le $Bytes.Length)) `
        'MSGPACK_TRUNCATED_STRING'
    $value = [Text.Encoding]::UTF8.GetString($Bytes, $Offset.Value, $Length)
    $Offset.Value += $Length
    return $value
}

function Read-MessagePackCollection {
    param(
        [byte[]]$Bytes,
        [ref]$Offset,
        [int]$Count,
        [bool]$Map
    )
    if ($Map) {
        $value = [ordered]@{}
        for ($index = 0; $index -lt $Count; $index++) {
            $key = Read-MessagePackValue $Bytes $Offset
            $value[[string]$key] = Read-MessagePackValue $Bytes $Offset
        }
        return $value
    }
    $items = [System.Collections.Generic.List[object]]::new()
    for ($index = 0; $index -lt $Count; $index++) {
        $items.Add((Read-MessagePackValue $Bytes $Offset))
    }
    return ,([object[]]$items.ToArray())
}

function Read-MessagePackValue {
    param([byte[]]$Bytes, [ref]$Offset)
    Assert-NodeControlCondition ($Offset.Value -lt $Bytes.Length) `
        'MSGPACK_TRUNCATED'
    $prefix = $Bytes[$Offset.Value]
    $Offset.Value++
    if ($prefix -le 0x7f) { return [long]$prefix }
    if ($prefix -ge 0xe0) { return [long]$prefix - 256 }
    if ($prefix -ge 0xa0 -and $prefix -le 0xbf) {
        return Read-MessagePackStringBody $Bytes $Offset ($prefix -band 0x1f)
    }
    if ($prefix -ge 0x90 -and $prefix -le 0x9f) {
        return Read-MessagePackCollection $Bytes $Offset ($prefix -band 0x0f) $false
    }
    if ($prefix -ge 0x80 -and $prefix -le 0x8f) {
        return Read-MessagePackCollection $Bytes $Offset ($prefix -band 0x0f) $true
    }
    switch ($prefix) {
        0xc0 { return $null }
        0xc2 { return $false }
        0xc3 { return $true }
        0xc4 {
            $length = [int](Read-MessagePackUnsigned $Bytes $Offset 1)
            $value = [byte[]]::new($length)
            [Array]::Copy($Bytes, $Offset.Value, $value, 0, $length)
            $Offset.Value += $length
            return $value
        }
        0xc5 {
            $length = [int](Read-MessagePackUnsigned $Bytes $Offset 2)
            $value = [byte[]]::new($length)
            [Array]::Copy($Bytes, $Offset.Value, $value, 0, $length)
            $Offset.Value += $length
            return $value
        }
        0xcc { return [long](Read-MessagePackUnsigned $Bytes $Offset 1) }
        0xcd { return [long](Read-MessagePackUnsigned $Bytes $Offset 2) }
        0xce { return [long](Read-MessagePackUnsigned $Bytes $Offset 4) }
        0xcf { return [long](Read-MessagePackUnsigned $Bytes $Offset 8) }
        0xd0 {
            $raw = [byte](Read-MessagePackUnsigned $Bytes $Offset 1)
            return $(if ($raw -ge 128) { [long]$raw - 256 } else { [long]$raw })
        }
        0xd1 {
            $raw = [uint64](Read-MessagePackUnsigned $Bytes $Offset 2)
            return $(if ($raw -ge 0x8000) { [long]$raw - 0x10000 } else { [long]$raw })
        }
        0xd2 {
            $raw = [uint64](Read-MessagePackUnsigned $Bytes $Offset 4)
            return $(if ($raw -ge 0x80000000) { [long]$raw - 0x100000000 } else { [long]$raw })
        }
        0xd3 { return [long](Read-MessagePackUnsigned $Bytes $Offset 8) }
        0xd9 {
            $length = [int](Read-MessagePackUnsigned $Bytes $Offset 1)
            return Read-MessagePackStringBody $Bytes $Offset $length
        }
        0xda {
            $length = [int](Read-MessagePackUnsigned $Bytes $Offset 2)
            return Read-MessagePackStringBody $Bytes $Offset $length
        }
        0xdb {
            $length = [int](Read-MessagePackUnsigned $Bytes $Offset 4)
            return Read-MessagePackStringBody $Bytes $Offset $length
        }
        0xdc {
            $count = [int](Read-MessagePackUnsigned $Bytes $Offset 2)
            return Read-MessagePackCollection $Bytes $Offset $count $false
        }
        0xdd {
            $count = [int](Read-MessagePackUnsigned $Bytes $Offset 4)
            return Read-MessagePackCollection $Bytes $Offset $count $false
        }
        0xde {
            $count = [int](Read-MessagePackUnsigned $Bytes $Offset 2)
            return Read-MessagePackCollection $Bytes $Offset $count $true
        }
        0xdf {
            $count = [int](Read-MessagePackUnsigned $Bytes $Offset 4)
            return Read-MessagePackCollection $Bytes $Offset $count $true
        }
        default { throw 'MSGPACK_UNSUPPORTED_TYPE' }
    }
}

function Read-ExactBytes {
    param([IO.Stream]$Stream, [int]$Count)
    $buffer = [byte[]]::new($Count)
    $offset = 0
    while ($offset -lt $Count) {
        $read = $Stream.Read($buffer, $offset, $Count - $offset)
        if ($read -le 0) { throw 'CONTROL_FRAME_TRUNCATED' }
        $offset += $read
    }
    return $buffer
}

function Write-FramedPayload {
    param([IO.Stream]$Stream, [byte[]]$Payload)
    Assert-NodeControlCondition `
        ($Payload.Length -gt 0 -and $Payload.Length -le $script:MaximumControlFrame) `
        'CONTROL_FRAME_SIZE_INVALID'
    $header = ConvertTo-BigEndianBytes -Value $Payload.Length -Count 4
    $Stream.Write($header, 0, $header.Length)
    $Stream.Write($Payload, 0, $Payload.Length)
    $Stream.Flush()
}

function Read-FramedPayload {
    param([IO.Stream]$Stream, [int]$Maximum = $script:MaximumControlFrame)
    $header = Read-ExactBytes $Stream 4
    $offset = 0
    $length = [int](Read-MessagePackUnsigned $header ([ref]$offset) 4)
    Assert-NodeControlCondition ($length -gt 0 -and $length -le $Maximum) `
        'CONTROL_FRAME_SIZE_INVALID'
    return Read-ExactBytes $Stream $length
}

function Open-LocalPipe {
    param([string]$Name, [int]$TimeoutMS = 5000)
    $pipe = [IO.Pipes.NamedPipeClientStream]::new(
        '.',
        $Name,
        [IO.Pipes.PipeDirection]::InOut,
        [IO.Pipes.PipeOptions]::None
    )
    try {
        $pipe.Connect($TimeoutMS)
        return $pipe
    } catch {
        $pipe.Dispose()
        throw
    }
}

function Invoke-ControlRequest {
    param([string]$PipeName, [ValidateSet('status', 'shutdown')][string]$Command)
    $requestID = [Guid]::NewGuid().ToString('N')
    $request = [ordered]@{
        version = 1
        request_id = $requestID
        command = $Command
    }
    $pipe = Open-LocalPipe -Name $PipeName
    try {
        Write-FramedPayload $pipe (ConvertTo-MessagePackValue $request)
        $payload = Read-FramedPayload $pipe
        $offset = 0
        $response = Read-MessagePackValue $payload ([ref]$offset)
        Assert-NodeControlCondition ($offset -eq $payload.Length) `
            'CONTROL_RESPONSE_TRAILING_BYTES'
        Assert-NodeControlCondition `
            ([string]$response.request_id -eq $requestID) `
            'CONTROL_RESPONSE_ID_MISMATCH'
        Assert-NodeControlCondition ([bool]$response.ok) `
            'CONTROL_RESPONSE_FAILED'
        return $response
    } finally {
        $pipe.Dispose()
    }
}

function Wait-ControlStatus {
    param([string]$PipeName, [int]$TimeoutSeconds = 20)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        try {
            $response = Invoke-ControlRequest -PipeName $PipeName -Command status
            if ($null -ne $response.status) { return $response.status }
        } catch {
            Start-Sleep -Milliseconds 100
        }
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'CONTROL_STATUS_TIMEOUT'
}

function Start-TestProcess {
    param(
        [string]$Executable,
        [string]$ConfigPath,
        [string]$Root,
        [string]$Name
    )
    $stdout = Join-Path $Root ($Name + '.stdout.log')
    $stderr = Join-Path $Root ($Name + '.stderr.log')
    $quotedConfig = '"{0}"' -f $ConfigPath.Replace('"', '\"')
    $process = Start-Process `
        -FilePath $Executable `
        -ArgumentList @('-config', $quotedConfig) `
        -WorkingDirectory $Root `
        -RedirectStandardOutput $stdout `
        -RedirectStandardError $stderr `
        -WindowStyle Hidden `
        -PassThru
    $script:StartedProcesses.Add($process)
    return $process
}

function Wait-ProcessExit {
    param([Diagnostics.Process]$Process, [int]$TimeoutSeconds, [string]$Code)
    if (-not $Process.WaitForExit($TimeoutSeconds * 1000)) {
        throw $Code
    }
}

function ConvertTo-ProtocolEnvelope {
    param([int]$MessageType, [byte[]]$Body)
    $stream = [IO.MemoryStream]::new()
    try {
        $stream.WriteByte(0x82)
        Add-ByteSequence $stream (ConvertTo-MessagePackString 't')
        Add-ByteSequence $stream (ConvertTo-MessagePackInteger $MessageType)
        Add-ByteSequence $stream (ConvertTo-MessagePackString 'b')
        Add-ByteSequence $stream $Body
        return $stream.ToArray()
    } finally {
        $stream.Dispose()
    }
}

function Read-ProtocolEnvelope {
    param([IO.Stream]$Stream)
    $payload = Read-FramedPayload -Stream $Stream -Maximum (16MB)
    $offset = 0
    return Read-MessagePackValue $payload ([ref]$offset)
}

function Assert-AgentTCPHello {
    param(
        [System.Collections.IDictionary]$Envelope,
        [string]$ExpectedMachineID,
        [int]$ExpectedPID
    )
    Assert-NodeControlCondition `
        ($null -ne $Envelope -and $Envelope.Contains('t') -and
            [int]$Envelope.t -eq 3 -and $Envelope.Contains('b')) `
        'AGENT_TCP_HELLO_ENVELOPE_INVALID'
    $hello = $Envelope.b
    Assert-NodeControlCondition `
        ($hello -is [System.Collections.IDictionary] -and
            $hello.Contains('version') -and [int]$hello.version -eq 1 -and
            $hello.Contains('machine_id') -and
            [string]$hello.machine_id -eq $ExpectedMachineID -and
            $hello.Contains('pid') -and [int]$hello.pid -eq $ExpectedPID) `
        'AGENT_TCP_HELLO_BODY_INVALID'
}

function Assert-CompleteDeleteReport {
    param(
        [System.Collections.IDictionary]$Envelope,
        [string]$ExpectedTaskID,
        [int]$ExpectedEntries
    )
    Assert-NodeControlCondition `
        ($null -ne $Envelope -and $Envelope.Contains('t') -and
            [int]$Envelope.t -eq 25 -and $Envelope.Contains('b')) `
        'HELPER_DELETE_REPORT_ENVELOPE_INVALID'
    $report = $Envelope.b
    Assert-NodeControlCondition `
        ($report -is [System.Collections.IDictionary] -and
            $report.Contains('task_id') -and
            [string]$report.task_id -eq $ExpectedTaskID -and
            $report.Contains('seq') -and [int]$report.seq -eq 1 -and
            $report.Contains('last_seq') -and
            [int]$report.last_seq -eq 1 -and
            $report.Contains('stats') -and
            $report.Contains('entries')) `
        'HELPER_DELETE_REPORT_BODY_INVALID'
    $stats = $report.stats
    $entries = @($report.entries)
    Assert-NodeControlCondition `
        ($stats -is [System.Collections.IDictionary] -and
            $stats.Contains('total') -and
            [int]$stats.total -eq $ExpectedEntries -and
            $stats.Contains('ok') -and $stats.Contains('failed') -and
            ([int]$stats.ok + [int]$stats.failed) -eq $ExpectedEntries -and
            $entries.Count -eq $ExpectedEntries) `
        'HELPER_DELETE_REPORT_COUNTS_INVALID'
    foreach ($entry in $entries) {
        Assert-NodeControlCondition `
            ($entry -is [System.Collections.IDictionary] -and
                $entry.Contains('path') -and
                -not [string]::IsNullOrWhiteSpace([string]$entry.path) -and
                $entry.Contains('ok')) `
            'HELPER_DELETE_REPORT_ENTRY_INVALID'
    }
}

function Test-IsExplicitTCPDisconnect {
    param([Management.Automation.ErrorRecord]$ErrorRecord)
    if ($null -eq $ErrorRecord) { return $false }
    if ($ErrorRecord.Exception.Message -eq 'CONTROL_FRAME_TRUNCATED') {
        return $true
    }
    $current = $ErrorRecord.Exception
    while ($null -ne $current) {
        if ($current -is [Net.Sockets.SocketException]) {
            return $current.SocketErrorCode -in @(
                [Net.Sockets.SocketError]::ConnectionAborted,
                [Net.Sockets.SocketError]::ConnectionReset,
                [Net.Sockets.SocketError]::Shutdown
            )
        }
        $current = $current.InnerException
    }
    return $false
}

function Assert-NoCredentialLeak {
    param(
        [string[]]$Paths,
        [string]$DSN,
        [string]$Marker
    )
    $needles = [System.Collections.Generic.List[string]]::new()
    if (-not [string]::IsNullOrEmpty($DSN)) { $needles.Add($DSN) }
    if (-not [string]::IsNullOrEmpty($Marker)) { $needles.Add($Marker) }
    if ($DSN -match '^[a-z][a-z0-9+.-]*://[^:/@]+:([^@]+)@') {
        $needles.Add([Uri]::UnescapeDataString($Matches[1]))
    }
    foreach ($path in $Paths) {
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { continue }
        $text = [IO.File]::ReadAllText($path)
        Assert-NodeControlCondition `
            (-not [regex]::IsMatch(
                $text,
                '(?i)postgres(?:ql)?://|password\s*[:=]'
            )) `
            'CREDENTIAL_PATTERN_FOUND_IN_EVIDENCE'
        foreach ($needle in $needles) {
            Assert-NodeControlCondition `
                (-not $text.Contains($needle, [StringComparison]::Ordinal)) `
                'SECRET_VALUE_FOUND_IN_EVIDENCE'
        }
    }
}

function Invoke-DynamicNodeControlChecks {
    foreach ($item in @(
            @{ Value = $TestRoot; Code = 'TEST_ROOT_REQUIRED' },
            @{ Value = $AgentConfig; Code = 'AGENT_CONFIG_REQUIRED' },
            @{ Value = $HelperConfig; Code = 'HELPER_CONFIG_REQUIRED' },
            @{ Value = $AgentExe; Code = 'AGENT_EXE_REQUIRED' },
            @{ Value = $WorkerExe; Code = 'WORKER_EXE_REQUIRED' },
            @{ Value = $HelperExe; Code = 'HELPER_EXE_REQUIRED' },
            @{ Value = $VideoCoreDll; Code = 'VIDEOCORE_DLL_REQUIRED' }
        )) {
        Assert-NodeControlCondition `
            (-not [string]::IsNullOrWhiteSpace([string]$item.Value)) `
            ([string]$item.Code)
    }

    $resolvedAgentConfig = Resolve-RequiredLeaf $AgentConfig 'AGENT_CONFIG_INVALID'
    $resolvedHelperConfig = Resolve-RequiredLeaf $HelperConfig 'HELPER_CONFIG_INVALID'
    $resolvedAgentExe = Resolve-RequiredLeaf $AgentExe 'AGENT_EXE_INVALID'
    $resolvedWorkerExe = Resolve-RequiredLeaf $WorkerExe 'WORKER_EXE_INVALID'
    $resolvedHelperExe = Resolve-RequiredLeaf $HelperExe 'HELPER_EXE_INVALID'
    $resolvedVideoCore = Resolve-RequiredLeaf $VideoCoreDll 'VIDEOCORE_DLL_INVALID'
    Assert-NodeControlCondition `
        ([IO.Path]::GetFileName($resolvedVideoCore) -ieq 'videocore.dll') `
        'VIDEOCORE_DLL_NAME_INVALID'

    $root = Resolve-TestRootCandidate $TestRoot
    Assert-NodeControlCondition (-not (Test-Path -LiteralPath $root)) `
        'TEST_ROOT_ALREADY_EXISTS'
    $createdRoot = $false
    $rootIdentity = $null
    $resolvedRoot = $null
    $marker = 'NODE_CONTROL_SECRET_{0}' -f [Guid]::NewGuid().ToString('N')
    $oldMarker = [Environment]::GetEnvironmentVariable('NODE_CONTROL_TEST_SECRET')
    $resultJSON = $null
    try {
        New-Item -ItemType Directory -Path $root | Out-Null
        $createdRoot = $true
        $resolvedRoot = [string](Resolve-Path -LiteralPath $root).Path
        $rootIdentity = Get-DirectoryIdentity $resolvedRoot
        Assert-NodeControlCondition `
            (-not $rootIdentity.IsReparsePoint) `
            'TEST_ROOT_REPARSE_FORBIDDEN'

        $agentSettings = Get-Content -Raw -LiteralPath $resolvedAgentConfig |
            ConvertFrom-Json
        $helperSettings = Get-Content -Raw -LiteralPath $resolvedHelperConfig |
            ConvertFrom-Json
        Assert-NodeControlCondition `
            ([string]$agentSettings.listen_addr).StartsWith('127.0.0.1:') `
            'AGENT_LISTEN_MUST_BE_LOOPBACK'
        $dataDir = [IO.Path]::GetFullPath(
            $(if ([IO.Path]::IsPathRooted([string]$agentSettings.data_dir)) {
                [string]$agentSettings.data_dir
            } else {
                Join-Path $resolvedRoot ([string]$agentSettings.data_dir)
            })
        )
        Assert-NodeControlCondition (Test-PathInsideRoot $dataDir $resolvedRoot) `
            'AGENT_DATA_DIR_OUTSIDE_TEST_ROOT'
        Assert-NodeControlCondition `
            ([string]$agentSettings.worker.exe_path -and
                ([IO.Path]::GetFullPath([string]$agentSettings.worker.exe_path) -ieq
                    $resolvedWorkerExe)) `
            'AGENT_WORKER_PATH_MISMATCH'
        Assert-NodeControlCondition `
            (@($helperSettings.allowed_roots).Count -gt 0) `
            'HELPER_ALLOWED_ROOT_REQUIRED'
        foreach ($allowed in @($helperSettings.allowed_roots)) {
            Assert-NodeControlCondition `
                (Test-PathInsideRoot ([string]$allowed) $resolvedRoot) `
                'HELPER_ALLOWED_ROOT_OUTSIDE_TEST_ROOT'
        }
        $helperLogDir = Resolve-HelperLogDirectoryForTest `
            ([string]$helperSettings.log_dir) $resolvedRoot
        Assert-NodeControlCondition `
            (-not [bool]$helperSettings.allow_hard_delete) `
            'HELPER_HARD_DELETE_MUST_BE_DISABLED'
        Assert-NodeControlCondition `
            ([string]$helperSettings.pipe_name -ne
                ('\\.\pipe\' + $script:HelperControlPipe)) `
            'HELPER_DELETE_AND_CONTROL_PIPE_COLLIDE'

        $agentRuntimeConfig = Join-Path $resolvedRoot 'agent.runtime.json'
        $helperRuntimeConfig = Join-Path $resolvedRoot 'helper.runtime.json'
        Copy-Item -LiteralPath $resolvedAgentConfig -Destination $agentRuntimeConfig
        Copy-Item -LiteralPath $resolvedHelperConfig -Destination $helperRuntimeConfig
        New-Item -ItemType Directory -Path $dataDir -Force | Out-Null
        foreach ($allowed in @($helperSettings.allowed_roots)) {
            New-Item -ItemType Directory -Path ([string]$allowed) -Force | Out-Null
        }
        New-Item -ItemType Directory -Path $helperLogDir -Force | Out-Null

        [Environment]::SetEnvironmentVariable('NODE_CONTROL_TEST_SECRET', $marker)
        $agent = Start-TestProcess $resolvedAgentExe $agentRuntimeConfig `
            $resolvedRoot 'agent'
        $agentStatus = Wait-ControlStatus $script:AgentControlPipe 30
        Assert-NodeControlCondition ([int]$agentStatus.pid -eq $agent.Id) `
            'AGENT_STATUS_PID_MISMATCH'
        Assert-NodeControlCondition `
            ([IO.Path]::GetFullPath([string]$agentStatus.executable_path) -ieq
                $resolvedAgentExe) `
            'AGENT_STATUS_EXECUTABLE_MISMATCH'
        Assert-NodeControlCondition `
            ([string]$agentStatus.config_sha256 -match '^[0-9a-f]{64}$') `
            'AGENT_STATUS_CONFIG_FINGERPRINT_INVALID'
        Assert-NodeControlCondition `
            (@($agentStatus.workers).Count -eq [int]$agentStatus.worker_expected) `
            'AGENT_STATUS_WORKER_SUMMARY_INVALID'
        $workerPIDs = @($agentStatus.workers | ForEach-Object { [int]$_.pid } |
            Where-Object { $_ -gt 0 })

        $secondAgent = Start-TestProcess $resolvedAgentExe $agentRuntimeConfig `
            $resolvedRoot 'agent-second'
        Wait-ProcessExit $secondAgent 15 'AGENT_SECOND_INSTANCE_DID_NOT_EXIT'
        Assert-NodeControlCondition ($secondAgent.ExitCode -ne 0) `
            'AGENT_SECOND_INSTANCE_WAS_ACCEPTED'

        $listenParts = ([string]$agentSettings.listen_addr).Split(':')
        $tcp = [Net.Sockets.TcpClient]::new()
        try {
            $tcp.Connect($listenParts[0], [int]$listenParts[1])
            $network = $tcp.GetStream()
            $network.ReadTimeout = 5000
            $agentHello = Read-ProtocolEnvelope $network
            Assert-AgentTCPHello `
                $agentHello `
                ([string]$agentSettings.machine_id) `
                $agent.Id
            $probe = [ordered]@{
                version = 1
                request_id = [Guid]::NewGuid().ToString('N')
                command = 'shutdown'
            }
            Write-FramedPayload $network (ConvertTo-MessagePackValue $probe)
            $disconnectObserved = $false
            try {
                $unexpectedPayload = Read-FramedPayload $network (16MB)
                $unexpectedOffset = 0
                $unexpectedMessage = Read-MessagePackValue `
                    $unexpectedPayload ([ref]$unexpectedOffset)
                if ($unexpectedMessage -is [System.Collections.IDictionary] -and
                    $unexpectedMessage.Contains('request_id') -and
                    $unexpectedMessage.Contains('ok')) {
                    throw 'AGENT_TCP_ACCEPTED_CONTROL_COMMAND'
                }
                throw 'AGENT_TCP_RETURNED_FRAME_AFTER_CONTROL_PROBE'
            } catch {
                if (Test-IsExplicitTCPDisconnect $_) {
                    $disconnectObserved = $true
                } else {
                    throw
                }
            }
            Assert-NodeControlCondition $disconnectObserved `
                'AGENT_TCP_REJECTION_NOT_OBSERVED'
        } finally {
            $tcp.Dispose()
        }
        $agentAfterTCPProbe = Wait-ControlStatus $script:AgentControlPipe 10
        Assert-NodeControlCondition `
            ([int]$agentAfterTCPProbe.pid -eq $agent.Id -and
                -not $agent.HasExited) `
            'AGENT_CHANGED_AFTER_TCP_CONTROL_PROBE'

        Invoke-ControlRequest $script:AgentControlPipe shutdown | Out-Null
        Wait-ProcessExit $agent 30 'AGENT_CONTROLLED_SHUTDOWN_TIMEOUT'
        foreach ($workerPID in $workerPIDs) {
            Assert-NodeControlCondition `
                ($null -eq (Get-Process -Id $workerPID -ErrorAction SilentlyContinue)) `
                'AGENT_WORKER_PROCESS_REMAINED'
        }

        $helper = Start-TestProcess $resolvedHelperExe $helperRuntimeConfig `
            $resolvedRoot 'helper'
        $helperStatus = Wait-ControlStatus $script:HelperControlPipe 30
        Assert-NodeControlCondition ([int]$helperStatus.pid -eq $helper.Id) `
            'HELPER_STATUS_PID_MISMATCH'
        Assert-NodeControlCondition `
            ([string]$helperStatus.config_sha256 -match '^[0-9a-f]{64}$') `
            'HELPER_STATUS_CONFIG_FINGERPRINT_INVALID'

        $deletePipeName = [string]$helperSettings.pipe_name
        Assert-NodeControlCondition $deletePipeName.StartsWith('\\.\pipe\') `
            'HELPER_DELETE_PIPE_INVALID'
        $deletePipe = Open-LocalPipe $deletePipeName.Substring(9) 5000
        $heldFiles = [System.Collections.Generic.List[IO.FileStream]]::new()
        try {
            $hello = Read-ProtocolEnvelope $deletePipe
            Assert-NodeControlCondition ([int]$hello.t -eq 3) `
                'HELPER_DELETE_PIPE_HELLO_INVALID'
            $deleteRoot = [string]@($helperSettings.allowed_roots)[0]
            $entries = [System.Collections.Generic.List[string]]::new()
            for ($index = 0; $index -lt 2000; $index++) {
                $path = Join-Path $deleteRoot ('held-{0:D4}.bin' -f $index)
                [IO.File]::WriteAllBytes($path, [byte[]]@(1))
                $heldFiles.Add([IO.File]::Open(
                    $path,
                    [IO.FileMode]::Open,
                    [IO.FileAccess]::ReadWrite,
                    [IO.FileShare]::Read
                ))
                $entries.Add($path)
            }
            $deleteTask = [ordered]@{
                task_id = 'node-control-active-request'
                seq = 1
                last_seq = 1
                mode = 'soft'
                confirmed = $true
                entries = [string[]]$entries.ToArray()
            }
            $body = ConvertTo-MessagePackValue $deleteTask
            Write-FramedPayload $deletePipe `
                (ConvertTo-ProtocolEnvelope 13 $body)

            $activeObserved = $false
            $deadline = [DateTime]::UtcNow.AddSeconds(15)
            do {
                $activeStatus = Wait-ControlStatus $script:HelperControlPipe 2
                if ([int]$activeStatus.active_requests -eq 1) {
                    $activeObserved = $true
                    break
                }
            } while ([DateTime]::UtcNow -lt $deadline)
            Assert-NodeControlCondition $activeObserved `
                'HELPER_ACTIVE_REQUEST_NOT_OBSERVED'

            Invoke-ControlRequest $script:HelperControlPipe shutdown | Out-Null
            $deleteReport = Read-ProtocolEnvelope $deletePipe
            Assert-CompleteDeleteReport `
                $deleteReport `
                'node-control-active-request' `
                2000
        } finally {
            foreach ($stream in $heldFiles) { $stream.Dispose() }
            $deletePipe.Dispose()
        }
        Wait-ProcessExit $helper 30 'HELPER_CONTROLLED_SHUTDOWN_TIMEOUT'
        $newDeleteAccepted = $false
        try {
            $unexpected = Open-LocalPipe $deletePipeName.Substring(9) 500
            $newDeleteAccepted = $true
            $unexpected.Dispose()
        } catch {
        }
        Assert-NodeControlCondition (-not $newDeleteAccepted) `
            'HELPER_ACCEPTED_NEW_TRANSACTION_AFTER_EXIT'

        $evidence = [ordered]@{
            schema_version = 1
            status = 'PASS'
            agent = [ordered]@{
                pid_match = $true
                config_fingerprint = 'present'
                worker_expected = [int]$agentStatus.worker_expected
                worker_ready = [int]$agentStatus.worker_ready
                second_instance_rejected = $true
                controlled_shutdown = $true
                tcp_lifecycle_control = 'rejected'
            }
            helper = [ordered]@{
                delete_and_control_pipes_distinct = $true
                active_requests_observed = 1
                accepted_transaction_drained = $true
                new_transaction_after_exit = 'rejected'
            }
            credential_scan = 'PASS'
        }
        $evidencePath = Join-Path $resolvedRoot 'node-control-result.json'
        $evidence | ConvertTo-Json -Depth 8 | Set-Content `
            -LiteralPath $evidencePath -Encoding utf8NoBOM
        $evidenceFiles = @(
            Get-ChildItem -LiteralPath $resolvedRoot -File |
                Where-Object Name -Match '(\.log|result\.json)$' |
                ForEach-Object FullName
        )
        Assert-NoCredentialLeak $evidenceFiles `
            ([string]$agentSettings.pg_dsn) $marker
        $resultJSON = Get-Content -Raw -LiteralPath $evidencePath
    } finally {
        [Environment]::SetEnvironmentVariable(
            'NODE_CONTROL_TEST_SECRET',
            $oldMarker
        )
        foreach ($process in $script:StartedProcesses) {
            if ($null -ne $process -and -not $process.HasExited) {
                Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
                $process.WaitForExit(5000) | Out-Null
            }
        }
        if ($createdRoot -and (Test-Path -LiteralPath $root)) {
            $validated = Resolve-TestRootCandidate $root
            $currentResolved = [string](Resolve-Path -LiteralPath $validated).Path
            $currentIdentity = Get-DirectoryIdentity $validated
            $safeIdentity = $null -ne $rootIdentity -and
                $null -ne $resolvedRoot -and
                $currentResolved.Equals(
                    $resolvedRoot,
                    [StringComparison]::OrdinalIgnoreCase
                ) -and
                (Test-CleanupRootIdentity `
                    $rootIdentity.FinalPath `
                    $currentIdentity.FinalPath `
                    $rootIdentity.Identity `
                    $currentIdentity.Identity `
                    $currentIdentity.IsReparsePoint)
            if (-not $safeIdentity) {
                throw 'TEST_ROOT_IDENTITY_CHANGED_CLEANUP_REFUSED'
            }
            $reparse = Get-ChildItem -LiteralPath $validated -Force -Recurse `
                -Attributes ReparsePoint -ErrorAction SilentlyContinue |
                Select-Object -First 1
            if ($null -ne $reparse) {
                throw 'TEST_ROOT_CHILD_REPARSE_CLEANUP_REFUSED'
            }
            Remove-Item -LiteralPath $validated -Recurse -Force
        }
    }
    if ($null -ne $resultJSON) { $resultJSON }
}

if ($WhatIfPreference) {
    Invoke-StaticNodeControlChecks
    return
}

Invoke-DynamicNodeControlChecks
