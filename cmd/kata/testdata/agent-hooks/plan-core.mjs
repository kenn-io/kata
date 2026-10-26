import {
  expect,
  test,
  runTests,
  realFixture,
  coreApp,
} from "./plan-test-utils.mjs";
import { mkdirSync } from "node:fs";
import path from "node:path";
const { cwd, outside, scratch, run, kata, close } =
  await realFixture("v2-builder");
process.env.KATA_AUTHOR = "v2-builder";
delete process.env.KATA_TEAMMATE;
const show = (ref) => kata(["show", ref, "--json"]);
let sequence = 0;
const issue = async (title = "Explicit parent") =>
  (await kata(["create", title + " " + ++sequence, "--force-new", "--json"]))
    .issue.short_id;
const evidence = ["test:node fixture"];
const message =
  "Implemented this isolated fixture and verified its behavior with the regression suite.";
const host = (options = {}) => coreApp({ cwd, run, ...options });
test("nonproject folders get a local plan without Kata mutations or prompt noise", async () => {
  const app = host({ cwd: outside });
  await app.event("session_start");
  await app.init();
  expect(
    (await app.call({ op: "done", task: "Implement fixture" })).isError,
  ).toBeUndefined();
  expect((await app.call({ op: "view" })).content[0].text).toContain(
    "Session-local",
  );
  expect(
    app.calls.some((a) => ["create", "claim", "meta", "close"].includes(a[0])),
  ).toBe(false);
  expect(app.notices).toEqual([]);
});

test(
  "qualified project refs beginning with local- still use Kata-backed transitions",
  async () => {
    const localCwd = path.join(scratch, `local-${++sequence}`);
    const project = `local-example-${sequence}`;
    mkdirSync(localCwd);
    const initialized = await run(
      ["init", "--project", project, "--json"],
      localCwd,
    );
    expect(initialized.code).toBe(0);
    const app = host({ cwd: localCwd });
    await app.init(["Use the Kata issue service"]);
    const taskRef = app.core.plan.tasks[0].ref;
    expect(taskRef.startsWith("local-")).toBe(true);
    const task = (await kata(["show", taskRef, "--json"], localCwd)).issue;
    expect(task.owner).toBe("v2-builder");
    expect(task.metadata["todo.status"]).toBe("in_progress");
    expect(task.metadata["work.attention"]).toBe("ok");
  },
);

test("KATA_REF beats stored session binding and is qualified", async () => {
  const old = await issue(),
    next = await issue();
  process.env.KATA_REF = next;
  const app = host({
    entries: [
      {
        type: "custom",
        customType: "io.kata.omp.parent",
        data: { parent: old },
      },
    ],
  });
  await app.event("session_start");
  const view = await app.call({ op: "view" });
  expect(view.details.kata.parent).toBe(`example-workspace#${next}`);
});

test("explicit init ref beats KATA_REF and session binding without changing the old parent", async () => {
  const bound = await issue(),
    explicit = await issue();
  process.env.KATA_REF = bound;
  const app = host();
  await app.event("session_start");
  const result = await app.call({
    op: "init",
    ref: explicit,
    items: ["Explicitly selected task"],
  });
  expect(result.isError).toBeUndefined();
  expect(result.details.kata.parent).toBe(`example-workspace#${explicit}`);
  expect((await show(bound)).children ?? []).toHaveLength(0);
  expect((await show(explicit)).children.map((child) => child.title)).toEqual([
    "Explicitly selected task",
  ]);
  await app.call({ op: "done", message, evidence });
  expect((await show(explicit)).children[0].status).toBe("closed");
  expect((await show(bound)).children ?? []).toHaveLength(0);

  delete process.env.KATA_REF;
  const boundParent = await issue(),
    selectedParent = await issue();
  const boundApp = host({ binding: async () => ({ ref: boundParent }) });
  await boundApp.event("session_start");
  await boundApp.call({
    op: "init",
    ref: boundParent,
    items: ["Keep existing bound task"],
  });
  const existing = (await show(boundParent)).children[0];
  const boundResult = await boundApp.call({
    op: "init",
    ref: selectedParent,
    items: ["Task under selected parent"],
  });
  expect(boundResult.isError).toBeUndefined();
  expect((await show(boundParent)).children[0].uid).toBe(existing.uid);
  expect((await show(boundParent)).children[0].status).toBe("open");
  expect((await show(selectedParent)).children.map((child) => child.title)).toEqual([
    "Task under selected parent",
  ]);
  await boundApp.call({ op: "done", message, evidence });
  expect((await show(boundParent)).children[0].status).toBe("open");
  expect((await show(selectedParent)).children[0].status).toBe("closed");

  const assignedParent = await issue(),
    firstBoundInit = await issue();
  const unbound = host({
    binding: async () => ({ ref: assignedParent, workspace: outside }),
  });
  const firstInit = await unbound.call({
    op: "init",
    ref: firstBoundInit,
    items: ["Initial explicit init task"],
  });
  expect(firstInit.isError).toBeUndefined();
  expect(firstInit.details.kata.parent).toBe(
    `example-workspace#${firstBoundInit}`,
  );
});

