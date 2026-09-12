import { afterEach, expect, it, vi } from 'vitest';
import { mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { Runtime } from '../src/runtime';
import { poll, setup, allowedUpdates } from '../src/main';
import { Documents } from '../src/storage';
const dirs:string[]=[];
const instances:Runtime[]=[];
function make(dir=mkdtempSync(join(tmpdir(),'poll-test-'))) {
 dirs.push(dir); const r=new Runtime(dir,{OWNER_ID:'100',BOT_TOKEN:'1:test',AI_BASE_URL:'',AI_MODEL:''}); instances.push(r);return r;
}
afterEach(async()=>{vi.restoreAllMocks();for(const r of instances.splice(0))await r.close();for(const d of dirs.splice(0))rmSync(d,{recursive:true,force:true});});
it('removes webhook without dropping updates and registers private commands',async()=>{
 const r=make();const calls:any[]=[];
 vi.stubGlobal('fetch',vi.fn(async(url,init)=>{calls.push({url,body:JSON.parse(init.body)});return Response.json({ok:true,result:String(url).endsWith('getMe')?{id:1,has_topics_enabled:true,can_connect_to_business:true}:true});}));
 await setup(r.env);expect(calls[1].body).toEqual({drop_pending_updates:false});expect(calls[2].body.scope).toEqual({type:'chat',chat_id:'100'});
 vi.unstubAllGlobals();
});
it('commits batch and offset before the next poll and survives process restart',async()=>{
 let r=make();const stop=new AbortController();let n=0;
 vi.stubGlobal('fetch',vi.fn(async(_url,init)=>{
  const body=JSON.parse(init.body);expect(body.allowed_updates).toEqual(allowedUpdates);
  if(n++===0)return Response.json({ok:true,result:[{update_id:5},{update_id:7}]});
  expect(body.offset).toBe(8);expect(r.env.ACCOUNTS.getByName('100').ctx.storage.sql.exec('SELECT * FROM inbox').toArray()).toHaveLength(2);
  stop.abort();return Response.json({ok:true,result:[]});
 }));
 await poll(r,stop.signal);const dir=r.directory;await r.close();r=make(dir);
 expect(new Documents(r.env.ACCOUNTS.getByName('100').ctx.storage.sql).get('offset',0)).toBe(8);
 vi.unstubAllGlobals();
});
it('rolls back failed SQLite transactions',()=>{
 const r=make();const s=r.env.ACCOUNTS.getByName('100').ctx.storage;
 expect(()=>s.transactionSync(()=>{s.sql.exec("INSERT INTO inbox(id,body) VALUES(1,'{}')");throw new Error('crash');})).toThrow();
 expect(s.sql.exec('SELECT * FROM inbox').toArray()).toHaveLength(0);
});
it('does not overlap an actor alarm while other actors remain runnable',async()=>{
 const r=make();const a=r.env.ACCOUNTS.getByName('100');const c=r.env.CHATS.getByName('100:200');
 let release!:()=>void;const alarm=vi.spyOn(a,'alarm').mockImplementation(()=>new Promise<void>(resolve=>{release=resolve;}));
 const other=vi.spyOn(c,'alarm').mockResolvedValue();
 await a.ctx.storage.setAlarm(1);await c.ctx.storage.setAlarm(1);
 await r.tick();await r.tick();expect(alarm).toHaveBeenCalledTimes(1);expect(other).toHaveBeenCalled();release();
});
