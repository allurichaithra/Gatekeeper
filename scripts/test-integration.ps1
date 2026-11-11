$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
Add-Type -AssemblyName System.Net.Http
$root = Split-Path -Parent $PSScriptRoot
$tempRoot = Join-Path $env:TEMP ("gatekeeper-integration-" + [Guid]::NewGuid().ToString('N'))
$logs = Join-Path $tempRoot 'logs'
New-Item -ItemType Directory -Path $logs -Force | Out-Null
$testPasswordBytes = New-Object byte[] 24
$passwordGenerator = [Security.Cryptography.RandomNumberGenerator]::Create()
try { $passwordGenerator.GetBytes($testPasswordBytes); $testPass = [Convert]::ToBase64String($testPasswordBytes) + 'aA7!' }
finally { $passwordGenerator.Dispose(); [Array]::Clear($testPasswordBytes, 0, $testPasswordBytes.Length) }
$testUser = 'ci-admin'
$processFile = Join-Path $tempRoot 'processes.json'
$processManifest = Join-Path $tempRoot 'process-manifest.json'
$envNames = @(
  'GATEKEEPER_PROCESS_FILE', 'GATEKEEPER_GATEWAY_BINARY', 'GATEKEEPER_LOG_DIRECTORY', 'GOCACHE',
  'GATEWAY_LISTEN_ADDR', 'CONTROL_API_PORT', 'USER_SERVICE_PORT', 'PRODUCT_SERVICE_PORT', 'VITE_PORT',
  'SQLITE_FILE', 'GATEWAY_CONFIG_FILE', 'GATEWAY_API_KEY', 'JWT_SECRET', 'GATEWAY_ID',
  'CONTROL_PLANE_URL', 'GATEWAY_URL', 'DASHBOARD_ORIGINS', 'VITE_CONTROL_API', 'GATEWAY_TOKEN',
  'GATEKEEPER_TEST_USERNAME', 'GATEKEEPER_TEST_PASSWORD', 'GATEKEEPER_TEST_TOKEN', 'SETUP_ADMIN_USERNAME', 'SETUP_ADMIN_PASSWORD'
)
$previousEnv = @{}
foreach ($name in $envNames) { $previousEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
$success = $false
$faultBackendProcess = $null
$canaryBackendProcess = $null

function Get-FreePort {
  $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
  $listener.Start()
  try { return ([Net.IPEndPoint]$listener.LocalEndpoint).Port } finally { $listener.Stop() }
}

function Invoke-Probe([string]$Url, [string]$Method = 'GET', [object]$Body = $null, [string]$Token = '', [switch]$RawBody) {
  $client = [System.Net.Http.HttpClient]::new()
  $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::new($Method), $Url)
  try {
    if ($Token) { $request.Headers.Authorization = [System.Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $Token) }
    if ($null -ne $Body) {
      $json = if ($RawBody) { [string]$Body } else { ConvertTo-Json -InputObject $Body -Depth 20 -Compress }
      $request.Content = [System.Net.Http.StringContent]::new($json, [Text.Encoding]::UTF8, 'application/json')
    }
    $response = $client.SendAsync($request).GetAwaiter().GetResult()
    $content = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
    $headers = @{}
    foreach ($header in $response.Headers) { $headers[$header.Key] = [string]::Join(',', $header.Value) }
    foreach ($header in $response.Content.Headers) { $headers[$header.Key] = [string]::Join(',', $header.Value) }
    return [pscustomobject]@{ Status = [int]$response.StatusCode; Content = $content; Headers = $headers }
  } finally {
    if ($response) { $response.Dispose() }
    $request.Dispose()
    $client.Dispose()
  }
}

function Assert-Status($Response, [int]$Expected, [string]$Label) {
  if ($Response.Status -ne $Expected) { throw "$Label returned HTTP $($Response.Status), expected $Expected. Body: $($Response.Content)" }
}

function Invoke-Control([string]$Path, [string]$Method = 'GET', [object]$Body = $null, [string]$Token = $script:adminToken) {
  Invoke-Probe ($script:controlUrl + $Path) $Method $Body $Token
}

function Wait-GatewaySync([int]$Version, [string]$Token, [int]$Seconds = 30) {
  $deadline = [DateTime]::UtcNow.AddSeconds($Seconds)
  do {
    $result = Invoke-Control '/api/gateways' 'GET' $null $Token
    if ($result.Status -eq 200) {
      $nodes = @($result.Content | ConvertFrom-Json)
      if ($nodes.Count -and [int]$nodes[0].currentVersion -eq $Version) { return $nodes[0] }
    }
    Start-Sleep -Milliseconds 300
  } while ([DateTime]::UtcNow -lt $deadline)
  throw "Gateway did not acknowledge configuration version $Version."
}

