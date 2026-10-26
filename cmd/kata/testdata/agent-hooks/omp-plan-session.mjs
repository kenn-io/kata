import assert from "node:assert/strict";
import { readFileSync, writeFileSync, mkdtempSync } from "node:fs";
import path from "node:path";
import { tmpdir } from "node:os";
import { pathToFileURL } from "node:url";
import { planHost } from "./omp-plan-host.mjs";
const source = readFileSync(process.argv[2], "utf8");
const file = path.join(
  mkdtempSync(path.join(tmpdir(), "kata-session-fence-")),
  "adapter.mjs",
);
const optionsMatch = /const options = ([^\n]+);/.exec(source);
assert.ok(optionsMatch);
const options = {
  ...JSON.parse(optionsMatch[1]),
  attention: true,
  contract: false,
};
writeFileSync(
  file,
  source
    .replace(
      optionsMatch[0],
      "const options = " + JSON.stringify(options) + ";",
    )
    .replace(
      /import\s*\{\s*execFile\s*\}\s*from\s*["']node:child_process["'];?/,
      'const execFile = (...args) => globalThis[Symbol.for("kata.test.session.exec")](...args);',
    ),
);
const factory = (await import(pathToFileURL(file))).default;
async function until(predicate, label) {
  const end = Date.now() + 2000;
  while (!predicate()) {
    assert.ok(Date.now() < end, "timed out waiting for " + label);
    await new Promise((resolve) => setImmediate(resolve));
  }
}
async function check(event, attentionDelay = false, sameID = false) {
  const parents = {},
    children = {};
  for (const key of ["a", "b"]) {
    parents[key] = {
      short_id: "parent-" + key,
      status: "open",
      revision: 1,
      metadata: {},
    };
    children[key] = {
      short_id: "child-" + key,
      qualified_id: "example-workspace#child-" + key,
      title: "Task " + key.toUpperCase(),
      status: "open",
      owner: "actor",
      revision: 1,
      metadata: {
        "todo.phase": "Tasks",
        "todo.status": key === "a" ? "pending" : "in_progress",
      },
    };
  }
  const branches = {};
  for (const key of ["a", "b"])
    branches[key] = [
      {
        type: "custom",
        customType: "io.kata.omp.parent",
        data: {
          parent: "example-workspace#parent-" + key,
          workspace: process.cwd(),
        },
      },
    ];
  let branch = "a",
    delayRead = false,
    pendingRead,
    pendingAttention;
  const appends = [],
    widgets = [],
    writes = [];
  const h = planHost(file, { sessionId: "A" });
  h.ctx.sessionManager.getBranch = () => branches[branch];
  h.appendEntry = (customType, data) => {
    appends.push({
      branch,
      session: h.ctx.sessionManager.getSessionId(),
      customType,
      data: structuredClone(data),
    });
    branches[branch].push({ type: "custom", customType, data });
  };
  h.ctx.ui.setWidget = (name, lines) => widgets.push({ branch, name, lines });
  h.execute = async (_exe, args) => {
    if (
      attentionDelay &&
      branch === "b" &&
      args[0] === "agent-hook" &&
      args[3] === "end"
    )
      await new Promise((resolve) => {
        pendingAttention = resolve;
      });
    return { code: 0, stdout: "", stderr: "" };
  };
  globalThis[Symbol.for("kata.test.session.exec")] = (_exe, args, opts, cb) => {
    const ok = (value) => cb(null, JSON.stringify(value), "");
    if (args[0] === "plugin") {
      ok({ marketplace: [] });
      return;
    }
    if (args[0] === "health") {
      ok({ api_schema_version: options.minDaemonAPI });
      return;
    }
    if (args[0] === "status") {
      ok({ actor: "actor" });
      return;
    }
    if (args[0] === "show") {
      const key = args[1].endsWith("-a") ? "a" : "b";
      const response = args[1].includes("#parent-")
        ? { issue: parents[key], children: [children[key]] }
        : { issue: children[key] };
      const snapshot = JSON.stringify(response);
      if (delayRead && args[1].endsWith("#child-a")) {
        delayRead = false;
        pendingRead = () => cb(null, snapshot, "");
        return;
      }
      cb(null, snapshot, "");
      return;
    }
    if (args[0] === "meta") {
      writes.push({ branch, args, env: opts.env });
      const child = children[args[2].endsWith("-a") ? "a" : "b"];
      if (args.includes("--if-match"))
        assert.equal(
          Number(args[args.indexOf("--if-match") + 1]),
          child.revision,
        );
      if (args[1] === "set") child.metadata[args[3]] = JSON.parse(args[4]);
      else delete child.metadata[args[3]];
      child.revision++;
      ok({});
      return;
    }
    throw Error("unexpected plan command " + JSON.stringify(args));
  };
  factory(h);
  // A valid parent ref makes the base hook attempt attention before the
  // session change, so the delayed end below represents a real active baseline.
  process.env.KATA_REF = "example-workspace#parent-a";
  await h.emit("session_start");
  delete process.env.KATA_REF;
  await h.call({ op: "view" });
  const expectedChild = structuredClone(children.a);
  delayRead = true;
  const old = h.call({ op: "start", task: "Task A" });
  await until(() => pendingRead, "A ownership read");
  branch = "b";
  if (!sameID) h.setSession("B");
  const change = h.emit(event, { reason: "resume" });
  if (attentionDelay) await until(() => pendingAttention, "base attention RPC");
  else await change;
  const before = JSON.stringify({ appends, widgets });
  pendingRead();
  const result = await old;
  assert.deepEqual(
    children.a,
    expectedChild,
    event + ": old session must not change its child after transition",
  );
  assert.equal(result.isError, true, event + ": old operation is rejected");
  assert.deepEqual(
    writes,
    [],
    event + ": no old-session write reaches either session",
  );
  assert.equal(
    JSON.stringify({ appends, widgets }),
    before,
    event + ": old completion changes no snapshot or widget",
  );
  assert.equal(
    appends.some(
      (entry) =>
        entry.branch === "b" &&
        entry.data?.plan?.parent === "example-workspace#parent-a",
    ),
    false,
    event + ": no A snapshot enters B",
  );
  if (attentionDelay) {
    pendingAttention();
    await change;
  }
  const current = await h.call({ op: "view" });
  assert.equal(current.isError, undefined, current.content[0].text);
  assert.equal(current.details.kata.parent, "example-workspace#parent-b");
  assert.match(widgets.at(-1).lines[0], /Task B/);
  console.log(
    "PASS " +
      event +
      (sameID ? " same-id" : "") +
      (attentionDelay ? " delayed base attention" : ""),
  );
}
const cases = [
  ["session_switch", false, false],
  ["session_branch", false, false],
  ["session_tree", false, true],
  ["session_branch", false, true],
  ["session_switch", true, false],
  ["session_branch", true, false],
];
for (const row of cases) {
  const name =
    row[0] + (row[1] ? " attention" : "") + (row[2] ? " same-id" : "");
  if (!process.argv[3] || name.includes(process.argv[3])) await check(...row);
}
