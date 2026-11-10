import test from 'node:test';
import assert from 'node:assert/strict';
import {
  hashPassword,
  verifyPassword,
  createAccessToken,
  verifyAccessToken,
  ACCESS_TOKEN_SECONDS,
} from '../src/security.js';

const secret = 'unit-test-signing-secret-that-is-long-enough';
test('passwords are bcrypt-hashed and verified without storing plaintext', async () => {
  const password = 'long secure password 123';
  const hash = await hashPassword(password);
  assert.match(hash, /^\$2[aby]\$/);
  assert.notEqual(hash, password);
  assert.equal(await verifyPassword(password, hash), true);
  assert.equal(await verifyPassword('wrong password', hash), false);
});
test('JWT tokens reject expiry, tampering and invalid signatures', () => {
  const now = 1_800_000_000_000;
  const { token } = createAccessToken({
    userId: 'user-1',
    username: 'admin',
    role: 'ADMIN',
    secret,
    now,
  });
  assert.equal(verifyAccessToken(token, secret, now).role, 'ADMIN');
  assert.throws(() => verifyAccessToken(token, secret, now + ACCESS_TOKEN_SECONDS * 1000));
  assert.throws(() => verifyAccessToken(`${token.slice(0, -1)}x`, secret, now));
  assert.throws(() => verifyAccessToken(token, 'another-secret-that-is-long-enough', now));
});
