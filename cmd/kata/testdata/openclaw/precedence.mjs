import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
const [userPath,projectPath,workspace]=process.argv.slice(2),root=process.env.OPENCLAW_TEST_ROOT;
fs.writeFileSync(path.join(root,'contract'),'live');fs.writeFileSync(path.join(root,'inbox'),'inbox');
const user=(await import(pathToFileURL(userPath))).default,project=(await import(pathToFileURL(projectPath))).default;
assert.notEqual(user.id,project.id);
const config={plugins:{entries:{[user.id]:{enabled:true,hooks:{allowConversationAccess:true}},[project.id]:{enabled:true,hooks:{allowConversationAccess:true}}}}};
function capture(plugin){let dispose;const handlers=new Map();const api={config,on:(n,f)=>handlers.set(n,f),lifecycle:{onDispose(fn){dispose=fn}}};plugin.register(api);return {handlers,dispose:()=>dispose()}}
const u=capture(user),p=capture(project);
const ctx={agentId:'example-agent',sessionId:'one',sessionKey:'agent:example-agent:main',workspaceDir:workspace,hookInvocation:{assertActive(){}}};
await p.handlers.get('session_start')({sessionId:'one',sessionKey:ctx.sessionKey},{sessionId:'one',sessionKey:ctx.sessionKey,agentId:ctx.agentId});
process.env.KATA_REF='later-ref';
const userPrompt=await u.handlers.get('before_prompt_build')({},ctx),projectPrompt=await p.handlers.get('before_prompt_build')({},ctx);
if(process.env.OPENCLAW_TEST_ATTENTION_ONLY==='true'){assert.equal(userPrompt.prependSystemContext,'contract:live');assert.equal(projectPrompt,undefined)}else{assert.equal(userPrompt,undefined);assert.equal(projectPrompt.prependSystemContext,'contract:live')}
let calls=()=>fs.readFileSync(path.join(root,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
assert.equal(calls().filter(c=>c.args[3]==='start').length,1);assert.equal(calls().find(c=>c.args[3]==='start').args[9],'launch-ref');
// Denied project registration must not suppress authorized user callbacks.
config.plugins.entries[project.id].hooks.allowConversationAccess=false;
assert.equal((await u.handlers.get('before_prompt_build')({},ctx)).prependSystemContext,'contract:live');
assert.equal(calls().filter(c=>c.args[3]==='start').length,1);
assert.equal(await p.handlers.get('before_prompt_build')({},ctx),undefined);
p.dispose();
await u.handlers.get('session_end')({sessionId:'one',sessionKey:ctx.sessionKey,reason:'shutdown'},{sessionId:'one',sessionKey:ctx.sessionKey});
assert.equal(calls().filter(c=>c.args[3]==='end').length,1);assert.equal(calls().find(c=>c.args[3]==='end').args[9],'launch-ref');
console.log('authorized per-workspace precedence passed');
