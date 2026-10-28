import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {writeFileSync} from 'node:fs';
import {pathToFileURL} from 'node:url';
import {emitBeforeAgentStart} from './pi-runner-compat.mjs';
const [root,extensionPath,cwd,source]=process.argv.slice(2);
const {ExtensionRunner}=await import(pathToFileURL(root+'/core/extensions/runner.js'));
const {createExtensionRuntime,loadExtensionFromFactory}=await import(pathToFileURL(root+'/core/extensions/loader.js'));
const {createEventBus}=await import(pathToFileURL(root+'/core/event-bus.js'));
const {SessionManager}=await import(pathToFileURL(root+'/core/session-manager.js'));
const {buildSystemPrompt}=await import(pathToFileURL(root+'/core/system-prompt.js'));
const factory=(await import(pathToFileURL(extensionPath))).default;
let revision=1,metadata={},status='open';const requests=[],writes=[],errors=[];
const server=createServer(async(req,res)=>{
 let raw='';for await(const chunk of req)raw+=chunk;const body=raw?JSON.parse(raw):{};const url=new URL(req.url,'http://daemon.example');requests.push({method:req.method,path:url.pathname,query:Object.fromEntries(url.searchParams),body});res.setHeader('content-type','application/json');
 const respond=value=>res.end(JSON.stringify(value));
 if(url.pathname==='/api/v1/projects/resolve'){assert.equal(body.name,'example-workspace');respond({project:{id:42,name:'example-workspace'}});}
 else if(url.pathname==='/api/v1/projects/42/issues/abc4/metadata'){
  assert.equal(req.headers['if-match'],`"rev-${revision}"`);assert.equal(body.actor,'actor');metadata={...metadata,...body.patch};revision++;writes.push(body.patch);respond({issue:{status,short_id:'abc4',metadata,revision}});
 } else if(url.pathname==='/api/v1/projects/42/issues/abc4')respond({issue:{status,short_id:'abc4',metadata,revision}});
 else if(url.pathname==='/api/v1/projects/42/issues'){
  assert.equal(url.searchParams.get('meta'),'notify.YWN0b3IvdGVhbW1hdGU');assert.equal(url.searchParams.get('status'),'open');
  respond({issues:[{short_id:'abc4',title:'example task',metadata:{'notify.YWN0b3IvdGVhbW1hdGU':{from:'coordinator',message:'fresh request'}}}]});
 } else if(req.method==='GET'&&url.pathname==='/api/v1/ping')respond({ok:true,service:'kata',version:'test',pid:process.pid});
 else if(req.method==='POST'&&url.pathname==='/api/v1/ui/telemetry'&&body.event==='agent_active'){res.statusCode=202;respond({status:'queued'});}
 else if(url.pathname==='/health')respond({status:'ok',api_version:1});
 else {errors.push(req.method+' '+url.pathname);res.statusCode=404;respond({error:'unexpected route'});}
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));process.env.KATA_SERVER=`http://127.0.0.1:${server.address().port}`;process.env.KATA_REF='abc4';process.env.KATA_INBOX_USER='actor/teammate';process.env.KATA_AUTHOR='actor';process.env.KATA_TEAMMATE='teammate';delete process.env.KATA_SESSION_ID;
try {
  const manager=SessionManager.inMemory(cwd),bus=createEventBus();
 async function load(manager) {
  const runtime=createExtensionRuntime(),extension=await loadExtensionFromFactory(factory,cwd,bus,runtime,extensionPath);
  const runner=new ExtensionRunner([extension],runtime,cwd,manager,{});runner.onError(e=>errors.push(e));
  return {runtime,extension,runner};
 }
 const initial=await load(manager);let active=initial,runner=active.runner;
 writeFileSync(source,'source current');await runner.emit({type:'session_start',reason:'startup'});assert.equal(metadata['work.attention'],'ok');assert.match(metadata['work.attention_session'],/^[0-9a-f]{64}$/);
 const originalOwner=metadata['work.attention_session'];const base={customPrompt:'base',cwd,sections:{other:'preserved'}};
 const result=await emitBeforeAgentStart(runner,'first',base),prompt=buildSystemPrompt(result.systemPromptOptions);assert(prompt.includes('source current'));assert(prompt.includes('fresh request'));
 metadata['work.attention']='stuck';await runner.emit({type:'session_start',reason:'startup'});await runner.emit({type:'agent_end',messages:[]});assert.equal(metadata['work.attention'],'stuck');assert.equal(writes.length,1);
 const oldSession=manager.getSessionId();
 await runner.emit({type:'session_shutdown',reason:'reload'});runner.invalidate();active=await load(manager);runner=active.runner;await runner.emit({type:'session_start',reason:'reload'});assert.equal(metadata['work.attention'],'stuck');assert.equal(writes.length,1,'reload preserves explicit handoff');
 status='closed';await runner.emit({type:'session_shutdown',reason:'new'});runner.invalidate();active=await load(SessionManager.inMemory(cwd));runner=active.runner;await runner.emit({type:'session_start',reason:'new'});await runner.emit({type:'session_shutdown',reason:'quit'});runner.invalidate();assert.equal(metadata['work.attention'],'stuck');assert.equal(writes.length,1,'closed issue ignores new session and terminal writes');
 status='open';active=await load(SessionManager.inMemory(cwd));runner=active.runner;await runner.emit({type:'session_start',reason:'startup'});assert.equal(metadata['work.attention'],'ok');assert.notEqual(metadata['work.attention_session'],originalOwner);
 // Delayed old runtime end is dispatched with its own native context.
 const oldManager=new Proxy(manager,{get(target,key){if(key==='getSessionId')return ()=>oldSession;const value=Reflect.get(target,key);return typeof value==='function'?value.bind(target):value;}});const oldRunner=new ExtensionRunner([initial.extension],initial.runtime,cwd,oldManager,{});await oldRunner.emit({type:'session_shutdown',reason:'quit'});assert.equal(metadata['work.attention'],'ok');
 process.env.KATA_REF='unrelated-ref';await runner.emit({type:'session_shutdown',reason:'quit'});assert.equal(metadata['work.attention'],'needs-human');assert.equal(metadata['work.attention_msg'],'session ended without hand-off');assert.equal(writes.length,3);
 assert.deepEqual(errors,[]);assert(requests.length>0);
 console.log('Pi real Kata transport passed: explicit stdin-free session, host launch identity, source, exact inbox, reload handoff, closed-issue preservation, delayed cleanup and captured ref');
} finally {await new Promise(resolve=>server.close(resolve));}
