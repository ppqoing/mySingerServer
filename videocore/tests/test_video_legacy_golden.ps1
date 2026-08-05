param(
    [Parameter(Mandatory = $true)][string]$Executable,
    [Parameter(Mandatory = $true)][string]$SarReference,
    [Parameter(Mandatory = $true)][string]$SarFixture,
    [Parameter(Mandatory = $true)][string]$Golden
)

$ErrorActionPreference = 'Stop'
$fieldFailures = [Collections.Generic.List[string]]::new()

function Assert-Equal($Actual, $Expected, [string]$Label) {
    if ([string]$Actual -cne [string]$Expected) {
        $script:fieldFailures.Add("$Label expected=[$Expected] actual=[$Actual]")
    }
}

$fixtureNames = @(
    'h264-standard.mp4',
    'h264-bframes.mp4',
    'h264-rotate90.mp4',
    'h264-sar-4x3.mp4',
    'h264-short.mp4',
    'truncated-container.mp4',
    'corrupt-packet.ts',
    'audio-only.m4a'
)

$nativeOutput = @(& $Executable 2>&1 | ForEach-Object { "$_" })
$nativeExit = $LASTEXITCODE
if ($nativeExit -ne 0) {
    $nativeOutput | ForEach-Object { Write-Host $_ }
    throw "focused native test failed with exit code $nativeExit"
}

$observed = @{}
foreach ($line in $nativeOutput) {
    if (-not $line.StartsWith('LEGACY_FRAME|')) { continue }
    $parts = $line.Split('|')
    if ($parts.Count -ne 16) {
        throw "invalid LEGACY_FRAME field count: $line"
    }
    $key = "$($parts[1])#$($parts[2])"
    if ($observed.ContainsKey($key)) { throw "duplicate observation $key" }
    $observed[$key] = [ordered]@{
        requestedMicros = $parts[3]
        status = $parts[4]
        ordinal = $parts[5]
        pts = $parts[6]
        ptsMicros = $parts[7]
        keyFrame = $parts[8]
        pictureType = $parts[9]
        width = $parts[10]
        height = $parts[11]
        pdq = $parts[12]
        quality = $parts[13]
        phash = $parts[14]
        sobel = $parts[15]
    }
}
Assert-Equal $observed.Count ($fixtureNames.Count * 6) 'observation count'

$sarOutput = @(& $SarReference $SarFixture 2>&1 | ForEach-Object { "$_" })
if ($LASTEXITCODE -ne 0) {
    $sarOutput | ForEach-Object { Write-Host $_ }
    throw "independent SAR reference failed with exit code $LASTEXITCODE"
}
$sarProvenance = @($sarOutput | Where-Object { $_.StartsWith('SAR_PROVENANCE|') })
$sarParams = @($sarOutput | Where-Object { $_.StartsWith('SAR_PARAMS|') })
Assert-Equal $sarProvenance.Count 1 'SAR provenance line count'
Assert-Equal $sarParams.Count 1 'SAR parameter line count'
Assert-Equal $sarParams[0] 'SAR_PARAMS|scale=max_side:512;filter=bicubic;pix_fmt=gray8;sar=display-before-scale;seek=decode-from-zero' 'SAR fixed parameters'
$fixtureSha256 = (Get-FileHash -LiteralPath $SarFixture -Algorithm SHA256).Hash.ToLowerInvariant()
if ($sarProvenance[0] -notmatch (';fixture_sha256=' + [regex]::Escape($fixtureSha256) + '$')) {
    throw 'SAR provenance fixture SHA-256 does not match the independent input file'
}
$sarObserved = @{}
foreach ($line in $sarOutput) {
    if (-not $line.StartsWith('SAR_REFERENCE|')) { continue }
    $parts = $line.Split('|')
    if ($parts.Count -ne 14) { throw "invalid SAR_REFERENCE field count: $line" }
    $sarObserved[$parts[1]] = [ordered]@{
        requestedMicros = $parts[2]
        ordinal = $parts[3]
        pts = $parts[4]
        ptsMicros = $parts[5]
        keyFrame = $parts[6]
        pictureType = $parts[7]
        width = $parts[8]
        height = $parts[9]
        pdq = $parts[10]
        quality = $parts[11]
        phash = $parts[12]
        sobel = $parts[13]
    }
}
Assert-Equal $sarObserved.Count 6 'SAR independent reference frame count'

