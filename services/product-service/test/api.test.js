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

test('product service lists, reads, creates, and validates products', async () => {
  const health = await fetch(`${baseURL}/health`).then((response) => response.json());
  assert.equal(health.persistence, 'in-memory');

  const listResponse = await fetch(`${baseURL}/products`);
  const products = await listResponse.json();
  assert.equal(listResponse.status, 200);
  assert.equal(products.length, 2);

  const detail = await fetch(`${baseURL}/products/1`).then((response) => response.json());
  assert.equal(detail.name, 'Gateway Starter');

  const createdResponse = await fetch(`${baseURL}/products`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name: 'New product', price: 4.5 }),
  });
  const created = await createdResponse.json();
  assert.equal(createdResponse.status, 201);
  assert.equal(created.price, 4.5);

  const invalid = await fetch(`${baseURL}/products`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name: '', price: -1 }),
  });
  assert.equal(invalid.status, 400);

  const invalidObject = await fetch(`${baseURL}/products`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: 'null',
  });
  assert.equal(invalidObject.status, 400);
});
