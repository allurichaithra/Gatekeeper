import { createServer } from 'node:http';
import { pathToFileURL } from 'node:url';

function send(response, status, body) {
  response.writeHead(status, { 'content-type': 'application/json; charset=utf-8' });
  response.end(JSON.stringify(body));
}

export function createApp() {
  const users = new Map([
    ['1', { id: '1', name: 'Ada Lovelace', email: 'ada@example.test' }],
    ['2', { id: '2', name: 'Grace Hopper', email: 'grace@example.test' }],
  ]);
  let nextId = 3;
  return createServer(async (request, response) => {
    const url = new URL(request.url ?? '/', 'http://localhost');
    if (url.pathname === '/health')
      return send(response, 200, { status: 'ok', persistence: 'in-memory' });
    if (request.method === 'GET' && ['/users', '/api/users'].includes(url.pathname))
      return send(response, 200, [...users.values()]);
    const match = url.pathname.match(/^\/(?:api\/)?users\/([^/]+)$/);
    if (request.method === 'GET' && match) {
      let id;
      try {
        id = decodeURIComponent(match[1]);
      } catch {
        return send(response, 400, { error: 'Invalid user ID' });
      }
      const user = users.get(id);
      return user ? send(response, 200, user) : send(response, 404, { error: 'User not found' });
    }
    if (request.method === 'POST' && ['/users', '/api/users'].includes(url.pathname)) {
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
        !input.name.trim()
      )
        return send(response, 400, { error: 'name is required' });
      const user = {
        id: String(nextId++),
        name: input.name.trim(),
        email: typeof input.email === 'string' ? input.email.trim() : null,
      };
      users.set(user.id, user);
      return send(response, 201, user);
    }
    send(response, 404, { error: 'Not found' });
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  createApp().listen(Number(process.env.PORT ?? 4001), '0.0.0.0');
}
