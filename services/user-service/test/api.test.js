import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createApp } from '../src/main.js';

let server;
let baseURL;

before(async () => {
  server = createApp();
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  baseURL = `http://127.0.0.1:${server.address().port}`;
});

after(async () => {
  await new Promise((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
});

test('user service lists, reads, creates, and validates users', async () => {
  const health = await fetch(`${baseURL}/health`).then((response) => response.json());
  assert.equal(health.persistence, 'in-memory');

  const listResponse = await fetch(`${baseURL}/users`);
  const users = await listResponse.json();
  assert.equal(listResponse.status, 200);
  assert.equal(users.length, 2);

  const detail = await fetch(`${baseURL}/users/1`).then((response) => response.json());
  assert.equal(detail.name, 'Ada Lovelace');

  const createdResponse = await fetch(`${baseURL}/users`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name: '  Katherine Johnson  ' }),
  });
  const created = await createdResponse.json();
  assert.equal(createdResponse.status, 201);
  assert.equal(created.name, 'Katherine Johnson');

  const invalid = await fetch(`${baseURL}/users`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name: ' ' }),
  });
  assert.equal(invalid.status, 400);

  const invalidObject = await fetch(`${baseURL}/users`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: 'null',
  });
  assert.equal(invalidObject.status, 400);
});
