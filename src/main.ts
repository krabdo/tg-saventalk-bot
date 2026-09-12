import { mkdirSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { Runtime } from './runtime.js';
import { ApiError, telegram, readJson } from './api.js';
import { Documents } from './storage.js';
import type { AppEnv, Update } from './types.js';

export const allowedUpdates = ['business_connection', 'business_message', 'edited_business_message', 'deleted_business_messages', 'message'];
export async function setup(env: AppEnv) {
  const me = await telegram<{ id: number; can_connect_to_business?: boolean; has_topics_enabled?: boolean }>(env, 'getMe', {});
  if (!me.can_connect_to_business || !me.has_topics_enabled) throw new Error('Enable Secretary Mode and private topics in BotFather first');
  await telegram(env, 'deleteWebhook', { drop_pending_updates: false });
  await telegram(env, 'setMyCommands', { scope: { type: 'chat', chat_id: env.OWNER_ID }, commands:
    [['help','使用说明'],['status','状态'],['pause','暂停 AI'],['resume','恢复 AI'],['limit','设置额度'],['reset','清零额度'],['prompt','设置提示词'],['prompt_show','查看提示词'],['prompt_reset','重置提示词'],['retry','重试归档']].map(([command,description]) => ({command,description})) });
  return me.id;
}
export async function poll(runtime: Runtime, signal: AbortSignal) {
  const account = runtime.env.ACCOUNTS.getByName(runtime.env.OWNER_ID);
  const docs = new Documents(account.ctx.storage.sql);
  while (!signal.aborted) {
    try {
      const response = await fetch(`https://api.telegram.org/bot${runtime.env.BOT_TOKEN}/getUpdates`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, redirect: 'error',
        signal: AbortSignal.any([signal, AbortSignal.timeout(45_000)]),
        body: JSON.stringify({ offset: docs.get('offset', 0), timeout: 30, limit: 100, allowed_updates: allowedUpdates })
      });
      const data = await readJson(response, 16_000_000) as { ok: boolean; result: Update[]; error_code?: number; parameters?: {retry_after?: number} };
      if (!data.ok) {
        if (data.error_code === 409 || data.error_code === 401) throw new Error('Polling conflict or invalid token; stop other instances and check configuration');
        await sleep(Math.max(1000, (data.parameters?.retry_after ?? 5) * 1000), undefined, { signal }); continue;
      }
      if (!Array.isArray(data.result) || data.result.some(u => !Number.isSafeInteger(u.update_id))) throw new Error('Invalid update batch');
      // Offset and inbox commit atomically before acknowledging via the next getUpdates.
      account.ctx.storage.transactionSync(() => {
        for (const update of data.result) account.ctx.storage.sql.exec('INSERT OR IGNORE INTO inbox (id, body) VALUES (?, ?)', update.update_id, JSON.stringify(update));
        if (data.result.length) docs.set('offset', Math.max(...data.result.map(u => u.update_id)) + 1);
      });
      if (data.result.length) await account.ctx.storage.setAlarm(Date.now() + 1);
      writeFileSync(join(runtime.directory, 'health'), String(Date.now()));
    } catch (error) {
      if (signal.aborted) break;
      if (error instanceof Error && error.message.startsWith('Polling conflict')) throw error;
      console.error('Polling temporarily unavailable; retrying');
      await sleep(5000, undefined, { signal }).catch(() => {});
    }
  }
}
async function main() {
  const config = { BOT_TOKEN: process.env.BOT_TOKEN ?? '', OWNER_ID: process.env.OWNER_ID ?? '', AI_BASE_URL: process.env.AI_BASE_URL ?? '', AI_MODEL: process.env.AI_MODEL ?? '', AI_API_KEY: process.env.AI_API_KEY };
  if (!/^\d+:[A-Za-z0-9_-]+$/.test(config.BOT_TOKEN) || !/^[1-9]\d*$/.test(config.OWNER_ID)) throw new Error('Set BOT_TOKEN and numeric OWNER_ID');
  const dir = resolve(process.env.DATA_DIR ?? './data'); mkdirSync(dir, {recursive:true,mode:0o700});
  // Exclusive lock is automatically released by the OS even after SIGKILL.
  const { DatabaseSync } = await import('node:sqlite');
  const lock = new DatabaseSync(join(dir,'instance.lock.sqlite'), {timeout:1000});
  lock.exec('CREATE TABLE IF NOT EXISTS lock (id INTEGER); BEGIN EXCLUSIVE');
  const runtime = new Runtime(dir, config);
  const stop = new AbortController();
  const halt = () => stop.abort(); process.once('SIGTERM',halt); process.once('SIGINT',halt);
  try {
    lock.exec('CREATE TABLE IF NOT EXISTS identity (id INTEGER PRIMARY KEY, value TEXT)');
    const identity = lock.prepare('SELECT value FROM identity WHERE id=1').get()?.value;
    const me = await telegram<{id:number}>(runtime.env,'getMe',{});
    if (identity && identity !== `${me.id}:${config.OWNER_ID}`) throw new Error('Data volume belongs to another bot or owner');
    lock.prepare('INSERT OR REPLACE INTO identity VALUES (1,?)').run(`${me.id}:${config.OWNER_ID}`);
    // Commit identity, then reacquire the process-lifetime lock synchronously.
    lock.exec('COMMIT; BEGIN EXCLUSIVE');
    await setup(runtime.env);
    // Recover an inbox committed immediately before a crash, even without its alarm.
    await runtime.env.ACCOUNTS.getByName(config.OWNER_ID).ctx.storage.setAlarm(Date.now()+1);
    runtime.start(); console.log('Secretary started: private topics, long polling, AI initially paused');
    await poll(runtime,stop.signal);
  } finally { stop.abort(); await runtime.close(); lock.close(); }
}
if (process.argv[1] && import.meta.url === (await import('node:url')).pathToFileURL(process.argv[1]).href) {
  main().catch(error => { console.error(error instanceof ApiError ? `Telegram setup failed (${error.code}); check BotFather and /start` : 'Startup stopped; check settings, exclusive data volume and other bot instances'); process.exitCode=1; });
}
