import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';
const [projectPath,userPath,cwd,mode]=process.argv.slice(2);
const project=(await import(pathToFileURL(projectPath))).default,user=(await import(pathToFileURL(userPath))).default;
let session='session-A';const calls=[],handlers=new Map();
const pi={on(name,handler){const list=handlers.get(name)||[];list.push(handler);handlers.set(name,list)},async exec(executable,args,options){calls.push({executable,args,options});return {code:0,stdout:args[0]==='agent-contract-hook'?JSON.stringify({hookSpecificOutput:{additionalContext:executable}}):'',stderr:'',killed:false}}};
const ctx={cwd,sessionManager:{getSessionId:()=>session},ui:{notify(){}}};
async function emit(name,event){for(const fn of [...(handlers.get(name)||[])])await fn(event,ctx)}
user(pi);project(pi);
await emit('session_start',{reason:'startup'});
const event={systemPromptOptions:{sections:{}}};await emit('before_agent_start',event);
assert.equal(calls.filter(c=>c.args[0]==='agent-hook').length,1);assert.equal(calls.filter(c=>c.args[0]==='agent-contract-hook').length,1);
assert.equal(event.systemPromptOptions.sections.kata_contract,mode==='contract'?'project-kata':'user-kata');
assert.equal(calls.find(c=>c.args[0]==='agent-hook').executable,mode==='attention'?'project-kata':'user-kata');
// A removed project adapter does not survive native resource reload.
await emit('session_shutdown',{reason:'reload'});user(pi);await emit('session_start',{reason:'reload'});
const next={systemPromptOptions:{sections:{}}};await emit('before_agent_start',next);assert.equal(next.systemPromptOptions.sections.kata_contract,'user-kata');
await emit('session_shutdown',{reason:'quit'});
const reloadedEnd=calls.filter(c=>c.args[0]==='agent-hook').at(-1);
assert.equal(reloadedEnd.args[3],'end');
assert.equal(reloadedEnd.executable,mode==='attention'?'project-kata':'user-kata','reload cleanup uses the executable that started attention');
// Detached callbacks cannot end the replacement session.
const retiring=[...(handlers.get('session_shutdown')||[])],oldSession=session;
await emit('session_shutdown',{reason:'new'});session='session-B';user(pi);await emit('session_start',{reason:'new'});
for(const fn of retiring)await fn({reason:'quit'},{...ctx,sessionManager:{getSessionId:()=>oldSession}});
assert.equal(calls.filter(c=>c.args[0]==='agent-hook'&&c.args[3]==='end').length,1);
process.env.KATA_REF='replacement-ref';await emit('session_shutdown',{reason:'quit'});await emit('session_shutdown',{reason:'quit'});
const attention=calls.filter(c=>c.args[0]==='agent-hook');assert.deepEqual(attention.map(c=>c.args[3]),['start','end','start','end']);assert.equal(attention[3].args[9],'issue-ref');
console.log('Pi capability precedence and reload fencing passed');
