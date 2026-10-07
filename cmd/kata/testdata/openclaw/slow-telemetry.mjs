import assert from 'node:assert/strict';
import fs from 'node:fs';
import {pathToFileURL} from 'node:url';

const [plugin,workspace,contract]=process.argv.slice(2);
const registrations=new Map();
const api={config:{plugins:{entries:{'kata-hooks-user':{enabled:true,hooks:{allowConversationAccess:true}}}}},on:(name,handler)=>registrations.set(name,handler),registerService(){}};
const mod=await import(pathToFileURL(plugin));mod.default.register(api);
const ctx={agentId:'example-agent',sessionId:'one',sessionKey:'agent:example-agent:main',workspaceDir:workspace,hookInvocation:{assertActive(){}}};
const prompt=await registrations.get('before_prompt_build')({prompt:'work',messages:[]},ctx);
assert.equal(prompt.prependSystemContext,fs.readFileSync(contract,'utf8'));
