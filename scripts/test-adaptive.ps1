param(
  [Parameter(Mandatory = $true)][string]$RouteId,
  [Parameter(Mandatory = $true)][string]$OtherRouteId,
  [string]$ControlApiUrl = 'http://localhost:8081',
  [string]$GatewayUrl = 'http://localhost:8080',
  [string]$TrafficGatewayUrl = 'http://127.0.0.2:8080'
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'auth.ps1')
$token = Get-GatekeeperAccessToken $ControlApiUrl
$headers = @{ Authorization = "Bearer $token" }
$burstSource = @'
using System;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
public sealed class AdaptiveBurstResult { public int Status; }
public static class AdaptiveBurstRunner {
  private static readonly HttpClient Client = new HttpClient { Timeout = TimeSpan.FromSeconds(20) };
  public static async Task<AdaptiveBurstResult[]> Run(string url, int count, int concurrency) {
    using (var gate = new SemaphoreSlim(concurrency)) {
      var tasks = new Task<AdaptiveBurstResult>[count];
      for (int i=0;i<count;i++) tasks[i]=Send(url,gate);
      return await Task.WhenAll(tasks).ConfigureAwait(false);
    }
  }
  private static async Task<AdaptiveBurstResult> Send(string url, SemaphoreSlim gate) {
    await gate.WaitAsync().ConfigureAwait(false);
    try { using (var response=await Client.GetAsync(url).ConfigureAwait(false)) return new AdaptiveBurstResult { Status=(int)response.StatusCode }; }
    catch { return new AdaptiveBurstResult { Status=0 }; }
    finally { gate.Release(); }
  }
}
'@
if (-not ('AdaptiveBurstRunner' -as [type])) { Add-Type -TypeDefinition $burstSource -ReferencedAssemblies 'System.Net.Http.dll' }
$allRoutes = Invoke-RestMethod "$ControlApiUrl/api/routes" -Headers $headers
$route = $allRoutes | Where-Object { $_.id -eq $RouteId } | Select-Object -First 1
$other = $allRoutes | Where-Object { $_.id -eq $OtherRouteId } | Select-Object -First 1
if (-not $route -or -not $other) { throw 'RouteId and OtherRouteId must identify two existing routes.' }
function New-RoutePayload($source, $mode, $enabled) {
  return @{
    serviceId=$source.serviceId; path=$source.path; matchMode=$source.matchMode; methods=@($source.methods)
    stripPrefix=[bool]$source.stripPrefix; rewritePrefix=$source.rewritePrefix; timeoutMs=[int]$source.timeoutMs
    rateLimit=20; rateLimitMode=$mode; adaptiveEnabled=$enabled; minimumRateLimit=4; maximumRateLimit=20
    spikeSensitivity=2; recoveryCooldownMs=5000; cacheTtlMs=0; enabled=$true
  }
}
function Publish-Route($payload) {
  $result = Invoke-RestMethod "$ControlApiUrl/api/routes/$RouteId" -Headers $headers -Method Put -ContentType 'application/json' -Body ($payload | ConvertTo-Json -Depth 8)
  $deadline = [DateTime]::UtcNow.AddSeconds(10)
  do {
    $active = (Invoke-RestMethod "$ControlApiUrl/api/gateways" -Headers $headers)[0]
    if ([int]$active.currentVersion -eq [int]$result.version) { return $active }
    Start-Sleep -Milliseconds 100
  } while ([DateTime]::UtcNow -lt $deadline)
  throw "Gateway did not activate route configuration version $($result.version)."
}
function Read-AdaptiveRoute {
  $payload = Invoke-RestMethod "$ControlApiUrl/api/gateway/adaptive" -Headers $headers
  return $payload.routes | Where-Object { $_.routeId -eq $RouteId } | Select-Object -First 1
}
function Read-Status([string]$url) {
  try { return [int](Invoke-WebRequest $url -UseBasicParsing -TimeoutSec 10).StatusCode }
  catch { if ($_.Exception.Response) { return [int]$_.Exception.Response.StatusCode }; return 0 }
}
$original = @{
  serviceId=$route.serviceId; path=$route.path; matchMode=$route.matchMode; methods=@($route.methods)
  stripPrefix=[bool]$route.stripPrefix; rewritePrefix=$route.rewritePrefix; timeoutMs=[int]$route.timeoutMs
  rateLimit=[int]$route.rateLimit; rateLimitMode=if($route.rateLimitMode){$route.rateLimitMode}else{'FIXED'}
  adaptiveEnabled=[bool]$route.adaptiveEnabled; minimumRateLimit=[int]$route.minimumRateLimit
  maximumRateLimit=[int]$route.maximumRateLimit; spikeSensitivity=[double]$route.spikeSensitivity
  recoveryCooldownMs=if($route.recoveryCooldownMs){[int]$route.recoveryCooldownMs}else{30000}
  cacheTtlMs=[int]$route.cacheTtlMs; enabled=[bool]$route.enabled
}
$path = $TrafficGatewayUrl.TrimEnd('/') + $route.path
$fixedPath = $GatewayUrl.TrimEnd('/') + $route.path
$startInfo = (Invoke-RestMethod "$ControlApiUrl/api/gateways" -Headers $headers)[0]
$fixedPayload = New-RoutePayload $route 'FIXED' $false
$adaptivePayload = New-RoutePayload $route 'ADAPTIVE' $true
try {
  $null = Publish-Route $fixedPayload
  $fixedStatuses = @(1..3 | ForEach-Object { Read-Status $fixedPath })
  if (@($fixedStatuses | Where-Object { $_ -ne 200 }).Count) { throw "Fixed mode normal requests failed: $fixedStatuses" }

  $activation = Publish-Route $adaptivePayload
  # Build four five-second baseline observations before the burst.
  $baselineStatuses = @()
  for ($index = 0; $index -lt 4; $index++) {
    $baselineStatuses += Read-Status $path
    if ($index -lt 3) { Start-Sleep -Milliseconds 5100 }
  }
  if (@($baselineStatuses | Where-Object { $_ -ne 200 }).Count) { throw "Adaptive baseline requests were unexpectedly throttled: $baselineStatuses" }
  $warm = Read-AdaptiveRoute
  if (-not $warm -or $warm.baselineRps -le 0) { throw 'Gateway did not establish a traffic baseline.' }

  $epoch = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
  $alignMs = [int]((5 - ($epoch % 5) + 0.15) * 1000)
  Start-Sleep -Milliseconds $alignMs
  $burst = [AdaptiveBurstRunner]::Run($path, 15, 15).GetAwaiter().GetResult()
  $burstSuccess = @($burst | Where-Object { $_.Status -eq 200 }).Count
  Start-Sleep -Milliseconds 5200
  $triggerStatus = Read-Status $path
  $spiked = Read-AdaptiveRoute
  if ($spiked.spikesDetected -lt 1 -or $spiked.effectiveLimit -ge 20 -or $triggerStatus -ne 429) {
    throw "Spike protection did not engage: spikes=$($spiked.spikesDetected), limit=$($spiked.effectiveLimit), trigger=$triggerStatus"
  }
  $otherStatus = Read-Status ($TrafficGatewayUrl.TrimEnd('/') + $other.path)
  if ($otherStatus -ne 200) { throw "Independent route failed during protection: HTTP $otherStatus" }
  $currentNode = (Invoke-RestMethod "$ControlApiUrl/api/gateways" -Headers $headers)[0]
  $versionNow = $currentNode.currentVersion
  $sameProcess = $activation.startedAt -eq $startInfo.startedAt
  if (-not $sameProcess -or [int]$versionNow -le [int]$startInfo.currentVersion) { throw 'Configuration did not reload in the same gateway process.' }

  $recovery = @()
  for ($index = 0; $index -lt 7; $index++) {
    Start-Sleep -Milliseconds 5100
    $null = Read-Status $path
    $sample = Read-AdaptiveRoute
    $recovery += [int]$sample.effectiveLimit
  }
  $recoveryReachedBase = $recovery[-1] -eq 20
  if (-not $recoveryReachedBase -or @($recovery | Where-Object { $_ -lt 15 -or $_ -gt 20 }).Count) { throw "Gradual recovery did not reach the configured base safely: $recovery" }
  [pscustomobject]@{
    Route=$route.path; FixedModeStatuses=($fixedStatuses -join ','); BaselineRps=[Math]::Round($warm.baselineRps,3)
    BurstRequests=15; BurstSuccesses=$burstSuccess; SpikeCount=$spiked.spikesDetected
    ReducedEffectiveLimit=$spiked.effectiveLimit; Throttled=$spiked.throttledRequests; RejectedStatus=$triggerStatus
    OtherRouteStatus=$otherStatus; ConfigVersionBefore=$startInfo.currentVersion; ConfigVersionAfter=$versionNow
    GatewayRestarted=(-not $sameProcess); RecoveryLimits=($recovery -join ' -> '); RecoveryReachedBase=$recoveryReachedBase
  } | Format-List
} finally {
  $null = Publish-Route $original
}
