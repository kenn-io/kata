import assert from 'node:assert/strict';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const [extension, cwd, mode, failure] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extension))).default;
const handlers = new Map(), calls = [];
let session = 'session-A', pending;
const result = {code: 0, killed: false, stdout: '', stderr: ''};
const pi = {
  on(name, handler) {
    const list = handlers.get(name) || [];
    list.push(handler); handlers.set(name, list);
  },
  async exec(executable, args, options) {
    calls.push({args, options});
    if (pending) return await pending.promise;
    return result;
  },
};
const context = (id = session, directory = cwd) => ({cwd: directory, sessionManager: {getSessionId: () => id}});
const snapshot = name => [...(handlers.get(name) || [])];
const emit = async (name, event, ctx = context()) => {
  for (const fn of snapshot(name)) await fn(event, ctx);
};
function defer() {
  let resolve, reject;
  const promise = new Promise((yes, no) => {resolve = yes; reject = no});
  return {promise, resolve, reject};
}
function fail(operation) {
  if (failure === 'reject') operation.reject(new Error('command unavailable'));
  else operation.resolve({...result, code: failure === 'nonzero' ? 1 : 0, killed: failure === 'killed'});
}
function assertRoute(call, id, ref) {
  assert.deepEqual(call.args, ['agent-hook', 'attention-native', 'pi', call.args[3],
    '--session', id, '--host-pid', String(process.pid), '--ref', ref, '--workspace', path.resolve(cwd)]);
  assert.equal(call.options.cwd, path.resolve(cwd));
  assert.equal(call.options.timeout, 10000);
}
factory(pi);
process.env.KATA_REF = 'captured-ref';
if (mode === 'end') await emit('session_start', {reason: 'startup'});
pending = defer();
const operation = pending;
const name = mode === 'start' ? 'session_start' : 'session_shutdown';
const event = {reason: mode === 'start' ? 'resume' : 'quit'};
const before = calls.length;
const first = emit(name, event), duplicate = emit(name, event);
await Promise.resolve();
assert.equal(calls.length, before + 1, 'concurrent lifecycle callbacks must share one command');
fail(operation); pending = undefined;
await Promise.all([first, duplicate]);
process.env.KATA_REF = 'later-ref';
await emit(name, event);
assert.equal(calls.length, before + 2, `failed ${mode} must retry after ${failure}`);
assertRoute(calls.at(-1), 'session-A', 'captured-ref');
await emit(name, event);
assert.equal(calls.length, before + 2, `successful ${mode} duplicate must be idempotent`);
if (mode === 'start') {
  await emit('agent_end', {});
  assert.equal(calls.length, before + 2, 'agent turn end cannot end native session attention');
  await emit('session_shutdown', {reason: 'quit'});
  assert.equal(calls.at(-1).args[3], 'end');
  assertRoute(calls.at(-1), 'session-A', 'captured-ref');
}

// A completed callback from a retired registration must not change its newer
// replacement's state; both start and end commands may still be in flight.
for (const staleMode of ['start', 'end']) {
  session = 'old-' + staleMode; factory(pi);
  process.env.KATA_REF = 'old-ref';
  if (staleMode === 'end') await emit('session_start', {reason: 'new'});
  pending = defer(); const staleOperation = pending;
  const oldContext = context();
  const stale = emit(staleMode === 'start' ? 'session_start' : 'session_shutdown',
    {reason: staleMode === 'start' ? 'new' : 'quit'}, oldContext);
  await new Promise(setImmediate);
  const retiring = snapshot('session_shutdown');
  await emit('session_shutdown', {reason: 'reload'}, oldContext);
  session = 'new-' + staleMode; factory(pi);
  process.env.KATA_REF = 'replacement-ref'; pending = undefined;
  await emit('session_start', {reason: 'resume'});
  staleOperation.resolve(result); await stale;
  const count = calls.length;
  for (const callback of retiring) await callback({reason: 'quit'}, oldContext);
  await emit('session_start', {reason: 'resume'});
  assert.equal(calls.length, count, 'stale callbacks cannot end or restart the replacement');
  process.env.KATA_REF = 'unrelated-ref';
  await emit('session_shutdown', {reason: 'quit'});
  assert.equal(calls.length, count + 1);
  assertRoute(calls.at(-1), session, 'replacement-ref');
}

// A quit arriving during start waits for that command. Concurrent quit events
// still issue one end, preserving start-before-end ordering and captured route.
session = 'joined-session'; factory(pi); process.env.KATA_REF = 'joined-ref';
pending = defer(); const starting = pending;
const start = emit('session_start', {reason: 'new'});
await new Promise(setImmediate);
const quit = emit('session_shutdown', {reason: 'quit'});
const quitDuplicate = emit('session_shutdown', {reason: 'quit'});
const joining = calls.length;
pending = undefined; starting.resolve(result);
await Promise.all([start, quit, quitDuplicate]);
assert.equal(calls.length, joining + 1, 'quit callbacks join pending start and end once');
assert.equal(calls.at(-1).args[3], 'end');
assertRoute(calls.at(-1), session, 'joined-ref');
console.log('Pi attention retry and in-flight fencing passed');
