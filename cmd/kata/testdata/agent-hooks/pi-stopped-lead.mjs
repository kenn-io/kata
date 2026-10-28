import assert from 'node:assert/strict';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const [extension, cwd] = process.argv.slice(2);
const calls = [];
let running = false;
delete process.env.KATA_ATTENTION_HOST;
process.env.KATA_REF = 'lead-ref';
const handlers = new Map();
(await import(pathToFileURL(extension))).default({
  on(name, fn) { handlers.set(name, [...(handlers.get(name) || []), fn]); },
  async exec(_executable, args) {
    calls.push(args[3] === 'daemon' ? 'locate' : args[3]);
    if (args[3] === 'daemon') {
      return running ? {code: 0, killed: false, stdout: JSON.stringify({network: 'unix', address: 'unix:///tmp/kata.sock'}), stderr: ''} :
        {code: 1, killed: false, stdout: '', stderr: 'daemon is not running'};
    }
    // The attention start brings up the stopped local daemon.
    running = true;
    return {code: 0, killed: false, stdout: '', stderr: ''};
  },
});
const ctx = {cwd, sessionManager: {getSessionId: () => 'lead-session'}};
for (const fn of handlers.get('session_start') || []) await fn({reason: 'startup'}, ctx);
assert.deepEqual(calls, ['locate', 'start', 'locate']);
assert.equal(process.env.KATA_ATTENTION_HOST, JSON.stringify({pid: process.pid,
  identity: JSON.stringify({ref: 'lead-ref', daemon: 'unix:///tmp/kata.sock', cwd: path.resolve(cwd)})}),
  'the lead claims its marker once the daemon it started can be located');
