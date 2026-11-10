import { DatabaseSync } from 'node:sqlite';
import { randomUUID } from 'node:crypto';
import { mkdirSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { hashPassword } from './security.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const dbPath = resolve(root, process.env.SQLITE_FILE ?? 'data/gatekeeper.sqlite');
const username = String(process.env.SETUP_ADMIN_USERNAME ?? '').trim();
const password = process.env.SETUP_ADMIN_PASSWORD ?? '';
if (!/^[A-Za-z0-9_.@-]{3,64}$/.test(username)) {
  console.error('Admin username must be 3–64 letters, digits, dot, underscore, @, or hyphen.');
  process.exit(1);
}
if (password.length < 12 || password.length > 128) {
  console.error('Admin password must be 12–128 characters.');
  process.exit(1);
}

mkdirSync(dirname(dbPath), { recursive: true });
const db = new DatabaseSync(dbPath);
db.exec('PRAGMA foreign_keys=ON');
db.exec(`CREATE TABLE IF NOT EXISTS users(
  id TEXT PRIMARY KEY,
  username TEXT UNIQUE COLLATE NOCASE NOT NULL,
  passwordHash TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('ADMIN','VIEWER')),
  createdAt INTEGER NOT NULL
)`);
try {
  db.exec('BEGIN IMMEDIATE');
  if (db.prepare("SELECT 1 FROM users WHERE role='ADMIN' LIMIT 1").get())
    throw new Error(
      'An administrator already exists. Use the authenticated Access Control page to add accounts.',
    );
  db.prepare('INSERT INTO users(id,username,passwordHash,role,createdAt) VALUES(?,?,?,?,?)').run(
    randomUUID(),
    username,
    await hashPassword(password),
    'ADMIN',
    Date.now(),
  );
  db.exec('COMMIT');
  console.log(`Initial administrator '${username}' created in ${dbPath}.`);
} catch (error) {
  try {
    db.exec('ROLLBACK');
  } catch {
    /* no open transaction */
  }
  console.error(error.message);
  process.exitCode = 1;
} finally {
  db.close();
}
