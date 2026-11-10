import { createServer } from 'node:http';
import { pathToFileURL } from 'node:url';

function send(response, status, body) {
  response.writeHead(status, { 'content-type': 'application/json; charset=utf-8' });
  response.end(JSON.stringify(body));
}

export function createApp() {
  const products = new Map([
    ['1', { id: '1', name: 'Gateway Starter', price: 19.99 }],
    ['2', { id: '2', name: 'Traffic Monitor', price: 49.5 }],
  ]);
  let nextId = 3;
  return createServer(async (request, response) => {
    const url = new URL(request.url ?? '/', 'http://localhost');
    if (url.pathname === '/health')
      return send(response, 200, { status: 'ok', persistence: 'in-memory' });
    if (request.method === 'GET' && ['/products', '/api/products'].includes(url.pathname))
      return send(response, 200, [...products.values()]);
    const match = url.pathname.match(/^\/(?:api\/)?products\/([^/]+)$/);
    if (request.method === 'GET' && match) {
      let id;
      try {
        id = decodeURIComponent(match[1]);
      } catch {
        return send(response, 400, { error: 'Invalid product ID' });
      }
      const product = products.get(id);
      return product
        ? send(response, 200, product)
        : send(response, 404, { error: 'Product not found' });
    }
    if (request.method === 'POST' && ['/products', '/api/products'].includes(url.pathname)) {
      let body = '';
      for await (const chunk of request) {
        body += chunk;
        if (body.length > 65536) return send(response, 413, { error: 'Request body too large' });
      }
      let input;
      try {
        input = JSON.parse(body);
      } catch {
        return send(response, 400, { error: 'Invalid JSON' });
      }
      if (
        !input ||
        typeof input !== 'object' ||
        Array.isArray(input) ||
        typeof input.name !== 'string' ||
        !input.name.trim() ||
        !Number.isFinite(input.price) ||
        input.price < 0
      )
        return send(response, 400, { error: 'name and non-negative price are required' });
      const product = { id: String(nextId++), name: input.name.trim(), price: input.price };
      products.set(product.id, product);
      return send(response, 201, product);
    }
    send(response, 404, { error: 'Not found' });
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  createApp().listen(Number(process.env.PORT ?? 4002), '0.0.0.0');
}
