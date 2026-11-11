# Security Model

## Identity and authorization

Control-plane passwords are bcrypt hashes. Admin login issues a short-lived signed access token backed by a persisted session; logout revokes the session and every authenticated request checks revocation/expiry. Login failures are throttled per client address and username. ADMIN can mutate configuration, accounts, alert thresholds, acknowledge alerts, and roll back configuration. VIEWER can inspect read APIs only; authorization is enforced in Express.

## Gateway credentials

`JWT_SECRET` signs dashboard sessions. `GATEWAY_API_KEY` protects internal gateway polling, acknowledgement, metrics, health, and inspector endpoints. They are distinct values, each at least 32 bytes. The dashboard receives neither secret. The local startup script generates random values when unset; use secret environment variables managed by the operator in deployment. Do not check secrets or the local `.env` file into source control.

## Input and response handling

Express accepts JSON bodies up to 64 KiB, validates route/upstream/user/alert inputs, sets security headers, restricts browser origins using `DASHBOARD_ORIGINS`, and sends sanitized errors. The Go gateway validates configuration snapshots before activation, limits config payload size, uses request-scoped deadlines and cancellation, and returns request IDs. Public health and configured data-plane routes remain unauthenticated by design; management and internal routes are protected.

## Logs, audit, and request history

Audit records contain actor, action, resource, timestamp, and allowlisted configuration changes. Configuration history stores validated snapshots, not authentication or gateway keys. Gateway request history contains only timestamp, request ID, method, path, route/upstream IDs, status, durations, cache/retry/circuit/rate decisions. It does not retain headers, cookies, authorization values, request bodies, passwords, or tokens and is limited to the newest 1,000 records in memory. Avoid putting personal or confidential values in URL paths because paths are part of request metadata.

## Operational limitations

The local sample does not include TLS termination, distributed rate-limit coordination, multi-node shared cache, external secret vault, or durable gateway telemetry. Put TLS and network access controls in front of the app where needed. Keep management endpoints private and rotate keys through a controlled restart when required.
