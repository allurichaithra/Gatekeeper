# Local end-to-end acceptance

Run from the repository root in Windows PowerShell 5.1 or PowerShell 7:

```powershell
./scripts/test-integration.ps1
# Alias retained for the original acceptance-test path:
./tests/integration/acceptance.ps1
```

The test creates a temporary SQLite database and snapshot, uses dynamically selected ports and generated local credentials, starts all five processes, and removes its data after a successful run. It does not touch `data/gatekeeper.sqlite` or the normal process list. Failed runs retain the temporary logs and print their path for diagnosis.

The live checks cover login/session creation, admin CRUD, viewer restrictions, both sample backends through Go, configuration acknowledgement, cache counters, rate limiting, adaptive spike/recovery, invalid and oversized requests, disabled/unmatched/method-restricted routes, invalid gateway keys, audit events, backend health failure/recovery, control API restart persistence, and revoked-token rejection. Go unit/integration tests separately cover upstream timeout/failure, health failover, atomic configuration validation/reload, and graceful gateway shutdown.

CI invokes the same isolated script on `windows-latest`. No Docker or external credentials are needed.
