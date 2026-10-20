param(
    [switch]$SkipDockerBuilds
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location -LiteralPath $root
$checks = New-Object System.Collections.Generic.List[object]

function Report([string]$Name, [string]$Status, [string]$Detail) {
    $checks.Add([pscustomobject]@{ Check = $Name; Status = $Status; Detail = $Detail })
}
function Find-Tool([string]$Name, [string]$Fallback) {
    $found = Get-Command $Name -ErrorAction SilentlyContinue
    if ($found) { return $found.Source }
    if ($Fallback -and (Test-Path -LiteralPath $Fallback -PathType Leaf)) { return $Fallback }
    return $null
}
function Run-Check([string]$Name, [scriptblock]$Action) {
    try {
        & $Action
        Report $Name 'PASS' 'Local check completed.'
    } catch {
        Report $Name 'FAIL' 'Local check failed; inspect the corresponding tool directly.'
    }
}
function Require-Exit([string]$Tool, [string[]]$Arguments) {
    # Windows PowerShell turns native stderr into error records when the
    # caller uses Stop. Docker writes normal progress there, so use exit code.
    $previousPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        & $Tool @Arguments *> $null
        $code = $LASTEXITCODE
    } finally { $ErrorActionPreference = $previousPreference }
    if ($code -ne 0) { throw 'tool returned a nonzero exit status' }
}

$go = Find-Tool 'go' (Join-Path $root '.tools/go/bin/go.exe')
$buf = Find-Tool 'buf' (Join-Path $root '.tools/buf.exe')
$terraform = Find-Tool 'terraform' (Join-Path $root '.tools/terraform/terraform.exe')
$docker = Find-Tool 'docker' ''
$git = Find-Tool 'git' ''

foreach ($tool in @(@('Go toolchain', $go), @('Docker CLI', $docker), @('Terraform CLI', $terraform), @('Buf CLI', $buf), @('Git CLI', $git))) {
    if ($tool[1]) { Report $tool[0] 'PASS' 'Executable found locally.' }
    else { Report $tool[0] 'NOT CONFIGURED' 'Executable not found.' }
}

if ($git) {
    Run-Check 'Git working tree' {
        $changes = & $git status --porcelain --untracked-files=normal
        if ($LASTEXITCODE -ne 0 -or $changes) { throw 'working tree is not clean' }
    }
} else { Report 'Git working tree' 'NOT TESTED' 'Git CLI unavailable.' }

if ($go) {
    Run-Check 'Go formatting' {
        $files = @(Get-ChildItem -Path @('cmd', 'internal', 'tests', 'gen', 'db') -Recurse -Filter '*.go' -File | ForEach-Object FullName)
        if (@(& gofmt -l $files).Count -gt 0) { throw 'Go files need formatting' }
    }
    Run-Check 'Go build' { Require-Exit $go @('build', './...') }
    Run-Check 'Go unit and available integration tests' { Require-Exit $go @('test', '-count=1', './...') }
    Run-Check 'Go vet' { Require-Exit $go @('vet', './...') }
} else {
    foreach ($name in @('Go formatting', 'Go build', 'Go unit and available integration tests', 'Go vet')) {
        Report $name 'NOT TESTED' 'Go toolchain unavailable.'
    }
}

$testDB = $env:TEST_DATABASE_URL
$testTemporal = $env:TEST_TEMPORAL_ADDRESS
if ($go -and $testDB -and $testTemporal) {
    try {
        $uri = [uri]$testDB
        $hostName = $uri.DnsSafeHost
        $dbName = $uri.AbsolutePath.TrimStart('/')
        if ($uri.Scheme -notin @('postgres', 'postgresql') -or
            $hostName -notin @('127.0.0.1', 'localhost', '::1') -or
            -not $dbName.StartsWith('payflow_loadtest_') -or
            $uri.Port -le 0 -or $uri.Port -eq 5432 -or
            $testTemporal -notmatch '^(127\.0\.0\.1|localhost|\[::1\]):\d+$') {
            throw 'integration targets are not disposable loopback services'
        }
        Run-Check 'Disposable PostgreSQL and Temporal integration tests' { Require-Exit $go @('test', '-count=1', './tests') }
    } catch {
        Report 'Disposable PostgreSQL and Temporal integration tests' 'NOT CONFIGURED' 'Set disposable loopback TEST_DATABASE_URL and TEST_TEMPORAL_ADDRESS.'
    }
} else {
    Report 'Disposable PostgreSQL and Temporal integration tests' 'NOT CONFIGURED' 'Set disposable loopback TEST_DATABASE_URL and TEST_TEMPORAL_ADDRESS.'
}

