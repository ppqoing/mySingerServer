param(
    [string]$StageDir = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repo = Split-Path -Parent $PSScriptRoot
$failures = [Collections.Generic.List[string]]::new()

function Add-GateFailure {
    param([string]$Code)
    $failures.Add($Code)
}

function Assert-True {
    param(
        [bool]$Condition,
        [string]$Code
    )
    if (-not $Condition) { Add-GateFailure $Code }
}

function Test-ManagerStartAllowsMissingConfig {
    param([string]$Text)
    $tokens = $null
    $errors = $null
    $ast = [Management.Automation.Language.Parser]::ParseInput(
        $Text, [ref]$tokens, [ref]$errors)
    if (@($errors).Count -gt 0) { return $false }
    $throws = @($ast.FindAll({
        param($node)
        $node -is [Management.Automation.Language.ThrowStatementAst]
    }, $true))
    return $throws.Count -eq 0
}

function Get-ParsedScript {
    param([string]$Path)
    $tokens = $null
    $errors = $null
    $ast = [Management.Automation.Language.Parser]::ParseFile(
        $Path, [ref]$tokens, [ref]$errors)
    if (@($errors).Count -gt 0) {
        Add-GateFailure ("POWERSHELL_PARSE_FAILED file={0}" -f $Path)
    }
    return $ast
}

function Get-CommandTexts {
    param([Management.Automation.Language.Ast]$Ast)
    return @(
        $Ast.FindAll({
            param($node)
            $node -is [Management.Automation.Language.CommandAst]
        }, $true) | ForEach-Object { $_.Extent.Text }
    )
}

$required = @(
    "scripts\build-nodetray.ps1",
    "scripts\package-manager-release.ps1",
    "third_party\webview2\manifest.schema.json",
    "third_party\webview2\manifest.json",
    "third_party\webview2\NOTICE.md",
    "third_party\webview2\MicrosoftEdgeWebview2Setup.exe",
    "third_party\everything\Everything.exe",
    "third_party\everything\LICENSE.txt",
    "third_party\everything\NOTICE.md",
    "third_party\everything\manifest.json",
    "nodetray\frontend\package-lock.json",
    "nodetray\build\windows\nodetray.manifest",
    "deploy\gui.default.json",
    "deploy\Start-Manager.ps1",
    "deploy\README-管理端部署.md"
)
foreach ($relative in $required) {
    Assert-True (Test-Path -LiteralPath (Join-Path $repo $relative) -PathType Leaf) `
        ("REQUIRED_FILE_MISSING path={0}" -f $relative)
}

$guiDefaultPath = Join-Path $repo 'deploy\gui.default.json'
if (Test-Path -LiteralPath $guiDefaultPath -PathType Leaf) {
    try {
        $guiDefault = Get-Content -Raw -LiteralPath $guiDefaultPath | ConvertFrom-Json
        Assert-True ([string]$guiDefault.pg_dsn -ceq '') 'GUI_DEFAULT_DSN_MUST_BE_EMPTY'
        Assert-True ([string]$guiDefault.listen_addr -ceq '127.0.0.1:18081') `
            'GUI_DEFAULT_LISTEN_ADDR_NOT_DEDICATED_LOOPBACK'
        $guiAgents = @($guiDefault.agents)
        Assert-True ($guiAgents.Count -eq 1 -and [string]$guiAgents[0].addr -ceq '127.0.0.1:9101') `
            'GUI_DEFAULT_AGENT_NOT_LOOPBACK_ONLY'
    } catch {
        Add-GateFailure 'GUI_DEFAULT_INVALID'
    }
}

$managerStartPath = Join-Path $repo 'deploy\Start-Manager.ps1'
if (Test-Path -LiteralPath $managerStartPath -PathType Leaf) {
    $managerStartText = Get-Content -Raw -LiteralPath $managerStartPath
    Assert-True ($managerStartText -match 'Join-Path \$root ''gui.exe''') 'MANAGER_START_GUI_EXE_MISSING'
    Assert-True ($managerStartText -match 'Join-Path \$root ''gui.json''') 'MANAGER_START_GUI_CONFIG_MISSING'
    Assert-True ($managerStartText -match `
        '& \(Join-Path \$root ''gui\.exe''\) -config \(Join-Path \$root ''gui\.json''\) @args') `
        'MANAGER_START_CONFIG_ARGUMENT_MISSING'
    Assert-True (Test-ManagerStartAllowsMissingConfig -Text $managerStartText) `
        'MANAGER_START_REJECTS_MISSING_CONFIG'
    $inlineThrowMutation = $managerStartText + `
        "`nif (-not (Test-Path -LiteralPath 'gui.json')) { throw 'missing config' }"
    Assert-True (-not (Test-ManagerStartAllowsMissingConfig -Text $inlineThrowMutation)) `
        'MANAGER_START_INLINE_THROW_MUTATION_ACCEPTED'
}

$buildNodeTray = Join-Path $repo "scripts\build-nodetray.ps1"
if (Test-Path -LiteralPath $buildNodeTray -PathType Leaf) {
    $ast = Get-ParsedScript $buildNodeTray
    $commands = Get-CommandTexts $ast
    $wailsCommands = @($commands | Where-Object {
        $_ -match 'github\.com/wailsapp/wails/v2/cmd/wails@v2\.12\.0' -and
        $_ -match '(?i)\bbuild\b'
    })
    Assert-True ($wailsCommands.Count -eq 1) "WAILS_BUILD_COMMAND_NOT_EXACTLY_ONE"
    if ($wailsCommands.Count -eq 1) {
        $wails = $wailsCommands[0]
        Assert-True ($wails -match '(?i)-webview2\s+embed') "WAILS_WEBVIEW2_EMBED_MISSING"
        Assert-True ($wails -notmatch '(?i)(^|\s)-(clean|devtools|debug)(\s|$)') `
            "WAILS_PRODUCTION_FORBIDDEN_FLAG"
        Assert-True ($wails -match '(?i)(^|\s)-trimpath(\s|$)') "WAILS_TRIMPATH_MISSING"
        Assert-True ($wails -match '(?i)(^|\s)-m(\s|$)') "WAILS_SKIP_MOD_TIDY_MISSING"
        Assert-True ($wails -match '(?i)(^|\s)-nosyncgomod(\s|$)') `
            "WAILS_NO_SYNC_GOMOD_MISSING"
        Assert-True ($wails -match '(?i)(^|\s)-s(\s|$)') `
            "WAILS_REBUILDS_FRONTEND_UNCONTROLLED"
        Assert-True ($wails -match '(?i)(^|\s)-skipbindings(\s|$)') `
            "WAILS_REGENERATES_BINDINGS_UNCONTROLLED"
    }
    $npmCI = @($commands | Where-Object { $_ -match '(?i)(^|\s)ci(\s|$)' })
    Assert-True ($npmCI.Count -ge 1) "NPM_CI_COMMAND_MISSING"

    . $buildNodeTray
    Assert-True ($null -ne (Get-Command Assert-WebView2Cache -ErrorAction SilentlyContinue)) `
        "WEBVIEW2_VALIDATOR_FUNCTION_MISSING"
    Assert-True ($null -ne (Get-Command Publish-FreshNodeTrayStage -ErrorAction SilentlyContinue)) `
        "ATOMIC_PUBLISH_FUNCTION_MISSING"
    Assert-True ($null -ne (Get-Command Get-LocalGoModuleProxy -ErrorAction SilentlyContinue)) `
        "LOCAL_GO_PROXY_FUNCTION_MISSING"
    if (Get-Command Get-LocalGoModuleProxy -ErrorAction SilentlyContinue) {
        $proxy = Get-LocalGoModuleProxy -GoModuleCache 'C:\cache\go\pkg\mod'
        Assert-True ($proxy -ceq 'file:///C:/cache/go/pkg/mod/cache/download') `
            "LOCAL_GO_PROXY_URI_INVALID"
    }
}

$lockPath = Join-Path $repo "nodetray\frontend\package-lock.json"
if (Test-Path -LiteralPath $lockPath -PathType Leaf) {
    try {
        $lock = Get-Content -Raw -LiteralPath $lockPath | ConvertFrom-Json -AsHashtable
        Assert-True ([int]$lock['lockfileVersion'] -ge 3) "NPM_LOCKFILE_VERSION_UNSUPPORTED"
        Assert-True ([string]$lock['packages']['node_modules/react']['version'] -eq "19.2.8") `
            "NPM_LOCK_REACT_VERSION_MISMATCH"
        Assert-True ([string]$lock['packages']['node_modules/vite']['version'] -eq "8.2.0") `
            "NPM_LOCK_VITE_VERSION_MISMATCH"
    } catch {
        Add-GateFailure "NPM_LOCKFILE_INVALID"
    }
}

$manifestSource = Join-Path $repo "nodetray\build\windows\nodetray.manifest"
if (Test-Path -LiteralPath $manifestSource -PathType Leaf) {
    try {
        [xml]$applicationManifest = Get-Content -Raw -LiteralPath $manifestSource
        $level = $applicationManifest.assembly.trustInfo.security.requestedPrivileges.requestedExecutionLevel.level
        Assert-True ([string]$level -ceq "asInvoker") "NODETRAY_MANIFEST_NOT_ASINVOKER"
        Assert-True ((Get-Content -Raw -LiteralPath $manifestSource) -notmatch 'requireAdministrator') `
            "NODETRAY_MANIFEST_REQUESTS_ADMIN"
    } catch {
        Add-GateFailure "NODETRAY_MANIFEST_INVALID_XML"
    }
}

