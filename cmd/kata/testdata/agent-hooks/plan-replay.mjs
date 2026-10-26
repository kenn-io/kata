import assert from "node:assert/strict";
import { loadCore } from "./plan-test-utils.mjs";
const { createKataCore, digest } = await loadCore();
const replayIdentity = `omp:${digest("example-workspace#parent", "Tasks", "Restored task")}`;
function fixture({ closed, foreign, removed = true, race }) {
  const child = {
    short_id: "child",
    qualified_id: "example-workspace#child",
    uid: "child-uid",
    project_id: 1,
    title: "Restored task",
    owner: foreign === "actor" ? "other-actor" : "actor",
    status: closed ? "closed" : "open",
    revision: 10,
    metadata: {
      "todo.phase": "Tasks",
      "todo.status": closed ? "completed" : "pending",
      "todo.identity": replayIdentity,
    },
  };
  if (removed) child.metadata["todo.removed"] = true;
  if (closed) {
    child.closed_reason = "done";
    child.closed_at = "2026-01-01T00:00:00Z";
  }
  if (foreign === "teammate") child.metadata.teammate = "other-worker";
  if (foreign === "delegate") child.metadata["omp.subagent_id"] = "worker";
  const parent = {
    short_id: "parent",
    project_id: 1,
    status: "open",
    revision: 1,
    metadata: {},
  };
  const calls = [],
    applied = [];
  let created = false,
    injected = false,
    expectedFailure;
  function inject(kind) {
    if (injected) return;
    injected = true;
    if (kind === "owner") child.owner = "other-actor";
    else child.metadata["external.note"] = "concurrent update";
    child.revision++;
    expectedFailure = structuredClone(child);
  }
  const core = createKataCore(
    {
      appendEntry() {},
      async exec(_exe, args) {
        calls.push(args);
        const ok = (value) => ({ code: 0, stdout: JSON.stringify(value) });
        if (args[0] === "health") return ok({ api_schema_version: "0.15.0" });
        if (args[0] === "status") return ok({ actor: "actor" });
        if (args[0] === "show") {
          if (args[1].endsWith("#child")) {
            if (created && !injected && race?.startsWith("preflight-"))
              inject(race.slice(10));
            if (
              !injected &&
              race?.startsWith("after-status-") &&
              applied.some((a) => a[0] === "meta" && a[3] === "todo.status")
            )
              inject(race.slice(13));
            return ok({ issue: structuredClone(child) });
          }
          return ok({
            issue: parent,
            // Another session's create commits after this identity lookup;
            // the stable CLI idempotency key must return that existing child.
            children: created ? [structuredClone(child)] : [],
          });
        }
        if (args[0] === "create") {
          created = true;
          return ok({
            issue: structuredClone(child),
            reused: true,
            event: { project_name: "example-workspace" },
          });
        }
        if (args[0] === "reopen") {
          if (race?.startsWith("reopen-")) inject(race.slice(7));
          child.status = "open";
          delete child.closed_reason;
          delete child.closed_at;
          child.revision++;
          applied.push(args);
          if (injected) expectedFailure = structuredClone(child);
          return ok({ issue: child });
        }
        if (args[0] === "meta") {
          if (
            !injected &&
            race?.startsWith("status-") &&
            args[3] === "todo.status"
          )
            inject(race.slice(7));
          if (
            !injected &&
            race?.startsWith("removal-") &&
            args[3] === "todo.removed"
          )
            inject(race.slice(8));
          if (
            args.includes("--if-match") &&
            Number(args[args.indexOf("--if-match") + 1]) !== child.revision
          )
            return { code: 1, stdout: "", stderr: "revision conflict" };
          if (args.includes("--if-value"))
            assert.equal(
              JSON.stringify(child.metadata[args[3]]),
              args[args.indexOf("--if-value") + 1],
            );
          if (args[1] === "set")
            child.metadata[args[3]] = args.includes("--json-value")
              ? JSON.parse(args[4])
              : args[4];
          else delete child.metadata[args[3]];
          child.revision++;
          applied.push(args);
          return ok({ issue: child });
        }
        throw Error("unexpected command " + JSON.stringify(args));
      },
    },
    { namespace: "omp", executable: "kata", minDaemonAPI: "0.15.0" },
  );
  const ctx = {
    cwd: process.cwd(),
    agent: { kind: "main" },
    sessionManager: {
      getSessionId: () => "replay",
      getBranch: () => [
        {
          type: "custom",
          customType: "io.kata.omp.parent",
          data: {
            parent: "example-workspace#parent",
            workspace: process.cwd(),
          },
        },
      ],
    },
  };
  return {
    child,
    core,
    ctx,
    calls,
    applied,
    get expectedFailure() {
      return expectedFailure;
    },
    get injected() {
      return injected;
    },
  };
}
const params = (op) =>
  op === "init"
    ? { op, list: [{ phase: "Tasks", items: ["Restored task"] }] }
    : { op, phase: "Tasks", items: ["Restored task"] };
