import React, { useCallback, useEffect, useState } from 'react';
import ReactDOM from 'react-dom/client';
import './style.css';

type Upstream = { id: string; url: string; weight: number; enabled: boolean };
type Service = { id: string; name: string; upstreams: Upstream[]; _count?: { routes: number } };
type Route = {
  id: string;
  path: string;
  matchMode: 'PREFIX' | 'EXACT';
  methods: string[];
  stripPrefix: boolean;
  rewritePrefix: string | null;
  timeoutMs: number;
  rateLimit?: number;
  rateLimitMode?: 'FIXED' | 'ADAPTIVE';
  adaptiveEnabled?: boolean;
  minimumRateLimit?: number;
  maximumRateLimit?: number;
  spikeSensitivity?: number;
  recoveryCooldownMs?: number;
  manualOverrideLimit?: number;
  manualOverrideExpiresAt?: number;
  cacheTtlMs?: number;
  loadBalancingAlgorithm?:
    'ROUND_ROBIN' | 'WEIGHTED_ROUND_ROBIN' | 'LEAST_CONNECTIONS' | 'LEAST_RESPONSE_TIME';
  circuitBreakerEnabled?: boolean;
  circuitFailureThreshold?: number;
  circuitRecoveryTimeoutMs?: number;
  circuitHalfOpenProbes?: number;
  maxRetries?: number;
  retryBackoffMs?: number;
  retryMaxBackoffMs?: number;
  retryJitter?: boolean;
  retryStatuses?: number[];
  retryConnectionErrors?: boolean;
  retryTimeouts?: boolean;
  retryNonIdempotent?: boolean;
  enabled: boolean;
  upstreamGroups?: {
    id: string;
    name: string;
    percentage: number;
    targets: { url: string; weight: number }[];
  }[];
  serviceId: string;
  service: Service;
};
type Gateway = {
  id: string;
  name: string;
  lastSeenAt: string | null;
  currentVersion: number;
  latestVersion: number;
  synced: boolean;
  latestAcknowledgement: { status: string; message: string | null } | null;
};
type MetricPoint = {
  timestamp: number;
  requests: number;
  averageResponseMs: number;
  errors: number;
  cacheHits: number;
  cacheMisses: number;
};
type Metrics = {
  totalRequests: number;
  requestsPerSecond: number;
  averageResponseMs: number;
  p50ResponseMs: number;
  p95ResponseMs: number;
  p99ResponseMs: number;
  errors: number;
  errorRate: number;
  clientErrors: number;
  expectedClientErrors: number;
  routeTraffic: Record<string, number>;
  routeErrors: Record<string, number>;
  cacheHits: number;
  cacheMisses: number;
  cacheHitRate: number;
  statusCodes: Record<string, number>;
  timeseries: MetricPoint[];
  retryAttempts: number;
  retrySuccesses: number;
  retryExhausted: number;
  circuitOpenRejections: number;
  upstreams: { upstreams: UpstreamReliability[]; circuitEvents: CircuitEvent[] };
};
type UpstreamReliability = {
  routeId: string;
  serviceId: string;
  upstream: string;
  algorithm: string;
  circuitState: 'CLOSED' | 'OPEN' | 'HALF-OPEN';
  failures: number;
  requests: number;
  failureRate: number;
  consecutiveFailures: number;
  activeConnections: number;
  averageResponseMs: number;
  lastTransition: string | null;
};
type CircuitEvent = {
  at: string;
  routeId: string;
  upstream: string;
  from: string;
  to: string;
  failures: number;
  reason: string;
};
type AdaptivePoint = {
  timestamp: number;
  incomingRps: number;
  baselineRps: number;
  effectiveLimit: number;
  throttled: number;
};
type AdaptiveRoute = {
  routeId: string;
  path: string;
  mode: string;
  enabled: boolean;
  baselineReliable: boolean;
  baseLimit: number;
  effectiveLimit: number;
  minimumLimit: number;
  maximumLimit: number;
  baselineRps: number;
  incomingRps: number;
  spikesDetected: number;
  throttledRequests: number;
  lastAdaptationTime: string | null;
  recoveryStatus: string;
  manualOverrideExpiresAt: number;
  timeseries: AdaptivePoint[];
};
type AdaptiveDecisionEvent = {
  routeId: string;
  type: string;
  at: string;
  explanation: string;
  effectiveLimit: number;
};
type AdaptivePayload = { routes: AdaptiveRoute[]; decisions: AdaptiveDecisionEvent[] };
type CurrentUser = { id: string; username: string; role: 'ADMIN' | 'VIEWER' };
type AuditRecord = {
  id: number;
  actorUsername: string;
  action: string;
  resource: string;
  changes: Record<string, unknown>;
  createdAt: number;
};

function LoginScreen({
  onLogin,
  notice,
}: {
  onLogin: (token: string, user: CurrentUser) => void;
  notice: string;
}) {
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [needsAdmin, setNeedsAdmin] = useState(false);
  useEffect(() => {
    fetch('/api/setup/status')
      .then((r) => r.json())
      .then((v) => setNeedsAdmin(Boolean(v.needsInitialAdmin)))
      .catch(() => {});
  }, []);
  return (
    <main className="auth-screen">
      <form
        className="panel auth-card"
        onSubmit={(event) => {
          event.preventDefault();
          const form = new FormData(event.currentTarget);
          setLoading(true);
          setError('');
          void fetch('/api/auth/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              username: form.get('username'),
              password: form.get('password'),
            }),
          })
            .then(async (r) => {
              const v = await r.json();
              if (!r.ok) throw new Error(v.message ?? 'Sign in failed');
              onLogin(v.accessToken as string, v.user as CurrentUser);
            })
            .catch((e: unknown) => setError(e instanceof Error ? e.message : 'Sign in failed'))
            .finally(() => setLoading(false));
        }}
      >
        <span className="brand-mark">G</span>
        <p className="eyebrow">GATEKEEPER CONTROL PLANE</p>
        <h1>Sign in</h1>
        <p>Manage routes and inspect live gateway activity.</p>
        {notice && <div className="alert success">{notice}</div>}
        {error && <div className="alert error">{error}</div>}
        {needsAdmin && (
          <div className="alert">
            No administrator exists. Run <code>./scripts/create-admin.ps1</code> in PowerShell.
          </div>
        )}
        <label>
          Username
          <input name="username" autoComplete="username" required />
        </label>
        <label>
          Password
          <input name="password" type="password" autoComplete="current-password" required />
        </label>
        <button className="primary" disabled={loading}>
          {loading ? 'Signing in?' : 'Sign in'}
        </button>
      </form>
    </main>
  );
}

function AccessControl({
  api,
  run,
}: {
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
  run: (fn: () => Promise<void>) => Promise<void>;
}) {
  const [users, setUsers] = useState<CurrentUser[]>([]);
  const [error, setError] = useState('');
  const load = useCallback(
    () =>
      api<CurrentUser[]>('/api/access/users')
        .then(setUsers)
        .catch((e: unknown) =>
          setError(e instanceof Error ? e.message : 'Could not load accounts'),
        ),
    [api],
  );
  useEffect(() => {
    void load();
  }, [load]);
  return (
    <section className="panel">
      <div className="panel-head">
        <div>
          <h2>Accounts</h2>
          <p>Create administrator and read-only viewer accounts.</p>
        </div>
      </div>
      {error && <div className="alert error">{error}</div>}
      <form
        className="form-grid"
        onSubmit={(event) => {
          event.preventDefault();
          const f = new FormData(event.currentTarget);
          const form = event.currentTarget;
          void run(async () => {
            await api('/api/access/users', {
              method: 'POST',
              body: JSON.stringify({
                username: f.get('username'),
                password: f.get('password'),
                role: f.get('role'),
              }),
            });
            form.reset();
            await load();
          });
        }}
      >
        <label>
          Username
          <input name="username" minLength={3} maxLength={64} required />
        </label>
        <label>
          Temporary password
          <input name="password" type="password" minLength={12} maxLength={128} required />
        </label>
        <label>
          Role
          <select name="role">
            <option>VIEWER</option>
            <option>ADMIN</option>
          </select>
        </label>
        <button className="primary">Create account</button>
      </form>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Username</th>
              <th>Role</th>
              <th>Created</th>
              <th>Change role</th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.id}>
                <td>{u.username}</td>
                <td>{u.role}</td>
                <td>
                  {'createdAt' in u
                    ? new Date(
                        Number((u as CurrentUser & { createdAt: number }).createdAt),
                      ).toLocaleString()
                    : ''}
                </td>
                <td>
                  <select
                    value={u.role}
                    onChange={(e) =>
                      void run(async () => {
                        await api(`/api/access/users/${u.id}`, {
                          method: 'PATCH',
                          body: JSON.stringify({ role: e.target.value }),
                        });
                        await load();
                      })
                    }
                  >
                    <option>VIEWER</option>
                    <option>ADMIN</option>
                  </select>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {users.length === 0 && <Empty>No accounts found.</Empty>}
      </div>
    </section>
  );
}