if ($buf) {
    Run-Check 'Protobuf lint' { Require-Exit $buf @('lint') }
    $protocGo = Find-Tool 'protoc-gen-go' (Join-Path $root '.tools/gopath/bin/protoc-gen-go.exe')
    $protocConnect = Find-Tool 'protoc-gen-connect-go' (Join-Path $root '.tools/gopath/bin/protoc-gen-connect-go.exe')
    if ($protocGo -and $protocConnect) {
        $env:PATH = "$(Split-Path -Parent $protocGo);$(Split-Path -Parent $protocConnect);$env:PATH"
        $scratch = Join-Path $root ('.tools/preflight-generated-' + [guid]::NewGuid().ToString('N'))
        Run-Check 'Protobuf generated code consistency' {
            New-Item -ItemType Directory -Path $scratch -Force | Out-Null
            Require-Exit $buf @('generate', '--output', $scratch)
            $fresh = Join-Path $scratch 'gen/go'
            $committed = Join-Path $root 'gen/go'
            $generated = @(Get-ChildItem -LiteralPath $fresh -Recurse -File)
            $tracked = @(Get-ChildItem -LiteralPath $committed -Recurse -File)
            if ($generated.Count -eq 0 -or $generated.Count -ne $tracked.Count) { throw 'generated file set differs' }
            foreach ($file in $generated) {
                $relative = $file.FullName.Substring($fresh.Length).TrimStart('\', '/')
                $expected = Join-Path $committed $relative
                if (-not (Test-Path -LiteralPath $expected -PathType Leaf) -or
                    (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash -ne
                    (Get-FileHash -LiteralPath $expected -Algorithm SHA256).Hash) { throw 'generated code differs' }
            }
        }
        # Remove only the uniquely created directory beneath this repository.
        $resolvedTools = [IO.Path]::GetFullPath((Join-Path $root '.tools'))
        $resolvedScratch = [IO.Path]::GetFullPath($scratch)
        if ($resolvedScratch.StartsWith($resolvedTools + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase) -and
            (Test-Path -LiteralPath $scratch)) { Remove-Item -LiteralPath $scratch -Recurse -Force }
    } else { Report 'Protobuf generated code consistency' 'NOT CONFIGURED' 'Install both versioned protoc Go plugins.' }
} else {
    Report 'Protobuf lint' 'NOT TESTED' 'Buf CLI unavailable.'
    Report 'Protobuf generated code consistency' 'NOT TESTED' 'Buf CLI unavailable.'
}

if ($terraform) {
    Run-Check 'Terraform formatting' { Require-Exit $terraform @('-chdir=infrastructure/terraform', 'fmt', '-check', '-recursive') }
    $providerDir = Join-Path $root 'infrastructure/terraform/.terraform/providers'
    Run-Check 'Terraform local initialization' {
        $arguments = @('-chdir=infrastructure/terraform', 'init', '-backend=false', '-input=false', '-lockfile=readonly')
        if (Test-Path -LiteralPath $providerDir -PathType Container) { $arguments += @('-plugin-dir', $providerDir) }
        Require-Exit $terraform $arguments
    }
    Run-Check 'Terraform syntax and provider validation' { Require-Exit $terraform @('-chdir=infrastructure/terraform', 'validate') }
} else {
    foreach ($name in @('Terraform formatting', 'Terraform local initialization', 'Terraform syntax and provider validation')) {
        Report $name 'NOT TESTED' 'Terraform CLI unavailable.'
    }
}

if ($docker) {
    try {
        Require-Exit $docker @('info', '--format', '{{.ServerVersion}}')
        Report 'Docker daemon' 'PASS' 'Local daemon is available.'
        if ($SkipDockerBuilds) {
            Report 'Four container build targets' 'NOT TESTED' 'Skipped by explicit flag.'
        } else {
            Run-Check 'Four container build targets' {
                foreach ($target in @('api', 'worker', 'migrate', 'admin')) {
                    Require-Exit $docker @('build', '--target', $target, '-t', "payflow-${target}:preflight", '.')
                }
            }
        }
    } catch {
        Report 'Docker daemon' 'NOT CONFIGURED' 'Start Docker Desktop; no containers were changed.'
        Report 'Four container build targets' 'NOT TESTED' 'Docker daemon unavailable.'
    }
} else {
    Report 'Docker daemon' 'NOT CONFIGURED' 'Docker CLI unavailable.'
    Report 'Four container build targets' 'NOT TESTED' 'Docker CLI unavailable.'
}

$tfvars = Join-Path $root 'infrastructure/terraform/terraform.tfvars'
if (Test-Path -LiteralPath $tfvars -PathType Leaf) {
    Report 'Deployment variable file' 'PASS' 'Local tfvars file exists; values and AWS resources were not verified.'
} else { Report 'Deployment variable file' 'NOT CONFIGURED' 'Create a reviewed ignored terraform.tfvars for a future authorized deployment.' }
Report 'AWS identity, route, certificate, state, secrets, roles, Temporal Cloud' 'NOT TESTED' 'Requires separate authorized external verification.'
Report 'AWS deployment readiness' 'NOT TESTED' 'Local preflight never calls AWS or proves a deployment is ready.'

$checks | Format-Table -AutoSize | Out-String -Width 180 | Write-Output
if (@($checks | Where-Object { $_.Status -eq 'FAIL' }).Count -gt 0) { exit 1 }
