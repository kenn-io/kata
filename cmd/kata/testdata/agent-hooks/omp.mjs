import assert from 'node:assert/strict';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
import {createOmpHost} from './omp-host.mjs';
const [extension, cwd, , userPath, nestedPath] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extension))).default;
const host = createOmpHost({cwd});
const attention = h => h.calls.filter(c => c.args[0] === 'agent-hook');
const identity = ref => JSON.stringify({daemon: 'http://127.0.0.1:7777', project: 42, issue: `uid-${ref}`});
const reset = () => {delete process.env.KATA_ATTENTION_HOST; process.env.KATA_REF = 'issue-ref';};
reset(); factory(host);
await host.emit('session_start');
const authored = 'authored entry', todo = '<kata_todo>\nkeep this\n</kata_todo>';
let prompt = await host.emit('before_agent_start', {systemPrompt: [authored, todo, '<kata_contract>old</kata_contract>', '<kata_inbox>stale</kata_inbox>']});
prompt = await host.emit('before_agent_start', prompt);
assert.deepEqual(prompt.systemPrompt.slice(0, 2), [authored, todo]);
assert.equal(prompt.systemPrompt.filter(p => p.startsWith('<kata_contract>')).length, 1);
assert.equal(prompt.systemPrompt.join('').match(/<kata_inbox>/g).length, 1);
assert(prompt.systemPrompt.join('').includes('contract current'));
assert(prompt.systemPrompt.join('').includes('request current'));
assert.deepEqual(host.calls.find(c => c.args[0] === 'agent-contract-hook').args, ['agent-contract-hook', '--source', "contract '$;`.txt"]);
host.setSession('B'); await host.emit('session_switch', {reason: 'new'});
process.env.KATA_REF = 'changed-ref';
await host.emit('session_shutdown'); await host.emit('session_shutdown');
assert.deepEqual(attention(host).map(c => [c.args[3], c.args[5]]), [['start', 'A'], ['end', 'A'], ['start', 'B'], ['end', 'B']]);
for (const {args, options} of attention(host)) {
  assert.deepEqual(args, ['agent-hook', 'attention-native', 'omp', args[3], '--session', args[5], '--host-pid', String(process.pid), '--ref', 'issue-ref', '--workspace', cwd]);
  assert.deepEqual(options, {cwd, timeout: 10000});
}
// Branches, entry forks and btw promotions replace the session ID through
// session_branch, preserving the old route until its attention has ended.
for (const reason of ['branch', 'fork', 'btw']) {
  reset(); const branching = createOmpHost({cwd}); factory(branching);
  await branching.emit('session_start');
  assert.deepEqual(JSON.parse(process.env.KATA_ATTENTION_HOST), {pid: process.pid, identity: identity('issue-ref')});
  process.env.KATA_REF = 'branch-ref'; branching.setSession('B');
  await branching.emit('session_branch', {reason, previousSessionFile: '/example/session-a.jsonl'});
  assert.deepEqual(attention(branching).map(c => [c.args[3], c.args[5]]), [['start', 'A'], ['end', 'A'], ['start', 'B']]);
  assert.deepEqual(JSON.parse(process.env.KATA_ATTENTION_HOST), {pid: process.pid, identity: identity('branch-ref')});
  process.env.KATA_REF = 'changed-ref'; await branching.emit('session_shutdown');
  assert.deepEqual(attention(branching).map(c => [c.args[3], c.args[5], c.args[9], c.args[11], c.options.cwd]),
    [['start', 'A', 'issue-ref', cwd, cwd], ['end', 'A', 'issue-ref', cwd, cwd],
     ['start', 'B', 'branch-ref', cwd, cwd], ['end', 'B', 'branch-ref', cwd, cwd]]);
  assert.equal(process.env.KATA_ATTENTION_HOST, undefined);
}
// Same-process subagents get context without claiming or releasing attention.
reset(); const sub = createOmpHost({cwd, agentKind: 'sub'}); factory(sub);
await sub.emit('session_start'); sub.setSession('child-B'); await sub.emit('session_switch', {reason: 'new'}); sub.setSession('child-C'); await sub.emit('session_branch', {reason: 'fork'}); await sub.emit('session_shutdown');
const subPrompt = await sub.emit('before_agent_start', {systemPrompt: [authored]});
assert(subPrompt.systemPrompt.join('').includes('contract current')); assert.equal(attention(sub).length, 0);
assert(!subPrompt.systemPrompt.join('').includes('request current'));
assert.equal(sub.calls.filter(c => c.args[0] === 'inbox').length, 0);
// Omp rebinds the same factories for workers with a separate session event bus.
// A worker registration must leave the lead's callbacks and ownership intact.
reset(); const lead = createOmpHost({cwd}), worker = createOmpHost({cwd, agentKind: 'sub'});
factory(lead); await lead.emit('session_start'); factory(worker);
const nativeRegistry = globalThis[Symbol.for('kata.omp.native.v1')].get(worker.events);
nativeRegistry.workerInboxRecipients.set(`project:${cwd}`, async ctx => {
  assert.equal(ctx.agent.kind, 'sub');
  await Promise.resolve();
  return {recipient: 'actor/worker', workspace: cwd};
});
const leadPrompt = await lead.emit('before_agent_start', {systemPrompt: []});
assert(leadPrompt.systemPrompt.join('').includes('contract current'));
const workerPrompt = await worker.emit('before_agent_start', {systemPrompt: []});
assert(workerPrompt.systemPrompt.join('').includes('worker request'));
assert(!workerPrompt.systemPrompt.join('').includes('request current'));
assert.deepEqual(worker.calls.filter(c => c.args[0] === 'inbox').map(c => c.args),
  [['inbox', '--context', '--for', 'actor/worker']]);
