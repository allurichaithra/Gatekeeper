# Local load comparison

Use the existing repeatable PowerShell load runner after starting Gatekeeper and creating a route:

```powershell
./scripts/start-all.ps1
. ./scripts/auth.ps1
$token = Get-GatekeeperAccessToken 'http://localhost:8081'
$headers = @{ Authorization = "Bearer $token" }
$routes = Invoke-RestMethod http://localhost:8081/api/routes -Headers $headers
./scripts/load-test.ps1 -RouteId '<route-id>' -Iterations 500 -Concurrency 20
```

The runner prompts for administrator credentials if `GATEKEEPER_TEST_USERNAME` and `GATEKEEPER_TEST_PASSWORD` are not set. It measures cache enabled and disabled with the same route and reports throughput, average latency, P50/P95/P99 latency, non-2xx error rate, cache hits/misses, and cache hit rate. It temporarily disables the route rate limit and restores all route settings in a `finally` block. You can configure `-ControlApiUrl`, `-GatewayUrl`, `-CacheTtlMs`, `-Iterations`, and `-Concurrency`.

Results are a local comparison, not a production capacity claim. They depend on CPU scheduling, local networking, Node/Go versions, sample-service behavior, warmup, and the configured concurrency. Repeat several runs on the same machine and report hardware and settings alongside results. Avoid comparing runs taken under different background load.
