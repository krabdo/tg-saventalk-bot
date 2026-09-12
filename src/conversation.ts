import { LocalActor, type ActorState } from './actor.js';
import { archiveParts, type SendPart } from './archive.js';
import { ApiError, telegram } from './api.js';
import { clip, Documents, wake } from './storage.js';
import type { AiTicket, AppEnv, Message, Policy, Update } from './types.js';

interface State {
  chat: string; name: string; paused: boolean; limit?: number; used: number; revision: number;
  lastIncoming: number; connectionId: string; firstPending: number; pendingAt: number;
  pendingPolicyVersion?: number;
  flight?: { token: string; phase: 'generating' | 'sending' | 'uncertain'; revision: number; epoch: number };
  epoch: number; aiError?: string; topic?: number; topicState?: 'creating' | 'uncertain';
}
type ArchiveJob = { id: number; source: string; parts: string; step: number; attempts: number; status: string; next_at: number; anchor: number | null };
type StoredMessage = { key: string; body: string | null; deleted: number; version: number; update_id: number; anchor: number | null };
const initial = (): State => ({ chat: '', name: '', paused: false, used: 0, revision: 0, lastIncoming: 0, connectionId: '', firstPending: 0, pendingAt: 0, epoch: 0 });

export class Conversation extends LocalActor<AppEnv> {
  private docs: Documents;
  constructor(ctx: ActorState, env: AppEnv) {
    super(ctx, env);
    this.docs = new Documents(ctx.storage.sql);
    ctx.storage.sql.exec('CREATE TABLE IF NOT EXISTS updates (id INTEGER PRIMARY KEY)');
    ctx.storage.sql.exec('CREATE TABLE IF NOT EXISTS versions (key TEXT PRIMARY KEY)');
    ctx.storage.sql.exec('CREATE TABLE IF NOT EXISTS messages (key TEXT PRIMARY KEY, body TEXT, deleted INTEGER NOT NULL DEFAULT 0, version INTEGER NOT NULL DEFAULT 0, update_id INTEGER NOT NULL DEFAULT 0, anchor INTEGER)');
    ctx.storage.sql.exec(`CREATE TABLE IF NOT EXISTS archive_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, source TEXT NOT NULL, parts TEXT NOT NULL, step INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'pending', next_at INTEGER NOT NULL DEFAULT 0, anchor INTEGER)`);
    ctx.storage.sql.exec('CREATE INDEX IF NOT EXISTS archive_pending ON archive_jobs (status, next_at, id)');
    ctx.storage.sql.exec("CREATE INDEX IF NOT EXISTS text_context ON messages (deleted, json_extract(body, '$.date') DESC, json_extract(body, '$.message_id') DESC) WHERE body IS NOT NULL AND json_extract(body, '$.text') IS NOT NULL");
    ctx.storage.sql.exec('CREATE TABLE IF NOT EXISTS controls (id INTEGER PRIMARY KEY, result TEXT NOT NULL)');
  }
  private state() { return this.docs.get<State>('state', initial()); }
  private save(state: State) { this.docs.set('state', state); }
  private account() { return this.env.ACCOUNTS.getByName(this.env.OWNER_ID); }
  private scheduleAi(state: State) {
    return state.pendingAt ? this.env.AI_JOBS.getByName(`${this.env.OWNER_ID}:${state.chat}`).enqueue(state.chat, state.pendingAt) : Promise.resolve();
  }
  private addJob(source: string, parts: SendPart[]) {
    this.ctx.storage.sql.exec('INSERT INTO archive_jobs (source, parts) VALUES (?, ?)', source, JSON.stringify(parts));
  }
  async ingest(update: Update) {
    if (!this.ctx.storage.sql.exec('SELECT id FROM updates WHERE id = ?', update.update_id).toArray().length) {
      const message = update.business_message ?? update.edited_business_message;
      const item = message ?? update.deleted_business_messages;
      if (!item) return;
      const policy = await this.account().policy();
      // Hash before entering the synchronous mutation section.
      const signature = message && update.edited_business_message ? Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(JSON.stringify(message))))).map(x => x.toString(16).padStart(2, '0')).join('') : 'original';
      if (this.ctx.storage.sql.exec('SELECT id FROM updates WHERE id = ?', update.update_id).toArray().length) return;
      this.ctx.storage.transactionSync(() => {
        const state = this.state();
        state.chat = String(item.chat.id);
        state.name = [item.chat.first_name, item.chat.last_name].filter(Boolean).join(' ') || item.chat.username || state.chat;
        if (message) this.recordMessage(state, message, update.update_id, !!update.edited_business_message, signature, policy);
        else {
          for (const id of update.deleted_business_messages!.message_ids) {
            const key = `${item.business_connection_id}:${id}`;
            const old = this.ctx.storage.sql.exec<StoredMessage>('SELECT * FROM messages WHERE key = ?', key).toArray()[0];
            this.ctx.storage.sql.exec('INSERT INTO messages (key, deleted) VALUES (?, 1) ON CONFLICT(key) DO UPDATE SET deleted = 1', key);
            this.addJob(key, [{ method: 'sendMessage', body: { text: `🗑 原消息 #${id} 已删除${old?.body ? '；已保留归档副本' : '；未收到原消息'}`, ...(old?.anchor ? { reply_parameters: { message_id: old.anchor, allow_sending_without_reply: true } } : {}) } }]);
          }
          state.revision++;
        }
        this.save(state);
        this.ctx.storage.sql.exec('INSERT INTO updates VALUES (?)', update.update_id);
      });
    }
    await wake(this.ctx.storage);
    await this.account().observe(this.state().chat);
    await this.scheduleAi(this.state());
  }
  private recordMessage(state: State, message: Message, update: number, edited: boolean, signature = 'original', policy?: Policy) {
    const key = `${message.business_connection_id}:${message.message_id}`;
    const versionKey = `${key}:${edited ? `edit:${message.edit_date}:${signature}` : 'original'}`;
    if (this.ctx.storage.sql.exec('SELECT key FROM versions WHERE key = ?', versionKey).toArray().length) return;
    this.ctx.storage.sql.exec('INSERT INTO versions VALUES (?)', versionKey);
    const old = this.ctx.storage.sql.exec<StoredMessage>('SELECT * FROM messages WHERE key = ?', key).toArray()[0];
    const version = (message.edit_date ?? message.date) * 2 + (edited ? 1 : 0);
    const isCurrent = !old?.body || version > old.version || (version === old.version && update > old.update_id);
    if (isCurrent) {
      this.ctx.storage.sql.exec(`INSERT INTO messages (key, body, version, update_id) VALUES (?, ?, ?, ?) ON CONFLICT(key) DO UPDATE SET body = excluded.body, version = excluded.version, update_id = excluded.update_id`, key, JSON.stringify(message), version, update);
      state.revision++;
    }
    const parts = archiveParts(message, this.env.OWNER_ID, edited);
    if (old?.anchor && edited) parts[0]!.body.reply_parameters = { message_id: old.anchor, allow_sending_without_reply: true };
    if (old?.deleted) parts[0]!.body.text += '\n🗑 原消息已删除（迟到的消息更新）';
    this.addJob(key, parts);
    const owner = String(message.from?.id) === this.env.OWNER_ID;
    if (owner && !message.sender_business_bot && !edited) {
      state.paused = true; state.pendingAt = 0; state.firstPending = 0; state.revision++;
    } else if (!owner && !message.sender_business_bot && !message.from?.is_bot && !edited && !old?.deleted) {
      state.lastIncoming = Math.max(state.lastIncoming, message.date * 1000);
      state.connectionId = message.business_connection_id!;
      if (message.text && !state.paused && policy && !policy.paused) {
        state.firstPending ||= Date.now();
        state.pendingAt = Math.min(Date.now() + 3000, state.firstPending + 10_000);
        state.pendingPolicyVersion = policy.version;
      }
    }
  }
  async control(id: number, command: string, limit?: number): Promise<string> {
    const old = this.ctx.storage.sql.exec<{ result: string }>('SELECT result FROM controls WHERE id = ?', id).toArray()[0];
    if (old) return old.result;
    const state = this.state();
    let result: string;
    if (command === 'status') {
      const rows = this.ctx.storage.sql.exec<{ status: string; n: number }>('SELECT status, COUNT(*) n FROM archive_jobs GROUP BY status').toArray();
      const policy = await this.account().policy();
      const fresh = this.state();
      const issues = this.ctx.storage.sql.exec<{ id: number; source: string; step: number; status: string }>("SELECT id, source, step, status FROM archive_jobs WHERE status IN ('failed', 'uncertain') ORDER BY id LIMIT 15").toArray();
      result = `联系人：${fresh.name || '尚无记录'} (${fresh.chat || '未知'})\nAI：${fresh.paused ? '联系人暂停' : policy.paused ? '全局暂停' : '运行'}\n已用额度（含待核对）：${fresh.used}/${fresh.limit ?? policy.limit}\n任务：${rows.map(row => `${row.status}=${row.n}`).join(', ') || '无'}\n话题状态：${fresh.topicState ?? fresh.topic ?? '待创建'}\nAI 状态：${fresh.flight?.phase ?? fresh.aiError ?? '空闲'}\n异常任务（最多15项）：\n${issues.map(row => `任务 ${row.id} · 源 ${row.source} · 步骤 ${row.step + 1} · ${row.status}`).join('\n') || '无'}`;
    } else {
      if (command === 'pause') { state.paused = true; state.pendingAt = 0; state.firstPending = 0; }
      if (command === 'resume') state.paused = false;
      if (command === 'limit') state.limit = limit;
      if (command === 'reset') {
        state.used = 0; state.epoch++;
        if (state.flight?.phase === 'uncertain') state.flight = undefined;
      }
      state.revision++;
      this.save(state);
      if (command === 'retry') {
        this.ctx.storage.sql.exec("UPDATE archive_jobs SET status = 'pending', attempts = 0, next_at = 0 WHERE status = 'failed'");
        await wake(this.ctx.storage);
      }
      result = command === 'retry' ? '已安排重试明确失败的归档任务；uncertain 待核对任务不会重发。' : '联系人设置已更新。重置额度不会解除暂停，恢复不会重置额度；下一条新文字消息可触发 AI。';
    }
    this.ctx.storage.sql.exec('INSERT OR IGNORE INTO controls VALUES (?, ?)', id, result);
    return result;
  }
  private eligible(state: State, policy: Policy) {
    return !!state.chat && !state.paused && !policy.paused && state.used < (state.limit ?? policy.limit)
      && !!policy.connection?.is_enabled && !!policy.connection.rights?.can_reply
      && policy.connection.id === state.connectionId && Date.now() - state.lastIncoming < 86_400_000;
  }
  async prepare(): Promise<AiTicket | null> {
    const policy = await this.account().policy();
    const state = this.state();
    if (state.flight || !state.pendingAt || state.pendingAt > Date.now()) return null;
    state.pendingAt = 0; state.firstPending = 0;
    if (!this.eligible(state, policy) || state.pendingPolicyVersion !== policy.version) { this.save(state); return null; }
    const rows = this.ctx.storage.sql.exec<{ body: string }>(`SELECT body FROM messages WHERE deleted = 0 AND body IS NOT NULL AND json_extract(body, '$.text') IS NOT NULL ORDER BY json_extract(body, '$.date') DESC, json_extract(body, '$.message_id') DESC LIMIT 20`).toArray();
    const messages: AiTicket['messages'] = [];
    let remaining = 24_000;
    for (const row of rows) {
      const message = JSON.parse(row.body) as Message;
      if (!message.text) continue;
      const content = message.text.slice(-remaining);
      messages.push({ role: String(message.from?.id) === this.env.OWNER_ID || message.sender_business_bot ? 'assistant' : 'user', content });
      remaining -= content.length;
      if (!remaining || messages.length === 20) break;
    }
    if (!messages.length) { this.save(state); return null; }
    const token = crypto.randomUUID();
    state.flight = { token, phase: 'generating', revision: state.revision, epoch: state.epoch };
    state.aiError = undefined; this.save(state);
    return { token, revision: state.revision, policyVersion: policy.version, connectionId: state.connectionId, messages: [{ role: 'system', content: policy.prompt }, ...messages.reverse()] };
  }
  async finishAi(ticket: AiTicket, text: string): Promise<number | undefined> {
    const policy = await this.account().policy();
    let state = this.state();
    if (state.flight?.token !== ticket.token || state.flight.phase !== 'generating') return;
    if (!this.eligible(state, policy) || state.revision !== ticket.revision || policy.version !== ticket.policyVersion || policy.connection?.id !== ticket.connectionId) {
      state.flight = undefined; this.save(state); await this.scheduleAi(state); return;
    }
    state.used++; state.flight.phase = 'sending'; this.save(state);
    // Persist the send reservation before the external request; recovery never re-sends it.
    await this.ctx.storage.sync();
    const finalPolicy = await this.account().policy();
    state = this.state();
    if (state.flight?.token !== ticket.token || state.flight.phase !== 'sending') return;
    // A command may have arrived while the reservation was committing.
    if (!this.eligible({ ...state, used: Math.max(0, state.used - 1) }, finalPolicy) || state.revision !== ticket.revision || finalPolicy.version !== ticket.policyVersion) {
      if (state.flight.epoch === state.epoch) state.used = Math.max(0, state.used - 1);
      state.flight = undefined; this.save(state); await this.scheduleAi(state); return;
    }
    try {
      const sent = await telegram<Message>(this.env, 'sendMessage', { business_connection_id: ticket.connectionId, chat_id: state.chat, text: clip(text) });
      state = this.state();
      sent.business_connection_id = ticket.connectionId;
      sent.sender_business_bot ??= { id: 0, first_name: 'AI', is_bot: true };
      sent.from ??= { id: Number(this.env.OWNER_ID), first_name: '我' };
      this.ctx.storage.transactionSync(() => {
        this.recordMessage(state, sent, 0, false);
        if (state.flight?.token === ticket.token) state.flight = undefined;
        this.save(state);
      });
      await wake(this.ctx.storage);
    } catch (error) {
      state = this.state();
      if (state.flight?.token === ticket.token) {
        if (error instanceof ApiError && !error.uncertain) {
          if (state.flight.epoch === state.epoch) state.used = Math.max(0, state.used - 1);
          if (error.code === 429) {
            state.flight.phase = 'generating'; state.aiError = 'Telegram 限流，等待重试'; this.save(state);
            return Math.max(1, error.retryAfter || 5);
          }
          state.flight = undefined; state.aiError = '发送明确失败，未扣额度；等待下一条文字';
        } else { state.flight.phase = 'uncertain'; state.aiError = '发送结果不确定，请核对原聊天后 /reset'; }
        this.save(state);
      }
      await this.account().notify();
    }
    await this.scheduleAi(this.state());
  }
  async bindTopic(id: number, topic: number): Promise<string> {
    const old = this.ctx.storage.sql.exec<{ result: string }>('SELECT result FROM controls WHERE id = ?', id).toArray()[0];
    if (old) return old.result;
    const state = this.state();
    if (!state.chat) return '没有这个联系人的消息记录。';
    if (state.topicState === 'creating') return '话题创建尚在进行，稍后再试。';
    if (!await this.account().registerTopic(state.chat, topic)) return '当前话题已经属于其他联系人，拒绝绑定。';
    const fresh = this.state(); fresh.topic = topic; fresh.topicState = undefined; this.save(fresh);
    const result = '已绑定当前话题。可执行 /retry 重试明确失败的任务；待核对任务使用 /archive_resolve。';
    this.ctx.storage.sql.exec('INSERT INTO controls VALUES (?, ?)', id, result);
    return result;
  }
  async resolveArchive(id: number, job: number, action: 'sent' | 'retry', messageId?: number): Promise<string> {
    const old = this.ctx.storage.sql.exec<{ result: string }>('SELECT result FROM controls WHERE id = ?', id).toArray()[0];
    if (old) return old.result;
    const row = this.ctx.storage.sql.exec<ArchiveJob>("SELECT * FROM archive_jobs WHERE id = ? AND status = 'uncertain'", job).toArray()[0];
    if (!row) return '未找到此待核对任务。';
    if (action === 'sent') {
      const parts = JSON.parse(row.parts) as SendPart[];
      this.ctx.storage.sql.exec('UPDATE archive_jobs SET status = ?, step = step + 1, anchor = COALESCE(anchor, ?) WHERE id = ?', row.step + 1 === parts.length ? 'done' : 'pending', messageId!, job);
      if (row.step === 0) this.ctx.storage.sql.exec('UPDATE messages SET anchor = COALESCE(anchor, ?) WHERE key = ?', messageId!, row.source);
    } else this.ctx.storage.sql.exec("UPDATE archive_jobs SET status = 'pending', attempts = 0, next_at = 0 WHERE id = ?", job);
    const result = action === 'sent' ? '已确认该步骤送达，继续后续归档。' : '已按你的核对结果重试该步骤；若实际上已送达，可能产生重复副本。';
    this.ctx.storage.sql.exec('INSERT INTO controls VALUES (?, ?)', id, result);
    await wake(this.ctx.storage);
    return result;
  }
  async failAi(token?: string) {
    const state = this.state();
    if (state.flight && (!token || state.flight.token === token)) {
      if (state.flight.phase === 'sending') state.flight.phase = 'uncertain';
      else if (state.flight.phase === 'generating') state.flight = undefined;
      state.aiError = 'AI 任务失败或中断；发送中的任务需核对，其余等待新文字';
      this.save(state);
    }
    await this.account().notify();
    await this.scheduleAi(state);
  }
  private async ensureTopic(): Promise<number> {
    let state = this.state();
    if (state.topic) {
      if (!await this.account().registerTopic(state.chat, state.topic)) throw new ApiError(400, 'Topic binding conflict');
      return state.topic;
    }
    if (state.topicState) {
      state.topicState = 'uncertain'; this.save(state);
      throw new ApiError(0, 'Topic creation outcome unknown', true);
    }
    state.topicState = 'creating'; this.save(state); await this.ctx.storage.sync();
    try {
      const topic = await telegram<{ message_thread_id: number }>(this.env, 'createForumTopic', { chat_id: this.env.OWNER_ID, name: `${clip(state.name, 125 - state.chat.length)} · ${state.chat}` });
      state = this.state(); state.topic = topic.message_thread_id; state.topicState = undefined; this.save(state);
      await this.account().registerTopic(state.chat, state.topic);
      return state.topic;
    } catch (error) {
      state = this.state();
      if (!state.topic) state.topicState = error instanceof ApiError && !error.uncertain ? undefined : 'uncertain';
      this.save(state); throw error;
    }
  }
  async alarm() {
    await this.ctx.storage.setAlarm(Date.now() + 60_000);
    // An interrupted send may have succeeded upstream: do not repeat it.
    this.ctx.storage.sql.exec("UPDATE archive_jobs SET status = 'uncertain' WHERE status = 'sending'");
    const row = this.ctx.storage.sql.exec<ArchiveJob>("SELECT * FROM archive_jobs WHERE status = 'pending' AND next_at <= ? ORDER BY id LIMIT 1", Date.now()).toArray()[0];
    if (row) {
      const slot = await this.account().archiveSlot();
      if (slot) { await this.ctx.storage.setAlarm(slot); return; }
      const parts = JSON.parse(row.parts) as SendPart[];
      const part = parts[row.step]!;
      try {
        const topic = await this.ensureTopic();
        this.ctx.storage.sql.exec("UPDATE archive_jobs SET status = 'sending' WHERE id = ?", row.id);
        await this.ctx.storage.sync();
        const sent = await telegram<Message>(this.env, part.method, { ...part.body, chat_id: this.env.OWNER_ID, message_thread_id: topic, disable_notification: true });
        this.ctx.storage.sql.exec("UPDATE archive_jobs SET status = ?, step = step + 1, attempts = 0, anchor = COALESCE(anchor, ?) WHERE id = ?", row.step + 1 === parts.length ? 'done' : 'pending', sent.message_id, row.id);
        if (row.step === 0) this.ctx.storage.sql.exec('UPDATE messages SET anchor = COALESCE(anchor, ?) WHERE key = ?', sent.message_id, row.source);
      } catch (error) {
        const api = error instanceof ApiError ? error : new ApiError(0, '', true);
        let status = api.uncertain ? 'uncertain' : row.attempts >= 4 ? 'failed' : 'pending';
        let delay = Math.max(api.retryAfter, Math.min(300, 2 ** (row.attempts + 1)));
        if (!api.uncertain && /TOPIC_CLOSED/i.test(api.description)) {
          // reopenForumTopic is supergroup-only. Keep the private archive intact
          // until the owner restores access or binds a replacement topic.
          status = 'failed';
        } else if (!api.uncertain && /message thread not found|TOPIC_DELETED|MESSAGE_THREAD_INVALID/i.test(api.description)) {
          const state = this.state(); state.topic = undefined; state.topicState = undefined; this.save(state);
          part.body.reply_parameters = undefined;
          parts.splice(row.step, 0, { method: 'sendMessage', body: { text: '⚠️ 原话题已删除，以下内容继续归档到新话题；旧话题副本无法恢复。' } });
          this.ctx.storage.sql.exec('UPDATE archive_jobs SET parts = ? WHERE id = ?', JSON.stringify(parts), row.id);
        } else if (!api.uncertain && api.code === 400 && part.media) {
          parts[row.step] = { method: 'sendMessage', body: { text: `⚠️ 媒体未备份：Telegram 拒绝重发。源消息 ${row.source}。类型 ${part.method}；元数据保留在存储中。${typeof part.body.caption === 'string' ? `\n说明：${part.body.caption}` : ''}` } };
          this.ctx.storage.sql.exec('UPDATE archive_jobs SET parts = ? WHERE id = ?', JSON.stringify(parts), row.id);
          status = 'pending';
        }
        if (api.retryAfter) { await this.account().deferArchive(api.retryAfter); delay = Math.max(delay, api.retryAfter); }
        this.ctx.storage.sql.exec('UPDATE archive_jobs SET status = ?, attempts = attempts + 1, next_at = ? WHERE id = ?', status, Date.now() + delay * 1000, row.id);
        await this.account().notify();
      }
    }
    const next = this.ctx.storage.sql.exec<{ at: number | null }>("SELECT MIN(next_at) at FROM archive_jobs WHERE status = 'pending'").one().at;
    if (next !== null) await this.ctx.storage.setAlarm(Math.max(Date.now() + 1, next));
    else await this.ctx.storage.deleteAlarm();
  }
}
