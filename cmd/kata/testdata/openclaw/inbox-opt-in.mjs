import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const [plugin, workspace] = process.argv.slice(2);
const root = process.env.OPENCLAW_TEST_ROOT;
const registrations = new Map();
const api = {
  config: {plugins: {entries: {'kata-hooks-user': {enabled: true, hooks: {allowConversationAccess: true}}}}},
  on: (name, handler) => registrations.set(name, handler),
};
const mod = await import(pathToFileURL(plugin));
mod.default.register(api);
const result = await registrations.get('before_prompt_build')({}, {workspaceDir: workspace});

const calls = fs.readFileSync(path.join(root, 'calls.jsonl'), 'utf8').trim().split('\n').map(JSON.parse);
assert.deepEqual(calls.map(call => call.args[0]), ['agent-contract-hook']);
assert.equal(result.prependSystemContext, 'contract:default-contract');
assert.equal(result.prependContext || '', '');
console.log('OpenClaw inbox remains off without explicit opt-in');