test("assigned worker binding wins over an explicit init ref", async () => {
  const assignedParent = await issue("Assigned worker parent"),
    requestedParent = await issue("Model selected parent");
  const app = host({
    agent: { kind: "worker", id: "worker" },
    binding: async () => ({ ref: assignedParent, workspace: cwd, teammate: "worker" }),
  });
  await app.event("session_start");
  const result = await app.call({
    op: "init",
    ref: requestedParent,
    items: ["Assigned worker task"],
  });
  expect(result.isError).toBeUndefined();
  expect(result.details.kata.parent).toBe(`example-workspace#${assignedParent}`);
  expect((await show(assignedParent)).children.map((child) => child.title)).toEqual([
    "Assigned worker task",
  ]);
  expect((await show(requestedParent)).children ?? []).toHaveLength(0);
});

test("auto parents carry typed identity, pinned refs and close exactly once from child evidence", async () => {
  const app = host();
  const ref = await app.init();
  expect(ref).toMatch(/^example-workspace#/);
  expect((await show(ref)).issue.metadata["omp.auto_parent"]).toBe(true);
  expect((await show(ref)).issue.metadata["omp.session_id"]).toBe(app.sid);
  for (const task of ["Implement fixture", "Verify fixture"])
    expect(
      (await app.call({ op: "done", task, message, evidence })).isError,
    ).toBeUndefined();
  expect((await show(ref)).issue.status).toBe("closed");
  await app.call({ op: "view" });
  await app.event("session_stop");
  const rows = (await kata(["audit", "closes", "--json"])).rows.filter(
    (r) => r.issue === ref.split("#")[1],
  );
  expect(rows).toHaveLength(1);
  const evt = (
    await kata([
      "events",
      "--after",
      String(rows[0].event_id - 1),
      "--limit",
      "1",
      "--json",
    ])
  ).events[0];
  expect(evt.payload.evidence).toEqual([
    { type: "test", command: "node fixture" },
  ]);
  expect(app.calls.every((a) => a.includes("--workspace"))).toBe(true);
});

test("retry completes automatic parent setup after its initial claim fails", async () => {
  let failClaim = true;
  const app = host({
    before: (args) => {
      if (failClaim && args[0] === "claim") {
        failClaim = false;
        return { code: 1, stdout: "", stderr: "transient claim failure" };
      }
    },
  });
  await app.event("session_start");
  const first = await app.call({
    op: "init",
    items: ["Recover automatic parent setup"],
  });
  expect(first.isError).toBe(true);
  const parent = app.core.plan.parent;
  expect(parent).toBeDefined();
  expect((await show(parent)).issue.owner).toBeUndefined();

  const retry = await app.call({
    op: "init",
    items: ["Recover automatic parent setup"],
  });
  expect(retry.isError).toBeUndefined();
  const recovered = (await show(parent)).issue;
  expect(recovered.owner).toBe("v2-builder");
  expect(recovered.metadata["work.attention"]).toBe("ok");
  expect((await show(parent)).children.map((child) => child.title)).toEqual([
    "Recover automatic parent setup",
  ]);
});

for (const failure of ["lost create response", "failed auto-parent marker"]) {
  test(`automatic parent setup resumes after ${failure}`, async () => {
    let injected = false;
    let created;
    const app = host({
      before: async (args, opts) => {
        const keyIndex = args.indexOf("--idempotency-key");
        const parentCreate =
          args[0] === "create" &&
          keyIndex >= 0 &&
          args[keyIndex + 1].startsWith("omp-session:");
        if (parentCreate) {
          const response = await run(args, opts.cwd, opts.timeout);
          expect(response.code).toBe(0);
          created ??= JSON.parse(response.stdout).issue;
          if (failure === "lost create response" && !injected) {
            injected = true;
            return {
              code: 1,
              stdout: "",
              stderr: "create committed but its response was lost",
            };
          }
          return response;
        }
        if (
          failure === "failed auto-parent marker" &&
          !injected &&
          args[0] === "meta" &&
          args[3] === "omp.auto_parent"
        ) {
          injected = true;
          return {
            code: 1,
            stdout: "",
            stderr: "transient automatic-parent marker failure",
          };
        }
      },
    });
    await app.event("session_start");
    const request = {
      op: "init",
      message: "Recover the ambiguous automatic parent setup",
      items: ["Recover automatic parent setup"],
    };
    const first = await app.call(request);
    expect(first.isError).toBe(true);
    expect(injected).toBe(true);
    expect(created?.short_id).toBeDefined();

    const retry = await app.call(request);
    expect(retry.isError).toBeUndefined();
    const parent = app.core.plan.parent;
    expect(parent).toBe(`example-workspace#${created.short_id}`);
    const recovered = (await show(parent)).issue;
    expect(recovered.metadata["omp.auto_parent"]).toBe(true);
    expect(recovered.owner).toBe("v2-builder");
    expect(recovered.metadata["work.attention"]).toBe("ok");
    expect((await show(parent)).children.map((child) => child.title)).toEqual([
      "Recover automatic parent setup",
    ]);
  });
}

test("init accepts dash-leading automatic parent and task titles", async () => {
  const app = host({ binding: async () => ({ teammate: "worker-a" }) });
  const result = await app.call({
    op: "init",
    message: "- Automatic plan title",
    items: ["- Child task title"],
  });
  expect(result.isError).toBeUndefined();
  const parent = app.core.plan.parent;
  expect((await show(parent)).issue.title).toBe("- Automatic plan title");
  expect((await show(parent)).children.map((child) => child.title)).toEqual([
    "- Child task title",
  ]);
});

test("init rejects blank task titles before changing the current plan", async () => {
  const app = host();
  const parent = await app.init(["Keep this existing task"]);
  const before = (await show(parent)).children[0];
  const result = await app.call({
    op: "init",
    items: ["  ", "A replacement that must not be created"],
  });
  expect(result.isError).toBe(true);
  const after = (await show(parent)).children;
  expect(after).toHaveLength(1);
  expect(after[0].uid).toBe(before.uid);
  expect(after[0].status).toBe("open");
  expect(after[0].metadata["todo.removed"]).toBeUndefined();
});

for (const priorStatus of ["closed", "removed"]) {
  test(`append restores a ${priorStatus} task by identity after its create key expires`, async () => {
    const parent = await issue();
    const title = `Restore ${priorStatus} task identity`;
    let expired = false;
    let expiredCreateCalls = 0;
    const app = host({
      parent,
      before: async (args, opts) => {
        const keyIndex = args.indexOf("--idempotency-key");
        if (
          !expired ||
          args[0] !== "create" ||
          args[args.indexOf("--") + 1] !== title ||
          keyIndex < 0
        )
          return;
        expiredCreateCalls++;
        const replay = [...args];
        replay[keyIndex + 1] += ":after-expiry";
        const response = await run(replay, opts.cwd, opts.timeout);
        expect(response.code).toBe(0);
        return response;
      },
    });
    await app.begin();
    expect(
      (await app.call({ op: "init", items: [title] })).isError,
    ).toBeUndefined();
    const original = (await show(parent)).children[0];
    expect(original.title).toBe(title);
    expect(original.metadata["todo.identity"]).toBeDefined();

    if (priorStatus === "closed") {
      expect(
        (
          await app.call({
            op: "done",
            task: title,
            message,
            evidence,
          })
        ).isError,
      ).toBeUndefined();
    } else {
      expect(
        (
          await app.call({
            op: "rm",
            task: title,
            message: "Retire this task before testing durable identity recovery.",
          })
        ).isError,
      ).toBeUndefined();
    }
    expired = true;
    expect(
      (await app.call({ op: "append", items: [title] })).isError,
    ).toBeUndefined();

    const children = (await show(parent)).children;
    expect(children).toHaveLength(1);
    expect(children[0].uid).toBe(original.uid);
    expect(children[0].status).toBe("open");
    expect(children[0].metadata["todo.removed"]).toBeUndefined();
    expect(children[0].metadata["todo.identity"]).toBeDefined();
    expect(expiredCreateCalls).toBe(0);
  });
}

test("canceled init stops after the first task create in its batch", async () => {
  const controller = new AbortController();
  let canceled = false;
  const app = host({
    before: async (args, opts) => {
      if (!canceled && args[0] === "create" && args.includes("--parent")) {
        const response = await run(args, opts.cwd, opts.timeout);
        expect(response.code).toBe(0);
        canceled = true;
        controller.abort(new Error("todo operation canceled"));
        return response;
      }
    },
  });
  await app.event("session_start");
  const result = await app.call(
    { op: "init", items: ["First canceled batch task", "Second canceled batch task"] },
    controller.signal,
  );
  expect(canceled).toBe(true);
  expect(result.isError).toBe(true);
  expect((await show(app.core.plan.parent)).children.map((child) => child.title)).toEqual([
    "First canceled batch task",
  ]);
});

test("canceled phase completion stops after the first close in its batch", async () => {
  const controller = new AbortController();
  let canceled = false;
  let firstClose;
  const app = host({
    before: async (args, opts) => {
      if (!canceled && args[0] === "close" && args.includes("--done")) {
        firstClose = args[1];
        const response = await run(args, opts.cwd, opts.timeout);
        expect(response.code).toBe(0);
        canceled = true;
        controller.abort(new Error("todo operation canceled"));
        return response;
      }
    },
  });
  const parent = await app.init(["First phase task", "Second phase task"]);
  const result = await app.call(
    { op: "done", phase: "Tasks", message, evidence },
    controller.signal,
  );
  expect(canceled).toBe(true);
  expect(result.isError).toBe(true);
  const children = (await show(parent)).children;
  expect(children.filter((child) => child.status === "closed").map((child) => child.short_id)).toEqual([
    firstClose.split("#").pop(),
  ]);
  expect(children.filter((child) => child.status === "open")).toHaveLength(1);
});

test("initialization order controls retained persisted and local tasks", async () => {
  const desired = ["Order first", "Order last", "Order middle"];
  const app = host();
  const parent = await app.init(["Order first", "Order middle", "Order last"]);
  await app.call({ op: "init", items: desired });
  expect(app.core.plan.tasks.map((task) => task.content)).toEqual(desired);
  await app.call({ op: "done", task: "Order first", message, evidence });
  expect(app.core.plan.tasks.find((task) => task.status === "in_progress").content).toBe(
    "Order last",
  );
  expect((await show(parent)).children.find((child) => child.title === "Order last").metadata["todo.status"]).toBe(
    "in_progress",
  );

  const local = host({ cwd: outside });
  await local.init(["Local order first", "Local order middle", "Local order last"]);
  await local.call({
    op: "init",
    items: ["Local order first", "Local order last", "Local order middle"],
  });
  await local.call({ op: "done", task: "Local order first" });
  expect(local.core.plan.tasks.find((task) => task.status === "in_progress").content).toBe(
    "Local order last",
  );
});

test("auto-parent adoption preserves its relationship across an ownership handoff", async () => {
  let oldParent;
  let armed = false;
  let parentReads = 0;
  let handoff = false;
  const app = host({
    before: async (args, opts) => {
      if (
        armed &&
        !handoff &&
        args[0] === "show" &&
        args[1] === oldParent
      ) {
        parentReads++;
        if (parentReads === 2) {
          const result = await run(
            ["claim", oldParent, "--as", "v2-other", "--force", "--json"],
            opts.cwd,
            opts.timeout,
          );
          expect(result.code).toBe(0);
          handoff = true;
        }
      }
    },
  });
  oldParent = await app.init(["Keep this automatic parent owned"]);
  const oldChildren = (await show(oldParent)).children.map((child) => child.uid);
  const selected = await issue("Selected parent after a handoff");
  armed = true;
  const result = await app.call({
    op: "init",
    ref: selected,
    items: ["Task under the selected parent"],
  });
  expect(handoff).toBe(true);
  expect(result.isError).toBe(true);
  expect((await show(oldParent)).children.map((child) => child.uid)).toEqual(
    oldChildren,
  );
  expect((await show(selected)).children ?? []).toHaveLength(0);
  expect(
    app.calls.some(
      (args) =>
        args[0] === "edit" &&
        args[1] === oldParent &&
        args.includes("--parent"),
    ),
  ).toBe(false);
  expect((await show(oldParent)).issue.owner).toBe("v2-other");
});

for (const scenario of ["empty", "populated"]) {
  test(`adoption leaves a ${scenario} foreign-owned automatic parent unchanged`, async () => {
    let failChildCreate = scenario === "empty";
    const app = host({
      before: (args) => {
        const keyIndex = args.indexOf("--idempotency-key");
        if (
          failChildCreate &&
          args[0] === "create" &&
          keyIndex >= 0 &&
          args[keyIndex + 1].startsWith("omp-kata:")
        ) {
          failChildCreate = false;
          return {
            code: 1,
            stdout: "",
            stderr: "transient child create failure",
          };
        }
      },
    });
    let oldParent;
    if (scenario === "empty") {
      await app.event("session_start");
      const failed = await app.call({
        op: "init",
        items: ["Child creation leaves an empty automatic parent"],
      });
      expect(failed.isError).toBe(true);
      oldParent = app.core.plan.parent;
      expect(oldParent).toBeDefined();
      expect((await show(oldParent)).children ?? []).toHaveLength(0);
    } else {
      oldParent = await app.init(["Existing automatic child"]);
    }
    const selectedParent = await issue("Selected explicit parent");
    await kata(["claim", oldParent, "--as", "v2-other", "--force", "--json"]);

    const result = await app.call({
      op: "init",
      ref: selectedParent,
      items: ["Task under the selected parent"],
    });
    expect(result.isError).toBe(true);
    expect((await show(selectedParent)).children ?? []).toHaveLength(0);
    expect(
      app.calls.some(
        (args) =>
          ["close", "edit"].includes(args[0]) && args[1] === oldParent,
      ),
    ).toBe(false);
  });
}

test("append does not reopen a closed automatic parent after owner handoff", async () => {
  const app = host();
  const parent = await app.init(["Initial child"]);
  expect(
    (await app.call({ op: "done", task: "Initial child", message, evidence }))
      .isError,
  ).toBeUndefined();
  expect((await show(parent)).issue.status).toBe("closed");

  await kata(["reopen", parent, "--json"]);
  await kata(["claim", parent, "--as", "v2-other", "--force", "--json"]);
  await kata([
    "close",
    parent,
    "--done",
    "--message",
    "Reclosed the automatic parent after every child was already complete.",
    "--test",
    "external owner handoff",
    "--json",
  ]);
  const foreignClosed = (await show(parent)).issue;
  expect(foreignClosed.status).toBe("closed");
  expect(foreignClosed.owner).toBe("v2-other");

  const result = await app.call({
    op: "append",
    items: ["This must not reopen a foreign parent"],
  });
  expect(result.isError).toBe(true);
  expect((await show(parent)).issue.status).toBe("closed");
  expect(
    app.calls.some(
      (args) => args[0] === "reopen" && args[1] === parent,
    ),
  ).toBe(false);
});

test("explicit parent is never auto-closed and rejected close leaves status active", async () => {
  const ref = await issue();
  process.env.KATA_REF = ref;
  let deny = true;
  const app = host({
    before: (a) =>
      a[0] === "close" && deny
        ? { code: 1, stderr: "evidence denied", stdout: "" }
        : undefined,
  });
  await app.event("session_start");
  await app.init(["A guarded task"]);
  expect((await app.call({ op: "done", message, evidence })).isError).toBe(
    true,
  );
  expect((await show(ref)).children[0].metadata["todo.status"]).toBe(
    "in_progress",
  );
  deny = false;
  expect(
    (await app.call({ op: "done", message, evidence })).isError,
  ).toBeUndefined();
  expect((await show(ref)).issue.status).toBe("open");
});

test("wontfix floors and slash sibling messages pass real Kata guards", async () => {
  const app = host();
  const ref = await app.init(["x", "y", "z"]);
  expect(
    (await app.call({ op: "drop", task: "x", message: "no" })).isError,
  ).toBeUndefined();
  await app.event("input", { text: "/todo done", source: "interactive" });
  await app.event("input", { text: "/todo done", source: "interactive" });
  expect((await show(ref)).children.every((c) => c.status === "closed")).toBe(
    true,
  );
});

test("id-less session never creates an unknown automatic parent", async () => {
  const app = host({ sid: null });
  expect(
    (await app.call({ op: "init", items: ["No session identity"] })).isError,
  ).toBe(true);
  expect(app.calls.some((a) => a[0] === "create")).toBe(false);
});

test("successful standalone main claim binds without reparenting existing tasks", async () => {
  const app = host();
  const auto = await app.init();
  const autoChildren = (await show(auto)).children.map((child) => child.uid);
  const explicit = await issue();
  await kata(["claim", explicit, "--json"]);
  await app.event("tool_result", {
    toolName: "bash",
    input: { command: `kata claim ${explicit}` },
    isError: false,
    details: { exitCode: 0 },
  });
  expect((await app.call({ op: "view" })).details.kata.parent).toBe(
    `example-workspace#${explicit}`,
  );
  expect((await show(auto)).children.map((child) => child.uid)).toEqual(
    autoChildren,
  );
  expect((await show(explicit)).children ?? []).toHaveLength(0);
  expect(
    app.calls.some(
      (args) =>
        args[0] === "edit" &&
        args[1] === auto &&
        args.includes("--parent"),
    ),
  ).toBe(false);
});

test("external closes reconcile an auto parent, including non-todo children", async () => {
  const app = host();
  const ref = await app.init(["External task"]);
  const extra = (
    await kata([
      "create",
      "External verification child",
      "--parent",
      ref,
      "--force-new",
      "--json",
    ])
  ).issue.short_id;
  expect(
    (await app.call({ op: "done", message, evidence })).isError,
  ).toBeUndefined();
  expect((await show(ref)).issue.status).toBe("open");
  await kata([
    "close",
    extra,
    "--done",
    "--message",
    "Externally verified this additional child with an independent test run.",
    "--test",
    "external verifier",
    "--json",
  ]);
  await app.call({ op: "view" });
  expect((await show(ref)).issue.status).toBe("closed");
});

test("pinned plan survives cwd changes and ignores another session's saved binding", async () => {
  const app = host();
  const parent = await app.init(["Pinned task"]);
  app.ctx.cwd = outside;
  expect(
    (await app.call({ op: "append", items: ["Another pinned task"] })).isError,
  ).toBeUndefined();
  expect((await show(parent)).children).toHaveLength(2);
  const next = host({ entries: app.entries });
  await next.event("session_start");
  expect((await next.call({ op: "view" })).details.kata.parent).toBeUndefined();
});

test("new work reopens an auto parent; all-wontfix parents require review", async () => {
  const app = host();
  const parent = await app.init(["Initial work"]);
  await app.call({ op: "done", message, evidence });
  expect((await show(parent)).issue.status).toBe("closed");
  await app.call({ op: "append", items: ["Additional work"] });
  expect((await show(parent)).issue.status).toBe("open");
  const dropped = host();
  const retired = await dropped.init(["x"]);
  await dropped.call({ op: "drop", message: "no" });
  const view = await show(retired);
  expect(view.issue.status).toBe("open");
  expect(view.labels.some((l) => l.label === "needs-review")).toBe(true);
});

test("sibling throttle retries the pinned close and does not falsely complete metadata", async () => {
  let throttled = false;
  const app = host({
    before: (a) => {
      if (!throttled && a[0] === "close") {
        throttled = true;
        return {
          code: 1,
          stdout: "",
          stderr: "sibling close throttle; retry after 60s",
        };
      }
    },
  });
  const parent = await app.init(["Throttled task"]);
  const result = await app.call({ op: "done", message, evidence });
  expect(result.isError).toBeUndefined();
  expect(result.content[0].text).toContain("retry");
  expect((await show(parent)).children[0].metadata["todo.status"]).toBe(
    "in_progress",
  );
  app.ctx.cwd = outside;
  const actualNow = Date.now;
  Date.now = () => actualNow() + 61_000;
  try {
    await app.event("session_stop");
    await app.call({ op: "view" });
  } finally {
    Date.now = actualNow;
  }
  expect((await show(parent)).issue.status).toBe("closed");
});

test("sibling throttle cancels a close retry after the task revision changes", async () => {
  let throttled = false;
  const app = host({
    before: (a) => {
      if (!throttled && a[0] === "close") {
        throttled = true;
        return {
          code: 1,
          stdout: "",
          stderr: "sibling close throttle; retry after 60s",
        };
      }
    },
  });
  const parent = await app.init(["Changed while throttled"]);
  await app.call({ op: "done", message, evidence });
  const child = (await show(parent)).children[0];
  await kata([
    "meta",
    "set",
    child.short_id,
    "todo.status",
    JSON.stringify("blocked"),
    "--if-match",
    String(child.revision),
    "--json-value",
    "--json",
  ]);
  const actualNow = Date.now;
  Date.now = () => actualNow() + 61_000;
  try {
    await app.event("session_stop");
  } finally {
    Date.now = actualNow;
  }
  const current = (await show(parent)).children[0];
  expect(current.status).toBe("open");
  expect(current.metadata["todo.status"]).toBe("blocked");
  expect(app.notices.some((notice) => notice.includes("was canceled"))).toBe(
    true,
  );
});

for (const operation of ["retain with init", "continue with start"]) {
  test(`sibling throttle cancels a deferred removal after ${operation}`, async () => {
    let throttled = false;
    const app = host({
      before: (args) => {
        if (!throttled && args[0] === "close") {
          throttled = true;
          return {
            code: 1,
            stdout: "",
            stderr: "sibling close throttle; retry after 60s",
          };
        }
      },
    });
    const title = `Retained after throttled removal ${operation}`;
    const parent = await app.init([title]);
    await app.call({
      op: "rm",
      task: title,
      message: "Remove only when the user has not retained this task.",
    });
    if (operation === "retain with init") {
      const retained = await app.call({ op: "init", items: [title] });
      expect(retained.isError).toBeUndefined();
    } else {
      const continued = await app.call({ op: "start", task: title });
      expect(continued.isError).toBeUndefined();
    }
    const actualNow = Date.now;
    Date.now = () => actualNow() + 61_000;
    try {
      await app.event("session_stop");
    } finally {
      Date.now = actualNow;
    }
    const task = (await show(parent)).children[0];
    expect(task.status).toBe("open");
    expect(task.metadata["todo.removed"]).toBeUndefined();
  });
}

test("membership changes during evidence retrieval prevent auto-parent close", async () => {
  let mutate = false;
  let parent;
  const app = host({
    before: async (a) => {
      if (mutate && a[0] === "events") {
        mutate = false;
        await kata([
          "create",
          "Concurrent child",
          "--parent",
          parent,
          "--force-new",
          "--json",
        ]);
      }
    },
  });
  parent = await app.init(["Initial closed child"]);
  mutate = true;
  await app.call({ op: "done", message, evidence });
  expect((await show(parent)).issue.status).toBe("open");
  expect(
    app.calls.some((args) => args[0] === "close" && args[1] === parent),
  ).toBe(false);
});

test("claim observer skips commands routed to an explicit daemon", async () => {
  const app = host();
  const parent = await app.init(["Keep default daemon binding"]);
  const explicit = await issue("Daemon-routed claim");
  await kata(["claim", explicit, "--json"]);
  for (const command of [
    `kata --daemon other claim ${explicit}`,
    `KATA_SERVER=https://daemon.example kata claim ${explicit}`,
  ])
    await app.event("tool_result", {
      toolName: "bash",
      input: { command },
      isError: false,
      details: { exitCode: 0 },
    });
  expect((await app.call({ op: "view" })).details.kata.parent).toBe(parent);
  expect((await show(explicit)).children ?? []).toHaveLength(0);
});

test("cross-project adoption pins new children to the claimed project", async () => {
  const other = path.join(scratch, `other-${++sequence}`);
  mkdirSync(other);
  await kata(["init", "--project", "v2other", "--json"], other);
  const foreign = (
    await kata(
      [
        "create",
        "Cross-project claimed issue",
        "--owner",
        "v2-builder",
        "--force-new",
        "--json",
      ],
      other,
    )
  ).issue.short_id;
  const app = host();
  const original = await app.init(["Original task"]);
  await app.event("tool_result", {
    toolName: "bash",
    input: { command: `kata --workspace '${other}' claim ${foreign}` },
    isError: false,
  });
  const current = await app.call({ op: "view" });
  expect(current.details.kata.parent).toBe(`v2other#${foreign}`);
  expect(
    (await app.call({ op: "append", items: ["Claimed-project task"] })).isError,
  ).toBeUndefined();
  const view = await show(`v2other#${foreign}`);
  const child = view.children.find((c) => c.title === "Claimed-project task");
  expect(child.project_id).toBe(view.issue.project_id);
  expect((await show(original)).issue.status).toBe("open");
});

test("restoring a removed cross-project child uses its qualified reference", async () => {
  const other = path.join(scratch, `cross-child-${++sequence}`);
  mkdirSync(other);
  await kata(["init", "--project", "example-cross-child", "--json"], other);
  const parent = await issue("Cross-project parent");
  const parentRef = `example-workspace#${parent}`;
  const title = "Restore cross-project task";
  const created = await kata(
    [
      "create",
      "--parent",
      parentRef,
      "--project",
      "example-cross-child",
      "--meta",
      "todo.phase=Tasks",
      "--force-new",
      "--json",
      "--",
      title,
    ],
    other,
  );
  const childRef = `example-cross-child#${created.issue.short_id}`;
  await kata(["claim", childRef, "--json"], other);
  await kata(
    [
      "close",
      childRef,
      "--wontfix",
      "--message",
      "Retire this cross-project child before testing task restoration.",
      "--json",
    ],
    other,
  );
  await kata(
    ["meta", "set", childRef, "todo.removed", "true", "--json-value", "--json"],
    other,
  );
  const app = host({ parent: parentRef });
  await app.begin();
  const result = await app.call({ op: "append", items: [title] });
  expect(result.isError).toBeUndefined();
  expect(app.core.plan.tasks[0].ref).toBe(childRef);
  const restored = (await kata(["show", childRef, "--json"], other)).issue;
  expect(restored.status).toBe("open");
  expect(restored.metadata["todo.removed"]).toBeUndefined();
});

test("typed automatic parent can be recovered after losing the session entry", async () => {
  const app = host();
  const parent = await app.init(["Durable existing task"]);
  const recovered = host({ sid: app.sid });
  await recovered.event("session_start");
  expect((await recovered.call({ op: "view" })).details.kata.parent).toBe(
    parent,
  );
});

test("explicitly adopting the auto issue revokes and persists automatic close authority", async () => {
  const app = host();
  const parent = await app.init(["Explicitly retained task"]);
  await app.call({
    op: "init",
    ref: parent,
    items: ["Explicitly retained task"],
  });
  expect(
    app.entries.filter((e) => e.customType === "io.kata.omp.parent").at(-1).data
      .auto,
  ).toBe(false);
  await app.call({ op: "done", message, evidence });
  expect((await show(parent)).issue.status).toBe("open");
  const restored = host({ sid: app.sid, entries: app.entries });
  await restored.event("session_start");
  await restored.call({ op: "view" });
  expect((await show(parent)).issue.status).toBe("open");
});

test("incompatible child evidence stays recorded and marks the automatic parent for review", async () => {
  const app = host();
  const parent = await app.init(["Typed union task"]);
  const audit = (
    await kata([
      "create",
      "No-change verification",
      "--parent",
      parent,
      "--force-new",
      "--json",
    ])
  ).issue.short_id;
  await kata([
    "close",
    audit,
    "--audit-no-change",
    "--message",
    "Audited the current implementation and verified no additional changes were required.",
    "--reviewed",
    "README.md",
    "--evidence",
    "no-change-audit:Existing behavior already satisfies the specification",
    "--json",
  ]);
  await app.call({ op: "done", message, evidence });
  expect(
    app.calls.some(
      (a) => a[0] === "close" && a[1] === parent && a.includes("--done"),
    ),
  ).toBe(false);
  const auditRow = (
    await kata(["audit", "closes", "--json"])
  ).rows.find((row) => row.issue === audit);
  const auditEvent = (
    await kata([
      "events",
      "--after",
      String(auditRow.event_id - 1),
      "--limit",
      "1",
      "--json",
    ])
  ).events[0];
  expect(auditEvent.payload.evidence).toEqual([
    { type: "no-change-audit", rationale: "Existing behavior already satisfies the specification" },
    { type: "reviewed-paths", paths: ["README.md"] },
  ]);
  const view = await show(parent);
  expect(view.issue.status).toBe("open");
  expect(view.labels.some((l) => l.label === "needs-review")).toBe(true);
});

test("auto-parent evidence resolves cross-project children in their own projects", async () => {
  const other = path.join(scratch, `evidence-${++sequence}`);
  mkdirSync(other);
  await kata(["init", "--project", "v2evidence", "--json"], other);
  const app = host();
  const parent = await app.init(["Local evidence task"]);
  const extra = (
    await kata(
      [
        "create",
        "Cross-project evidence",
        "--parent",
        parent,
        "--force-new",
        "--json",
      ],
      other,
    )
  ).issue.short_id;
  await kata(
    [
      "close",
      `v2evidence#${extra}`,
      "--done",
      "--message",
      "Verified this cross-project component with its own independent test evidence.",
      "--test",
      "cross-project validation",
      "--json",
    ],
    other,
  );
  await app.call({ op: "done", message, evidence });
  expect((await show(parent)).issue.status).toBe("closed");
});

test("local plan and blockers survive extension recreation through session entries", async () => {
  const app = host({ cwd: outside });
  await app.event("session_start");
  await app.init(["Durable local first", "Durable local second"]);
  await app.call({ op: "block", reason: "Waiting on a durable dependency" });
  const entries = JSON.parse(JSON.stringify(app.entries));
  const resumed = host({ cwd: outside, sid: app.sid, entries });
  await resumed.event("session_start");
  const view = await resumed.call({ op: "view" });
  expect(view.content[0].text).toContain("Waiting on a durable dependency");
  expect(view.details.phases[0].tasks.map((t) => t.status)).toEqual([
    "blocked",
    "in_progress",
  ]);
  const other = host({ cwd: outside, entries });
  await other.event("session_start");
  expect((await other.call({ op: "view" })).content[0].text).toContain(
    "No todo plan",
  );
});

test("queued close survives extension recreation and retries only after its deadline", async () => {
  const app = host({
    before: (a) =>
      a[0] === "close"
        ? { code: 1, stdout: "", stderr: "sibling close throttle" }
        : undefined,
  });
  const parent = await app.init(["Durable queued close"]);
  await app.call({ op: "done", message, evidence });
  const entries = JSON.parse(JSON.stringify(app.entries));
  const resumed = host({ sid: app.sid, entries });
  await resumed.event("session_start");
  expect(resumed.calls.some((a) => a[0] === "close")).toBe(false);
  const now = Date.now;
  Date.now = () => now() + 61_000;
  try {
    const restarted = host({ sid: app.sid, entries });
    await restarted.event("session_start");
    await restarted.call({ op: "view" });
    expect((await show(parent)).issue.status).toBe("closed");
    expect(
      (await kata(["audit", "closes", "--json"])).rows.filter(
        (r) => r.issue === parent.split("#")[1],
      ),
    ).toHaveLength(1);
  } finally {
    Date.now = now;
  }
});

test("adopting an explicit parent retires an empty automatic parent", async () => {
  const app = host();
  await app.event("session_start");
  await app.core.serial(() => app.core.ensureParent({}, app.ctx));
  const auto = (await app.call({ op: "view" })).details.kata.parent;
  expect(auto).toMatch(/^example-workspace#/);
  const explicit = await issue();
  await kata(["claim", explicit, "--json"]);
  await app.event("tool_result", {
    toolName: "bash",
    input: { command: `kata claim ${explicit}` },
    isError: false,
    details: { exitCode: 0 },
  });
  expect((await app.call({ op: "view" })).details.kata.parent).toBe(
    `example-workspace#${explicit}`,
  );
  const retired = await show(auto);
  expect(retired.issue.status).toBe("closed");
  expect(retired.issue.closed_reason).toBe("superseded");
  expect((await show(explicit)).children ?? []).toHaveLength(0);
});
try {
  await runTests();
} finally {
  await close();
}
