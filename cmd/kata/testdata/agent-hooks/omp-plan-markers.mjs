import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import { planHost } from "./omp-plan-host.mjs";
const file = process.argv[2],
  factory = (await import(pathToFileURL(file))).default;
const h = planHost(file, {
  sessionId: "main-session",
  entries: [
    {
      type: "custom",
      customType: "io.kata.omp.parent",
      data: { parent: "example-workspace#parent", workspace: process.cwd() },
    },
  ],
});
factory(h);
await h.emit("session_start");
const result = await h.call({
  op: "done",
  task: "Current task",
  message:
    "Completed the requested fixture and verified its subprocess behavior.",
  evidence: ["test:fixture"],
});
assert.equal(result.isError, undefined, result.content[0].text);
const calls = readFileSync(process.env.KATA_NATIVE_PLAN_CALLS, "utf8")
  .trim()
  .split("\n")
  .map((line) => JSON.parse(line));
const close = calls.find((call) => call.args[0] === "close");
assert.ok(close, "real execFile child recorded close");
assert.equal(close.env.OMPCODE, "1");
assert.equal(close.env.PI_SESSION_ID, "main-session");
console.log("PASS subprocess close markers");
