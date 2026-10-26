import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { realFixture, test, runTests } from "./plan-test-utils.mjs";
import { planHost } from "./omp-plan-host.mjs";
const generated = readFileSync(process.env.KATA_TEST_OMP_ADAPTER, "utf8");
const options = JSON.parse(/const options = ([^\n]+);/.exec(generated)[1]);
const f = await realFixture("actor", "lead");
// Keep the real kataEnv result, including the lead's identity, in execFile.
for (const key of Object.keys(process.env)) if (key.startsWith("KATA_")) delete process.env[key];
Object.assign(process.env, f.env);
const file = path.join(f.scratch, "runtime.mjs");
writeFileSync(
  file,
  `import path from 'node:path';import {createHash} from 'node:crypto';import {realpathSync,appendFileSync} from 'node:fs';import {fileURLToPath} from 'node:url';const options=${JSON.stringify(options)};const execFile=(...args)=>globalThis[Symbol.for('kata.real.exec')](...args);\n${readFileSync(process.argv[2], "utf8")}\n${readFileSync(path.join(path.dirname(process.argv[2]), "plan_omp.js"), "utf8")}\nexport default kataPlanOmp;`,
);
const factory = (await import(pathToFileURL(file))).default;
let holdRead,
  failNextNotify = false,
  afterNotify,
  failWorkspace,
  abortAfterCreate,
  beforeTaskShow,
  failClearTerminal = false;
