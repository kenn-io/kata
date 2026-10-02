import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const [plugin,workspace]=process.argv.slice(2),root=process.env.OPENCLAW_TEST_ROOT;
fs.writeFileSync(path.join(root,'contract'),'shared contract');
const registrations=new Map(),services=[];
const api={config:{plugins:{entries:{'kata-hooks-user':{enabled:true,hooks:{allowConversationAccess:true}}}}},on:(name,handler)=>registrations.set(name,handler),registerService:service=>services.push(service)};
const mod=await import(pathToFileURL(plugin));mod.default.register(api);

const parent={agentId:'main',sessionId:'parent-session',sessionKey:'agent:main:main',workspaceDir:workspace};
await registrations.get('session_start')({sessionId:parent.sessionId,sessionKey:parent.sessionKey},{...parent});
const parentPrompt=await registrations.get('before_prompt_build')({},parent);
assert.equal(parentPrompt.prependSystemContext,'contract:shared contract');

const progressChild={agentId:'worker',sessionId:'progress-child-session',sessionKey:'agent:worker:subagent:progress-run',workspaceDir:workspace};
await registrations.get('subagent_progress')?.(
 {phase:'started',runId:'progress-run',childSessionKey:progressChild.sessionKey},
 {runId:'progress-run',childSessionKey:progressChild.sessionKey,requesterSessionKey:parent.sessionKey},
);
await registrations.get('session_start')({sessionId:progressChild.sessionId,sessionKey:progressChild.sessionKey},{...progressChild});
const progressPrompt=await registrations.get('before_prompt_build')({},progressChild);
assert.equal(progressPrompt.prependSystemContext,'contract:shared contract');

const spawnedChild={agentId:'worker',sessionId:'spawned-child-session',sessionKey:'agent:worker:subagent:spawned-run',workspaceDir:workspace};
await registrations.get('subagent_spawned')?.(
 {runId:'spawned-run',childSessionKey:spawnedChild.sessionKey,agentId:'worker',mode:'run',threadRequested:false},
 {runId:'spawned-run',childSessionKey:spawnedChild.sessionKey,requesterSessionKey:parent.sessionKey},
);
await registrations.get('session_start')({sessionId:spawnedChild.sessionId,sessionKey:spawnedChild.sessionKey},{...spawnedChild});
const spawnedPrompt=await registrations.get('before_prompt_build')({},spawnedChild);
assert.equal(spawnedPrompt.prependSystemContext,'contract:shared contract');

for (const child of [progressChild,spawnedChild]) {
 await registrations.get('session_end')(
  {sessionId:child.sessionId,sessionKey:child.sessionKey,messageCount:1,reason:'shutdown'},
  {...child},
 );
}
await registrations.get('session_end')(
 {sessionId:parent.sessionId,sessionKey:parent.sessionKey,messageCount:1,reason:'shutdown'},
 {...parent},
);

const calls=fs.readFileSync(path.join(root,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
const attention=calls.filter(call=>call.args[0]==='agent-hooks'&&call.args[1]==='attention-native');
assert.deepEqual(attention.map(call=>call.args.slice(3,6)),[
 ['start','--session','parent-session'],
 ['end','--session','parent-session'],
]);
console.log('subagent attention isolation passed');
