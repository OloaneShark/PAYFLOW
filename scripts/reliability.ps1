param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('api-restart', 'worker-restart', 'postgres-interruption', 'temporal-unavailable')]
    [string]$Scenario,
    [switch]$AllowLocalFailure
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (-not $AllowLocalFailure) { throw 'Explicit -AllowLocalFailure opt-in is required.' }
if ($env:PAYFLOW_ENV -eq 'production') { throw 'Production mode is refused.' }

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $repo
$compose = @('compose', '-f', 'compose.loadtest.yaml')
$dbName = 'payflow_loadtest_smoke'
$dbURL = "postgres://payflow:payflow@127.0.0.1:15432/$dbName`?sslmode=disable"
$binDir = New-Item -ItemType Directory -Force (Join-Path $repo '.tools/reliability')
$apiBinary = Join-Path $binDir.FullName 'api.exe'
$workerBinary = Join-Path $binDir.FullName 'worker.exe'
$loadBinary = Join-Path $binDir.FullName 'loadtest.exe'
$localGo = Join-Path $repo '.tools/go/bin/go.exe'
$goCommand = Get-Command go -ErrorAction SilentlyContinue
$goBinary = if (Test-Path -LiteralPath $localGo) { $localGo } elseif ($null -ne $goCommand) { $goCommand.Source } else { '' }
if (-not (Test-Path -LiteralPath $goBinary)) { throw 'Go is unavailable.' }
$api = $null
$worker = $null
$load = $null
$startedCompose = $false
$stoppedService = ''

function Stop-OwnedProcess($process, [string]$binary) {
    if ($null -eq $process) { return }
    $live = Get-Process -Id $process.Id -ErrorAction SilentlyContinue
    if ($null -ne $live -and $live.Path -eq $binary) {
        Stop-Process -Id $process.Id -Force
        $process.WaitForExit(10000) | Out-Null
    }
}

function New-APIProcess {
    Start-Process -FilePath $apiBinary -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput (Join-Path $binDir.FullName 'api.stdout') `
        -RedirectStandardError (Join-Path $binDir.FullName 'api.stderr')
}

function New-WorkerProcess {
    Start-Process -FilePath $workerBinary -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput (Join-Path $binDir.FullName 'worker.stdout') `
        -RedirectStandardError (Join-Path $binDir.FullName 'worker.stderr')
}

function Get-DispatchCount {
    $value = & docker @compose exec -T postgres psql -U payflow -d $dbName -Atqc 'SELECT count(*) FROM payment_workflow_dispatch'
    if ($LASTEXITCODE -ne 0) { throw 'Dedicated database dispatch count unavailable.' }
    return [int]($value | Select-Object -Last 1)
}

function Get-DatabaseEpoch {
    $value = & docker @compose exec -T postgres psql -U payflow -d $dbName -Atqc 'SELECT extract(epoch FROM clock_timestamp())'
    if ($LASTEXITCODE -ne 0) { throw 'Dedicated database clock unavailable.' }
    $epoch = [string]($value | Select-Object -Last 1)
    if ($epoch -notmatch '^\d+(\.\d+)?$') { throw 'Invalid dedicated database timestamp.' }
    return $epoch
}

function Get-PendingCount([string]$since) {
    if ($since -notmatch '^\d+(\.\d+)?$') { throw 'Invalid benchmark start timestamp.' }
    $sql = "SELECT count(*) FROM payments p JOIN payment_workflow_dispatch d ON d.payment_id=p.id WHERE d.requested_at >= to_timestamp($since) AND p.status NOT IN ('COMPLETED','FAILED')"
    $value = & docker @compose exec -T postgres psql -U payflow -d $dbName -Atqc $sql
    if ($LASTEXITCODE -ne 0) { throw 'Dedicated database pending count unavailable.' }
    return [int]($value | Select-Object -Last 1)
}