// A user-scoped worker binding still supplies inbox context when the project
// contract adapter wins prompt selection.
reset(); const crossScopeWorker = createOmpHost({cwd, agentKind: 'sub'});
const userFactory = (await import(pathToFileURL(userPath))).default;
factory(crossScopeWorker); userFactory(crossScopeWorker);
const crossScopeRegistry = globalThis[Symbol.for('kata.omp.native.v1')].get(crossScopeWorker.events);
crossScopeRegistry.workerInboxRecipients.set('user:', async ctx => {
  assert.equal(ctx.agent.kind, 'sub');
  return {recipient: 'actor/worker', workspace: cwd};
});
const crossScopePrompt = await crossScopeWorker.emit('before_agent_start', {systemPrompt: []});
assert(crossScopePrompt.systemPrompt.join('').includes('worker request'));
assert(!crossScopePrompt.systemPrompt.join('').includes('request current'));
assert.equal(crossScopeWorker.calls.find(c => c.args[0] === 'agent-contract-hook').executable, 'kata');
assert.deepEqual(crossScopeWorker.calls.filter(c => c.args[0] === 'inbox').map(c => c.args),
  [['inbox', '--context', '--for', 'actor/worker']]);
// A restored worker routes its inbox through the saved assignment workspace,
// even when its current cwd is elsewhere.
const savedWorkspace = path.join(cwd, 'saved-assignment');
const restoredCwd = path.join(path.dirname(cwd), `${path.basename(cwd)}-restored`);
const savedSessionFile = path.join(savedWorkspace, 'worker-session.jsonl');
reset(); const restoredWorker = createOmpHost({cwd: restoredCwd, agentKind: 'sub', sessionFile: savedSessionFile});
userFactory(restoredWorker);
const restoredRegistry = globalThis[Symbol.for('kata.omp.native.v1')].get(restoredWorker.events);
restoredRegistry.workerInboxRecipients.set(`project:${path.join(cwd, 'other-assignment')}`, async (_ctx, sessionFile) => {
  assert.equal(sessionFile, savedSessionFile);
  return undefined;
});
restoredRegistry.workerInboxRecipients.set(`project:${savedWorkspace}`, async (_ctx, sessionFile) => {
  assert.equal(sessionFile, savedSessionFile);
  return {recipient: 'actor/worker', workspace: savedWorkspace};
});
const restoredPrompt = await restoredWorker.emit('before_agent_start', {systemPrompt: []});
assert(restoredPrompt.systemPrompt.join('').includes('worker request'));
assert(!restoredPrompt.systemPrompt.join('').includes('request current'));
const restoredInbox = restoredWorker.calls.find(c => c.args[0] === 'inbox');
assert.deepEqual(restoredInbox.args, ['inbox', '--context', '--for', 'actor/worker']);
assert.deepEqual(restoredInbox.options, {cwd: savedWorkspace, timeout: 10000});
await lead.emit('session_shutdown'); assert.deepEqual(attention(lead).map(c => c.args[3]), ['start', 'end']);
assert.equal(attention(worker).length, 0);
// Old callbacks retire immediately after factory rebinding, including prompts.
reset(); const old = createOmpHost({cwd}), fresh = createOmpHost({cwd, events: old.events}); factory(old); factory(fresh);
await old.emit('session_start'); await old.emit('before_agent_start', {systemPrompt: []}); await old.emit('session_shutdown');
assert.equal(old.calls.length, 0); await fresh.emit('session_start'); assert.equal(attention(fresh).length, 1);
// Failure never leaves stale context behind; the next prompt refreshes.
fresh.execute = async () => ({code: 1, killed: false, stdout: ''});
const failed = await fresh.emit('before_agent_start', {systemPrompt: ['<kata_contract>stale</kata_contract>', authored]});
assert(!failed.systemPrompt.join('').includes('stale')); assert(failed.systemPrompt.join('').includes('unavailable')); assert.equal(fresh.notifications.length, 2);
// Failed starts and ends retry, pending calls collapse, and reload preserves handoff.
for (const failure of ['nonzero', 'killed', 'reject']) {
  reset(); const h = createOmpHost({cwd}); factory(h); let fail = true;
  h.execute = async () => {if (fail) {if (failure === 'reject') throw Error('transport'); return {code: failure === 'nonzero' ? 1 : 0, killed: failure === 'killed'};} return {code: 0, killed: false};};
  await h.emit('session_start'); fail = false; await h.emit('session_start'); await h.emit('session_start');
  assert.equal(attention(h).length, 2);
  factory(h); await h.emit('session_start'); assert.equal(attention(h).length, 2);
  fail = true; await h.emit('session_shutdown'); fail = false; await h.emit('session_shutdown'); await h.emit('session_shutdown');
  assert.deepEqual(attention(h).map(c => c.args[3]), ['start', 'start', 'end', 'end']);
}
reset(); const pending = createOmpHost({cwd}); factory(pending); let resolve;
pending.execute = () => new Promise(r => {resolve = r;});
const start = pending.emit('session_start'), duplicate = pending.emit('session_start');
for (let i = 0; i < 5 && !resolve; i++) await new Promise(resolve => setImmediate(resolve));
assert.equal(typeof resolve, 'function');
assert.equal(attention(pending).length, 1); resolve({code: 0, killed: false}); await Promise.all([start, duplicate]);
// Shutdown waits for an in-flight startup before deciding whether cleanup is needed.
reset(); const exiting = createOmpHost({cwd}); factory(exiting); let completeStart;
exiting.execute = (_, args) => args[0] === 'agent-hook' && args[3] === 'start'
  ? new Promise(resolve => {completeStart = resolve;}) : {code: 0, killed: false};
