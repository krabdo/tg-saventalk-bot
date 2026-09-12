import { env } from './harness';
import { reset, runInDurableObject, runDurableObjectAlarm, evictDurableObject } from './harness';
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';
import { archiveParts } from '../src/archive';
import { Documents } from '../src/storage';
import type { AppEnv, Connection, Message, Update } from '../src/types';

const e = env as AppEnv;
const account = () => e.ACCOUNTS.getByName('100');
const chat = (id = '200') => e.CHATS.getByName(`100:${id}`);
const ai = () => e.AI_JOBS.getByName('100:200');
let sequence = 100;
let outgoing: { method: string; body: Record<string, any> }[];
let handler: ((method: string, body: Record<string, any>) => Response | Promise<Response> | undefined) | undefined;
const connection: Connection = { id: 'conn', user: { id: 100, first_name: 'Owner' }, user_chat_id: 100, date: 1, is_enabled: true, rights: { can_reply: true } };
function message(extra: Partial<Message> = {}): Message {
  return { message_id: 1, date: Math.floor(Date.now() / 1000), chat: { id: 200, type: 'private', first_name: 'Alice' }, from: { id: 200, first_name: 'Alice' }, business_connection_id: 'conn', text: '你好', ...extra };
}
function ok(result: unknown) { return Response.json({ ok: true, result }); }
async function route(update: Update) { await account().enqueue(update); await runDurableObjectAlarm(account()); }
async function connect() { await route({ update_id: sequence++, business_connection: connection }); }
async function command(text: string, extra: Partial<Message> = {}) {
  await route({ update_id: sequence++, message: message({ text, from: connection.user, chat: { id: 100, type: 'private' }, business_connection_id: undefined, ...extra }) });
}
async function state(stub = chat()) {
  return runInDurableObject(stub, (_obj, ctx) => new Documents(ctx.storage.sql).get<Record<string, any>>('state', {}));
}
async function due(stub = chat()) {
  await runInDurableObject(stub, (_obj, ctx) => {
    const docs = new Documents(ctx.storage.sql); const value = docs.get<Record<string, any>>('state', {});
    value.pendingAt = Date.now() - 1; docs.set('state', value);
  });
}
async function archiveStep(stub = chat()) {
  await runInDurableObject(account(), (_obj, ctx) => new Documents(ctx.storage.sql).set('archiveNext', 0));
  await runInDurableObject(stub, (_obj, ctx) => ctx.storage.sql.exec('UPDATE archive_jobs SET next_at = 0'));
  await runDurableObjectAlarm(stub);
}
async function rows(table: string, stub = chat()) {
  return runInDurableObject(stub, (_obj, ctx) => ctx.storage.sql.exec(`SELECT * FROM ${table}`).toArray());
}

beforeEach(async () => {
  await reset(); sequence = 100; outgoing = []; handler = undefined;
  // Keep alarms in the future on the real runtime clock; trigger them explicitly.
  vi.spyOn(Date, 'now').mockReturnValue(2_000_000_000_000);
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    const url = String(input);
    const method = url.split('/').at(-1)!;
    const body = JSON.parse(String(init?.body ?? '{}'));
    outgoing.push({ method, body });
    const override = await handler?.(method, body);
    if (override) return override;
    if (method === 'getBusinessConnection') return ok(connection);
    if (method === 'getMe') return ok({ id: 900, username: 'secretary_bot' });
    if (method === 'createForumTopic') return ok({ message_thread_id: 77 });
    if (method === 'completions') return Response.json({ choices: [{ message: { content: '您好，我是聊天助理。' } }] });
    return ok(message({ message_id: 1000 + outgoing.length, from: connection.user, text: body.text, chat: { id: Number(body.chat_id), type: 'private' } }));
  });
});
afterEach(async () => { await reset(); vi.restoreAllMocks(); });