function Get-LastTerminalEpoch([string]$since) {
    if ($since -notmatch '^\d+(\.\d+)?$') { throw 'Invalid benchmark start timestamp.' }
    $sql = "SELECT coalesce(max(extract(epoch FROM coalesce(p.completed_at,p.failed_at))),0) FROM payments p JOIN payment_workflow_dispatch d ON d.payment_id=p.id WHERE d.requested_at >= to_timestamp($since)"
    $value = & docker @compose exec -T postgres psql -U payflow -d $dbName -Atqc $sql
    if ($LASTEXITCODE -ne 0) { throw 'Dedicated database terminal timestamp unavailable.' }
    return [string]($value | Select-Object -Last 1)
}

try {
    $existing = & docker @compose ps --status running --format json
    if ($LASTEXITCODE -ne 0) { throw 'Docker Desktop is unavailable.' }
    if (-not $existing) {
        & docker @compose up -d --wait
        if ($LASTEXITCODE -ne 0) { throw 'Dedicated Compose startup failed.' }
        $startedCompose = $true
    }
    else {
        & docker @compose up -d --wait
        if ($LASTEXITCODE -ne 0) { throw 'Dedicated Compose services are unavailable.' }
    }
    $env:DATABASE_URL = $dbURL
    $env:PAYFLOW_LOADTEST_DATABASE_URL = $dbURL
    $env:PAYFLOW_ENV = 'development'
    $env:PAYFLOW_LOADTEST_MODE = 'true'
    $env:PAYFLOW_ENABLE_DEV_PROCESSING = 'false'
    $env:LISTEN_ADDR = '127.0.0.1:18080'
    $env:PAYFLOW_METRICS_ADDR = '127.0.0.1:19090'
    $env:PAYFLOW_WORKER_METRICS_ADDR = '127.0.0.1:19091'
    $env:TEMPORAL_ADDRESS = '127.0.0.1:17233'

    $tcp = New-Object System.Net.Sockets.TcpClient
    try { $tcp.Connect('127.0.0.1', 18080); throw 'Port 18080 is already in use; refusing to touch another API.' }
    catch [System.Net.Sockets.SocketException] { }
    finally { $tcp.Dispose() }

    & $goBinary build -o $apiBinary ./cmd/api
    if ($LASTEXITCODE -ne 0) { throw 'API build failed.' }
    & $goBinary build -o $workerBinary ./cmd/worker
    if ($LASTEXITCODE -ne 0) { throw 'Worker build failed.' }
    & $goBinary build -o $loadBinary ./cmd/loadtest
    if ($LASTEXITCODE -ne 0) { throw 'Load client build failed.' }
    & $goBinary run ./cmd/migrate
    if ($LASTEXITCODE -ne 0) { throw 'Dedicated database migration failed.' }

    $api = New-APIProcess
    $worker = New-WorkerProcess
    $ready = $false
    for ($i = 0; $i -lt 100; $i++) {
        try {
            $response = Invoke-WebRequest 'http://127.0.0.1:18080/healthz' -UseBasicParsing -TimeoutSec 1
            if ($response.StatusCode -eq 200 -and $response.Headers['X-Payflow-Loadtest-Database'] -eq $dbName) { $ready = $true; break }
        } catch { Start-Sleep -Milliseconds 100 }
    }
    if (-not $ready) { throw 'API did not advertise the disposable test database.' }

    $before = Get-DispatchCount
    $runStartedEpoch = Get-DatabaseEpoch
    $arguments = @('-allow-local-test', '-preset', 'recovery', '-workload', 'submit', '-requests', '40',
        '-clients', '4', '-settle-timeout', '90s', '-api', 'http://127.0.0.1:18080',
        '-recovery-scenario', $Scenario, '-output-dir', 'results/loadtest')
    $load = Start-Process -FilePath $loadBinary -ArgumentList $arguments -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput (Join-Path $binDir.FullName 'load.stdout') `
        -RedirectStandardError (Join-Path $binDir.FullName 'load.stderr')
    $queued = $false
    for ($i = 0; $i -lt 200; $i++) {
        if ((Get-DispatchCount) -gt $before) { $queued = $true; break }
        if ($load.HasExited) { break }
        Start-Sleep -Milliseconds 100
    }
    if (-not $queued) { throw 'No new durable dispatch was observed; no failure was injected.' }
    if ((Get-PendingCount $runStartedEpoch) -lt 1) { throw 'Newly submitted work already settled; no failure was injected.' }

    $outageStartedEpoch = Get-DatabaseEpoch
    switch ($Scenario) {
        'api-restart' {
            Stop-OwnedProcess $api $apiBinary
            Start-Sleep -Seconds 2
            $api = New-APIProcess
        }
        'worker-restart' {
            Stop-OwnedProcess $worker $workerBinary
            Start-Sleep -Seconds 2
            $worker = New-WorkerProcess
        }
        'postgres-interruption' {
            & docker @compose stop postgres
            if ($LASTEXITCODE -ne 0) { throw 'Could not stop the isolated PostgreSQL container.' }
            $stoppedService = 'postgres'
            Start-Sleep -Seconds 2
            & docker @compose up -d --wait postgres
            if ($LASTEXITCODE -ne 0) { throw 'Could not restart isolated PostgreSQL.' }
            $stoppedService = ''
        }
        'temporal-unavailable' {
            & docker @compose stop temporal
            if ($LASTEXITCODE -ne 0) { throw 'Could not stop the isolated Temporal container.' }
            $stoppedService = 'temporal'
            Start-Sleep -Seconds 3
            & docker @compose up -d --wait temporal
            if ($LASTEXITCODE -ne 0) { throw 'Could not restart isolated Temporal.' }
            $stoppedService = ''
        }
    }
    $restoredEpoch = Get-DatabaseEpoch
    if (-not $load.WaitForExit(120000)) { throw 'Recovery run exceeded two minutes.' }
    $load.WaitForExit()
    $load.Refresh()
    $artifacts = @(Get-Content (Join-Path $binDir.FullName 'load.stdout'))
    $artifacts
    $jsonPath = $artifacts | Where-Object { $_ -like '*.json' } | Select-Object -Last 1
    if (-not $jsonPath -or -not (Test-Path -LiteralPath $jsonPath)) { throw 'Recovery benchmark produced no JSON result.' }
    $result = Get-Content -LiteralPath $jsonPath -Raw | ConvertFrom-Json
    if (-not $result.passed -or $result.recovery_result -notlike "$Scenario`:*") {
        throw 'Recovery benchmark reported failures or incomplete financial invariants.'
    }
    if ($null -ne $load.ExitCode -and $load.ExitCode -ne 0) { throw 'Recovery benchmark process failed.' }
    $terminalEpoch = Get-LastTerminalEpoch $runStartedEpoch
    $invariant = [Globalization.CultureInfo]::InvariantCulture
    $outageSeconds = [double]::Parse($terminalEpoch, $invariant) - [double]::Parse($outageStartedEpoch, $invariant)
    $postRestoreSeconds = [double]::Parse($terminalEpoch, $invariant) - [double]::Parse($restoredEpoch, $invariant)
    if ($outageSeconds -lt 0) { throw 'New work has no confirmed post-injection terminal timestamp.' }
    $message = "{0} recovered; last terminal result {1:N3}s after injection, {2:N3}s relative to restoration." -f $Scenario, $outageSeconds, $postRestoreSeconds
    if ($postRestoreSeconds -lt 0) { $message += ' Settlement preceded service restoration.' }
    Write-Output "$message Inspect the generated local benchmark artifacts."
}
finally {
    if ($stoppedService) { & docker @compose up -d --wait $stoppedService | Out-Null }
    Stop-OwnedProcess $load $loadBinary
    Stop-OwnedProcess $api $apiBinary
    Stop-OwnedProcess $worker $workerBinary
    if ($startedCompose) { & docker @compose down | Out-Null }
}
