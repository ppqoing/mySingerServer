param(
    [Parameter(Mandatory=$true)][string]$Manifest,
    [Parameter(Mandatory=$true)][string]$Golden,
    [Parameter(Mandatory=$true)][string]$StageDir,
    [Parameter(Mandatory=$true)][string]$Evidence,
    [string]$Runner = ""
)

$ErrorActionPreference = "Stop"

function Resolve-FullPath {
    param([string]$Path)
    if ([IO.Path]::IsPathFullyQualified($Path)) {
        return [IO.Path]::GetFullPath($Path)
    }
    return [IO.Path]::GetFullPath((Join-Path (Get-Location) $Path))
}

function Write-AtomicJson {
    param([string]$Path, $Value)
    $full = Resolve-FullPath $Path
    $parent = [IO.Path]::GetDirectoryName($full)
    [IO.Directory]::CreateDirectory($parent) | Out-Null
    $temporary = "$full.tmp-$([Guid]::NewGuid().ToString('N'))"
    try {
        $json = $Value | ConvertTo-Json -Depth 32
        [IO.File]::WriteAllText($temporary, $json + [Environment]::NewLine,
            [Text.UTF8Encoding]::new($false))
        [IO.File]::Move($temporary, $full, $true)
    }
    finally {
        if (Test-Path -LiteralPath $temporary) {
            Remove-Item -LiteralPath $temporary -Force
        }
    }
}

function Get-RequiredJson {
    param([string]$Path, [string]$Label)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "VIDEOCORE_COMPAT_INPUT_MISSING label=$Label"
    }
    try {
        $document = Get-Content -Raw -LiteralPath $Path | ConvertFrom-Json
        return $document
    }
    catch {
        throw "VIDEOCORE_COMPAT_JSON_INVALID label=$Label"
    }
}

function Get-FixtureMap {
    param([object[]]$Fixtures, [string]$Label)
    $map = @{}
    foreach ($fixture in $Fixtures) {
        $path = [string]$fixture.path
        if ([string]::IsNullOrWhiteSpace($path) -or $map.ContainsKey($path)) {
            throw "VIDEOCORE_COMPAT_FIXTURE_SET_INVALID label=$Label path=$path count=$($map.Count)"
        }
        $map[$path] = $fixture
    }
    return $map
}

function Add-CompatDiff {
    param(
        [Collections.Generic.List[object]]$Diffs,
        [string]$FixturePath,
        [string]$JsonPath,
        $Expected,
        $Actual
    )
    $Diffs.Add([ordered]@{
        fixture_path = $FixturePath
        json_path = $JsonPath
        expected = $Expected
        actual = $Actual
    })
}

function Get-CompatScalarKind {
    param($Value)
    if ($Value -is [string]) { return "string" }
    if ($Value -is [bool]) { return "boolean" }
    if ($Value -is [byte] -or $Value -is [sbyte] -or
        $Value -is [int16] -or $Value -is [uint16] -or
        $Value -is [int32] -or $Value -is [uint32] -or
        $Value -is [int64] -or $Value -is [uint64] -or
        $Value -is [single] -or $Value -is [double] -or $Value -is [decimal]) {
        return "number"
    }
    return $Value.GetType().FullName
}

