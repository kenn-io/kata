import assert from 'node:assert/strict';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

// The attention start may start a stopped daemon or replace an older one,
// which can move it to a new address.
const [extension, cwd, before] = process.argv.slice(2);
const after = 'unix:///tmp/replacement.sock';
const calls = [];
let daemon = before;
delete process.env.KATA_ATTENTION_HOST;
process.env.KATA_REF = 'lead-ref';
const handlers = new Map();
(await import(pathToFileURL(extension))).default({
  on(name, fn) { handlers.set(name, [...(handlers.get(name) || []), fn]); },
  async exec(_executable, args) {
    calls.push(args[3] === 'daemon' ? 'locate' : args[3]);
    if (args[3] === 'daemon') {
      return daemon ? {code: 0, killed: false, stdout: JSON.stringify({network: 'unix', address: daemon}), stderr: ''} :
        {code: 1, killed: false, stdout: '', stderr: 'daemon is not running'};
    }
    daemon = after;
    return {code: 0, killed: false, stdout: '', stderr: ''};
  },
});
const ctx = {cwd, sessionManager: {getSessionId: () => 'lead-session'}};
for (const fn of handlers.get('session_start') || []) await fn({reason: 'startup'}, ctx);
assert.deepEqual(calls, ['start', 'locate'], 'a lead without an inherited marker locates its daemon once, after the start');
assert.equal(process.env.KATA_ATTENTION_HOST, JSON.stringify({pid: process.pid,
  identity: JSON.stringify({ref: 'lead-ref', daemon: after, cwd: path.resolve(cwd)})}),
  'the lead claims the daemon its start left running');
