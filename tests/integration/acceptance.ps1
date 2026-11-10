$ErrorActionPreference = 'Stop'

$composeFile = 'infra/compose/docker-compose.yml'
$base = 'http://localhost:8081'
$gateway = 'http://localhost:8080'
$email = $env:ADMIN_EMAIL
$password = $env:ADMIN_PASSWORD
if (-not $email -or -not $password) {
  throw 'Set ADMIN_EMAIL and ADMIN_PASSWORD from the Compose .env file before running acceptance.ps1.'
}

function Get-Json([string]$Url, [string]$Token = '') {
  $headers = @{}
  if ($Token) { $headers.Authorization = "Bearer $Token" }
  Invoke-RestMethod -Uri $Url -Headers $headers -Method Get
}

function Send-Json([string]$Url, [string]$Method, [object]$Body, [string]$Token) {
  $headers = @{}
  if ($Token) { $headers.Authorization = "Bearer $Token" }
  $json = ConvertTo-Json -InputObject $Body -Depth 20 -Compress
  Invoke-RestMethod -Uri $Url -Headers $headers -Method $Method -ContentType 'application/json' -Body $json
}

function Wait-Synced([string]$Token, [int]$TimeoutSeconds = 30) {
  $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
  do {
    $nodes = Get-Json "$base/api/gateways" $Token
    if ($nodes.Count -gt 0 -and ($nodes | Where-Object { -not $_.synced }).Count -eq 0) { return }
    Start-Sleep -Milliseconds 500
  } while ([DateTime]::UtcNow -lt $deadline)
  throw 'Gateway did not acknowledge the latest configuration before timeout.'
}

function Assert-UserRoute([string]$Path, [int]$Expected) {
  try {
    $response = Invoke-WebRequest -Uri "$gateway$Path" -Method Get -UseBasicParsing
    $status = [int]$response.StatusCode
    $content = $response.Content
  } catch {
    $status = [int]$_.Exception.Response.StatusCode
    $content = ''
  }
  if ($status -ne $Expected) { throw "GET $Path returned $status; expected $Expected" }
  if ($Expected -eq 200 -and $content -notmatch 'Ada|Grace|Gateway Starter|Traffic Monitor') {
    throw "GET $Path did not return expected sample backend content: $content"
  }
}

Write-Host 'Waiting for control plane and gateway health...'
$deadline = [DateTime]::UtcNow.AddSeconds(60)
do {
  try { $null = Invoke-RestMethod "$base/health/ready"; break } catch { Start-Sleep -Seconds 2 }
} while ([DateTime]::UtcNow -lt $deadline)
$null = Invoke-RestMethod "$gateway/health"

$login = Send-Json "$base/api/auth/login" POST @{ email = $email; password = $password } ''
$token = $login.token
$gatewayRuntime = Invoke-RestMethod "$gateway/health/config"

# Make this workflow repeatable by clearing its own named development resources.
$existingRoutes = Get-Json "$base/api/routes" $token
foreach ($route in $existingRoutes | Where-Object { $_.path -in @('/api/users', '/api/products') }) {
  Invoke-RestMethod -Uri "$base/api/routes/$($route.id)" -Headers @{ Authorization = "Bearer $token" } -Method Delete | Out-Null
}
$existingServices = Get-Json "$base/api/services" $token
foreach ($service in $existingServices | Where-Object { $_.name -in @('user-service', 'product-service') }) {
  Invoke-RestMethod -Uri "$base/api/services/$($service.id)" -Headers @{ Authorization = "Bearer $token" } -Method Delete | Out-Null
}

Write-Host 'Creating user-service and route...'
$userService = Send-Json "$base/api/services" POST @{ name = 'user-service'; upstreams = @('http://user-service:4001') } $token
$userRoute = Send-Json "$base/api/routes" POST @{ serviceId = $userService.id; path = '/api/users'; matchMode = 'PREFIX'; methods = @('GET', 'POST'); stripPrefix = $true; timeoutMs = 10000; enabled = $true } $token
Wait-Synced $token
$updatedRuntime = Invoke-RestMethod "$gateway/health/config"
if ($updatedRuntime.startedAt -ne $gatewayRuntime.startedAt -or [int]$updatedRuntime.version -le [int]$gatewayRuntime.version) {
  throw 'Gateway did not activate the new configuration in the existing process.'
}
Assert-UserRoute '/api/users' 200

Write-Host 'Creating product-service and route...'
$productService = Send-Json "$base/api/services" POST @{ name = 'product-service'; upstreams = @('http://product-service:4002') } $token
$productRoute = Send-Json "$base/api/routes" POST @{ serviceId = $productService.id; path = '/api/products'; matchMode = 'PREFIX'; methods = @('GET', 'POST'); stripPrefix = $true; timeoutMs = 10000; enabled = $true } $token
Wait-Synced $token
Assert-UserRoute '/api/users' 200
Assert-UserRoute '/api/products' 200

Write-Host 'Disabling user route and checking product route remains live...'
$null = Send-Json "$base/api/routes/$($userRoute.id)" PUT @{ serviceId = $userService.id; path = '/api/users'; matchMode = 'PREFIX'; methods = @('GET', 'POST'); stripPrefix = $true; timeoutMs = 10000; enabled = $false } $token
Wait-Synced $token
Assert-UserRoute '/api/users' 404
Assert-UserRoute '/api/products' 200

Write-Host 'Stopping control plane temporarily; gateway must keep serving last-known-good routes...'
try {
  docker compose -f $composeFile stop control-plane
  $runtimeDuringOutage = Invoke-RestMethod "$gateway/health/config"
  if ($runtimeDuringOutage.startedAt -ne $gatewayRuntime.startedAt) { throw 'Gateway process restarted during the control-plane outage.' }
  Assert-UserRoute '/api/users' 404
  Assert-UserRoute '/api/products' 200
} finally {
  docker compose -f $composeFile start control-plane
}
$deadline = [DateTime]::UtcNow.AddSeconds(60)
do {
  try { $null = Invoke-RestMethod "$base/health/ready"; break } catch { Start-Sleep -Seconds 2 }
} while ([DateTime]::UtcNow -lt $deadline)
Wait-Synced $token

Write-Host 'PASS: dynamic registration, simultaneous routing, disablement, acknowledgements, and control-plane outage retention verified.'