function AuditPage({ api }: { api: <T>(path: string, init?: RequestInit) => Promise<T> }) {
  const [events, setEvents] = useState<AuditRecord[]>([]);
  const [error, setError] = useState('');
  const load = useCallback(
    () =>
      api<AuditRecord[]>('/api/access/audit')
        .then(setEvents)
        .catch((e: unknown) =>
          setError(e instanceof Error ? e.message : 'Could not load audit history'),
        ),
    [api],
  );
  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 10000);
    return () => window.clearInterval(timer);
  }, [load]);
  return (
    <section className="panel">
      <div className="panel-head">
        <div>
          <h2>Administrative history</h2>
          <p>Recent configuration and account changes.</p>
        </div>
        <button className="secondary" onClick={() => void load()}>
          Refresh
        </button>
      </div>
      {error && <div className="alert error">{error}</div>}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Actor</th>
              <th>Action</th>
              <th>Resource</th>
              <th>Changes</th>
            </tr>
          </thead>
          <tbody>
            {events.map((event) => (
              <tr key={event.id}>
                <td>{new Date(event.createdAt).toLocaleString()}</td>
                <td>{event.actorUsername}</td>
                <td>{event.action}</td>
                <td>{event.resource}</td>
                <td>
                  <code>{JSON.stringify(event.changes)}</code>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {events.length === 0 && <Empty>No administrative changes recorded.</Empty>}
      </div>
    </section>
  );
}

function App() {
  const [page, setPage] = useState('overview');
  const [services, setServices] = useState<Service[]>([]);
  const [routes, setRoutes] = useState<Route[]>([]);
  const [gateways, setGateways] = useState<Gateway[]>([]);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [authToken, setAuthToken] = useState(
    () => sessionStorage.getItem('gatekeeper.accessToken') ?? '',
  );
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);
  const [authChecking, setAuthChecking] = useState(() =>
    Boolean(sessionStorage.getItem('gatekeeper.accessToken')),
  );
  const [loginNotice, setLoginNotice] = useState('');

  const clearSession = useCallback((message = '') => {
    sessionStorage.removeItem('gatekeeper.accessToken');
    setAuthToken('');
    setCurrentUser(null);
    setAuthChecking(false);
    setLoginNotice(message);
  }, []);

  useEffect(() => {
    if (!authToken) {
      setCurrentUser(null);
      setAuthChecking(false);
      return;
    }
    let active = true;
    void fetch('/api/auth/me', { headers: { Authorization: `Bearer ${authToken}` } })
      .then(async (response) => {
        const value = await response.json().catch(() => ({}));
        if (!response.ok)
          throw new Error(value.message ?? 'Session expired. Please sign in again.');
        if (active) setCurrentUser(value as CurrentUser);
      })
      .catch(() => {
        if (active) clearSession('Your session expired. Sign in again.');
      })
      .finally(() => {
        if (active) setAuthChecking(false);
      });
    return () => {
      active = false;
    };
  }, [authToken, clearSession]);

  const api = useCallback(
    async <T,>(path: string, init: RequestInit = {}): Promise<T> => {
      const response = await fetch(path, {
        ...init,
        headers: {
          ...(init.body ? { 'Content-Type': 'application/json' } : {}),
          ...(authToken ? { Authorization: `Bearer ${authToken}` } : {}),
          ...init.headers,
        },
      });
      if (response.status === 204) return undefined as T;
      const data = await response.json().catch(() => ({}));
      if (response.status === 401 && authToken) {
        clearSession('Your session expired. Sign in again.');
        throw new Error('Session expired. Sign in again.');
      }
      if (!response.ok) throw new Error(data.error ?? `Request failed (${response.status})`);
      return data as T;
    },
    [authToken, clearSession],
  );

  const refresh = useCallback(async () => {
    const [nextServices, nextRoutes, nextGateways] = await Promise.all([
      api<Service[]>('/api/services'),
      api<Route[]>('/api/routes'),
      api<Gateway[]>('/api/gateways'),
    ]);
    setServices(nextServices);
    setRoutes(nextRoutes);
    setGateways(nextGateways);
    setError('');
  }, [api]);

  useEffect(() => {
    if (!currentUser) {
      setLoading(false);
      return;
    }
    setLoading(true);
    void refresh()
      .catch((reason: Error) => setError(reason.message))
      .finally(() => setLoading(false));
  }, [refresh, currentUser]);

  useEffect(() => {
    if (!currentUser) return;
    const timer = window.setInterval(() => {
      void refresh().catch((reason: Error) => setError(reason.message));
    }, 5000);
    return () => window.clearInterval(timer);
  }, [refresh, currentUser]);

  const handleLogin = (token: string, user: CurrentUser) => {
    sessionStorage.setItem('gatekeeper.accessToken', token);
    setAuthToken(token);
    setCurrentUser(user);
    setLoginNotice('');
    setError('');
    setPage('overview');
  };
  const handleLogout = async () => {
    try {
      await api('/api/auth/logout', { method: 'POST' });
    } catch {
      /* local sign-out still clears an expired token */
    }
    clearSession('You have signed out.');
  };

  const withAction = async (action: () => Promise<void>) => {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      await action();
      await refresh();
      setNotice('Changes saved. Gateway status is refreshing.');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Request failed');
    } finally {
      setBusy(false);
    }
  };

  if (authChecking) return <div className="auth-loading">Checking your Gatekeeper session…</div>;
  if (!currentUser) return <LoginScreen onLogin={handleLogin} notice={loginNotice} />;
  const isAdmin = currentUser.role === 'ADMIN';
  const navigation: [string, string][] = [
    ['overview', 'Overview'],
    ['services', 'Services'],
    ['routes', 'Routes'],
    ['analytics', 'Traffic Analytics'],
    ['health', 'Gateway Health'],
    ['circuits', 'Circuit Breakers'],
    ['alerts', 'Analytics & Alerts'],
    ['config-history', 'Configuration History'],
    ['inspector', 'Request Inspector'],
    ['settings', 'Settings'],
  ];
  if (isAdmin) navigation.splice(2, 0, ['upstreams', 'Upstreams']);
  if (isAdmin)
    navigation.push(
      ['canary', 'Canary Deployments'],
      ['adaptive', 'Rate Limiting'],
      ['gateways', 'Gateway nodes'],
      ['access', 'User Management'],
      ['audit', 'Audit log'],
    );

  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="brand">
          <span className="brand-mark">G</span>
          <div>
            <b>Gatekeeper</b>
            <small>Traffic control plane</small>
          </div>
        </div>
        <nav>
          {navigation.map(([key, label]) => (
            <button key={key} className={page === key ? 'active' : ''} onClick={() => setPage(key)}>
              {label}
            </button>
          ))}
        </nav>
        <div className="sidebar-foot">
          <span>
            {currentUser.username} · {currentUser.role}
          </span>
          <button className="secondary small" onClick={() => void handleLogout()}>
            Sign out
          </button>
        </div>
      </aside>
      <main className="content">
        <header className="topbar">
          <div>
            <p className="eyebrow">GATEKEEPER CONTROL PLANE</p>
            <h1>{pageTitle(page)}</h1>
          </div>
          <div className="topbar-actions">
            <span className="user-chip">
              {currentUser.username} · {currentUser.role}
            </span>
            <button
              className="secondary"
              disabled={busy}
              onClick={() =>
                void refresh()
                  .then(() => setNotice('Live data refreshed'))
                  .catch((reason: Error) => setError(reason.message))
              }
            >
              Refresh
            </button>
            <button className="secondary" onClick={() => void handleLogout()}>
              Logout
            </button>
          </div>
        </header>
        {error && <div className="alert error">{error}</div>}
        {notice && <div className="alert success">{notice}</div>}
        {loading ? (
          <div className="panel empty">Loading routes, services, and gateway status…</div>
        ) : (
          <>
            {page === 'overview' && (
              <Overview services={services} routes={routes} gateways={gateways} api={api} />
            )}
            {page === 'services' && (
              <Services
                services={services}
                busy={busy}
                run={withAction}
                api={api}
                readOnly={!isAdmin}
              />
            )}
            {page === 'upstreams' && (
              <Upstreams services={services} busy={busy} run={withAction} api={api} />
            )}
            {page === 'routes' && (
              <Routes
                routes={routes}
                services={services}
                busy={busy}
                run={withAction}
                api={api}
                readOnly={!isAdmin}
              />
            )}
            {page === 'gateways' && <Gateways gateways={gateways} api={api} run={withAction} />}
            {page === 'analytics' && <AnalyticsPanel routes={routes} api={api} />}
            {page === 'config-history' && (
              <ConfigurationHistory api={api} run={withAction} readOnly={!isAdmin} />
            )}
            {page === 'canary' && <CanaryDeployments routes={routes} api={api} run={withAction} />}
            {page === 'inspector' && <RequestInspector api={api} />}
            {page === 'alerts' && <AlertsPage routes={routes} api={api} readOnly={!isAdmin} />}
            {page === 'health' && <ReliabilityPanel api={api} />}
            {page === 'circuits' && <ReliabilityPanel api={api} />}
            {page === 'adaptive' && (
              <AdaptiveDashboard routes={routes} api={api} run={withAction} />
            )}
            {page === 'settings' && (
              <LivePanel title="Gateway configuration" endpoint="/api/gateway/config" api={api} />
            )}
            {page === 'access' && isAdmin && <AccessControl api={api} run={withAction} />}
            {page === 'audit' && isAdmin && <AuditPage api={api} />}
          </>
        )}
        <footer>
          Routes and services are stored in local SQLite. Traffic data is collected from the live
          gateway.
        </footer>
      </main>
    </div>
  );
}

