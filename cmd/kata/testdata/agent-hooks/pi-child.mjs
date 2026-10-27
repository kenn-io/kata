import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

const [extension, cwd, leadRef, childRef, leadServer, childServer] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extension))).default;
const registryKey = Symbol.for('kata.pi.native.v1');
const calls = [];
function host(adapter, pid, ref, server) {
  Object.defineProperty(process, 'pid', {value: pid, configurable: true});
  process.env.KATA_REF = ref;
  process.env.KATA_SERVER = server;
  const handlers = new Map();
  adapter({
    on(name, fn) {
      const list = handlers.get(name) || [];
      list.push(fn); handlers.set(name, list);
    },
    async exec(_executable, args) {
      calls.push([pid, args[3], args[args.indexOf('--ref') + 1]]);
      return {code: 0, killed: false, stdout: '', stderr: ''};
    },
  });
  const ctx = {cwd, sessionManager: {getSessionId: () => 'session-' + pid}};
  return async (name, reason) => {
    for (const fn of handlers.get(name) || []) await fn({reason}, ctx);
  };
}
delete process.env.KATA_ATTENTION_HOST;
process.kill = () => true;
const lead = host(factory, 101, leadRef, leadServer);
await lead('session_start', 'startup');
const inherited = process.env.KATA_ATTENTION_HOST;
assert.equal(process.env.KATA_REF, leadRef);
// A child inherits the environment, but has its own module registry.
const leadRegistry = globalThis[registryKey];
delete globalThis[registryKey];
const childFactory = (await import(pathToFileURL(extension) + '?child')).default;
const child = host(childFactory, 202, childRef, childServer);
await child('session_start', 'startup');
await child('session_shutdown', 'quit');
assert.equal(process.env.KATA_REF, childRef, 'child tracking preserves its issue ref');
const sameIssue = leadServer === childServer && leadRef === childRef;
const leadIdentity = JSON.stringify({ref: leadRef, server: leadServer});
assert.deepEqual(calls, sameIssue ? [[101, 'start', leadRef]] :
  [[101, 'start', leadRef], [202, 'start', childRef], [202, 'end', childRef]]);
assert.deepEqual(JSON.parse(inherited), {pid: 101, identity: leadIdentity});
if (sameIssue) assert.equal(process.env.KATA_ATTENTION_HOST, inherited);
else assert.equal(process.env.KATA_ATTENTION_HOST, undefined);
// Restore the lead's private environment and verify its eventual end clears it.
globalThis[registryKey] = leadRegistry;
process.env.KATA_ATTENTION_HOST = inherited;
Object.defineProperty(process, 'pid', {value: 101, configurable: true});
await lead('session_shutdown', 'quit');
assert.deepEqual(calls.at(-1), [101, 'end', leadRef]);
assert.equal(process.env.KATA_ATTENTION_HOST, undefined);
for (const replacement of [JSON.stringify({pid: 303, identity: leadIdentity}),
  JSON.stringify({pid: 101, identity: 'replacement-identity'}), '{']) {
  delete process.env.KATA_ATTENTION_HOST;
  const restarted = host(factory, 101, leadRef, leadServer);
  await restarted('session_start', 'new');
  process.env.KATA_ATTENTION_HOST = replacement;
  await restarted('session_shutdown', 'quit');
  assert.equal(process.env.KATA_ATTENTION_HOST, replacement, 'end preserves a replacement host marker');
}