describe('Webhook and owner controls', () => {
  it('rejects other owners, group commands and ordinary private notes', async () => {
    await route({ update_id: 1, business_connection: { ...connection, user: { id: 999, first_name: 'Other' } } });
    expect((await account().policy()).connection).toBeUndefined();
    await command('/resume all', { from: { id: 999, first_name: 'Other' } });
    await command('/resume all', { sender_chat: { id: -100999, type: 'supergroup' } });
    await command('/resume all', { chat: { id: -100999, type: 'supergroup' }, message_thread_id: 77 });
    await command('/resume all', { chat: { id: 999, type: 'private' } });
    await command('普通备注', { chat: { id: 100, type: 'private' }, message_thread_id: 77 });
    expect((await account().policy()).paused).toBe(true);
    expect(outgoing).toHaveLength(0);
  });
  it('validates unknown business connections before forwarding', async () => {
    handler = method => method === 'getBusinessConnection' ? ok({ ...connection, user: { id: 999 } }) : undefined;
    await route({ update_id: 1, business_message: message() });
    expect(await rows('messages')).toHaveLength(0);
  });
  it('keeps latest connection state when older updates arrive', async () => {
    await connect();
    await route({ update_id: 300, business_connection: { ...connection, is_enabled: false } });
    await route({ update_id: 200, business_connection: connection });
    expect((await account().policy()).connection?.is_enabled).toBe(false);
  });
  it('resolves topics, preserves local pause and does not reset quotas on resume', async () => {
    await connect(); await chat().ingest({ update_id: 1, business_message: message() });
    await account().registerTopic('200', 77);
    await command('/pause', { chat: { id: 100, type: 'private' }, message_thread_id: 77 });
    await command('/resume all');
    expect((await state()).paused).toBe(true);
    await command('/limit 3 200');
    expect((await state()).limit).toBe(3);
    await command('/prompt 测试提示词');
    expect((await account().policy()).prompt).toBe('测试提示词');
  });
  it('uses the reconnected account while keeping the same topic and quota', async () => {
    await connect(); await chat().ingest({ update_id: 1, business_message: message() }); await archiveStep();
    await chat().control(700, 'limit', 7);
    await route({ update_id: 300, business_connection: { ...connection, id: 'new-conn', date: 2 } });
    await route({ update_id: 301, business_message: message({ business_connection_id: 'new-conn', message_id: 2 }) });
    expect((await account().policy()).connection?.id).toBe('new-conn');
    expect((await state()).topic).toBe(77);
    expect((await state()).limit).toBe(7);
  });
  it('does not apply a duplicate reset twice', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    await chat().control(500, 'reset');
    await runInDurableObject(chat(), (_obj, ctx) => {
      const docs = new Documents(ctx.storage.sql); const value = docs.get<Record<string, any>>('state', {}); value.used = 2; docs.set('state', value);
    });
    await chat().control(500, 'reset');
    expect((await state()).used).toBe(2);
  });
});

