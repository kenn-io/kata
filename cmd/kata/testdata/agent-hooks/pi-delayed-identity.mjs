import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

const [extension, cwd] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extension))).default;
const handlers = new Map();
const attentionCalls = [];
let session = 'session-A';
let releaseA;
const delayedA = new Promise(resolve => { releaseA = resolve; });
let releaseB;
let delayIssueB = false;
process.env.KATA_REF = 'issue-A';
delete process.env.KATA_ATTENTION_HOST;
process.kill = () => true;

const pi = {
  on(name, handler) {
    const list = handlers.get(name) || [];
    list.push(handler);
    handlers.set(name, list);
  },
  async exec(_executable, args) {
    if (args[0] === '--json' && args[3] === 'daemon') {
      return {code: 0, killed: false, stdout: JSON.stringify({
        network: 'tcp', address: 'https://daemon.example', request_base_url: 'https://daemon.example',
      }), stderr: ''};
    }
    if (args[0] === '--json' && args[3] === 'show') {
      if (args[4] === 'issue-A') return delayedA;
      if (args[4] === 'issue-B' && delayIssueB) {
        return new Promise(resolve => { releaseB = resolve; });
      }
      return {code: 0, killed: false, stdout: JSON.stringify({issue: {
        project_id: 42, uid: `spoke-project:${args[4]}`,
      }}), stderr: ''};
    }
    attentionCalls.push([args[3], args[args.indexOf('--ref') + 1]]);
    return {code: 0, killed: false, stdout: '', stderr: ''};
  },
};
factory(pi);
const ctx = {cwd, sessionManager: {getSessionId: () => session}};
async function emit(name, reason) {
  for (const handler of handlers.get(name) || []) await handler({reason}, ctx);
}

const retired = emit('session_start', 'startup');
await new Promise(setImmediate);
session = 'session-B';
process.env.KATA_REF = 'issue-B';
await emit('session_start', 'new');
assert.deepEqual(attentionCalls, [['start', 'issue-B']], 'the replacement session starts while the old identity lookup is pending');
const replacementMarker = process.env.KATA_ATTENTION_HOST;
releaseA({code: 0, killed: false, stdout: JSON.stringify({issue: {
  project_id: 42, uid: 'spoke-project:issue-A',
}}), stderr: ''});
await retired;
assert.deepEqual(attentionCalls, [['start', 'issue-B']], 'a retired lookup cannot start attention after replacement');
assert.equal(process.env.KATA_ATTENTION_HOST, replacementMarker, 'a retired lookup cannot replace the new marker');

const registry = globalThis[Symbol.for('kata.pi.native.v1')];
const baselineB = registry.active.get(cwd);
assert.equal(baselineB?.started, true, 'replacement baseline is started before reload');
delete baselineB.identity;
delete process.env.KATA_ATTENTION_HOST;
delayIssueB = true;
const retiredReload = emit('session_start', 'reload');
await new Promise(setImmediate);
assert.equal(typeof releaseB, 'function', 'marker restoration waits for issue identity lookup');
session = 'session-C';
process.env.KATA_REF = 'issue-C';
await emit('session_start', 'new');
const replacementMarkerC = process.env.KATA_ATTENTION_HOST;
assert.deepEqual(attentionCalls, [['start', 'issue-B'], ['start', 'issue-C']]);
releaseB({code: 0, killed: false, stdout: JSON.stringify({issue: {
  project_id: 42, uid: 'spoke-project:issue-B',
}}), stderr: ''});
await retiredReload;
assert.deepEqual(attentionCalls, [['start', 'issue-B'], ['start', 'issue-C']], 'a retired reload never repeats the attention write');
assert.equal(process.env.KATA_ATTENTION_HOST, replacementMarkerC, 'a retired reload cannot replace the newer host marker');