$schemaPath = Join-Path $repo "third_party\webview2\manifest.schema.json"
$cacheManifestPath = Join-Path $repo "third_party\webview2\manifest.json"
$bootstrapperPath = Join-Path $repo "third_party\webview2\MicrosoftEdgeWebview2Setup.exe"
if (Test-Path -LiteralPath $schemaPath -PathType Leaf) {
    try {
        $schema = Get-Content -Raw -LiteralPath $schemaPath | ConvertFrom-Json
        Assert-True ([string]$schema.'$schema' -eq 'https://json-schema.org/draft/2020-12/schema') `
            "WEBVIEW2_SCHEMA_DRAFT_MISMATCH"
        foreach ($name in @('official_source_url','actual_cache_origin','sha256','size','acquired_utc','notice_path')) {
            Assert-True ($null -ne $schema.properties.$name) `
                ("WEBVIEW2_SCHEMA_PROPERTY_MISSING name={0}" -f $name)
        }
        Assert-True (@($schema.required).Count -ge 10) "WEBVIEW2_SCHEMA_REQUIRED_SET_INCOMPLETE"
    } catch {
        Add-GateFailure "WEBVIEW2_SCHEMA_INVALID"
    }
}

if ((Test-Path -LiteralPath $buildNodeTray -PathType Leaf) -and
    (Test-Path -LiteralPath $cacheManifestPath -PathType Leaf) -and
    (Test-Path -LiteralPath $bootstrapperPath -PathType Leaf)) {
    try {
        $metadata = Assert-WebView2Cache -Bootstrapper $bootstrapperPath `
            -ManifestPath $cacheManifestPath -RepositoryRoot $repo
        Assert-True ([string]$metadata.official_source_url -eq `
            'https://go.microsoft.com/fwlink/p/?LinkId=2124703') `
            "WEBVIEW2_OFFICIAL_URL_MISMATCH"
        Assert-True ([string]$metadata.actual_cache_origin.kind -eq `
            'wails_module_embedded_asset') "WEBVIEW2_CACHE_ORIGIN_MISMATCH"
        Assert-True ([string]$metadata.actual_cache_origin.wails_version -eq 'v2.12.0') `
            "WEBVIEW2_CACHE_WAILS_VERSION_MISMATCH"
    } catch {
        Add-GateFailure ("WEBVIEW2_CACHE_VALIDATION_FAILED code={0}" -f $_.Exception.Message)
    }
}

$coreBuild = Join-Path $repo "scripts\build.ps1"
if (Test-Path -LiteralPath $coreBuild -PathType Leaf) {
    $coreAst = Get-ParsedScript $coreBuild
    $parameterNames = @($coreAst.ParamBlock.Parameters.Name.VariablePath.UserPath)
    Assert-True ($parameterNames -contains 'SkipNodeTrayBuild') `
        "FULL_BUILD_SKIP_NODETRAY_SWITCH_MISSING"
    $coreText = Get-Content -Raw -LiteralPath $coreBuild
    foreach ($name in @(
        'nodetray.exe',
        'MicrosoftEdgeWebview2Setup.exe',
        'Everything.exe',
        'everything-LICENSE.txt',
        'everything-NOTICE.md',
        'agent.example.json',
        'helper.example.json'
    )) {
        Assert-True ($coreText -match [regex]::Escape($name)) `
            ("FULL_BUILD_RELEASE_FILE_MISSING name={0}" -f $name)
    }
    Assert-True ($coreText -match 'build-nodetray\.ps1') "FULL_BUILD_NODETRAY_INTEGRATION_MISSING"

    $coreCommands = Get-CommandTexts $coreAst
    $workerBuildCommands = @($coreCommands | Where-Object {
        $_ -match '(?i)\bbuild\b' -and $_ -match '(?i)\./cmd/worker\b'
    })
    Assert-True ($workerBuildCommands.Count -eq 1) `
        "WORKER_BUILD_COMMAND_NOT_EXACTLY_ONE"
    if ($workerBuildCommands.Count -eq 1) {
        Assert-True ($workerBuildCommands[0] -match '(?i)(^|\s)-tags\s+nodynamic(\s|$)') `
            "WORKER_BUILD_NODYNAMIC_TAG_MISSING"
    }
    $agentBuildCommands = @($coreCommands | Where-Object {
        $_ -match '(?i)\bbuild\b' -and $_ -match '(?i)\./cmd/agent\b'
    })
    Assert-True ($agentBuildCommands.Count -eq 1) `
        "AGENT_BUILD_COMMAND_NOT_EXACTLY_ONE"
    if ($agentBuildCommands.Count -eq 1) {
        Assert-True ($agentBuildCommands[0] -match '(?i)(^|\s)-tags\s+nodynamic(\s|$)') `
            "AGENT_BUILD_NODYNAMIC_TAG_MISSING"
    }
}

if ((Get-Command Publish-FreshNodeTrayStage -ErrorAction SilentlyContinue)) {
    $testRoot = Join-Path (Join-Path $repo '.tmp') `
        ("mysingerserver-node-tray-supply-{0}" -f [Guid]::NewGuid().ToString('N'))
    try {
        New-Item -ItemType Directory -Path $testRoot | Out-Null

        $tampered = Join-Path $testRoot 'MicrosoftEdgeWebview2Setup.exe'
        Copy-Item -LiteralPath $bootstrapperPath -Destination $tampered
        (Get-Item -LiteralPath $tampered).IsReadOnly = $false
        $tamperStream = [IO.File]::Open($tampered, [IO.FileMode]::Open,
            [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
        try {
            $first = $tamperStream.ReadByte()
            $tamperStream.Position = 0
            $tamperStream.WriteByte([byte]($first -bxor 0x01))
        } finally {
            $tamperStream.Dispose()
        }
        $tamperRejected = $false
        try {
            Assert-WebView2Cache -Bootstrapper $tampered `
                -ManifestPath $cacheManifestPath -RepositoryRoot $repo | Out-Null
        } catch {
            $tamperRejected = $_.Exception.Message -match `
                '^WEBVIEW2_CACHE_SHA256_MISMATCH$'
        }
        Assert-True $tamperRejected "WEBVIEW2_TAMPER_NOT_REJECTED"

        $prepared = Join-Path $testRoot 'prepared'
        New-Item -ItemType Directory -Path $prepared | Out-Null
        Set-Content -LiteralPath (Join-Path $prepared 'nodetray.exe') -Value 'pe-placeholder'
        Set-Content -LiteralPath (Join-Path $prepared 'MicrosoftEdgeWebview2Setup.exe') -Value 'setup-placeholder'
        $published = Join-Path $testRoot 'published'
        Publish-FreshNodeTrayStage -PreparedStage $prepared -OutDir $published
        Assert-True (Test-Path -LiteralPath (Join-Path $published 'nodetray.exe') -PathType Leaf) `
            "ATOMIC_PUBLISH_DID_NOT_PUBLISH"
        Assert-True (-not (Test-Path -LiteralPath $prepared)) "ATOMIC_PUBLISH_LEFT_SOURCE"

        $prepared2 = Join-Path $testRoot 'prepared2'
        $existing = Join-Path $testRoot 'existing'
        New-Item -ItemType Directory -Path $prepared2 | Out-Null
        New-Item -ItemType Directory -Path $existing | Out-Null
        Set-Content -LiteralPath (Join-Path $prepared2 'nodetray.exe') -Value 'new'
        Set-Content -LiteralPath (Join-Path $prepared2 'MicrosoftEdgeWebview2Setup.exe') -Value 'new'
        Set-Content -LiteralPath (Join-Path $existing 'sentinel.txt') -Value 'keep'
        $rejected = $false
        try {
            Publish-FreshNodeTrayStage -PreparedStage $prepared2 -OutDir $existing
        } catch {
            $rejected = $_.Exception.Message -match 'NODETRAY_STAGE_EXISTS'
        }
        Assert-True $rejected "ATOMIC_PUBLISH_DID_NOT_REJECT_EXISTING_TARGET"
        Assert-True (Test-Path -LiteralPath (Join-Path $existing 'sentinel.txt') -PathType Leaf) `
            "ATOMIC_PUBLISH_CHANGED_EXISTING_TARGET"
    } finally {
        $resolvedTestRoot = [IO.Path]::GetFullPath($testRoot).TrimEnd('\')
        $allowedTestPrefix = [IO.Path]::GetFullPath(
            (Join-Path (Join-Path $repo '.tmp') 'mysingerserver-node-tray-supply-'))
        if ($resolvedTestRoot.StartsWith($allowedTestPrefix,
                [StringComparison]::OrdinalIgnoreCase) -and
            (Test-Path -LiteralPath $resolvedTestRoot)) {
            Remove-Item -LiteralPath $resolvedTestRoot -Recurse -Force
        }
    }
}

if (-not [string]::IsNullOrWhiteSpace($StageDir)) {
    $stage = [IO.Path]::GetFullPath($StageDir)
    foreach ($name in @('nodetray.exe','MicrosoftEdgeWebview2Setup.exe')) {
        Assert-True (Test-Path -LiteralPath (Join-Path $stage $name) -PathType Leaf) `
            ("NODETRAY_STAGE_FILE_MISSING name={0}" -f $name)
    }
    $forbidden = @(
        Get-ChildItem -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue |
            Where-Object {
                $_.Name -ieq 'node_modules' -or
                $_.Extension -in @('.map','.tsx','.ts') -or
                $_.Name -match '(?i)(credential|secret|password)'
            }
    )
    Assert-True ($forbidden.Count -eq 0) "NODETRAY_STAGE_FORBIDDEN_CONTENT"
}

if ($failures.Count -gt 0) {
    $failures | Sort-Object -Unique | ForEach-Object { Write-Error $_ -ErrorAction Continue }
    throw ("NODETRAY_SUPPLY_CHAIN_GATE_FAILED count={0}" -f `
        @($failures | Sort-Object -Unique).Count)
}

Write-Host "NODETRAY_SUPPLY_CHAIN_GATE_PASS"