describe('Durable archive', () => {
  it('deduplicates concurrent updates and creates one topic', async () => {
    const update = { update_id: 1, business_message: message() };
    await Promise.all([chat().ingest(update), chat().ingest(update)]);
    expect(await rows('archive_jobs')).toHaveLength(1);
    await archiveStep(); await archiveStep();
    expect(outgoing.filter(x => x.method === 'createForumTopic')).toHaveLength(1);
    expect(outgoing.some(x => x.method === 'createForumTopic' && x.body.chat_id === '100')).toBe(true);
    expect(outgoing.some(x => x.body.text === '你好' && x.body.chat_id === '100' && x.body.message_thread_id === 77 && !x.body.business_connection_id)).toBe(true);
  });
  it('preserves versions and tombstones when original arrives after edit/delete', async () => {
    const edited = message({ text: '新版', edit_date: Math.floor(Date.now() / 1000) + 1 });
    await chat().ingest({ update_id: 3, edited_business_message: edited });
    await chat().ingest({ update_id: 4, deleted_business_messages: { business_connection_id: 'conn', chat: message().chat, message_ids: [1] } });
    await chat().ingest({ update_id: 1, business_message: message() });
    const saved = await rows('messages');
    expect(saved[0].deleted).toBe(1);
    expect(JSON.parse(String(saved[0].body)).text).toBe('新版');
    expect(await rows('archive_jobs')).toHaveLength(3);
  });
  it('records unknown deletion without inventing message contents', async () => {
    await chat().ingest({ update_id: 1, deleted_business_messages: { business_connection_id: 'conn', chat: message().chat, message_ids: [90] } });
    expect(String((await rows('archive_jobs'))[0].parts)).toContain('未收到原消息');
  });
  it.each(['photo', 'video', 'document', 'voice', 'audio', 'sticker', 'video_note', 'animation'])('resends %s by file_id', type => {
    const file = { file_id: 'file-ref' };
    const parts = archiveParts(message({ text: undefined, [type]: type === 'photo' ? [file] : file, caption: '说明', media_group_id: 'album' }), '100', false);
    expect(parts[1].body[type]).toBe('file-ref');
    expect(parts[0].body.text).toContain('album');
  });
  it('retains text entities and marks unsupported media explicitly', () => {
    const entities = [{ type: 'bold', offset: 0, length: 2 }];
    expect(archiveParts(message({ entities }), '100', false)[1].body.entities).toEqual(entities);
    expect(archiveParts(message({ text: undefined, poll: {} }), '100', false)[1].body.text).toContain('媒体未备份');
  });
  it('recovers queued messages after eviction', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    await evictDurableObject(chat());
    await archiveStep(); await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('done');
  });
  it('does not re-send an archive step interrupted after its send reservation', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    await runInDurableObject(chat(), (_obj, ctx) => ctx.storage.sql.exec("UPDATE archive_jobs SET status = 'sending'"));
    await evictDurableObject(chat());
    await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('uncertain');
    expect(outgoing).toHaveLength(0);
  });
  it('honors rate limiting and retains ambiguous sends for review', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    handler = method => method === 'sendMessage' ? Response.json({ ok: false, error_code: 429, parameters: { retry_after: 30 } }, { status: 429 }) : undefined;
    await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('pending');
    expect(Number((await rows('archive_jobs'))[0].next_at)).toBeGreaterThanOrEqual(Date.now() + 29000);
    handler = method => { if (method === 'sendMessage') throw new Error('network'); return undefined; };
    await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('uncertain');
    await chat().control(800, 'retry');
    expect((await rows('archive_jobs'))[0].status).toBe('uncertain');
  });
  it('retains a closed private topic failure without calling supergroup-only APIs', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    let rejected = false;
    handler = (method, body) => {
      if (method === 'sendMessage' && body.chat_id === '100' && !rejected) { rejected = true; return Response.json({ ok: false, error_code: 400, description: 'Bad Request: TOPIC_CLOSED' }); }
    };
    await archiveStep();
    expect(outgoing.some(x => x.method === 'reopenForumTopic')).toBe(false);
    expect((await rows('archive_jobs'))[0].status).toBe('failed');
    await chat().control(800, 'retry'); await archiveStep();
    expect((await rows('archive_jobs'))[0].step).toBe(1);
  });
  it('rebuilds a deleted topic with a discontinuity notice', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    let deleted = false;
    handler = (method, body) => {
      if (method === 'sendMessage' && body.chat_id === '100' && !deleted) { deleted = true; return Response.json({ ok: false, error_code: 400, description: 'Bad Request: message thread not found' }); }
      if (method === 'createForumTopic' && deleted) return ok({ message_thread_id: 88 });
    };
    await archiveStep(); await archiveStep();
    expect((await state()).topic).toBe(88);
    expect(outgoing.some(x => x.body.text?.includes('原话题已删除'))).toBe(true);
  });
  it('retains rejected private sends until bot access is restored and retry is requested', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    handler = (method, body) => method === 'sendMessage' && body.chat_id === '100' ? Response.json({ ok: false, error_code: 403 }) : undefined;
    for (let i = 0; i < 5; i++) await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('failed');
    handler = undefined;
    await chat().control(800, 'retry'); await archiveStep(); await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('done');
  });
  it('permits explicit repair of an uncertain step after human inspection', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    handler = (method, body) => { if (method === 'sendMessage' && body.chat_id === '100') throw new Error('timeout'); };
    await archiveStep();
    handler = undefined;
    const job = (await rows('archive_jobs'))[0];
    await command(`/archive_resolve ${job.id} sent 900`, { chat: { id: 100, type: 'private' }, message_thread_id: 77 });
    await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('done');
    expect(outgoing.some(x => x.body.text === '你好')).toBe(true);
  });
  it('allows owner to bind a manually created topic but prevents cross-contact reuse', async () => {
    await chat().ingest({ update_id: 1, business_message: message() });
    await command('/topic_bind 200', { chat: { id: 100, type: 'private' }, message_thread_id: 99 });
    expect((await state()).topic).toBe(99);
    await chat('201').ingest({ update_id: 2, business_message: message({ chat: { id: 201, type: 'private' } }) });
    await command('/topic_bind 201', { chat: { id: 100, type: 'private' }, message_thread_id: 99 });
    expect((await state(chat('201'))).topic).toBeUndefined();
  });
});