const entering = exiting.emit('session_start');
for (let i = 0; i < 5 && !completeStart; i++) await new Promise(resolve => setImmediate(resolve));
assert.equal(typeof completeStart, 'function');
const leaving = exiting.emit('session_shutdown');
await new Promise(resolve => setImmediate(resolve));
completeStart({code: 0, killed: false}); await Promise.all([entering, leaving]);
assert.deepEqual(attention(exiting).map(c => c.args[3]), ['start', 'end']);
// Resolve the issue before locating a daemon that the lookup may auto-start.
reset(); const warming = createOmpHost({cwd}); factory(warming); const lookups = [];
warming.executeIdentity = async (_, args) => {
  if (args[3] === 'show') {
    lookups.push('show');
    return {code: 0, killed: false, stdout: JSON.stringify({issue: {project_id: 42, uid: 'uid-issue-ref'}})};
  }
  lookups.push('locate');
  if (!lookups.includes('show')) return {code: 1, killed: false, stdout: ''};
  return {code: 0, killed: false, stdout: JSON.stringify({network: 'tcp', address: '127.0.0.1:7777', request_base_url: 'http://127.0.0.1:7777'})};
};
await warming.emit('session_start'); await warming.emit('session_shutdown');
assert.deepEqual(lookups, ['show', 'locate']);
assert.deepEqual(attention(warming).map(c => c.args[3]), ['start', 'end']);
// Untracked and option-like refs must be rejected before a show can start a daemon.
for (const ref of [undefined, '', '   ', '-help']) {
  reset(); const untracked = createOmpHost({cwd}); factory(untracked); const calls = [];
  if (ref === undefined) delete process.env.KATA_REF; else process.env.KATA_REF = ref;
  untracked.executeIdentity = async (_, args) => {
    calls.push(args[3]);
    return {code: 0, killed: false, stdout: args[3] === 'show'
      ? JSON.stringify({issue: {project_id: 42, uid: 'uid-untracked'}})
      : JSON.stringify({network: 'tcp', address: '127.0.0.1:7777', request_base_url: 'http://127.0.0.1:7777'})};
  };
  await untracked.emit('session_start'); await untracked.emit('session_shutdown');
  assert.deepEqual(calls, [], `unsupported ref ${JSON.stringify(ref)} must not query identity`);
  assert.equal(attention(untracked).length, 0);
}
// Switching sessions must discard a baseline whose issue never resolved.
reset(); const refSwitch = createOmpHost({cwd}); factory(refSwitch); const refModes = [];
refSwitch.executeIdentity = async (_, args) => {
  if (args[3] === 'show' && args[4] === 'missing-ref') return {code: 1, killed: false, stdout: ''};
  if (args[3] === 'show') return {code: 0, killed: false, stdout: JSON.stringify({issue: {project_id: 42, uid: 'uid-issue-ref'}})};
  return {code: 0, killed: false, stdout: JSON.stringify({network: 'tcp', address: '127.0.0.1:7777', request_base_url: 'http://127.0.0.1:7777'})};
};
refSwitch.execute = async (_, args) => {
  refModes.push([args[3], args[9]]);
  return {code: args[9] === 'missing-ref' ? 1 : 0, killed: false};
};
process.env.KATA_REF = 'missing-ref'; await refSwitch.emit('session_start');
process.env.KATA_REF = 'issue-ref'; refSwitch.setSession('B');
await refSwitch.emit('session_switch', {reason: 'new'}); await refSwitch.emit('session_shutdown');
assert.deepEqual(refModes, [['start', 'issue-ref'], ['end', 'issue-ref']]);
reset();
// If a start write commits but its response is lost, shutdown still sends the
// owner-checked end operation for that uncertain start.
const uncertain = createOmpHost({cwd}); factory(uncertain); let persisted = false;
const uncertainModes = [];
uncertain.execute = async (_, args) => {
  const mode = args[3]; uncertainModes.push(mode);
  if (mode === 'start') {persisted = true; return {code: 1, killed: false};}
  if (mode === 'end') persisted = false;
  return {code: 0, killed: false};
};
await uncertain.emit('session_start'); assert.equal(persisted, true);
await uncertain.emit('session_shutdown');
assert.deepEqual(uncertainModes, ['start', 'end']); assert.equal(persisted, false);
// A live Pi owner and OMP session resolve to the same identity on a TCP route.
reset(); const tcpChild = createOmpHost({cwd}); factory(tcpChild);
const piOwner = JSON.stringify({pid: process.ppid, identity: identity('issue-ref')});
process.env.KATA_ATTENTION_HOST = piOwner;
await tcpChild.emit('session_start'); await tcpChild.emit('session_shutdown');
assert.equal(attention(tcpChild).length, 0);
assert.equal(process.env.KATA_ATTENTION_HOST, piOwner);
// A failed end during switch retains its baseline and retries before starting B.
reset(); const switching = createOmpHost({cwd}); factory(switching); await switching.emit('session_start');
let failEnd = true; switching.execute = async (_, args) => ({code: args[3] === 'end' && failEnd ? 1 : 0, killed: false});
switching.setSession('B'); await switching.emit('session_switch', {reason: 'new'}); failEnd = false; await switching.emit('session_switch', {reason: 'new'}); await switching.emit('session_shutdown');
assert.deepEqual(attention(switching).map(c => [c.args[3], c.args[5]]), [['start', 'A'], ['end', 'A'], ['end', 'A'], ['start', 'B'], ['end', 'B']]);
// Shutdown fences a switch whose old-session end is still pending.
reset(); const overlapping = createOmpHost({cwd}); factory(overlapping); await overlapping.emit('session_start');
let finishSwitchEnd;
overlapping.execute = (_, args) => args[3] === 'end'
  ? new Promise(resolve => {finishSwitchEnd = resolve;}) : {code: 0, killed: false};
