import { randomBytes, createHmac, timingSafeEqual } from 'node:crypto';
import bcrypt from 'bcryptjs';

const BCRYPT_ROUNDS = 12;
const ACCESS_TOKEN_SECONDS = 15 * 60;

export async function hashPassword(password) {
  return bcrypt.hash(password, BCRYPT_ROUNDS);
}

export async function verifyPassword(password, hash) {
  if (typeof password !== 'string' || typeof hash !== 'string') return false;
  return bcrypt.compare(password, hash);
}

export function createAccessToken({ userId, username, role, secret, now = Date.now() }) {
  const issuedAt = Math.floor(now / 1000);
  const jti = randomBytes(24).toString('base64url');
  const payload = {
    sub: userId,
    username,
    role,
    jti,
    iat: issuedAt,
    exp: issuedAt + ACCESS_TOKEN_SECONDS,
    iss: 'gatekeeper-control-api',
    aud: 'gatekeeper-dashboard',
  };
  const head = encode({ alg: 'HS256', typ: 'JWT' });
  const body = encode(payload);
  const signature = createHmac('sha256', secret).update(`${head}.${body}`).digest('base64url');
  return { token: `${head}.${body}.${signature}`, payload };
}

export function verifyAccessToken(token, secret, now = Date.now()) {
  if (typeof token !== 'string' || token.length > 4096) throw new Error('invalid_token');
  const parts = token.split('.');
  if (parts.length !== 3) throw new Error('invalid_token');
  const [head, body, signature] = parts;
  const expected = createHmac('sha256', secret).update(`${head}.${body}`).digest();
  let actual;
  try {
    actual = Buffer.from(signature, 'base64url');
  } catch {
    throw new Error('invalid_token');
  }
  if (actual.length !== expected.length || !timingSafeEqual(actual, expected))
    throw new Error('invalid_token');
  let header;
  let payload;
  try {
    header = JSON.parse(Buffer.from(head, 'base64url').toString('utf8'));
    payload = JSON.parse(Buffer.from(body, 'base64url').toString('utf8'));
  } catch {
    throw new Error('invalid_token');
  }
  const nowSeconds = Math.floor(now / 1000);
  if (
    header.alg !== 'HS256' ||
    header.typ !== 'JWT' ||
    payload.iss !== 'gatekeeper-control-api' ||
    payload.aud !== 'gatekeeper-dashboard' ||
    typeof payload.sub !== 'string' ||
    typeof payload.jti !== 'string' ||
    !['ADMIN', 'VIEWER'].includes(payload.role) ||
    !Number.isInteger(payload.exp) ||
    !Number.isInteger(payload.iat) ||
    payload.exp <= nowSeconds ||
    payload.iat > nowSeconds + 30
  )
    throw new Error(payload.exp <= nowSeconds ? 'expired_token' : 'invalid_token');
  return payload;
}

function encode(value) {
  return Buffer.from(JSON.stringify(value)).toString('base64url');
}

export { ACCESS_TOKEN_SECONDS };