globalThis[Symbol.for("kata.real.exec")] = (exe, args, opts, cb) => {
  if (exe === "omp") {
    cb(null, JSON.stringify(args[0] === "config" ? { value: false } : { marketplace: [] }), "");
    return;
  }
  if (failWorkspace && opts?.cwd === failWorkspace) {
    cb(new Error("alternate daemon unavailable"), "", "alternate daemon unavailable");
    return;
  }
  if (
    ["show", "status"].includes(args[0]) &&
    (args[1] === holdRead?.ref || holdRead?.ref === "*")
  ) {
    holdRead.resolve(() => execFile(exe, args, opts, cb));
    holdRead = undefined;
    return;
  }
  if (args[0] === "show" && args[1] === beforeTaskShow?.ref) {
    const hook = beforeTaskShow.run;
    beforeTaskShow = undefined;
    hook().then(
      () => execFile(exe, args, opts, cb),
      (error) => cb(error, "", String(error)),
    );
    return;
  }
  if (args[0] === "notify" && failNextNotify) {
    failNextNotify = false;
    cb(new Error("notification unavailable"), "", "notification unavailable");
    return;
  }
  if (
    args[0] === "meta" &&
    args[1] === "unset" &&
    args[3] === "omp.terminal_state" &&
    failClearTerminal
  ) {
    failClearTerminal = false;
    cb(new Error("terminal marker unavailable"), "", "terminal marker unavailable");
    return;
  }
  if (args[0] === "notify" && afterNotify) {
    const hook = afterNotify;
    afterNotify = undefined;
    execFile(exe, args, opts, (error, stdout, stderr) => {
      if (error) {
        cb(error, stdout, stderr);
        return;
      }
      hook().then(
        () => cb(null, stdout, stderr),
        (hookError) => cb(hookError, "", String(hookError)),
      );
    });
    return;
  }
  if (args[0] === "create" && abortAfterCreate) {
    const controller = abortAfterCreate;
    abortAfterCreate = undefined;
    execFile(exe, args, opts, (error, stdout, stderr) => {
      cb(error, stdout, stderr);
      if (!error) controller.abort();
    });
    return;
  }
  execFile(exe, args, opts, cb);
};
const names = ["Lead implementation", "Worker verification"];
let sequence = 0;
async function until(predicate, label) {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error("timed out waiting for " + label);
}
async function fixture() {
  const created = await f.kata([
    "create",
    `Example delegated work ${++sequence}`,
    "--force-new",
    "--owner",
    "actor",
    "--json",
  ]);
  const parent = "example-workspace#" + created.issue.short_id;
  const entries = [
    {
      type: "custom",
      customType: "io.kata.omp.parent",
      data: { parent, workspace: f.cwd },
    },
  ];
  function host(
    sessionId,
    agentKind = "main",
    plugin = factory,
    hostEntries = entries,
    cwd = f.cwd,
  ) {
    let lifecycle;
    const h = planHost(file, {
      cwd,
      sessionId,
      agentKind,
      entries: hostEntries,
      events: {
        on(name, fn) {
          if (name === "task:subagent:lifecycle") lifecycle = fn;
        },
      },
    });
    h.ctx.agent.id = sessionId;
    h.ctx.sessionManager.getSessionFile = () => path.join(f.scratch, sessionId + ".jsonl");
    const registryKey = Symbol.for("kata.omp.native.v1");
    const registries = (globalThis[registryKey] ||= new WeakMap());
    registries.set(h.events, { workerInboxRecipients: new Map() });
    plugin(h);
    return Object.assign(h, { lifecycle: (...args) => lifecycle(...args) });
  }
  const lead = host(`lead-${sequence}`);
  await lead.emit("session_start");
  const init = await lead.call({ op: "init", items: names });
  assert.equal(init.isError, undefined, init.content[0].text);
  const children = (await f.kata(["show", parent, "--json"])).children;
  const refs = names.map(
    (title) => "example-workspace#" + children.find((i) => i.title === title).short_id,
  );
  const ref = refs[1];
  const show = () => f.kata(["show", ref, "--json"]);
  async function spawn(id, text = ref, { beforeStart, pauseAt = ref } = {}) {
    await lead.emit("tool_call", {
      toolName: "task",
      toolCallId: id,
      input: { task: text },
    });
    await beforeStart?.();
    const worker = host(id, "sub");
    await worker.emit("session_start");
    const lifecycle = {
      id,
      status: "started",
      parentToolCallId: id,
      sessionFile: worker.ctx.sessionManager.getSessionFile(),
    };
    let releaseRead = () => {};
    let started;
    if (pauseAt === null) started = lead.lifecycle(lifecycle);
    else {
      const paused = new Promise((resolve) => {
        holdRead = { ref: pauseAt, resolve };
      });
      started = lead.lifecycle(lifecycle);
      releaseRead = await paused;
    }
    let prompted = false,
      called = false;
    const prompt = worker.emit("before_agent_start").then((value) => {
      prompted = true;
      return value;
    });
    const command = worker
      .emit("tool_call", {
        toolName: "bash",
        input: { command: "printenv KATA_REF" },
      })
      .then((value) => {
        called = true;
        return value;
      });
    if (pauseAt !== null) {
      try {
        await new Promise((resolve) => setImmediate(resolve));
        assert.equal(prompted, false, "prompt awaits delegate binding");
        assert.equal(called, false, "bash awaits delegate binding");
      } finally {
        releaseRead();
      }
    }
    await started;
    await command;
    await lead.emit("tool_result", { toolName: "task", toolCallId: id });
    return { worker, prompt: await prompt };
  }
  async function settled(id, status = "failed") {
    // Prove this settlement adds the label, rather than accepting an old one.
    await f.kata(["label", "rm", ref, "needs-review", "--json"]);
    const workerFile = path.join(f.scratch, id + ".jsonl");
    await lead.lifecycle({ id, status, sessionFile: workerFile });
    const view = await show();
    assert.equal(view.issue.metadata["omp.subagent_id"], undefined);
    assert.equal(view.issue.metadata["omp.lead_actor"], undefined);
    assert.equal(view.issue.metadata.teammate, "lead");
    assert.ok(view.labels.some((l) => (l.label ?? l) === "needs-review"));
    assert.ok(view.comments.some((c) => c.body.includes(id)));
    const inbox = await f.kata(["inbox", "--for", "actor/lead", "--json"]);
    assert.match(JSON.stringify(inbox), new RegExp(`Subagent ${id} ${status}`));
    const restarted = await lead.call({ op: "start", task: ref });
    assert.equal(restarted.isError, undefined, restarted.content[0].text);
  }
  return { lead, host, ref, refs, show, spawn, settled };
}
async function runShell(command) {
  return new Promise((resolve, reject) =>
    execFile(
      "sh",
      ["-c", command],
      {
        cwd: f.cwd,
        env: {
          ...f.env,
          KATA_INBOX_USER: "actor/lead",
          KATA_REF: "inherited-lead",
        },
      },
      (error, stdout) => (error ? reject(error) : resolve(stdout.trim())),
    ),
  );
}
test("attribution, shell forms, failed settlement and task reuse", async () => {
  assert.equal(options.minDaemonAPI, "0.18.0");
  const { ref, show, spawn, settled } = await fixture();
  const { worker, prompt } = await spawn("worker");
  assert.match(prompt.systemPrompt.join(""), new RegExp(ref));
  assert.match(prompt.systemPrompt.join(""), /--workspace.*--project.*--teammate/);
  const bound = (await show()).issue;
  assert.equal(bound.owner, "actor");
  assert.equal(bound.metadata["omp.lead_actor"], "actor");
  assert.equal(bound.metadata["omp.subagent_id"], "worker");
  const teammate = bound.metadata.teammate;
  assert.ok(teammate && teammate !== "lead");
  // Execute only the command, as omp 18.12.6 does; tool input.env is ignored.
  for (const command of [
    'printf "%s" "$(printenv KATA_TEAMMATE)"',
    "printenv KATA_TEAMMATE | cat",
  ]) {
    const event = await worker.emit("tool_call", {
      toolName: "bash",
      input: { command },
    });
    assert.equal(await runShell(event.input.command), teammate);
  }
  const binding = await worker.emit("tool_call", {
    toolName: "bash",
    input: { command: 'printf "%s\\n%s" "$KATA_REF" "$KATA_INBOX_USER"' },
  });
  assert.equal(await runShell(binding.input.command), `${ref}\nactor/${teammate}`);
  await settled("worker");
});
test("queued workers stay bound to the plan that submitted them", async () => {
  const { lead, ref, show, spawn } = await fixture();
  let otherRef;
  const { prompt } = await spawn("queued-origin", "Worker verification", {
    pauseAt: "*",
    beforeStart: async () => {
      const otherParent = await f.kata([
        "create",
        "Another selected plan",
        "--owner",
        "actor",
        "--force-new",
        "--json",
      ]);
      otherRef = "example-workspace#" + otherParent.issue.short_id;
      const selected = await lead.call({
        op: "init",
        ref: otherRef,
        items: ["Worker verification"],
      });
      assert.equal(selected.isError, undefined, selected.content[0]?.text);
    },
  });
  assert.match(prompt.systemPrompt.join(""), new RegExp(ref));
  assert.equal((await show()).issue.metadata["omp.subagent_id"], "queued-origin");
  const other = await f.kata(["show", otherRef, "--json"]);
  assert.equal(
    other.children.find((issue) => issue.title === "Worker verification")
      .metadata["omp.subagent_id"],
    undefined,
  );
});
test("task assignment waits for initial plan binding", async () => {
  const { lead, host, ref } = await fixture();
  const leadID = lead.ctx.sessionManager.getSessionId();
  const resumed = host(leadID);
  let releaseBinding;
  holdRead = {
    ref: lead.entries[0].data.parent,
    resolve: (resume) => {
      releaseBinding = resume;
    },
  };
  const starting = resumed.emit("session_start");
  await until(() => releaseBinding, "initial plan binding read");
  await starting;
  let assigned = false;
  const assignment = resumed
    .emit("tool_call", {
      toolName: "task",
      toolCallId: "binding-race",
      input: { task: "Worker verification" },
    })
    .then(() => {
      assigned = true;
    });
  try {
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(assigned, false, "task assignment waits for plan binding");
  } finally {
    releaseBinding();
  }
  await assignment;
  await resumed.call({ op: "view" });
  const worker = host("binding-race", "sub");
  await worker.emit("session_start");
  const sessionFile = worker.ctx.sessionManager.getSessionFile();
  await resumed.lifecycle({
    id: "binding-race",
    status: "started",
    parentToolCallId: "binding-race",
    sessionFile,
  });
  const prompt = await worker.emit("before_agent_start");
  assert.match(prompt.systemPrompt.join(""), new RegExp(ref));
  assert.equal(
    (await f.kata(["show", ref, "--json"])).issue.metadata["omp.subagent_id"],
    "binding-race",
  );
  await resumed.lifecycle({ id: "binding-race", status: "failed", sessionFile });
});
test("todo cancellation stops a batch after the active create", async () => {
  const { lead } = await fixture();
  const parent = lead.entries[0].data.parent;
  const first = "Interrupted first append item";
  const second = "Interrupted second append item";
  const controller = new AbortController();
  abortAfterCreate = controller;
  const result = await lead.call(
    { op: "append", items: [first, second] },
    controller.signal,
  );
  assert.equal(result.isError, true, "an aborted batch reports cancellation");
  const children = (await f.kata(["show", parent, "--json"])).children;
  const titles = children.map((child) => child.title);
  assert.ok(titles.includes(first));
  assert.ok(!titles.includes(second), "later batch items are not created");
});
test("a task blocked after matching is not assigned to a worker", async () => {
  const { lead, ref, show, spawn } = await fixture();
  beforeTaskShow = {
    ref,
    run: () => f.kata(["meta", "set", ref, "todo.status", "blocked", "--json"]),
  };
  const { worker, prompt } = await spawn("blocked-after-match");
  const issue = (await show()).issue;
  assert.equal(issue.metadata["todo.status"], "blocked");
  assert.equal(issue.metadata["omp.subagent_id"], undefined);
  assert.equal(issue.metadata["omp.lead_session_key"], undefined);
  assert.equal(issue.metadata.teammate, "lead");
  assert.match(prompt.systemPrompt.join(""), /No Kata task was assigned\./);
  await lead.lifecycle({
    id: "blocked-after-match",
    status: "aborted",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
});
test("settlement after session switch", async () => {
  const { lead, show, spawn, settled } = await fixture();
  await spawn("switched");
  lead.setSession("new-lead");
  await lead.emit("session_switch");
  assert.equal((await show()).issue.metadata["omp.subagent_id"], "switched");
  await settled("switched", "aborted");
});
test("terminal worker settles when the same plan resumes with a new session file", async () => {
  const { lead, show, spawn } = await fixture();
  const { worker } = await spawn("resume-file-switch");
  lead.setSession("resumed-lead");
  lead.ctx.sessionManager.getSessionFile = () =>
    path.join(f.scratch, "resumed-lead.jsonl");
  await lead.lifecycle({
    id: "resume-file-switch",
    status: "failed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  await lead.emit("session_start");
  await until(
    async () => {
      const metadata = (await show()).issue.metadata;
      return (
        metadata["omp.subagent_id"] === undefined &&
        metadata["omp.terminal_state"] === undefined
      );
    },
    "same-plan resumed delegate settlement",
  );
  const issue = (await show()).issue;
  assert.equal(issue.metadata["omp.subagent_id"], undefined);
  assert.equal(issue.metadata["omp.terminal_state"], undefined);
  assert.equal(issue.metadata.teammate, "lead");
  assert.ok((await show()).comments.some((comment) =>
    comment.body.includes("resume-file-switch"),
  ));
});
test("terminal worker settles after the same plan reloads under a new session file", async () => {
  const { lead, show, spawn } = await fixture();
  const { worker } = await spawn("terminal-after-file-switch");
  lead.setSession("new-lead");
  lead.ctx.sessionManager.getSessionFile = () =>
    path.join(f.scratch, "new-lead.jsonl");
  await lead.emit("session_start");
  await lead.lifecycle({
    id: "terminal-after-file-switch",
    status: "failed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  await until(
    async () => {
      const metadata = (await show()).issue.metadata;
      return (
        metadata["omp.subagent_id"] === undefined &&
        metadata["omp.terminal_state"] === undefined
      );
    },
    "terminal delegate settlement after plan reload",
  );
  const issue = (await show()).issue;
  assert.equal(issue.metadata["omp.subagent_id"], undefined);
  assert.equal(issue.metadata["omp.terminal_state"], undefined);
  assert.equal(issue.metadata.teammate, "lead");
  assert.ok((await show()).comments.some((comment) =>
    comment.body.includes("terminal-after-file-switch"),
  ));
});
test("terminal hand-back recovers after a plugin restart", async () => {
  const { lead, host, show, spawn } = await fixture();
  const { worker } = await spawn("restart-terminal");
  lead.setSession("resumed-lead");
  lead.ctx.sessionManager.getSessionFile = () =>
    path.join(f.scratch, "resumed-lead.jsonl");
  await lead.emit("session_start");
  const resumed = await lead.call({ op: "view" });
  assert.equal(resumed.isError, undefined, resumed.content[0].text);
  failNextNotify = true;
  await lead.lifecycle({
    id: "restart-terminal",
    status: "failed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  const pending = (await show()).issue.metadata;
  assert.equal(pending["omp.subagent_id"], "restart-terminal");
  assert.equal(
    pending["omp.terminal_state"]?.id,
    "restart-terminal",
  );
  assert.equal(pending["omp.terminal_state"]?.state, "failed");
  const restartFactory = (
    await import(`${pathToFileURL(file).href}?terminal-restart=${sequence}`)
  ).default;
  const restarted = host(`restart-lead-${sequence}`, "main", restartFactory);
  await restarted.emit("session_start");
  await until(
    async () => {
      const metadata = (await show()).issue.metadata;
      return (
        metadata["omp.subagent_id"] === undefined &&
        metadata["omp.terminal_state"] === undefined
      );
    },
    "durable terminal hand-back after plugin restart",
  );
  const issue = (await show()).issue;
  assert.equal(issue.metadata["omp.subagent_id"], undefined);
  assert.equal(issue.metadata["omp.terminal_state"], undefined);
  assert.equal(issue.metadata.teammate, "lead");
  assert.ok((await show()).comments.some((comment) =>
    comment.body.includes("restart-terminal"),
  ));
});
test("worker hand-back leaves a reassigned issue with its new owner", async () => {
  const { lead, ref, show, spawn } = await fixture();
  const { worker } = await spawn("reassigned-worker");
  const assigned = (await show()).issue;
  assert.equal(assigned.metadata["omp.lead_actor"], "actor");
  const assignedTeammate = assigned.metadata.teammate;
  await f.kata(["edit", ref, "--owner", "another-actor", "--json"]);
  await lead.lifecycle({
    id: "reassigned-worker",
    status: "failed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  const reassigned = await show();
  assert.equal(reassigned.issue.owner, "another-actor");
  assert.equal(reassigned.issue.metadata.teammate, assignedTeammate);
  assert.equal(
    reassigned.issue.metadata["omp.subagent_id"],
    "reassigned-worker",
  );
  assert.equal(reassigned.issue.metadata["omp.terminal_state"], undefined);
  assert.ok(
    !reassigned.comments.some((comment) =>
      comment.body.includes("Subagent reassigned-worker"),
    ),
  );
});
test("terminal state is durable while another plan is selected", async () => {
  const { lead, host, ref, show, spawn } = await fixture();
  const { worker } = await spawn("deferred-terminal");
  const otherParent = await f.kata([
    "create",
    "Another selected plan",
    "--owner",
    "actor",
    "--force-new",
    "--json",
  ]);
  const otherRef = "example-workspace#" + otherParent.issue.short_id;
  const selected = await lead.call({
    op: "init",
    ref: otherRef,
    items: ["Work in the selected plan"],
  });
  assert.equal(selected.isError, undefined, selected.content[0]?.text);
  assert.equal(
    (await show()).issue.metadata["omp.subagent_id"],
    "deferred-terminal",
    "selecting another plan leaves the original delegate assigned",
  );

  await lead.lifecycle({
    id: "deferred-terminal",
    status: "failed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  const pending = (await show()).issue;
  assert.equal(pending.metadata["omp.subagent_id"], "deferred-terminal");
  assert.equal(pending.metadata["omp.terminal_state"]?.id, "deferred-terminal");
  assert.equal(pending.metadata["omp.terminal_state"]?.state, "failed");
  assert.equal(pending.metadata["omp.terminal_state"]?.ref, ref);
  assert.equal(pending.metadata["omp.terminal_state"]?.workspace, f.cwd);

  const restartFactory = (
    await import(pathToFileURL(file).href + "?deferred-terminal=" + sequence)
  ).default;
  const restarted = host("deferred-lead-" + sequence, "main", restartFactory);
  await restarted.emit("session_start");
  await until(
    async () => {
      const metadata = (await show()).issue.metadata;
      return (
        metadata["omp.subagent_id"] === undefined &&
        metadata["omp.terminal_state"] === undefined
      );
    },
    "deferred terminal hand-back after restart",
  );
  const issue = (await show()).issue;
  assert.equal(issue.metadata.teammate, "lead");
  assert.ok((await show()).comments.some((comment) =>
    comment.body.includes("deferred-terminal"),
  ));
});
test("worker settlement uses its assignment workspace after switching", async () => {
  const { lead, show, spawn } = await fixture();
  const { worker } = await spawn("workspace-bound");
  writeFileSync(
    path.join(f.outside, ".kata.local.toml"),
    'version = 1\n\n[server]\nurl = "http://127.0.0.1:1"\n',
  );
  lead.entries[0].data.workspace = f.outside;
  lead.ctx.cwd = f.outside;
  lead.setSession("other-daemon");
  failWorkspace = f.outside;
  await lead.emit("session_switch");
  await lead.lifecycle({
    id: "workspace-bound",
    status: "failed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  failWorkspace = undefined;
  const issue = (await show()).issue;
  assert.equal(issue.metadata["omp.subagent_id"], undefined);
  assert.equal(issue.metadata.teammate, "lead");
  assert.ok((await show()).labels.some((l) => (l.label ?? l) === "needs-review"));
  assert.ok((await show()).comments.some((c) => c.body.includes("workspace-bound")));
});
test("worker close instructions use its saved assignment workspace", async () => {
  const { spawn } = await fixture();
  const { worker } = await spawn("workspace-close-prompt");
  worker.ctx.cwd = f.outside;
  const prompt = await worker.emit("before_agent_start");
  const text = prompt.systemPrompt.join("\n");
  assert.ok(text.includes(`--workspace '${f.cwd}'`));
  assert.ok(!text.includes(`--workspace '${f.outside}'`));
});
test("worker inbox resolver returns its saved assignment workspace", async () => {
  const { lead, spawn } = await fixture();
  const { worker } = await spawn("workspace-inbox");
  const sessionFile = worker.ctx.sessionManager.getSessionFile();
  worker.ctx.cwd = f.outside;
  worker.ctx.sessionManager.getSessionFile = () => path.relative(f.cwd, sessionFile);
  const registry = globalThis[Symbol.for("kata.omp.native.v1")].get(lead.events);
  const resolver = registry.workerInboxRecipients.get(
    `${options.scope}:${options.workspace}`,
  );
  assert.equal(typeof resolver, "function");
  const binding = await resolver(
    worker.ctx,
    worker.ctx.sessionManager.getSessionFile(),
  );
  assert.match(binding.recipient, /^actor\//);
  assert.equal(binding.workspace, f.cwd);
});
test("local-plan worker inbox identity uses the effective actor", async () => {
  const { host } = await fixture();
  const lead = host(
    `unplanned-lead-${sequence}`,
    "main",
    factory,
    [],
    f.outside,
  );
  await lead.emit("session_start");
  const init = await lead.call({ op: "init", items: names });
  assert.equal(init.isError, undefined, init.content[0].text);
  await lead.emit("tool_call", {
    toolName: "task",
    toolCallId: "unplanned-worker",
    input: { task: "Handle work before selecting a plan" },
  });
  const worker = host(
    "unplanned-worker",
    "sub",
    factory,
    [],
    f.outside,
  );
  await worker.emit("session_start");
  await lead.lifecycle({
    id: "unplanned-worker",
    status: "started",
    parentToolCallId: "unplanned-worker",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  const rewritten = await worker.emit("tool_call", {
    toolName: "bash",
    input: { command: 'printf "%s" "$KATA_INBOX_USER"' },
  });
  assert.match(rewritten.input.command, /KATA_INBOX_USER='actor\//);
  assert.doesNotMatch(rewritten.input.command, /undefined\//);
});
test("worker command clears inbox identity when actor lookup fails", async () => {
  const { host } = await fixture();
  const lead = host(
    `unavailable-lead-${sequence}`,
    "main",
    factory,
    [],
    f.outside,
  );
  await lead.emit("session_start");
  await lead.emit("tool_call", {
    toolName: "task",
    toolCallId: "unavailable-worker",
    input: { task: "Handle work before selecting a plan" },
  });
  const worker = host(
    "unavailable-worker",
    "sub",
    factory,
    [],
    f.outside,
  );
  await worker.emit("session_start");
  failWorkspace = f.outside;
  try {
    await lead.lifecycle({
      id: "unavailable-worker",
      status: "started",
      parentToolCallId: "unavailable-worker",
      sessionFile: worker.ctx.sessionManager.getSessionFile(),
    });
    const rewritten = await worker.emit("tool_call", {
      toolName: "bash",
      input: { command: 'printf "%s" "$KATA_INBOX_USER"' },
    });
    assert.match(rewritten.input.command, /unset KATA_INBOX_USER;/);
    assert.doesNotMatch(rewritten.input.command, /undefined\//);
  } finally {
    failWorkspace = undefined;
  }
});
test("a second OMP session preserves another session's live delegate", async () => {
  const { host, show, spawn } = await fixture();
  await spawn("live-in-first-session");
  const assigned = (await show()).issue;
  const secondFactory = (
    await import(`${pathToFileURL(file).href}?second-session=${sequence}`)
  ).default;
  const secondLead = host(`second-lead-${sequence}`, "main", secondFactory);
  await secondLead.emit("session_start");
  const after = (await show()).issue;
  assert.equal(after.metadata["omp.subagent_id"], "live-in-first-session");
  assert.equal(after.metadata.teammate, assigned.metadata.teammate);
  assert.ok(
    !(await show()).labels.some((label) => (label.label ?? label) === "needs-review"),
  );
  assert.ok(
    !(await show()).comments.some((comment) =>
      comment.body.includes("Subagent live-in-first-session"),
    ),
  );
});
test(
  "terminal worker during resume transition settles after session starts",
  async () => {
    const { lead, show, spawn } = await fixture();
    const { worker } = await spawn("resume-race");
    const sessionFile = worker.ctx.sessionManager.getSessionFile();
    lead.setSession("resumed-lead");
    await lead.lifecycle({ id: "resume-race", status: "failed", sessionFile });
    assert.equal((await show()).issue.metadata["omp.subagent_id"], "resume-race");
    await lead.emit("session_start");
    await until(
      async () => {
        const metadata = (await show()).issue.metadata;
        return (
          metadata["omp.subagent_id"] === undefined &&
          metadata["omp.terminal_state"] === undefined
        );
      },
      "resumed-session delegate settlement",
    );
    const issue = (await show()).issue;
    assert.equal(issue.metadata["omp.subagent_id"], undefined);
    assert.equal(issue.metadata.teammate, "lead");
    assert.ok(
      (await show()).comments.some((comment) =>
        comment.body.includes("resume-race"),
      ),
    );
    console.log("PASS terminal worker during resume transition");
  },
);
test("restart recovers marker-only partial transfer", async () => {
  const { lead, host, ref, show } = await fixture();
  const leadId = lead.ctx.sessionManager.getSessionId();
  const leadFile = path.join(f.scratch, `${leadId}.jsonl`);
  const leadKey = createHash("sha256")
    .update(`file:${leadFile}`)
    .digest("hex");
  await f.kata(["edit", ref, "--owner", "actor", "--json"]);
  await f.kata(["meta", "set", ref, "omp.subagent_id", "orphan", "--json"]);
  await f.kata([
    "meta",
    "set",
    ref,
    "omp.lead_session_key",
    JSON.stringify(leadKey),
    "--json-value",
    "--json",
  ]);
  await f.kata(["meta", "set", ref, "omp.lead_actor", "actor", "--json"]);
  const restartFactory = (
    await import(`${pathToFileURL(file).href}?restart=${sequence}`)
  ).default;
  const fresh = host(leadId, "main", restartFactory);
  fresh.ctx.getAsyncJobSnapshot = () => ({
    running: [{ id: "job", agentId: "orphan" }],
  });
  await fresh.emit("session_start");
  assert.equal((await show()).issue.metadata["omp.subagent_id"], "orphan");
  fresh.ctx.getAsyncJobSnapshot = () => ({ running: [] });
  const beforeView = await show();
  assert.equal(beforeView.issue.owner, "actor");
  assert.equal(beforeView.issue.metadata["omp.lead_actor"], "actor");
  const status = await f.kata(["status", ref, "--json"]);
  assert.equal(status.actor, "actor", JSON.stringify(status));
  const viewed = await fresh.call({ op: "view" });
  assert.equal(viewed.isError, undefined, viewed.content?.[0]?.text);
  const afterView = await show();
  assert.equal(
    afterView.issue.metadata["omp.subagent_id"],
    undefined,
    JSON.stringify(afterView.issue.metadata),
  );
  assert.ok((await show()).comments.some((c) => c.body.includes("orphan")));
});
for (const count of [1, 2])
  test(`queued assignment with ${count} children`, async () => {
    const { lead, host, refs } = await fixture();
    const call = `queued-${count}`;
    await lead.emit("tool_call", {
      toolName: "task",
      toolCallId: call,
      input: {
        tasks: names
          .slice(0, count)
          .map((name) => ({ name, task: "Complete the named plan item" })),
      },
    });
    await lead.emit("tool_result", { toolName: "task", toolCallId: call });
    for (let index = 0; index < count; index++) {
      const id = `${call}-${index}`,
        worker = host(id, "sub");
      await worker.emit("session_start");
      await lead.lifecycle({
        id,
        index,
        parentToolCallId: call,
        status: "started",
        sessionFile: worker.ctx.sessionManager.getSessionFile(),
      });
      const prompt = await worker.emit("before_agent_start");
      assert.match(prompt.systemPrompt.join(""), new RegExp(refs[index]));
      assert.equal(
        (await f.kata(["show", refs[index], "--json"])).issue.metadata["omp.subagent_id"],
        id,
      );
      await lead.lifecycle({
        id,
        status: "failed",
        sessionFile: worker.ctx.sessionManager.getSessionFile(),
      });
    }
  });
test("worker self-close refreshes the lead", async () => {
  const { lead, ref, show, spawn } = await fixture();
  const { worker } = await spawn("closed");
  const command = `${JSON.stringify(options.executable)} close ${ref} --done --message 'Finished the delegated check and verified the expected output.' --evidence test:fixture --json`;
  const event = await worker.emit("tool_call", {
    toolName: "bash",
    input: { command },
  });
  await runShell(event.input.command);
  await lead.lifecycle({
    id: "closed",
    status: "completed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  assert.equal((await show()).issue.status, "closed");
  assert.match(lead.widgets.at(-1)[1][0], /1\/2 done/);
  const prompt = await lead.emit("before_agent_start");
  assert.match(prompt.systemPrompt.join(""), /1\/2 done/);
});
test("unmatched worker identity without inherited ref", async () => {
  const { lead, spawn } = await fixture();
  const { worker, prompt } = await spawn(
    "unmatched",
    "Unrelated work with no plan match",
    { pauseAt: null },
  );
  assert.match(prompt.systemPrompt.join(""), /no.*task.*assigned/i);
  const event = await worker.emit("tool_call", {
    toolName: "bash",
    input: {
      command: 'printf "%s\\n%s\\n%s" "${KATA_REF-unset}" "$KATA_TEAMMATE" "$KATA_INBOX_USER"',
    },
  });
  const [ref, teammate, inbox] = (await runShell(event.input.command)).split("\n");
  assert.equal(ref, "unset");
  assert.ok(teammate && teammate !== "lead");
  assert.equal(inbox, `actor/${teammate}`);
  await lead.lifecycle({
    id: "unmatched",
    status: "completed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
});
test("terminal state remains latched after a partial settlement", async () => {
  const { lead, ref, show, spawn } = await fixture();
  const { worker } = await spawn("terminal-race");
  const sessionFile = worker.ctx.sessionManager.getSessionFile();
  failNextNotify = true;
  await lead.lifecycle({
    id: "terminal-race",
    status: "failed",
    sessionFile,
  });
  assert.equal((await show()).issue.metadata["omp.subagent_id"], "terminal-race");
  await lead.lifecycle({
    id: "terminal-race",
    status: "completed",
    sessionFile,
  });
  assert.equal((await show()).issue.metadata["omp.subagent_id"], undefined);
  const inbox = await f.kata(["inbox", "--for", "actor/lead", "--json"]);
  assert.match(JSON.stringify(inbox), /Subagent terminal-race failed/);
});
test("a partial terminal cleanup does not block the next delegate", async () => {
  const { lead, ref, show, spawn } = await fixture();
  const { worker } = await spawn("partial-cleanup");
  failClearTerminal = true;
  await lead.lifecycle({
    id: "partial-cleanup",
    status: "failed",
    sessionFile: worker.ctx.sessionManager.getSessionFile(),
  });
  const partial = (await show()).issue;
  assert.equal(partial.metadata["omp.subagent_id"], undefined);
  assert.equal(partial.metadata["omp.terminal_state"]?.id, "partial-cleanup");

  const { prompt } = await spawn("after-partial-cleanup");
  assert.match(prompt.systemPrompt.join(""), new RegExp(ref));
  const recovered = (await show()).issue;
  assert.equal(recovered.metadata["omp.terminal_state"], undefined);
  assert.equal(recovered.metadata["omp.subagent_id"], "after-partial-cleanup");
  assert.equal(recovered.metadata["omp.lead_actor"], "actor");
});
test("settlement stops after an unrelated notification revision", async () => {
  const { lead, ref, show, spawn } = await fixture();
  const { worker } = await spawn("revision-race");
  const sessionFile = worker.ctx.sessionManager.getSessionFile();
  const originalTeammate = (await show()).issue.metadata.teammate;
  afterNotify = () =>
    f.kata([
      "meta",
      "set",
      ref,
      "work.attention_msg",
      "concurrent update",
      "--json",
    ]);
  await lead.lifecycle({ id: "revision-race", status: "failed", sessionFile });
  const issue = (await show()).issue;
  assert.equal(issue.metadata["omp.subagent_id"], "revision-race");
  assert.equal(issue.metadata.teammate, originalTeammate);
  assert.equal(issue.metadata["work.attention_msg"], "concurrent update");
});
try {
  await runTests();
} finally {
  await f.close();
}
