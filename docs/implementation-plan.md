# Delivery status

The local application is implemented as a Go gateway, Express control API with SQLite persistence, React/Vite dashboard, and two sample Node services. The dashboard manages services, upstream instances, and routes. Configuration is atomically published to a shared file and loaded by the gateway without a restart.

The required local workflows and checks are documented in the root [README](../README.md). The Windows PowerShell startup and stop scripts run the complete application without container tools.

The project intentionally keeps its rate limiter, response cache, request metrics, and health checks in the Go process. It does not include separate orchestration, cache, telemetry, or forecasting infrastructure.
