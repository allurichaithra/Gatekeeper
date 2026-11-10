$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$dataDirectory = Join-Path $root 'data'
New-Item -ItemType Directory -Path $dataDirectory -Force | Out-Null
$processFile = if ($env:GATEKEEPER_PROCESS_FILE) { $env:GATEKEEPER_PROCESS_FILE } else { Join-Path $dataDirectory 'gatekeeper-processes.json' }
if (Test-Path -LiteralPath $processFile) {
  foreach ($record in (Get-Content -LiteralPath $processFile -Raw | ConvertFrom-Json)) {
    $existing = Get-Process -Id $record.id -ErrorAction SilentlyContinue
    if ($existing -and $existing.Path -eq $record.path -and $existing.StartTime.ToUniversalTime().ToString('o') -eq $record.startedAt) { throw 'Gatekeeper appears to be running already. Run ./scripts/stop-all.ps1 first.' }
  }
}
$logDirectory = Join-Path $dataDirectory (Join-Path 'logs' (Get-Date -Format 'yyyyMMdd-HHmmss-fff'))
New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null

function New-LocalSecret {
  $secretBytes = New-Object byte[] 32
  $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
  try { $generator.GetBytes($secretBytes); return [Convert]::ToBase64String($secretBytes) }
  finally { $generator.Dispose(); [Array]::Clear($secretBytes, 0, $secretBytes.Length) }
}

if (-not $env:CONTROL_API_PORT) { $env:CONTROL_API_PORT = '8081' }
if (-not $env:GATEWAY_LISTEN_ADDR) { $env:GATEWAY_LISTEN_ADDR = ':8080' }
if (-not $env:GATEWAY_CONFIG_FILE) { $env:GATEWAY_CONFIG_FILE = Join-Path $dataDirectory 'gateway-config.json' }
if (-not $env:SQLITE_FILE) { $env:SQLITE_FILE = Join-Path $dataDirectory 'gatekeeper.sqlite' }
if (-not $env:USER_SERVICE_PORT) { $env:USER_SERVICE_PORT = '4001' }
if (-not $env:PRODUCT_SERVICE_PORT) { $env:PRODUCT_SERVICE_PORT = '4002' }
if (-not $env:VITE_PORT) { $env:VITE_PORT = '5173' }
if (-not $env:GATEWAY_API_KEY) { $env:GATEWAY_API_KEY = New-LocalSecret }
if (-not $env:JWT_SECRET) { $env:JWT_SECRET = New-LocalSecret }
if (-not $env:GATEWAY_ID) { $env:GATEWAY_ID = 'local-gateway' }
if (-not $env:CONTROL_PLANE_URL) { $env:CONTROL_PLANE_URL = "http://localhost:$env:CONTROL_API_PORT" }
$env:GATEWAY_TOKEN = $env:GATEWAY_API_KEY
$env:DASHBOARD_ORIGINS = "http://localhost:$env:VITE_PORT"
$env:VITE_CONTROL_API = "http://localhost:$env:CONTROL_API_PORT"
if (-not $env:GATEWAY_URL) {
  $gatewayPort = $env:GATEWAY_LISTEN_ADDR.Substring($env:GATEWAY_LISTEN_ADDR.LastIndexOf(':') + 1)
  $env:GATEWAY_URL = "http://localhost:$gatewayPort"
}
if (-not $env:GOCACHE) { $env:GOCACHE = Join-Path $root '.gocache' }

$gatewayBinary = if ($env:GATEKEEPER_GATEWAY_BINARY) { $env:GATEKEEPER_GATEWAY_BINARY } else { Join-Path $dataDirectory 'gateway.exe' }
& go -C (Join-Path $root 'services/gateway') build -o $gatewayBinary ./cmd/gateway
if ($LASTEXITCODE -ne 0) { throw 'Could not build the Go gateway.' }

$processes = @()
$processes += Start-Process -FilePath $gatewayBinary -WindowStyle Hidden -PassThru -WorkingDirectory $root -RedirectStandardOutput (Join-Path $logDirectory 'gateway.log') -RedirectStandardError (Join-Path $logDirectory 'gateway-error.log')
$processes += Start-Process -FilePath 'node.exe' -WindowStyle Hidden -PassThru -ArgumentList 'src/main.js' -WorkingDirectory (Join-Path $root 'services/control-plane') -RedirectStandardOutput (Join-Path $logDirectory 'control-api.log') -RedirectStandardError (Join-Path $logDirectory 'control-api-error.log')
$originalPort = $env:PORT
$env:PORT = $env:USER_SERVICE_PORT
$processes += Start-Process -FilePath 'node.exe' -WindowStyle Hidden -PassThru -ArgumentList 'src/main.js' -WorkingDirectory (Join-Path $root 'services/user-service') -RedirectStandardOutput (Join-Path $logDirectory 'user-service.log') -RedirectStandardError (Join-Path $logDirectory 'user-service-error.log')
$env:PORT = $env:PRODUCT_SERVICE_PORT
$processes += Start-Process -FilePath 'node.exe' -WindowStyle Hidden -PassThru -ArgumentList 'src/main.js' -WorkingDirectory (Join-Path $root 'services/product-service') -RedirectStandardOutput (Join-Path $logDirectory 'product-service.log') -RedirectStandardError (Join-Path $logDirectory 'product-service-error.log')
if ($null -eq $originalPort) { Remove-Item Env:PORT } else { $env:PORT = $originalPort }
$processes += Start-Process -FilePath 'node.exe' -WindowStyle Hidden -PassThru -ArgumentList (Join-Path $root 'node_modules/vite/bin/vite.js') -WorkingDirectory (Join-Path $root 'apps/dashboard') -RedirectStandardOutput (Join-Path $logDirectory 'dashboard.log') -RedirectStandardError (Join-Path $logDirectory 'dashboard-error.log')

$processRecords = @($processes | ForEach-Object { @{ id = $_.Id; path = $_.Path; startedAt = $_.StartTime.ToUniversalTime().ToString('o') } })
Set-Content -LiteralPath $processFile -Value (ConvertTo-Json -InputObject $processRecords) -Encoding ascii
Write-Host "Starting Gatekeeper: dashboard http://localhost:$env:VITE_PORT, gateway $env:GATEWAY_LISTEN_ADDR, control API :$env:CONTROL_API_PORT, services :$env:USER_SERVICE_PORT/:$env:PRODUCT_SERVICE_PORT"
Write-Host 'Use ./scripts/stop-all.ps1 to stop these processes.'

