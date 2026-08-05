[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Manifest,

    [Parameter(Mandatory = $true)]
    [string]$OutFile,

    [Parameter(Mandatory = $true)]
    [string]$LegacyBinDir
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$versionProcessTimeout = [TimeSpan]::FromSeconds(10)
$probeProcessTimeout = [TimeSpan]::FromSeconds(15)
$frameProcessTimeout = [TimeSpan]::FromSeconds(30)
$processTerminationGrace = [TimeSpan]::FromSeconds(2)
if (-not [string]::IsNullOrWhiteSpace($env:VIDEOCORE_CAPTURE_PROCESS_TIMEOUT_MS)) {
    [int]$timeoutOverrideMS = 0
    if (-not [int]::TryParse(
            $env:VIDEOCORE_CAPTURE_PROCESS_TIMEOUT_MS,
            [Globalization.NumberStyles]::None,
            [Globalization.CultureInfo]::InvariantCulture,
            [ref]$timeoutOverrideMS
        ) -or
        $timeoutOverrideMS -lt 1 -or
        $timeoutOverrideMS -gt 300000) {
        throw "VIDEOCORE_CAPTURE_PROCESS_TIMEOUT_MS must be between 1 and 300000"
    }
    $versionProcessTimeout = [TimeSpan]::FromMilliseconds($timeoutOverrideMS)
    $probeProcessTimeout = $versionProcessTimeout
    $frameProcessTimeout = $versionProcessTimeout
}

function Get-FullPath([string]$Path, [string]$BasePath = (Get-Location).Path) {
    if ([IO.Path]::IsPathFullyQualified($Path)) {
        return [IO.Path]::GetFullPath($Path)
    }
    return [IO.Path]::GetFullPath((Join-Path $BasePath $Path))
}

function Assert-ChildPath([string]$Root, [string]$Candidate) {
    $rootWithSeparator = $Root.TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
    if (-not $Candidate.StartsWith($rootWithSeparator, [StringComparison]::OrdinalIgnoreCase)) {
        throw "fixture path escapes compatibility root: $Candidate"
    }
}

function Assert-NoReparsePointInPath([string]$Root, [string]$RelativePath) {
    $candidate = Get-FullPath $RelativePath $Root
    Assert-ChildPath $Root $candidate
    $rootItem = Get-Item -LiteralPath $Root -Force
    if (($rootItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "fixture compatibility root is a reparse point"
    }
    $segments = $RelativePath -split '[\\/]'
    $current = $Root
    foreach ($segment in $segments) {
        if ([string]::IsNullOrWhiteSpace($segment) -or $segment -eq '.' -or $segment -eq '..') {
            throw "fixture path has an invalid segment: $RelativePath"
        }
        $current = Join-Path $current $segment
        $item = Get-Item -LiteralPath $current -Force
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "fixture path contains a reparse point: $RelativePath"
        }
    }
}

function Get-LowerFileHash([string]$Path, [string]$Algorithm) {
    return (Get-FileHash -LiteralPath $Path -Algorithm $Algorithm).Hash.ToLowerInvariant()
}

function Invoke-CapturedProcess(
    [string]$FilePath,
    [string[]]$Arguments,
    [TimeSpan]$Timeout
) {
    if ($Timeout -le [TimeSpan]::Zero -or $Timeout.TotalMilliseconds -gt [int]::MaxValue) {
        throw "process timeout is outside the supported finite range"
    }
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = $FilePath
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    foreach ($argument in $Arguments) {
        [void]$start.ArgumentList.Add($argument)
    }
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $start
    try {
        if (-not $process.Start()) {
            throw "could not start $FilePath"
        }
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        $readerTasks = [Threading.Tasks.Task[]]@($stdoutTask, $stderrTask)
        $timeoutMS = [int][Math]::Ceiling($Timeout.TotalMilliseconds)
        $graceMS = [int][Math]::Ceiling($processTerminationGrace.TotalMilliseconds)
        $processName = [IO.Path]::GetFileName($FilePath)
        if (-not $process.WaitForExit($timeoutMS)) {
            $killFailed = $false
            try {
                $process.Kill($true)
            }
            catch {
                $killFailed = $true
            }
            $processExited = $process.WaitForExit($graceMS)
            $readersDrained = $false
            try {
                $readersDrained = [Threading.Tasks.Task]::WaitAll($readerTasks, $graceMS)
            }
            catch {
                $readersDrained = $false
            }
            if ($killFailed) {
                throw "process deadline exceeded; process-tree termination failed: $processName"
            }
            if (-not $processExited) {
                throw "process deadline exceeded; process tree did not exit within 2s termination grace: $processName"
            }
            if (-not $readersDrained) {
                throw "process deadline exceeded; output readers did not drain within 2s termination grace: $processName"
            }
            throw "process deadline exceeded; process tree terminated within 2s grace: $processName"
        }
        $readersDrained = $false
        try {
            $readersDrained = [Threading.Tasks.Task]::WaitAll($readerTasks, $graceMS)
        }
        catch {
            $readersDrained = $false
        }
        if (-not $readersDrained) {
            throw "process exited; output readers did not drain within 2s grace: $processName"
        }
        return [pscustomobject]@{
            ExitCode = $process.ExitCode
            Stdout = $stdoutTask.GetAwaiter().GetResult()
            Stderr = $stderrTask.GetAwaiter().GetResult()
        }
    }
    finally {
        $process.Dispose()
    }
}

function Convert-ToHex([byte[]]$Bytes) {
    return [Convert]::ToHexString($Bytes).ToLowerInvariant()
}

function Convert-FFmpegError([string]$Message, [string]$FixturePath, [string]$TemporaryPath) {
    $clean = $Message.Replace($FixturePath, '<fixture>', [StringComparison]::OrdinalIgnoreCase)
    if ($TemporaryPath) {
        $clean = $clean.Replace($TemporaryPath, '<frame>', [StringComparison]::OrdinalIgnoreCase)
    }
    $clean = [regex]::Replace(
        $clean,
        '@\s+(?:0x)?[0-9A-Fa-f]{8,}',
        '@ <address>'
    )
    $clean = [regex]::Replace($clean, '\b0x[0-9A-Fa-f]{8,}\b', '<address>')
    $clean = [regex]::Replace($clean, '\belapsed=\S+', '<elapsed>')
    $clean = [regex]::Replace($clean, '\bspeed=\S+', '<speed>')
    $clean = [regex]::Replace($clean, '\bfps=\S+', '<fps>')
    $lines = $clean -split '\r?\n' | Where-Object {
        $trimmed = $_.Trim()
        $trimmed -ne '' -and
        $trimmed -notmatch '^frame=\s*\d+'
    }
    if ($lines.Count -gt 8) {
        $lines = $lines[($lines.Count - 8)..($lines.Count - 1)]
    }
    return ($lines -join "`n").Trim()
}

function Get-SampleTimesMicros([Int64]$DurationMicros) {
    $durationMS = [Int64][Math]::Round(
        $DurationMicros / 1000.0,
        0,
        [MidpointRounding]::AwayFromZero
    )
    [Int64]$remainder = 0
    $quotient = [Math]::DivRem($durationMS, [Int64]12, [ref]$remainder)
    $result = [Collections.Generic.List[Int64]]::new()
    foreach ($multiplier in 1, 3, 5, 7, 9, 11) {
        $timeMS = $quotient * $multiplier +
            [Int64][Math]::Floor(([Decimal]$remainder * $multiplier) / 12)
        $result.Add($timeMS * 1000)
    }
    return $result.ToArray()
}

$nativeSource = @'
using System;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;

public sealed class LegacyMediaCoreImage
{
    public int Width;
    public int Height;
    public byte[] PDQ;
    public int Quality;
    public ulong[] PHashParts;
    public uint[] SobelFloatBits;
}

public static class LegacyMediaCore
{
    private const int ErrorBufferLength = 256;
    private const int PDQBytes = 32;
    private const int PHashParts = 9;
    private const int SobelDimensions = 128;

    [StructLayout(LayoutKind.Sequential)]
    private struct McImage
    {
        public int Width;
        public int Height;
        public IntPtr Gray;
    }

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool SetDllDirectory(string path);

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern IntPtr mc_version();

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern IntPtr mc_sha512_new();

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern void mc_sha512_free(IntPtr context);

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern int mc_sha512_update(
        IntPtr context, byte[] data, UIntPtr length,
        StringBuilder error, UIntPtr errorLength);

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern int mc_sha512_final(
        IntPtr context, byte[] output,
        StringBuilder error, UIntPtr errorLength);

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern int mc_decode_gray(
        byte[] data, UIntPtr length, ref McImage image,
        StringBuilder error, UIntPtr errorLength);

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern void mc_free_image(ref McImage image);

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern int mc_pdq256_from_gray(
        IntPtr gray, int width, int height, byte[] output, out int quality,
        StringBuilder error, UIntPtr errorLength);

    [DllImport("mediacore.dll", CallingConvention = CallingConvention.Cdecl)]
    private static extern int mc_phase2_image(
        ref McImage image, IntPtr output,
        StringBuilder error, UIntPtr errorLength);

    private static StringBuilder ErrorBuffer()
    {
        return new StringBuilder(ErrorBufferLength);
    }

    private static void EnsureSuccess(string operation, int result, StringBuilder error)
    {
        if (result != 0)
        {
            throw new InvalidOperationException(
                operation + " failed (" + result + "): " + error.ToString());
        }
    }

    public static string Version()
    {
        return Marshal.PtrToStringAnsi(mc_version()) ?? "";
    }

    public static byte[] SHA512File(string path)
    {
        IntPtr context = mc_sha512_new();
        if (context == IntPtr.Zero)
        {
            throw new InvalidOperationException("mc_sha512_new returned null");
        }
        try
        {
            byte[] buffer = new byte[1024 * 1024];
            using (FileStream stream = File.OpenRead(path))
            {
                while (true)
                {
                    int count = stream.Read(buffer, 0, buffer.Length);
                    if (count == 0)
                    {
                        break;
                    }
                    byte[] chunk = buffer;
                    if (count != buffer.Length)
                    {
                        chunk = new byte[count];
                        Buffer.BlockCopy(buffer, 0, chunk, 0, count);
                    }
                    StringBuilder error = ErrorBuffer();
                    int result = mc_sha512_update(
                        context, chunk, new UIntPtr((uint)count),
                        error, new UIntPtr(ErrorBufferLength));
                    EnsureSuccess("mc_sha512_update", result, error);
                }
            }
            byte[] output = new byte[64];
            StringBuilder finalError = ErrorBuffer();
            int finalResult = mc_sha512_final(
                context, output, finalError, new UIntPtr(ErrorBufferLength));
            EnsureSuccess("mc_sha512_final", finalResult, finalError);
            return output;
        }
        finally
        {
            mc_sha512_free(context);
        }
    }

    public static LegacyMediaCoreImage AnalyzeImage(byte[] data)
    {
        McImage image = new McImage();
        StringBuilder decodeError = ErrorBuffer();
        int decodeResult = mc_decode_gray(
            data, new UIntPtr((uint)data.Length), ref image,
            decodeError, new UIntPtr(ErrorBufferLength));
        EnsureSuccess("mc_decode_gray", decodeResult, decodeError);
        try
        {
            byte[] pdq = new byte[PDQBytes];
            int quality;
            StringBuilder pdqError = ErrorBuffer();
            int pdqResult = mc_pdq256_from_gray(
                image.Gray, image.Width, image.Height, pdq, out quality,
                pdqError, new UIntPtr(ErrorBufferLength));
            EnsureSuccess("mc_pdq256_from_gray", pdqResult, pdqError);

            int phase2Bytes = PHashParts * sizeof(ulong) + SobelDimensions * sizeof(float);
            IntPtr phase2 = Marshal.AllocHGlobal(phase2Bytes);
            try
            {
                for (int offset = 0; offset < phase2Bytes; offset += sizeof(int))
                {
                    Marshal.WriteInt32(phase2, offset, 0);
                }
                StringBuilder phase2Error = ErrorBuffer();
                int phase2Result = mc_phase2_image(
                    ref image, phase2, phase2Error, new UIntPtr(ErrorBufferLength));
                EnsureSuccess("mc_phase2_image", phase2Result, phase2Error);

                ulong[] pHashParts = new ulong[PHashParts];
                for (int index = 0; index < pHashParts.Length; index++)
                {
                    pHashParts[index] = unchecked(
                        (ulong)Marshal.ReadInt64(phase2, index * sizeof(ulong)));
                }
                uint[] sobelBits = new uint[SobelDimensions];
                int sobelOffset = PHashParts * sizeof(ulong);
                for (int index = 0; index < sobelBits.Length; index++)
                {
                    sobelBits[index] = unchecked(
                        (uint)Marshal.ReadInt32(phase2, sobelOffset + index * sizeof(float)));
                }
                return new LegacyMediaCoreImage
                {
                    Width = image.Width,
                    Height = image.Height,
                    PDQ = pdq,
                    Quality = quality,
                    PHashParts = pHashParts,
                    SobelFloatBits = sobelBits
                };
            }
            finally
            {
                Marshal.FreeHGlobal(phase2);
            }
        }
        finally
        {
            mc_free_image(ref image);
        }
    }
}
'@

if (-not [string]::IsNullOrWhiteSpace($env:VIDEOCORE_CAPTURE_TIMEOUT_HELPER) -or
    -not [string]::IsNullOrWhiteSpace($env:VIDEOCORE_CAPTURE_TIMEOUT_HELPER_PID_FILE)) {
    if ([string]::IsNullOrWhiteSpace($env:VIDEOCORE_CAPTURE_TIMEOUT_HELPER) -or
        [string]::IsNullOrWhiteSpace($env:VIDEOCORE_CAPTURE_TIMEOUT_HELPER_PID_FILE)) {
        throw "timeout helper path and PID file must be provided together"
    }
    $helperPath = Get-FullPath $env:VIDEOCORE_CAPTURE_TIMEOUT_HELPER
    $helperPIDFile = Get-FullPath $env:VIDEOCORE_CAPTURE_TIMEOUT_HELPER_PID_FILE
    $helperHost = (Get-Process -Id $PID).Path
    [void](Invoke-CapturedProcess $helperHost @(
        '-NoProfile',
        '-File', $helperPath,
        '-PidFile', $helperPIDFile
    ) $frameProcessTimeout)
    throw "timeout helper unexpectedly exited before its deadline"
}

$manifestPath = Get-FullPath $Manifest
$outPath = Get-FullPath $OutFile
$legacyBinPath = Get-FullPath $LegacyBinDir
$compatRoot = [IO.Path]::GetDirectoryName($manifestPath)
$manifestHashBefore = Get-LowerFileHash $manifestPath 'SHA256'

if (Test-Path -LiteralPath $outPath) {
    throw "refusing to overwrite frozen legacy golden: $outPath"
}
$outParent = [IO.Path]::GetDirectoryName($outPath)
if (-not (Test-Path -LiteralPath $outParent -PathType Container)) {
    throw "golden output directory does not exist: $outParent"
}

$mediaCoreDLL = Join-Path $legacyBinPath 'mediacore.dll'
$ffmpegPath = Join-Path $legacyBinPath 'tools\ffmpeg.exe'
$ffprobePath = Join-Path $legacyBinPath 'tools\ffprobe.exe'
$approvedComponents = @(
    [ordered]@{
        role = 'image-feature-library'
        path = 'mediacore.dll'
        fullPath = $mediaCoreDLL
        sha256 = '2260110367bf43b368cbfc70dbdb316556588b5a60eb832f699292352ab463df'
        version = '1.0.0'
    },
    [ordered]@{
        role = 'frame-extractor'
        path = 'tools/ffmpeg.exe'
        fullPath = $ffmpegPath
        sha256 = '5f3c767af1cdbb9c44ad14478ce5fc036aec20e6a724755caa2f70abb9655c3f'
        version = 'ffmpeg version N-125444-g6d72600a30-20260703 Copyright (c) 2000-2026 the FFmpeg developers'
    },
    [ordered]@{
        role = 'media-probe'
        path = 'tools/ffprobe.exe'
        fullPath = $ffprobePath
        sha256 = '5d54bcd31343e6b0471bccc2159fa324af2af3ef986474343f572872e9fbeaac'
        version = 'ffprobe version N-125444-g6d72600a30-20260703 Copyright (c) 2007-2026 the FFmpeg developers'
    }
)
foreach ($component in $approvedComponents) {
    if (-not (Test-Path -LiteralPath $component.fullPath -PathType Leaf)) {
        throw "legacy component is missing: $($component.fullPath)"
    }
    $actualComponentHash = Get-LowerFileHash $component.fullPath 'SHA256'
    if ($actualComponentHash -ne $component.sha256) {
        throw "legacy component SHA-256 is not approved: $($component.role) actual=$actualComponentHash approved=$($component.sha256)"
    }
}

$manifestObject = Get-Content -Raw -LiteralPath $manifestPath | ConvertFrom-Json
if ($manifestObject.schemaVersion -ne 1) {
    throw "unsupported manifest schemaVersion: $($manifestObject.schemaVersion)"
}
$fixtures = @($manifestObject.images) + @($manifestObject.videos)
if ($fixtures.Count -lt 12) {
    throw "compatibility manifest has only $($fixtures.Count) fixtures"
}

foreach ($fixture in $fixtures) {
    if (@($fixture.scenarios) -notcontains 'synthetic') {
        throw "fixture is not explicitly synthetic: $($fixture.path)"
    }
    $fixturePath = Get-FullPath $fixture.path $compatRoot
    Assert-ChildPath $compatRoot $fixturePath
    Assert-NoReparsePointInPath $compatRoot $fixture.path
    if ((Get-LowerFileHash $fixturePath 'SHA256') -ne $fixture.sha256) {
        throw "fixture SHA-256 does not match manifest: $($fixture.path)"
    }
}

Add-Type -TypeDefinition $nativeSource -Language CSharp
if (-not [LegacyMediaCore]::SetDllDirectory($legacyBinPath)) {
    throw "SetDllDirectory failed for legacy bin directory: $legacyBinPath"
}

$mediaCoreVersion = [LegacyMediaCore]::Version()
$ffmpegVersionResult = Invoke-CapturedProcess $ffmpegPath @('-version') $versionProcessTimeout
if ($ffmpegVersionResult.ExitCode -ne 0) {
    throw "legacy ffmpeg -version failed: $($ffmpegVersionResult.Stderr)"
}
$ffmpegVersion = ($ffmpegVersionResult.Stdout -split '\r?\n')[0].Trim()
$ffprobeVersionResult = Invoke-CapturedProcess $ffprobePath @('-version') $versionProcessTimeout
if ($ffprobeVersionResult.ExitCode -ne 0) {
    throw "legacy ffprobe -version failed: $($ffprobeVersionResult.Stderr)"
}
$ffprobeVersion = ($ffprobeVersionResult.Stdout -split '\r?\n')[0].Trim()
if ($mediaCoreVersion -ne $approvedComponents[0].version -or
    $ffmpegVersion -ne $approvedComponents[1].version -or
    $ffprobeVersion -ne $approvedComponents[2].version) {
    throw "legacy component version does not match the approved identity"
}

function Get-ImageResult([string]$Path) {
    $features = [LegacyMediaCore]::AnalyzeImage([IO.File]::ReadAllBytes($Path))
    return [ordered]@{
        width = $features.Width
        height = $features.Height
        pdqHex = Convert-ToHex $features.PDQ
        quality = $features.Quality
        pHashPartsHex = @($features.PHashParts | ForEach-Object { $_.ToString('x16') })
        sobelFloatBitsHex = @($features.SobelFloatBits | ForEach-Object { $_.ToString('x8') })
    }
}

function Get-VideoResult([object]$Fixture, [string]$Path) {
    $probeResult = Invoke-CapturedProcess $ffprobePath @(
        '-v', 'error',
        '-show_entries',
        'format=duration:stream=index,codec_type,codec_name,width,height,sample_aspect_ratio,has_b_frames:stream_side_data=rotation',
        '-of', 'json',
        $Path
    ) $probeProcessTimeout
    if ($probeResult.ExitCode -ne 0) {
        throw "ffprobe failed for $($Fixture.path): $($probeResult.Stderr.Trim())"
    }
    $probe = $probeResult.Stdout | ConvertFrom-Json
    $videoStream = @($probe.streams | Where-Object { $_.codec_type -eq 'video' }) |
        Select-Object -First 1
    $audioStream = @($probe.streams | Where-Object { $_.codec_type -eq 'audio' }) |
        Select-Object -First 1
    $sourceStream = if ($null -ne $videoStream) { $videoStream } else { $audioStream }
    if ($null -eq $sourceStream) {
        throw "no supported source stream for $($Fixture.path)"
    }
    $streamType = [string]$sourceStream.codec_type
    $sourceCodec = [string]$sourceStream.codec_name
    if ($sourceCodec -eq 'mjpeg') {
        $sourceCodec = 'jpeg'
    }
    if ($sourceCodec -ne [string]$Fixture.codec) {
        throw "codec metadata mismatch for $($Fixture.path): probe=$sourceCodec manifest=$($Fixture.codec)"
    }
    $durationMicros = [Int64][Decimal]::Round(
        [Decimal]::Parse(
            [string]$probe.format.duration,
            [Globalization.CultureInfo]::InvariantCulture
        ) * 1000000,
        0,
        [MidpointRounding]::AwayFromZero
    )
    if ($durationMicros -ne [Int64]$Fixture.durationMicros) {
        throw "duration metadata mismatch for $($Fixture.path): probe=$durationMicros manifest=$($Fixture.durationMicros)"
    }

    $rotation = 0
    if ($null -ne $videoStream -and
        $null -ne $videoStream.PSObject.Properties['side_data_list']) {
        $rotationValue = @($videoStream.side_data_list |
            Where-Object { $null -ne $_.rotation } |
            Select-Object -First 1)
        if ($rotationValue.Count -ne 0) {
            $rotation = [int]$rotationValue[0].rotation
        }
    }
    if ($rotation -ne [int]$Fixture.rotation) {
        throw "rotation metadata mismatch for $($Fixture.path): probe=$rotation manifest=$($Fixture.rotation)"
    }
    $sourceSAR = 'n/a'
    if ($null -ne $sourceStream.PSObject.Properties['sample_aspect_ratio'] -and
        -not [string]::IsNullOrWhiteSpace([string]$sourceStream.sample_aspect_ratio)) {
        $sourceSAR = [string]$sourceStream.sample_aspect_ratio
    }
    if ($sourceSAR -ne [string]$Fixture.sar) {
        throw "SAR metadata mismatch for $($Fixture.path): probe=$sourceSAR manifest=$($Fixture.sar)"
    }

    $sampleTimes = Get-SampleTimesMicros $durationMicros
    $frames = [Collections.Generic.List[object]]::new()
    for ($sampleIndex = 0; $sampleIndex -lt $sampleTimes.Count; $sampleIndex++) {
        $requestedMicros = $sampleTimes[$sampleIndex]
        if ($null -eq $videoStream) {
            $frames.Add([ordered]@{
                sampleIndex = $sampleIndex
                requestedMicros = $requestedMicros
                error = [ordered]@{
                    stage = 'stream'
                    message = 'no video stream'
                }
            })
            continue
        }

        $framePath = Join-Path ([IO.Path]::GetTempPath()) (
            'videocore-legacy-frame-' + [Guid]::NewGuid().ToString('N') + '.png'
        )
        $identityPath = Join-Path ([IO.Path]::GetTempPath()) (
            'videocore-identity-frame-' + [Guid]::NewGuid().ToString('N') + '.png'
        )
        try {
            $seek = ([Decimal]$requestedMicros / 1000000).ToString(
                '0.000',
                [Globalization.CultureInfo]::InvariantCulture
            )
            $frameResult = Invoke-CapturedProcess $ffmpegPath @(
                '-nostdin',
                '-hide_banner',
                '-loglevel', 'info',
                '-i', $Path,
                '-ss', $seek,
                '-frames:v', '1',
                '-vf', 'scale=512:512:force_original_aspect_ratio=decrease,format=gray',
                '-an', '-sn', '-dn',
                '-f', 'image2',
                '-vcodec', 'png',
                '-y', $framePath
            ) $frameProcessTimeout
            if ($frameResult.ExitCode -ne 0 -or
                -not (Test-Path -LiteralPath $framePath -PathType Leaf) -or
                (Get-Item -LiteralPath $framePath).Length -eq 0) {
                $frames.Add([ordered]@{
                    sampleIndex = $sampleIndex
                    requestedMicros = $requestedMicros
                    error = [ordered]@{
                        stage = 'ffmpeg'
                        exitCode = $frameResult.ExitCode
                        message = Convert-FFmpegError $frameResult.Stderr $Path $framePath
                    }
                })
                continue
            }

            $identityResult = Invoke-CapturedProcess $ffmpegPath @(
                '-nostdin',
                '-hide_banner',
                '-loglevel', 'info',
                '-i', $Path,
                '-frames:v', '1',
                '-vf', "showinfo,select=gte(t\,$seek),scale=512:512:force_original_aspect_ratio=decrease,format=gray",
                '-an', '-sn', '-dn',
                '-f', 'image2',
                '-vcodec', 'png',
                '-y', $identityPath
            ) $frameProcessTimeout
            if ($identityResult.ExitCode -ne 0 -or
                -not (Test-Path -LiteralPath $identityPath -PathType Leaf) -or
                (Get-Item -LiteralPath $identityPath).Length -eq 0) {
                throw "source-frame identity capture failed for $($Fixture.path) sample $sampleIndex"
            }
            $frameSHA256 = Get-LowerFileHash $framePath 'SHA256'
            $identitySHA256 = Get-LowerFileHash $identityPath 'SHA256'
            if ($frameSHA256 -ne $identitySHA256) {
                throw "source-frame identity does not match legacy selection for $($Fixture.path) sample $sampleIndex"
            }
            $identityMatches = [regex]::Matches(
                $identityResult.Stderr,
                'n:\s*(?<n>\d+)\s+pts:\s*(?<pts>-?\d+)\s+pts_time:(?<ptsTime>[-+0-9.eE]+).*?iskey:(?<key>[01])\s+type:(?<type>[A-Z?])'
            )
            $identityMatch = $null
            $ptsSeconds = [Decimal]0
            $requestedSeconds = [Decimal]$requestedMicros / 1000000
            foreach ($candidateMatch in $identityMatches) {
                $candidatePTS = [Decimal]::Parse(
                    $candidateMatch.Groups['ptsTime'].Value,
                    [Globalization.CultureInfo]::InvariantCulture
                )
                if ($candidatePTS -ge $requestedSeconds) {
                    $identityMatch = $candidateMatch
                    $ptsSeconds = $candidatePTS
                    break
                }
            }
            if ($null -eq $identityMatch) {
                throw "could not parse selected frame identity for $($Fixture.path) sample $sampleIndex"
            }
            $features = Get-ImageResult $framePath
            $frames.Add([ordered]@{
                sampleIndex = $sampleIndex
                requestedMicros = $requestedMicros
                selectedIdentity = [ordered]@{
                    sourceDecodeOrdinal = [int]$identityMatch.Groups['n'].Value
                    pts = [Int64]$identityMatch.Groups['pts'].Value
                    ptsTimeMicros = [Int64][Decimal]::Round(
                        $ptsSeconds * 1000000,
                        0,
                        [MidpointRounding]::AwayFromZero
                    )
                    keyFrame = $identityMatch.Groups['key'].Value -eq '1'
                    pictureType = $identityMatch.Groups['type'].Value
                }
                displayWidth = $features.width
                displayHeight = $features.height
                outputFrameSHA256 = $frameSHA256
                pdqHex = $features.pdqHex
                quality = $features.quality
                pHashPartsHex = $features.pHashPartsHex
                sobelFloatBitsHex = $features.sobelFloatBitsHex
            })
        }
        finally {
            if (Test-Path -LiteralPath $framePath) {
                Remove-Item -LiteralPath $framePath -Force
            }
            if (Test-Path -LiteralPath $identityPath) {
                Remove-Item -LiteralPath $identityPath -Force
            }
        }
    }

    $source = [ordered]@{
        streamType = $streamType
        codec = $sourceCodec
        width = if ($null -eq $videoStream) { 0 } else { [int]$videoStream.width }
        height = if ($null -eq $videoStream) { 0 } else { [int]$videoStream.height }
        rotation = $rotation
        sar = $sourceSAR
        hasBFrames = if ($null -eq $videoStream) { 0 } else { [int]$videoStream.has_b_frames }
    }
    return [ordered]@{
        durationMicros = $durationMicros
        sampleTimesMicros = @($sampleTimes)
        source = $source
        frames = @($frames)
    }
}

$goldenFixtures = [Collections.Generic.List[object]]::new()
foreach ($fixture in $fixtures) {
    $fixturePath = Get-FullPath $fixture.path $compatRoot
    $entry = [ordered]@{
        path = [string]$fixture.path
        sha512 = Convert-ToHex ([LegacyMediaCore]::SHA512File($fixturePath))
    }
    if ($fixture.mediaType -eq 'image') {
        $entry.image = Get-ImageResult $fixturePath
    }
    else {
        $entry.video = Get-VideoResult $fixture $fixturePath
    }
    $goldenFixtures.Add($entry)
}

$golden = [ordered]@{
    schemaVersion = 1
    generator = [ordered]@{
        kind = 'legacy-mediacore-plus-ffmpeg-exe'
        components = @($approvedComponents | ForEach-Object {
            [ordered]@{
                role = $_.role
                path = $_.path
                sha256 = $_.sha256
                version = $_.version
            }
        })
    }
    standardSampleMicros = @(83000, 250000, 416000, 583000, 750000, 916000)
    approvedSemanticDeltas = @(
        [ordered]@{
            id = 'sar-corrected-feature-geometry'
            fixturePath = 'videos/h264-sar-4x3.mp4'
            approval = 'approved-design-delta'
            legacyBehavior = 'raw-pixel-scaling-before-features'
            legacyDisplayWidth = 512
            legacyDisplayHeight = 341
            futureBehavior = 'apply-sar-before-feature-scaling'
            futureDisplayWidth = 512
            futureDisplayHeight = 256
        }
    )
    fixtures = @($goldenFixtures)
}

$json = ($golden | ConvertTo-Json -Depth 20).Replace("`r`n", "`n") + "`n"
$temporaryOutput = Join-Path $outParent (
    '.' + [IO.Path]::GetFileName($outPath) + '.tmp-' + [Guid]::NewGuid().ToString('N')
)
$resultHash = $null
try {
    $jsonBytes = [Text.UTF8Encoding]::new($false).GetBytes($json)
    $stream = [IO.FileStream]::new(
        $temporaryOutput,
        [IO.FileMode]::CreateNew,
        [IO.FileAccess]::Write,
        [IO.FileShare]::None,
        4096,
        [IO.FileOptions]::WriteThrough
    )
    try {
        $stream.Write($jsonBytes, 0, $jsonBytes.Length)
        $stream.Flush($true)
    }
    finally {
        $stream.Dispose()
    }
    if ($env:VIDEOCORE_CAPTURE_FAULT_AFTER_TEMP_WRITE -eq '1') {
        throw "injected failure after temporary golden flush"
    }

    $roundTrip = Get-Content -Raw -LiteralPath $temporaryOutput | ConvertFrom-Json
    if ($roundTrip.schemaVersion -ne 1 -or
        @($roundTrip.generator.components).Count -ne 3 -or
        @($roundTrip.approvedSemanticDeltas).Count -ne 1 -or
        @($roundTrip.fixtures).Count -ne $fixtures.Count) {
        throw "temporary golden failed invariant validation"
    }
    $manifestHashAfter = Get-LowerFileHash $manifestPath 'SHA256'
    if ($manifestHashAfter -ne $manifestHashBefore) {
        throw "manifest changed during legacy capture"
    }
    $resultHash = Get-LowerFileHash $temporaryOutput 'SHA256'
    [IO.File]::Move($temporaryOutput, $outPath, $false)
    $temporaryOutput = $null
}
finally {
    if ($null -ne $temporaryOutput -and (Test-Path -LiteralPath $temporaryOutput)) {
        Remove-Item -LiteralPath $temporaryOutput -Force
    }
}
Write-Output "fixtures=$($fixtures.Count) images=$(@($manifestObject.images).Count) videos=$(@($manifestObject.videos).Count)"
Write-Output "mediacore=$mediaCoreVersion"
Write-Output "ffmpeg=$ffmpegVersion"
Write-Output "ffprobe=$ffprobeVersion"
Write-Output "manifest-sha256=$manifestHashAfter"
Write-Output "golden-sha256=$resultHash"