overlapping.setSession('B'); const switchingEnd = overlapping.emit('session_switch', {reason: 'new'});
for (let i = 0; i < 5 && !finishSwitchEnd; i++) await new Promise(resolve => setImmediate(resolve));
assert.equal(typeof finishSwitchEnd, 'function');
const shutdownDuringSwitch = overlapping.emit('session_shutdown');
finishSwitchEnd({code: 0, killed: false}); await Promise.all([switchingEnd, shutdownDuringSwitch]);
assert.deepEqual(attention(overlapping).map(c => [c.args[3], c.args[5]]), [['start', 'A'], ['end', 'A']]);
assert.equal(process.env.KATA_ATTENTION_HOST, undefined);
// A shutdown after a failed switch retries the retained baseline even though
// the current SDK session ID has already changed.
reset(); const retained = createOmpHost({cwd}); factory(retained); await retained.emit('session_start');
let failRetainedEnd = true; retained.execute = async (_, args) => ({code: args[3] === 'end' && failRetainedEnd ? 1 : 0, killed: false});
retained.setSession('B'); await retained.emit('session_switch', {reason: 'new'});
process.env.KATA_REF = 'changed-ref'; failRetainedEnd = false; await retained.emit('session_shutdown');
assert.deepEqual(attention(retained).map(c => [c.args[3], c.args[5], c.args[9], c.args[11], c.options.cwd]),
  [['start', 'A', 'issue-ref', cwd, cwd], ['end', 'A', 'issue-ref', cwd, cwd], ['end', 'A', 'issue-ref', cwd, cwd]]);
