import assert from "node:assert/strict";
import { readFileSync, writeFileSync, mkdtempSync } from "node:fs";
import { createInterface } from "node:readline";
import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { planHost } from "./omp-plan-host.mjs";
const core = readFileSync(process.argv[2], "utf8"),
  omp = readFileSync(
    path.join(path.dirname(process.argv[2]), "plan_omp.js"),
    "utf8",
  );
const file = path.join(
  mkdtempSync(path.join(tmpdir(), "kata-plan-properties-")),
  "runtime % café.mjs",
);
writeFileSync(
  file,
  `import path from 'node:path';import {createHash} from 'node:crypto';import {appendFileSync,realpathSync} from 'node:fs';import {fileURLToPath} from 'node:url';
const options={executable:'kata',minDaemonAPI:'0.15.0'};
const execFile=(...args)=>globalThis[Symbol.for('kata.test.exec')](...args);
${core}\n${omp}\nexport default kataPlanOmp;`,
);
const factory = (await import(pathToFileURL(file))).default;
const parent = {
  short_id: "parent",
  status: "open",
  revision: 1,
  metadata: {},
  project_id: 1,
};
const child = (title) => ({
  short_id: "child",
  title,
  status: "open",
  revision: 1,
  owner: "actor",
  metadata: { "todo.phase": "Tasks", "todo.status": "in_progress" },
});
function transport(fn) {
  globalThis[Symbol.for("kata.test.exec")] = (exe, args, opts, callback) => {
    if (exe === "omp") {
      callback(null, JSON.stringify({ marketplace: [] }), "");
      return;
    }
    fn?.(args, opts, callback) ??
      callback(
        null,
        JSON.stringify(
          args[0] === "health"
            ? { api_schema_version: "0.15.0" }
            : args[0] === "status"
              ? { actor: "actor" }
              : { issue: parent, children: [child("Current task")] },
        ),
        "",
      );
  };
}
async function generationProbe(row = {}) {
  let pending,
    first = true;
  transport((args, _opts, cb) => {
    if (first && args[0] === "show") {
      first = false;
      pending = cb;
      return true;
    }
    if (args[0] === "show") {
      cb(
        null,
        JSON.stringify({
          issue: parent,
          children: [child(row.newTitle ?? "Current task")],
        }),
        "",
      );
      return true;
    }
  });
  const h = planHost(file, {
    sessionId: row.oldID ?? "A",
    entries: [
      {
        type: "custom",
        customType: "io.kata.omp.parent",
        data: { parent: "example-workspace#parent", workspace: process.cwd() },
      },
    ],
  });
  factory(h);
  const old = h.tools[0];
  const oldStart = h.emit("session_start");
  while (!pending) await new Promise((resolve) => setImmediate(resolve));
  if (row.disarm) {
    h.ctx.invokeTool = async (p) => ({ native: p });
    await old.execute("native", { op: "view" }, undefined, undefined, h.ctx);
    delete h.ctx.invokeTool;
  } else {
    h.setSession(row.newID ?? "A");
    factory(h);
    await h.emit("session_start");
    await h.call({ op: "view" });
    assert.equal(
      h.entries.filter((e) => e.customType === "io.kata.omp.state").at(-1)?.data
        .plan.tasks[0].content,
      row.newTitle ?? "Current task",
    );
  }
  const before = JSON.stringify({ widgets: h.widgets, entries: h.entries });
  pending(
    null,
    JSON.stringify({
      issue: parent,
      children: [child(row.oldTitle ?? "Old task")],
    }),
    "",
  );
  await oldStart;
  await old.execute("old", { op: "view" }, undefined, undefined, h.ctx);
  assert.equal(
    JSON.stringify({ widgets: h.widgets, entries: h.entries }),
    before,
    "old completion cannot change current widget or persistence",
  );
  if (!row.disarm) {
    const worker = planHost(file, { agentKind: "sub" });
    factory(worker);
    await worker.emit("session_start");
    const view = await h.call({ op: "view" });
    assert.equal(
      view.isError,
      undefined,
      "worker factory does not replace lead generation",
    );
  }
  return { unchanged: true };
}
if (process.argv[3] === "generation") {
  if (process.argv[4] === "stream")
    for await (const line of createInterface({ input: process.stdin }))
      console.log(JSON.stringify(await generationProbe(JSON.parse(line))));
  else {
    await generationProbe();
    console.log("PASS generation fence and separate worker host");
  }
} else if (process.argv[3] === "markers") {
  for await (const line of createInterface({ input: process.stdin })) {
    const row = JSON.parse(line),
      environments = [];
    const previous = {
      OMPCODE: process.env.OMPCODE,
      PI_SESSION_ID: process.env.PI_SESSION_ID,
    };
    process.env.OMPCODE = "inherited-wrong";
    process.env.PI_SESSION_ID = "inherited-wrong";
    process.env.KATA_PLAN_INHERITED = JSON.stringify(row.inherited);
    transport((_args, opts) => {
      environments.push(opts.env);
    });
    const h = planHost(file, {
      sessionId: row.session,
      entries: [
        {
          type: "custom",
          customType: "io.kata.omp.parent",
          data: {
            parent: "example-workspace#parent",
            workspace: process.cwd(),
          },
        },
      ],
    });
    factory(h);
    await h.emit("session_start");
    await h.call({ op: "view" });
    assert.ok(environments.length > 0);
    for (const env of environments) {
      assert.equal(env.OMPCODE, "1");
      assert.equal(env.PI_SESSION_ID, row.session);
      assert.equal(env.KATA_PLAN_INHERITED, JSON.stringify(row.inherited));
    }
    for (const [key, value] of Object.entries(previous)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
    delete process.env.KATA_PLAN_INHERITED;
    console.log(
      JSON.stringify({
        count: environments.length,
        marker: environments[0].PI_SESSION_ID,
      }),
    );
  }
} else {
  for await (const line of createInterface({ input: process.stdin })) {
    const row = JSON.parse(line);
    let calls = [];
    transport((args) => {
      calls.push(args);
    });
    const h = planHost(file, {
      entries: [
        {
          type: "custom",
          customType: "io.kata.omp.parent",
          data: {
            parent: "example-workspace#parent",
            workspace: process.cwd(),
          },
        },
      ],
    });
    factory(h);
    if (!row.own) h.tools[0].sourceInfo.path = file + ".other";
    await h.emit("session_start");
    if (row.invoke) {
      h.ctx.invokeTool = async (params) => ({ native: params });
      await h.call({ op: "view" });
      delete h.ctx.invokeTool;
    }
    calls = [];
    if (row.handler === "todo") await h.call({ op: "view" });
    else
      await h.emit(row.handler, {
        text: "/todo",
        toolName: "read",
        messages: [],
      });
    const allowed = row.own && !row.invoke;
    assert.equal(calls.length > 0, allowed && row.handler !== "tool_call");
    const possible = calls.length;
    if (row.disarm && allowed) {
      h.ctx.invokeTool = async (p) => ({ native: p });
      await h.call({ op: "view" });
      calls = [];
      delete h.ctx.invokeTool;
      await h.emit(row.handler === "todo" ? "input" : row.handler, {
        text: "/todo",
        messages: [],
      });
      assert.equal(calls.length, 0);
    }
    console.log(JSON.stringify({ possible: possible > 0, allowed }));
  }
}
