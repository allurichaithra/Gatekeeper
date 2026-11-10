import express from 'express';
import { DatabaseSync } from 'node:sqlite';
import { randomUUID, randomBytes, timingSafeEqual } from 'node:crypto';
import { mkdirSync, renameSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createAccessToken, hashPassword, verifyAccessToken, verifyPassword } from './security.js';

const app = express();
app.disable('x-powered-by');
const dashboardOrigins = new Set(
  (process.env.DASHBOARD_ORIGINS ?? 'http://localhost:5173')
    .split(',')
    .map((value) => value.trim())
    .filter(Boolean),
);
app.use((req, res, next) => {
  res.set('X-Content-Type-Options', 'nosniff');
  res.set('X-Frame-Options', 'DENY');
  res.set('Referrer-Policy', 'no-referrer');
  res.set('Content-Security-Policy', "default-src 'none'; frame-ancestors 'none'");
  res.set('Cache-Control', 'no-store');
  const origin = req.get('Origin');
  if (origin && !dashboardOrigins.has(origin))
    return res.status(403).json({ error: 'origin_not_allowed' });
  if (origin) {
    res.set('Access-Control-Allow-Origin', origin);
    res.set('Vary', 'Origin');
    res.set('Access-Control-Allow-Headers', 'Authorization, Content-Type');
    res.set('Access-Control-Allow-Methods', 'GET, POST, PUT, PATCH, DELETE, OPTIONS');
  }
  if (req.method === 'OPTIONS') return res.status(204).end();
  next();
});
app.use(express.json({ limit: '64kb' }));
const projectRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const dbPath = resolve(projectRoot, process.env.SQLITE_FILE ?? 'data/gatekeeper.sqlite');
mkdirSync(dirname(dbPath), { recursive: true });
const db = new DatabaseSync(dbPath);
db.exec(`PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS services(id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL);
CREATE TABLE IF NOT EXISTS upstreams(id TEXT PRIMARY KEY, serviceId TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE, url TEXT NOT NULL, weight INTEGER NOT NULL DEFAULT 1, enabled INTEGER NOT NULL DEFAULT 1);
CREATE TABLE IF NOT EXISTS routes(id TEXT PRIMARY KEY, serviceId TEXT NOT NULL REFERENCES services(id), path TEXT NOT NULL, matchMode TEXT NOT NULL DEFAULT 'PREFIX', methods TEXT NOT NULL, stripPrefix INTEGER NOT NULL DEFAULT 0, rewritePrefix TEXT, timeoutMs INTEGER NOT NULL DEFAULT 10000, rateLimit INTEGER NOT NULL DEFAULT 0, cacheTtlMs INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1);`);
const routeColumnNames = new Set(
  db
    .prepare('PRAGMA table_info(routes)')
    .all()
    .map((column) => column.name),
);
for (const [name, definition] of Object.entries({
  rateLimitMode: "TEXT NOT NULL DEFAULT 'FIXED'",
  adaptiveEnabled: 'INTEGER NOT NULL DEFAULT 0',
  minimumRateLimit: 'INTEGER NOT NULL DEFAULT 0',
  maximumRateLimit: 'INTEGER NOT NULL DEFAULT 0',
  spikeSensitivity: 'REAL NOT NULL DEFAULT 0',
  recoveryCooldownMs: 'INTEGER NOT NULL DEFAULT 0',
})) {
  if (!routeColumnNames.has(name)) db.exec(`ALTER TABLE routes ADD COLUMN ${name} ${definition}`);
}
db.exec(
  'CREATE TABLE IF NOT EXISTS rate_limit_overrides(routeId TEXT PRIMARY KEY REFERENCES routes(id) ON DELETE CASCADE, limitValue INTEGER NOT NULL, expiresAt INTEGER NOT NULL)',
);
db.exec(`CREATE TABLE IF NOT EXISTS users(
  id TEXT PRIMARY KEY,
  username TEXT UNIQUE COLLATE NOCASE NOT NULL,
  passwordHash TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('ADMIN','VIEWER')),
  createdAt INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions(
  jti TEXT PRIMARY KEY,
  userId TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expiresAt INTEGER NOT NULL,
  revokedAt INTEGER
);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions(userId);
CREATE TABLE IF NOT EXISTS audit_logs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  actorId TEXT NOT NULL,
  actorUsername TEXT NOT NULL,
  action TEXT NOT NULL,
  resource TEXT NOT NULL,
  changes TEXT NOT NULL,
  createdAt INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS gateway_config_acks(
  gatewayId TEXT NOT NULL,
  version INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('APPLIED','REJECTED')),
  message TEXT NOT NULL DEFAULT '',
  updatedAt INTEGER NOT NULL,
  PRIMARY KEY(gatewayId,version)
);`);
const jwtSecret = process.env.JWT_SECRET ?? '';
if (Buffer.byteLength(jwtSecret) < 32)
  throw new Error('JWT_SECRET must contain at least 32 bytes.');
const gatewayApiKey = process.env.GATEWAY_API_KEY ?? '';
if (Buffer.byteLength(gatewayApiKey) < 32)
  throw new Error('GATEWAY_API_KEY must contain at least 32 bytes.');
