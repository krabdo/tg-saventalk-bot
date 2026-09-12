import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Runtime } from '../src/runtime';
import type { LocalActor, ActorState } from '../src/actor';
import type { AppEnv } from '../src/types';
let directory = '';
let runtime: Runtime;
export const env = {} as AppEnv;
export async function reset() {
  if (runtime) await runtime.close();
  if (directory) rmSync(directory, {recursive:true,force:true});
  directory = mkdtempSync(join(tmpdir(),'secretary-test-'));
  runtime = new Runtime(directory, {OWNER_ID:'100',BOT_TOKEN:'123:test',AI_BASE_URL:'https://ai.example/v1',AI_MODEL:'test-model',AI_API_KEY:'test-key'});
  Object.assign(env,runtime.env);
}
export async function runInDurableObject<T>(obj: LocalActor<AppEnv>, fn: (obj: any,ctx:ActorState)=>T) { return fn(obj,obj.ctx); }
export async function runDurableObjectAlarm(obj: LocalActor<AppEnv> & {alarm():Promise<void>}) { await obj.alarm(); return true; }
export async function evictDurableObject(_obj: unknown) {
  await runtime.close();
  runtime = new Runtime(directory, {OWNER_ID:'100',BOT_TOKEN:'123:test',AI_BASE_URL:'https://ai.example/v1',AI_MODEL:'test-model',AI_API_KEY:'test-key'});
  Object.assign(env,runtime.env);
}
