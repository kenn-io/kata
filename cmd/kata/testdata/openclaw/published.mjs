// Published OpenClaw 2026.9.7 loader, typed runner and native payload builders.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
const [runtime,configPath,workspace]=process.argv.slice(2),dist=path.join(runtime,'dist');
async function published(prefix,name){for(const file of fs.readdirSync(dist).filter(n=>n.startsWith(prefix)&&n.endsWith('.mjs'))){const exports=await import(pathToFileURL(path.join(dist,file)));const fn=Object.values(exports).find(v=>typeof v==='function'&&v.name===name);if(fn)return fn}assert.fail(prefix+' '+name)}
const loader=await published('loader-runtime-load-','loadOpenClawPlugins'),hooks=await published('hooks-','createHookRunner'),startBuilder=await published('active-sessions-shutdown-tracker-','buildSessionStartHookPayload'),endBuilder=await published('active-sessions-shutdown-tracker-','buildSessionEndHookPayload');
const config=JSON.parse(fs.readFileSync(configPath,'utf8'));
const registry=loader({config,workspaceDir:workspace,logger:{info(){},warn(){},error(){},debug(){}}});
const owned=registry.plugins.find(p=>p.id==='kata-hooks-user');assert.ok(owned);assert.equal(owned.status,'loaded',JSON.stringify(owned));
assert.deepEqual(registry.typedHooks.filter(h=>h.pluginId==='kata-hooks-user').map(h=>h.hookName).sort(),['before_prompt_build','session_end','session_start','subagent_progress','subagent_spawned']);
const runner=hooks(registry);
const start=startBuilder({sessionId:'native-one',sessionKey:'agent:example-agent:main',agentId:'example-agent'});
assert.equal(start.context.workspaceDir,undefined);await runner.runSessionStart(start.event,start.context);
const ctx={...start.context,workspaceDir:workspace};
const prompt=await runner.runBeforePromptBuild({prompt:'image prompt',messages:[]},ctx);
assert.equal(prompt.prependSystemContext,'contract:native-contract');assert.equal(prompt.prependContext,'native-request');
for(const reason of ['reset','new','shutdown']){
 const end=endBuilder({sessionId:'native-one',sessionKey:start.event.sessionKey,agentId:'example-agent',reason});await runner.runSessionEnd(end.event,end.context);
}
const calls=fs.readFileSync(path.join(process.env.OPENCLAW_TEST_ROOT,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
assert.equal(calls.filter(c=>c.args[3]==='start').length,1);assert.equal(calls.filter(c=>c.args[3]==='end').length,1);
console.log('published 2026.9.7 loader, typed prompt runner and native lifecycle payloads passed');
