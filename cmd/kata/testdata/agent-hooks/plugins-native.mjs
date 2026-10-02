import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
const [api,userFile,projectFile,workspace,otherWorkspace,mode]=process.argv.slice(2);
if(api==='amp'&&mode==='single-user-contract-retry'){
 globalThis[Symbol.for('kata.amp.native.plugins.v1')]={
  adapters:new Map(),starts:new Map(),contracts:new Set(),context:new WeakMap()
 };
}
// V2 fixture implements only the published define identity. SDK-backed loading
// is a separate validation; v1 and Amp have no value SDK imports.
if(api==='v2'){
 for(const file of [userFile,projectFile]){
  const pkg=path.join(path.dirname(file),'node_modules/@opencode/plugin');await fs.mkdir(path.dirname(pkg),{recursive:true});
  if(process.env.KATA_NATIVE_PLUGIN_SDK_ROOT){await fs.symlink(path.join(process.env.KATA_NATIVE_PLUGIN_SDK_ROOT,'node_modules/@opencode/plugin'),pkg,'dir');}
  else {await fs.mkdir(pkg);await fs.writeFile(path.join(pkg,'package.json'),JSON.stringify({type:'module',exports:'./index.js'}));await fs.writeFile(path.join(pkg,'index.js'),'export const Plugin={define:p=>p};');}
 }
}
// Node needs an ESM boundary; native Amp/OpenCode load these modules as ESM.
for(const file of [userFile,projectFile])await fs.writeFile(path.join(path.dirname(file),'package.json'),' {"type":"module"}');
const user=await import(pathToFileURL(userFile));const project=await import(pathToFileURL(projectFile));
async function sessionInfo(id){
 if(id==='lookup-failure')throw new Error('native lookup failed');
 if(id==='lookup-missing')return undefined;
 return {id,parentID:id==='child-session'?'native-session':undefined,location:{directory:id==='other-session'?otherWorkspace:workspace}};
}
const v1Client={session:{get:async input=>{assert.ok(input.path.id);assert.equal(input.query.directory,workspace);return {data:await sessionInfo(input.path.id)}}}};
const registrations=[];let cleanup;let releaseContext;let pendingSetup;
if(api==='amp'){
 function load(mod){const hooks=new Map();mod.default({on:(name,fn)=>hooks.set(name,fn),helpers:{filePathFromURI:u=>new URL(String(u)).pathname}});registrations.push(hooks)}
 load(user);
}else if(api==='v1'){
 registrations.push(await user.KataPlugin({directory:workspace,client:v1Client}));if(mode!=='project-unloaded')registrations.push(await project.KataPlugin({directory:workspace,client:v1Client}));
}else{
 for(const mod of mode==='project-unloaded'?[user]:[user,project]){
  const hooks=new Map();const setup=mod.default.setup({location:{directory:'/wrong/location'},session:{get:async({sessionID})=>sessionInfo(sessionID),hook:async(name,fn)=>{
   if(mod===project&&mode==='project-pending-context'&&name==='context')await new Promise(resolve=>releaseContext=resolve);
   hooks.set(name,fn);return{dispose:async()=>hooks.delete(name)}}}});
  if(mod===project&&mode==='project-pending-context'){pendingSetup=setup;await new Promise(resolve=>setImmediate(resolve));}else cleanup=await setup;
  registrations.push(hooks)
 }

}
async function start(id){
 for(const h of registrations){if(api==='amp')await h.get('session.start')?.({thread:{id}},{$:async()=>({exitCode:0,stdout:workspace+'\n',stderr:''}),system:{}});
 else if(api==='v1')await h['chat.message']?.({sessionID:id},{});else await h.get('prompt')?.({sessionID:id,prompt:{text:'hello'}})}
}
async function prompt(id){
 const system=api==='v2'?[{type:'text',text:'authored'}]:['authored'];let messages=[];
 for(const h of registrations){if(api==='amp'){const r=await h.get('agent.start')?.({thread:{id}},{$:async()=>({exitCode:0,stdout:workspace+'\n',stderr:''}),system:{}});if(r?.message){assert.equal(r.message.display,false);messages.push(r.message.content)}}
 else if(api==='v1')await h['experimental.chat.system.transform']?.({sessionID:id},{system});else await h.get('context')?.({sessionID:id,system})}
 if(api!=='amp'){assert.deepEqual(system[0],api==='v2'?{type:'text',text:'authored'}:'authored');messages=system.slice(1).map(s=>typeof s==='string'?s:s.text)}
 assert.equal(messages.length,1,'one selected context contribution');return messages[0];
}
if(pendingSetup){assert.match(await prompt('native-session'),/fresh request/);releaseContext();cleanup=await pendingSetup;}
await start('native-session');await start('native-session');
if(api!=='amp'){
 const attentionCalls=async()=>{const calls=(await fs.readFile(process.env.KATA_PLUGIN_LOG,'utf8')).trim().split('\n').map(JSON.parse);return calls.filter(c=>c.args.includes('attention-native'));};
 const parentCalls=await attentionCalls();assert.equal(parentCalls.length,1);
 await start('child-session');await start('lookup-failure');await start('lookup-missing');
 assert.deepEqual(await attentionCalls(),parentCalls,'child or unresolved sessions cannot replace parent attention ownership');
 assert.match(await prompt('child-session'),/fresh request/,'child context still refreshes inbox');
}
if(api==='amp'&&mode==='single-user-contract-retry'){
 const concurrent=await Promise.all([prompt('native-session'),prompt('native-session')]);
 assert.equal(concurrent.filter(text=>text.includes('contract:source')).length,0,'a failed read contributes no contract');
 assert.match(concurrent.join('\n'),/unavailable/,'the failed first read is reported');
 const callsAfterConcurrent=(await fs.readFile(process.env.KATA_PLUGIN_LOG,'utf8')).trim().split('\n').map(JSON.parse);
 assert.equal(callsAfterConcurrent.filter(call=>call.args.includes('agent-contract-hook')).length,1,'concurrent prompts share one in-flight contract read');
 const retried=await prompt('native-session');
 assert.match(retried,/contract:source \$\(touch bad\) `echo bad`; spaced/,'a later prompt retries and delivers the recovered contract');
 const callsAfterRetry=(await fs.readFile(process.env.KATA_PLUGIN_LOG,'utf8')).trim().split('\n').map(JSON.parse);
 assert.equal(callsAfterRetry.filter(call=>call.args.includes('agent-contract-hook')).length,2,'the failed read is retried once');
}else{
 const first=await prompt('native-session');assert.match(first,/contract:source \$\(touch bad\) `echo bad`; spaced/);assert.match(first,/fresh request/);
 await fs.writeFile(process.env.KATA_PLUGIN_STATE,'');const second=await prompt('native-session');assert.doesNotMatch(second,/fresh request/);
 if(api==='amp')assert.doesNotMatch(second,/contract:source/,'Amp contract only once per thread');else assert.match(second,/contract:source/);
 await fs.writeFile(process.env.KATA_PLUGIN_STATE,'fail');const third=await prompt('native-session');assert.doesNotMatch(third,/fresh request/);assert.match(third,/unavailable/);
}
// Exercise real turn/idle/deletion event shapes before launcher cleanup. None
// of these signals establishes terminal ownership of the native host.
for(const hooks of registrations){
 if(api==='amp')await hooks.get('agent.end')?.({thread:{id:'native-session'},status:'done'},{});
 if(api==='v1'){
  await hooks.event?.({event:{type:'session.idle',properties:{sessionID:'native-session'}}});
  await hooks.event?.({event:{type:'session.deleted',properties:{info:{id:'native-session'}}}});
 }
}
process.env.KATA_REF='spoke-project#other';
if(api==='v2'){await start('other-session');await prompt('other-session');}
else if(api==='v1'){const other=await user.KataPlugin({directory:otherWorkspace,client:{session:{get:async input=>{assert.equal(input.query.directory,otherWorkspace);return {data:await sessionInfo(input.path.id)}}}}});await other['chat.message']?.({sessionID:'other-session'},{});const system=['other authored'];await other['experimental.chat.system.transform']?.({sessionID:'other-session'},{system});assert.equal(system.length,2,'separate v1 workspace receives user contract');}
else {await start('other-session');await prompt('other-session');}
if(cleanup)await cleanup();
const calls=(await fs.readFile(process.env.KATA_PLUGIN_LOG,'utf8')).trim().split('\n').map(JSON.parse);
const starts=calls.filter(c=>c.args.includes('attention-native'));assert.equal(starts.length,2);assert.ok(starts.every(c=>c.args.includes('start')&&!c.args.includes('end')));
for(const [index,c] of starts.entries()){const nativeWorkspace=index===1&&api!=='amp'?otherWorkspace:workspace;assert.equal(c.cwd,nativeWorkspace);assert.equal(c.args[c.args.indexOf('--workspace')+1],nativeWorkspace);assert.equal(c.args[c.args.indexOf('--host-pid')+1],String(api==='amp'?process.ppid:process.pid));assert.ok(c.args.includes('--ref'));}
assert.equal(starts[0].args[starts[0].args.indexOf('--ref')+1],'spoke-project#abc4');
for(const c of calls.filter(c=>c.args.includes('agent-contract-hook')||c.args.includes('inbox'))){assert.ok([workspace,otherWorkspace].includes(c.args[c.args.indexOf('--workspace')+1]));assert.equal(c.server,'http://127.0.0.1:7777');if(c.args.includes('inbox'))assert.equal(c.args[c.args.indexOf('--for')+1],'actor/worker');}
assert.equal(await fs.stat(path.join(workspace,'bad')).then(()=>true,()=>false),false);
console.log('native plugin fixture passed');
