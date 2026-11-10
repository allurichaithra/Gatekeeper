# Go gateway

The gateway loads its initial snapshot from `GATEWAY_CONFIG_FILE` (default `<project root>/data/gateway-config.json`) and checks the file for changes once a second. Complete snapshots are validated before activation. The last valid snapshot is stored through an atomic replacement and remains available after restarts.

Useful endpoints:

- `GET /health`
- `GET /api/gateway/metrics`
- `GET /api/gateway/routes`
- `GET /api/gateway/health`

Routing supports `EXACT` and segment-boundary `PREFIX` paths, allowed methods, path stripping or rewriting, per-route timeouts, weighted round robin, per-client-IP sliding-window limits, and bounded TTL caching for successful GET responses. Request IDs and JSON access logs are enabled by default.

See the root [README](../../README.md) for startup, configuration, and test commands.
