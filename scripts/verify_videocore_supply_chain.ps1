[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Manifest,

    [Parameter(Mandatory = $true)]
    [string]$FFmpegRoot,

    [Parameter(Mandatory = $true)]
    [ValidateSet('Local', 'Release')]
    [string]$Mode,

    [Parameter(Mandatory = $true)]
    [string]$Evidence
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$exitValidationFailed = 2
$exitReleaseBlocked = 3
$exitOperationalFailure = 4
$scriptVersion = 1
$expectedSDKID = 'N-125444-g6d72600a30-20260703'
$expectedConfigureSHA256 = '90a4aa41107cb238202af98543521e0d03139cfd7102b690b087fa5a5db50c1a'
$expectedComponents = [ordered]@{
    libavutil     = '61.2.100'
    libavcodec    = '63.3.100'
    libavformat   = '63.3.100'
    libavdevice   = '63.2.100'
    libavfilter   = '12.2.100'
    libswscale    = '10.2.100'
    libswresample = '7.2.100'
}

if (-not ('VideoCoreFinalPath' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;
using System.Text;
using Microsoft.Win32.SafeHandles;

public static class VideoCoreFinalPath
{
    private const uint FILE_SHARE_READ = 0x00000001;
    private const uint FILE_SHARE_WRITE = 0x00000002;
    private const uint FILE_SHARE_DELETE = 0x00000004;
    private const uint OPEN_EXISTING = 3;
    private const uint FILE_FLAG_BACKUP_SEMANTICS = 0x02000000;

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern SafeFileHandle CreateFileW(
        string fileName,
        uint desiredAccess,
        uint shareMode,
        IntPtr securityAttributes,
        uint creationDisposition,
        uint flagsAndAttributes,
        IntPtr templateFile);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern uint GetFinalPathNameByHandleW(
        SafeFileHandle file,
        StringBuilder path,
        uint pathLength,
        uint flags);

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

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetFileInformationByHandle(
        SafeFileHandle file,
        out BY_HANDLE_FILE_INFORMATION information);

    private static SafeFileHandle Open(string path)
    {
        SafeFileHandle handle = CreateFileW(
            path,
            0,
            FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
            IntPtr.Zero,
            OPEN_EXISTING,
            FILE_FLAG_BACKUP_SEMANTICS,
            IntPtr.Zero);
        if (handle.IsInvalid)
        {
            handle.Dispose();
            throw new Win32Exception(Marshal.GetLastWin32Error());
        }
        return handle;
    }

    public static string Get(string path)
    {
        using (SafeFileHandle handle = Open(path))
        {
            StringBuilder buffer = new StringBuilder(32768);
            uint length = GetFinalPathNameByHandleW(handle, buffer, (uint)buffer.Capacity, 0);
            if (length == 0 || length >= (uint)buffer.Capacity)
            {
                throw new Win32Exception(Marshal.GetLastWin32Error());
            }
            return buffer.ToString();
        }
    }

    public static string GetFileIdentity(string path)
    {
        using (SafeFileHandle handle = Open(path))
        {
            BY_HANDLE_FILE_INFORMATION information;
            if (!GetFileInformationByHandle(handle, out information))
            {
                throw new Win32Exception(Marshal.GetLastWin32Error());
            }
            return String.Format(
                "{0:x8}:{1:x8}{2:x8}",
                information.VolumeSerialNumber,
                information.FileIndexHigh,
                information.FileIndexLow);
        }
    }
}
'@
}

function Add-Issue {
    param(
        [System.Collections.Generic.List[object]]$Issues,
        [string]$Code,
        [string]$Path,
        [string]$Message
    )

    $Issues.Add([ordered]@{
            code    = $Code
            path    = $Path
            message = $Message
        })
}

function ConvertTo-NormalizedWindowsPath {
    param([string]$Path)

    if ([string]::IsNullOrWhiteSpace($Path)) {
        throw 'Path is empty.'
    }
    $value = $Path
    $providerPrefix = 'Microsoft.PowerShell.Core\FileSystem::'
    if ($value.StartsWith($providerPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        $value = $value.Substring($providerPrefix.Length)
    }
    $value = $value.Replace('/', '\')
    if ($value.StartsWith('\\.\', [System.StringComparison]::OrdinalIgnoreCase) -or
        $value.IndexOf('GLOBALROOT', [System.StringComparison]::OrdinalIgnoreCase) -ge 0) {
        throw 'Device namespace paths are not supported.'
    }
    if ($value.StartsWith('\\?\UNC\', [System.StringComparison]::OrdinalIgnoreCase)) {
        $value = '\\' + $value.Substring(8)
    }
    elseif ($value.StartsWith('\\?\', [System.StringComparison]::OrdinalIgnoreCase)) {
        $value = $value.Substring(4)
    }
    elseif ($value.StartsWith('\??\UNC\', [System.StringComparison]::OrdinalIgnoreCase)) {
        $value = '\\' + $value.Substring(8)
    }
    elseif ($value.StartsWith('\??\', [System.StringComparison]::OrdinalIgnoreCase)) {
        $value = $value.Substring(4)
    }
    $fullPath = [System.IO.Path]::GetFullPath($value)
    $pathRoot = [System.IO.Path]::GetPathRoot($fullPath)
    if ($fullPath.Length -gt $pathRoot.Length) {
        $fullPath = $fullPath.TrimEnd('\')
    }
    return $fullPath
}

function Get-FinalPathIdentity {
    param([string]$Path)

    return ConvertTo-NormalizedWindowsPath -Path ([VideoCoreFinalPath]::Get($Path))
}

function Get-FileIdentity {
    param([string]$Path)

    return [VideoCoreFinalPath]::GetFileIdentity($Path)
}

function Get-ProspectiveFinalPathIdentity {
    param([string]$Path)

    $normalized = ConvertTo-NormalizedWindowsPath -Path $Path
    if ([System.IO.File]::Exists($normalized) -or [System.IO.Directory]::Exists($normalized)) {
        return Get-FinalPathIdentity -Path $normalized
    }

    $suffix = [System.Collections.Generic.List[string]]::new()
    $current = $normalized
    while (-not [System.IO.Directory]::Exists($current)) {
        $name = [System.IO.Path]::GetFileName($current)
        if ([string]::IsNullOrWhiteSpace($name)) {
            throw 'Evidence destination has no existing parent.'
        }
        $suffix.Insert(0, $name)
        $parent = [System.IO.Path]::GetDirectoryName($current)
        if ([string]::IsNullOrWhiteSpace($parent) -or $parent -eq $current) {
            throw 'Evidence destination has no existing parent.'
        }
        $current = $parent
    }
    $finalParent = Get-FinalPathIdentity -Path $current
    foreach ($part in $suffix) {
        $finalParent = [System.IO.Path]::Combine($finalParent, $part)
    }
    return ConvertTo-NormalizedWindowsPath -Path $finalParent
}

function Get-NormalizedRelativePath {
    param(
        [string]$Root,
        [string]$FullName
    )

    $relative = [System.IO.Path]::GetRelativePath($Root, $FullName)
    return $relative.Replace('\', '/')
}

function Test-SafeRelativePath {
    param([string]$Path)

    if ([string]::IsNullOrWhiteSpace($Path) -or
        [System.IO.Path]::IsPathRooted($Path) -or
        $Path.Contains('\') -or
        $Path.Contains(':') -or
        $Path.Contains('//') -or
        $Path -match '[\x00-\x1f]') {
        return $false
    }
    $parts = $Path.Split('/')
    if ($parts.Count -eq 0 -or $parts -contains '.' -or $parts -contains '..') {
        return $false
    }
    $reserved = @('CON', 'PRN', 'AUX', 'NUL', 'COM1', 'COM2', 'COM3', 'COM4', 'COM5', 'COM6', 'COM7', 'COM8', 'COM9',
        'LPT1', 'LPT2', 'LPT3', 'LPT4', 'LPT5', 'LPT6', 'LPT7', 'LPT8', 'LPT9')
    foreach ($part in $parts) {
        if ([string]::IsNullOrWhiteSpace($part) -or $part.EndsWith('.') -or $part.EndsWith(' ')) {
            return $false
        }
        $baseName = $part.Split('.')[0]
        if ($reserved -contains $baseName.ToUpperInvariant()) {
            return $false
        }
    }
    return $true
}

function Test-PathInside {
    param(
        [string]$Root,
        [string]$Candidate
    )

    $rootFull = ConvertTo-NormalizedWindowsPath -Path $Root
    $candidateFull = ConvertTo-NormalizedWindowsPath -Path $Candidate
    return $candidateFull.Equals($rootFull, [System.StringComparison]::OrdinalIgnoreCase) -or
        $candidateFull.StartsWith(
            $rootFull + [System.IO.Path]::DirectorySeparatorChar,
            [System.StringComparison]::OrdinalIgnoreCase
        )
}

function Test-ExistingAncestorHasReparsePoint {
    param([string]$Path)

    $current = ConvertTo-NormalizedWindowsPath -Path $Path
    while (-not [System.IO.Directory]::Exists($current)) {
        $parent = [System.IO.Path]::GetDirectoryName($current)
        if ([string]::IsNullOrWhiteSpace($parent) -or $parent -eq $current) {
            return $false
        }
        $current = $parent
    }
    while (-not [string]::IsNullOrWhiteSpace($current)) {
        $item = [System.IO.DirectoryInfo]::new($current)
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            return $true
        }
        $parent = $item.Parent
        if ($null -eq $parent) {
            break
        }
        $current = $parent.FullName
    }
    return $false
}

function Test-FileOrParentReparsePoint {
    param(
        [string]$Root,
        [string]$FullName
    )

    $item = Get-Item -LiteralPath $FullName -Force
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        return $true
    }
    $rootFull = ConvertTo-NormalizedWindowsPath -Path $Root
    $current = $item.Directory
    while ($null -ne $current -and
        -not $current.FullName.Equals($rootFull, [System.StringComparison]::OrdinalIgnoreCase)) {
        if (($current.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            return $true
        }
        $current = $current.Parent
    }
    return $false
}

function Resolve-SafeChildPath {
    param(
        [string]$Root,
        [string]$RelativePath
    )

    if (-not (Test-SafeRelativePath -Path $RelativePath)) {
        return $null
    }
    $candidate = [System.IO.Path]::GetFullPath(
        [System.IO.Path]::Combine($Root, $RelativePath.Replace('/', [System.IO.Path]::DirectorySeparatorChar))
    )
    $prefix = $Root.TrimEnd('\', '/') + [System.IO.Path]::DirectorySeparatorChar
    if (-not $candidate.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        return $null
    }
    return $candidate
}

function Get-RequiredFileKind {
    param([string]$RelativePath)

    if ($RelativePath -match '^include/.+\.h$') {
        return 'header'
    }
    if ($RelativePath -match '^lib/.+\.dll\.a$') {
        return 'gnu-import-lib'
    }
    if ($RelativePath -match '^lib/.+\.lib$') {
        return 'msvc-import-lib'
    }
    if ($RelativePath -match '^bin/.+\.dll$') {
        return 'runtime-dll'
    }
    return $null
}

function Get-StreamingSHA256 {
    param([string]$LiteralPath)

    $stream = $null
    $algorithm = $null
    try {
        $stream = [System.IO.File]::Open(
            $LiteralPath,
            [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::Read,
            [System.IO.FileShare]::Read
        )
        $algorithm = [System.Security.Cryptography.SHA256]::Create()
        $digest = $algorithm.ComputeHash($stream)
        return [System.Convert]::ToHexString($digest).ToLowerInvariant()
    }
    finally {
        if ($null -ne $algorithm) {
            $algorithm.Dispose()
        }
        if ($null -ne $stream) {
            $stream.Dispose()
        }
    }
}

function Get-StringSHA256 {
    param([string]$Value)

    $bytes = [System.Text.Encoding]::UTF8.GetBytes($Value)
    $digest = [System.Security.Cryptography.SHA256]::HashData($bytes)
    return [System.Convert]::ToHexString($digest).ToLowerInvariant()
}

function Write-EvidenceFile {
    param(
        [string]$Path,
        [object]$Value
    )

    $destination = [System.IO.Path]::GetFullPath($Path)
    $parent = [System.IO.Path]::GetDirectoryName($destination)
    [System.IO.Directory]::CreateDirectory($parent) | Out-Null
    $temporary = Join-Path $parent ('.' + [System.IO.Path]::GetFileName($destination) + '.tmp-' + [Guid]::NewGuid().ToString('N'))
    try {
        $json = $Value | ConvertTo-Json -Depth 12
        $content = [System.Text.UTF8Encoding]::new($false).GetBytes($json + [Environment]::NewLine)
        $stream = [System.IO.FileStream]::new(
            $temporary,
            [System.IO.FileMode]::CreateNew,
            [System.IO.FileAccess]::Write,
            [System.IO.FileShare]::None,
            4096,
            [System.IO.FileOptions]::WriteThrough
        )
        try {
            $stream.Write($content, 0, $content.Length)
            $stream.Flush($true)
        }
        finally {
            $stream.Dispose()
        }
        [System.IO.File]::Move($temporary, $destination, $true)
    }
    finally {
        if ([System.IO.File]::Exists($temporary)) {
            [System.IO.File]::Delete($temporary)
        }
    }
}

$issues = [System.Collections.Generic.List[object]]::new()
$verifiedFiles = [System.Collections.Generic.List[object]]::new()
$verifiedDocuments = [System.Collections.Generic.List[object]]::new()
$schemaErrors = [System.Collections.Generic.List[string]]::new()
$manifestObject = $null
$manifestPath = ''
$rootPath = ''
$schemaStatus = 'not_run'
$integrityStatus = 'not_run'
$expectedCount = 0
$discoveredCount = 0
$sdkID = ''
$redistributionStatus = 'not_run'
$redistributable = $false
$blockers = @()
$components = @()
$manifestSHA256 = ''
$schemaSHA256 = ''
$finalStatus = 'fail'
$finalExitCode = $exitOperationalFailure
$evidencePreflightApproved = $false
$evidenceWriteAuthorized = $false
$evidenceFinalPath = ''
$evidenceInitialFileIdentity = ''
$protectedFileIdentities = [System.Collections.Generic.HashSet[string]]::new(
    [System.StringComparer]::OrdinalIgnoreCase
)

try {
    $lexicalRootPath = ConvertTo-NormalizedWindowsPath -Path $FFmpegRoot
    $lexicalManifestPath = ConvertTo-NormalizedWindowsPath -Path $Manifest
    $lexicalSchemaPath = Join-Path ([System.IO.Path]::GetDirectoryName($lexicalManifestPath)) 'manifest.schema.json'
    $evidencePathFull = ConvertTo-NormalizedWindowsPath -Path $Evidence
    if ((Test-PathInside -Root $lexicalRootPath -Candidate $evidencePathFull) -or
        $evidencePathFull.Equals($lexicalManifestPath, [System.StringComparison]::OrdinalIgnoreCase) -or
        $evidencePathFull.Equals($lexicalSchemaPath, [System.StringComparison]::OrdinalIgnoreCase) -or
        (Test-ExistingAncestorHasReparsePoint -Path ([System.IO.Path]::GetDirectoryName($evidencePathFull)))) {
        [Console]::Error.WriteLine(
            'EVIDENCE_PATH_PROTECTED: Evidence must be outside FFmpegRoot and must not traverse a reparse point.'
        )
        exit $exitOperationalFailure
    }
    $evidencePreflightApproved = $true
}
catch {
    [Console]::Error.WriteLine(
        'EVIDENCE_PREFLIGHT_FAILED: Evidence destination could not be safely normalized.'
    )
    exit $exitOperationalFailure
}

try {
    $manifestPath = ConvertTo-NormalizedWindowsPath -Path (Resolve-Path -LiteralPath $Manifest -ErrorAction Stop).Path
    $rootPath = ConvertTo-NormalizedWindowsPath -Path (Resolve-Path -LiteralPath $FFmpegRoot -ErrorAction Stop).Path
    if (-not [System.IO.Directory]::Exists($rootPath)) {
        throw "FFmpegRoot is not a directory."
    }
    $manifestItem = Get-Item -LiteralPath $manifestPath -Force
    if ((Test-ExistingAncestorHasReparsePoint -Path $rootPath) -or
        (Test-ExistingAncestorHasReparsePoint -Path ([System.IO.Path]::GetDirectoryName($manifestPath))) -or
        (($manifestItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)) {
        [Console]::Error.WriteLine(
            'REPARSE_POINT_FORBIDDEN: FFmpegRoot and manifest control paths must not traverse a reparse point.'
        )
        exit $exitOperationalFailure
    }

    $schemaPath = Join-Path (Split-Path -Parent $manifestPath) 'manifest.schema.json'
    $evidencePathFull = ConvertTo-NormalizedWindowsPath -Path $Evidence
    if ((Test-PathInside -Root $rootPath -Candidate $evidencePathFull) -or
        $evidencePathFull.Equals($manifestPath, [System.StringComparison]::OrdinalIgnoreCase) -or
        $evidencePathFull.Equals($schemaPath, [System.StringComparison]::OrdinalIgnoreCase) -or
        (Test-ExistingAncestorHasReparsePoint -Path ([System.IO.Path]::GetDirectoryName($evidencePathFull)))) {
        [Console]::Error.WriteLine(
            'EVIDENCE_PATH_PROTECTED: Evidence must be outside FFmpegRoot and must not traverse a reparse point.'
        )
        exit $exitOperationalFailure
    }

    if (-not (Test-Path -LiteralPath $schemaPath -PathType Leaf)) {
        Add-Issue -Issues $issues -Code 'MANIFEST_SCHEMA_MISSING' -Path 'manifest.schema.json' `
            -Message 'The manifest schema file is missing.'
    }
    else {
        $schemaItem = Get-Item -LiteralPath $schemaPath -Force
        if ((Test-ExistingAncestorHasReparsePoint -Path ([System.IO.Path]::GetDirectoryName($schemaPath))) -or
            (($schemaItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)) {
            [Console]::Error.WriteLine(
                'REPARSE_POINT_FORBIDDEN: manifest schema control paths must not traverse a reparse point.'
            )
            exit $exitOperationalFailure
        }

        $rootFinalPath = Get-FinalPathIdentity -Path $rootPath
        $manifestFinalPath = Get-FinalPathIdentity -Path $manifestPath
        $schemaFinalPath = Get-FinalPathIdentity -Path $schemaPath
        if (-not (Test-PathInside -Root $rootFinalPath -Candidate $manifestFinalPath) -or
            -not (Test-PathInside -Root $rootFinalPath -Candidate $schemaFinalPath)) {
            [Console]::Error.WriteLine(
                'CONTROL_PATH_IDENTITY_MISMATCH: manifest and schema must resolve inside FFmpegRoot.'
            )
            exit $exitOperationalFailure
        }

        [void]$protectedFileIdentities.Add((Get-FileIdentity -Path $manifestPath))
        [void]$protectedFileIdentities.Add((Get-FileIdentity -Path $schemaPath))
        $evidenceFinalPath = Get-ProspectiveFinalPathIdentity -Path $evidencePathFull
        if ([System.IO.File]::Exists($evidencePathFull) -or
            [System.IO.Directory]::Exists($evidencePathFull)) {
            $evidenceInitialFileIdentity = Get-FileIdentity -Path $evidencePathFull
        }
        if ((Test-PathInside -Root $rootPath -Candidate $evidencePathFull) -or
            (Test-PathInside -Root $rootFinalPath -Candidate $evidenceFinalPath) -or
            $evidenceFinalPath.Equals($manifestFinalPath, [System.StringComparison]::OrdinalIgnoreCase) -or
            $evidenceFinalPath.Equals($schemaFinalPath, [System.StringComparison]::OrdinalIgnoreCase) -or
            ((-not [string]::IsNullOrWhiteSpace($evidenceInitialFileIdentity)) -and
                $protectedFileIdentities.Contains($evidenceInitialFileIdentity)) -or
            (Test-ExistingAncestorHasReparsePoint -Path ([System.IO.Path]::GetDirectoryName($evidencePathFull)))) {
            [Console]::Error.WriteLine(
                'EVIDENCE_PATH_PROTECTED: Evidence aliases a protected FFmpeg control or SDK path.'
            )
            exit $exitOperationalFailure
        }

        $manifestRaw = [System.IO.File]::ReadAllText($manifestPath)
        try {
            $schemaValid = Test-Json -Json $manifestRaw -SchemaFile $schemaPath `
                -ErrorAction Stop -WarningAction SilentlyContinue
            if (-not $schemaValid) {
                throw 'JSON does not conform to the schema.'
            }
            $manifestObject = $manifestRaw | ConvertFrom-Json -Depth 20
            $manifestSHA256 = Get-StreamingSHA256 -LiteralPath $manifestPath
            $schemaSHA256 = Get-StreamingSHA256 -LiteralPath $schemaPath
            $schemaStatus = 'pass'
        }
        catch {
            $schemaStatus = 'fail'
            $schemaErrors.Add('manifest.json does not conform to manifest.schema.json')
            Add-Issue -Issues $issues -Code 'MANIFEST_SCHEMA_INVALID' -Path 'manifest.json' `
                -Message 'The manifest does not conform to the pinned JSON schema.'
        }
    }

    if ($schemaStatus -eq 'pass') {
        $sdkID = [string]$manifestObject.sdk_id
        $expectedCount = @($manifestObject.files).Count
        $blockers = @($manifestObject.blockers | ForEach-Object {
                [ordered]@{
                    code    = [string]$_.code
                    message = [string]$_.message
                }
            })
        $components = @($manifestObject.components | Sort-Object name | ForEach-Object {
                [ordered]@{
                    name    = [string]$_.name
                    version = [string]$_.version
                    major   = [int]$_.major
                }
            })

        if ($sdkID -cne $expectedSDKID) {
            Add-Issue -Issues $issues -Code 'SDK_ID_MISMATCH' -Path 'sdk_id' `
                -Message 'The manifest does not identify the pinned FFmpeg SDK.'
        }
        $componentMap = [System.Collections.Generic.Dictionary[string, object]]::new(
            [System.StringComparer]::Ordinal
        )
        foreach ($component in @($manifestObject.components)) {
            $name = [string]$component.name
            if ($componentMap.ContainsKey($name)) {
                Add-Issue -Issues $issues -Code 'DUPLICATE_COMPONENT' -Path $name `
                    -Message 'The manifest contains a duplicate component.'
                continue
            }
            $componentMap.Add($name, $component)
        }
        foreach ($expectedName in $expectedComponents.Keys) {
            if (-not $componentMap.ContainsKey($expectedName)) {
                Add-Issue -Issues $issues -Code 'COMPONENT_SET_MISMATCH' -Path $expectedName `
                    -Message 'A pinned FFmpeg component is missing.'
                continue
            }
            $component = $componentMap[$expectedName]
            $version = [string]$component.version
            $major = [int]$component.major
            $versionMajor = [int]($version.Split('.')[0])
            if ($version -cne $expectedComponents[$expectedName] -or $major -ne $versionMajor) {
                Add-Issue -Issues $issues -Code 'COMPONENT_VERSION_MISMATCH' -Path $expectedName `
                    -Message 'The component version or major differs from the pinned SDK.'
            }
        }
        if ($componentMap.Count -ne $expectedComponents.Count) {
            Add-Issue -Issues $issues -Code 'COMPONENT_SET_MISMATCH' -Path 'components' `
                -Message 'The component set differs from the pinned SDK.'
        }

        $configureDigest = Get-StringSHA256 -Value (@($manifestObject.provenance.configure_flags) -join "`n")
        if ($configureDigest -cne $expectedConfigureSHA256) {
            Add-Issue -Issues $issues -Code 'CONFIGURE_FLAGS_MISMATCH' -Path 'provenance.configure_flags' `
                -Message 'The ordered configure flags differ from the locally pinned SDK evidence.'
        }

        $manifestByPath = [System.Collections.Generic.Dictionary[string, object]]::new(
            [System.StringComparer]::OrdinalIgnoreCase
        )
        foreach ($entry in @($manifestObject.files)) {
            $relativePath = [string]$entry.path
            if (-not (Test-SafeRelativePath -Path $relativePath)) {
                Add-Issue -Issues $issues -Code 'FILE_PATH_INVALID' -Path $relativePath `
                    -Message 'Manifest file paths must be normalized relative paths inside FFmpegRoot.'
                continue
            }
            if ($manifestByPath.ContainsKey($relativePath)) {
                Add-Issue -Issues $issues -Code 'DUPLICATE_FILE_PATH' -Path $relativePath `
                    -Message 'The manifest contains a duplicate file path.'
                continue
            }
            $manifestByPath.Add($relativePath, $entry)
        }

        $discoveredByPath = [System.Collections.Generic.Dictionary[string, object]]::new(
            [System.StringComparer]::OrdinalIgnoreCase
        )
        foreach ($file in Get-ChildItem -LiteralPath $rootPath -Recurse -File -Force) {
            $relativePath = Get-NormalizedRelativePath -Root $rootPath -FullName $file.FullName
            $kind = Get-RequiredFileKind -RelativePath $relativePath
            if ($null -eq $kind) {
                continue
            }
            if (Test-FileOrParentReparsePoint -Root $rootPath -FullName $file.FullName) {
                Add-Issue -Issues $issues -Code 'REPARSE_POINT_FORBIDDEN' -Path $relativePath `
                    -Message 'Required SDK inputs must not traverse a reparse point.'
                continue
            }
            $fileFinalPath = Get-FinalPathIdentity -Path $file.FullName
            $fileIdentity = Get-FileIdentity -Path $file.FullName
            if (-not (Test-PathInside -Root $rootFinalPath -Candidate $fileFinalPath)) {
                Add-Issue -Issues $issues -Code 'INPUT_PATH_IDENTITY_MISMATCH' -Path $relativePath `
                    -Message 'A required SDK input resolves outside FFmpegRoot.'
            }
            [void]$protectedFileIdentities.Add($fileIdentity)
            if ((-not [string]::IsNullOrWhiteSpace($evidenceInitialFileIdentity)) -and
                $fileIdentity.Equals($evidenceInitialFileIdentity, [System.StringComparison]::OrdinalIgnoreCase)) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_INPUT_IDENTITY_COLLISION' -Path $relativePath `
                    -Message 'A required SDK input has the same Windows file identity as Evidence.'
            }
            if ($discoveredByPath.ContainsKey($relativePath)) {
                Add-Issue -Issues $issues -Code 'DISCOVERED_PATH_COLLISION' -Path $relativePath `
                    -Message 'Multiple SDK inputs normalize to the same Windows path.'
                continue
            }
            $discoveredByPath.Add($relativePath, [ordered]@{
                    full_path = $file.FullName
                    kind      = $kind
                    size      = [int64]$file.Length
                })
        }
        $discoveredCount = $discoveredByPath.Count

        foreach ($relativePath in $discoveredByPath.Keys | Sort-Object) {
            if (-not $manifestByPath.ContainsKey($relativePath)) {
                Add-Issue -Issues $issues -Code 'UNLISTED_REQUIRED_FILE' -Path $relativePath `
                    -Message 'A required header, import library, or runtime DLL is not listed in the manifest.'
            }
        }
        foreach ($relativePath in $manifestByPath.Keys | Sort-Object) {
            $entry = $manifestByPath[$relativePath]
            $expectedKind = Get-RequiredFileKind -RelativePath $relativePath
            if ($null -eq $expectedKind) {
                Add-Issue -Issues $issues -Code 'FILE_KIND_UNSUPPORTED' -Path $relativePath `
                    -Message 'The manifest entry is not a required header, import library, or runtime DLL.'
                continue
            }
            if ([string]$entry.kind -cne $expectedKind) {
                Add-Issue -Issues $issues -Code 'FILE_KIND_MISMATCH' -Path $relativePath `
                    -Message 'The manifest file kind does not match the path.'
            }
            if (-not $discoveredByPath.ContainsKey($relativePath)) {
                Add-Issue -Issues $issues -Code 'MISSING_REQUIRED_FILE' -Path $relativePath `
                    -Message 'A manifest-listed file is missing from FFmpegRoot.'
                continue
            }

            $actual = $discoveredByPath[$relativePath]
            $actualHash = Get-StreamingSHA256 -LiteralPath $actual.full_path
            $verifiedFiles.Add([ordered]@{
                    path   = $relativePath
                    kind   = $actual.kind
                    size   = [int64]$actual.size
                    sha256 = $actualHash
                })
            if ([int64]$entry.size -ne [int64]$actual.size) {
                Add-Issue -Issues $issues -Code 'SIZE_MISMATCH' -Path $relativePath `
                    -Message 'The file size differs from the manifest.'
            }
            if ([string]$entry.sha256 -ine $actualHash) {
                Add-Issue -Issues $issues -Code 'HASH_MISMATCH' -Path $relativePath `
                    -Message 'The independently streamed SHA-256 differs from the manifest.'
            }
        }

        $documentChecks = @(
            @{
                path = [string]$manifestObject.provenance.source_document
                code = 'SOURCE_DOCUMENT_MISSING'
                message = 'The corresponding-source evidence document is missing.'
            },
            @{
                path = [string]$manifestObject.license.license_file
                code = 'LICENSE_DOCUMENT_MISSING'
                message = 'The license evidence document is missing.'
            },
            @{
                path = [string]$manifestObject.license.notice_file
                code = 'NOTICE_DOCUMENT_MISSING'
                message = 'The redistribution notice document is missing.'
            }
        )
        foreach ($document in $documentChecks) {
            $documentPath = Resolve-SafeChildPath -Root $rootPath -RelativePath $document.path
            if ($null -eq $documentPath -or -not (Test-Path -LiteralPath $documentPath -PathType Leaf)) {
                Add-Issue -Issues $issues -Code $document.code -Path $document.path `
                    -Message $document.message
            }
        }

        $expectedEvidencePaths = [ordered]@{
            source  = [string]$manifestObject.provenance.source_document
            license = [string]$manifestObject.license.license_file
            notice  = [string]$manifestObject.license.notice_file
        }
        $evidenceByRole = [System.Collections.Generic.Dictionary[string, object]]::new(
            [System.StringComparer]::Ordinal
        )
        $evidencePaths = [System.Collections.Generic.HashSet[string]]::new(
            [System.StringComparer]::OrdinalIgnoreCase
        )
        foreach ($document in @($manifestObject.evidence_documents)) {
            $role = [string]$document.role
            $relativePath = [string]$document.path
            if ($evidenceByRole.ContainsKey($role)) {
                Add-Issue -Issues $issues -Code 'DUPLICATE_EVIDENCE_ROLE' -Path $role `
                    -Message 'Each evidence-document role must appear exactly once.'
                continue
            }
            $evidenceByRole.Add($role, $document)
            if (-not $evidencePaths.Add($relativePath)) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_DOCUMENT_ALIAS' -Path $relativePath `
                    -Message 'Source, license, and notice evidence must be distinct files.'
            }
        }
        foreach ($role in $expectedEvidencePaths.Keys) {
            if (-not $evidenceByRole.ContainsKey($role)) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_DOCUMENT_MISSING' -Path $role `
                    -Message 'A required evidence-document role is missing.'
                continue
            }
            $document = $evidenceByRole[$role]
            $relativePath = [string]$document.path
            if ($relativePath -cne $expectedEvidencePaths[$role]) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_DOCUMENT_PATH_MISMATCH' -Path $relativePath `
                    -Message 'The evidence-document role does not match its fixed manifest path.'
                continue
            }
            $fullPath = Resolve-SafeChildPath -Root $rootPath -RelativePath $relativePath
            if ($null -eq $fullPath -or -not (Test-Path -LiteralPath $fullPath -PathType Leaf)) {
                continue
            }
            if (Test-FileOrParentReparsePoint -Root $rootPath -FullName $fullPath) {
                Add-Issue -Issues $issues -Code 'REPARSE_POINT_FORBIDDEN' -Path $relativePath `
                    -Message 'Evidence documents must not traverse a reparse point.'
                continue
            }
            $documentFinalPath = Get-FinalPathIdentity -Path $fullPath
            $documentIdentity = Get-FileIdentity -Path $fullPath
            if (-not (Test-PathInside -Root $rootFinalPath -Candidate $documentFinalPath)) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_DOCUMENT_IDENTITY_MISMATCH' -Path $relativePath `
                    -Message 'An evidence document resolves outside FFmpegRoot.'
            }
            [void]$protectedFileIdentities.Add($documentIdentity)
            if ((-not [string]::IsNullOrWhiteSpace($evidenceInitialFileIdentity)) -and
                $documentIdentity.Equals($evidenceInitialFileIdentity, [System.StringComparison]::OrdinalIgnoreCase)) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_INPUT_IDENTITY_COLLISION' -Path $relativePath `
                    -Message 'An evidence document has the same Windows file identity as Evidence.'
            }
            $item = Get-Item -LiteralPath $fullPath
            $actualHash = Get-StreamingSHA256 -LiteralPath $fullPath
            $verifiedDocuments.Add([ordered]@{
                    role   = $role
                    path   = $relativePath
                    size   = [int64]$item.Length
                    sha256 = $actualHash
                })
            if ([int64]$document.size -ne [int64]$item.Length) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_DOCUMENT_SIZE_MISMATCH' -Path $relativePath `
                    -Message 'The evidence-document size differs from the manifest.'
            }
            if ([string]$document.sha256 -ine $actualHash) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_DOCUMENT_HASH_MISMATCH' -Path $relativePath `
                    -Message 'The evidence-document SHA-256 differs from the manifest.'
            }
        }

        $licenseClass = [string]$manifestObject.license.classification
        if ($licenseClass -in @('unknown', 'nonfree')) {
            Add-Issue -Issues $issues -Code 'LICENSE_FORBIDDEN' -Path 'license.classification' `
                -Message "License classification '$licenseClass' is forbidden."
        }

        if (-not [bool]$manifestObject.redistributable -and $blockers.Count -eq 0) {
            Add-Issue -Issues $issues -Code 'REDISTRIBUTION_BLOCKERS_MISSING' -Path 'blockers' `
                -Message 'A non-redistributable manifest must record at least one explicit blocker.'
        }

        if ($issues.Count -eq 0) {
            $reboundEvidenceFinalPath = Get-ProspectiveFinalPathIdentity -Path $evidencePathFull
            if (-not $reboundEvidenceFinalPath.Equals(
                    $evidenceFinalPath,
                    [System.StringComparison]::OrdinalIgnoreCase
                )) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_PATH_IDENTITY_CHANGED' -Path '' `
                    -Message 'Evidence path identity changed while required inputs were being verified.'
            }
            if (Test-ExistingAncestorHasReparsePoint -Path ([System.IO.Path]::GetDirectoryName($evidencePathFull))) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_PATH_BOUNDARY_CHANGED' -Path '' `
                    -Message 'Evidence parent boundary changed while required inputs were being verified.'
            }

            $evidenceCurrentFileIdentity = ''
            if ([System.IO.File]::Exists($evidencePathFull) -or
                [System.IO.Directory]::Exists($evidencePathFull)) {
                $evidenceCurrentFileIdentity = Get-FileIdentity -Path $evidencePathFull
            }
            if ((-not [string]::IsNullOrWhiteSpace($evidenceInitialFileIdentity)) -and
                (-not $evidenceInitialFileIdentity.Equals(
                        $evidenceCurrentFileIdentity,
                        [System.StringComparison]::OrdinalIgnoreCase
                    ))) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_FILE_IDENTITY_CHANGED' -Path '' `
                    -Message 'Evidence file identity changed while required inputs were being verified.'
            }
            if ((-not [string]::IsNullOrWhiteSpace($evidenceCurrentFileIdentity)) -and
                $protectedFileIdentities.Contains($evidenceCurrentFileIdentity)) {
                Add-Issue -Issues $issues -Code 'EVIDENCE_INPUT_IDENTITY_COLLISION' -Path '' `
                    -Message 'Evidence has the same Windows file identity as a protected input.'
            }
        }

        if ($issues.Count -eq 0) {
            $integrityStatus = 'pass'
            if ([bool]$manifestObject.redistributable) {
                $blockers += [ordered]@{
                    code = 'AUTHORITATIVE_REVIEW_GATE_REQUIRED'
                    message = 'Self-asserted manifest fields cannot authorize redistribution; a separately controlled authority gate is required.'
                }
            }
            $redistributable = $false
            $redistributionStatus = 'blocked'
            $finalStatus = 'release_blocked'
            $evidenceWriteAuthorized = $true
            if ($Mode -eq 'Release') {
                $finalExitCode = $exitReleaseBlocked
            }
            else {
                $finalExitCode = 0
            }
        }
        else {
            $integrityStatus = 'fail'
            $redistributionStatus = 'not_run'
            $finalStatus = 'fail'
            $finalExitCode = $exitValidationFailed
        }
    }
    else {
        $integrityStatus = 'not_run'
        $redistributionStatus = 'not_run'
        $finalStatus = 'fail'
        $finalExitCode = $exitValidationFailed
    }
}
catch {
    Add-Issue -Issues $issues -Code 'OPERATIONAL_FAILURE' -Path '' `
        -Message 'The verifier could not read or validate the requested inputs.'
    $finalStatus = 'fail'
    $finalExitCode = $exitOperationalFailure
}

$evidenceObject = [ordered]@{
    schema_version    = $scriptVersion
    generated_at_utc = [DateTime]::UtcNow.ToString('o')
    mode              = $Mode
    status            = $finalStatus
    exit_code         = $finalExitCode
    sdk_id            = $sdkID
    components        = @($components)
    digests           = [ordered]@{
        manifest_sha256 = $manifestSHA256
        schema_sha256   = $schemaSHA256
    }
    schema_validation = [ordered]@{
        status = $schemaStatus
        errors = @($schemaErrors)
    }
    file_integrity   = [ordered]@{
        status           = $integrityStatus
        expected_count   = $expectedCount
        discovered_count = $discoveredCount
        verified         = @($verifiedFiles)
    }
    evidence_documents = @($verifiedDocuments)
    redistribution  = [ordered]@{
        status          = $redistributionStatus
        redistributable = $redistributable
        blockers        = @($blockers)
    }
    errors           = @($issues)
}

if (-not $evidencePreflightApproved -or -not $evidenceWriteAuthorized) {
    Write-Output 'VIDEOCORE FFMPEG SUPPLY CHAIN FAIL'
    foreach ($issue in $issues) {
        $location = if ([string]::IsNullOrWhiteSpace([string]$issue.path)) { '' } else { " [$($issue.path)]" }
        Write-Output "$($issue.code)$($location): $($issue.message)"
    }
    [Console]::Error.WriteLine(
        'EVIDENCE_WRITE_NOT_AUTHORIZED: required input verification did not authorize Evidence output.'
    )
    if ($finalExitCode -eq 0) {
        exit $exitOperationalFailure
    }
    exit $finalExitCode
}

try {
    Write-EvidenceFile -Path $Evidence -Value $evidenceObject
}
catch {
    [Console]::Error.WriteLine('VIDEOCORE FFMPEG SUPPLY CHAIN FAIL: evidence file could not be written.')
    exit $exitOperationalFailure
}

if ($finalStatus -eq 'pass') {
    Write-Output "VIDEOCORE FFMPEG REDISTRIBUTION PASS ($($verifiedFiles.Count) files)"
}
elseif ($finalStatus -eq 'release_blocked') {
    Write-Output "RELEASE BLOCKED: authoritative FFmpeg redistribution evidence is incomplete ($($verifiedFiles.Count) files verified)"
    foreach ($blocker in $blockers) {
        Write-Output "BLOCKER $($blocker.code): $($blocker.message)"
    }
}
else {
    Write-Output 'VIDEOCORE FFMPEG SUPPLY CHAIN FAIL'
    foreach ($issue in $issues) {
        $location = if ([string]::IsNullOrWhiteSpace([string]$issue.path)) { '' } else { " [$($issue.path)]" }
        Write-Output "$($issue.code)${location}: $($issue.message)"
    }
}

exit $finalExitCode
