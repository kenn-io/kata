import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
const [userPath,projectPath,workspace,projectExecutable,transition,reason]=process.argv.slice(2),root=process.env.OPENCLAW_TEST_ROOT;
const user=(await import(pathToFileURL(userPath))).default,project=(await import(pathToFileURL(projectPath))).default;
const config={plugins:{entries:{[user.id]:{enabled:true,hooks:{allowConversationAccess:true}},[project.id]:{enabled:true,hooks:{allowConversationAccess:true}}}}};
function capture(plugin){let dispose;const handlers=new Map();plugin.register({config,on:(n,f)=>handlers.set(n,f),lifecycle:{onDispose(fn){dispose=fn}}});return {handlers,dispose:()=>dispose()}}
const u=capture(user);let p=capture(project);
const ctx={agentId:'example-agent',sessionId:'one',sessionKey:'agent:example-agent:main',workspaceDir:workspace,hookInvocation:{assertActive(){}}};
let calls=()=>fs.readFileSync(path.join(root,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
function suspendProject(){if(transition==='dispose')p.dispose();else config.plugins.entries[project.id].hooks.allowConversationAccess=false}
function end(sessionId){const event={sessionId,sessionKey:ctx.sessionKey,reason},nativeContext={sessionId,sessionKey:ctx.sessionKey,agentId:ctx.agentId};return Promise.all([u.handlers.get('session_end')(event,nativeContext),p.handlers.get('session_end')(event,nativeContext)])}
await p.handlers.get('before_prompt_build')({},ctx);assert.equal(calls().filter(c=>c.args[3]==='start').length,1);
const start=calls().find(c=>c.args[3]==='start');assert.equal(start.executable,projectExecutable);
process.env.KATA_REF='changed-ref';suspendProject();
// No new prompt may be required to transfer terminal responsibility.
await end('one');await end('one');
assert.equal(calls().filter(c=>c.args[3]==='start').length,1);assert.equal(calls().filter(c=>c.args[3]==='end').length,1);
const terminal=calls().find(c=>c.args[3]==='end');const expected=[...start.args];expected[3]='end';assert.deepEqual(terminal.args,expected);assert.equal(terminal.executable,projectExecutable);assert.equal(terminal.cwd,workspace);
// Fallback must still fence delayed old-session cleanup after replacement.
config.plugins.entries[project.id].hooks.allowConversationAccess=true;if(transition==='dispose')p=capture(project);
ctx.sessionId='two';await p.handlers.get('before_prompt_build')({},ctx);ctx.sessionId='three';await p.handlers.get('before_prompt_build')({},ctx);
suspendProject();await end('two');assert.equal(calls().filter(c=>c.args[3]==='end').length,1);
await end('three');await end('three');assert.equal(calls().filter(c=>c.args[3]==='end').length,2);assert.equal(calls().filter(c=>c.args[3]==='start').length,3);
console.log('terminal fallback without another prompt passed: '+transition+' '+reason);
