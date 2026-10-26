import path from 'node:path';

// The runner calls every handler in registration order and chains prompt rewrites.
export function createOmpHost({cwd, sessionId = 'A', agentKind = 'main', events = {}, sessionFile} = {}) {
  const handlers = new Map(), calls = [], notifications = [];
  let id = sessionId;
  const ctx = {cwd, agent: {kind: agentKind}, sessionManager: {
    getSessionId: () => id,
    getSessionFile: () => sessionFile || path.join(cwd, `${id}.jsonl`),
  },
    ui: {notify: (message, kind) => notifications.push({message, kind})}};
  const host = {zod: {}, events, handlers, calls, notifications, ctx,
    on(name, fn) {const list = handlers.get(name) || []; list.push(fn); handlers.set(name, list);},
    async exec(executable, args, options) {
      calls.push({executable, args, options});
      const identityLookup = args[0] === '--json' && args[1] === '--workspace';
      if (identityLookup) {
        if (host.executeIdentity) return host.executeIdentity(executable, args, options);
        const stdout = args[3] === 'daemon' && args[4] === 'locate'
          ? JSON.stringify({network: 'tcp', address: '127.0.0.1:7777', request_base_url: 'http://127.0.0.1:7777'})
          : args[3] === 'show' ? JSON.stringify({issue: {project_id: 42, uid: `uid-${args[4]}`}}) : '';
        return {code: stdout ? 0 : 1, killed: false, stdout, stderr: ''};
      }
      if (host.execute) return host.execute(executable, args, options);
      const stdout = args[0] === 'agent-contract-hook'
        ? JSON.stringify({hookSpecificOutput: {additionalContext: 'contract current'}})
        : args[0] === 'inbox' ? args[3] === 'actor/worker' ? 'worker request' : 'request current' : '';
      return {code: 0, killed: false, stdout, stderr: ''};
    },
    setSession(value) {id = value;},
    async emit(name, event = {}) {
      let result = {...event};
      for (const fn of handlers.get(name) || []) {
        const update = await fn(result, ctx);
        if (update) result = {...result, ...update};
      }
      return result;
    },
  };
  return host;
}
