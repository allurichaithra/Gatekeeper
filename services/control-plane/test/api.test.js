import { mkdtempSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { hashPassword } from '../src/security.js';

const directory = mkdtempSync(join(tmpdir(), 'gatekeeper-control-'));
const sqliteFile = join(directory, 'state.sqlite');
process.env.CONTROL_API_PORT = '0';
process.env.SQLITE_FILE = sqliteFile;
process.env.GATEWAY_CONFIG_FILE = join(directory, 'gateway.json');
process.env.JWT_SECRET = 'test-only-secret-with-more-than-32-bytes';
process.env.GATEWAY_API_KEY = 'test-gateway-key-with-32-bytes-minimum';
const { server, db } = await import('../src/main.js');
await new Promise((resolve) => server.once('listening', resolve));
const base = `http://127.0.0.1:${server.address().port}`;
const adminId = randomUUID();
db.prepare('INSERT INTO users(id,username,passwordHash,role,createdAt) VALUES(?,?,?,?,?)').run(
  adminId,
  'admin-test',
  await hashPassword('correct horse battery staple'),
  'ADMIN',
  Date.now(),
);
const viewerId = randomUUID();
db.prepare('INSERT INTO users(id,username,passwordHash,role,createdAt) VALUES(?,?,?,?,?)').run(
  viewerId,
  'viewer-test',
  await hashPassword('viewer password phrase 123'),
  'VIEWER',
  Date.now(),
);
let adminToken = '';
let viewerToken = '';
async function controlFetch(url, init = {}) {
  const headers = new Headers(init.headers ?? {});
  if (!headers.has('content-type') && init.body) headers.set('content-type', 'application/json');
  if (!headers.has('authorization')) headers.set('authorization', `Bearer ${adminToken}`);
  return fetch(url, { ...init, headers });
}

test('authentication, authorization, internal API key and audit history', async () => {
  const invalid = await fetch(`${base}/api/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ username: 'admin-test', password: 'wrong password' }),
  });
  assert.equal(invalid.status, 401);
  const login = await fetch(`${base}/api/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ username: 'admin-test', password: 'correct horse battery staple' }),
  });
  assert.equal(login.status, 200);
  const loginBody = await login.json();
  adminToken = loginBody.accessToken;
  const viewerLogin = await fetch(`${base}/api/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ username: 'viewer-test', password: 'viewer password phrase 123' }),
  });
  viewerToken = (await viewerLogin.json()).accessToken;
  assert.equal((await fetch(`${base}/api/services`)).status, 401);
  const forbidden = await controlFetch(`${base}/api/services`, {
    method: 'POST',
    headers: { authorization: `Bearer ${viewerToken}`, 'content-type': 'application/json' },
    body: JSON.stringify({ name: 'blocked', upstreams: ['http://localhost:4001'] }),
  });
  assert.equal(forbidden.status, 403);
  const permittedView = await controlFetch(`${base}/api/services`, {
    headers: { authorization: `Bearer ${viewerToken}` },
  });
  assert.equal(permittedView.status, 200);
  assert.equal(
    (
      await controlFetch(`${base}/api/config/history`, {
        headers: { authorization: `Bearer ${viewerToken}` },
      })
    ).status,
    200,
  );
  assert.equal(
    (
      await controlFetch(`${base}/api/config/rollback/0`, {
        method: 'POST',
        headers: { authorization: `Bearer ${viewerToken}` },
      })
    ).status,
    403,
  );
  assert.equal(
    (
      await controlFetch(`${base}/api/alerts/rules`, {
        method: 'PUT',
        headers: { authorization: `Bearer ${viewerToken}`, 'content-type': 'application/json' },
        body: JSON.stringify({ errorRate: 0.1, p95LatencyMs: 100, trafficSpikeMultiplier: 2 }),
      })
    ).status,
    403,
  );
  assert.equal((await fetch(`${base}/internal/gateway/config`)).status, 401);
  const internal = await fetch(`${base}/internal/gateway/config`, {
    headers: {
      authorization: `Bearer ${process.env.GATEWAY_API_KEY}`,
      'x-gateway-id': 'test-gateway',
    },
  });
  assert.equal(internal.status, 200);
  assert.equal(
    (
      await fetch(`${base}/internal/gateway/config/ack`, {
        method: 'POST',
        headers: {
          authorization: `Bearer ${process.env.GATEWAY_API_KEY}`,
          'content-type': 'application/json',
          'x-gateway-id': 'test-gateway',
        },
        body: JSON.stringify({ version: 1, status: 'APPLIED' }),
      })
    ).status,
    204,
  );
  assert.equal(
    (
      await controlFetch(`${base}/api/access/audit`, {
        headers: { authorization: `Bearer ${viewerToken}` },
      })
    ).status,
    403,
  );
  for (let i = 0; i < 5; i++)
    await controlFetch(`${base}/api/auth/login`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ username: 'attacker-test', password: 'incorrect' }),
    });
  const limited = await controlFetch(`${base}/api/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ username: 'attacker-test', password: 'incorrect' }),
  });
  assert.equal(limited.status, 429);
  const me = await controlFetch(`${base}/api/auth/me`);
  assert.equal(me.status, 200);
  const logout = await controlFetch(`${base}/api/auth/logout`, { method: 'POST' });
  assert.equal(logout.status, 204);
  const revoked = await fetch(`${base}/api/auth/me`, {
    headers: { authorization: `Bearer ${adminToken}` },
  });
  assert.equal(revoked.status, 401);
  const relogin = await fetch(`${base}/api/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ username: 'admin-test', password: 'correct horse battery staple' }),
  });
  adminToken = (await relogin.json()).accessToken;
});

test('control API stores entities in SQLite and publishes gateway snapshots', async (t) => {
  t.after(() => {
    server.close();
    db.close();
    rmSync(directory, { recursive: true, force: true });
  });
  const excessiveUpstreams = await controlFetch(`${base}/api/services`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      name: 'too-many-upstreams',
      upstreams: Array.from({ length: 101 }, () => 'http://localhost:4001'),
    }),
  });
  assert.equal(excessiveUpstreams.status, 400);
  const invalidAlerts = await controlFetch(`${base}/api/alerts/rules`, {
    method: 'PUT',
    body: JSON.stringify({ errorRate: 3, p95LatencyMs: 100, trafficSpikeMultiplier: 2 }),
  });
  assert.equal(invalidAlerts.status, 400);
  const alertRules = await controlFetch(`${base}/api/alerts/rules`, {
    method: 'PUT',
    body: JSON.stringify({ errorRate: 0.08, p95LatencyMs: 750, trafficSpikeMultiplier: 2.5 }),
  });
  assert.equal(alertRules.status, 200);
  assert.equal(db.prepare('SELECT errorRate FROM alert_rules WHERE id=1').get().errorRate, 0.08);
  const createService = await controlFetch(`${base}/api/services`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name: 'users', upstreams: ['http://localhost:4001'] }),
  });
  assert.equal(createService.status, 201);
  const service = await createService.json();
  const createRoute = await controlFetch(`${base}/api/routes`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      serviceId: service.id,
      path: '/api/users',
      matchMode: 'PREFIX',
      methods: ['GET'],
      stripPrefix: true,
      timeoutMs: 5000,
      rateLimit: 30,
      rateLimitMode: 'ADAPTIVE',
      adaptiveEnabled: true,
      minimumRateLimit: 5,
      maximumRateLimit: 30,
      spikeSensitivity: 2.5,
      recoveryCooldownMs: 5000,
      cacheTtlMs: 1000,
      loadBalancingAlgorithm: 'LEAST_CONNECTIONS',
      circuitBreakerEnabled: true,
      circuitFailureThreshold: 4,
      circuitRecoveryTimeoutMs: 15000,
      circuitHalfOpenProbes: 2,
      maxRetries: 2,
      retryBackoffMs: 25,
      retryMaxBackoffMs: 250,
      retryJitter: true,
      retryStatuses: [502, 503],
      retryConnectionErrors: true,
      retryTimeouts: true,
    }),
  });
  assert.equal(createRoute.status, 201);
  const auditRows = db
    .prepare('SELECT actorUsername,action,changes FROM audit_logs ORDER BY id')
    .all();
  assert.ok(auditRows.some((row) => row.actorUsername === 'admin-test' && row.action === 'CREATE'));
  assert.ok(!JSON.stringify(auditRows).includes('correct horse battery staple'));
  assert.ok(!JSON.stringify(auditRows).includes(process.env.GATEWAY_API_KEY));
  assert.equal((await controlFetch(`${base}/api/access/audit`)).status, 200);
  const config = JSON.parse(readFileSync(process.env.GATEWAY_CONFIG_FILE, 'utf8'));
  assert.equal(config.version, '1');
  assert.equal(config.routes[0].rateLimit, 30);
  assert.equal(config.routes[0].rateLimitMode, 'ADAPTIVE');
  assert.equal(config.routes[0].adaptiveEnabled, true);
  assert.equal(config.routes[0].minimumRateLimit, 5);
  assert.equal(config.routes[0].spikeSensitivity, 2.5);
  assert.equal(config.routes[0].recoveryCooldownMs, 5000);
  assert.equal(config.routes[0].targets[0].url, 'http://localhost:4001');
  assert.equal(config.routes[0].loadBalancingAlgorithm, 'LEAST_CONNECTIONS');
  assert.equal(config.routes[0].circuitBreakerEnabled, true);
  assert.equal(config.routes[0].circuitFailureThreshold, 4);
  assert.equal(config.routes[0].circuitRecoveryTimeoutMs, 15000);
  assert.equal(config.routes[0].circuitHalfOpenProbes, 2);
  assert.equal(config.routes[0].maxRetries, 2);
  assert.deepEqual(config.routes[0].retryStatuses, [502, 503]);
  assert.equal(config.routes[0].retryConnectionErrors, true);
  assert.equal(config.routes[0].retryTimeouts, true);
  const badUpdate = await controlFetch(`${base}/api/routes/${config.routes[0].id}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      serviceId: service.id,
      path: '/api/users',
      methods: ['GET'],
      timeoutMs: 0,
    }),
  });
  assert.equal(badUpdate.status, 400);
  const unsupportedBalancer = await controlFetch(`${base}/api/routes/${config.routes[0].id}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      serviceId: service.id,
      path: '/api/users',
      loadBalancingAlgorithm: 'MAGIC',
    }),
  });
  assert.equal(unsupportedBalancer.status, 400);
  const invalidCircuit = await controlFetch(`${base}/api/routes/${config.routes[0].id}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      serviceId: service.id,
      path: '/api/users',
      circuitBreakerEnabled: true,
      circuitFailureThreshold: 0,
    }),
  });
  assert.equal(invalidCircuit.status, 400);
  const invalidAdaptive = await controlFetch(`${base}/api/routes/${config.routes[0].id}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      serviceId: service.id,
      path: '/api/users',
      rateLimit: 30,
      rateLimitMode: 'ADAPTIVE',
      adaptiveEnabled: true,
      minimumRateLimit: 5,
      maximumRateLimit: 30,
      spikeSensitivity: 1,
      recoveryCooldownMs: 5000,
    }),
  });
  assert.equal(invalidAdaptive.status, 400);
  const invalidOverride = await controlFetch(
    `${base}/api/routes/${config.routes[0].id}/adaptive/override`,
    {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ limit: 2, expiresInSeconds: 60 }),
    },
  );
  assert.equal(invalidOverride.status, 400);
  const override = await controlFetch(
    `${base}/api/routes/${config.routes[0].id}/adaptive/override`,
    {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ limit: 10, expiresInSeconds: 60 }),
    },
  );
  assert.equal(override.status, 200);
  const overrideResult = await override.json();
  const withOverride = JSON.parse(readFileSync(process.env.GATEWAY_CONFIG_FILE, 'utf8'));
  assert.equal(withOverride.version, overrideResult.version);
  assert.equal(withOverride.routes[0].manualOverrideLimit, 10);
  assert.equal(
    db
      .prepare('SELECT limitValue FROM rate_limit_overrides WHERE routeId=?')
      .get(config.routes[0].id).limitValue,
    10,
  );
  const conflictingBounds = await controlFetch(`${base}/api/routes/${config.routes[0].id}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      serviceId: service.id,
      path: '/api/users',
      rateLimit: 20,
      rateLimitMode: 'ADAPTIVE',
      adaptiveEnabled: true,
      minimumRateLimit: 15,
      maximumRateLimit: 20,
      spikeSensitivity: 2,
      recoveryCooldownMs: 5000,
    }),
  });
  assert.equal(conflictingBounds.status, 400);
  const persistedAdaptive = db
    .prepare(
      'SELECT rateLimitMode,adaptiveEnabled,minimumRateLimit,maximumRateLimit FROM routes WHERE id=?',
    )
    .get(config.routes[0].id);
  assert.equal(persistedAdaptive.rateLimitMode, 'ADAPTIVE');
  assert.equal(persistedAdaptive.adaptiveEnabled, 1);
  assert.equal(persistedAdaptive.minimumRateLimit, 5);
  assert.equal(persistedAdaptive.maximumRateLimit, 30);
  const update = await controlFetch(`${base}/api/routes/${config.routes[0].id}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      serviceId: service.id,
      path: '/api/users',
      matchMode: 'PREFIX',
      methods: ['GET'],
      stripPrefix: false,
      timeoutMs: 7000,
      rateLimit: 30,
      rateLimitMode: 'FIXED',
      adaptiveEnabled: false,
      minimumRateLimit: 5,
      maximumRateLimit: 30,
      spikeSensitivity: 2.5,
      recoveryCooldownMs: 5000,
      enabled: false,
    }),
  });
  assert.equal(update.status, 200);
  const current = await controlFetch(`${base}/api/gateway/config`).then((response) =>
    response.json(),
  );
  assert.equal(current.version, '3');
  assert.equal(current.routes[0].timeoutMs, 7000);
  assert.equal(current.routes[0].enabled, false);
  assert.equal(current.routes[0].rateLimitMode, 'FIXED');
  const history = await controlFetch(`${base}/api/config/history`).then((response) =>
    response.json(),
  );
  assert.equal(history.activeVersion, 3);
  assert.ok(
    history.versions.some((entry) => entry.version === 1 && entry.actorUsername === 'admin-test'),
  );
  const comparison = await controlFetch(`${base}/api/config/history/compare?from=1&to=3`).then(
    (response) => response.json(),
  );
  assert.ok(
    comparison.changes.some(
      (change) => change.kind === 'changed' && change.fields.includes('timeoutMs'),
    ),
  );
  const rollback = await controlFetch(`${base}/api/config/rollback/3`, { method: 'POST' });
  assert.equal(rollback.status, 200, await rollback.clone().text());
  const rollbackResult = await rollback.json();
  assert.equal(rollbackResult.rollbackOf, 3);
  assert.equal(
    JSON.parse(readFileSync(process.env.GATEWAY_CONFIG_FILE, 'utf8')).routes[0].enabled,
    false,
  );
  assert.ok(db.prepare("SELECT 1 FROM audit_logs WHERE action='ROLLBACK' LIMIT 1").get());
  const invalidRollback = await controlFetch(`${base}/api/config/rollback/99999`, {
    method: 'POST',
  });
  assert.equal(invalidRollback.status, 404);
  const invalidCanary = await controlFetch(`${base}/api/routes/${config.routes[0].id}`, {
    method: 'PUT',
    body: JSON.stringify({
      serviceId: service.id,
      path: '/api/users',
      methods: ['GET'],
      timeoutMs: 5000,
      upstreamGroups: [
        {
          id: 'stable',
          name: 'Stable',
          percentage: 90,
          targets: [{ url: 'http://localhost:4001', weight: 1 }],
        },
        {
          id: 'canary',
          name: 'Canary',
          percentage: 5,
          targets: [{ url: 'http://localhost:4001', weight: 1 }],
        },
      ],
    }),
  });
  assert.equal(invalidCanary.status, 400);
  assert.equal(
    (
      await controlFetch(`${base}/api/routes/${config.routes[0].id}/adaptive/override`, {
        method: 'DELETE',
      })
    ).status,
    200,
  );
  assert.equal(
    db
      .prepare('SELECT count(*) n FROM rate_limit_overrides WHERE routeId=?')
      .get(config.routes[0].id).n,
    0,
  );
  assert.equal((await controlFetch(`${base}/api/routes`)).status, 200);
  const persisted = new DatabaseSync(sqliteFile);
  assert.equal(persisted.prepare('SELECT count(*) n FROM routes').get().n, 1);
  assert.ok(persisted.prepare('SELECT count(*) n FROM config_history').get().n >= 5);
  persisted.close();
  assert.equal(
    (await controlFetch(`${base}/api/routes/${config.routes[0].id}`, { method: 'DELETE' })).status,
    204,
  );
  assert.equal(
    (await controlFetch(`${base}/api/routes`)).headers.get('content-type'),
    'application/json; charset=utf-8',
  );
});