const dummyPasswordHash = await hashPassword(randomBytes(32).toString('hex'));
const loginAttempts = new Map();
const LOGIN_WINDOW_MS = 15 * 60 * 1000;
const LOGIN_MAX_FAILURES = 5;
db.exec('CREATE UNIQUE INDEX IF NOT EXISTS route_match_unique ON routes(path, matchMode)');
db.exec('PRAGMA foreign_keys=ON');
const gatewayURL = (process.env.GATEWAY_URL ?? 'http://localhost:8080').replace(/\/$/, '');
const configFile = resolve(
  projectRoot,
  process.env.GATEWAY_CONFIG_FILE ?? 'data/gateway-config.json',
);

function authenticationError(res, message = 'Authentication required.') {
  return res.status(401).json({ error: 'unauthorized', message });
}

function authenticate(req, res, next) {
  const match = /^Bearer ([A-Za-z0-9._~-]{20,4096})$/.exec(req.get('Authorization') ?? '');
  if (!match) return authenticationError(res);
  let payload;
  try {
    payload = verifyAccessToken(match[1], jwtSecret);
  } catch {
    return authenticationError(res, 'Access token is invalid or expired.');
  }
  const session = db
    .prepare(
      'SELECT s.jti,u.id,u.username,u.role FROM sessions s JOIN users u ON u.id=s.userId WHERE s.jti=? AND s.userId=? AND s.expiresAt>? AND s.revokedAt IS NULL',
    )
    .get(payload.jti, payload.sub, Date.now());
  if (!session) return authenticationError(res, 'Access token is expired or logged out.');
  req.auth = { id: session.id, username: session.username, role: session.role, jti: payload.jti };
  next();
}

function adminOnly(req, res, next) {
  if (req.auth?.role !== 'ADMIN')
    return res.status(403).json({ error: 'forbidden', message: 'Administrator role required.' });
  next();
}

function gatewayKeyAuthorized(req) {
  if (!gatewayApiKey) return false;
  const supplied = req.get('Authorization') ?? '';
  const expected = `Bearer ${gatewayApiKey}`;
  const left = Buffer.from(supplied);
  const right = Buffer.from(expected);
  return left.length === right.length && timingSafeEqual(left, right);
}

function requireGatewayKey(req, res, next) {
  if (!gatewayKeyAuthorized(req))
    return res.status(401).json({ error: 'gateway_authentication_failed' });
  next();
}

function safeError(error) {
  const message = error instanceof Error ? error.message : '';
  const safePrefixes =
    /^(name |at least one |serviceId |upstream URL |path |matchMode |methods |timeoutMs |rateLimit |rateLimitMode |enabled adaptive |minimumRateLimit |maximumRateLimit |spikeSensitivity |recoveryCooldownMs |cacheTtlMs |rewritePrefix |weight |limit must |expiresInSeconds |active administrator override |username |password |role )/i;
  return safePrefixes.test(message) ? message : 'Request could not be completed.';
}

function attemptKey(req, username) {
  return `${req.socket.remoteAddress ?? 'unknown'}|${username.toLowerCase()}`;
}

function loginLocked(key, now = Date.now()) {
  const current = loginAttempts.get(key);
  if (!current || (current.lockedUntil <= now && now - current.windowStart >= LOGIN_WINDOW_MS)) {
    loginAttempts.delete(key);
    return 0;
  }
  return Math.max(0, current.lockedUntil - now);
}

function failLogin(key, now = Date.now()) {
  let current = loginAttempts.get(key);
  if (!current || now - current.windowStart >= LOGIN_WINDOW_MS)
    current = { failures: 0, windowStart: now, lockedUntil: 0 };
  current.failures++;
  if (current.failures >= LOGIN_MAX_FAILURES) current.lockedUntil = now + LOGIN_WINDOW_MS;
  loginAttempts.set(key, current);
  if (loginAttempts.size > 10000) {
    for (const [candidate, attempt] of loginAttempts)
      if (now - attempt.windowStart >= LOGIN_WINDOW_MS) loginAttempts.delete(candidate);
    if (loginAttempts.size > 10000) loginAttempts.delete(loginAttempts.keys().next().value);
  }
  return Math.max(0, current.lockedUntil - now);
}

const auditableFields = new Set([
  'name',
  'serviceId',
  'path',
  'matchMode',
  'methods',
  'stripPrefix',
  'rewritePrefix',
  'timeoutMs',
  'rateLimit',
  'rateLimitMode',
  'adaptiveEnabled',
  'minimumRateLimit',
  'maximumRateLimit',
  'spikeSensitivity',
  'recoveryCooldownMs',
  'cacheTtlMs',
  'enabled',
  'weight',
  'role',
  'limit',
  'expiresInSeconds',
]);

function auditMiddleware(req, res, next) {
  if (
    !req.auth ||
    !['POST', 'PUT', 'PATCH', 'DELETE'].includes(req.method) ||
    req.path.startsWith('/auth/')
  )
    return next();
  res.once('finish', () => {
    if (res.statusCode < 200 || res.statusCode >= 400) return;
    const changes = {};
    for (const [key, value] of Object.entries(req.body ?? {})) {
      if (!auditableFields.has(key)) continue;
      changes[key] =
        key === 'upstreams' && Array.isArray(value)
          ? `${value.length} upstream instance(s)`
          : value;
    }
    const action = { POST: 'CREATE', PUT: 'UPDATE', PATCH: 'UPDATE', DELETE: 'DELETE' }[req.method];
    try {
      db.prepare(
        'INSERT INTO audit_logs(actorId,actorUsername,action,resource,changes,createdAt) VALUES(?,?,?,?,?,?)',
      ).run(
        req.auth.id,
        req.auth.username,
        action,
        req.path.slice(0, 256),
        JSON.stringify(changes),
        Date.now(),
      );
    } catch {
      /* Audit failures must not expose internal errors to the request. */
    }
  });
  next();
}