// Inherited live ownership suppresses nested attention; stale/different markers do not.
for (const marker of [JSON.stringify({pid: process.ppid, identity: identity('issue-ref')}), JSON.stringify({pid: process.ppid, identity: identity('other-ref')}), '{', JSON.stringify({pid: process.pid, identity: identity('issue-ref')})]) {
  reset(); process.env.KATA_ATTENTION_HOST = marker; const h = createOmpHost({cwd}); factory(h); await h.emit('session_start'); await h.emit('session_shutdown');
  assert.equal(attention(h).length, marker === JSON.stringify({pid: process.ppid, identity: identity('issue-ref')}) ? 0 : 2);
}
// Scope selection is per capability: the deepest contract wins while attention
// falls back to the parent project; outside the project the user adapter wins.
if (userPath) {
  const user = (await import(pathToFileURL(userPath))).default;
  const nested = (await import(pathToFileURL(nestedPath))).default;
  reset(); const scoped = createOmpHost({cwd: path.join(cwd, 'nested', 'deep')});
  user(scoped); factory(scoped); nested(scoped);
  await scoped.emit('session_start'); await scoped.emit('before_agent_start', {systemPrompt: []});
  assert.equal(attention(scoped)[0].executable, 'kata');
  assert.equal(scoped.calls.find(c => c.args[0] === 'agent-contract-hook').executable, 'nested-kata');
  const outside = createOmpHost({cwd: path.dirname(cwd)}); user(outside); factory(outside); nested(outside);
  await outside.emit('before_agent_start', {systemPrompt: []});
  assert.equal(outside.calls[0].executable, 'user-kata');
}
console.log('omp native fixture passed');
