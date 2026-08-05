param(
    [switch]$VerifyEmbedded
)

$ErrorActionPreference = "Stop"

function Assert-DirectChildPath {
    param(
        [string]$Parent,
        [string]$Candidate,
        [string]$Label
    )

    $parentFull = (Resolve-Path -LiteralPath $Parent).Path.TrimEnd('\')
    $candidateFull = [System.IO.Path]::GetFullPath($Candidate).TrimEnd('\')
    $candidateParent = [System.IO.Path]::GetDirectoryName($candidateFull).TrimEnd('\')
    if (-not [string]::Equals(
        $candidateParent,
        $parentFull,
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        throw "Refusing to use $Label outside its expected direct parent: $candidateFull"
    }
    return $candidateFull
}

function Assert-PathInsideDirectory {
    param(
        [string]$Parent,
        [string]$Candidate,
        [string]$Label
    )

    $parentFull = [System.IO.Path]::GetFullPath($Parent).TrimEnd('\')
    $candidateFull = [System.IO.Path]::GetFullPath($Candidate)
    $prefix = $parentFull + [System.IO.Path]::DirectorySeparatorChar
    if (-not $candidateFull.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to read $Label outside ${parentFull}: $candidateFull"
    }
    return $candidateFull
}

function Invoke-Checked {
    param(
        [string]$Description,
        [scriptblock]$Command
    )

    & $Command
    if ($LASTEXITCODE -ne 0) {
        throw "$Description failed with exit code $LASTEXITCODE"
    }
}

function Resolve-Application {
    param(
        [string]$Name,
        [string]$Label
    )

    $commands = @(Get-Command $Name -CommandType Application -All -ErrorAction SilentlyContinue)
    $paths = [System.Collections.Generic.List[string]]::new()
    foreach ($command in $commands) {
        $candidate = $command.Source
        if ([string]::IsNullOrWhiteSpace($candidate)) {
            $candidate = $command.Path
        }
        if ([string]::IsNullOrWhiteSpace($candidate) -or
            -not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
            continue
        }
        $resolved = (Resolve-Path -LiteralPath $candidate).Path
        if (-not $paths.Contains($resolved)) {
            $null = $paths.Add($resolved)
        }
    }
    if ($paths.Count -eq 0) {
        throw "$Label executable was not found on PATH: $Name"
    }

    if ([System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT) {
        foreach ($path in $paths) {
            $extension = [System.IO.Path]::GetExtension($path)
            if ($extension -ieq ".cmd" -or $extension -ieq ".exe") {
                return [string]$path
            }
        }
    }
    return [string]$paths[0]
}

function Get-StaticResourceReferences {
    param([string]$HTML)

    $tagPattern = '(?is)<\s*(script|link)\b[^>]*>'
    $attributePattern = '(?is)\b([a-z][a-z0-9:_-]*)\s*=\s*(?:"([^"]*)"|''([^'']*)''|([^\s"''=<>]+))'
    foreach ($tag in [regex]::Matches($HTML, $tagPattern)) {
        $attributes = @{}
        foreach ($attribute in [regex]::Matches($tag.Value, $attributePattern)) {
            if ($attribute.Groups[2].Success) {
                $value = $attribute.Groups[2].Value
            } elseif ($attribute.Groups[3].Success) {
                $value = $attribute.Groups[3].Value
            } else {
                $value = $attribute.Groups[4].Value
            }
            $attributes[$attribute.Groups[1].Value.ToLowerInvariant()] = $value
        }

        $tagName = $tag.Groups[1].Value.ToLowerInvariant()
        if ($tagName -eq "script" -and $attributes.ContainsKey("src")) {
            [pscustomobject]@{
                Kind = "script"
                URL  = [string]$attributes["src"]
            }
            continue
        }
        if ($tagName -ne "link" -or -not $attributes.ContainsKey("rel")) {
            continue
        }
        $isStylesheet = $false
        foreach ($token in ([string]$attributes["rel"] -split '\s+')) {
            if ($token -ieq "stylesheet") {
                $isStylesheet = $true
                break
            }
        }
        if ($isStylesheet -and $attributes.ContainsKey("href")) {
            [pscustomobject]@{
                Kind = "stylesheet"
                URL  = [string]$attributes["href"]
            }
        }
    }
}

function Test-HasRemoteScriptOrStylesheet {
    param([string]$HTML)

    foreach ($reference in @(Get-StaticResourceReferences -HTML $HTML)) {
        if ($reference.URL.Trim() -match '(?i)^(?:https?:)?//') {
            return $true
        }
    }
    return $false
}

function Test-EmbeddedStylesheet {
    param(
        [string]$Stylesheet,
        [string]$Directory,
        [string]$AssetsRoot,
        [string]$Label
    )

    $css = Get-Content -LiteralPath $Stylesheet -Raw
    if ($css -match '(?i)@import\b') {
        throw "$Label contains a forbidden CSS @import"
    }
    $pattern = '(?is)url\(\s*(?:"([^"]*)"|''([^'']*)''|([^''")]*))\s*\)'
    foreach ($match in [regex]::Matches($css, $pattern)) {
        if ($match.Groups[1].Success) {
            $url = $match.Groups[1].Value.Trim()
        } elseif ($match.Groups[2].Success) {
            $url = $match.Groups[2].Value.Trim()
        } else {
            $url = $match.Groups[3].Value.Trim()
        }
        if ($url.StartsWith("data:", [System.StringComparison]::OrdinalIgnoreCase) -or
            $url.StartsWith("#", [System.StringComparison]::Ordinal)) {
            continue
        }
        if (-not $url.StartsWith("/assets/", [System.StringComparison]::Ordinal) -or
            $url.Contains('\') -or
            $url -match '(?i)^(?:https?:)?//') {
            throw "$Label contains a remote or invalid CSS asset URL: $url"
        }
        $asset = Assert-PathInsideDirectory -Parent $AssetsRoot -Candidate (
            Join-Path $Directory ($url.TrimStart('/'))
        ) -Label "CSS asset URL $url"
        if (-not (Test-Path -LiteralPath $asset -PathType Leaf)) {
            throw "$Label references a missing CSS asset: $url"
        }
        if ((Get-Item -LiteralPath $asset).Length -eq 0) {
            throw "$Label references an empty CSS asset: $url"
        }
    }
}

function Test-EmbeddedWebDirectory {
    param(
        [string]$Directory,
        [string]$Label
    )

    $directoryFull = (Resolve-Path -LiteralPath $Directory).Path.TrimEnd('\')
    $pages = @{}
    foreach ($name in @("index.html", "groups.html", "legacy.html", "legacy-groups.html")) {
        $page = Join-Path $directoryFull $name
        if (-not (Test-Path -LiteralPath $page -PathType Leaf)) {
            throw "$Label is missing required page: $name"
        }
        $html = Get-Content -LiteralPath $page -Raw
        if (Test-HasRemoteScriptOrStylesheet -HTML $html) {
            throw "$Label page $name contains a remote script or stylesheet"
        }
        $pages[$name] = $html
    }

    $assetsRoot = Join-Path $directoryFull "assets"
    if (-not (Test-Path -LiteralPath $assetsRoot -PathType Container)) {
        throw "$Label is missing assets directory"
    }

    foreach ($entry in @("index.html", "groups.html")) {
        $html = $pages[$entry]
        if ($html -notmatch 'id\s*=\s*["'']root["'']') {
            throw "$Label React entry $entry has no root element"
        }
        if ($html -match '(?i)https?://') {
            throw "$Label React entry $entry contains an HTTP(S) URL"
        }
        $foundScript = $false
        $foundStylesheet = $false
        foreach ($reference in @(Get-StaticResourceReferences -HTML $html)) {
            $url = $reference.URL.Trim()
            if (-not $url.StartsWith("/assets/", [System.StringComparison]::Ordinal)) {
                throw "$Label React entry $entry has a non-root-relative $($reference.Kind) URL: $url"
            }
            if ($reference.Kind -eq "script") {
                if (-not $url.EndsWith(".js", [System.StringComparison]::OrdinalIgnoreCase)) {
                    throw "$Label React entry $entry script URL is not JavaScript: $url"
                }
                $foundScript = $true
            } elseif ($reference.Kind -eq "stylesheet") {
                if (-not $url.EndsWith(".css", [System.StringComparison]::OrdinalIgnoreCase)) {
                    throw "$Label React entry $entry stylesheet URL is not CSS: $url"
                }
                $foundStylesheet = $true
            } else {
                throw "$Label React entry $entry has unknown asset kind: $($reference.Kind)"
            }
            $asset = Assert-PathInsideDirectory -Parent $assetsRoot -Candidate (
                Join-Path $directoryFull ($url.TrimStart('/'))
            ) -Label "asset URL $url"
            if (-not (Test-Path -LiteralPath $asset -PathType Leaf)) {
                throw "$Label React entry $entry references missing asset: $url"
            }
            if ((Get-Item -LiteralPath $asset).Length -eq 0) {
                throw "$Label React entry $entry references an empty asset: $url"
            }
            if ($reference.Kind -eq "stylesheet") {
                Test-EmbeddedStylesheet `
                    -Stylesheet $asset `
                    -Directory $directoryFull `
                    -AssetsRoot $assetsRoot `
                    -Label "$Label stylesheet $url"
            }
        }
        if (-not $foundScript -or -not $foundStylesheet) {
            throw "$Label React entry $entry must reference at least one JavaScript and one stylesheet asset"
        }
    }
}

function Write-RelativeAssetManifest {
    param([string]$Directory)

    $directoryFull = (Resolve-Path -LiteralPath $Directory).Path.TrimEnd('\')
    $assetsRoot = Join-Path $directoryFull "assets"
    $assets = @(Get-ChildItem -LiteralPath $assetsRoot -File -Recurse | Sort-Object FullName)
    if ($assets.Count -eq 0) {
        throw "No embedded assets exist in $assetsRoot"
    }
    Write-Host "Embedded asset manifest:"
    foreach ($asset in $assets) {
        $relative = $asset.FullName.Substring($directoryFull.Length).TrimStart('\') -replace '\\', '/'
        $hash = (Get-FileHash -LiteralPath $asset.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        Write-Host ("{0} {1} {2}" -f $relative, $asset.Length, $hash)
    }
}

$repo = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$webui = (Resolve-Path -LiteralPath (Join-Path $repo "webui")).Path
$guiDirectory = (Resolve-Path -LiteralPath (Join-Path $repo "internal\gui")).Path
$staging = Assert-DirectChildPath -Parent $webui -Candidate (Join-Path $webui "dist") -Label "web staging directory"
$target = Assert-DirectChildPath -Parent $guiDirectory -Candidate (Join-Path $guiDirectory "web") -Label "embedded web directory"
$backup = Assert-DirectChildPath -Parent $guiDirectory -Candidate (Join-Path $guiDirectory "web.backup") -Label "embedded web backup directory"

if ($VerifyEmbedded) {
    Test-EmbeddedWebDirectory -Directory $target -Label "embedded web directory"
    Write-RelativeAssetManifest -Directory $target
    Write-Host "Embedded web directory verified without rebuilding."
    return
}

$node = Resolve-Application -Name "node" -Label "node"
$npm = Resolve-Application -Name "npm" -Label "npm"

Write-Host "node version: $(& $node --version)"
if ($LASTEXITCODE -ne 0) { throw "node --version failed" }
Write-Host "npm version: $(& $npm --version)"
if ($LASTEXITCODE -ne 0) { throw "npm --version failed" }

$lockfile = Join-Path $webui "package-lock.json"
if (Test-Path -LiteralPath $lockfile -PathType Leaf) {
    Invoke-Checked -Description "npm ci" -Command { & $npm --prefix $webui ci }
}
Invoke-Checked -Description "frontend tests" -Command { & $npm --prefix $webui test }
Invoke-Checked -Description "frontend lint" -Command { & $npm --prefix $webui run lint }

if (Test-Path -LiteralPath $staging) {
    Remove-Item -LiteralPath $staging -Recurse -Force
}
Invoke-Checked -Description "frontend build" -Command {
    & $npm --prefix $webui run build -- --outDir $staging
}
Test-EmbeddedWebDirectory -Directory $staging -Label "staged web directory"

if (Test-Path -LiteralPath $backup) {
    throw "Refusing to overwrite the pre-existing recovery directory: $backup"
}
if (-not (Test-Path -LiteralPath $target -PathType Container)) {
    throw "Embedded web directory does not exist, so no recoverable swap can be performed: $target"
}

$backupCreated = $false
try {
    Move-Item -LiteralPath $target -Destination $backup -ErrorAction Stop
    $backupCreated = $true
    Move-Item -LiteralPath $staging -Destination $target -ErrorAction Stop
    Test-EmbeddedWebDirectory -Directory $target -Label "embedded web directory"
    Write-RelativeAssetManifest -Directory $target
}
catch {
    $swapError = $_
    try {
        if ($backupCreated) {
            if (Test-Path -LiteralPath $target) {
                Remove-Item -LiteralPath $target -Recurse -Force -ErrorAction Stop
            }
            if (-not (Test-Path -LiteralPath $backup -PathType Container)) {
                throw "Expected recovery directory is missing: $backup"
            }
            Move-Item -LiteralPath $backup -Destination $target -ErrorAction Stop
            $backupCreated = $false
        } elseif (-not (Test-Path -LiteralPath $target) -and
            (Test-Path -LiteralPath $backup -PathType Container)) {
            Move-Item -LiteralPath $backup -Destination $target -ErrorAction Stop
        }
    }
    catch {
        throw "Embedded web swap failed: $swapError. Restore also failed: $_"
    }
    throw "Embedded web swap failed and the previous directory was restored: $swapError"
}

if ($backupCreated -and (Test-Path -LiteralPath $backup)) {
    Remove-Item -LiteralPath $backup -Recurse -Force
}
Write-Host "Embedded web build completed."
