import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, symlinkSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { loadCore } from "./plan-test-utils.mjs";
const {
  createKataCore,
  digest,
  sessionKey,
  kataCommands,
  shellOptions,
  phasesOf,
  render,
} = await loadCore();
assert.equal(digest("abc"), "ba7816bf8f01cfea414140de");
assert.notEqual(
  digest("ab", "c"),
  digest("a", "bc"),
  "part boundaries remain distinct",
);
const args = shellOptions(
  kataCommands(
    `env X=y kata --workspace 'some path' claim abc4 --teammate=worker -- --literal`,
  )[0].words,
);
assert.deepEqual(args.args, ["claim", "abc4", "--literal"]);
assert.equal(args.value("--workspace"), "some path");
assert.equal(args.value("--teammate"), "worker");
const root = mkdtempSync(path.join(tmpdir(), "kata-plan-path-")),
  target = path.join(root, "target");
mkdirSync(target);
assert.equal(
  sessionKey("missing.jsonl", target),
  path.join(realpathSync(target), "missing.jsonl"),
);
try {
  const alias = path.join(root, "alias");
  symlinkSync(target, alias, "junction");
  assert.equal(
    sessionKey("missing.jsonl", alias),
    sessionKey("missing.jsonl", target),
  );
} catch (error) {
  if (!["EPERM", "EACCES"].includes(error.code)) throw error;
}
const calls = [];
const core = createKataCore(
  {
    appendEntry() {},
    async exec(_f, args) {
      calls.push(args);
      return { code: 1, stdout: "", stderr: "project_not_initialized" };
    },
  },
  { namespace: "omp", executable: "kata", minDaemonAPI: "0.15.0" },
);
const ctx = {
  cwd: root,
  sessionManager: { getSessionId: () => "local-ops", getBranch: () => [] },
};
const call = async (params) => {
  const r = await core.execute(params, ctx);
  assert.equal(r.isError, undefined, r.content[0].text);
  return r;
};
await call({
  op: "init",
  list: [
    { phase: "Build", items: ["First", "Second"] },
    { phase: "Verify", items: ["Third"] },
  ],
});
assert.deepEqual(
  core.plan.tasks.map((t) => t.status),
  ["in_progress", "pending", "pending"],
);
await call({ op: "block", reason: "Review pending" });
assert.deepEqual(
  core.plan.tasks.map((t) => t.status),
  ["blocked", "in_progress", "pending"],
);
await call({ op: "unblock", task: "First" });
await call({ op: "start", task: "First" });
assert.deepEqual(
  core.plan.tasks.map((t) => t.status),
  ["in_progress", "pending", "pending"],
);
await call({ op: "done", phase: "Build" });
assert.deepEqual(
  core.plan.tasks.map((t) => t.status),
  ["completed", "completed", "in_progress"],
);
await call({ op: "append", phase: "Verify", items: ["Fourth"] });
await call({ op: "drop", task: "Third", message: "retired" });
await call({ op: "rm", task: "Fourth", message: "removed" });
assert.deepEqual(
  core.plan.tasks.map((t) => t.status),
  ["completed", "completed", "abandoned"],
);
assert.deepEqual(
  phasesOf(core.plan.tasks).map((p) => p.name),
  ["Build", "Verify"],
);
assert.match(render(core.plan), /Session-local plan: 2\/3 done/);
assert.deepEqual(
  calls.map((args) => args[0]),
  ["list"],
  "local plans never check daemon health or mutate Kata",
);
console.log("PASS pure helpers, phase order and all local todo operations");
