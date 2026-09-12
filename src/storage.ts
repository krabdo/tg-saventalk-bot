import type { SqlStore, ActorStorage } from './actor.js';
/** Small configuration documents live beside relational message/job tables. */
export class Documents {
  constructor(private sql: SqlStore) {
    sql.exec('CREATE TABLE IF NOT EXISTS documents (key TEXT PRIMARY KEY, value TEXT NOT NULL)');
  }
  get<T>(key: string, fallback: T): T {
    const row = this.sql.exec<{ value: string }>('SELECT value FROM documents WHERE key = ?', key).toArray()[0];
    return row ? JSON.parse(row.value) as T : fallback;
  }
  set(key: string, value: unknown) {
    this.sql.exec('INSERT OR REPLACE INTO documents VALUES (?, ?)', key, JSON.stringify(value));
  }
}
export async function wake(storage: ActorStorage, at = Date.now() + 1) {
  const current = await storage.getAlarm();
  if (current === null || current > at) await storage.setAlarm(Math.max(Date.now() + 1, at));
}
export function clip(text: string, max = 4000) {
  if (text.length <= max) return text;
  const suffix = '\n…[内容已截断]';
  let end = max - suffix.length;
  if (/[\uD800-\uDBFF]/.test(text[end - 1] ?? '')) end--;
  return text.slice(0, end) + suffix;
}
