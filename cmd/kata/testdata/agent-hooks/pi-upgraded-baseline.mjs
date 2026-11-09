import assert from 'node:assert/strict';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const [extension, cwd, started] = process.argv.slice(2);
const session = 'upgraded-session';
const calls = [];
const registry = globalThis[Symbol.for('kata.pi.native.v1')] = {adapters: new Map(), active: new Map()};
// A session from the previous adapter retains its baseline across the
// reinstall and reload. That baseline has no startAttempted field. It is not
// started when the start command's response was lost.
registry.active.set(path.resolve(cwd), {id: session, cwd: path.resolve(cwd), executable: 'kata', ref: 'lead-ref', started: started === 'true', ended: false});
const handlers = new Map();
(await import(pathToFileURL(extension))).default({
  on(name, fn) { handlers.set(name, [...(handlers.get(name) || []), fn]); },
  async exec(_executable, args) { calls.push(args[3]); return {code: 0, killed: false, stdout: '', stderr: ''}; },
});
const ctx = {cwd, sessionManager: {getSessionId: () => session}};
for (const fn of handlers.get('session_shutdown') || []) await fn({reason: 'quit'}, ctx);
assert.deepEqual(calls, ['end'], 'quit ends attention started by the previous adapter');