const tests = [];
const test = (name, fn) => tests.push({ name, fn });
for (const op of ["append", "init"])
  for (const closed of [true, false])
    for (const foreign of ["actor", "teammate", "delegate"])
      test(`${op} ${closed ? "closed" : "open"} removed replay refuses foreign ${foreign} before restoration`, async () => {
        const f = fixture({ closed, foreign });
        await f.core.bind(f.ctx);
        const before = structuredClone(f.child);
        const result = await f.core.execute(params(op), f.ctx);
        assert.deepEqual(
          f.child,
          before,
          "foreign issue/status/removal remain exactly unchanged",
        );
        assert.equal(result.isError, true);
        assert.deepEqual(f.applied, [], "no restoration mutation");
        assert.ok(
          f.calls.some((args) => args[0] === "create"),
          "exercise actual idempotent replay path",
        );
      });
for (const op of ["append", "init"])
  for (const closed of [true, false])
    test(`${op} ${closed ? "closed" : "open"} owned replay restores the exact task`, async () => {
      const f = fixture({ closed });
      await f.core.bind(f.ctx);
      const result = await f.core.execute(params(op), f.ctx);
      assert.equal(result.isError, undefined, result.content[0].text);
      assert.deepEqual(f.child, {
        short_id: "child",
        qualified_id: "example-workspace#child",
        uid: "child-uid",
        project_id: 1,
        title: "Restored task",
        owner: "actor",
        status: "open",
        revision: closed ? 16 : 14,
        metadata: {
          "todo.phase": "Tasks",
          "todo.status": "in_progress",
          "todo.identity": replayIdentity,
          "todo.order": 0,
          "work.attention": "ok",
        },
      });
      for (const args of f.applied.filter((args) => args[0] === "meta"))
        assert.ok(
          args.includes("--if-match"),
          "restoration/advance metadata is conditional",
        );
    });
for (const foreign of ["actor", "teammate", "delegate"])
  test(`append closed nonremoved replay refuses foreign ${foreign}`, async () => {
    const f = fixture({ closed: true, foreign, removed: false });
    await f.core.bind(f.ctx);
    const before = structuredClone(f.child);
    const result = await f.core.execute(params("append"), f.ctx);
    assert.deepEqual(f.child, before);
    assert.equal(result.isError, true);
    assert.deepEqual(f.applied, []);
  });
test("append closed nonremoved owned replay restores exactly", async () => {
  const f = fixture({ closed: true, removed: false });
  await f.core.bind(f.ctx);
  const result = await f.core.execute(params("append"), f.ctx);
  assert.equal(result.isError, undefined, result.content[0].text);
  assert.deepEqual(f.child, {
    short_id: "child",
    qualified_id: "example-workspace#child",
    uid: "child-uid",
    project_id: 1,
    title: "Restored task",
    owner: "actor",
    status: "open",
    revision: 15,
    metadata: {
      "todo.phase": "Tasks",
      "todo.status": "in_progress",
      "todo.identity": replayIdentity,
      "todo.order": 0,
      "work.attention": "ok",
    },
  });
});
for (const op of ["append", "init"])
  for (const closed of [true, false]) {
    const races = [
      "preflight-owner",
      "preflight-revision",
      "removal-owner",
      "removal-revision",
    ];
    if (closed)
      races.push(
        "reopen-owner",
        "reopen-revision",
        "status-owner",
        "status-revision",
        "after-status-owner",
        "after-status-revision",
      );
    for (const race of races)
      test(`${op} ${closed ? "closed" : "open"} replay ${race} preserves the exact partial outcome and recovers`, async () => {
        const f = fixture({ closed, race });
        await f.core.bind(f.ctx);
        const result = await f.core.execute(params(op), f.ctx);
        assert.equal(f.injected, true, "race boundary reached");
        assert.equal(result.isError, true, result.content[0].text);
        assert.deepEqual(
          f.child,
          f.expectedFailure,
          "no mutation follows the ownership/revision conflict",
        );
        assert.equal(
          f.child.metadata["todo.removed"],
          true,
          "removal marker survives the failed restoration",
        );
        if (race.startsWith("preflight-"))
          assert.deepEqual(f.applied, [], "preflight failure never reopens");
        if (race.startsWith("reopen-")) {
          assert.equal(
            f.child.status,
            "open",
            "unconditional reopen may complete across a concurrent handoff",
          );
          assert.equal(f.child.metadata["todo.status"], "completed");
          assert.deepEqual(
            f.applied.map((args) => args[0]),
            ["reopen"],
            "no status/removal write after the reopen race",
          );
        }
        const recoveryBefore = structuredClone(f.child);
        f.child.owner = "actor";
        f.child.revision++;
        const recovered = await f.core.execute(params(op), f.ctx);
        assert.equal(recovered.isError, undefined, recovered.content[0].text);
        const extra =
          f.child.status === "open" && recoveryBefore.status === "closed"
            ? 5
            : 3;
        const expected = {
          ...recoveryBefore,
          owner: "actor",
          status: "open",
          revision: recoveryBefore.revision + 1 + extra + 1,
          metadata: {
            ...recoveryBefore.metadata,
            "todo.status": "in_progress",
            "todo.order": 0,
            "work.attention": "ok",
          },
        };
        delete expected.metadata["todo.removed"];
        delete expected.closed_reason;
        delete expected.closed_at;
        assert.deepEqual(
          f.child,
          expected,
          "fresh owned retry restores exactly once without losing concurrent metadata",
        );
      });
  }
for (const { name, fn } of tests) {
  if (process.argv[3] && !name.includes(process.argv[3])) continue;
  await fn();
  console.log("PASS " + name);
}