function Compare-CompatNode {
    param(
        $Expected,
        $Actual,
        [string]$JsonPath,
        [string]$FixturePath,
        [Collections.Generic.List[object]]$Diffs
    )
    if ($null -eq $Expected -or $null -eq $Actual) {
        if ($null -ne $Expected -or $null -ne $Actual) {
            Add-CompatDiff $Diffs $FixturePath $JsonPath $Expected $Actual
        }
        return
    }

    if ($Expected -is [pscustomobject]) {
        if ($Actual -isnot [pscustomobject]) {
            Add-CompatDiff $Diffs $FixturePath $JsonPath $Expected $Actual
            return
        }
        $expectedNames = @($Expected.PSObject.Properties.Name)
        $actualNames = @($Actual.PSObject.Properties.Name)
        foreach ($name in @($expectedNames + $actualNames | Sort-Object -Unique)) {
            $expectedProperty = $Expected.PSObject.Properties[$name]
            $actualProperty = $Actual.PSObject.Properties[$name]
            if ($null -eq $expectedProperty) {
                Add-CompatDiff $Diffs $FixturePath "$JsonPath.$name" "<missing>" $actualProperty.Value
            }
            elseif ($null -eq $actualProperty) {
                Add-CompatDiff $Diffs $FixturePath "$JsonPath.$name" $expectedProperty.Value "<missing>"
            }
            else {
                Compare-CompatNode $expectedProperty.Value $actualProperty.Value `
                    "$JsonPath.$name" $FixturePath $Diffs
            }
        }
        return
    }

    $expectedArray = $Expected -is [Collections.IList] -and $Expected -isnot [string]
    $actualArray = $Actual -is [Collections.IList] -and $Actual -isnot [string]
    if ($expectedArray -or $actualArray) {
        if (-not ($expectedArray -and $actualArray)) {
            Add-CompatDiff $Diffs $FixturePath $JsonPath $Expected $Actual
            return
        }
        $expectedItems = @($Expected)
        $actualItems = @($Actual)
        if ($expectedItems.Count -ne $actualItems.Count) {
            Add-CompatDiff $Diffs $FixturePath "$JsonPath.length" $expectedItems.Count $actualItems.Count
        }
        $count = [Math]::Min($expectedItems.Count, $actualItems.Count)
        for ($index = 0; $index -lt $count; $index++) {
            Compare-CompatNode $expectedItems[$index] $actualItems[$index] `
                "$JsonPath[$index]" $FixturePath $Diffs
        }
        return
    }

    $expectedKind = Get-CompatScalarKind $Expected
    $actualKind = Get-CompatScalarKind $Actual
    if ($expectedKind -cne $actualKind) {
        Add-CompatDiff $Diffs $FixturePath $JsonPath $Expected $Actual
        return
    }
    $equal = if ($expectedKind -eq "number") {
        [decimal]$Expected -eq [decimal]$Actual
    }
    elseif ($expectedKind -eq "string") {
        [string]$Expected -ceq [string]$Actual
    }
    else {
        $Expected -eq $Actual
    }
    if (-not $equal) {
        Add-CompatDiff $Diffs $FixturePath $JsonPath $Expected $Actual
    }
}

$manifestPath = Resolve-FullPath $Manifest
$goldenPath = Resolve-FullPath $Golden
$stagePath = Resolve-FullPath $StageDir
$evidencePath = Resolve-FullPath $Evidence
$evidenceDir = [IO.Path]::GetDirectoryName($evidencePath)
$diffPath = Join-Path $evidenceDir "compat-diff.json"
$actualPath = Join-Path $evidenceDir ("compat-actual.tmp-{0}.json" -f [Guid]::NewGuid().ToString("N"))
$diffs = [Collections.Generic.List[object]]::new()
$errors = [Collections.Generic.List[string]]::new()
$exitCode = 2
$status = "fail"
$fixtureCount = 0
$manifestHash = ""
$goldenHash = ""
$stageManifestHash = ""

try {
    [IO.Directory]::CreateDirectory($evidenceDir) | Out-Null
    $manifestDocument = Get-RequiredJson $manifestPath "manifest"
    $goldenDocument = Get-RequiredJson $goldenPath "golden"
    if ($null -eq $goldenDocument.fixtures) {
        throw "VIDEOCORE_COMPAT_GOLDEN_SHAPE_INVALID type=$($goldenDocument.GetType().FullName) props=$($goldenDocument.PSObject.Properties.Name -join ',')"
    }
    if (-not (Test-Path -LiteralPath $stagePath -PathType Container)) {
        throw "VIDEOCORE_COMPAT_STAGE_MISSING"
    }
    $stageManifestPath = Join-Path $stagePath "release-manifest.json"
    [void](Get-RequiredJson $stageManifestPath "stage_manifest")
    $manifestHash = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $goldenHash = (Get-FileHash -LiteralPath $goldenPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $stageManifestHash = (Get-FileHash -LiteralPath $stageManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()

    if ([string]::IsNullOrWhiteSpace($Runner)) {
        throw "BLOCKED_NOT_RUN: real VideoCore compatibility runner is not configured"
    }
    $runnerPath = Resolve-FullPath $Runner
    if (-not (Test-Path -LiteralPath $runnerPath -PathType Leaf)) {
        throw "BLOCKED_NOT_RUN: compatibility runner is missing"
    }
    $pwsh = Join-Path $PSHOME "pwsh.exe"
    & $pwsh -NoProfile -File $runnerPath `
        -Manifest $manifestPath -StageDir $stagePath -OutFile $actualPath
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $actualPath -PathType Leaf)) {
        throw "VIDEOCORE_COMPAT_RUNNER_FAILED"
    }
    $actualDocument = Get-RequiredJson $actualPath "actual"

    $manifestPaths = @(
        foreach ($fixture in @($manifestDocument.images) + @($manifestDocument.videos)) {
            if ($null -ne $fixture) {
                [string]$fixture.path
            }
        }
    )
    if ($manifestPaths.Count -ne @($manifestPaths | Sort-Object -Unique).Count) {
        throw "VIDEOCORE_COMPAT_MANIFEST_FIXTURE_SET_INVALID"
    }
    $goldenMap = Get-FixtureMap -Fixtures @($goldenDocument.fixtures) -Label "golden"
    $actualMap = Get-FixtureMap -Fixtures @($actualDocument.fixtures) -Label "actual"
    $fixtureCount = $manifestPaths.Count
    foreach ($path in @($manifestPaths + @($goldenMap.Keys) + @($actualMap.Keys) | Sort-Object -Unique)) {
        if ($path -notin $manifestPaths -or -not $goldenMap.ContainsKey($path) -or -not $actualMap.ContainsKey($path)) {
            Add-CompatDiff $diffs $path '$.fixtures' `
                ($goldenMap.ContainsKey($path)) ($actualMap.ContainsKey($path))
        }
    }
    for ($index = 0; $index -lt @($goldenDocument.fixtures).Count; $index++) {
        $expectedFixture = @($goldenDocument.fixtures)[$index]
        $path = [string]$expectedFixture.path
        if ($actualMap.ContainsKey($path)) {
            Compare-CompatNode $expectedFixture $actualMap[$path] `
                "$.fixtures[$index]" $path $diffs
        }
    }

    if ($diffs.Count -eq 0) {
        $exitCode = 0
        $status = "pass"
    }
    else {
        $exitCode = 1
    }
}
catch {
    $errors.Add([string]$_.Exception.Message)
    if ($_.Exception.Message -like "BLOCKED_NOT_RUN*") {
        Write-Host $_.Exception.Message
    } else {
        Write-Host ("VIDEOCORE_COMPAT_ERROR {0}" -f $_.Exception.Message)
    }
}
finally {
    if (Test-Path -LiteralPath $actualPath) {
        Remove-Item -LiteralPath $actualPath -Force
    }
    $diffDocument = [ordered]@{
        schema_version = 1
        status = $status
        differences = $diffs.Count
        diffs = @($diffs)
        errors = @($errors)
    }
    $evidenceDocument = [ordered]@{
        schema_version = 1
        status = $status
        pass = $exitCode -eq 0
        exit_code = $exitCode
        manifest_sha256 = $manifestHash
        golden_sha256 = $goldenHash
        stage_manifest_sha256 = $stageManifestHash
        fixtures = $fixtureCount
        differences = $diffs.Count
        diff_file = $diffPath
        errors = @($errors)
    }
    Write-AtomicJson $diffPath $diffDocument
    Write-AtomicJson $evidencePath $evidenceDocument
}

if ($exitCode -eq 0) {
    Write-Host "VIDEOCORE COMPAT PASS differences=0"
} elseif ($exitCode -eq 1) {
    foreach ($diff in $diffs) {
        Write-Host ("VIDEOCORE COMPAT DIFF fixture={0} path={1} expected={2} actual={3}" -f `
            $diff.fixture_path, $diff.json_path, $diff.expected, $diff.actual)
    }
    Write-Host "VIDEOCORE COMPAT FAIL differences=$($diffs.Count)"
} else {
    Write-Host "VIDEOCORE COMPAT BLOCKED_OR_ERROR"
}
exit $exitCode
