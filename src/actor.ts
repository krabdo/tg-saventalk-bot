import { DatabaseSync, type SQLInputValue } from 'node:sqlite';
export class SqlStore {
  constructor(readonly db: DatabaseSync) {}
  exec<T = Record<string, unknown>>(sql: string, ...params: unknown[]) {
    const stmt = this.db.prepare(sql);
    const rows = stmt.all(...params as SQLInputValue[]) as T[];
    return { toArray: () => rows, one: () => { if (rows.length !== 1) throw new Error('Expected one row'); return rows[0]!; } };
  }
}
export class ActorStorage {
  readonly sql: SqlStore;
  constructor(readonly db: DatabaseSync) {
    db.exec('PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; CREATE TABLE IF NOT EXISTS scheduler (id INTEGER PRIMARY KEY CHECK(id=1), due INTEGER)');
    this.sql = new SqlStore(db);
  }
  async getAlarm(): Promise<number | null> { return (this.db.prepare('SELECT due FROM scheduler WHERE id=1').get()?.due as number) ?? null; }
  async setAlarm(at: number) { this.db.prepare('INSERT OR REPLACE INTO scheduler VALUES (1, ?)').run(at); }
  async deleteAlarm() { this.db.exec('DELETE FROM scheduler'); }
  async sync() { /* SQLite FULL commits are already durable. */ }
  transactionSync<T>(fn: () => T): T {
    this.db.exec('BEGIN IMMEDIATE');
    try { const result = fn(); this.db.exec('COMMIT'); return result; }
    catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }
}
export interface ActorState { storage: ActorStorage }
export class LocalActor<E> {
  constructor(public readonly ctx: ActorState, protected readonly env: E) {}
}
