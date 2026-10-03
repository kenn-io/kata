import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
const [plugin,workspace]=process.argv.slice(2),root=process.env.OPENCLAW_TEST_ROOT;
const initialContract=fs.readFileSync(path.join(root,'contract'),'utf8');fs.writeFileSync(path.join(root,'inbox'),'request-one');
// Windows Node exposes synthetic POSIX modes rather than filesystem ACLs.
if(process.env.OPENCLAW_TEST_PLATFORM==='windows-mode') {
 Object.defineProperty(process,'platform',{value:'win32'});
 process.getuid=undefined;
 const stat=fs.lstatSync;
 fs.lstatSync=(...args)=>{const result=stat(...args);result.mode|=0o077;return result};
}
const registrations=new Map(),services=[];
const api={config:{plugins:{entries:{'kata-hooks-user':{enabled:true,hooks:{allowConversationAccess:true}}}}},on:(name,handler)=>registrations.set(name,handler),registerService:service=>services.push(service)};
const mod=await import(pathToFileURL(plugin));mod.default.register(api);
assert.equal(registrations.has('agent_end'),false);
let checks=0;const ctx={agentId:'example-agent',sessionId:'one',sessionKey:'agent:example-agent:main',workspaceDir:workspace,hookInvocation:{assertActive(){checks++}}};
await registrations.get('session_start')({sessionId:'one',sessionKey:ctx.sessionKey},{sessionId:'one',sessionKey:ctx.sessionKey,agentId:ctx.agentId});
const prompt=await registrations.get('before_prompt_build')({prompt:'',messages:[{role:'user',content:[{type:'image'}]}]},ctx);
assert.equal(prompt.prependSystemContext,'contract:'+initialContract);assert.equal(prompt.prependContext,'request-one');assert.ok(checks>=2);
let calls=()=>fs.readFileSync(path.join(root,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
const start=calls().find(c=>c.args[0]==='agent-hook');assert.deepEqual(start.args,['agent-hook','attention-native','openclaw','start','--session','one','--host-pid',String(process.pid),'--ref','abcd','--workspace',workspace]);
assert.equal(start.cwd,workspace);assert.equal(start.recipient,'actor/worker');
assert.deepEqual(calls().find(c=>c.args[0]==='agent-contract-hook').args,['agent-contract-hook','--source','prompt with spaces;literal.txt']);
assert.deepEqual(calls().find(c=>c.args[0]==='inbox').args,['inbox','--for','actor/worker','--context']);
process.env.KATA_REF='changed';fs.writeFileSync(path.join(root,'contract'),'second');fs.writeFileSync(path.join(root,'inbox'),'');
let second=await registrations.get('before_prompt_build')({prompt:'next',messages:[]},ctx);assert.equal(second.prependSystemContext,'contract:second');assert.equal(second.prependContext,'');
assert.equal(calls().filter(c=>c.args[3]==='start').length,1);
const unavailable='Kata agent contract is unavailable. Stop before making project changes and tell the user to check kata agent-hook status openclaw.';
fs.writeFileSync(path.join(root,'inbox'),'request-fallback');fs.writeFileSync(path.join(root,'fail-contract'),'');
const failedContract=await registrations.get('before_prompt_build')({prompt:'failed contract',messages:[]},ctx);assert.equal(failedContract.prependSystemContext,unavailable);assert.equal(failedContract.prependContext,'request-fallback');
fs.rmSync(path.join(root,'fail-contract'));fs.writeFileSync(path.join(root,'no-contract-context'),'');
const missingContext=await registrations.get('before_prompt_build')({prompt:'missing contract context',messages:[]},ctx);assert.equal(missingContext.prependSystemContext,unavailable);assert.equal(missingContext.prependContext,'request-fallback');
fs.rmSync(path.join(root,'no-contract-context'));
// Reload must retain ownership without overwriting a handoff baseline.
await services[0].stop({});
const host=globalThis[Symbol.for('kata.openclaw.hooks.v1')];host.sessions.clear();
const reloaded=await import(pathToFileURL(plugin).href+'?reload=1');reloaded.default.register(api);await registrations.get('before_prompt_build')({prompt:'reload',messages:[]},ctx);
assert.equal(calls().filter(c=>c.args[3]==='start').length,1);
await registrations.get('session_end')({sessionId:'one',sessionKey:ctx.sessionKey,reason:'reset'},{sessionId:'one',sessionKey:ctx.sessionKey,agentId:ctx.agentId});
assert.equal(calls().filter(c=>c.args[3]==='end').length,1);assert.equal(calls().find(c=>c.args[3]==='end').args[9],'abcd');
// A late end for an older session must not terminate its replacement.
ctx.sessionId='two';await registrations.get('before_prompt_build')({},ctx);ctx.sessionId='three';await registrations.get('before_prompt_build')({},ctx);
await registrations.get('session_end')({sessionId:'two',sessionKey:ctx.sessionKey,reason:'new'},{sessionId:'two',sessionKey:ctx.sessionKey});
assert.equal(calls().filter(c=>c.args[3]==='end').length,1);
fs.writeFileSync(path.join(root,'fail-inbox'),'');const failure=await registrations.get('before_prompt_build')({},ctx);assert.match(failure.prependContext,/unavailable/i);assert.equal(failure.prependSystemContext,'contract:second');
// Cancellation after an awaited read yields no stale prompt contribution.
ctx.hookInvocation.assertActive=()=>{throw Error('stale')};assert.equal(await registrations.get('before_prompt_build')({},ctx),undefined);
console.log('native generated plugin behavior passed');
