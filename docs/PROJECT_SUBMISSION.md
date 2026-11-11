# Gatekeeper Project Submission

## Abstract

Gatekeeper is a locally runnable API gateway and control dashboard. It combines a Go data plane for route selection, reverse proxying, caching, per-client throttling, health-aware load balancing and fault handling with an Express control plane that persists settings and accounts in SQLite. A React dashboard gives administrators and viewers role-appropriate visibility into real gateway activity.

## Problem statement

Small applications need a single entry point for multiple backends and a practical way to manage route policies, limit abusive traffic, observe latency, and respond to backend failure without adopting heavyweight orchestration infrastructure.

## Objectives

- Route configured HTTP traffic to multiple local backend services.
- Change gateway configuration dynamically using validated snapshots.
- Apply bounded caching, fixed/adaptive rate limits, health-aware selection, circuit breaking and safe retries.
- Persist management configuration and audit history in SQLite.
- Provide authenticated ADMIN/VIEWER access and live dashboard observations.
- Keep installation and demonstration simple on Windows.

## Existing system limitations

Manual reverse-proxy configuration is easy to drift, provides weak operational visibility, and often couples route changes to process restarts. Heavy distributed platforms add operational burden that is not appropriate for this individual local project.

## Proposed system

Express accepts and validates management requests, stores route/service/user/audit data in SQLite, generates versioned configuration snapshots and records configuration history. Go polls and validates snapshots and serves traffic from immutable active configuration. React presents live metrics and authorized controls. User and product sample services make the system reproducible without external infrastructure.

## Architecture

The full component, request lifecycle, and circuit-state diagrams are in [architecture.md](architecture.md). The project uses Go standard library, Node.js/Express, built-in `node:sqlite`, React, TypeScript, and Vite. The gateway and runtime observations stay in process memory; management and audit state persist locally.

## Implementation details and algorithms

- Exact or longest-prefix route selection with method checks and optional path rewriting.
- Immutable configuration snapshots validated before atomic activation; rollback republishes an old valid configuration under a new version.
- Deterministic percentage-based group assignment for canary traffic and configured load balancing within each group.
- Per-IP/per-route fixed window throttling and adaptive EWMA traffic response with gradual recovery.
- Bounded TTL GET cache that excludes unsuccessful and cookie-setting responses.
- Per-upstream CLOSED/OPEN/HALF-OPEN circuits, health-based exclusion and bounded safe retries.
- Bounded rolling request metadata and observed-sample P50/P95/P99 calculations.
- Bcrypt password hashes, short-lived signed tokens, persisted session revocation, ADMIN/VIEWER checks, a separate gateway key and audit records.

## Testing and results

Run the commands in [TESTING.md](TESTING.md). The project includes Go unit/integration tests, Express API and security tests, sample service tests, dashboard lint/typecheck/build, and an isolated Windows PowerShell end-to-end suite. Test results for a particular submission build must be reported from the actual run; do not infer results from CI configuration.

## Performance analysis

The repeatable load script reports requests/second, mean/P50/P95/P99 latency, errors, and cache counters for enabled/disabled cache runs. No hardware-specific measurement is included here because no standardized performance run has been made for this submission. Local loopback runs are useful for regression comparison, not production capacity claims.

## Limitations

Rate-limit, cache, circuit, health, adaptive and request-history state is process-local and resets on restart. There is no cross-node coordination, TLS termination, external secret vault, or durable data-plane telemetry. Canary splitting is deterministic request-weight scheduling and is not sticky per client. Sample data is intentionally small and local.

## Future scope

Potential future work includes persistent metrics export, distributed coordination, TLS integration, more detailed canary group analytics, and automated end-to-end tests driven through browser UI. These are outside the scope of the current local architecture.

## Conclusion

Gatekeeper demonstrates an integrated, understandable API gateway control loop with authentication, persistent configuration, adaptive traffic policies, upstream fault handling, live observability, rollback, canary routing, and a reproducible Windows demonstration while retaining a lightweight single-machine stack.
