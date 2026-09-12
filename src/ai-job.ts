import { LocalActor, type ActorState } from './actor.js';
import { completion } from './api.js';
import { Documents, wake } from './storage.js';
import type { AiTicket, AppEnv } from './types.js';

/** Separate alarm execution keeps slow model calls out of the archive worker. */
export class AiJob extends LocalActor<AppEnv> {
  private docs: Documents;
  constructor(ctx: ActorState, env: AppEnv) { super(ctx, env); this.docs = new Documents(ctx.storage.sql); }
  async enqueue(chat: string, at: number) {
    this.docs.set('chat', chat);
    const due = Math.max(at, this.docs.get('retryNotBefore', 0));
    this.docs.set('next', due);
    await wake(this.ctx.storage, due);
  }
  async alarm() {
    const chat = this.docs.get('chat', '');
    if (!chat) return;
    const conversation = this.env.CHATS.getByName(`${this.env.OWNER_ID}:${chat}`);
    const active = this.docs.get<{ token?: string } | null>('active', null);
    if (active) {
      await conversation.failAi(active.token);
      this.docs.set('active', null);
    }
    const at = Math.max(this.docs.get('next', 0), this.docs.get('retryNotBefore', 0));
    if (!at) return;
    if (at > Date.now()) { await this.ctx.storage.setAlarm(at); return; }
    this.docs.set('next', 0);
    this.docs.set('active', {});
    await this.ctx.storage.setAlarm(Date.now() + 90_000);
    let ticket: AiTicket | null = null;
    try {
      const retry = this.docs.get<{ ticket: AiTicket; text: string; attempts: number } | null>('retry', null);
      ticket = retry?.ticket ?? await conversation.prepare();
      if (ticket) {
        this.docs.set('active', { token: ticket.token });
        const text = retry?.text ?? await completion(this.env, ticket.messages);
        const delay = await conversation.finishAi(ticket, text);
        if (delay && (retry?.attempts ?? 0) < 4) {
          this.docs.set('retry', { ticket, text, attempts: (retry?.attempts ?? 0) + 1 });
          this.docs.set('retryNotBefore', Date.now() + delay * 1000);
          this.docs.set('next', Date.now() + delay * 1000);
        } else {
          this.docs.set('retry', null);
          this.docs.set('retryNotBefore', 0);
          if (delay) await conversation.failAi(ticket.token);
        }
      }
    } catch { this.docs.set('retry', null); this.docs.set('retryNotBefore', 0); await conversation.failAi(ticket?.token); }
    this.docs.set('active', null);
    const next = this.docs.get('next', 0);
    if (next) await this.ctx.storage.setAlarm(Math.max(Date.now() + 1, next));
    else await this.ctx.storage.deleteAlarm();
  }
}
