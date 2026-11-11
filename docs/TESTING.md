# Testing and Performance Validation

Run from a PowerShell prompt at the repository root. Node tests use temporary SQLite files; the integration script chooses temporary ports, config, and test credentials and cleans up child processes. It does not use `data/gatekeeper.sqlite`.

```powershell
npm ci
$env:GOCACHE = Join-Path (Get-Location) '.gocache'
go -C services/gateway test ./...
go -C services/gateway vet ./...
npm run test:control-api
npm run test:sample-services
npm run lint --workspace @gatekeeper/control-plane
npm run typecheck --workspace @gatekeeper/control-plane
npm run lint --workspace @gatekeeper/dashboard
npm run typecheck --workspace @gatekeeper/dashboard
npm run build --workspace @gatekeeper/dashboard
npm run format:check
./scripts/test-integration.ps1
```

The Windows integration suite covers login, RBAC, CRUD, real user/product forwarding, route disablement, cache miss/hit counters, 429 and retry metadata, adaptive burst and recovery, circuit isolation and half-open recovery, backend outage and return, API-key rejection, audit, control-plane restart/config persistence, and session revocation. It also verifies published canary weights/config history and the request inspector. `scripts/demo-gatekeeper.ps1` runs the same isolated sequence as a demonstration.

## Load tests

With all applications running and a route configured, execute:

```powershell
./scripts/load-test.ps1 -Url 'http://localhost:8080/api/users' -Requests 1000 -Concurrency 20
```

The script exercises cache enabled and disabled, reports requests per second, average/P50/P95/P99 latency, errors, and cache counters. Repeat with the same machine, route, request count, concurrency, and warm-up conditions. These small localhost runs validate behavior and compare changes only; no benchmark number is stated in this document because no standardized hardware run was performed for this submission. Local measurements do not establish production capacity or availability.

## CI

`.github/workflows/ci.yml` runs on Windows Server with Go 1.23 and Node 24. It executes Go tests and vet, control/sample service tests, JavaScript lint/typecheck, dashboard lint/typecheck/build, formatting and PowerShell parser checks, plus the isolated end-to-end suite. CI credentials and test databases are generated per test run and are not repository secrets.

Race detector runs are environment-dependent on Windows because Go's race detector requires cgo and a compatible C compiler. Do not report a race run as passed unless the command ran successfully on the chosen toolchain.
