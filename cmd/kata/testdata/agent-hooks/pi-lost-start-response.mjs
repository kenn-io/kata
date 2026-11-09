import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

const [extension, cwd] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extension))).default;
const handlers = new Map();
const attentionCalls = [];
let serverOwner;
const session = 'lost-response-session';
const ref = 'abc4';
delete process.env.KATA_ATTENTION_HOST;
process.env.KATA_REF = ref;

const pi = {
  on(name, handler) {
    const list = handlers.get(name) || [];
    list.push(handler);
    handlers.set(name, list);
  },
  async exec(_executable, args) {
    if (args[3] === 'daemon') {
      return {code: 0, killed: false, stdout: JSON.stringify({network: 'unix', address: 'unix:///tmp/kata.sock'}), stderr: ''};
    }
    attentionCalls.push(args);
    const identity = {
      session: args[5],
      hostPID: args[7],
      ref: args[9],
      workspace: args[11],
    };
    if (args[3] === 'start') {
      // The daemon commits the owner, then the local CLI loses its response.
      serverOwner = identity;
      return {code: 1, killed: true, stdout: '', stderr: 'response lost after commit'};
    }
    if (args[3] === 'end' && JSON.stringify(identity) === JSON.stringify(serverOwner)) {
      serverOwner = undefined;
    }
    return {code: 0, killed: false, stdout: '', stderr: ''};
  },
};

factory(pi);
const context = {cwd, sessionManager: {getSessionId: () => session}};
async function emit(name, event) {
  for (const handler of handlers.get(name) || []) await handler(event, context);
}

await emit('session_start', {reason: 'startup'});
assert.deepEqual(serverOwner, {
  session,
  hostPID: String(process.pid),
  ref,
  workspace: cwd,
}, 'the daemon committed the attention owner before the response was lost');
assert.equal(process.env.KATA_ATTENTION_HOST, undefined, 'a lost response cannot claim the local marker');

await emit('session_shutdown', {reason: 'quit'});
assert.deepEqual(attentionCalls.map(args => args[3]), ['start', 'end'],
  'graceful shutdown attempts owner cleanup after an ambiguous start');
assert.deepEqual(attentionCalls[1].slice(4), attentionCalls[0].slice(4),
  'cleanup uses the same session, host process, issue, and workspace');
assert.equal(serverOwner, undefined, 'owner-checked cleanup clears the committed start');
console.log('Pi attention cleaned up an owner after the start response was lost');
