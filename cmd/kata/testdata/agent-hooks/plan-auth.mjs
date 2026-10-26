import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { loadCore } from "./plan-test-utils.mjs";
const { createKataCore } = await loadCore();
const [binary, ref, cwd] = process.argv.slice(3);
const exec = promisify(execFile);
const core = createKataCore(
  {
    appendEntry() {},
    exec: async (f, args, opts) => {
      try {
        return {
          ...(await exec(f, args, { ...opts, env: process.env })),
          code: 0,
        };
      } catch (e) {
        return {
          code: 1,
          stdout: e.stdout || "",
          stderr: e.stderr || e.message,
        };
      }
    },
  },
  { namespace: "omp", executable: binary, minDaemonAPI: "0.15.0" },
);
const ctx = {
  cwd,
  agent: { kind: "main" },
  sessionManager: {
    getSessionId: () => "authenticated-main",
    getBranch: () => [],
  },
};
if (process.argv[6] === "claim") {
  const issue = await core.owned(ref, cwd, true);
  assert.equal(issue.owner, "daemon-actor");
  assert.equal(await core.effectiveActor(ref, cwd), "daemon-actor");
  console.log("PASS real federated claim ownership");
} else {
  const result = await core.execute(
    { op: "init", ref, items: ["First authenticated task"] },
    ctx,
  );
  assert.equal(result.isError, undefined, result.content[0].text);
  assert.equal(await core.effectiveActor(ref, cwd), "daemon-actor");
  const child = core.plan.tasks[0];
  assert.equal(child.status, "in_progress");
  const issue = JSON.parse(
    (
      await exec(binary, ["show", child.ref, "--json"], {
        cwd,
        env: process.env,
      })
    ).stdout,
  ).issue;
  assert.equal(issue.owner, "daemon-actor");
  console.log("PASS real daemon actor ownership");
}
