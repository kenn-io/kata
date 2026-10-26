import assert from "node:assert/strict";
import { createInterface } from "node:readline";
import { loadCore } from "./plan-test-utils.mjs";
const { createKataCore } = await loadCore();
function fixture({ foreign = false, active = false, race = false } = {}) {
  const issue = (id, owner, status) => ({
    short_id: id,
    qualified_id: "example-workspace#" + id,
    uid: id,
    project_id: 1,
    title: id,
    status: "open",
    owner,
    revision: 1,
    metadata: { "todo.phase": "Tasks", "todo.status": status },
  });
  const child = issue(
    "child",
    foreign ? "other-actor" : "actor",
    active ? "in_progress" : "pending",
  );
  const next = issue("next", "actor", "pending");
  const parent = { ...issue("parent", "actor", "pending"), metadata: {} };
  const calls = [];
  let injected = false;
  const host = {
    appendEntry() {},
    async exec(_file, args) {
      calls.push(args);
      const op = args[0],
        ref = args[1],
        key = ref?.split("#").pop();
      const selected = key === "child" ? child : key === "next" ? next : parent;
      const response = (value) => ({ code: 0, stdout: JSON.stringify(value) });
      if (op === "health")
        return response({
          api_schema_version: process.env.KATA_SERVER ? "0.6.0" : "0.15.0",
        });
      if (op === "status") return response({ actor: "actor" });
      if (op === "show")
        return response({
          issue: structuredClone(selected),
          children:
            selected === parent
              ? [structuredClone(child), structuredClone(next)]
              : [],
        });
      if (op === "meta") {
        const target = args[2].split("#").pop() === "child" ? child : next;
        if (race && !injected && args[3] === "todo.status") {
          injected = true;
          target.owner = "other-actor";
          target.revision++;
        }
        if (
          args.includes("--if-match") &&
          Number(args[args.indexOf("--if-match") + 1]) !== target.revision
        )
          return { code: 1, stdout: "", stderr: "revision conflict" };
        if (args[1] === "set")
          target.metadata[args[3]] = args.includes("--json-value")
            ? JSON.parse(args[4])
            : args[4];
        else delete target.metadata[args[3]];
        target.revision++;
        return response({});
      }
      if (op === "close") {
        if (
          args.includes("--if-match") &&
          Number(args[args.indexOf("--if-match") + 1]) !== selected.revision
        )
          return { code: 1, stdout: "", stderr: "revision conflict" };
        selected.status = "closed";
        selected.revision++;
        return response({});
      }
      throw Error("Unexpected command " + args.join(" "));
    },
  };
  const core = createKataCore(host, {
    namespace: "omp",
    executable: "kata",
    minDaemonAPI: "0.15.0",
  });
  const ctx = {
    cwd: process.cwd(),
    agent: { kind: "main" },
    sessionManager: {
      getSessionId: () => "safety",
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
  return { core, ctx, calls, child, next };
}
if (process.argv[3] !== "stream") {
  for (const op of ["done", "drop", "rm"]) {
    const f = fixture({ foreign: true });
    await f.core.bind(f.ctx);
    const result = await f.core.execute(
      {
        op,
        task: "child",
        message:
          "Checked the requested work with real independent verification.",
        evidence: ["test:ownership"],
      },
      f.ctx,
    );
    assert.equal(result.isError, true, op + " refuses a foreign owner");
    assert.equal(f.child.status, "open");
    assert.equal(
      f.calls.some((a) => ["meta", "close"].includes(a[0])),
      false,
    );
  }
  {
    const f = fixture({ foreign: true, active: true });
    await f.core.bind(f.ctx);
    assert.equal(
      (await f.core.execute({ op: "start", task: "next" }, f.ctx)).isError,
      true,
      "starting own task cannot demote a foreign active task",
    );
    assert.equal(f.child.metadata["todo.status"], "in_progress");
  }
  {
    const f = fixture({ race: true });
    await f.core.bind(f.ctx);
    assert.equal(
      (await f.core.execute({ op: "start", task: "child" }, f.ctx)).isError,
      true,
      "final status write rejects an ownership revision race",
    );
    assert.equal(f.child.metadata["todo.status"], "pending");
  }
  {
    const f = fixture();
    await f.core.bind(f.ctx);
    process.env.KATA_SERVER = "https://other-daemon.example";
    const before = f.calls.length;
    const result = await f.core.execute({ op: "start", task: "child" }, f.ctx);
    delete process.env.KATA_SERVER;
    assert.equal(result.isError, true, "daemon switch rechecks the floor");
    assert.equal(
      f.calls
        .slice(before)
        .some((a) => ["meta", "claim", "close"].includes(a[0])),
      false,
    );
  }
  {
    const f = fixture();
    f.child.metadata.teammate = "omp.model";
    await f.core.bind(f.ctx);
    assert.equal(
      (await f.core.execute({ op: "start", task: "child" }, f.ctx)).isError,
      undefined,
      "inferred harness lead teammate remains writable",
    );
  }
  {
    let active = true;
    const calls = [];
    const core = createKataCore(
      {
        current: () => active,
        appendEntry() {},
        async exec(_f, args) {
          calls.push(args);
          return {
            code: 0,
            get stdout() {
              if (args[0] === "health") queueMicrotask(() => (active = false));
              return JSON.stringify({ api_schema_version: "0.15.0" });
            },
          };
        },
      },
      { namespace: "omp", executable: "kata", minDaemonAPI: "0.15.0" },
    );
    await assert.rejects(
      () =>
        core.kata(
          [
            "meta",
            "set",
            "example-workspace#child",
            "todo.status",
            "in_progress",
          ],
          process.cwd(),
        ),
      /inactive/,
    );
    assert.equal(
      calls.some((args) => args[0] === "meta"),
      false,
      "generation loss during the floor handshake prevents the write",
    );
  }
  console.log(
    "PASS ownership at every transition, guarded final status, daemon switch and parser boundaries",
  );
} else {
  for await (const line of createInterface({ input: process.stdin })) {
    const row = JSON.parse(line),
      canRace = ["start", "block", "unblock", "demote"].includes(row.op);
    const f = fixture({
      foreign: !row.owned,
      active: row.op !== "start",
      race: row.race && canRace,
    });
    if (row.op === "unblock") f.child.metadata["todo.status"] = "blocked";
    f.child.metadata["example.note"] = row.note;
    // The sibling belongs to this lead; only the selected child varies.
    f.next.metadata.teammate = row.lead;
    f.child.metadata.teammate = row.teammate;
    if (row.delegate) f.child.metadata["omp.subagent_id"] = "worker";
    if (row.lead) process.env.KATA_TEAMMATE = row.lead;
    else delete process.env.KATA_TEAMMATE;
    await f.core.bind(f.ctx);
    const result = await f.core.execute(
      {
        op: row.op === "demote" ? "start" : row.op,
        task: row.op === "demote" ? "next" : "child",
        message:
          "Completed this fixture with independently recorded verification evidence.",
        evidence: ["test:safety"],
        reason: "awaiting review",
      },
      f.ctx,
    );
    const writes = f.calls.filter((args) =>
      args[0] === "meta"
        ? args[2].endsWith("#child")
        : ["claim", "close", "reopen"].includes(args[0]) &&
          args[1].endsWith("#child"),
    );
    console.log(
      JSON.stringify({
        writes: writes.length,
        error: !!result.isError,
        status: f.child.status,
        todo: f.child.metadata["todo.status"],
        metadata: f.child.metadata,
        owner: f.child.owner,
        errorText: result.isError
          ? result.content.map((part) => part.text || "").join("\n")
          : "",
      }),
    );
  }
}
