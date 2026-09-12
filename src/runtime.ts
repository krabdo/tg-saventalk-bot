import { DatabaseSync, type SQLInputValue } from 'node:sqlite';
import { mkdirSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { Account } from './account.js';
import { Conversation } from './conversation.js';
import { AiJob } from './ai-job.js';
import type { AppEnv } from './types.js';

import { ActorStorage } from './actor.js';
type Actor = Account | Conversation | AiJob;
export class Runtime {
  readonly actors = new Map<string, Actor>();
  readonly running = new Set<Promise<void>>();
  private busy = new Set<string>();
  readonly env: AppEnv;
  private timer?: ReturnType<typeof setInterval>;
  constructor(readonly directory: string, config: Pick<AppEnv, 'BOT_TOKEN' | 'OWNER_ID' | 'AI_BASE_URL' | 'AI_MODEL' | 'AI_API_KEY'>) {
    mkdirSync(directory, { recursive: true, mode: 0o700 });
    this.env = { ...config,
      ACCOUNTS: { getByName: name => this.get('account', name) as Account },
      CHATS: { getByName: name => this.get('chat', name) as Conversation },
      AI_JOBS: { getByName: name => this.get('ai', name) as AiJob }
    };
    for (const file of readdirSync(directory)) {
      const match = /^(account|chat|ai)-([0-9_]+)\.sqlite$/.exec(file);
      if (match) this.get(match[1]!, match[2]!.replaceAll('_', ':'));
    }
  }
  get(kind: string, name: string): Actor {
    if (!/^\d+(?::\d+)?$/.test(name)) throw new Error('Invalid actor name');
    const key = `${kind}-${name.replaceAll(':', '_')}`;
    let actor = this.actors.get(key);
    if (!actor) {
      const ctx = { storage: new ActorStorage(new DatabaseSync(join(this.directory, `${key}.sqlite`))) };
      actor = kind === 'account' ? new Account(ctx, this.env) : kind === 'chat' ? new Conversation(ctx, this.env) : new AiJob(ctx, this.env);
      this.actors.set(key, actor);
    }
    return actor;
  }
  async tick() {
    for (const [key, actor] of this.actors) {
      if (this.busy.has(key)) continue;
      // Mark before awaiting: overlapping timer ticks cannot run an actor twice.
      this.busy.add(key);
      const task = (async () => {
        const due = await actor.ctx.storage.getAlarm();
        if (due !== null && due <= Date.now()) {
          // Keep the persisted due time until the handler installs its watchdog.
          try { await actor.alarm(); }
          catch { await actor.ctx.storage.setAlarm(Date.now() + 5000); console.error('Background task failed; retry scheduled'); }
        }
      })().finally(() => { this.busy.delete(key); this.running.delete(task); });
      this.running.add(task);
    }
  }
  start() { this.timer = setInterval(() => { void this.tick(); }, 200); }
  async close() {
    clearInterval(this.timer);
    await Promise.allSettled([...this.running]);
    for (const actor of this.actors.values()) actor.ctx.storage.db.close();
    this.actors.clear();
  }
}
