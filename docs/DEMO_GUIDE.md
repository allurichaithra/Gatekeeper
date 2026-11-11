# Gatekeeper Demonstration Guide

## One-command isolated demonstration

From PowerShell at the repository root:

```powershell
./scripts/demo-gatekeeper.ps1
```

The script invokes the repeatable Windows integration flow. It generates throwaway credentials, uses temporary ports and a temporary SQLite database/configuration, starts and stops its own processes, and exits nonzero on a failed assertion. It does not touch the normal `data` directory. It prints real responses, metric deltas, configuration versions, circuit states, and recovery observations while exercising the registered user/product backends, role restrictions, cache, rate limiting, adaptive response, canary snapshot, request inspection, backend failure/recovery, and control-plane restart.

## Dashboard walk-through

For a visual walkthrough, start the normal local application and create the initial administrator:

```powershell
./scripts/create-admin.ps1
./scripts/start-all.ps1
```

1. Sign in, open Services, and confirm the registered user/product upstreams.
2. Open Routes and inspect path, method, cache, timeout, rate, circuit, and retry settings.
3. Open Canary Deployments and set a percentage split after registering distinct upstream instances. The split is deterministic; a healthy alternate group receives requests when the assigned group has no eligible target.
4. Open Configuration History to inspect actors/change summaries, compare two versions, inspect the snapshot, or roll back. Each rollback publishes a new version.
5. Open Request Inspector after making gateway calls to inspect request IDs and sanitized request-level decisions.
6. Open Analytics & Alerts for actual gateway percentiles/status/traffic and current alert history. ADMIN can change thresholds and acknowledge active alerts; VIEWER can only inspect.
7. Open Gateway Health and Circuit Breakers to observe actual upstream states and recovery.

Use **Sign out** when finished, then stop locally launched services:

```powershell
./scripts/stop-all.ps1
```

Do not use production credentials during a class demonstration. The sample services keep small in-memory datasets and reset on restart.
