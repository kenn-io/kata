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
await registrations.get('session_start')({sessionId:progressChild.sessionId,sessionKey:progressChild.sessionKey},{...progressChild});
const progressPrompt=await registrations.get('before_prompt_build')({},progressChild);
assert.equal(progressPrompt.prependSystemContext,'contract:shared contract');
await registrations.get('subagent_progress')?.(
 {phase:'started',runId:'progress-run',childSessionKey:progressChild.sessionKey},
 {runId:'progress-run',childSessionKey:progressChild.sessionKey,requesterSessionKey:parent.sessionKey},
);

const spawnedChild={agentId:'worker',sessionId:'spawned-child-session',sessionKey:'agent:worker:subagent:spawned-run',workspaceDir:workspace};
await registrations.get('session_start')({sessionId:spawnedChild.sessionId,sessionKey:spawnedChild.sessionKey},{...spawnedChild});
const spawnedPrompt=await registrations.get('before_prompt_build')({},spawnedChild);
assert.equal(spawnedPrompt.prependSystemContext,'contract:shared contract');
fs.writeFileSync(path.join(root,'slow-parent-start'),'');
const reconciliation=registrations.get('subagent_spawned')?.(
 {runId:'spawned-run',childSessionKey:spawnedChild.sessionKey,agentId:'worker',mode:'run',threadRequested:false},
 {runId:'spawned-run',childSessionKey:spawnedChild.sessionKey,requesterSessionKey:parent.sessionKey},
);
async function waitForParentStarts(count) {
 for (let attempt=0;attempt<100;attempt++) {
  const calls=fs.readFileSync(path.join(root,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
  if(calls.filter(call=>call.args[0]==='agent-hook'&&call.args[1]==='attention-native'&&call.args[3]==='start'&&call.args[5]==='parent-session').length>=count)return;
  await new Promise(resolve=>setTimeout(resolve,10));
 }
 assert.fail('timed out waiting for parent attention reassertion');
}
await waitForParentStarts(3);
const parentEnd=registrations.get('session_end')(
 {sessionId:parent.sessionId,sessionKey:parent.sessionKey,messageCount:1,reason:'shutdown'},
 {...parent},
);
await Promise.all([reconciliation,parentEnd]);

for (const child of [progressChild,spawnedChild]) {
 await registrations.get('session_end')(
  {sessionId:child.sessionId,sessionKey:child.sessionKey,messageCount:1,reason:'shutdown'},
  {...child},
 );
}

const retryParent={agentId:'main',sessionId:'retry-parent-session',sessionKey:'agent:main:retry',workspaceDir:workspace};
await registrations.get('session_start')({sessionId:retryParent.sessionId,sessionKey:retryParent.sessionKey},{...retryParent});
await registrations.get('before_prompt_build')({},retryParent);
const failedChild={agentId:'worker',sessionId:'failed-child-session',sessionKey:'agent:worker:subagent:failed-run',workspaceDir:workspace};
await registrations.get('session_start')({sessionId:failedChild.sessionId,sessionKey:failedChild.sessionKey},{...failedChild});
await registrations.get('before_prompt_build')({},failedChild);
fs.writeFileSync(path.join(root,'fail-parent-start'),'');
await registrations.get('subagent_spawned')?.(
 {runId:'failed-run',childSessionKey:failedChild.sessionKey,agentId:'worker',mode:'run',threadRequested:false},
 {runId:'failed-run',childSessionKey:failedChild.sessionKey,requesterSessionKey:retryParent.sessionKey},
);
await registrations.get('session_end')(
 {sessionId:retryParent.sessionId,sessionKey:retryParent.sessionKey,messageCount:1,reason:'shutdown'},
 {...retryParent},
);
await registrations.get('session_end')(
 {sessionId:failedChild.sessionId,sessionKey:failedChild.sessionKey,messageCount:1,reason:'shutdown'},
 {...failedChild},
);

const calls=fs.readFileSync(path.join(root,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
const attention=calls.filter(call=>call.args[0]==='agent-hook'&&call.args[1]==='attention-native');
assert.deepEqual(attention.map(call=>call.args.slice(3,6)),[
 ['start','--session','parent-session'],
 ['start','--session','progress-child-session'],
 ['start','--session','parent-session'],
 ['start','--session','spawned-child-session'],
 ['start','--session','parent-session'],
 ['end','--session','parent-session'],
 ['start','--session','retry-parent-session'],
 ['start','--session','failed-child-session'],
 ['start','--session','retry-parent-session'],
 ['end','--session','retry-parent-session'],
 ['end','--session','failed-child-session'],
]);
const completed=fs.readFileSync(path.join(root,'attention-completions.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
const parentCompletions=completed.filter(call=>call.args[0]==='agent-hook'&&call.args[1]==='attention-native'&&call.args[5]==='parent-session').map(call=>call.args[3]);
assert.deepEqual(parentCompletions.slice(-2),['start','end']);
console.log('subagent attention isolation passed');
