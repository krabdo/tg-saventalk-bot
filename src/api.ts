import type { AppEnv, JsonObject } from './types.js';
export class ApiError extends Error {
  constructor(public code: number, public description: string, public uncertain = false, public retryAfter = 0) {
    super(`API error ${code}`); // Never put upstream bodies (possibly private data) into logs.
  }
}
export async function readJson(response: Response, maxBytes = 2_000_000): Promise<unknown> {
  if (!response.body) throw new Error('Empty response');
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    length += value.byteLength;
    if (length > maxBytes) { await reader.cancel(); throw new Error('Response too large'); }
    chunks.push(value);
  }
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.length; }
  return JSON.parse(new TextDecoder().decode(bytes));
}
export async function telegram<T>(env: AppEnv, method: string, body: JsonObject): Promise<T> {
  try {
    const response = await fetch(`https://api.telegram.org/bot${env.BOT_TOKEN}/${method}`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      signal: AbortSignal.timeout(15_000), redirect: 'error',
    });
    const data = await readJson(response) as { ok?: boolean; result: T; error_code?: number; description?: string; parameters?: { retry_after?: number } };
    if (data.ok !== true) throw new ApiError(data.error_code ?? response.status, data.description ?? '', response.status >= 500, data.parameters?.retry_after ?? 0);
    return data.result;
  } catch (error) {
    if (error instanceof ApiError) throw error;
    throw new ApiError(0, 'Transport failure; outcome unknown', true);
  }
}
export async function completion(env: AppEnv, messages: { role: string; content: string }[]): Promise<string> {
  const base = new URL(env.AI_BASE_URL);
  if (base.protocol !== 'https:' || base.username || base.password || base.search || base.hash || !env.AI_MODEL || !env.AI_API_KEY) throw new Error('AI configuration invalid');
  const response = await fetch(`${base.href.replace(/\/$/, '')}/chat/completions`, {
    method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${env.AI_API_KEY}` },
    body: JSON.stringify({ model: env.AI_MODEL, messages, stream: false }),
    signal: AbortSignal.timeout(45_000), redirect: 'error',
  });
  if (!response.ok) { await response.body?.cancel(); throw new Error('AI request failed'); }
  const data = await readJson(response) as { choices?: { message?: { content?: unknown } }[] };
  const value = data.choices?.[0]?.message?.content;
  if (typeof value !== 'string' || !value.trim()) throw new Error('AI returned no text');
  return value.trim();
}
