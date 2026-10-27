import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';
const [projectPath,userPath,cwd]=process.argv.slice(2);
const project=(await import(pathToFileURL(projectPath))).default;
const user=(await import(pathToFileURL(userPath))).default;
let session='native-A',contract='contract current',inbox='request A',failInbox=false,failContract=false;
const calls=[],notifications=[],handlers=new Map();
const pi={on(name,fn){const list=handlers.get(name)||[];list.push(fn);handlers.set(name,list)},async exec(exe,args,opts){calls.push({exe,args,opts});assert.equal(opts.cwd,cwd);assert.equal(opts.timeout,10000);assert.equal(process.env.KATA_SERVER,'https://daemon.example');
 if(args[0]==='agent-contract-hook'){if(failContract)return {code:1,stdout:'',stderr:'failed',killed:false};return {code:0,stdout:JSON.stringify({hookSpecificOutput:{additionalContext:contract}}),stderr:'',killed:false};}
 if(args[0]==='inbox'){assert.deepEqual(args,['inbox','--context','--for',process.env.KATA_INBOX_USER]);return {code:failInbox?1:0,stdout:inbox,stderr:'',killed:false};}
 assert.deepEqual(args,['agent-hook','attention-native','pi',args[3],'--session',session,'--host-pid',String(process.pid),'--ref','issue-ref','--workspace',cwd]);return {code:0,stdout:'',stderr:'',killed:false};
}};
const ctx={cwd,sessionManager:{getSessionId:()=>session},ui:{notify:(message,kind)=>notifications.push({message,kind})}};
// Loading both scopes, in either order, must select project exactly once.
project(pi);user(pi);
async function emit(name,event){for(const fn of handlers.get(name)||[])await fn(event,ctx)}
await emit('session_start',{reason:'startup'});
assert.equal(calls.filter(c=>c.args[0]==='agent-hook').length,1);
const event={systemPromptOptions:{customPrompt:'base',sections:{other:'preserved'}}};
await emit('before_agent_start',event);
assert.equal(event.systemPromptOptions.sections.kata_contract,'contract current');assert.equal(event.systemPromptOptions.sections.kata_inbox,'request A');assert.equal(event.systemPromptOptions.sections.other,'preserved');assert.equal(event.systemPromptOptions.customPrompt,'base');
assert.equal(calls.filter(c=>c.args[0]==='agent-contract-hook').length,1);assert.deepEqual(calls.find(c=>c.args[0]==='agent-contract-hook').args,['agent-contract-hook','--source',"contract '$;`.txt"]);
assert.equal(calls[0].exe,"/example path/kata '$;`");
inbox='stale request';failInbox=true;failContract=true;
const failed={systemPromptOptions:{sections:{}}};await emit('before_agent_start',failed);assert(!JSON.stringify(failed.systemPromptOptions.sections).includes('stale request'));assert(!failed.systemPromptOptions.sections.kata_contract.includes('contract current'));assert(notifications.length>=2);
let attentionCalls=calls.filter(c=>c.args[0]==='agent-hook').length;
await emit('session_shutdown',{reason:'reload'});project(pi);user(pi);await emit('session_start',{reason:'reload'});await emit('agent_end',{});assert.equal(calls.filter(c=>c.args[0]==='agent-hook').length,attentionCalls);
for(const reason of ['new','resume','fork']){await emit('session_shutdown',{reason});session=`native-${reason}`;project(pi);user(pi);await emit('session_start',{reason});}
await emit('session_shutdown',{reason:'quit'});await emit('session_shutdown',{reason:'quit'});
assert.deepEqual(calls.filter(c=>c.args[0]==='agent-hook').map(c=>c.args[3]),['start','start','start','start','end']);
console.log('Pi native fixture passed');
