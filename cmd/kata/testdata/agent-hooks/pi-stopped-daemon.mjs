import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {pathToFileURL} from 'node:url';

const [extension, cwd, ref] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extension))).default;
const calls = [];
const handlers = new Map();
process.env.KATA_REF = ref;
delete process.env.KATA_ATTENTION_HOST;

const pi = {
  on(name, handler) {
    const list = handlers.get(name) || [];
    list.push(handler);
    handlers.set(name, list);
  },
  async exec(executable, args, options) {
    const result = spawnSync(executable, args, {
      cwd: options.cwd,
      env: process.env,
      encoding: 'utf8',
      timeout: options.timeout,
    });
    calls.push({args, code: result.status ?? 1, stdout: result.stdout || '', stderr: result.stderr || result.error?.message || ''});
    return {
      code: result.status ?? 1,
      killed: result.error?.code === 'ETIMEDOUT',
      stdout: result.stdout || '',
      stderr: result.stderr || result.error?.message || '',
    };
  },
};
factory(pi);
const ctx = {cwd, sessionManager: {getSessionId: () => 'stopped-daemon-session'}};
for (const handler of handlers.get('session_start') || []) await handler({reason: 'startup'}, ctx);
const start = calls.findIndex(call => call.args[0] === 'agent-hook' && call.args[1] === 'attention-native' && call.args[3] === 'start');
assert.ok(start >= 0, `attention starts after a stopped local daemon is auto-started: ${JSON.stringify(calls)}`);
assert.ok(calls.findIndex(call => call.args[3] === 'show') < calls.findIndex(call => call.args[3] === 'daemon'),
  'show can auto-start the daemon before the non-starting daemon locate probe');
for (const handler of handlers.get('session_shutdown') || []) await handler({reason: 'quit'}, ctx);
