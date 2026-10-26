import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

const [extension, cwd] = process.argv.slice(2);
const originalFactory = (await import(pathToFileURL(extension))).default;
const session = 'lead-session';
const calls = [];
const identity = JSON.stringify({daemon: 'https://daemon.example', project: 42, issue: 'spoke-project:lead-ref'});
const registry = globalThis[Symbol.for('kata.pi.native.v1')];
process.env.KATA_REF = 'lead-ref';
delete process.env.KATA_ATTENTION_HOST;

function register(factory) {
  const handlers = new Map();
  factory({
    on(name, fn) {
      const list = handlers.get(name) || [];
      list.push(fn);
      handlers.set(name, list);
    },
    async exec(_executable, args) {
      if (args[3] === 'daemon') return {code: 0, killed: false, stdout: JSON.stringify({
        network: 'tcp', address: 'https://daemon.example', request_base_url: 'https://daemon.example',
      }), stderr: ''};
      if (args[3] === 'show') return {code: 0, killed: false, stdout: JSON.stringify({issue: {project_id: 42, uid: 'spoke-project:lead-ref'}}), stderr: ''};
      calls.push(args);
      return {code: 0, killed: false, stdout: '', stderr: ''};
    },
  });
  const context = {cwd, sessionManager: {getSessionId: () => session}};
  return async reason => {
    for (const fn of handlers.get('session_start') || []) await fn({reason}, context);
  };
}

await register(originalFactory)('startup');
assert.equal(calls.length, 1, 'the lead starts attention once');
function removeRetainedIdentity() {
  const baseline = registry.active.get(cwd);
  assert.equal(baseline?.started, true, 'the retained lead baseline is started');
  delete baseline.identity;
}

// Older installed adapters retained this started baseline without setting the host marker.
removeRetainedIdentity();
delete process.env.KATA_ATTENTION_HOST;
const upgradedFactory = (await import(pathToFileURL(extension) + '?upgrade')).default;
await register(upgradedFactory)('reload');

assert.equal(
  process.env.KATA_ATTENTION_HOST,
  JSON.stringify({pid: process.pid, identity}),
  'the upgraded extension restores the marker from the retained started baseline',
);
assert.equal(calls.length, 1, 'restoring the host marker does not repeat the attention write');

// A running lead created by the previous extension version has a raw-ref
// marker. Reload upgrades that marker before descendants inherit it.
removeRetainedIdentity();
process.env.KATA_ATTENTION_HOST = JSON.stringify({pid: process.pid, ref: 'lead-ref'});
await register(upgradedFactory)('reload');

assert.equal(
  process.env.KATA_ATTENTION_HOST,
  JSON.stringify({pid: process.pid, identity}),
  'the upgraded extension migrates its retained legacy marker to canonical identity',
);
assert.equal(calls.length, 1, 'migrating the host marker does not repeat the attention write');
console.log('Pi markerless reload restored the host marker without another attention write');