function Update-Route($Route, [string]$Token, [bool]$Enabled, [int]$CacheTtl = 0) {
  $body = @{
    serviceId = $Route.serviceId; path = $Route.path; matchMode = $Route.matchMode; methods = @($Route.methods)
    stripPrefix = [bool]$Route.stripPrefix; rewritePrefix = $Route.rewritePrefix; timeoutMs = [int]$Route.timeoutMs
    rateLimit = [int]$Route.rateLimit; rateLimitMode = $Route.rateLimitMode; adaptiveEnabled = [bool]$Route.adaptiveEnabled
    minimumRateLimit = [int]$Route.minimumRateLimit; maximumRateLimit = [int]$Route.maximumRateLimit
    spikeSensitivity = [double]$Route.spikeSensitivity; recoveryCooldownMs = [int]$Route.recoveryCooldownMs
    cacheTtlMs = $CacheTtl; enabled = $Enabled
  }
  $result = Invoke-Control "/api/routes/$($Route.id)" 'PUT' $body $Token
  Assert-Status $result 200 'Update route'
  $updated = $result.Content | ConvertFrom-Json
  $null = Wait-GatewaySync ([int]$updated.version) $Token
  return $updated
}

function Set-ProcessRecord([string]$Name, $Process) {
  $records = @(Get-Content -LiteralPath $processFile -Raw | ConvertFrom-Json | ForEach-Object { $_ })
  $index = -1
  for ($i = 0; $i -lt $records.Count; $i++) { if ($records[$i].name -eq $Name) { $index = $i; break } }
  if ($index -lt 0) { throw "Could not find the $Name process record." }
  $records[$index] = [pscustomobject]@{ name = $Name; id = $Process.Id; path = $Process.Path; startedAt = $Process.StartTime.ToUniversalTime().ToString('o') }
  Set-Content -LiteralPath $processFile -Value (ConvertTo-Json -InputObject $records -Depth 4) -Encoding ascii
}