describe('AI policy and races', () => {
  async function ready() { await connect(); await command('/resume all'); await chat().ingest({ update_id: 1, business_message: message() }); await due(); }
  it('does not schedule captions or pure media', async () => {
    await connect(); await command('/resume all');
    await chat().ingest({ update_id: 1, business_message: message({ text: undefined, caption: '解释图片', photo: [{ file_id: 'x' }] }) });
    expect((await state()).pendingAt).toBe(0);
    expect(await chat().prepare()).toBeNull();
  });
  it('does not answer messages received while globally paused when resumed', async () => {
    await connect(); await chat().ingest({ update_id: 1, business_message: message() });
    await command('/resume all');
    expect((await state()).pendingAt).toBe(0);
    expect(await chat().prepare()).toBeNull();
  });
  it('debounces bursts and caps waiting at 10 seconds', async () => {
    await ready();
    const first = (await state()).firstPending;
    vi.mocked(Date.now).mockReturnValue(first + 2500);
    await chat().ingest({ update_id: 2, business_message: message({ message_id: 2 }) });
    expect((await state()).pendingAt).toBe(first + 5500);
    vi.mocked(Date.now).mockReturnValue(first + 9000);
    await chat().ingest({ update_id: 3, business_message: message({ message_id: 3 }) });
    expect((await state()).pendingAt).toBe(first + 10000);
  });
  it('only one preparation owns the quota and successful echo is not re-archived', async () => {
    await ready(); await chat().control(800, 'limit', 1);
    const tickets = await Promise.all([chat().prepare(), chat().prepare()]);
    expect(tickets.filter(Boolean)).toHaveLength(1);
    await chat().finishAi(tickets.find(Boolean)!, 'AI 回复');
    expect((await state()).used).toBe(1);
    const body = JSON.parse(String((await rows('messages')).find(row => JSON.parse(String(row.body)).text === 'AI 回复')!.body));
    await chat().ingest({ update_id: 50, business_message: body });
    expect(await rows('archive_jobs')).toHaveLength(2);
    expect((await state()).paused).toBe(false);
    await chat().ingest({ update_id: 60, business_message: message({ message_id: 2 }) }); await due();
    expect(await chat().prepare()).toBeNull();
  });
  it('human takeover cancels in-flight generation and pauses the contact', async () => {
    await ready(); const ticket = await chat().prepare();
    await chat().ingest({ update_id: 2, business_message: message({ message_id: 2, from: connection.user, text: '我来处理' }) });
    await chat().finishAi(ticket!, '旧回复');
    expect((await state()).paused).toBe(true);
    expect(outgoing.filter(x => x.body.business_connection_id)).toHaveLength(0);
  });
  it.each(['pause', 'prompt', 'disconnect'])('%s invalidates a pending generation', async action => {
    await ready(); const ticket = await chat().prepare();
    if (action === 'pause') await command('/pause all');
    if (action === 'prompt') await command('/prompt 新提示词');
    if (action === 'disconnect') await route({ update_id: 400, business_connection: { ...connection, is_enabled: false } });
    await chat().finishAi(ticket!, '旧回复');
    expect(outgoing.filter(x => x.body.business_connection_id)).toHaveLength(0);
  });
  it('excludes deleted text and never replies outside the 24-hour window', async () => {
    await ready();
    await chat().ingest({ update_id: 2, business_message: message({ message_id: 2, text: '要保留' }) });
    await chat().ingest({ update_id: 3, deleted_business_messages: { business_connection_id: 'conn', chat: message().chat, message_ids: [1] } }); await due();
    const ticket = await chat().prepare();
    expect(ticket!.messages.map(m => m.content)).not.toContain('你好');
    await chat().failAi(ticket!.token);
    await runInDurableObject(chat(), (_obj, ctx) => {
      const docs = new Documents(ctx.storage.sql); const value = docs.get<Record<string, any>>('state', {});
      value.lastIncoming = Date.now() - 86_400_001; value.pendingAt = Date.now() - 1; docs.set('state', value);
    });
    expect(await chat().prepare()).toBeNull();
  });
  it('bounds model context while preserving the newest texts and updated versions', async () => {
    await ready();
    for (let i = 2; i <= 25; i++) await chat().ingest({ update_id: i, business_message: message({ message_id: i, text: `${i}:` + '文'.repeat(2000) }) });
    await chat().ingest({ update_id: 30, edited_business_message: message({ message_id: 25, text: '最新版本', edit_date: Math.floor(Date.now() / 1000) + 1 }) });
    await due();
    const ticket = await chat().prepare();
    expect(ticket!.messages.length).toBeLessThanOrEqual(21);
    expect(ticket!.messages.slice(1).reduce((n, m) => n + m.content.length, 0)).toBeLessThanOrEqual(24000);
    expect(ticket!.messages.at(-1)?.content).toBe('最新版本');
  });
  it('marks an interrupted AI send uncertain after durable recovery', async () => {
    await ready(); const ticket = await chat().prepare();
    await runInDurableObject(chat(), (_obj, ctx) => {
      const docs = new Documents(ctx.storage.sql); const value = docs.get<Record<string, any>>('state', {});
      value.flight.phase = 'sending'; value.used = 1; docs.set('state', value);
    });
    await runInDurableObject(ai(), (_obj, ctx) => {
      const docs = new Documents(ctx.storage.sql); docs.set('active', { token: ticket!.token }); docs.set('next', 0);
    });
    await evictDurableObject(chat()); await evictDurableObject(ai());
    await runDurableObjectAlarm(ai());
    expect((await state()).flight.phase).toBe('uncertain');
    expect((await state()).used).toBe(1);
    expect(outgoing.filter(x => x.body.business_connection_id)).toHaveLength(0);
  });
  it('reserves uncertain send quota and does not retry it until manual reset', async () => {
    await ready(); const ticket = await chat().prepare();
    handler = (_method, body) => { if (body.business_connection_id) throw new Error('timeout'); };
    await chat().finishAi(ticket!, 'hello');
    expect((await state()).used).toBe(1);
    expect((await state()).flight.phase).toBe('uncertain');
    await chat().finishAi(ticket!, 'hello');
    expect(outgoing.filter(x => x.body.business_connection_id)).toHaveLength(1);
    await chat().control(800, 'reset');
    expect((await state()).used).toBe(0);
    expect((await state()).flight).toBeUndefined();
  });
  it('does not deduct quota on an explicit send rejection', async () => {
    await ready(); const ticket = await chat().prepare();
    handler = (_method, body) => body.business_connection_id ? Response.json({ ok: false, error_code: 403 }) : undefined;
    await chat().finishAi(ticket!, 'hello');
    expect((await state()).used).toBe(0);
  });
  it('retries an AI 429 after retry_after without generating again', async () => {
    await ready();
    let limited = false;
    handler = (_method, body) => {
      if (body.business_connection_id && !limited) { limited = true; return Response.json({ ok: false, error_code: 429, parameters: { retry_after: 30 } }); }
    };
    await ai().enqueue('200', Date.now() - 1); await runDurableObjectAlarm(ai());
    expect((await state()).used).toBe(0);
    expect(await runInDurableObject(ai(), (_obj, ctx) => ctx.storage.getAlarm())).toBe(Date.now() + 30000);
    vi.mocked(Date.now).mockReturnValue(Date.now() + 30001);
    await runDurableObjectAlarm(ai());
    expect((await state()).used).toBe(1);
    expect(outgoing.filter(x => x.method === 'completions')).toHaveLength(1);
  });
  it('invalidates an AI response when newer text arrives during generation', async () => {
    await ready(); const ticket = await chat().prepare();
    await chat().ingest({ update_id: 2, business_message: message({ message_id: 2, text: '等等，我改主意了' }) });
    await chat().finishAi(ticket!, '已过时');
    expect(outgoing.filter(x => x.body.business_connection_id)).toHaveLength(0);
    await due();
    const next = await chat().prepare();
    expect(next?.messages.at(-1)?.content).toBe('等等，我改主意了');
  });
  it('archives while the separate model call is still waiting', async () => {
    await ready();
    handler = async method => {
      if (method === 'completions') {
        await new Promise(resolve => setTimeout(resolve, 1000));
        return Response.json({ choices: [{ message: { content: '回复' } }] });
      }
    };
    await ai().enqueue('200', Date.now() - 1);
    const running = runDurableObjectAlarm(ai());
    for (let i = 0; i < 50 && !outgoing.some(x => x.method === 'completions'); i++) await new Promise(resolve => setTimeout(resolve, 10));
    expect(outgoing.some(x => x.method === 'completions')).toBe(true);
    await archiveStep(); await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('done');
    expect((await state()).flight.phase).toBe('generating');
    await running;
    expect((await state()).used).toBe(1);
  });
  it('runs a compatible completion through the separate AI alarm', async () => {
    await ready();
    await ai().enqueue('200', Date.now() - 1); await runDurableObjectAlarm(ai());
    expect(outgoing.some(x => x.method === 'completions' && x.body.model === 'test-model' && x.body.stream === false)).toBe(true);
    expect((await state()).used).toBe(1);
  });
  it('model failure does not stop archive delivery', async () => {
    await ready();
    handler = method => { if (method === 'completions') throw new Error('model timeout'); };
    await ai().enqueue('200', Date.now() - 1); await runDurableObjectAlarm(ai());
    await archiveStep(); await archiveStep();
    expect((await rows('archive_jobs'))[0].status).toBe('done');
    expect((await state()).used).toBe(0);
  });
});
