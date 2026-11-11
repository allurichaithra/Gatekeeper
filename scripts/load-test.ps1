param(
  [Parameter(Mandatory = $true)][string]$RouteId,
  [int]$Iterations = 200,
  [int]$Concurrency = 10,
  [int]$CacheTtlMs = 30000,
  [string]$ControlApiUrl = 'http://localhost:8081',
  [string]$GatewayUrl = 'http://localhost:8080'
)
$ErrorActionPreference = 'Stop'
if ($Iterations -lt 1 -or $Concurrency -lt 1) { throw 'Iterations and Concurrency must be positive.' }
. (Join-Path $PSScriptRoot 'auth.ps1')
$token = Get-GatekeeperAccessToken $ControlApiUrl
$headers = @{ Authorization = "Bearer $token" }
$runnerSource = @'
using System;
using System.Diagnostics;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
public sealed class GatekeeperLoadResult { public int Status; public double Milliseconds; }
public static class GatekeeperLoadRunner {
  private static readonly HttpClient Client = new HttpClient { Timeout = TimeSpan.FromSeconds(30) };
  public static async Task<GatekeeperLoadResult[]> Run(string url, int count, int concurrency) {
    using (var limit = new SemaphoreSlim(concurrency)) {
      var tasks = new Task<GatekeeperLoadResult>[count];
      for (int i = 0; i < count; i++) tasks[i] = Send(url, limit);
      return await Task.WhenAll(tasks).ConfigureAwait(false);
    }
  }
  private static async Task<GatekeeperLoadResult> Send(string url, SemaphoreSlim limit) {
    await limit.WaitAsync().ConfigureAwait(false); var clock = Stopwatch.StartNew();
    try { using (var response = await Client.GetAsync(url).ConfigureAwait(false))
      return new GatekeeperLoadResult { Status = (int)response.StatusCode, Milliseconds = clock.Elapsed.TotalMilliseconds }; }
    catch { return new GatekeeperLoadResult { Status = 0, Milliseconds = clock.Elapsed.TotalMilliseconds }; }
    finally { limit.Release(); }
  }
}
'@
if (-not ('GatekeeperLoadRunner' -as [type])) { Add-Type -TypeDefinition $runnerSource -ReferencedAssemblies 'System.Net.Http.dll' }
$route = Invoke-RestMethod "$ControlApiUrl/api/routes" -Headers $headers
$route = $route | Where-Object id -eq $RouteId | Select-Object -First 1
if (-not $route) { throw "Route '$RouteId' was not found. List routes at $ControlApiUrl/api/routes." }
$original = @{
  serviceId = $route.serviceId; path = $route.path; matchMode = $route.matchMode
  methods = @($route.methods); stripPrefix = [bool]$route.stripPrefix
  rewritePrefix = $route.rewritePrefix; timeoutMs = [int]$route.timeoutMs
  rateLimit = [int]$route.rateLimit; cacheTtlMs = [int]$route.cacheTtlMs
  rateLimitMode = if ($route.rateLimitMode) { $route.rateLimitMode } else { 'FIXED' }
  adaptiveEnabled = [bool]$route.adaptiveEnabled
  minimumRateLimit = [int]$route.minimumRateLimit
  maximumRateLimit = [int]$route.maximumRateLimit
  spikeSensitivity = [double]$route.spikeSensitivity
  recoveryCooldownMs = if ($route.recoveryCooldownMs) { [int]$route.recoveryCooldownMs } else { 30000 }
  enabled = [bool]$route.enabled
}
$payload = $original.Clone()
$uri = $GatewayUrl.TrimEnd('/') + $route.path
function Set-LoadConfig([int]$Ttl) {
  $payload.cacheTtlMs = $Ttl
  $payload.rateLimit = 0
  $null = Invoke-RestMethod "$ControlApiUrl/api/routes/$RouteId" -Headers $headers -Method Put -ContentType 'application/json' -Body ($payload | ConvertTo-Json -Depth 8)
  # Wait until the Go data plane has applied the newly published version.
  $expected = (Invoke-RestMethod "$ControlApiUrl/api/gateways" -Headers $headers)[0].latestVersion
  $deadline = [DateTime]::UtcNow.AddSeconds(10)
  do {
    $active = (Invoke-RestMethod "$ControlApiUrl/api/gateways" -Headers $headers)[0]
    if ([int]$active.currentVersion -eq [int]$expected) { return }
    Start-Sleep -Milliseconds 100
  } while ([DateTime]::UtcNow -lt $deadline)
  throw "Gateway did not apply configuration version $expected."
}
function Invoke-LoadRun([string]$Label, [int]$Ttl) {
  Set-LoadConfig $Ttl
  $before = Invoke-RestMethod "$ControlApiUrl/api/gateway/metrics" -Headers $headers
  $watch = [Diagnostics.Stopwatch]::StartNew()
  $results = [GatekeeperLoadRunner]::Run($uri, $Iterations, $Concurrency).GetAwaiter().GetResult()
  $watch.Stop()
  $after = Invoke-RestMethod "$ControlApiUrl/api/gateway/metrics" -Headers $headers
  $durations = @($results | ForEach-Object Milliseconds | Sort-Object)
  $percentile = { param([double]$p) [Math]::Round($durations[[Math]::Min($durations.Count - 1, [Math]::Max(0, [Math]::Ceiling($durations.Count * $p) - 1))], 2) }
  $failed = @($results | Where-Object { $_.Status -lt 200 -or $_.Status -ge 300 }).Count
  $hits = [int64]$after.cacheHits - [int64]$before.cacheHits
  $misses = [int64]$after.cacheMisses - [int64]$before.cacheMisses
  [pscustomobject]@{
    Mode = $Label; Requests = $Iterations; Concurrency = $Concurrency
    RequestsPerSecond = [Math]::Round($Iterations / [Math]::Max($watch.Elapsed.TotalSeconds, 0.001), 2)
    AverageLatencyMs = [Math]::Round(($durations | Measure-Object -Average).Average, 2)
    P50LatencyMs = & $percentile 0.50
    P95LatencyMs = [Math]::Round($durations[[Math]::Min($durations.Count - 1, [Math]::Ceiling($durations.Count * .95) - 1)], 2)
    P99LatencyMs = & $percentile 0.99
    ErrorRate = [Math]::Round($failed / $Iterations, 4)
    CacheHits = $hits; CacheMisses = $misses
    CacheHitRate = if (($hits + $misses) -gt 0) { [Math]::Round($hits / ($hits + $misses), 4) } else { 0 }
  }
}
try {
  $report = @(Invoke-LoadRun 'Cache enabled' $CacheTtlMs; Invoke-LoadRun 'Cache disabled' 0)
  $report | Format-Table -AutoSize | Out-String -Width 180 | Write-Host
} finally {
  $payload = $original
  $null = Invoke-RestMethod "$ControlApiUrl/api/routes/$RouteId" -Headers $headers -Method Put -ContentType 'application/json' -Body ($payload | ConvertTo-Json -Depth 8)
}
