import assert from "node:assert/strict";
import { createInterface } from "node:readline";
import { loadCore } from "./plan-test-utils.mjs";
const { createKataCore, classifyTask, kataEnv } = await loadCore();
const mode = process.argv[3] || "identity";
async function* batches() {
  if (process.argv[4] === "stream") {
    for await (const line of createInterface({ input: process.stdin }))
      yield JSON.parse(line);
  } else {
    let text = "";
    for await (const chunk of process.stdin) text += chunk;
    yield text ? JSON.parse(text) : undefined;
  }
}
function app(exec, entries = [], hostOptions = {}) {
  const calls = [];
  const ctx = {
    cwd: process.cwd(),
    agent: { kind: "main" },
    sessionManager: { getSessionId: () => "main", getBranch: () => entries },
  };
  const core = createKataCore(
    {
      exec: async (f, a, o) => {
        calls.push(a);
        return {
          code: 0,
          stdout: JSON.stringify(await exec(a)),
          killed: false,
        };
      },
      appendEntry() {},
      beforeReconcile: hostOptions.beforeReconcile,
    },
    { namespace: "omp", executable: "kata", minDaemonAPI: "0.15.0" },
  );
  return { core, ctx, calls };
}
if (mode === "identity") {
  const a = app((a) =>
    a[0] === "list"
      ? { issues: [] }
      : a[0] === "health"
        ? { api_schema_version: "0.14.0" }
        : {},
  );
  const r = await a.core.execute({ op: "init", items: ["First"] }, a.ctx);
  assert.equal(r.isError, true);
  assert.match(
    r.content[0].text,
    /Kata daemon 0.14.0 is too old for the plan layer \(needs 0.15.0\)/,
  );
  assert.equal(
    a.calls.some((a) => ["create", "claim", "meta", "close"].includes(a[0])),
    false,
  );
  await assert.rejects(() => a.core.checkDaemonFloor(a.ctx.cwd), {
    message: "Kata daemon 0.14.0 is too old for the plan layer (needs 0.15.0)",
  });
  const b = app((a) => {
    if (a[0] === "status") throw Error("status offline");
    return a[0] === "health" ? { api_schema_version: "0.15.0" } : { issues: [] };
  });
  const unavailable = await b.core.execute({ op: "view" }, b.ctx);
  assert.match(
    unavailable.content[0].text,
    /Kata state unavailable.*status offline/,
  );
  assert.equal(
    b.calls.some((a) => ["create", "claim", "meta", "close"].includes(a[0])),
    false,
  );
  await assert.rejects(
    b.core.effectiveActor("example-workspace#abc4", b.ctx.cwd),
    /status offline/,
  );
  const c = app((a) => {
    if (a[0] === "status") return { actor: "daemon-actor" };
    return {};
  });
  process.env.KATA_AUTHOR = "local-actor";
  assert.equal(
    await c.core.effectiveActor(undefined, c.ctx.cwd),
    "daemon-actor",
  );
  assert.equal(await c.core.effectiveActor("x", c.ctx.cwd), "daemon-actor");
  assert.equal(c.calls.length, 1);
  process.env.KATA_SERVER = "https://daemon.example";
  await c.core.effectiveActor("x", c.ctx.cwd);
  assert.equal(c.calls.length, 2);
  await c.core.effectiveActor("x", c.ctx.cwd + "/other");
  assert.equal(c.calls.length, 3);
  delete process.env.KATA_SERVER;
  assert.equal(
    kataEnv({ OMPCODE: "1", PI_SESSION_ID: "main" }).PI_SESSION_ID,
    "main",
  );
  console.log("PASS effective actor, cache selection and daemon floor");
}
if (mode === "classification") {
  for await (const rows of batches())
    console.log(
      JSON.stringify(
        rows.map((r) => ({
          classification: classifyTask(
            { metadata: r.metadata },
            { namespace: "omp", harness: "omp", leadTeammate: r.lead },
          ),
        })),
      ),
    );
}
if (mode === "parent") {
  for await (const batch of batches()) {
    const rows = batch || [
      { proofs: [["test:a"], ["commit:b"]], reset: false },
      { proofs: [["test:a"], []], wontfix: true },
      { proofs: [["test:a"]], reset: true },
      { proofs: [["test:handoff"]], ownerRace: true },
      { proofs: [["test:handoff"]], teammateRace: true },
      { proofs: [["test:old"]], late: true },
      { proofs: [["test:cross"]], cross: true },
    ];
    const output = [];
    for (const row of rows) {
      if (row.histories)
        row.proofs = row.histories.map((history) => history.at(-1));
      const historyEvents = [];
      (row.histories || row.proofs.map((proof) => [proof])).forEach(
        (history, child) =>
          history.forEach((proof, index) =>
            historyEvents.push({
              id: historyEvents.length + 1,
              child,
              proof,
              index,
            }),
          ),
      );
      const latest = (child) =>
        historyEvents.filter((event) => event.child === child).at(-1);
      const closedAt = (event) =>
        new Date(Date.UTC(2026, 0, 1) + event.id * 1000).toISOString();
      const children = row.proofs.map((proof, i) => ({
        uid: "uid" + i,
        short_id: "c" + i,
        project_id: row.cross ? 2 : 1,
        status: "closed",
        revision: 2 + latest(i).index * 2,
        closed_at: closedAt(latest(i)),
        closed_reason: row.wontfix && i === 1 ? "wontfix" : "done",
        title: "Child " + i,
      }));
      const issue = {
        status: "open",
        revision: 1,
        owner: "test-actor",
        project_id: 1,
        metadata: { "omp.auto_parent": true, "omp.session_id": "main" },
      };
      let finalReads = 0,
        closed,
        review = false,
        handedOff = false;
      const a = app(
        (args) => {
          if (args[0] === "show") {
            finalReads++;
            return { issue, children };
          }
          if (args[0] === "status") return { actor: "test-actor" };
          if (args[0] === "health") return { api_schema_version: "0.15.0" };
          if (args[0] === "projects")
            return { projects: [{ id: 2, name: "other-project" }] };
          if (args[0] === "audit")
            return {
              rows: historyEvents
                .map((event) => ({
                  issue: children[event.child].short_id,
                  event_id: event.id,
                }))
                .reverse(),
            };
          if (args[0] === "events") {
            const event = historyEvents.find(
              (event) =>
                event.id === Number(args[args.indexOf("--after") + 1]) + 1,
            );
            return {
              reset_required: row.reset,
              events: [
                {
                  event_id: event.id,
                  type: "issue.closed",
                  issue_uid: "uid" + event.child,
                  payload: {
                    closed_at: closedAt(event),
                    evidence: event.proof.map((proof) => {
                      const [type, ...value] = proof.split(":");
                      return {
                        type,
                        [type === "test" ? "command" : "sha"]: value.join(":"),
                      };
                    }),
                  },
                },
              ],
            };
          }
          if (args[0] === "close") {
            if (row.late) {
              assert.ok(
                finalReads >= 4,
                "race runs after the final parent/child read",
              );
              children[0].status = "open";
              children[0].revision++;
              const event = {
                id: historyEvents.length + 1,
                child: 0,
                proof: ["test:new"],
                index: latest(0).index + 1,
              };
              historyEvents.push(event);
              children[0].status = "closed";
              children[0].revision++;
              children[0].closed_at = closedAt(event);
              row.proofs[0] = event.proof;
            }
            closed = args;
            issue.status = "closed";
            return {};
          }
          if (args[0] === "label") {
            review = true;
            return {};
          }
          return {};
        },
        [
          {
            type: "custom",
            customType: "io.kata.omp.parent",
            data: {
              parent: "example-workspace#parent",
              workspace: process.cwd(),
              auto: true,
              sessionId: "main",
            },
          },
        ],
        {
          beforeReconcile() {
            if (handedOff) return;
            if (row.ownerRace) issue.owner = "other-actor";
            if (row.teammateRace) issue.metadata.teammate = "other.worker";
            if (row.ownerRace || row.teammateRace) {
              issue.revision++;
              handedOff = true;
            }
          },
        },
      );
      await a.core.bind(a.ctx);
      await a.core.reconcile();
      const proof =
        closed?.flatMap((v, i) =>
          v === "--evidence" ? [closed[i + 1]] : [],
        ) || [];
      if (row.reset) {
        assert.equal(closed, undefined);
        assert.equal(review, true);
      } else if (row.ownerRace || row.teammateRace) {
        assert.equal(handedOff, true);
        assert.equal(closed, undefined);
      } else {
        const expected = row.late
          ? ["test:old"]
          : row.proofs.flatMap((p, i) => (row.wontfix && i === 1 ? [] : p));
        assert.deepEqual(new Set(proof), new Set(expected));
        assert.equal(
          closed[closed.indexOf("--message") + 1],
          `OMP session main: all ${children.length} child issues are closed in Kata and delegated work has settled. Evidence is copied from the children's close events; each child's own close remains authoritative.`,
        );
      }
      output.push(proof);
    }
    console.log(JSON.stringify(output));
  }
}