try {
  $gatewayPort = Get-FreePort
  $controlPort = Get-FreePort
  $usersPort = Get-FreePort
  $productsPort = Get-FreePort
  $dashboardPort = Get-FreePort
  $env:GATEKEEPER_PROCESS_FILE = $processFile
  $env:GATEKEEPER_GATEWAY_BINARY = Join-Path $tempRoot 'gateway-test.exe'
  $env:GATEKEEPER_LOG_DIRECTORY = $logs
  $env:GOCACHE = Join-Path $root '.gocache'
  $env:GATEWAY_LISTEN_ADDR = ":$gatewayPort"
  $env:CONTROL_API_PORT = [string]$controlPort
  $env:USER_SERVICE_PORT = [string]$usersPort
  $env:PRODUCT_SERVICE_PORT = [string]$productsPort
  $env:VITE_PORT = [string]$dashboardPort
  $env:SQLITE_FILE = Join-Path $tempRoot 'gatekeeper.sqlite'
  $env:GATEWAY_CONFIG_FILE = Join-Path $tempRoot 'gateway-config.json'
  $env:GATEWAY_ID = 'ci-gateway'
  $env:CONTROL_PLANE_URL = "http://127.0.0.1:$controlPort"
  $env:GATEWAY_URL = "http://127.0.0.1:$gatewayPort"
  $env:VITE_CONTROL_API = $env:CONTROL_PLANE_URL
  $env:GATEKEEPER_TEST_USERNAME = $testUser
  $env:GATEKEEPER_TEST_PASSWORD = $testPass
  $env:SETUP_ADMIN_USERNAME = $testUser
  $env:SETUP_ADMIN_PASSWORD = $testPass
  node (Join-Path $root 'services/control-plane/src/create-admin.js')
  if ($LASTEXITCODE -ne 0) { throw 'Could not create isolated integration administrator.' }
  Remove-Item Env:SETUP_ADMIN_USERNAME,Env:SETUP_ADMIN_PASSWORD -ErrorAction SilentlyContinue

  & (Join-Path $PSScriptRoot 'start-all.ps1')
  Copy-Item -LiteralPath $processFile -Destination $processManifest
  $namedProcesses = @(Get-Content -LiteralPath $processManifest -Raw | ConvertFrom-Json | ForEach-Object { $_ })
  if (-not (@($namedProcesses | Where-Object { $_.name -eq 'user-service' }).Count)) { throw "Process records missing service names: $(Get-Content -LiteralPath $processFile -Raw)" }
  $script:controlUrl = "http://127.0.0.1:$controlPort"
  $gatewayUrl = "http://127.0.0.1:$gatewayPort"
  $deadline = [DateTime]::UtcNow.AddSeconds(60)
  do {
    try {
      $health = Invoke-Probe "$script:controlUrl/health"
      if ($health.Status -eq 200) { break }
    } catch { }
    Start-Sleep -Milliseconds 500
  } while ([DateTime]::UtcNow -lt $deadline)
  Assert-Status $health 200 'Control API health'
  Assert-Status (Invoke-Probe "$gatewayUrl/health") 200 'Public gateway health'
  Assert-Status (Invoke-Probe "http://127.0.0.1:$dashboardPort") 200 'Dashboard HTTP'
  Write-Host 'Started isolated stack and dashboard.'

  $badLogin = Invoke-Control '/api/auth/login' 'POST' @{ username = $testUser; password = 'incorrect' } ''
  Assert-Status $badLogin 401 'Invalid login'
  $login = Invoke-Control '/api/auth/login' 'POST' @{ username = $testUser; password = $testPass } ''
  Assert-Status $login 200 'Admin login'
  $script:adminToken = [string]($login.Content | ConvertFrom-Json).accessToken
  $env:GATEKEEPER_TEST_TOKEN = $script:adminToken
  $session = Invoke-Control '/api/auth/me'
  Assert-Status $session 200 'Admin session'
  if (($session.Content | ConvertFrom-Json).role -ne 'ADMIN') { throw 'Admin login returned an unexpected role.' }
  Assert-Status (Invoke-Control '/api/services' 'GET' $null '') 401 'Unauthenticated management request'

  $invalidPayload = Invoke-Control '/api/routes' 'POST' @{ serviceId = 'bad'; path = 'missing-leading-slash'; methods = @('GET') }
  Assert-Status $invalidPayload 400 'Invalid route configuration'
  $largeBody = '{"payload":"' + ('x' * 70000) + '"}'
  $oversizedResponse = Invoke-Probe "$script:controlUrl/api/routes" 'POST' $largeBody $script:adminToken -RawBody
  if ($oversizedResponse.Status -ne 413 -or $oversizedResponse.Content -match 'at .+\(|stack|node_modules') { throw "Oversized JSON was not safely rejected: HTTP $($oversizedResponse.Status) $($oversizedResponse.Content)" }
  $malformed = Invoke-Probe "$script:controlUrl/api/routes" 'POST' '{"broken":' $script:adminToken -RawBody
  Assert-Status $malformed 400 'Malformed control API request'
  if ($malformed.Content -match 'at .+\(|stack|node_modules') { throw 'Malformed request response exposed internal details.' }
  Write-Host 'Authentication and request validation checks passed.'

  $userServiceResponse = Invoke-Control '/api/services' 'POST' @{ name = 'ci-users'; upstreams = @("http://localhost:$usersPort") }
  Assert-Status $userServiceResponse 201 'Create user service'
  $userService = $userServiceResponse.Content | ConvertFrom-Json
  $productServiceResponse = Invoke-Control '/api/services' 'POST' @{ name = 'ci-products'; upstreams = @("http://localhost:$productsPort") }
  Assert-Status $productServiceResponse 201 'Create product service'
  $productService = $productServiceResponse.Content | ConvertFrom-Json
  $userRouteResponse = Invoke-Control '/api/routes' 'POST' @{ serviceId = $userService.id; path = '/api/users'; matchMode = 'PREFIX'; methods = @('GET', 'POST'); timeoutMs = 5000; rateLimit = 0; cacheTtlMs = 10000; enabled = $true }
  Assert-Status $userRouteResponse 201 'Create user route'
  $userRoute = $userRouteResponse.Content | ConvertFrom-Json
  $productRouteResponse = Invoke-Control '/api/routes' 'POST' @{ serviceId = $productService.id; path = '/api/products'; matchMode = 'PREFIX'; methods = @('GET', 'POST'); timeoutMs = 5000; rateLimit = 0; enabled = $true }
  Assert-Status $productRouteResponse 201 'Create product route'
  $productRoute = $productRouteResponse.Content | ConvertFrom-Json
  $canaryPort = Get-FreePort
  $originalPort = $env:PORT
  $env:PORT = [string]$canaryPort
  $canaryBackendProcess = Start-Process -FilePath 'node.exe' -WindowStyle Hidden -PassThru -WorkingDirectory (Join-Path $root 'services/user-service') -ArgumentList 'src/main.js' -RedirectStandardOutput (Join-Path $logs 'canary-user-service.log') -RedirectStandardError (Join-Path $logs 'canary-user-service-error.log')
  if ($null -eq $originalPort) { Remove-Item Env:PORT } else { $env:PORT = $originalPort }
  $canaryReady = $false
  for ($attempt = 0; $attempt -lt 40; $attempt++) { Start-Sleep -Milliseconds 250; try { if ((Invoke-Probe "http://127.0.0.1:$canaryPort/health").Status -eq 200) { $canaryReady = $true; break } } catch { } }
  if (-not $canaryReady) { throw 'Canary user backend failed to become healthy.' }
  $registeredCanary = Invoke-Control "/api/services/$($userService.id)/upstreams" 'POST' @{ url = "http://localhost:$canaryPort"; weight = 1 }
  Assert-Status $registeredCanary 201 'Register canary backend instance'
  $null = Wait-GatewaySync ([int]($registeredCanary.Content | ConvertFrom-Json).version) $script:adminToken
  $canaryConfig = @(
    @{ id = 'stable'; name = 'Stable'; percentage = 90; targets = @(@{ url = "http://localhost:$usersPort"; weight = 1 }) },
    @{ id = 'canary'; name = 'Canary'; percentage = 10; targets = @(@{ url = "http://localhost:$canaryPort"; weight = 1 }) }
  )
  $canaryUpdate = Invoke-Control "/api/routes/$($userRoute.id)" 'PUT' @{ serviceId = $userService.id; path = '/api/users'; matchMode = 'PREFIX'; methods = @('GET', 'POST'); timeoutMs = 5000; rateLimit = 0; cacheTtlMs = 0; upstreamGroups = $canaryConfig; enabled = $true }
  Assert-Status $canaryUpdate 200 'Configure stable/canary split'
  $userRoute = $canaryUpdate.Content | ConvertFrom-Json
  $null = Wait-GatewaySync ([int]$userRoute.version) $script:adminToken
  $nodes = Invoke-Control '/api/gateways'
  $node = Wait-GatewaySync ([int]($nodes.Content | ConvertFrom-Json)[0].latestVersion) $script:adminToken
  if ([int]$node.currentVersion -ne [int]$node.latestVersion) { throw 'Gateway configuration did not become active.' }

  $userResult = Invoke-Probe "$gatewayUrl/api/users"
  Assert-Status $userResult 200 'Gateway user forwarding'
  if ($userResult.Content -notmatch 'Ada Lovelace') { throw 'User response was not returned by the sample user service.' }
  $productResult = Invoke-Probe "$gatewayUrl/api/products"
  Assert-Status $productResult 200 'Gateway product forwarding'
  if ($productResult.Content -notmatch 'Gateway Starter') { throw 'Product response was not returned by the sample product service.' }
  $splitBefore = (Invoke-Control "/api/gateway/requests?route=$($userRoute.id)&limit=200").Content | ConvertFrom-Json
  $beforeStableRequests = @($splitBefore | Where-Object groupId -eq 'stable').Count
  $beforeCanaryRequests = @($splitBefore | Where-Object groupId -eq 'canary').Count
  for ($requestIndex = 0; $requestIndex -lt 100; $requestIndex++) { $splitResponse = Invoke-Probe "$gatewayUrl/api/users"; Assert-Status $splitResponse 200 'Canary traffic request' }
  $splitRecords = (Invoke-Control "/api/gateway/requests?route=$($userRoute.id)&limit=200").Content | ConvertFrom-Json
  $stableRequests = @($splitRecords | Where-Object groupId -eq 'stable').Count - $beforeStableRequests
  $canaryRequests = @($splitRecords | Where-Object groupId -eq 'canary').Count - $beforeCanaryRequests
  if ($stableRequests -ne 90 -or $canaryRequests -ne 10) { throw "Canary distribution mismatch: stable=$stableRequests, canary=$canaryRequests, expected 90/10." }
  Write-Host "Canary distribution: stable=$stableRequests, canary=$canaryRequests requests across 100 calls."
  $liveConfig = (Invoke-Control '/api/gateway/config').Content | ConvertFrom-Json
  $groupTotal = [int](($liveConfig.routes | Where-Object id -eq $userRoute.id).upstreamGroups | Measure-Object -Property percentage -Sum).Sum
  if ($groupTotal -ne 100) { throw "Canary traffic weights did not reach gateway configuration (total=$groupTotal)." }
  $history = (Invoke-Control '/api/config/history').Content | ConvertFrom-Json
  if (-not @($history.versions | Where-Object { $_.version -eq [int]$userRoute.version }).Count) { throw 'The canary configuration version was not persisted in history.' }
  $requestList = (Invoke-Control '/api/gateway/requests?limit=10').Content | ConvertFrom-Json
  $inspected = @($requestList | Where-Object routeId -eq $userRoute.id | Select-Object -First 1)
  if (-not $inspected.Count) { throw 'Gateway request inspector did not record the forwarded user request.' }
  Assert-Status (Invoke-Control "/api/gateway/requests/$($inspected[0].requestId)") 200 'Request inspector detail lookup'
  $cacheRouteUpdate = Invoke-Control "/api/routes/$($userRoute.id)" 'PUT' @{ serviceId = $userService.id; path = '/api/users'; matchMode = 'PREFIX'; methods = @('GET', 'POST'); timeoutMs = 5000; rateLimit = 0; cacheTtlMs = 10000; upstreamGroups = $canaryConfig; enabled = $true }
  Assert-Status $cacheRouteUpdate 200 'Re-enable GET response caching'
  $userRoute = $cacheRouteUpdate.Content | ConvertFrom-Json
  $null = Wait-GatewaySync ([int]$userRoute.version) $script:adminToken
  $created = Invoke-Probe "$gatewayUrl/api/users" 'POST' @{ name = 'Integration User'; email = 'integration@example.test' }
  Assert-Status $created 201 'Gateway POST forwarding'
  $methodRejected = Invoke-Probe "$gatewayUrl/api/users" 'DELETE'
  Assert-Status $methodRejected 405 'Gateway method restriction'
  $unmatched = Invoke-Probe "$gatewayUrl/no-matching-route"
  Assert-Status $unmatched 404 'Unmatched gateway path'
  $malformedUpstream = Invoke-Probe "$gatewayUrl/api/users" 'POST' '{"broken":' '' -RawBody
  Assert-Status $malformedUpstream 400 'Malformed JSON forwarded to sample service'
  if ($malformedUpstream.Content -match 'at .+\(|stack|node_modules') { throw 'Malformed request response exposed a stack trace.' }
  Write-Host 'Admin CRUD and both gateway forwarding paths passed.'

  $metricsBefore = Invoke-Control '/api/gateway/metrics'
  Assert-Status $metricsBefore 200 'Authenticated gateway metrics'
  $before = $metricsBefore.Content | ConvertFrom-Json
  $null = Invoke-Probe "$gatewayUrl/api/users"
  $null = Invoke-Probe "$gatewayUrl/api/users"
  $metricsAfter = (Invoke-Control '/api/gateway/metrics').Content | ConvertFrom-Json
  if ([int64]$metricsAfter.cacheHits -le [int64]$before.cacheHits -or [int64]$metricsAfter.cacheMisses -lt 1) { throw 'Gateway cache hit/miss counters did not reflect the miss and repeated cache hits.' }
  $unauthorizedMetrics = Invoke-Probe "$gatewayUrl/api/gateway/metrics"
  Assert-Status $unauthorizedMetrics 401 'Gateway metrics missing key'
  $badKeyResponse = Invoke-Probe "$gatewayUrl/api/gateway/metrics" 'GET' $null 'invalid-integration-key'
  if ($badKeyResponse.Status -ne 401) { throw "Invalid gateway API key returned HTTP $($badKeyResponse.Status)." }
  Write-Host "Cache counters: hits $($before.cacheHits) -> $($metricsAfter.cacheHits), misses $($metricsAfter.cacheMisses). Internal key rejection passed."

  $limitedRouteResponse = Invoke-Control '/api/routes' 'POST' @{ serviceId = $userService.id; path = '/limit-probe'; matchMode = 'PREFIX'; methods = @('GET'); timeoutMs = 5000; rateLimit = 2; cacheTtlMs = 0; enabled = $true }
  Assert-Status $limitedRouteResponse 201 'Create rate-limited route'
  $limitedRoute = $limitedRouteResponse.Content | ConvertFrom-Json
  $null = Wait-GatewaySync ([int]$limitedRoute.version) $script:adminToken
  $limitFirst = Invoke-Probe "$gatewayUrl/limit-probe"
  $limitSecond = Invoke-Probe "$gatewayUrl/limit-probe"
  $throttled = Invoke-Probe "$gatewayUrl/limit-probe"
  if ($throttled.Status -ne 429) { throw "Per-IP limit did not engage: statuses=$($limitFirst.Status),$($limitSecond.Status),$($throttled.Status), remaining=$($limitFirst.Headers['X-RateLimit-Remaining']),$($limitSecond.Headers['X-RateLimit-Remaining']),$($throttled.Headers['X-RateLimit-Remaining'])." }
  if (-not $throttled.Headers['Retry-After'] -or $throttled.Content -notmatch 'rate|limit|thrott') { throw "429 response was missing useful retry metadata or JSON detail: Retry-After='$($throttled.Headers['Retry-After'])', body='$($throttled.Content)'" }
  Write-Host 'Per-IP route throttling check passed.'

  $viewerResponse = Invoke-Control '/api/access/users' 'POST' @{ username = 'ci-viewer'; password = 'Viewer-test-passphrase-7!'; role = 'VIEWER' }
  Assert-Status $viewerResponse 201 'Create viewer account'
  $viewerLogin = Invoke-Control '/api/auth/login' 'POST' @{ username = 'ci-viewer'; password = 'Viewer-test-passphrase-7!' } ''
  Assert-Status $viewerLogin 200 'Viewer login'
  $viewerToken = [string]($viewerLogin.Content | ConvertFrom-Json).accessToken
  Assert-Status (Invoke-Control '/api/routes' 'GET' $null $viewerToken) 200 'Viewer route inspection'
  Assert-Status (Invoke-Control '/api/services' 'POST' @{ name = 'forbidden-viewer-service'; upstreams = @("http://localhost:$usersPort") } $viewerToken) 403 'Viewer write restriction'

  $auditResponse = Invoke-Control '/api/access/audit'
  Assert-Status $auditResponse 200 'Read audit history'
  if ($auditResponse.Content -notmatch 'ci-admin' -or $auditResponse.Content -notmatch 'CREATE') { throw 'Expected administrative events were missing from audit history.' }
  foreach ($secret in @($testPass, $script:adminToken, $env:GATEWAY_API_KEY)) { if ($auditResponse.Content.Contains($secret)) { throw 'Audit history included a credential or secret.' } }

  $disabled = Update-Route $userRoute $script:adminToken $false 10000
  Assert-Status (Invoke-Probe "$gatewayUrl/api/users") 404 'Disabled route'
  Assert-Status (Invoke-Probe "$gatewayUrl/api/products") 200 'Unrelated route during route disablement'
  $null = Update-Route $userRoute $script:adminToken $true 10000
  Assert-Status (Invoke-Probe "$gatewayUrl/api/users") 200 'Re-enabled route'

  $env:GATEKEEPER_TEST_USERNAME = $testUser
  $env:GATEKEEPER_TEST_PASSWORD = $testPass
  & (Join-Path $PSScriptRoot 'test-adaptive.ps1') -RouteId $userRoute.id -OtherRouteId $productRoute.id -ControlApiUrl $script:controlUrl -GatewayUrl $gatewayUrl -TrafficGatewayUrl "http://127.0.0.2:$gatewayPort"
  & (Join-Path $PSScriptRoot 'load-test.ps1') -RouteId $userRoute.id -Iterations 80 -Concurrency 8 -CacheTtlMs 5000 -ControlApiUrl $script:controlUrl -GatewayUrl $gatewayUrl

  # Point the configured user upstream at an unused local port to simulate an
  # unavailable backend, then restore it to verify health recovery.
  $failedPort = Get-FreePort
  $unavailableService = Invoke-Control "/api/services/$($userService.id)" 'PUT' @{ name = $userService.name; upstreams = @("http://127.0.0.1:$failedPort") }
  Assert-Status $unavailableService 200 'Set unavailable user backend'
  $null = Wait-GatewaySync ([int]($unavailableService.Content | ConvertFrom-Json).version) $script:adminToken
  $health = (Invoke-Control '/api/gateway/health').Content | ConvertFrom-Json
  $userHealthKey = "$($userRoute.id):0"
  $userHealth = $health.PSObject.Properties[$userHealthKey].Value
  if (($health.PSObject.Properties.Name -notcontains $userHealthKey) -or [bool]$userHealth) { throw "Gateway health did not mark the unavailable user backend unhealthy: key=$userHealthKey value=$userHealth" }
  $failedBackend = Invoke-Probe "$gatewayUrl/api/users"
  if ($failedBackend.Status -lt 500) { throw "Unavailable backend returned HTTP $($failedBackend.Status), expected upstream failure." }
  $null = Invoke-Control '/api/alerts/rules' 'PUT' @{ errorRate = 0.001; p95LatencyMs = 300000; trafficSpikeMultiplier = 10 }
  $liveAlerts = Invoke-Control '/api/alerts'
  Assert-Status $liveAlerts 200 'Evaluate live gateway alerts'
  $alertPayload = $liveAlerts.Content | ConvertFrom-Json
  $healthAlert = @($alertPayload.alerts | Where-Object { $_.active -and $_.kind -eq 'UNHEALTHY_BACKEND' } | Select-Object -First 1)
  if (-not $healthAlert.Count) { throw 'Live alert evaluation did not record the unhealthy backend.' }
  Assert-Status (Invoke-Control "/api/alerts/$($healthAlert[0].id)/acknowledge" 'POST') 204 'Acknowledge backend alert'
  Assert-Status (Invoke-Probe "$gatewayUrl/api/products") 200 'Product route while user backend is unavailable'
  $restoredService = Invoke-Control "/api/services/$($userService.id)" 'PUT' @{ name = $userService.name; upstreams = @("http://localhost:$usersPort") }
  Assert-Status $restoredService 200 'Restore healthy user backend'
  $null = Wait-GatewaySync ([int]($restoredService.Content | ConvertFrom-Json).version) $script:adminToken
  $health = (Invoke-Control '/api/gateway/health').Content | ConvertFrom-Json
  $userHealth = $health.PSObject.Properties[$userHealthKey].Value
  if (($health.PSObject.Properties.Name -notcontains $userHealthKey) -or -not [bool]$userHealth) { throw "Gateway health did not recover after restoring the user backend: key=$userHealthKey value=$userHealth" }
  Assert-Status (Invoke-Probe "$gatewayUrl/api/users") 200 'Gateway forwarding after backend recovery'
  $recoveredAlerts = (Invoke-Control '/api/alerts').Content | ConvertFrom-Json
  if (-not @($recoveredAlerts.alerts | Where-Object { $_.kind -eq 'UNHEALTHY_BACKEND' -and -not $_.active -and $_.resolvedAt }).Count) { throw 'Backend health alert did not resolve after recovery.' }

  # Exercise per-instance circuit isolation, failover, and half-open recovery
  # using the same upstream URL before and after the backend process returns.
  $faultRouteBody = @{
    serviceId = $userRoute.serviceId; path = $userRoute.path; matchMode = $userRoute.matchMode; methods = @('GET')
    timeoutMs = 3000; rateLimit = 0; rateLimitMode = 'FIXED'; adaptiveEnabled = $false
    minimumRateLimit = 0; maximumRateLimit = 0; spikeSensitivity = 2; recoveryCooldownMs = 30000
    cacheTtlMs = 0; enabled = $true; loadBalancingAlgorithm = 'ROUND_ROBIN'
    circuitBreakerEnabled = $true; circuitFailureThreshold = 1; circuitRecoveryTimeoutMs = 1000; circuitHalfOpenProbes = 1
    maxRetries = 1; retryBackoffMs = 1; retryMaxBackoffMs = 25; retryStatuses = @(502, 503, 504)
    retryConnectionErrors = $true; retryTimeouts = $true; retryJitter = $false; retryNonIdempotent = $false
  }
  $faultRouteResponse = Invoke-Control "/api/routes/$($userRoute.id)" 'PUT' $faultRouteBody
  Assert-Status $faultRouteResponse 200 'Enable circuit breaker and retry policy'
  $faultRouteVersion = [int]($faultRouteResponse.Content | ConvertFrom-Json).version
  $null = Wait-GatewaySync $faultRouteVersion $script:adminToken
  $faultUrl = "http://127.0.0.1:$failedPort"
  $twoUpstreams = Invoke-Control "/api/services/$($userService.id)" 'PUT' @{ name = $userService.name; upstreams = @("http://localhost:$usersPort", $faultUrl) }
  Assert-Status $twoUpstreams 200 'Configure healthy and unavailable upstream instances'
  $null = Wait-GatewaySync ([int]($twoUpstreams.Content | ConvertFrom-Json).version) $script:adminToken
  $faultCircuitOpened = $false
  for ($attempt = 0; $attempt -lt 8; $attempt++) {
    $probe = Invoke-Probe "$gatewayUrl/api/users"
    if ($probe.Status -ne 200) { throw "Healthy upstream failover returned HTTP $($probe.Status)." }
    $liveMetrics = (Invoke-Control '/api/gateway/metrics').Content | ConvertFrom-Json
    $badRuntime = $liveMetrics.upstreams.upstreams | Where-Object { $_.upstream -eq $faultUrl -and $_.routeId -eq $userRoute.id } | Select-Object -First 1
    if ($badRuntime -and $badRuntime.circuitState -eq 'OPEN') { $faultCircuitOpened = $true; break }
  }
  if (-not $faultCircuitOpened) { throw "Unavailable upstream did not open its per-instance circuit while the healthy target continued serving traffic. Target '$faultUrl'; telemetry: $($liveMetrics.upstreams | ConvertTo-Json -Depth 6 -Compress)" }
  Write-Host "Circuit failover: bad upstream $faultUrl is OPEN; healthy user target continued returning HTTP 200."

  $oldPort = $env:PORT
  $env:PORT = [string]$failedPort
  try {
    $faultBackendProcess = Start-Process -FilePath 'node.exe' -WindowStyle Hidden -PassThru -WorkingDirectory (Join-Path $root 'services/user-service') -ArgumentList 'src/main.js' -RedirectStandardOutput (Join-Path $logs 'recovered-user-service.log') -RedirectStandardError (Join-Path $logs 'recovered-user-service-error.log')
  } finally {
    if ($null -eq $oldPort) { Remove-Item Env:PORT -ErrorAction SilentlyContinue } else { $env:PORT = $oldPort }
  }
  $backendReady = $false
  for ($attempt = 0; $attempt -lt 30; $attempt++) {
    $backendProbe = Invoke-Probe "$faultUrl/health"
    if ($backendProbe.Status -eq 200) { $backendReady = $true; break }
    Start-Sleep -Milliseconds 100
  }
  if (-not $backendReady) { throw 'Restarted backend did not become healthy.' }
  $faultRecovered = $false
  for ($attempt = 0; $attempt -lt 30; $attempt++) {
    $null = Invoke-Control '/api/gateway/health'
    $null = Invoke-Probe "$gatewayUrl/api/users"
    $liveMetrics = (Invoke-Control '/api/gateway/metrics').Content | ConvertFrom-Json
    $badRuntime = $liveMetrics.upstreams.upstreams | Where-Object { $_.upstream -eq $faultUrl -and $_.routeId -eq $userRoute.id } | Select-Object -First 1
    $recoveryEvents = @($liveMetrics.upstreams.circuitEvents | Where-Object { $_.upstream -eq $faultUrl -and $_.routeId -eq $userRoute.id })
    if ($badRuntime -and $badRuntime.circuitState -eq 'CLOSED' -and @($recoveryEvents | Where-Object to -eq 'HALF-OPEN').Count -gt 0) { $faultRecovered = $true; break }
    Start-Sleep -Milliseconds 150
  }
  if (-not $faultRecovered) { throw 'Recovered upstream did not pass a half-open probe and close its circuit.' }
  Write-Host "Circuit recovery: $faultUrl returned through HALF-OPEN to CLOSED; traffic resumed automatically."
  $null = Invoke-Control "/api/services/$($userService.id)" 'PUT' @{ name = $userService.name; upstreams = @("http://localhost:$usersPort") }
  $null = Update-Route $userRoute $script:adminToken $true 0
  if ($faultBackendProcess -and -not $faultBackendProcess.HasExited) { Stop-Process -Id $faultBackendProcess.Id -Force; $faultBackendProcess = $null }

  # Restart only the isolated control API process and confirm SQLite/session/config survive.
  $records = @(Get-Content -LiteralPath $processManifest -Raw | ConvertFrom-Json | ForEach-Object { $_ })
  $controlProcessRecord = $records | Where-Object name -eq 'control-api' | Select-Object -First 1
  if (-not $controlProcessRecord) { throw 'Could not find the isolated control API process record.' }
  $controlProcessId = [int]$controlProcessRecord.id
  Stop-Process -Id $controlProcessId -Force
  Start-Sleep -Milliseconds 600
  $controlProcess = Start-Process -FilePath 'node.exe' -WindowStyle Hidden -PassThru -WorkingDirectory (Join-Path $root 'services/control-plane') -ArgumentList 'src/main.js' -RedirectStandardOutput (Join-Path $logs 'control-api-restarted.log') -RedirectStandardError (Join-Path $logs 'control-api-restarted-error.log')
  Set-ProcessRecord 'control-api' $controlProcess
  $apiReady = $false
  for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 500
    $probe = Invoke-Probe "$script:controlUrl/health"
    if ($probe.Status -eq 200) { $apiReady = $true; break }
  }
  if (-not $apiReady) { throw 'Control API did not recover after restart.' }
  Assert-Status (Invoke-Control '/api/auth/me') 200 'Persistent session after control API restart'
  $routesAfterRestart = (Invoke-Control '/api/routes').Content | ConvertFrom-Json
  if (-not (@($routesAfterRestart | Where-Object id -eq $userRoute.id).Count)) { throw 'SQLite route configuration did not survive control API restart.' }
  $nodeAfterRestart = (Invoke-Control '/api/gateways').Content | ConvertFrom-Json
  if ([int]$nodeAfterRestart[0].currentVersion -ne [int]$nodeAfterRestart[0].latestVersion) { throw 'Gateway and persisted configuration versions diverged after control API restart.' }
  Assert-Status (Invoke-Probe "$gatewayUrl/api/users") 200 'Gateway forwarding after control API restart'

  $logout = Invoke-Control '/api/auth/logout' 'POST'
  Assert-Status $logout 204 'Admin logout'
  Assert-Status (Invoke-Control '/api/auth/me' 'GET' $null $script:adminToken) 401 'Revoked token rejection'
  $success = $true
  Write-Host 'Integration summary: admin/viewer auth, CRUD, both backends, cache, per-IP throttling, adaptive spike/recovery, canary snapshot/history, request inspector, circuit failover/recovery, health failure/recovery, API-key protection, audit, control restart, and logout verified.'
} finally {
  if ($faultBackendProcess -and -not $faultBackendProcess.HasExited) { Stop-Process -Id $faultBackendProcess.Id -Force -ErrorAction SilentlyContinue }
  if ($canaryBackendProcess -and -not $canaryBackendProcess.HasExited) { Stop-Process -Id $canaryBackendProcess.Id -Force -ErrorAction SilentlyContinue }
  if (Test-Path -LiteralPath $processFile) {
    $env:GATEKEEPER_PROCESS_FILE = $processFile
    & (Join-Path $PSScriptRoot 'stop-all.ps1')
  }
  Start-Sleep -Milliseconds 500
  $logLeak = $false
  if ($success -and (Test-Path -LiteralPath $tempRoot)) {
    $logText = (Get-ChildItem -LiteralPath $logs -File -ErrorAction SilentlyContinue | ForEach-Object { Get-Content -LiteralPath $_.FullName -Raw -ErrorAction SilentlyContinue }) -join "`n"
    foreach ($secret in @($testPass, $script:adminToken, $env:GATEWAY_API_KEY)) {
      if ($secret -and $logText.Contains($secret)) { $logLeak = $true }
    }
    Remove-Item -LiteralPath $tempRoot -Recurse -Force
  } elseif (Test-Path -LiteralPath $tempRoot) {
    Write-Warning "Integration artifacts and logs were retained for diagnosis at $tempRoot"
  }
  foreach ($name in $envNames) {
    if ($null -eq $previousEnv[$name]) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue }
    else { [Environment]::SetEnvironmentVariable($name, $previousEnv[$name], 'Process') }
  }
  if ($logLeak) { throw 'A test credential was found in application logs.' }
}

