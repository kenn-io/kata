import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

const [extension, cwd, ref] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extension))).default;
const handlers = new Map();
const calls = [];
if (ref) process.env.KATA_REF = ref;
else delete process.env.KATA_REF;
delete process.env.KATA_ATTENTION_HOST;

const pi = {
  on(name, handler) {
    const list = handlers.get(name) || [];
    list.push(handler);
    handlers.set(name, list);
  },
  async exec(_executable, args) {
    calls.push(args);
    return {code: 1, killed: false, stdout: '', stderr: 'must not run'};
  },
};
factory(pi);
const ctx = {cwd, sessionManager: {getSessionId: () => 'session-A'}};
for (const handler of handlers.get('session_start') || []) await handler({reason: 'startup'}, ctx);
assert.deepEqual(calls, [], 'unsupported captured refs never start attention');
