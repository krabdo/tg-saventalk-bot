import { LocalActor, type ActorState } from './actor.js';
import { ApiError, telegram } from './api.js';
import { clip, Documents, wake } from './storage.js';
import { DEFAULT_PROMPT, type AppEnv, type Connection, type Message, type Policy, type Update } from './types.js';

const HELP = `通过 BotFather 开启 Secretary Mode 和私聊话题模式，再在 Telegram「聊天自动化」连接本 Bot。仅配置的主人可接入。归档直接保存在你与 Bot 私聊的联系人话题中，无需配置群组。\n\n/status all 或 用户ID\n/pause all 或 用户ID\n/resume all 或 用户ID\n/limit N all 或 用户ID\n/reset 用户ID\n/prompt 提示词（也可回复文字执行）\n/prompt_show /prompt_reset\n/retry（联系人话题内）\n\n已绑定话题内省略目标即当前联系人，其他位置必须指定目标。初始 AI 全局暂停，默认每人累计 10 条；在原私聊手动回复会暂停该联系人。在本 Bot 话题中写的普通文字只是备注，不会代发。/reset 不会解除暂停。`;
type InboxRow = { id: number; body: string; attempts: number };
type ConnectionRow = { body: string; version: number };
export class Account extends LocalActor<AppEnv> {
  private docs: Documents;
  constructor(ctx: ActorState, env: AppEnv) {
    super(ctx, env);
    this.docs = new Documents(ctx.storage.sql);
    ctx.storage.sql.exec(`CREATE TABLE IF NOT EXISTS inbox (id INTEGER PRIMARY KEY, body TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0, done INTEGER NOT NULL DEFAULT 0)`);
    ctx.storage.sql.exec('CREATE INDEX IF NOT EXISTS inbox_pending ON inbox (done, next_at, id)');
    ctx.storage.sql.exec('CREATE TABLE IF NOT EXISTS connections (id TEXT PRIMARY KEY, body TEXT NOT NULL, version INTEGER NOT NULL)');
    ctx.storage.sql.exec('CREATE TABLE IF NOT EXISTS topics (chat_id TEXT PRIMARY KEY, topic INTEGER NOT NULL UNIQUE)');
    ctx.storage.sql.exec('CREATE TABLE IF NOT EXISTS contacts (chat_id TEXT PRIMARY KEY)');
    ctx.storage.sql.exec('CREATE TABLE IF NOT EXISTS command_results (id INTEGER PRIMARY KEY, result TEXT NOT NULL)');
  }
  async enqueue(update: Update) {
    this.ctx.storage.sql.exec('INSERT OR IGNORE INTO inbox (id, body) VALUES (?, ?)', update.update_id, JSON.stringify(update));
    await wake(this.ctx.storage);
  }
  policy(): Policy {
    return this.docs.get<Policy>('policy', { paused: true, limit: 10, prompt: DEFAULT_PROMPT, version: 0 });
  }
  private savePolicy(policy: Policy) { this.docs.set('policy', policy); }
  private async connection(id: string): Promise<Connection | null> {
    const row = this.ctx.storage.sql.exec<ConnectionRow>('SELECT body, version FROM connections WHERE id = ?', id).toArray()[0];
    if (row) return JSON.parse(row.body) as Connection;
    const connection = await telegram<Connection>(this.env, 'getBusinessConnection', { business_connection_id: id });
    if (String(connection.user.id) !== this.env.OWNER_ID) return null;
    this.recordConnection(connection, -1);
    return connection;
  }
  private recordConnection(connection: Connection, version: number) {
    if (String(connection.user.id) !== this.env.OWNER_ID) return;
    const row = this.ctx.storage.sql.exec<ConnectionRow>('SELECT body, version FROM connections WHERE id = ?', connection.id).toArray()[0];
    if (row && row.version >= version) return;
    this.ctx.storage.sql.exec('INSERT OR REPLACE INTO connections VALUES (?, ?, ?)', connection.id, JSON.stringify(connection), version);
    const policy = this.policy();
    const current = policy.connection;
    if (!current || current.id === connection.id || connection.date >= current.date) {
      policy.connection = connection;
      policy.version++;
      this.savePolicy(policy);
    }
  }
  registerTopic(chat: string, topic: number) {
    const existing = this.ctx.storage.sql.exec<{ chat_id: string }>('SELECT chat_id FROM topics WHERE topic = ?', topic).toArray()[0];
    if (existing && existing.chat_id !== chat) return false;
    this.ctx.storage.sql.exec('INSERT OR REPLACE INTO topics VALUES (?, ?)', chat, topic);
    return true;
  }
  observe(chat: string) { this.ctx.storage.sql.exec('INSERT OR IGNORE INTO contacts VALUES (?)', chat); }
  /** All contact topics share one private destination and one pacing gate. */
  archiveSlot(): number {
    const next = this.docs.get<number>('archiveNext', 0);
    if (next > Date.now()) return next;
    this.docs.set('archiveNext', Date.now() + 3100);
    return 0;
  }
  deferArchive(seconds: number) { this.docs.set('archiveNext', Math.max(this.docs.get<number>('archiveNext', 0), Date.now() + seconds * 1000)); }
  async notify() {
    const last = this.docs.get<number>('lastAlert', 0);
    if (Date.now() - last < 600_000) return;
    this.docs.set('lastAlert', Date.now());
    try { await telegram(this.env, 'sendMessage', { chat_id: this.env.OWNER_ID, text: '归档或 AI 任务出现异常。请在联系人话题执行 /status，或私聊 /status all。失败任务已保留；发送结果不确定的任务不会自动重发。' }); } catch { /* State remains available in /status. */ }
  }
  private async route(update: Update) {
    if (update.business_connection) { this.recordConnection(update.business_connection, update.update_id); return; }
    const item = update.business_message ?? update.edited_business_message ?? update.deleted_business_messages;
    if (item?.business_connection_id) {
      const connection = await this.connection(item.business_connection_id);
      if (!connection || String(connection.user.id) !== this.env.OWNER_ID || item.chat.type !== 'private') return;
      await this.env.CHATS.getByName(`${this.env.OWNER_ID}:${item.chat.id}`).ingest(update);
      return;
    }
    if (update.message) await this.command(update.update_id, update.message);
  }
  private async command(id: number, message: Message) {
    if (String(message.from?.id) !== this.env.OWNER_ID || message.sender_chat) return;
    if (message.chat.type !== 'private' || String(message.chat.id) !== this.env.OWNER_ID) return;
    const match = message.text?.match(/^\/([a-z_]+)(?:@([A-Za-z0-9_]+))?(?:\s+([\s\S]*))?$/);
    if (!match) return;
    if (match[2]) {
      let username = this.docs.get<string>('username', '');
      if (!username) {
        const bot = await telegram<{ username: string }>(this.env, 'getMe', {});
        username = bot.username; this.docs.set('username', username);
      }
      if (match[2].toLowerCase() !== username.toLowerCase()) return;
    }
    const old = this.ctx.storage.sql.exec<{ result: string }>('SELECT result FROM command_results WHERE id = ?', id).toArray()[0];
    // Results are committed before replying: duplicate webhooks never apply /reset twice.
    if (old) return;
    let result: string;
    try { result = await this.executeCommand(id, match[1]!, match[3]?.trim() ?? '', message); }
    catch (error) { if (error instanceof ApiError) throw error; result = '命令无法完成，请检查参数和配置。'; }
    this.ctx.storage.sql.exec('INSERT OR IGNORE INTO command_results VALUES (?, ?)', id, result);
    try {
      if (match[1] === 'prompt_show') {
        let remaining = result;
        while (remaining) {
          let length = Math.min(4000, remaining.length);
          if (length < remaining.length && /[\uD800-\uDBFF]/.test(remaining[length - 1] ?? '')) length--;
          await telegram(this.env, 'sendMessage', { chat_id: message.chat.id, message_thread_id: message.message_thread_id, text: remaining.slice(0, length) });
          remaining = remaining.slice(length);
        }
      } else await telegram(this.env, 'sendMessage', { chat_id: message.chat.id, message_thread_id: message.message_thread_id, text: clip(result) });
    }
    catch { /* Do not repeat a command merely because its acknowledgement failed. */ }
  }
  private async executeCommand(id: number, command: string, args: string, message: Message): Promise<string> {
    if (command === 'help' || command === 'start') return HELP;
    if (command === 'topic_bind') {
      if (String(message.chat.id) !== this.env.OWNER_ID || !message.message_thread_id || !/^\d+$/.test(args)) return '在正确的归档话题执行 /topic_bind 用户ID。用于人工修复话题绑定。';
      return this.env.CHATS.getByName(`${this.env.OWNER_ID}:${args}`).bindTopic(id, message.message_thread_id);
    }
    const policy = this.policy();
    if (command === 'prompt_show') return policy.prompt;
    if (command === 'prompt' || command === 'prompt_reset') {
      const prompt = command === 'prompt_reset' ? DEFAULT_PROMPT : args || message.reply_to_message?.text;
      if (!prompt || prompt.length > 12000) return '提示词不能为空，且不能超过 12000 字符。';
      policy.prompt = prompt; policy.version++; this.savePolicy(policy);
      return '全局系统提示词已更新，尚未发送的旧生成结果作废。';
    }
    const repair = command === 'archive_resolve' ? args.match(/^(\d+) (sent (\d+)|retry)$/) : null;
    if (command === 'archive_resolve' && !repair) return '核对归档话题中的消息后执行 /archive_resolve 任务ID sent 归档消息ID 或 /archive_resolve 任务ID retry。';
    let target = command === 'archive_resolve' ? '' : args;
    let limit: number | undefined;
    if (command === 'limit') {
      const tokens = args.split(/\s+/);
      if (!/^\d+$/.test(tokens[0] ?? '') || tokens.length > 2) return '用法：/limit N all 或 用户ID';
      limit = Number(tokens[0]); target = tokens[1] ?? '';
      if (!Number.isSafeInteger(limit) || limit > 1_000_000) return '额度范围：0 至 1000000。';
    }
    if (!target && message.message_thread_id && String(message.chat.id) === this.env.OWNER_ID) {
      target = this.ctx.storage.sql.exec<{ chat_id: string }>('SELECT chat_id FROM topics WHERE topic = ?', message.message_thread_id).toArray()[0]?.chat_id ?? '';
    }
    if (target === 'all') {
      if (command === 'status') {
        const rows = this.ctx.storage.sql.exec<{ chat_id: string }>('SELECT chat_id FROM contacts ORDER BY chat_id').toArray();
        const counts = this.ctx.storage.sql.exec<{ n: number }>('SELECT COUNT(*) n FROM inbox WHERE done = 0').one().n;
        return `连接：${policy.connection?.is_enabled ? '已启用' : '未连接/已停用'}\nAI 全局：${policy.paused ? '暂停' : '运行'}\n默认额度：${policy.limit}\n联系人话题：${rows.length}\n待路由更新：${counts}\n端点配置：${this.env.AI_BASE_URL && this.env.AI_MODEL && this.env.AI_API_KEY ? '已填写' : '未完整填写'}\n联系人 ID：${rows.map(row => row.chat_id).join(', ')}\n具体归档失败、待核对任务和 AI 状态请 /status 用户ID。`;
      }
      if (command === 'pause' || command === 'resume' || command === 'limit') {
        if (command === 'resume' && (!this.env.AI_BASE_URL.startsWith('https://') || !this.env.AI_MODEL || !this.env.AI_API_KEY)) return '请先配置 HTTPS AI_BASE_URL、AI_MODEL 和 AI_API_KEY。';
        if (command === 'limit') policy.limit = limit!;
        else policy.paused = command === 'pause';
        policy.version++; this.savePolicy(policy);
        return '全局设置已更新。联系人暂停和已用额度保持不变。';
      }
      return '此命令不支持 all。';
    }
    if (!/^\d+$/.test(target)) return '请指定用户ID，或在对应联系人话题执行。全局操作明确填写 all。';
    if (command === 'archive_resolve' && repair) return this.env.CHATS.getByName(`${this.env.OWNER_ID}:${target}`).resolveArchive(id, Number(repair[1]), repair[2] === 'retry' ? 'retry' : 'sent', repair[3] ? Number(repair[3]) : undefined);
    if (!['status', 'pause', 'resume', 'limit', 'reset', 'retry'].includes(command)) return HELP;
    if (command === 'retry' && (!message.message_thread_id || String(message.chat.id) !== this.env.OWNER_ID)) return '/retry 仅可在联系人话题内执行。';
    return this.env.CHATS.getByName(`${this.env.OWNER_ID}:${target}`).control(id, command, limit);
  }
  async alarm() {
    const rows = this.ctx.storage.sql.exec<InboxRow>('SELECT id, body, attempts FROM inbox WHERE done = 0 AND next_at <= ? ORDER BY id LIMIT 25', Date.now()).toArray();
    // Watchdog survives a process failure mid-route; downstream ingestion is idempotent.
    await this.ctx.storage.setAlarm(Date.now() + 60_000);
    for (const row of rows) {
      try {
        await this.route(JSON.parse(row.body) as Update);
        this.ctx.storage.sql.exec("UPDATE inbox SET done = 1, body = '{}' WHERE id = ?", row.id);
      } catch (error) {
        const seconds = error instanceof ApiError && error.retryAfter ? error.retryAfter : Math.min(300, 2 ** Math.min(row.attempts + 1, 8));
        this.ctx.storage.sql.exec('UPDATE inbox SET attempts = attempts + 1, next_at = ? WHERE id = ?', Date.now() + seconds * 1000, row.id);
        await this.notify();
      }
    }
    const next = this.ctx.storage.sql.exec<{ at: number | null }>('SELECT MIN(next_at) at FROM inbox WHERE done = 0').one().at;
    if (next !== null) await this.ctx.storage.setAlarm(Math.max(Date.now() + 1, next));
    else await this.ctx.storage.deleteAlarm();
  }
}
