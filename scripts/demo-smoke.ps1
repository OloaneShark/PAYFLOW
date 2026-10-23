# Run only after a separately authorized temporary AWS deployment and SSM tunnel.
param(
    [Parameter(Mandatory = $true)][string]$CertificatePath,
    [Parameter(Mandatory = $true)][string]$MerchantId,
    [int]$LocalPort = 18443,
    [string]$ClientPath = '.tools/demo-client.exe',
    [string]$EvidenceDirectory = 'results/aws-demo'
)

$ErrorActionPreference = 'Stop'
if ($MerchantId -notmatch '^[a-zA-Z0-9_-]{1,64}$') { throw 'Use a fresh synthetic merchant ID.' }
if (-not (Test-Path -LiteralPath $CertificatePath -PathType Leaf)) { throw 'Certificate file is missing.' }
if ($LocalPort -lt 1024 -or $LocalPort -gt 65535) { throw 'Local port must be 1024..65535.' }
$cert = (Resolve-Path -LiteralPath $CertificatePath).Path.Replace('\', '/')
if (-not (Test-Path -LiteralPath $ClientPath -PathType Leaf)) { throw 'Build cmd/demo-client first.' }
$client = (Resolve-Path -LiteralPath $ClientPath).Path

function Invoke-DemoRequest([string]$path, [string]$body, [string]$key) {
    $inputJson = @{ path = $path; body = $body; bearer = $key } | ConvertTo-Json -Compress
    # The bearer key goes through stdin; it never appears in process arguments.
    $response = $inputJson | & $client -cert $cert -local-port $LocalPort
    if ($LASTEXITCODE -ne 0) { throw "HTTPS request failed: $path (client exit $LASTEXITCODE)" }
    return $response
}

$secureKey = Read-Host 'Synthetic merchant bearer key' -AsSecureString
$bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureKey)
try { $key = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr) }
finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
if (-not $key) { throw 'No merchant key supplied.' }

try {
    $null = Invoke-DemoRequest '/readyz' '' ''
    $selected = @{}
    $created = @()
    for ($i = 0; $i -lt 20 -and $selected.Count -lt 2; $i++) {
        $payload = @{ merchantId = $MerchantId; idempotencyKey = "demo-$([guid]::NewGuid())"; amount = '100'; currency = 'JPY' } | ConvertTo-Json -Compress
        $result = Invoke-DemoRequest '/payflow.v1.PaymentService/CreatePayment' $payload $key | ConvertFrom-Json
        $p = $result.payment
        if (-not $p.paymentId -or $p.merchantId -ne $MerchantId -or $p.status -ne 'PAYMENT_STATUS_CREATED') { throw 'CreatePayment response was unexpected.' }
        $created += $p.paymentId
        $sha = [Security.Cryptography.SHA256]::Create()
        try { $firstByte = $sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($p.paymentId))[0] }
        finally { $sha.Dispose() }
        $scenario = if (($firstByte % 5) -eq 0) { 'decline' } else { 'approve' }
        if (-not $selected.ContainsKey($scenario)) { $selected[$scenario] = $p.paymentId }
    }
    if ($selected.Count -ne 2) { throw 'Could not select both mock outcomes within 20 synthetic creations; no payments were submitted.' }

    $observed = @{}
    foreach ($scenario in @('approve', 'decline')) {
        $payload = @{ paymentId = $selected[$scenario] } | ConvertTo-Json -Compress
        $null = Invoke-DemoRequest '/payflow.v1.PaymentService/SubmitPayment' $payload $key | ConvertFrom-Json
    }
    $deadline = (Get-Date).AddSeconds(120)
    do {
        foreach ($scenario in @('approve', 'decline')) {
            $payload = @{ paymentId = $selected[$scenario] } | ConvertTo-Json -Compress
            $result = Invoke-DemoRequest '/payflow.v1.PaymentService/GetPayment' $payload $key | ConvertFrom-Json
            $observed[$scenario] = $result.payment.status
        }
        if ($observed.approve -eq 'PAYMENT_STATUS_COMPLETED' -and $observed.decline -eq 'PAYMENT_STATUS_FAILED') { break }
        Start-Sleep -Seconds 3
    } while ((Get-Date) -lt $deadline)

    $balance = Invoke-DemoRequest '/payflow.v1.PaymentService/GetMerchantBalance' '{"currency":"JPY"}' $key | ConvertFrom-Json
    $pass = $observed.approve -eq 'PAYMENT_STATUS_COMPLETED' -and
        $observed.decline -eq 'PAYMENT_STATUS_FAILED' -and
        $balance.currency -eq 'JPY' -and [string]$balance.amount -eq '100'
    $evidence = [ordered]@{
        recordedAtUtc = (Get-Date).ToUniversalTime().ToString('o')
        hostname = 'payflow-demo.test'
        tlsCertificateSha256 = (Get-FileHash -LiteralPath $CertificatePath -Algorithm SHA256).Hash
        merchantId = $MerchantId
        createdPaymentCount = $created.Count
        approvedPaymentId = $selected.approve
        approvedStatus = $observed.approve
        declinedPaymentId = $selected.decline
        declinedStatus = $observed.decline
        merchantBalanceJpy = [string]$balance.amount
        apiAndBalanceCheck = if ($pass) { 'PASS' } else { 'INCOMPLETE' }
        ledgerJournalVerification = 'PENDING_PRIVATE_ADMIN_INSPECTION'
    }
    New-Item -ItemType Directory -Path $EvidenceDirectory -Force | Out-Null
    $file = Join-Path $EvidenceDirectory 'smoke-redacted.json'
    $evidence | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $file -Encoding utf8
    Write-Host "Redacted evidence: $file"
    if (-not $pass) { throw 'Terminal status or merchant balance check was incomplete; inspect evidence and do not claim financial verification.' }
}
finally { $key = $null; $secureKey.Dispose() }