$document = Get-Content -Raw -LiteralPath $Golden | ConvertFrom-Json
$videoFixtures = @($document.fixtures | Where-Object {
    $_.path -like 'videos/*' -and
    $fixtureNames -contains ([IO.Path]::GetFileName($_.path))
})
Assert-Equal $videoFixtures.Count $fixtureNames.Count 'selected golden fixture count'

$delta = @($document.approvedSemanticDeltas | Where-Object {
    $_.id -eq 'sar-corrected-feature-geometry' -and
    $_.fixturePath -eq 'videos/h264-sar-4x3.mp4' -and
    $_.approval -eq 'approved-design-delta'
})
Assert-Equal $delta.Count 1 'approved SAR delta count'
Assert-Equal $delta[0].legacyDisplayWidth 512 'SAR legacy width'
Assert-Equal $delta[0].legacyDisplayHeight 341 'SAR legacy height'
Assert-Equal $delta[0].futureDisplayWidth 512 'SAR future width'
Assert-Equal $delta[0].futureDisplayHeight 256 'SAR future height'

foreach ($name in $fixtureNames) {
    $fixture = $videoFixtures | Where-Object path -eq "videos/$name"
    Assert-Equal @($fixture).Count 1 "$name golden entry count"
    Assert-Equal @($fixture.video.frames).Count 6 "$name golden frame count"
    foreach ($frame in $fixture.video.frames) {
        $label = "$name slot $($frame.sampleIndex)"
        $actual = $observed["$name#$($frame.sampleIndex)"]
        if ($null -eq $actual) { throw "$label observation missing" }
        Assert-Equal $actual.requestedMicros $frame.requestedMicros "$label requestedMicros"
        if ($null -ne $frame.selectedIdentity) {
            Assert-Equal $actual.status 0 "$label status"
            Assert-Equal $actual.ordinal $frame.selectedIdentity.sourceDecodeOrdinal "$label sourceDecodeOrdinal"
            Assert-Equal $actual.pts $frame.selectedIdentity.pts "$label pts"
            Assert-Equal $actual.ptsMicros $frame.selectedIdentity.ptsTimeMicros "$label ptsTimeMicros"
            $expectedKey = if ($frame.selectedIdentity.keyFrame) { 1 } else { 0 }
            Assert-Equal $actual.keyFrame $expectedKey "$label keyFrame"
            Assert-Equal $actual.pictureType $frame.selectedIdentity.pictureType "$label pictureType"

            $isSarDelta = $name -eq 'h264-sar-4x3.mp4'
            $expectedWidth = if ($isSarDelta) { $delta[0].futureDisplayWidth } else { $frame.displayWidth }
            $expectedHeight = if ($isSarDelta) { $delta[0].futureDisplayHeight } else { $frame.displayHeight }
            Assert-Equal $actual.width $expectedWidth "$label displayWidth"
            Assert-Equal $actual.height $expectedHeight "$label displayHeight"

            if ($frame.outputFrameSHA256 -notmatch '^[0-9a-f]{64}$') {
                throw "$label frozen outputFrameSHA256 is malformed"
            }
            if (-not $isSarDelta) {
                Assert-Equal $actual.pdq $frame.pdqHex "$label pdqHex"
                Assert-Equal $actual.quality $frame.quality "$label quality"
                Assert-Equal $actual.phash (($frame.pHashPartsHex | ForEach-Object { $_.ToLowerInvariant() }) -join ',') "$label pHashPartsHex"
                Assert-Equal $actual.sobel (($frame.sobelFloatBitsHex | ForEach-Object { $_.ToLowerInvariant() }) -join ',') "$label sobelFloatBitsHex"
            } else {
                $reference = $sarObserved[[string]$frame.sampleIndex]
                if ($null -eq $reference) { throw "$label independent SAR reference missing" }
                foreach ($field in @('requestedMicros', 'ordinal', 'pts', 'ptsMicros',
                                      'keyFrame', 'pictureType', 'width', 'height',
                                      'pdq', 'quality', 'phash', 'sobel')) {
                    Assert-Equal $actual[$field] $reference[$field] "$label independent SAR $field"
                }
            }
        } else {
            if ([int]$actual.status -eq 0) {
                throw "$label expected frozen legacy error but succeeded"
            }
        }
    }
}

if ($fieldFailures.Count -ne 0) {
    $summary = ($fieldFailures | Select-Object -First 20) -join [Environment]::NewLine
    throw "$($fieldFailures.Count) legacy golden field mismatch(es):`n$summary"
}

Write-Host "legacy golden field oracle passed: $($observed.Count) frames; SAR approved delta independently referenced"