async function gatewayFetch(path, init = {}) {
  const headers = new Headers(init.headers ?? {});
  headers.set('Authorization', `Bearer ${gatewayApiKey}`);
  return fetch(`${gatewayURL}${path}`, { ...init, headers });
}

function backendURL(value) {
  let url;
  try {
    url = new URL(value);
  } catch {
    throw new Error('upstream URL must be absolute HTTP or HTTPS');
  }
  if (
    !['http:', 'https:'].includes(url.protocol) ||
    !url.hostname ||
    url.username ||
    url.password ||
    url.search ||
    url.hash
  )
    throw new Error('upstream URL must be HTTP(S) without credentials, query, or fragment');
  return url.toString().replace(/\/$/, '');
}

function routeInput(body) {
  const route = {
    serviceId: String(body.serviceId ?? ''),
    path: String(body.path ?? ''),
    matchMode: body.matchMode ?? 'PREFIX',
    methods: body.methods ?? ['GET'],
    stripPrefix: body.stripPrefix === true,
    rewritePrefix: body.rewritePrefix || null,
    timeoutMs: Number(body.timeoutMs ?? 10000),
    rateLimit: Number(body.rateLimit ?? 0),
    rateLimitMode: String(body.rateLimitMode ?? 'FIXED').toUpperCase(),
    adaptiveEnabled: body.adaptiveEnabled === true,
    minimumRateLimit: Number(
      body.minimumRateLimit ??
        (Number(body.rateLimit ?? 0) > 0
          ? Math.max(1, Math.ceil(Number(body.rateLimit) * 0.2))
          : 0),
    ),
    maximumRateLimit: Number(body.maximumRateLimit ?? body.rateLimit ?? 0),
    spikeSensitivity: Number(body.spikeSensitivity ?? 2),
    recoveryCooldownMs: Number(body.recoveryCooldownMs ?? 30000),
    cacheTtlMs: Number(body.cacheTtlMs ?? 0),
    enabled: body.enabled !== false,
  };
  if (!db.prepare('SELECT 1 FROM services WHERE id=?').get(route.serviceId))
    throw new Error('serviceId must identify a registered service');
  if (!db.prepare('SELECT 1 FROM upstreams WHERE serviceId=? AND enabled=1').get(route.serviceId))
    throw new Error('service must have an enabled upstream');
  if (!route.path.startsWith('/') || /[?#\\]/.test(route.path))
    throw new Error('path must be absolute and must not contain query, fragment, or backslash');
  if (!['PREFIX', 'EXACT'].includes(route.matchMode))
    throw new Error('matchMode must be PREFIX or EXACT');
  if (
    !Array.isArray(route.methods) ||
    route.methods.length === 0 ||
    route.methods.some((m) => typeof m !== 'string' || !/^[A-Z][A-Z0-9_-]*$/.test(m))
  )
    throw new Error('methods must be a non-empty array of uppercase HTTP methods');
  if (!Number.isInteger(route.timeoutMs) || route.timeoutMs < 1 || route.timeoutMs > 300000)
    throw new Error('timeoutMs must be between 1 and 300000');
  if (!Number.isInteger(route.rateLimit) || route.rateLimit < 0 || route.rateLimit > 1000000)
    throw new Error('rateLimit must be between 0 and 1000000');
  if (!['FIXED', 'ADAPTIVE'].includes(route.rateLimitMode))
    throw new Error('rateLimitMode must be FIXED or ADAPTIVE');
  if (typeof body.adaptiveEnabled !== 'undefined' && typeof body.adaptiveEnabled !== 'boolean')
    throw new Error('adaptiveEnabled must be a boolean');
  if (route.adaptiveEnabled && (route.rateLimitMode !== 'ADAPTIVE' || route.rateLimit < 1))
    throw new Error('enabled adaptive mode requires ADAPTIVE mode and a positive base limit');
  if (
    !Number.isInteger(route.minimumRateLimit) ||
    route.minimumRateLimit < 0 ||
    route.minimumRateLimit > route.rateLimit
  )
    throw new Error('minimumRateLimit must be between 0 and the base rateLimit');
  if (
    !Number.isInteger(route.maximumRateLimit) ||
    route.maximumRateLimit < Math.max(route.minimumRateLimit, route.rateLimit) ||
    route.maximumRateLimit > 1000000
  )
    throw new Error(
      'maximumRateLimit must be at least the base and minimum rates and no greater than 1000000',
    );
  if (
    !Number.isFinite(route.spikeSensitivity) ||
    route.spikeSensitivity < 1.1 ||
    route.spikeSensitivity > 10
  )
    throw new Error('spikeSensitivity must be between 1.1 and 10');
  if (
    !Number.isInteger(route.recoveryCooldownMs) ||
    route.recoveryCooldownMs < 1000 ||
    route.recoveryCooldownMs > 3600000
  )
    throw new Error('recoveryCooldownMs must be between 1000 and 3600000');
  if (!Number.isInteger(route.cacheTtlMs) || route.cacheTtlMs < 0 || route.cacheTtlMs > 3600000)
    throw new Error('cacheTtlMs must be between 0 and 3600000');
  if (
    route.rewritePrefix &&
    (!route.rewritePrefix.startsWith('/') || /[?#\\]/.test(route.rewritePrefix))
  )
    throw new Error('rewritePrefix must be an absolute path');
  route.methods = [...new Set(route.methods)];
  return route;
}

function snapshot({ nextVersion = true } = {}) {
  const routes = db
    .prepare('SELECT * FROM routes ORDER BY length(path) DESC, path')
    .all()
    .map((r) => ({
      id: r.id,
      serviceId: r.serviceId,
      path: r.path,
      matchMode: r.matchMode,
      methods: JSON.parse(r.methods),
      stripPrefix: !!r.stripPrefix,
      ...(r.rewritePrefix ? { rewritePrefix: r.rewritePrefix } : {}),
      timeoutMs: r.timeoutMs,
      rateLimit: r.rateLimit,
      rateLimitMode: r.rateLimitMode,
      adaptiveEnabled: !!r.adaptiveEnabled,
      minimumRateLimit:
        r.minimumRateLimit || (r.rateLimit > 0 ? Math.max(1, Math.ceil(r.rateLimit * 0.2)) : 0),
      maximumRateLimit: r.maximumRateLimit || r.rateLimit,
      spikeSensitivity: r.spikeSensitivity || 2,
      recoveryCooldownMs: r.recoveryCooldownMs || 30000,
      ...(() => {
        const override = db
          .prepare(
            'SELECT limitValue,expiresAt FROM rate_limit_overrides WHERE routeId=? AND expiresAt>?',
          )
          .get(r.id, Date.now());
        return override
          ? {
              manualOverrideLimit: override.limitValue,
              manualOverrideExpiresAt: override.expiresAt,
            }
          : {};
      })(),
      cacheTtlMs: r.cacheTtlMs,
      enabled: !!r.enabled,
      targets: db
        .prepare('SELECT url,weight FROM upstreams WHERE serviceId=? AND enabled=1 ORDER BY rowid')
        .all(r.serviceId),
    }));
  if (routes.length > 1000)
    throw new Error('Gateway configuration may contain at most 1000 routes');
  for (const route of routes)
    if (!route.targets.length)
      throw new Error(`Service ${route.serviceId} has no enabled upstreams`);
  const currentVersion = Number(
    db.prepare("SELECT value FROM metadata WHERE key='version'").get()?.value ?? 0,
  );
  return { version: String(currentVersion + (nextVersion ? 1 : 0)), routes };
}
db.exec(
  "CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL); INSERT OR IGNORE INTO metadata VALUES ('version','0')",
);
function publish() {
  const config = snapshot();
  const temporary = `${configFile}.${process.pid}.tmp`;
  mkdirSync(dirname(configFile), { recursive: true });
  writeFileSync(temporary, JSON.stringify(config, null, 2), { mode: 0o600 });
  renameSync(temporary, configFile);
  db.prepare("UPDATE metadata SET value=? WHERE key='version'").run(config.version);
  return config.version;
}
function record(req, res, next) {
  const start = Date.now();
  res.on('finish', () =>
    db
      .prepare(
        'INSERT INTO requests(id,method,path,status,durationMs,createdAt) VALUES(?,?,?,?,?,?)',
      )
      .run(randomUUID(), req.method, req.path, res.statusCode, Date.now() - start, Date.now()),
  );
  next();
}
db.exec(
  'CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY,method TEXT,path TEXT,status INTEGER,durationMs INTEGER,createdAt INTEGER)',
);
app.use(record);
app.get('/health', (_req, res) =>
  res.json({
    status: 'ok',
    version: db.prepare("SELECT value FROM metadata WHERE key='version'").get().value,
  }),
);
app.get('/api/setup/status', (_req, res) =>
  res.json({
    needsInitialAdmin: !db.prepare("SELECT 1 FROM users WHERE role='ADMIN' LIMIT 1").get(),
  }),
);
app.post('/api/auth/login', async (req, res) => {
  const username = typeof req.body?.username === 'string' ? req.body.username.trim() : '';
  const password = typeof req.body?.password === 'string' ? req.body.password : '';
  if (!username || username.length > 64 || !password || password.length > 128)
    return res
      .status(400)
      .json({ error: 'credentials_required', message: 'Enter a valid username and password.' });
  const key = attemptKey(req, username);
  const lockedFor = loginLocked(key);
  if (lockedFor > 0) {
    res.set('Retry-After', String(Math.ceil(lockedFor / 1000)));
    return res.status(429).json({
      error: 'login_temporarily_locked',
      message: 'Too many failed attempts. Try again later.',
    });
  }
  const account = db
    .prepare('SELECT id,username,passwordHash,role FROM users WHERE username=?')
    .get(username);
  let valid = false;
  try {
    valid = await verifyPassword(password, account?.passwordHash ?? dummyPasswordHash);
  } catch {
    valid = false;
  }
  if (!account || !valid) {
    const retryAfter = failLogin(key);
    if (retryAfter > 0) {
      res.set('Retry-After', String(Math.ceil(retryAfter / 1000)));
      return res.status(429).json({
        error: 'login_temporarily_locked',
        message: 'Too many failed attempts. Try again later.',
      });
    }
    return res
      .status(401)
      .json({ error: 'invalid_credentials', message: 'Username or password is incorrect.' });
  }
  loginAttempts.delete(key);
  const { token, payload } = createAccessToken({
    userId: account.id,
    username: account.username,
    role: account.role,
    secret: jwtSecret,
  });
  db.prepare('INSERT INTO sessions(jti,userId,expiresAt,revokedAt) VALUES(?,?,?,NULL)').run(
    payload.jti,
    account.id,
    payload.exp * 1000,
  );
  res.json({
    accessToken: token,
    tokenType: 'Bearer',
    expiresAt: payload.exp * 1000,
    user: { id: account.id, username: account.username, role: account.role },
  });
});
app.post('/api/auth/logout', authenticate, (req, res) => {
  db.prepare('UPDATE sessions SET revokedAt=? WHERE jti=? AND revokedAt IS NULL').run(
    Date.now(),
    req.auth.jti,
  );
  res.status(204).end();
});
app.get('/api/auth/me', authenticate, (req, res) =>
  res.json({ id: req.auth.id, username: req.auth.username, role: req.auth.role }),
);
app.use('/api', authenticate);
app.use('/api', (req, res, next) => {
  if (!['GET', 'HEAD', 'OPTIONS'].includes(req.method) && req.auth.role !== 'ADMIN')
    return res.status(403).json({ error: 'forbidden', message: 'Administrator role required.' });
  next();
});
app.use('/api', auditMiddleware);
app.get('/internal/gateway/config', requireGatewayKey, (req, res) => {
  const gatewayId = req.get('X-Gateway-ID') ?? '';
  if (!/^[A-Za-z0-9_.-]{1,64}$/.test(gatewayId))
    return res.status(400).json({ error: 'invalid_gateway_id' });
  const config = snapshot({ nextVersion: false });
  const etag = `"${config.version}"`;
  res.set('ETag', etag);
  if (req.get('If-None-Match') === etag) return res.status(304).end();
  res.json(config);
});
app.post('/internal/gateway/config/ack', requireGatewayKey, (req, res) => {
  const gatewayId = req.get('X-Gateway-ID') ?? '';
  const { version, status, message = '' } = req.body ?? {};
  if (
    !/^[A-Za-z0-9_.-]{1,64}$/.test(gatewayId) ||
    !Number.isSafeInteger(version) ||
    version < 1 ||
    !['APPLIED', 'REJECTED'].includes(status) ||
    typeof message !== 'string'
  )
    return res.status(400).json({ error: 'invalid_configuration_acknowledgement' });
  db.prepare(
    'INSERT INTO gateway_config_acks(gatewayId,version,status,message,updatedAt) VALUES(?,?,?,?,?) ON CONFLICT(gatewayId,version) DO UPDATE SET status=excluded.status,message=excluded.message,updatedAt=excluded.updatedAt',
  ).run(gatewayId, version, status, message.slice(0, 512), Date.now());
  res.status(204).end();
});
app.get('/api/services', (_req, res) =>
  res.json(
    db
      .prepare(
        'SELECT s.*, (SELECT count(*) FROM routes r WHERE r.serviceId=s.id) AS routeCount FROM services s ORDER BY name',
      )
      .all()
      .map((s) => ({
        ...s,
        upstreams: db
          .prepare('SELECT * FROM upstreams WHERE serviceId=?')
          .all(s.id)
          .map((u) => ({ ...u, enabled: !!u.enabled })),
      })),
  ),
);
app.post('/api/services', (req, res) => {
  try {
    const id = randomUUID();
    const name = String(req.body.name ?? '').trim();
    const urls = req.body.upstreams ?? (req.body.backendUrl ? [req.body.backendUrl] : []);
    if (!name || !Array.isArray(urls) || !urls.length)
      return res.status(400).json({ error: 'name and at least one upstream are required' });
    const normalized = urls.map(backendURL);
    db.prepare('INSERT INTO services(id,name) VALUES(?,?)').run(id, name);
    for (const url of normalized)
      db.prepare('INSERT INTO upstreams(id,serviceId,url) VALUES(?,?,?)').run(
        randomUUID(),
        id,
        url,
      );
    res.status(201).json(db.prepare('SELECT * FROM services WHERE id=?').get(id));
  } catch (error) {
    res.status(400).json({ error: safeError(error) });
  }
});
app.put('/api/services/:id', (req, res) => {
  const found = db.prepare('SELECT * FROM services WHERE id=?').get(req.params.id);
  if (!found) return res.status(404).json({ error: 'Service not found' });
  try {
    const name = String(req.body.name ?? found.name).trim();
    if (!name) throw new Error('name is required');
    const urls = req.body.upstreams === undefined ? null : req.body.upstreams.map(backendURL);
    if (urls && urls.length === 0) throw new Error('at least one upstream is required');
    db.exec('BEGIN IMMEDIATE');
    db.prepare('UPDATE services SET name=? WHERE id=?').run(name, found.id);
    if (urls) {
      db.prepare('DELETE FROM upstreams WHERE serviceId=?').run(found.id);
      for (const url of urls)
        db.prepare('INSERT INTO upstreams(id,serviceId,url) VALUES(?,?,?)').run(
          randomUUID(),
          found.id,
          url,
        );
    }
    const version = publish();
    db.exec('COMMIT');
    res.json({ id: found.id, name, version });
  } catch (error) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* Rollback is a no-op if the transaction never began. */
    }
    res.status(400).json({ error: safeError(error) });
  }
});
app.get('/api/upstreams', (_req, res) =>
  res.json(
    db
      .prepare('SELECT u.*,s.name serviceName FROM upstreams u JOIN services s ON s.id=u.serviceId')
      .all()
      .map((u) => ({ ...u, enabled: !!u.enabled })),
  ),
);
app.post('/api/services/:id/upstreams', (req, res) => {
  try {
    const id = randomUUID();
    const service = db.prepare('SELECT id FROM services WHERE id=?').get(req.params.id);
    if (!service) return res.status(404).json({ error: 'Service not found' });
    const url = backendURL(req.body.url);
    const weight = Number(req.body.weight ?? 1);
    if (!Number.isInteger(weight) || weight < 1 || weight > 1000)
      throw new Error('weight must be between 1 and 1000');
    db.exec('BEGIN IMMEDIATE');
    db.prepare('INSERT INTO upstreams(id,serviceId,url,weight) VALUES(?,?,?,?)').run(
      id,
      service.id,
      url,
      weight,
    );
    const version = publish();
    db.exec('COMMIT');
    res.status(201).json({ id, serviceId: req.params.id, url, weight, enabled: true, version });
  } catch (error) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* Rollback is a no-op if the transaction never began. */
    }
    res.status(400).json({ error: safeError(error) });
  }
});
app.put('/api/upstreams/:id', (req, res) => {
  try {
    if (typeof req.body.enabled !== 'boolean') throw new Error('enabled must be a boolean');
    db.exec('BEGIN IMMEDIATE');
    const result = db
      .prepare('UPDATE upstreams SET enabled=? WHERE id=?')
      .run(req.body.enabled ? 1 : 0, req.params.id);
    if (!result.changes) {
      db.exec('ROLLBACK');
      return res.status(404).json({ error: 'Upstream not found' });
    }
    const version = publish();
    db.exec('COMMIT');
    res.json({ version });
  } catch (error) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* Rollback is a no-op if the transaction never began. */
    }
    res.status(400).json({ error: safeError(error) });
  }
});
app.delete('/api/upstreams/:id', (req, res) => {
  try {
    db.exec('BEGIN IMMEDIATE');
    db.prepare('DELETE FROM upstreams WHERE id=?').run(req.params.id);
    const version = publish();
    db.exec('COMMIT');
    res.set('X-Configuration-Version', version).status(204).end();
  } catch (error) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* Rollback is a no-op if the transaction never began. */
    }
    res.status(400).json({ error: safeError(error) });
  }
});
app.get('/api/routes', (_req, res) =>
  res.json(
    db
      .prepare(
        'SELECT r.*,s.name serviceName FROM routes r JOIN services s ON s.id=r.serviceId ORDER BY r.path',
      )
      .all()
      .map((r) => ({
        ...r,
        methods: JSON.parse(r.methods),
        enabled: !!r.enabled,
        stripPrefix: !!r.stripPrefix,
        adaptiveEnabled: !!r.adaptiveEnabled,
        minimumRateLimit:
          r.minimumRateLimit || (r.rateLimit > 0 ? Math.max(1, Math.ceil(r.rateLimit * 0.2)) : 0),
        maximumRateLimit: r.maximumRateLimit || r.rateLimit,
        spikeSensitivity: r.spikeSensitivity || 2,
        recoveryCooldownMs: r.recoveryCooldownMs || 30000,
        service: {
          id: r.serviceId,
          name: r.serviceName,
          upstreams: db
            .prepare('SELECT * FROM upstreams WHERE serviceId=?')
            .all(r.serviceId)
            .map((u) => ({ ...u, enabled: !!u.enabled })),
        },
      })),
  ),
);
app.post('/api/routes', (req, res) => {
  try {
    const b = routeInput(req.body);
    const id = randomUUID();
    db.exec('BEGIN IMMEDIATE');
    db.prepare(
      'INSERT INTO routes(id,serviceId,path,matchMode,methods,stripPrefix,rewritePrefix,timeoutMs,rateLimit,rateLimitMode,adaptiveEnabled,minimumRateLimit,maximumRateLimit,spikeSensitivity,recoveryCooldownMs,cacheTtlMs,enabled) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
    ).run(
      id,
      b.serviceId,
      b.path,
      b.matchMode,
      JSON.stringify(b.methods),
      b.stripPrefix ? 1 : 0,
      b.rewritePrefix,
      b.timeoutMs,
      b.rateLimit,
      b.rateLimitMode,
      b.adaptiveEnabled ? 1 : 0,
      b.minimumRateLimit,
      b.maximumRateLimit,
      b.spikeSensitivity,
      b.recoveryCooldownMs,
      b.cacheTtlMs,
      b.enabled ? 1 : 0,
    );
    const version = publish();
    db.exec('COMMIT');
    res.status(201).json({ ...b, id, version });
  } catch (e) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* Rollback is a no-op if the transaction never began. */
    }
    res.status(400).json({ error: safeError(e) });
  }
});
app.put('/api/routes/:id', (req, res) => {
  try {
    if (!db.prepare('SELECT 1 FROM routes WHERE id=?').get(req.params.id))
      return res.status(404).json({ error: 'Route not found' });
    const b = routeInput(req.body);
    const override = db
      .prepare('SELECT limitValue,expiresAt FROM rate_limit_overrides WHERE routeId=?')
      .get(req.params.id);
    const minimum =
      b.minimumRateLimit || (b.rateLimit > 0 ? Math.max(1, Math.ceil(b.rateLimit * 0.2)) : 0);
    if (
      override &&
      override.expiresAt > Date.now() &&
      (override.limitValue < minimum || override.limitValue > b.maximumRateLimit)
    )
      throw new Error(
        'active administrator override would be outside the updated rate-limit bounds; clear or replace it first',
      );
    db.exec('BEGIN IMMEDIATE');
    db.prepare(
      'UPDATE routes SET serviceId=?,path=?,matchMode=?,methods=?,stripPrefix=?,rewritePrefix=?,timeoutMs=?,rateLimit=?,rateLimitMode=?,adaptiveEnabled=?,minimumRateLimit=?,maximumRateLimit=?,spikeSensitivity=?,recoveryCooldownMs=?,cacheTtlMs=?,enabled=? WHERE id=?',
    ).run(
      b.serviceId,
      b.path,
      b.matchMode,
      JSON.stringify(b.methods),
      b.stripPrefix ? 1 : 0,
      b.rewritePrefix,
      b.timeoutMs,
      b.rateLimit,
      b.rateLimitMode,
      b.adaptiveEnabled ? 1 : 0,
      b.minimumRateLimit,
      b.maximumRateLimit,
      b.spikeSensitivity,
      b.recoveryCooldownMs,
      b.cacheTtlMs,
      b.enabled ? 1 : 0,
      req.params.id,
    );
    const version = publish();
    db.exec('COMMIT');
    res.json({ ...b, id: req.params.id, version });
  } catch (e) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* Rollback is a no-op if the transaction never began. */
    }
    res.status(400).json({ error: safeError(e) });
  }
});
app.delete('/api/routes/:id', (req, res) => {
  try {
    db.exec('BEGIN IMMEDIATE');
    db.prepare('DELETE FROM routes WHERE id=?').run(req.params.id);
    const version = publish();
    db.exec('COMMIT');
    res.set('X-Configuration-Version', version).status(204).end();
  } catch (error) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* Rollback is a no-op if the transaction never began. */
    }
    res.status(400).json({ error: safeError(error) });
  }
});
app.post('/api/routes/:id/adaptive/override', (req, res) => {
  try {
    const route = db.prepare('SELECT * FROM routes WHERE id=?').get(req.params.id);
    if (!route) return res.status(404).json({ error: 'Route not found' });
    const limit = Number(req.body.limit);
    const expiresInSeconds = Number(req.body.expiresInSeconds);
    const minimum =
      route.minimumRateLimit ||
      (route.rateLimit > 0 ? Math.max(1, Math.ceil(route.rateLimit * 0.2)) : 1);
    const maximum = route.maximumRateLimit || route.rateLimit;
    if (!Number.isInteger(limit) || limit < minimum || limit > maximum)
      throw new Error(`limit must be between ${minimum} and ${maximum}`);
    if (!Number.isInteger(expiresInSeconds) || expiresInSeconds < 1 || expiresInSeconds > 86400)
      throw new Error('expiresInSeconds must be between 1 and 86400');
    const expiresAt = Date.now() + expiresInSeconds * 1000;
    db.exec('BEGIN IMMEDIATE');
    db.prepare(
      'INSERT INTO rate_limit_overrides(routeId,limitValue,expiresAt) VALUES(?,?,?) ON CONFLICT(routeId) DO UPDATE SET limitValue=excluded.limitValue, expiresAt=excluded.expiresAt',
    ).run(req.params.id, limit, expiresAt);
    const version = publish();
    db.exec('COMMIT');
    res.json({ routeId: req.params.id, limit, expiresAt, version });
  } catch (error) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* No open transaction. */
    }
    res.status(400).json({ error: safeError(error) });
  }
});
app.delete('/api/routes/:id/adaptive/override', (req, res) => {
  if (!db.prepare('SELECT 1 FROM routes WHERE id=?').get(req.params.id))
    return res.status(404).json({ error: 'Route not found' });
  try {
    db.exec('BEGIN IMMEDIATE');
    db.prepare('DELETE FROM rate_limit_overrides WHERE routeId=?').run(req.params.id);
    const version = publish();
    db.exec('COMMIT');
    res.json({ routeId: req.params.id, automatic: true, version });
  } catch (error) {
    try {
      db.exec('ROLLBACK');
    } catch {
      /* No open transaction. */
    }
    res.status(400).json({ error: safeError(error) });
  }
});
app.get('/api/access/users', adminOnly, (_req, res) =>
  res.json(db.prepare('SELECT id,username,role,createdAt FROM users ORDER BY createdAt').all()),
);
app.post('/api/access/users', adminOnly, async (req, res) => {
  const username = typeof req.body?.username === 'string' ? req.body.username.trim() : '';
  const password = typeof req.body?.password === 'string' ? req.body.password : '';
  const role = req.body?.role;
  if (!/^[A-Za-z0-9_.@-]{3,64}$/.test(username))
    return res.status(400).json({ error: 'username must be 3-64 valid characters' });
  if (password.length < 12 || password.length > 128)
    return res.status(400).json({ error: 'password must be 12-128 characters' });
  if (!['ADMIN', 'VIEWER'].includes(role))
    return res.status(400).json({ error: 'role must be ADMIN or VIEWER' });
  try {
    const id = randomUUID();
    db.prepare('INSERT INTO users(id,username,passwordHash,role,createdAt) VALUES(?,?,?,?,?)').run(
      id,
      username,
      await hashPassword(password),
      role,
      Date.now(),
    );
    res
      .status(201)
      .json(db.prepare('SELECT id,username,role,createdAt FROM users WHERE id=?').get(id));
  } catch {
    res.status(409).json({
      error: 'account_conflict',
      message: 'An account with that username already exists.',
    });
  }
});
app.patch('/api/access/users/:id', adminOnly, (req, res) => {
  const role = req.body?.role;
  if (!['ADMIN', 'VIEWER'].includes(role))
    return res.status(400).json({ error: 'role must be ADMIN or VIEWER' });
  const user = db
    .prepare('SELECT id,username,role,createdAt FROM users WHERE id=?')
    .get(req.params.id);
  if (!user) return res.status(404).json({ error: 'user_not_found' });
  if (
    user.role === 'ADMIN' &&
    role === 'VIEWER' &&
    db.prepare("SELECT count(*) n FROM users WHERE role='ADMIN'").get().n <= 1
  )
    return res
      .status(409)
      .json({ error: 'last_administrator', message: 'The last administrator cannot be demoted.' });
  db.prepare('UPDATE users SET role=? WHERE id=?').run(role, user.id);
  db.prepare('UPDATE sessions SET revokedAt=? WHERE userId=? AND revokedAt IS NULL').run(
    Date.now(),
    user.id,
  );
  res.json({ ...user, role });
});
app.get('/api/access/audit', adminOnly, (req, res) => {
  const rawLimit = Number(req.query.limit ?? 100);
  const limit = Number.isInteger(rawLimit) ? Math.max(1, Math.min(500, rawLimit)) : 100;
  res.json(
    db
      .prepare(
        'SELECT id,actorId,actorUsername,action,resource,changes,createdAt FROM audit_logs ORDER BY id DESC LIMIT ?',
      )
      .all(limit)
      .map((event) => ({ ...event, changes: JSON.parse(event.changes) })),
  );
});
app.get('/api/gateway/config', (_req, res) => res.json(snapshot({ nextVersion: false })));
app.get('/api/gateway/adaptive', async (_req, res) => {
  try {
    const response = await gatewayFetch('/api/gateway/adaptive');
    if (!response.ok)
      return res.status(503).json({ error: 'Gateway adaptive metrics unavailable' });
    res.json(await response.json());
  } catch {
    res.status(503).json({ error: 'Gateway adaptive metrics unavailable' });
  }
});
app.get('/api/gateway/adaptive/history', async (_req, res) => {
  try {
    const response = await gatewayFetch('/api/gateway/adaptive/history');
    if (!response.ok)
      return res.status(503).json({ error: 'Gateway adaptive history unavailable' });
    res.json(await response.json());
  } catch {
    res.status(503).json({ error: 'Gateway adaptive history unavailable' });
  }
});
app.get('/api/gateways', async (_req, res) => {
  try {
    const response = await gatewayFetch('/health/config');
    if (!response.ok) throw new Error('gateway unavailable');
    const health = await response.json();
    const gatewayId = process.env.GATEWAY_ID ?? 'local-gateway';
    const acknowledgement = db
      .prepare(
        'SELECT status,message,updatedAt FROM gateway_config_acks WHERE gatewayId=? ORDER BY version DESC LIMIT 1',
      )
      .get(gatewayId);
    res.json([
      {
        id: gatewayId,
        name: 'Local gateway',
        lastSeenAt: new Date().toISOString(),
        currentVersion: Number(health.version),
        latestVersion: Number(
          db.prepare("SELECT value FROM metadata WHERE key='version'").get().value,
        ),
        synced:
          Number(health.version) ===
          Number(db.prepare("SELECT value FROM metadata WHERE key='version'").get().value),
        latestAcknowledgement: acknowledgement
          ? {
              status: acknowledgement.status,
              message: acknowledgement.message || null,
              updatedAt: acknowledgement.updatedAt,
            }
          : null,
      },
    ]);
  } catch {
    res.json([]);
  }
});
app.get('/api/gateway/metrics', async (_req, res) => {
  try {
    const response = await gatewayFetch('/api/gateway/metrics');
    if (!response.ok) return res.status(503).json({ error: 'Gateway metrics unavailable' });
    const metrics = await response.json();
    res.json({
      ...metrics,
      activeRoutes: db.prepare('SELECT count(*) n FROM routes WHERE enabled=1').get().n,
    });
  } catch {
    res.status(503).json({ error: 'Gateway metrics unavailable' });
  }
});
app.get('/api/analytics', (_req, res) =>
  res.json({
    totalRequests: db.prepare('SELECT count(*) n FROM requests').get().n,
    averageResponseMs: db.prepare('SELECT coalesce(avg(durationMs),0) n FROM requests').get().n,
    errors: db.prepare('SELECT count(*) n FROM requests WHERE status>=500').get().n,
    requestsPerSecond: db
      .prepare('SELECT count(*)/60.0 n FROM requests WHERE createdAt > ?')
      .get(Date.now() - 60000).n,
  }),
);
app.get('/api/gateway/health', async (_req, res) => {
  try {
    const response = await gatewayFetch('/api/gateway/health');
    if (!response.ok) return res.status(503).json({ error: 'Gateway health unavailable' });
    res.json(await response.json());
  } catch {
    res.status(503).json({ error: 'Gateway health unavailable' });
  }
});
app.use((_req, res) =>
  res.status(404).json({ error: 'not_found', message: 'The requested endpoint does not exist.' }),
);
app.use((error, _req, res, next) => {
  void next;
  const status =
    Number.isInteger(error?.status) && error.status >= 400 && error.status < 500
      ? error.status
      : 500;
  res.status(status).json({
    error: status === 400 ? 'invalid_request' : 'request_failed',
    message:
      status === 400 ? 'Request body is invalid or too large.' : 'Request could not be completed.',
  });
});
const port = Number(process.env.CONTROL_API_PORT ?? process.env.PORT ?? 8081);
const server = app.listen(port, '0.0.0.0', () =>
  console.log(`Control API listening on ${port}; SQLite ${dbPath}`),
);
export { server, db };
for (const signal of ['SIGINT', 'SIGTERM'])
  process.on(signal, () =>
    server.close(() => {
      db.close();
      process.exit(0);
    }),
  );
