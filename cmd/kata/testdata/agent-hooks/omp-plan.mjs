import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import { createOmpHost } from "./omp-host.mjs";
const file = process.argv[2],
  factory = (await import(pathToFileURL(file))).default;
const schema = new Proxy(() => schema, { get: () => schema });
function host(events = {}) {
  const h = createOmpHost({ cwd: process.cwd(), events });
  h.zod = schema;
  const widgets = [],
    entries = [];
  h.ctx.ui.setWidget = (...a) => widgets.push(a);
  h.appendEntry = (...a) => entries.push(a);
  h.getSessionName = () => "Example plan";
  h.ctx.sessionManager.getBranch = () => [];
  const register = h.registerTool;
  h.registerTool = (t) => register({ ...t, sourceInfo: { path: file } });
  return Object.assign(h, {
    widgets,
    entries,
    call: (params, signal) =>
      h.tools
        .find((t) => t.name === "todo")
        .execute("test", params, signal, undefined, h.ctx),
  });
}
const h = host();
factory(h);
assert.ok(
  h.tools.find((t) => t.name === "todo"),
  "plan-enabled extension registers todo",
);
await h.emit("session_start");
const init = await h.call({ op: "init", items: ["First", "Second"] });
assert.equal(init.isError, undefined, init.content[0].text);
assert.match(init.content[0].text, /Session-local/);
const canceled = new AbortController();
canceled.abort(new Error("todo operation canceled"));
const canceledAppend = await h.call(
  { op: "append", items: ["Canceled append"] },
  canceled.signal,
);
assert.equal(canceledAppend.isError, true);
assert.doesNotMatch(canceledAppend.content[0].text, /Canceled append/);
assert.equal(
  (
    await h.emit("input", {
      text: "/todo block awaiting review",
      source: "interactive",
    })
  ).handled,
  true,
);
assert.match(
  (await h.call({ op: "view" })).content[0].text,
  /blocked: awaiting review/,
);
assert.equal((await h.call({ op: "done", task: "Second" })).isError, undefined);
assert.equal(
  (
    await h.emit("session_stop", {
      messages: [{ role: "assistant", content: "Please confirm?" }],
    })
  ).continue,
  undefined,
);
for (let i = 0; i < 4; i++) {
  await h.emit("tool_result", { toolName: "read" });
  const r = await h.emit("session_stop");
  assert.equal(!!r.continue, false, "blocked work suppresses reminders");
}
await h.call({ op: "append", items: ["Continue after review"] });
assert.equal(
  (
    await h.emit("session_stop", {
      messages: [{ role: "assistant", content: "Should we pause?" }],
    })
  ).continue,
  undefined,
);
for (let i = 0; i < 4; i++) {
  await h.emit("tool_result", { toolName: "read" });
  const result = await h.emit("session_stop");
  assert.equal(!!result.continue, i < 3, "stop reminders stop after three");
}
const subprocesses = () =>
  process.env.KATA_NATIVE_PLAN_CALLS
    ? readFileSync(process.env.KATA_NATIVE_PLAN_CALLS, "utf8")
    : "";
const native = host();
factory(native);
native.ctx.invokeTool = async (p) => ({ forwarded: p });
await native.emit("session_start");
const before = subprocesses();
assert.deepEqual(
  await native.call({
    op: "done",
    task: "First",
    ref: "x",
    message: "private",
    evidence: ["test:x"],
    reason: "wait",
  }),
  { forwarded: { op: "done", task: "First" } },
);
for (const event of [
  "input",
  "tool_call",
  "tool_result",
  "session_stop",
  "before_agent_start",
  "session_shutdown",
])
  await native.emit(event, { text: "/todo" });
assert.equal(subprocesses(), before);
assert.equal(
  native.notifications.filter((n) =>
    n.message.startsWith("Kata plan inactive:"),
  ).length,
  1,
);
const beforeOther = subprocesses();
const other = host();
factory(other);
other.tools[0].sourceInfo.path = "/another/todo.js";
await other.emit("session_start");
await other.emit("input", { text: "/todo" });
assert.equal(subprocesses(), beforeOther);
console.log("PASS todo operations, native forwarding and foreign winner");
