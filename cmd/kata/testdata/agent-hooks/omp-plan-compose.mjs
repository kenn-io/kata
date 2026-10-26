import assert from "node:assert/strict";
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { planHost } from "./omp-plan-host.mjs";
const file = process.argv[2],
  factory = (await import(pathToFileURL(file))).default;
process.env.KATA_INBOX_USER = "actor/lead";
const entries = [
  {
    type: "custom",
    customType: "io.kata.omp.parent",
    data: { parent: "example-workspace#parent", workspace: process.cwd() },
  },
];
let lifecycle;
const events = {
  on(name, handler) {
    if (name === "task:subagent:lifecycle") lifecycle = handler;
  },
};
const h = planHost(file, { entries, events });
h.ctx.sessionManager.getSessionFile = () => path.resolve("omp-lead.jsonl");
factory(h);
await h.emit("session_start");
await h.call({ op: "view" });
let prompt = { systemPrompt: ["authored entry"] };
for (let i = 0; i < 2; i++) {
  prompt = await h.emit("before_agent_start", prompt);
  const text = prompt.systemPrompt.join("\n");
  for (const block of ["kata_contract", "kata_inbox", "kata_todo"])
    assert.equal(
      text.split("<" + block + ">").length - 1,
      1,
      block + " appears once after refresh",
    );
  assert.equal(
    prompt.systemPrompt.filter((p) => p === "authored entry").length,
    1,
  );
}
const inboxRecipients = () =>
  h.calls
    .filter((call) => call.args[0] === "inbox")
    .map((call) => call.args.at(-1));
assert.deepEqual(inboxRecipients(), ["actor/lead", "actor/lead"]);
h.ctx.agent.kind = "sub";
h.ctx.sessionManager.getSessionFile = () => path.resolve("omp-unbound.jsonl");
await h.emit("before_agent_start", { systemPrompt: [] });
assert.deepEqual(inboxRecipients(), ["actor/lead", "actor/lead"]);
h.ctx.agent.kind = "main";
h.ctx.sessionManager.getSessionFile = () => path.resolve("omp-lead.jsonl");
await h.emit("tool_call", {
  toolName: "task",
  toolCallId: "worker-assignment",
  input: { task: "Current task" },
});
assert.ok(lifecycle);
const workerFile = "omp-worker.jsonl";
await lifecycle({
  id: "worker",
  status: "started",
  parentToolCallId: "worker-assignment",
  description: "Current task",
  sessionFile: workerFile,
});
h.ctx.agent.kind = "sub";
h.ctx.sessionManager.getSessionFile = () => workerFile;
h.ctx.cwd = path.resolve("omp-restored-worker");
const workerCommand = await h.emit("tool_call", {
  toolName: "bash",
  input: { command: "true" },
});
const workerRecipient = /KATA_INBOX_USER='([^']+)'/.exec(
  workerCommand.input.command,
)?.[1];
assert.ok(workerRecipient?.startsWith("actor/worker-"));
await h.emit("before_agent_start", { systemPrompt: [] });
assert.equal(inboxRecipients().at(-1), workerRecipient);
assert.equal(
  h.calls.filter((call) => call.args[0] === "inbox").at(-1).options.cwd,
  process.cwd(),
  "restored worker inbox reads use the saved assignment workspace",
);
h.ctx.agent.kind = "main";
h.ctx.sessionManager.getSessionFile = () => path.resolve("omp-lead.jsonl");
// A project contract-only adapter beside the user plan adapter owns only its
// base capability. It must not replace the user's plan generation.
const project = file + ".project.mjs";
let source = readFileSync(file, "utf8");
source = source.replace(
  /const options = [^\n]+;/,
  `const options = ${JSON.stringify({ executable: "kata", scope: "project", workspace: process.cwd(), contract: true, plan: false })};`,
);
writeFileSync(project, source);
(await import(pathToFileURL(project))).default(h);
const result = await h.emit("before_agent_start", prompt);
const text = result.systemPrompt.join("\n");
assert.equal(text.split("<kata_contract>").length - 1, 1);
assert.equal(text.split("<kata_todo>").length - 1, 1);
assert.match(
  h.widgets.at(-1)[1][0],
  /Kata example-workspace#parent · 0\/1 done · ▶ Current task/,
);
console.log("PASS prompt block composition, refresh and capability precedence");