type ConfigVersion = {
  version: number;
  actorUsername: string;
  summary: string;
  rollbackOf: number | null;
  createdAt: number;
  active: boolean;
};
function ConfigurationHistory({
  api,
  run,
  readOnly,
}: {
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
  run: (fn: () => Promise<void>) => Promise<void>;
  readOnly: boolean;
}) {
  const [versions, setVersions] = useState<ConfigVersion[]>([]);
  const [activeVersion, setActiveVersion] = useState(0);
  const [detail, setDetail] = useState<unknown>(null);
  const [comparison, setComparison] = useState<unknown>(null);
  const [error, setError] = useState('');
  const load = useCallback(
    () =>
      api<{ activeVersion: number; versions: ConfigVersion[] }>('/api/config/history')
        .then((data) => {
          setVersions(data.versions);
          setActiveVersion(data.activeVersion);
        })
        .catch((e: Error) => setError(e.message)),
    [api],
  );
  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 10000);
    return () => window.clearInterval(timer);
  }, [load]);
  return (
    <section className="panel">
      <div className="panel-head">
        <div>
          <h2>Configuration versions</h2>
          <p>Validated snapshots persisted with the administrator and change summary.</p>
        </div>
      </div>
      {error && <div className="alert error">{error}</div>}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Version</th>
              <th>Created</th>
              <th>Administrator</th>
              <th>Summary</th>
              <th>Status</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {versions.map((v) => (
              <tr key={v.version}>
                <td>v{v.version}</td>
                <td>{new Date(v.createdAt).toLocaleString()}</td>
                <td>{v.actorUsername}</td>
                <td>{v.summary}</td>
                <td>
                  {v.version === activeVersion
                    ? 'Active'
                    : v.rollbackOf != null
                      ? `Rollback of v${v.rollbackOf}`
                      : 'Inactive'}
                </td>
                <td>
                  <button
                    className="secondary small"
                    onClick={() =>
                      void api(`/api/config/history/${v.version}`)
                        .then(setDetail)
                        .catch((e: Error) => setError(e.message))
                    }
                  >
                    View
                  </button>{' '}
                  <button
                    className="secondary small"
                    onClick={() =>
                      void api(`/api/config/history/compare?from=${activeVersion}&to=${v.version}`)
                        .then(setComparison)
                        .catch((e: Error) => setError(e.message))
                    }
                  >
                    Compare
                  </button>{' '}
                  {!readOnly && v.version !== activeVersion && (
                    <button
                      className="danger small"
                      onClick={() => {
                        if (window.confirm(`Roll back to configuration v${v.version}?`))
                          void run(async () => {
                            await api(`/api/config/rollback/${v.version}`, { method: 'POST' });
                            await load();
                          });
                      }}
                    >
                      Rollback
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {detail !== null && <pre className="json-detail">{JSON.stringify(detail, null, 2)}</pre>}
      {comparison !== null && (
        <pre className="json-detail">{JSON.stringify(comparison, null, 2)}</pre>
      )}
      {versions.length === 0 && (
        <div className="empty">No configuration versions have been published.</div>
      )}
    </section>
  );
}

function CanaryDeployments({
  routes,
  api,
  run,
}: {
  routes: Route[];
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
  run: (fn: () => Promise<void>) => Promise<void>;
}) {
  const [percentages, setPercentages] = useState<Record<string, number>>({});
  const [selectedUpstream, setSelectedUpstream] = useState<Record<string, string>>({});
  const [metrics, setMetrics] = useState<Metrics | null>(null);
  const [requestRecords, setRequestRecords] = useState<InspectedRequest[]>([]);
  useEffect(() => {
    const load = () =>
      void Promise.all([
        api<Metrics>('/api/gateway/metrics'),
        api<InspectedRequest[]>('/api/gateway/requests?limit=200'),
      ])
        .then(([live, requests]) => {
          setMetrics(live);
          setRequestRecords(requests);
        })
        .catch(() => {});
    load();
    const timer = window.setInterval(load, 5000);
    return () => window.clearInterval(timer);
  }, [api]);
  const save = (route: Route, canaryPct: number, promote = false, rollback = false) =>
    run(async () => {
      const registered =
        route.service?.upstreams
          ?.filter((u) => u.enabled)
          .map(({ url, weight }) => ({ url, weight })) ?? [];
      const canaryUrl =
        selectedUpstream[route.id] ??
        route.upstreamGroups?.find((group) => group.id === 'canary')?.targets[0]?.url ??
        registered[1]?.url;
      const canary = registered.filter((target) => target.url === canaryUrl);
      const stable =
        route.upstreamGroups?.find((group) => group.id === 'stable')?.targets ??
        registered.filter((target) => target.url !== canaryUrl);
      if (
        !stable.length ||
        !canary.length ||
        stable.some((target) => canary.some((candidate) => candidate.url === target.url))
      )
        throw new Error(
          'Register at least two distinct enabled upstream instances, then choose one for canary traffic.',
        );
      const promoted =
        route.upstreamGroups?.find((group) => group.id === 'stable')?.name ===
        'Stable (promoted canary)';
      const groups = promote
        ? [
            { id: 'stable', name: 'Stable (promoted canary)', percentage: 100, targets: canary },
            { id: 'canary', name: 'Canary', percentage: 0, targets: stable },
          ]
        : rollback && promoted
          ? [
              { id: 'stable', name: 'Stable', percentage: 100, targets: canary },
              { id: 'canary', name: 'Canary', percentage: 0, targets: stable },
            ]
          : [
              { id: 'stable', name: 'Stable', percentage: 100 - canaryPct, targets: stable },
              { id: 'canary', name: 'Canary', percentage: canaryPct, targets: canary },
            ];
      await api(`/api/routes/${route.id}`, {
        method: 'PUT',
        body: JSON.stringify({ ...route, upstreamGroups: groups }),
      });
      setPercentages((current) => {
        const next = { ...current };
        delete next[route.id];
        return next;
      });
      setSelectedUpstream((current) => {
        const next = { ...current };
        delete next[route.id];
        return next;
      });
    });
  const routed = routes.filter((r) => r.upstreamGroups?.length);
  return (
    <>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Canary traffic splitting</h2>
            <p>
              Traffic is assigned deterministically by route. A group without healthy targets falls
              back to the next healthy group.
            </p>
          </div>
        </div>
        {routes
          .filter((r) => r.enabled)
          .map((route) => {
            const pct =
              percentages[route.id] ??
              route.upstreamGroups?.find((g) => g.id === 'canary')?.percentage ??
              0;
            const traffic =
              metrics?.timeseries?.reduce((sum, point) => sum + point.requests, 0) ?? 0;
            const groupStats = (groupId: string) => {
              const records = requestRecords.filter(
                (record) => record.routeId === route.id && record.groupId === groupId,
              );
              const errors = records.filter((record) => record.status >= 500).length;
              const avg = records.length
                ? records.reduce((sum, record) => sum + record.durationMs, 0) / records.length
                : 0;
              return `${records.length} requests · ${records.length ? ((errors / records.length) * 100).toFixed(1) : '0.0'}% errors · ${avg.toFixed(2)} ms avg`;
            };
            return (
              <div className="canary-row" key={route.id}>
                <div>
                  <b>{route.path}</b>
                  <small>
                    {route.id} ·{' '}
                    {route.upstreamGroups?.length
                      ? pct === 0
                        ? 'Stable only'
                        : pct === 100
                          ? 'Canary at 100%'
                          : 'Active rollout'
                      : 'No split configured'}{' '}
                    · {traffic} gateway requests in current window
                  </small>
                  {route.upstreamGroups?.map((group) => (
                    <small key={group.id}>
                      {group.name} {group.percentage}%: {groupStats(group.id)}
                    </small>
                  ))}
                </div>
                <label>
                  Canary upstream
                  <select
                    value={
                      selectedUpstream[route.id] ??
                      route.upstreamGroups?.find((group) => group.id === 'canary')?.targets[0]
                        ?.url ??
                      route.service?.upstreams?.filter((upstream) => upstream.enabled)[1]?.url ??
                      ''
                    }
                    onChange={(event) =>
                      setSelectedUpstream({ ...selectedUpstream, [route.id]: event.target.value })
                    }
                  >
                    <option value="">Choose registered upstream</option>
                    {route.service?.upstreams
                      ?.filter((upstream) => upstream.enabled)
                      .map((upstream) => (
                        <option key={upstream.id} value={upstream.url}>
                          {upstream.url}
                        </option>
                      ))}
                  </select>
                </label>
                <label>
                  Canary %{' '}
                  <input
                    type="number"
                    min="0"
                    max="100"
                    value={pct}
                    onChange={(e) =>
                      setPercentages({ ...percentages, [route.id]: Number(e.target.value) })
                    }
                  />
                </label>
                <button
                  className="primary small"
                  onClick={() => void save(route, Math.max(0, Math.min(100, pct)))}
                >
                  Apply split
                </button>
                <button
                  className="secondary small"
                  disabled={!route.upstreamGroups?.length}
                  onClick={() => void save(route, 0, false, true)}
                >
                  Pause / stable only
                </button>
                <button
                  className="secondary small"
                  disabled={pct === 0}
                  onClick={() => void save(route, 0, true)}
                >
                  Promote canary
                </button>
                <button className="secondary small" onClick={() => void save(route, 0)}>
                  Rollback
                </button>
              </div>
            );
          })}
        {!routes.length && (
          <div className="empty">Create an enabled route before configuring canary traffic.</div>
        )}
      </section>
      <section className="panel">
        <h2>Group configuration</h2>
        {routed.map((route) => (
          <div key={route.id}>
            <h3>{route.path}</h3>
            <pre className="json-detail">{JSON.stringify(route.upstreamGroups, null, 2)}</pre>
          </div>
        ))}
        {!routed.length && <div className="empty">No canary groups are configured.</div>}
      </section>
    </>
  );
}

type InspectedRequest = {
  timestamp: string;
  requestId: string;
  method: string;
  path: string;
  routeId?: string;
  upstream?: string;
  groupId?: string;
  status: number;
  durationMs: number;
  cache: string;
  retryCount?: number;
  circuitDecision?: string;
  rateLimitDecision?: string;
  upstreamDurationMs?: number;
};
type AlertEntry = {
  id: string;
  kind: string;
  message: string;
  related: string | null;
  active: boolean;
  acknowledgedAt: number | null;
  createdAt: number;
  resolvedAt: number | null;
};
type AlertRules = { errorRate: number; p95LatencyMs: number; trafficSpikeMultiplier: number };
function AlertsPage({
  routes,
  api,
  readOnly,
}: {
  routes: Route[];
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
  readOnly: boolean;
}) {
  const [data, setData] = useState<{ rules: AlertRules; alerts: AlertEntry[] } | null>(null);
  const [metrics, setMetrics] = useState<Metrics | null>(null);
  const [error, setError] = useState('');
  const load = useCallback(async () => {
    try {
      const [alerts, live] = await Promise.all([
        api<{ rules: AlertRules; alerts: AlertEntry[] }>('/api/alerts'),
        api<Metrics>('/api/gateway/metrics'),
      ]);
      setData(alerts);
      setMetrics(live);
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not load live analytics and alerts');
    }
  }, [api]);
  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(timer);
  }, [load]);
  const saveRules = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    void api('/api/alerts/rules', {
      method: 'PUT',
      body: JSON.stringify({
        errorRate: Number(form.get('errorRate')) / 100,
        p95LatencyMs: Number(form.get('p95LatencyMs')),
        trafficSpikeMultiplier: Number(form.get('trafficSpikeMultiplier')),
      }),
    })
      .then(load)
      .catch((e: Error) => setError(e.message));
  };
  return (
    <>
      <section className="stats">
        {[
          ['P50 latency', metrics?.p50ResponseMs],
          ['P95 latency', metrics?.p95ResponseMs],
          ['P99 latency', metrics?.p99ResponseMs],
          ['Requests / sec', metrics?.requestsPerSecond],
          ['Error rate', metrics ? `${(metrics.errorRate * 100).toFixed(2)}%` : undefined],
          ['Cache hit ratio', metrics ? `${(metrics.cacheHitRate * 100).toFixed(2)}%` : undefined],
        ].map(([label, value]) => (
          <article className="stat" key={String(label)}>
            <span>{label}</span>
            <b>{typeof value === 'number' ? `${value.toFixed(2)} ms` : (value ?? '—')}</b>
          </article>
        ))}
      </section>
      {error && <div className="alert error">{error}</div>}
      <section className="panel">
        <h2>Route traffic</h2>
        {routes.length ? (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Route</th>
                  <th>Requests (recent sample)</th>
                  <th>5xx responses</th>
                  <th>Server error rate</th>
                </tr>
              </thead>
              <tbody>
                {routes.map((route) => {
                  const requests = metrics?.routeTraffic?.[route.id] ?? 0;
                  const errors = metrics?.routeErrors?.[route.id] ?? 0;
                  return (
                    <tr key={route.id}>
                      <td>
                        {route.path}
                        <small>{route.id}</small>
                      </td>
                      <td>{requests}</td>
                      <td>{errors}</td>
                      <td>{requests ? `${((errors / requests) * 100).toFixed(2)}%` : '—'}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="empty">No routes configured.</div>
        )}
      </section>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Alert thresholds</h2>
            <p>
              Alerts are evaluated from live gateway metrics and upstream health during polling.
            </p>
          </div>
        </div>
        {data && (
          <form className="form-grid" onSubmit={saveRules}>
            <label>
              Error rate threshold (%)
              <input
                name="errorRate"
                type="number"
                min="0.1"
                max="100"
                step="0.1"
                defaultValue={data.rules.errorRate * 100}
                disabled={readOnly}
              />
            </label>
            <label>
              P95 latency threshold (ms)
              <input
                name="p95LatencyMs"
                type="number"
                min="1"
                max="300000"
                defaultValue={data.rules.p95LatencyMs}
                disabled={readOnly}
              />
            </label>
            <label>
              Traffic spike multiplier
              <input
                name="trafficSpikeMultiplier"
                type="number"
                min="1.1"
                max="10"
                step="0.1"
                defaultValue={data.rules.trafficSpikeMultiplier}
                disabled={readOnly}
              />
            </label>
            {!readOnly && <button className="primary">Save thresholds</button>}
          </form>
        )}
      </section>
      <section className="panel">
        <h2>Alert history</h2>
        {data?.alerts.length ? (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Alert</th>
                  <th>Details</th>
                  <th>Related</th>
                  <th>State</th>
                  <th>Action</th>
                </tr>
              </thead>
              <tbody>
                {data.alerts.map((item) => (
                  <tr key={item.id}>
                    <td>{new Date(item.createdAt).toLocaleString()}</td>
                    <td>{item.kind.replaceAll('_', ' ')}</td>
                    <td>{item.message}</td>
                    <td>{item.related ?? '—'}</td>
                    <td>
                      {item.active
                        ? item.acknowledgedAt
                          ? 'Acknowledged'
                          : 'Active'
                        : `Resolved ${item.resolvedAt ? new Date(item.resolvedAt).toLocaleString() : ''}`}
                    </td>
                    <td>
                      {item.active && !item.acknowledgedAt && !readOnly && (
                        <button
                          className="secondary small"
                          onClick={() =>
                            void api(`/api/alerts/${item.id}/acknowledge`, { method: 'POST' })
                              .then(load)
                              .catch((e: Error) => setError(e.message))
                          }
                        >
                          Acknowledge
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="empty">No alert conditions have been observed.</div>
        )}
      </section>
    </>
  );
}

function RequestInspector({ api }: { api: <T>(path: string, init?: RequestInit) => Promise<T> }) {
  const [items, setItems] = useState<InspectedRequest[]>([]);
  const [filter, setFilter] = useState('');
  const [routeFilter, setRouteFilter] = useState('');
  const [statusFilter, setStatusFilter] = useState('');
  const [upstreamFilter, setUpstreamFilter] = useState('');
  const [error, setError] = useState('');
  const [selected, setSelected] = useState<InspectedRequest | null>(null);
  const load = useCallback(
    () =>
      api<InspectedRequest[]>(
        `/api/gateway/requests?limit=200${filter ? `&requestId=${encodeURIComponent(filter)}` : ''}${routeFilter ? `&route=${encodeURIComponent(routeFilter)}` : ''}${statusFilter ? `&status=${encodeURIComponent(statusFilter)}` : ''}${upstreamFilter ? `&upstream=${encodeURIComponent(upstreamFilter)}` : ''}`,
      )
        .then(setItems)
        .catch((e: Error) => setError(e.message)),
    [api, filter, routeFilter, statusFilter, upstreamFilter],
  );
  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 3000);
    return () => window.clearInterval(timer);
  }, [load]);
  return (
    <section className="panel">
      <div className="panel-head">
        <div>
          <h2>Recent gateway requests</h2>
          <p>
            Sanitized metadata retained in a bounded in-memory ring. Request bodies and credentials
            are never stored.
          </p>
        </div>
        <input
          aria-label="Filter by request ID"
          placeholder="Filter request ID"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        <input
          aria-label="Filter by route"
          placeholder="Route ID"
          value={routeFilter}
          onChange={(e) => setRouteFilter(e.target.value)}
        />
        <input
          aria-label="Filter by status"
          placeholder="Status"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
        />
        <input
          aria-label="Filter by upstream"
          placeholder="Upstream"
          value={upstreamFilter}
          onChange={(e) => setUpstreamFilter(e.target.value)}
        />
      </div>
      {error && <div className="alert error">{error}</div>}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Request ID</th>
              <th>Request</th>
              <th>Route</th>
              <th>Upstream</th>
              <th>Status</th>
              <th>Latency</th>
              <th>Cache</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.requestId}>
                <td>{new Date(item.timestamp).toLocaleTimeString()}</td>
                <td>
                  <code>{item.requestId}</code>
                </td>
                <td>
                  {item.method} {item.path}
                </td>
                <td>{item.routeId ?? '—'}</td>
                <td>{item.upstream ?? '—'}</td>
                <td>
                  <span className={`state-pill ${item.status >= 500 ? 'open' : 'closed'}`}>
                    {item.status}
                  </span>
                </td>
                <td>{item.durationMs.toFixed(2)} ms</td>
                <td>{item.cache}</td>
                <td>
                  <button
                    className="secondary small"
                    onClick={() =>
                      void api<InspectedRequest>(
                        `/api/gateway/requests/${encodeURIComponent(item.requestId)}`,
                      )
                        .then(setSelected)
                        .catch((e: Error) => setError(e.message))
                    }
                  >
                    Details
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {selected && (
        <div className="panel nested">
          <div className="panel-head">
            <h3>Request {selected.requestId}</h3>
            <button className="secondary small" onClick={() => setSelected(null)}>
              Close
            </button>
          </div>
          <dl className="detail-grid">
            {[
              ['Route', selected.routeId ?? 'unmatched'],
              ['Status', selected.status],
              ['Total duration', `${selected.durationMs.toFixed(2)} ms`],
              ['Upstream duration', `${(selected.upstreamDurationMs ?? 0).toFixed(2)} ms`],
              ['Retry count', selected.retryCount ?? 0],
              ['Circuit decision', selected.circuitDecision ?? '—'],
              ['Rate-limit decision', selected.rateLimitDecision ?? '—'],
              ['Cache', selected.cache],
            ].map(([label, value]) => (
              <div key={String(label)}>
                <dt>{label}</dt>
                <dd>{value}</dd>
              </div>
            ))}
          </dl>
        </div>
      )}
      {!items.length && !error && (
        <div className="empty">No gateway requests have been recorded yet.</div>
      )}
    </section>
  );
}

function Overview({
  services,
  routes,
  gateways,
  api,
}: {
  services: Service[];
  routes: Route[];
  gateways: Gateway[];
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
}) {
  const enabled = routes.filter((route) => route.enabled).length;
  const [metrics, setMetrics] = useState<Metrics | null>(null);
  const [health, setHealth] = useState<Record<string, boolean> | null>(null);
  const [loadError, setLoadError] = useState('');
  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const [nextMetrics, nextHealth] = await Promise.all([
          api<Metrics>('/api/gateway/metrics'),
          api<Record<string, boolean>>('/api/gateway/health'),
        ]);
        if (active) {
          setMetrics(nextMetrics as Metrics);
          setHealth(nextHealth as Record<string, boolean>);
          setLoadError('');
        }
      } catch (reason) {
        if (active)
          setLoadError(reason instanceof Error ? reason.message : 'Could not load gateway data');
      }
    };
    void load();
    const timer = window.setInterval(() => void load(), 5000);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [api]);
  const metricValue = (value: number | string) => (metrics ? value : '—');
  const healthyCount = health ? Object.values(health).filter(Boolean).length : null;
  return (
    <>
      <section className="stats">
        {[
          ['Total requests', metricValue(metrics?.totalRequests ?? 0)],
          ['Requests / sec', metricValue((metrics?.requestsPerSecond ?? 0).toFixed(2))],
          ['Average response', metricValue(`${(metrics?.averageResponseMs ?? 0).toFixed(2)} ms`)],
          ['Error count', metricValue(metrics?.errors ?? 0)],
          ['Error rate', metricValue(`${((metrics?.errorRate ?? 0) * 100).toFixed(1)}%`)],
          ['Cache hit rate', metricValue(`${((metrics?.cacheHitRate ?? 0) * 100).toFixed(1)}%`)],
          ['Expected client errors', metricValue(metrics?.expectedClientErrors ?? 0)],
          [
            'Cache hits / misses',
            metricValue(`${metrics?.cacheHits ?? 0} / ${metrics?.cacheMisses ?? 0}`),
          ],
          ['Active routes', enabled],
          ['Backend health', health ? `${healthyCount} / ${Object.keys(health).length}` : '—'],
          ['Services', services.length],
        ].map(([label, value]) => (
          <article className="stat" key={label}>
            <span>{label}</span>
            <b>{value}</b>
          </article>
        ))}
      </section>
      {loadError && <div className="alert error">{loadError}</div>}
      <MetricsCharts metrics={metrics} health={health} routes={routes} />
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Gateway synchronization</h2>
            <p>Applied configuration version by registered gateway</p>
          </div>
        </div>
        <GatewayTable gateways={gateways} />
      </section>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Recent routes</h2>
            <p>Routes currently stored in the control plane</p>
          </div>
        </div>
        <RouteTable routes={routes.slice(0, 5)} />
      </section>
    </>
  );
}

function MetricsCharts({
  metrics,
  health,
  routes,
}: {
  metrics: Metrics | null;
  health: Record<string, boolean> | null;
  routes: Route[];
}) {
  const points = metrics?.timeseries ?? [];
  const line = (key: 'requests' | 'averageResponseMs') => {
    if (!points.length) return '';
    const vals = points.map((point) => point[key]);
    const max = Math.max(1, ...vals);
    return vals
      .map((value, i) => `${(i / Math.max(1, vals.length - 1)) * 100},${92 - (value / max) * 82}`)
      .join(' ');
  };
  const codes = Object.entries(metrics?.statusCodes ?? {}).sort(
    ([a], [b]) => Number(a) - Number(b),
  );
  const upstreams = routes.flatMap((route) =>
    (route.service?.upstreams ?? []).map((item, i) => ({
      route: route.path,
      name: route.service.name,
      item,
      key: `${route.id}:${i}`,
    })),
  );
  return (
    <section className="observability-grid">
      {(
        [
          ['Request volume', 'requests'],
          ['Average response time', 'averageResponseMs'],
        ] as const
      ).map(([title, key]) => (
        <article className="panel chart-panel" key={key}>
          <div className="panel-head">
            <div>
              <h2>{title}</h2>
              <p>Recent live gateway samples</p>
            </div>
          </div>
          {points.length ? (
            <svg
              className="metric-chart"
              viewBox="0 0 100 100"
              preserveAspectRatio="none"
              role="img"
              aria-label={title}
            >
              <polyline points={line(key)} />
            </svg>
          ) : (
            <div className="chart-empty">Waiting for gateway traffic</div>
          )}
          <small>
            {points.length
              ? `${points.length} seconds sampled · latest ${key === 'requests' ? (points.at(-1)?.requests ?? 0) : `${(points.at(-1)?.averageResponseMs ?? 0).toFixed(2)} ms`}`
              : 'No samples yet'}
          </small>
        </article>
      ))}
      <article className="panel">
        <div className="panel-head">
          <div>
            <h2>Status codes</h2>
            <p>Responses observed by the gateway</p>
          </div>
        </div>
        {codes.length ? (
          <div className="status-list">
            {codes.map(([code, count]) => (
              <div key={code}>
                <span className={`status-code status-${code[0]}`}>{code}</span>
                <span>{count}</span>
              </div>
            ))}
          </div>
        ) : (
          <div className="chart-empty">No responses recorded</div>
        )}
      </article>
      <article className="panel">
        <div className="panel-head">
          <div>
            <h2>Upstream health</h2>
            <p>Latest gateway probe status</p>
          </div>
        </div>
        {!health ? (
          <div className="chart-empty">Health data unavailable</div>
        ) : upstreams.length ? (
          <div className="health-list">
            {upstreams.map(({ route, name, item, key }) => (
              <div key={key}>
                <span>
                  <b>{name}</b>
                  <small>
                    {route} · {item.url}
                  </small>
                </span>
                <i className={health[key] ? 'healthy' : 'unhealthy'}>
                  {health[key] ? 'Healthy' : 'Unhealthy'}
                </i>
              </div>
            ))}
          </div>
        ) : (
          <div className="chart-empty">No upstreams configured</div>
        )}
      </article>
    </section>
  );
}

function AnalyticsPanel({
  routes,
  api,
}: {
  routes: Route[];
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
}) {
  const [metrics, setMetrics] = useState<Metrics | null>(null);
  const [health, setHealth] = useState<Record<string, boolean> | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let active = true;
    const load = async () => {
      try {
        const [m, h] = await Promise.all([
          api<Metrics>('/api/gateway/metrics'),
          api<Record<string, boolean>>('/api/gateway/health'),
        ]);
        if (active) {
          setMetrics(m as Metrics);
          setHealth(h as Record<string, boolean>);
          setError('');
        }
      } catch (reason) {
        if (active)
          setError(reason instanceof Error ? reason.message : 'Could not load live analytics');
      }
    };
    void load();
    const timer = window.setInterval(() => void load(), 5000);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [api]);
  return (
    <>
      {error && <div className="alert error">{error}</div>}
      <MetricsCharts metrics={metrics} health={health} routes={routes} />
    </>
  );
}

function ReliabilityPanel({ api }: { api: <T>(path: string, init?: RequestInit) => Promise<T> }) {
  const [metrics, setMetrics] = useState<Metrics | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let active = true;
    const load = () =>
      void api<Metrics>('/api/gateway/metrics')
        .then((value) => {
          if (active) {
            setMetrics(value);
            setError('');
          }
        })
        .catch((reason: Error) => {
          if (active) setError(reason.message);
        });
    load();
    const timer = window.setInterval(load, 5000);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [api]);
  const upstreams = metrics?.upstreams?.upstreams ?? [];
  const events = metrics?.upstreams?.circuitEvents ?? [];
  return (
    <>
      {error && <div className="alert error">{error}</div>}
      <section className="stats">
        {[
          ['Retry attempts', metrics?.retryAttempts ?? '—'],
          ['Retry recoveries', metrics?.retrySuccesses ?? '—'],
          ['Retry exhaustion', metrics?.retryExhausted ?? '—'],
          ['Open circuit rejections', metrics?.circuitOpenRejections ?? '—'],
        ].map(([label, value]) => (
          <article className="stat" key={label}>
            <span>{label}</span>
            <b>{value}</b>
          </article>
        ))}
      </section>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Upstream reliability</h2>
            <p>Live circuit state, active requests and response time by route target.</p>
          </div>
        </div>
        {upstreams.length ? (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Route / upstream</th>
                  <th>Algorithm</th>
                  <th>Circuit</th>
                  <th>Requests / failures</th>
                  <th>Failures</th>
                  <th>Active</th>
                  <th>Avg response</th>
                  <th>Last transition</th>
                </tr>
              </thead>
              <tbody>
                {upstreams.map((item) => (
                  <tr key={`${item.routeId}|${item.upstream}`}>
                    <td>
                      {item.routeId}
                      <small>{item.upstream}</small>
                    </td>
                    <td>{item.algorithm.replaceAll('_', ' ')}</td>
                    <td>
                      <span className={`state-pill ${item.circuitState.toLowerCase()}`}>
                        {item.circuitState}
                      </span>
                    </td>
                    <td>
                      {item.requests} / {(item.failureRate * 100).toFixed(2)}%
                    </td>
                    <td>
                      {item.consecutiveFailures} / {item.failures}
                    </td>
                    <td>{item.activeConnections}</td>
                    <td>{item.averageResponseMs.toFixed(2)} ms</td>
                    <td>
                      {item.lastTransition ? new Date(item.lastTransition).toLocaleString() : '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="chart-empty">No upstream telemetry received yet.</div>
        )}
      </section>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Circuit transitions</h2>
            <p>Most recent state changes from the live gateway.</p>
          </div>
        </div>
        {events.length ? (
          <div className="decision-list">
            {[...events]
              .reverse()
              .slice(0, 30)
              .map((event, index) => (
                <article key={`${event.at}-${index}`}>
                  <b>
                    {event.from} → {event.to}
                  </b>
                  <span>
                    {event.routeId} · {event.upstream}
                  </span>
                  <small>
                    {new Date(event.at).toLocaleString()} · {event.reason} ({event.failures}{' '}
                    failures)
                  </small>
                </article>
              ))}
          </div>
        ) : (
          <div className="chart-empty">No circuit transitions recorded.</div>
        )}
      </section>
    </>
  );
}

function Services({
  services,
  busy,
  run,
  api,
  readOnly = false,
}: {
  services: Service[];
  busy: boolean;
  run: (fn: () => Promise<void>) => Promise<void>;
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
  readOnly?: boolean;
}) {
  const [editing, setEditing] = useState<Service | null>(null);
  return (
    <>
      {!readOnly && (
        <section className="panel">
          <div className="panel-head">
            <div>
              <h2>{editing ? 'Edit service' : 'Register service'}</h2>
              <p>Each service has one or more HTTP backend instances.</p>
            </div>
          </div>
          <form
            key={editing?.id ?? 'new-service'}
            className="form-grid"
            onSubmit={(event) => {
              event.preventDefault();
              const formElement = event.currentTarget;
              const form = new FormData(formElement);
              void run(async () => {
                const payload = {
                  name: form.get('name'),
                  upstreams: String(form.get('upstreams'))
                    .split('\n')
                    .map((value) => value.trim())
                    .filter(Boolean),
                };
                await api(editing ? `/api/services/${editing.id}` : '/api/services', {
                  method: editing ? 'PUT' : 'POST',
                  body: JSON.stringify(payload),
                });
                setEditing(null);
                formElement.reset();
              });
            }}
          >
            <label>
              Service name
              <input
                name="name"
                required
                defaultValue={editing?.name ?? ''}
                placeholder="user-service"
              />
            </label>
            <label className="wide">
              Backend URLs, one per line
              <textarea
                name="upstreams"
                required
                defaultValue={editing?.upstreams.map((upstream) => upstream.url).join('\n') ?? ''}
                placeholder="http://user-service:4001"
              />
            </label>
            <div className="form-actions">
              {editing && (
                <button type="button" className="secondary" onClick={() => setEditing(null)}>
                  Cancel
                </button>
              )}
              <button className="primary" disabled={busy}>
                {editing ? 'Save service' : 'Create service'}
              </button>
            </div>
          </form>
        </section>
      )}
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Services</h2>
            <p>Registered backend services and instances</p>
          </div>
        </div>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Upstreams</th>
                <th>Routes</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {services.map((service) => (
                <tr key={service.id}>
                  <td>
                    <b>{service.name}</b>
                    <small className="cell-sub">{service.id}</small>
                  </td>
                  <td>
                    {service.upstreams.map((item) => (
                      <div key={item.id}>{item.url}</div>
                    ))}
                  </td>
                  <td>{service._count?.routes ?? '—'}</td>
                  <td className="actions">
                    {!readOnly && (
                      <>
                        <button className="secondary small" onClick={() => setEditing(service)}>
                          Edit
                        </button>
                        <button
                          className="danger small"
                          disabled={busy}
                          onClick={() => {
                            if (confirm(`Delete ${service.name} and its routes?`))
                              void run(async () => {
                                await api(`/api/services/${service.id}`, { method: 'DELETE' });
                              });
                          }}
                        >
                          Delete
                        </button>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {services.length === 0 && <Empty>No services yet. Register a backend above.</Empty>}
        </div>
      </section>
    </>
  );
}

function Upstreams({
  services,
  busy,
  run,
  api,
}: {
  services: Service[];
  busy: boolean;
  run: (fn: () => Promise<void>) => Promise<void>;
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
}) {
  const [selected, setSelected] = useState('');
  const [url, setUrl] = useState('');
  const [weight, setWeight] = useState(1);
  const flat = services.flatMap((service) =>
    service.upstreams.map((upstream) => ({
      ...upstream,
      serviceName: service.name,
      serviceId: service.id,
    })),
  );
  return (
    <>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Add backend instance</h2>
            <p>Add an upstream to an existing service.</p>
          </div>
        </div>
        <form
          className="inline-form"
          onSubmit={(event) => {
            event.preventDefault();
            void run(async () => {
              await api(`/api/services/${selected}/upstreams`, {
                method: 'POST',
                body: JSON.stringify({ url, weight }),
              });
              setUrl('');
              setWeight(1);
            });
          }}
        >
          <label>
            Service
            <select required value={selected} onChange={(event) => setSelected(event.target.value)}>
              <option value="">Select service</option>
              {services.map((service) => (
                <option key={service.id} value={service.id}>
                  {service.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            Load weight
            <input
              type="number"
              min="1"
              max="1000"
              value={weight}
              onChange={(event) => setWeight(Number(event.target.value))}
            />
          </label>
          <label className="grow">
            Backend URL
            <input
              value={url}
              onChange={(event) => setUrl(event.target.value)}
              placeholder="http://host:port"
              required
            />
          </label>
          <button className="primary" disabled={busy || !selected}>
            Add upstream
          </button>
        </form>
      </section>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Upstream instances</h2>
            <p>Instance availability and routing weight</p>
          </div>
        </div>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Service</th>
                <th>URL</th>
                <th>Weight</th>
                <th>Status</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {flat.map((item) => (
                <tr key={item.id}>
                  <td>{item.serviceName}</td>
                  <td>{item.url}</td>
                  <td>{item.weight}</td>
                  <td>
                    <span className={`pill ${item.enabled ? 'green' : 'gray'}`}>
                      {item.enabled ? 'Enabled' : 'Disabled'}
                    </span>
                  </td>
                  <td>
                    <button
                      className="secondary small"
                      disabled={busy}
                      onClick={() =>
                        void run(async () => {
                          await api(`/api/upstreams/${item.id}`, {
                            method: 'PUT',
                            body: JSON.stringify({ enabled: !item.enabled }),
                          });
                        })
                      }
                    >
                      {item.enabled ? 'Disable' : 'Enable'}
                    </button>
                    <button
                      className="danger small"
                      disabled={busy}
                      onClick={() => {
                        if (confirm('Delete this upstream?'))
                          void run(async () => {
                            await api(`/api/upstreams/${item.id}`, { method: 'DELETE' });
                          });
                      }}
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {flat.length === 0 && <Empty>No upstreams registered.</Empty>}
        </div>
      </section>
    </>
  );
}

function Routes({
  routes,
  services,
  busy,
  run,
  api,
  readOnly = false,
}: {
  routes: Route[];
  services: Service[];
  busy: boolean;
  run: (fn: () => Promise<void>) => Promise<void>;
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
  readOnly?: boolean;
}) {
  const [editing, setEditing] = useState<Route | null>(null);
  const beginEdit = (route: Route) => setEditing(route);
  const saveRoute = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    const payload = {
      serviceId: form.get('serviceId'),
      path: form.get('path'),
      matchMode: form.get('matchMode'),
      methods: String(form.get('methods'))
        .split(',')
        .map((method) => method.trim().toUpperCase())
        .filter(Boolean),
      stripPrefix: form.get('stripPrefix') === 'on',
      rewritePrefix: form.get('rewritePrefix') || null,
      timeoutMs: Number(form.get('timeoutMs')),
      rateLimit: Number(form.get('rateLimit')),
      rateLimitMode: form.get('rateLimitMode'),
      adaptiveEnabled: form.get('adaptiveEnabled') === 'on',
      minimumRateLimit: Number(form.get('minimumRateLimit')),
      maximumRateLimit: Number(form.get('maximumRateLimit')),
      spikeSensitivity: Number(form.get('spikeSensitivity')),
      recoveryCooldownMs: Number(form.get('recoveryCooldownMs')),
      cacheTtlMs: Number(form.get('cacheTtlMs')),
      loadBalancingAlgorithm: form.get('loadBalancingAlgorithm'),
      circuitBreakerEnabled: form.get('circuitBreakerEnabled') === 'on',
      circuitFailureThreshold: Number(form.get('circuitFailureThreshold')),
      circuitRecoveryTimeoutMs: Number(form.get('circuitRecoveryTimeoutMs')),
      circuitHalfOpenProbes: Number(form.get('circuitHalfOpenProbes')),
      maxRetries: Number(form.get('maxRetries')),
      retryBackoffMs: Number(form.get('retryBackoffMs')),
      retryMaxBackoffMs: Number(form.get('retryMaxBackoffMs')),
      retryJitter: form.get('retryJitter') === 'on',
      retryStatuses: String(form.get('retryStatuses'))
        .split(',')
        .map((status) => Number(status.trim()))
        .filter((status) => status > 0),
      retryConnectionErrors: form.get('retryConnectionErrors') === 'on',
      retryTimeouts: form.get('retryTimeouts') === 'on',
      retryNonIdempotent: form.get('retryNonIdempotent') === 'on',
      upstreamGroups: String(form.get('upstreamGroups') ?? '').trim()
        ? JSON.parse(String(form.get('upstreamGroups')))
        : null,
      enabled: form.get('enabled') === 'on',
    };
    void run(async () => {
      await api(editing ? `/api/routes/${editing.id}` : '/api/routes', {
        method: editing ? 'PUT' : 'POST',
        body: JSON.stringify(payload),
      });
      setEditing(null);
      formElement.reset();
    });
  };
  return (
    <>
      {!readOnly && (
        <section className="panel">
          <div className="panel-head">
            <div>
              <h2>{editing ? 'Edit route' : 'Create route'}</h2>
              <p>Route changes publish a new immutable gateway configuration version.</p>
            </div>
          </div>
          <form key={editing?.id ?? 'new-route'} className="form-grid" onSubmit={saveRoute}>
            <label>
              Service
              <select name="serviceId" required defaultValue={editing?.serviceId ?? ''}>
                <option value="">Select service</option>
                {services.map((service) => (
                  <option key={service.id} value={service.id}>
                    {service.name}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Public path
              <input
                name="path"
                required
                defaultValue={editing?.path ?? ''}
                placeholder="/api/users"
              />
            </label>
            <label>
              Match mode
              <select name="matchMode" defaultValue={editing?.matchMode ?? 'PREFIX'}>
                <option value="PREFIX">Longest prefix</option>
                <option value="EXACT">Exact path</option>
              </select>
            </label>
            <label>
              Allowed methods
              <input name="methods" defaultValue={editing?.methods.join(', ') ?? 'GET, POST'} />
            </label>
            <label>
              Timeout (ms)
              <input
                name="timeoutMs"
                type="number"
                min="1"
                max="300000"
                defaultValue={editing?.timeoutMs ?? 10000}
              />
            </label>
            <label>
              Requests per minute per IP
              <input
                name="rateLimit"
                type="number"
                min="0"
                defaultValue={editing?.rateLimit ?? 0}
              />
            </label>
            <label>
              Rate-limit mode
              <select name="rateLimitMode" defaultValue={editing?.rateLimitMode ?? 'FIXED'}>
                <option value="FIXED">Fixed</option>
                <option value="ADAPTIVE">Adaptive</option>
              </select>
            </label>
            <label>
              Minimum adaptive limit
              <input
                name="minimumRateLimit"
                type="number"
                min="0"
                defaultValue={
                  editing?.minimumRateLimit ??
                  Math.max(0, Math.ceil((editing?.rateLimit ?? 0) * 0.2))
                }
              />
            </label>
            <label>
              Maximum adaptive limit
              <input
                name="maximumRateLimit"
                type="number"
                min="0"
                defaultValue={editing?.maximumRateLimit ?? editing?.rateLimit ?? 0}
              />
            </label>
            <label>
              Spike sensitivity (× baseline)
              <input
                name="spikeSensitivity"
                type="number"
                min="1.1"
                max="10"
                step="0.1"
                defaultValue={editing?.spikeSensitivity ?? 2}
              />
            </label>
            <label>
              Recovery cooldown (ms)
              <input
                name="recoveryCooldownMs"
                type="number"
                min="1000"
                max="3600000"
                step="1000"
                defaultValue={editing?.recoveryCooldownMs ?? 30000}
              />
            </label>
            <label className="check-label">
              <input
                name="adaptiveEnabled"
                type="checkbox"
                defaultChecked={editing?.adaptiveEnabled ?? false}
              />{' '}
              Enable automatic adaptive enforcement
            </label>
            <label>
              GET cache TTL (ms)
              <input
                name="cacheTtlMs"
                type="number"
                min="0"
                max="3600000"
                defaultValue={editing?.cacheTtlMs ?? 0}
              />
            </label>
            <label>
              Load balancing
              <select
                name="loadBalancingAlgorithm"
                defaultValue={editing?.loadBalancingAlgorithm ?? 'WEIGHTED_ROUND_ROBIN'}
              >
                <option value="ROUND_ROBIN">Round robin</option>
                <option value="WEIGHTED_ROUND_ROBIN">Weighted round robin</option>
                <option value="LEAST_CONNECTIONS">Least connections</option>
                <option value="LEAST_RESPONSE_TIME">Least response time</option>
              </select>
            </label>
            <label className="check-label">
              <input
                name="circuitBreakerEnabled"
                type="checkbox"
                defaultChecked={editing?.circuitBreakerEnabled ?? false}
              />{' '}
              Enable upstream circuit breaker
            </label>
            <label>
              Failure threshold
              <input
                name="circuitFailureThreshold"
                type="number"
                min="1"
                max="1000"
                defaultValue={editing?.circuitFailureThreshold ?? 5}
              />
            </label>
            <label>
              Recovery timeout (ms)
              <input
                name="circuitRecoveryTimeoutMs"
                type="number"
                min="100"
                max="3600000"
                defaultValue={editing?.circuitRecoveryTimeoutMs ?? 30000}
              />
            </label>
            <label>
              Half-open probes
              <input
                name="circuitHalfOpenProbes"
                type="number"
                min="1"
                max="100"
                defaultValue={editing?.circuitHalfOpenProbes ?? 1}
              />
            </label>
            <label>
              Maximum retries
              <input
                name="maxRetries"
                type="number"
                min="0"
                max="5"
                defaultValue={editing?.maxRetries ?? 0}
              />
            </label>
            <label>
              Retry backoff (ms)
              <input
                name="retryBackoffMs"
                type="number"
                min="0"
                max="30000"
                defaultValue={editing?.retryBackoffMs ?? 50}
              />
            </label>
            <label>
              Maximum backoff (ms)
              <input
                name="retryMaxBackoffMs"
                type="number"
                min="0"
                max="300000"
                defaultValue={editing?.retryMaxBackoffMs ?? 1000}
              />
            </label>
            <label>
              Retryable status codes
              <input
                name="retryStatuses"
                defaultValue={editing?.retryStatuses?.join(', ') ?? ''}
                placeholder="502, 503, 504"
              />
            </label>
            <label className="check-label">
              <input
                name="retryConnectionErrors"
                type="checkbox"
                defaultChecked={editing?.retryConnectionErrors ?? false}
              />{' '}
              Retry connection errors
            </label>
            <label className="check-label">
              <input
                name="retryTimeouts"
                type="checkbox"
                defaultChecked={editing?.retryTimeouts ?? false}
              />{' '}
              Retry upstream timeouts
            </label>
            <label className="check-label">
              <input
                name="retryJitter"
                type="checkbox"
                defaultChecked={editing?.retryJitter ?? false}
              />{' '}
              Add backoff jitter
            </label>
            <label className="check-label">
              <input
                name="retryNonIdempotent"
                type="checkbox"
                defaultChecked={editing?.retryNonIdempotent ?? false}
              />{' '}
              Allow unsafe method retries with Idempotency-Key
            </label>
            <label>
              Rewrite prefix
              <input
                name="rewritePrefix"
                defaultValue={editing?.rewritePrefix ?? ''}
                placeholder="Optional, e.g. /users"
              />
            </label>
            <label className="full-width">
              Canary traffic groups (JSON, optional)
              <textarea
                name="upstreamGroups"
                rows={5}
                defaultValue={
                  editing?.upstreamGroups ? JSON.stringify(editing.upstreamGroups, null, 2) : ''
                }
                placeholder={
                  '[{"id":"stable","name":"Stable","percentage":90,"targets":[{"url":"http://localhost:4001","weight":1}]},{"id":"canary","name":"Canary","percentage":10,"targets":[{"url":"http://localhost:4003","weight":1}]}]'
                }
              />
              <small>
                Each target must already be registered and enabled for this service; percentages
                must total 100.
              </small>
            </label>
            <label className="check-label">
              <input
                name="stripPrefix"
                type="checkbox"
                defaultChecked={editing?.stripPrefix ?? false}
              />{' '}
              Strip matched prefix
            </label>
            <label className="check-label">
              <input name="enabled" type="checkbox" defaultChecked={editing?.enabled ?? true} />{' '}
              Route enabled
            </label>
            <div className="form-actions">
              {editing && (
                <button type="button" className="secondary" onClick={() => setEditing(null)}>
                  Cancel
                </button>
              )}
              <button className="primary" disabled={busy}>
                {editing ? 'Save route' : 'Create route'}
              </button>
            </div>
          </form>
        </section>
      )}
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Routes</h2>
            <p>Active state and current service targets</p>
          </div>
        </div>
        <RouteTable
          routes={routes}
          actions={
            readOnly
              ? undefined
              : (route) => (
                  <>
                    <button className="secondary small" onClick={() => beginEdit(route)}>
                      Edit
                    </button>
                    <button
                      className="secondary small"
                      disabled={busy}
                      onClick={() =>
                        void run(async () => {
                          await api(`/api/routes/${route.id}`, {
                            method: 'PUT',
                            body: JSON.stringify({
                              serviceId: route.serviceId,
                              path: route.path,
                              matchMode: route.matchMode,
                              methods: route.methods,
                              stripPrefix: route.stripPrefix,
                              rewritePrefix: route.rewritePrefix,
                              timeoutMs: route.timeoutMs,
                              rateLimit: route.rateLimit ?? 0,
                              rateLimitMode: route.rateLimitMode ?? 'FIXED',
                              adaptiveEnabled: route.adaptiveEnabled ?? false,
                              minimumRateLimit: route.minimumRateLimit ?? 0,
                              maximumRateLimit: route.maximumRateLimit ?? route.rateLimit ?? 0,
                              spikeSensitivity: route.spikeSensitivity ?? 2,
                              recoveryCooldownMs: route.recoveryCooldownMs ?? 30000,
                              cacheTtlMs: route.cacheTtlMs ?? 0,
                              loadBalancingAlgorithm:
                                route.loadBalancingAlgorithm ?? 'WEIGHTED_ROUND_ROBIN',
                              circuitBreakerEnabled: route.circuitBreakerEnabled ?? false,
                              circuitFailureThreshold: route.circuitFailureThreshold ?? 5,
                              circuitRecoveryTimeoutMs: route.circuitRecoveryTimeoutMs ?? 30000,
                              circuitHalfOpenProbes: route.circuitHalfOpenProbes ?? 1,
                              maxRetries: route.maxRetries ?? 0,
                              retryBackoffMs: route.retryBackoffMs ?? 50,
                              retryMaxBackoffMs: route.retryMaxBackoffMs ?? 1000,
                              retryJitter: route.retryJitter ?? false,
                              retryStatuses: route.retryStatuses ?? [],
                              retryConnectionErrors: route.retryConnectionErrors ?? false,
                              retryTimeouts: route.retryTimeouts ?? false,
                              retryNonIdempotent: route.retryNonIdempotent ?? false,
                              enabled: !route.enabled,
                            }),
                          });
                        })
                      }
                    >
                      {route.enabled ? 'Disable' : 'Enable'}
                    </button>
                    <button
                      className="danger small"
                      disabled={busy}
                      onClick={() => {
                        if (confirm(`Delete route ${route.path}?`))
                          void run(async () => {
                            await api(`/api/routes/${route.id}`, { method: 'DELETE' });
                          });
                      }}
                    >
                      Delete
                    </button>
                  </>
                )
          }
        />
      </section>
    </>
  );
}

function AdaptiveDashboard({
  routes,
  api,
  run,
}: {
  routes: Route[];
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
  run: (fn: () => Promise<void>) => Promise<void>;
}) {
  const [data, setData] = useState<AdaptivePayload | null>(null);
  const [error, setError] = useState('');
  const [routeId, setRouteId] = useState('');
  const [limit, setLimit] = useState(10);
  const [expiresInSeconds, setExpiresInSeconds] = useState(300);
  const [loading, setLoading] = useState(true);
  const load = useCallback(async () => {
    try {
      const value = await api<AdaptivePayload>('/api/gateway/adaptive');
      setData(value);
      setError('');
    } catch (reason) {
      setError(
        reason instanceof Error ? reason.message : 'Could not load adaptive protection state',
      );
    } finally {
      setLoading(false);
    }
  }, [api]);
  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 2000);
    return () => window.clearInterval(timer);
  }, [load]);
  const adaptiveRoutes = (data?.routes ?? []).filter((item) => item.mode === 'ADAPTIVE');
  const selected = adaptiveRoutes.find((route) => route.routeId === routeId) ?? adaptiveRoutes[0];
  const setOverride = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!selected) return;
    void run(async () => {
      await api(`/api/routes/${selected.routeId}/adaptive/override`, {
        method: 'POST',
        body: JSON.stringify({ limit: Number(limit), expiresInSeconds: Number(expiresInSeconds) }),
      });
      await load();
    });
  };
  return (
    <>
      {error && <div className="alert error">{error}</div>}
      {loading && !data ? (
        <div className="panel empty">Loading adaptive gateway state…</div>
      ) : !adaptiveRoutes.length ? (
        <div className="panel empty">
          No adaptive routes yet. Edit a route, select Adaptive mode, and enable adaptive
          enforcement.
        </div>
      ) : (
        <>
          <section className="adaptive-cards">
            {adaptiveRoutes.map((route) => (
              <article className="panel adaptive-card" key={route.routeId}>
                <div className="panel-head">
                  <div>
                    <h2>{route.path}</h2>
                    <p>
                      {route.mode} · {route.enabled ? 'enabled' : 'disabled'} · baseline{' '}
                      {route.baselineReliable ? 'ready' : 'warming'}
                    </p>
                  </div>
                  <span className={`adaptive-state ${route.recoveryStatus}`}>
                    {labelStatus(route.recoveryStatus)}
                  </span>
                </div>
                <div className="adaptive-values">
                  <div>
                    <small>Configured / effective</small>
                    <b>
                      {route.baseLimit} / {route.effectiveLimit} <small>per minute / IP</small>
                    </b>
                  </div>
                  <div>
                    <small>Traffic baseline / incoming</small>
                    <b>
                      {route.baselineRps.toFixed(2)} / {route.incomingRps.toFixed(2)}{' '}
                      <small>req/sec</small>
                    </b>
                  </div>
                  <div>
                    <small>Spikes detected</small>
                    <b>{route.spikesDetected}</b>
                  </div>
                  <div>
                    <small>Throttled requests</small>
                    <b>{route.throttledRequests}</b>
                  </div>
                  <div>
                    <small>Last adaptation</small>
                    <b>
                      {route.lastAdaptationTime
                        ? new Date(route.lastAdaptationTime).toLocaleString()
                        : '—'}
                    </b>
                  </div>
                  <div>
                    <small>Protection range</small>
                    <b>
                      {route.minimumLimit}–{route.maximumLimit}/min
                    </b>
                  </div>
                  {route.manualOverrideExpiresAt > Date.now() && (
                    <div>
                      <small>Manual override expires</small>
                      <b>{new Date(route.manualOverrideExpiresAt).toLocaleString()}</b>
                    </div>
                  )}
                </div>
              </article>
            ))}
          </section>
          {selected && (
            <>
              <section className="panel">
                <div className="panel-head">
                  <div>
                    <h2>Adaptive traffic · {selected.path}</h2>
                    <p>Five-second route windows · recent 60 seconds</p>
                  </div>
                </div>
                <AdaptiveChart
                  title="Incoming traffic vs EWMA baseline (requests/sec)"
                  points={selected.timeseries}
                  fields={[
                    ['incomingRps', 'Incoming'],
                    ['baselineRps', 'Baseline'],
                  ]}
                />
                <div className="adaptive-chart-row">
                  <AdaptiveChart
                    title="Effective rate limit (requests/min/IP)"
                    points={selected.timeseries}
                    fields={[['effectiveLimit', 'Effective limit']]}
                  />
                  <AdaptiveChart
                    title="Throttled requests per window"
                    points={selected.timeseries}
                    fields={[['throttled', 'Throttled']]}
                  />
                </div>
              </section>
              <section className="panel">
                <div className="panel-head">
                  <div>
                    <h2>Administrator override</h2>
                    <p>Temporary limits remain inside the configured minimum and maximum.</p>
                  </div>
                </div>
                <form className="inline-form" onSubmit={setOverride}>
                  <label>
                    Route
                    <select
                      value={selected.routeId}
                      onChange={(event) => setRouteId(event.target.value)}
                    >
                      {adaptiveRoutes.map((route) => (
                        <option key={route.routeId} value={route.routeId}>
                          {route.path}
                        </option>
                      ))}
                    </select>
                  </label>
                  <label>
                    Temporary limit
                    <input
                      type="number"
                      min={selected.minimumLimit}
                      max={selected.maximumLimit}
                      value={limit}
                      onChange={(event) => setLimit(Number(event.target.value))}
                    />
                  </label>
                  <label>
                    Expires in (seconds)
                    <input
                      type="number"
                      min="1"
                      max="86400"
                      value={expiresInSeconds}
                      onChange={(event) => setExpiresInSeconds(Number(event.target.value))}
                    />
                  </label>
                  <button className="primary" type="submit">
                    Apply override
                  </button>
                  <button
                    className="secondary"
                    type="button"
                    onClick={() =>
                      void run(async () => {
                        await api(`/api/routes/${selected.routeId}/adaptive/override`, {
                          method: 'DELETE',
                        });
                        await load();
                      })
                    }
                  >
                    Return to automatic adaptation
                  </button>
                </form>
              </section>
            </>
          )}
        </>
      )}
      {data && (
        <section className="panel">
          <div className="panel-head">
            <div>
              <h2>Recent adaptive decisions</h2>
              <p>Gateway-generated explanations; history is bounded to the latest 500 decisions.</p>
            </div>
          </div>
          {data.decisions.length ? (
            <div className="decision-list">
              {data.decisions.slice(0, 30).map((event, index) => (
                <article key={`${event.routeId}-${event.at}-${index}`}>
                  <time>{new Date(event.at).toLocaleString()}</time>
                  <b>{event.type.replaceAll('_', ' ')}</b>
                  <span>{event.explanation}</span>
                  <small>
                    {routes.find((route) => route.id === event.routeId)?.path ?? event.routeId} ·{' '}
                    {event.effectiveLimit}/min
                  </small>
                </article>
              ))}
            </div>
          ) : (
            <div className="empty">No adaptive decisions recorded.</div>
          )}
        </section>
      )}
    </>
  );
}

function AdaptiveChart({
  title,
  points,
  fields,
}: {
  title: string;
  points: AdaptivePoint[];
  fields: [keyof AdaptivePoint, string][];
}) {
  const values = fields.flatMap(([key]) => points.map((point) => Number(point[key])));
  const maxValue = Math.max(1, ...values);
  return (
    <div className="adaptive-chart">
      <h3>{title}</h3>
      {points.length ? (
        <svg viewBox="0 0 100 100" preserveAspectRatio="none" aria-label={title}>
          {fields.map(([key], series) => (
            <polyline
              className={`series-${series}`}
              key={key}
              points={points
                .map(
                  (point, i) =>
                    `${(i / Math.max(1, points.length - 1)) * 100},${94 - (Number(point[key]) / maxValue) * 84}`,
                )
                .join(' ')}
            />
          ))}
        </svg>
      ) : (
        <div className="chart-empty">No traffic samples</div>
      )}
      <div className="chart-legend">
        {fields.map(([, label], index) => (
          <span key={label}>
            <i className={`legend-${index}`} />
            {label}
          </span>
        ))}
      </div>
    </div>
  );
}

function labelStatus(status: string) {
  return (
    (
      {
        normal: 'Normal',
        spike_detected: 'Spike detected',
        rate_limit_reduced: 'Rate limit reduced',
        recovering: 'Recovering',
      } as Record<string, string>
    )[status] ?? status
  );
}

function RouteTable({
  routes,
  actions,
}: {
  routes: Route[];
  actions?: (route: Route) => React.ReactNode;
}) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Path</th>
            <th>Match</th>
            <th>Service</th>
            <th>Methods</th>
            <th>Timeout</th>
            <th>Rate limit</th>
            <th>Cache TTL</th>
            <th>Status</th>
            {actions && <th>Actions</th>}
          </tr>
        </thead>
        <tbody>
          {routes.map((route) => (
            <tr key={route.id}>
              <td>
                <b>{route.path}</b>
                {route.stripPrefix && <small className="cell-sub">Prefix stripped</small>}
              </td>
              <td>{route.matchMode}</td>
              <td>{route.service?.name ?? route.serviceId}</td>
              <td>{route.methods.join(', ')}</td>
              <td>{route.timeoutMs} ms</td>
              <td>{route.rateLimit ? `${route.rateLimit}/min` : 'Off'}</td>
              <td>{route.cacheTtlMs ? `${route.cacheTtlMs} ms` : 'Off'}</td>
              <td>
                <span className={`pill ${route.enabled ? 'green' : 'gray'}`}>
                  {route.enabled ? 'Enabled' : 'Disabled'}
                </span>
              </td>
              {actions && <td className="actions">{actions(route)}</td>}
            </tr>
          ))}
        </tbody>
      </table>
      {routes.length === 0 && <Empty>No routes configured.</Empty>}
    </div>
  );
}

function Gateways({
  gateways,
  api,
  run,
}: {
  gateways: Gateway[];
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
  run: (fn: () => Promise<void>) => Promise<void>;
}) {
  const [detail, setDetail] = useState<Record<string, unknown> | null>(null);
  const [name, setName] = useState('');
  const [newToken, setNewToken] = useState('');
  return (
    <>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Register gateway node</h2>
            <p>Credentials are hashed in storage. Copy the token when it is shown once.</p>
          </div>
        </div>
        <form
          className="inline-form"
          onSubmit={(event) => {
            event.preventDefault();
            void run(async () => {
              const result = await api<{ token: string }>('/api/gateways/register', {
                method: 'POST',
                body: JSON.stringify({ name }),
              });
              setNewToken(result.token);
              setName('');
            });
          }}
        >
          <label className="grow">
            Node name
            <input
              required
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder="edge-us-east-1"
            />
          </label>
          <button className="primary">Register node</button>
        </form>
        {newToken && (
          <div className="token-result">
            <b>Copy this gateway token now</b>
            <code>{newToken}</code>
            <button
              className="secondary small"
              onClick={() => void navigator.clipboard.writeText(newToken)}
            >
              Copy token
            </button>
          </div>
        )}
      </section>
      <section className="panel">
        <div className="panel-head">
          <div>
            <h2>Gateway nodes</h2>
            <p>Each node polls with its own registered credential and reports applied versions.</p>
          </div>
        </div>
        <GatewayTable
          gateways={gateways}
          onSelect={(id) =>
            void api<Record<string, unknown>>(`/api/gateways/${id}`)
              .then(setDetail)
              .catch(() => setDetail(null))
          }
        />
        {detail && <pre className="json-detail">{JSON.stringify(detail, null, 2)}</pre>}
      </section>
    </>
  );
}
function GatewayTable({
  gateways,
  onSelect,
}: {
  gateways: Gateway[];
  onSelect?: (id: string) => void;
}) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Node</th>
            <th>Heartbeat</th>
            <th>Applied version</th>
            <th>Latest version</th>
            <th>Sync status</th>
            {onSelect && <th>Details</th>}
          </tr>
        </thead>
        <tbody>
          {gateways.map((gateway) => (
            <tr key={gateway.id}>
              <td>
                <b>{gateway.name}</b>
                <small className="cell-sub">{gateway.id}</small>
              </td>
              <td>
                {gateway.lastSeenAt ? new Date(gateway.lastSeenAt).toLocaleString() : 'Never'}
              </td>
              <td>v{gateway.currentVersion}</td>
              <td>v{gateway.latestVersion}</td>
              <td>
                <span className={`pill ${gateway.synced ? 'green' : 'amber'}`}>
                  {gateway.synced ? 'Synced' : 'Pending'}
                </span>
                {gateway.latestAcknowledgement?.status === 'REJECTED' && (
                  <small className="cell-sub">{gateway.latestAcknowledgement.message}</small>
                )}
              </td>
              {onSelect && (
                <td>
                  <button className="secondary small" onClick={() => onSelect(gateway.id)}>
                    View
                  </button>
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
      {gateways.length === 0 && <Empty>No gateway nodes registered.</Empty>}
    </div>
  );
}
function Empty({ children }: { children: React.ReactNode }) {
  return <div className="empty">{children}</div>;
}
function LivePanel({
  title,
  endpoint,
  api,
}: {
  title: string;
  endpoint: string;
  api: <T>(path: string, init?: RequestInit) => Promise<T>;
}) {
  const [data, setData] = useState<unknown>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let active = true;
    const load = () =>
      void api<unknown>(endpoint)
        .then((value) => {
          if (active) {
            setData(value);
            setError('');
          }
        })
        .catch((e: unknown) => {
          if (active) setError(e instanceof Error ? e.message : 'Could not load data');
        });
    load();
    const timer = window.setInterval(load, 5000);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [endpoint, api]);
  return (
    <section className="panel">
      <div className="panel-head">
        <div>
          <h2>{title}</h2>
          <p>Live values from the control API and gateway</p>
        </div>
      </div>
      {error ? (
        <div className="alert error">{error}</div>
      ) : data === null ? (
        <div className="empty">Loading…</div>
      ) : (
        <pre className="json-detail">{JSON.stringify(data, null, 2)}</pre>
      )}
    </section>
  );
}
function pageTitle(page: string) {
  return (
    (
      {
        overview: 'Overview',
        services: 'Services',
        upstreams: 'Upstreams',
        routes: 'Routes',
        gateways: 'Gateway nodes',
        analytics: 'Traffic Analytics',
        health: 'Gateway Health',
        settings: 'Settings',
        adaptive: 'Adaptive Protection',
        access: 'Access Control',
        audit: 'Audit log',
      } as Record<string, string>
    )[page] ?? 'Overview'
  );
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
