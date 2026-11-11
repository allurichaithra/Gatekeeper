# API Reference

The dashboard calls the Express API under `/api` with `Authorization: Bearer <access-token>`. All management reads require authentication. Mutations require ADMIN. VIEWER can read dashboard, route, service, metrics, health, alert, history, and inspector data. Gateway internal endpoints require the separate configured gateway API key and are never called directly from browser code.

## Authentication and access

| Method   | Endpoint                | Access            | Description                       |
| -------- | ----------------------- | ----------------- | --------------------------------- |
| POST     | `/api/auth/login`       | Public, throttled | Create short-lived access session |
| GET      | `/api/auth/me`          | Authenticated     | Current user                      |
| POST     | `/api/auth/logout`      | Authenticated     | Revoke current session            |
| GET/POST | `/api/access/users`     | ADMIN             | List/create users                 |
| PATCH    | `/api/access/users/:id` | ADMIN             | Change account role/state         |
| GET      | `/api/access/audit`     | ADMIN             | Administrative audit history      |

## Routes, services and configuration

| Method       | Endpoint                                  | Description                                            |
| ------------ | ----------------------------------------- | ------------------------------------------------------ |
| GET, POST    | `/api/services`                           | List/create backend services                           |
| PUT, DELETE  | `/api/services/:id`                       | Update/delete service                                  |
| POST         | `/api/services/:id/upstreams`             | Register an upstream instance                          |
| PUT, DELETE  | `/api/upstreams/:id`                      | Update/delete upstream                                 |
| GET, POST    | `/api/routes`                             | List/create routes                                     |
| PUT, DELETE  | `/api/routes/:id`                         | Update/delete routes                                   |
| POST, DELETE | `/api/routes/:id/adaptive/override`       | Set/clear an expiring adaptive limit override          |
| GET          | `/api/gateway/config`                     | Current persisted gateway snapshot                     |
| GET          | `/api/config/history`                     | Version list and active version                        |
| GET          | `/api/config/history/:version`            | Historical non-secret snapshot                         |
| GET          | `/api/config/history/compare?from=1&to=2` | Route field comparison                                 |
| POST         | `/api/config/rollback/:version`           | Validate and publish rollback as a new version (ADMIN) |

Optional `upstreamGroups` route value:

```json
[
  {
    "id": "stable",
    "name": "Stable",
    "percentage": 90,
    "targets": [{ "url": "http://localhost:4001", "weight": 1 }]
  },
  {
    "id": "canary",
    "name": "Canary",
    "percentage": 10,
    "targets": [{ "url": "http://localhost:4003", "weight": 1 }]
  }
]
```

Groups require unique IDs, registered enabled upstreams belonging to the route service, and percentages totaling 100.

## Live gateway data

| Method   | Endpoint                                                                          | Description                                                                  |
| -------- | --------------------------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| GET      | `/api/gateway/metrics`                                                            | Counters, response percentiles, route metrics, cache/retry/circuit telemetry |
| GET      | `/api/gateway/health`                                                             | Active upstream health probes                                                |
| GET      | `/api/gateway/requests?limit=100&route=...&status=...&upstream=...&requestId=...` | Recent sanitized gateway requests                                            |
| GET      | `/api/gateway/requests/:requestId`                                                | One retained request record                                                  |
| GET      | `/api/alerts`                                                                     | Evaluated current alerts and bounded persistent history                      |
| GET, PUT | `/api/alerts/rules`                                                               | Read/update error-rate, P95 latency and traffic spike thresholds             |
| POST     | `/api/alerts/:id/acknowledge`                                                     | Acknowledge active alert (ADMIN)                                             |
| GET      | `/api/analytics`                                                                  | Control API request counters                                                 |
| GET      | `/health`                                                                         | Control API liveness                                                         |

## Data plane

Default local URLs are gateway `http://localhost:8080`, user data through `/api/users`, and products through `/api/products`.

| Method   | Gateway path        | Service behavior     |
| -------- | ------------------- | -------------------- |
| GET/POST | `/api/users`        | List/create users    |
| GET      | `/api/users/:id`    | Read a user          |
| GET/POST | `/api/products`     | List/create products |
| GET      | `/api/products/:id` | Read a product       |

Gateway operator endpoints `/health`, `/api/gateway/metrics`, `/api/gateway/routes`, `/api/gateway/health`, `/api/gateway/requests`, and adaptive telemetry endpoints are protected by the gateway API key. `/health` is public liveness only. Do not place that API key in browser code.
