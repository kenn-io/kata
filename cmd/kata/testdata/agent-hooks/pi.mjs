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
 assert.deepEqual(args,['agent-hooks','attention-native','pi',args[3],'--session',session,'--host-pid',String(process.pid),'--ref','issue-ref','--workspace',cwd]);return {code:0,stdout:'',stderr:'',killed:false};
}};
const ctx={cwd,sessionManager:{getSessionId:()=>session},ui:{notify:(message,kind)=>notifications.push({message,kind})}};
// Loading both scopes, in either order, must select project exactly once.
project(pi);user(pi);
async function emit(name,event){for(const fn of handlers.get(name)||[])await fn(event,ctx)}
await emit('session_start',{reason:'startup'});
assert.equal(calls.filter(c=>c.args[0]==='agent-hooks').length,1);
let event={systemPromptOptions:{customPrompt:'base',sections:{other:'preserved'}}};
await emit('before_agent_start',event);
assert.equal(event.systemPromptOptions.sections.kata_contract,'contract current');assert.equal(event.systemPromptOptions.sections.kata_inbox,'request A');assert.equal(event.systemPromptOptions.sections.other,'preserved');assert.equal(event.systemPromptOptions.customPrompt,'base');
assert.equal(calls.filter(c=>c.args[0]==='agent-contract-hook').length,1);assert.deepEqual(calls.find(c=>c.args[0]==='agent-contract-hook').args,['agent-contract-hook','--source',"contract '$;`.txt"]);
assert.equal(calls[0].exe,"/example path/kata '$;`");
inbox='';contract='new contract';await emit('before_agent_start',event);assert.equal(event.systemPromptOptions.sections.kata_inbox,'');assert.equal(event.systemPromptOptions.sections.kata_contract,'new contract');
inbox='stale request';failInbox=true;failContract=true;await emit('before_agent_start',event);assert(!JSON.stringify(event.systemPromptOptions.sections).includes('stale request'));assert(!event.systemPromptOptions.sections.kata_contract.includes('new contract'));assert(notifications.length>=2);
failInbox=false;failContract=false;inbox='request B';contract='forced contract';event={systemPromptOptions:{sections:{other:'preserved'},forceSystemPrompt:'authored forced'}};
await emit('before_agent_start',event);assert(event.systemPromptOptions.forceSystemPrompt.includes('authored forced'));assert.equal(event.systemPromptOptions.forceSystemPrompt.split('forced contract').length-1,1);
// Later authored whole-prompt replacement retains Pi's native precedence.
event.systemPromptOptions.forceSystemPrompt='later authored';assert.equal(event.systemPromptOptions.forceSystemPrompt,'later authored');
inbox='';await emit('before_agent_start',event);assert(!event.systemPromptOptions.forceSystemPrompt.includes('request B'));assert.equal(event.systemPromptOptions.forceSystemPrompt.split('forced contract').length-1,1);
let attentionCalls=calls.filter(c=>c.args[0]==='agent-hooks').length;
await emit('session_shutdown',{reason:'reload'});project(pi);user(pi);await emit('session_start',{reason:'reload'});await emit('agent_end',{});assert.equal(calls.filter(c=>c.args[0]==='agent-hooks').length,attentionCalls);
for(const reason of ['new','resume','fork']){await emit('session_shutdown',{reason});session=`native-${reason}`;project(pi);user(pi);await emit('session_start',{reason});}
await emit('session_shutdown',{reason:'quit'});await emit('session_shutdown',{reason:'quit'});
assert.deepEqual(calls.filter(c=>c.args[0]==='agent-hooks').map(c=>c.args[3]),['start','start','start','start','end']);
console.log('Pi native fixture passed');
