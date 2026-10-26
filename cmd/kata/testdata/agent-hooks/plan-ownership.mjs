import {
  expect,
  test,
  runTests,
  realFixture,
  coreApp,
} from "./plan-test-utils.mjs";
const { cwd, run, kata, close, env } = await realFixture("actor-a", "worker-a");
process.env.KATA_AUTHOR = "actor-a";
process.env.KATA_TEAMMATE = "worker-a";
let sequence = 0;
async function issue(meta = [], parent) {
  const args = [
    "create",
    "Ownership regression " + ++sequence,
    "--force-new",
    "--json",
  ];
  if (parent) args.push("--parent", parent);
  for (const value of meta) args.push("--meta", value);
  return (await kata(args)).issue.short_id;
}
async function show(ref) {
  return (await kata(["show", ref, "--json"])).issue;
}
async function set(ref, key, value) {
  await kata(["meta", "set", ref, key, value, "--json"]);
}
async function claim(ref, actor = "actor-a") {
  await kata(["claim", ref, "--as", actor, "--json"]);
}
function host(parent, before) {
  return coreApp({ cwd, run, parent, before, teammate: "worker-a" });
}
// Removing the ownership gate or swallowing claim failures must fail these.
for (const op of ["start", "block", "unblock"]) {
  for (const foreign of ["actor", "teammate"]) {
    test(`${op} leaves ${foreign}-owned attention unchanged`, async () => {
      const parent = await issue();
      const ref = await issue(
        [
          "todo.phase=Tasks",
          `todo.status=${op === "unblock" ? "blocked" : "pending"}`,
          "work.attention=needs-human",
          "work.attention_msg=foreign worker note",
        ],
        parent,
      );
      await claim(ref, foreign === "actor" ? "actor-b" : "actor-a");
      if (foreign === "teammate") await set(ref, "teammate", "worker-b");
      const app = host(parent);
      await app.begin();
      const result = await app.call({
        op,
        task: ref,
        reason: "waiting on a service",
      });
      expect(result.isError).toBe(true);
      expect(result.content[0].text).toMatch(/own|teammate/i);
      expect((await show(ref)).metadata["work.attention"]).toBe("needs-human");
      expect((await show(ref)).metadata["work.attention_msg"]).toBe(
        "foreign worker note",
      );
      expect(
        app.calls.filter(
          (args) => args[0] === "meta" && args[3]?.startsWith("work."),
        ),
      ).toEqual([]);
    }, 20000);
  }
}
test("failed claim on an unowned task is reported before attention writes", async () => {
  const parent = await issue();
  const ref = await issue(["todo.phase=Tasks", "todo.status=pending"], parent);
  const app = host(parent, async (args) =>
    args[0] === "claim"
      ? { code: 1, stderr: "claim transport failed", stdout: "" }
      : undefined,
  );
  await app.begin();
  const result = await app.call({ op: "start", task: ref });
  expect(result.isError).toBe(true);
  expect((await show(ref)).metadata["work.attention"]).toBeUndefined();
  expect((await show(ref)).metadata["todo.status"]).toBe("pending");
});
test("failed parent claim is reported before creating or starting children", async () => {
  let parentRef;
  const app = host("unused", async (args) => {
    if (!parentRef && args[0] === "claim") {
      parentRef = args[1];
      return { code: 1, stderr: "parent claim transport failed", stdout: "" };
    }
  });
  const result = await app.call({
    op: "init",
    message: "A fresh plan with a failed parent claim",
    items: ["A child that must never start"],
  });
  expect(result.isError).toBe(true);
  expect(parentRef).toBeDefined();
  expect((await kata(["show", parentRef, "--json"])).children ?? []).toEqual(
    [],
  );
  expect(
    app.calls.filter(
      (args) => args[0] === "meta" && args[3]?.startsWith("work."),
    ),
  ).toEqual([]);
});
test("automatic advance refuses a foreign pending task", async () => {
  const parent = await issue();
  const ref = await issue(
    ["todo.phase=Tasks", "todo.status=pending", "work.attention=needs-human"],
    parent,
  );
  await claim(ref, "actor-b");
  const app = host(parent);
  await app.begin();
  const result = await app.call({
    op: "append",
    items: ["A separate pending plan task"],
  });
  expect(result.isError).toBe(true);
  expect((await show(ref)).metadata["work.attention"]).toBe("needs-human");
});

test("starting a foreign task leaves the current active task in progress", async () => {
  const parent = await issue();
  const active = await issue(
    ["todo.phase=Tasks", "todo.status=in_progress"],
    parent,
  );
  const foreign = await issue(
    ["todo.phase=Tasks", "todo.status=pending"],
    parent,
  );
  await claim(active);
  await claim(foreign, "actor-b");

  const app = host(parent);
  await app.begin();
  const result = await app.call({ op: "start", task: foreign });
  expect(result.isError).toBe(true);
  expect((await show(active)).metadata["todo.status"]).toBe("in_progress");
  expect((await show(foreign)).owner).toBe("actor-b");
});

test("automatic advance does not overwrite a blocker added after plan load", async () => {
  const parent = await issue();
  const completed = await issue(
    ["todo.phase=Tasks", "todo.status=in_progress"],
    parent,
  );
  const next = await issue(
    ["todo.phase=Tasks", "todo.status=pending"],
    parent,
  );
  await claim(completed);
  await claim(next);
  let inject = false;
  let raced = false;
  const app = host(parent, async (args) => {
    if (inject && !raced && args[0] === "show" && args[1] === next) {
      raced = true;
      await set(next, "work.attention", "stuck");
      await set(next, "work.attention_msg", "Concurrent task blocker");
      await set(next, "todo.status", "blocked");
    }
  });
  await app.begin();
  inject = true;

  const result = await app.call({
    op: "done",
    task: completed,
    message: "Completed the first plan task before concurrent advancement.",
    evidence: ["test:ownership harness"],
  });
  expect(result.isError).toBe(true);
  expect(raced).toBe(true);
  expect((await show(completed)).status).toBe("closed");
  const blocked = await show(next);
  expect(blocked.metadata["todo.status"]).toBe("blocked");
  expect(blocked.metadata["work.attention"]).toBe("stuck");
  expect(blocked.metadata["work.attention_msg"]).toBe(
    "Concurrent task blocker",
  );
});
test("owned tasks can start, block, and unblock using guarded attention writes", async () => {
  const parent = await issue();
  const ref = await issue(["todo.phase=Tasks", "todo.status=pending"], parent);
  const app = host(parent);
  await app.begin();
  expect((await app.call({ op: "start", task: ref })).isError).toBeUndefined();
  expect((await show(ref)).owner).toBe("actor-a");
  expect(
    (await app.call({ op: "block", task: ref, reason: "waiting for review" }))
      .isError,
  ).toBeUndefined();
  expect((await show(ref)).metadata["work.attention"]).toBe("stuck");
  expect(
    (await app.call({ op: "unblock", task: ref })).isError,
  ).toBeUndefined();
  const final = await show(ref);
  expect(final.metadata["work.attention"]).toBe("ok");
  expect(final.metadata["work.attention_msg"]).toBeUndefined();
  const writes = app.calls.filter(
    (args) => args[0] === "meta" && args[3]?.startsWith("work."),
  );
  expect(writes.length).toBeGreaterThan(0);
  for (const args of writes) {
    expect(args).toContain("--if-match");
    expect(args.includes("--if-absent") || args.includes("--if-value")).toBe(
      true,
    );
  }
}, 20000);
test("closed task block skips all work metadata writes", async () => {
  const parent = await issue();
  const ref = await issue(["todo.phase=Tasks", "todo.status=pending"], parent);
  await claim(ref);
  await kata([
    "close",
    ref,
    "--done",
    "--message",
    "Verified the isolated ownership regression task before closing it.",
    "--evidence",
    "test:ownership harness",
    "--json",
  ]);
  const app = host(parent);
  await app.begin();
  const result = await app.call({ op: "block", task: ref, reason: "too late" });
  expect(result.isError).toBe(true);
  expect((await show(ref)).status).toBe("closed");
  expect((await show(ref)).metadata["work.attention"]).toBeUndefined();
});
test("explicit restart reopens an owned task after start and done", async () => {
  const parent = await issue();
  const ref = await issue(["todo.phase=Tasks", "todo.status=pending"], parent);
  const app = host(parent);
  await app.begin();
  expect((await app.call({ op: "start", task: ref })).isError).toBeUndefined();
  expect(
    (
      await app.call({
        op: "done",
        task: ref,
        message:
          "Verified the restart fixture before closing its first lifecycle.",
        evidence: ["test:ownership harness"],
      })
    ).isError,
  ).toBeUndefined();
  expect((await show(ref)).status).toBe("closed");
  expect((await app.call({ op: "start", task: ref })).isError).toBeUndefined();
  const final = await show(ref);
  expect(final.status).toBe("open");
  expect(final.owner).toBe("actor-a");
  expect(final.metadata.teammate).toBe("worker-a");
  expect(final.metadata["todo.status"]).toBe("in_progress");
  expect(final.metadata["work.attention"]).toBe("ok");
});
for (const foreign of ["actor", "teammate"]) {
  test(`explicit restart refuses a closed ${foreign}-owned task`, async () => {
    const parent = await issue();
    const ref = await issue(
      ["todo.phase=Tasks", "todo.status=pending", "work.attention=stuck"],
      parent,
    );
    await claim(ref, foreign === "actor" ? "actor-b" : "actor-a");
    if (foreign === "teammate") await set(ref, "teammate", "worker-b");
    await kata([
      "close",
      ref,
      "--wontfix",
      "--message",
      "Retired this isolated foreign restart fixture after checking that its worker must retain control.",
      "--json",
    ]);
    const app = host(parent);
    await app.begin();
    expect((await app.call({ op: "start", task: ref })).isError).toBe(true);
    expect((await show(ref)).status).toBe("closed");
    expect((await show(ref)).metadata["work.attention"]).toBe("stuck");
  });
}
for (const race of ["teammate", "metadata"]) {
  test(`explicit restart rejects a ${race} change during reopen`, async () => {
    const parent = await issue();
    const ref = await issue(
      ["todo.phase=Tasks", "todo.status=pending", "work.attention=stuck"],
      parent,
    );
    await claim(ref);
    await kata([
      "close",
      ref,
      "--wontfix",
      "--message",
      "Retired this isolated restart fixture to test a concurrent handoff at the reopen boundary.",
      "--json",
    ]);
    let raced = false;
    const app = host(parent, async (args) => {
      if (!raced && args[0] === "reopen" && args[1] === ref) {
        raced = true;
        await set(
          ref,
          race === "teammate" ? "teammate" : "work.attention_msg",
          race === "teammate" ? "worker-b" : "new blocker during restart",
        );
      }
    });
    await app.begin();
    expect((await app.call({ op: "start", task: ref })).isError).toBe(true);
    expect(raced).toBe(true);
    expect((await show(ref)).metadata["work.attention"]).toBe("stuck");
    expect((await show(ref)).metadata["todo.status"]).toBe("pending");
  });
}
test("a concurrent attention update defeats the guarded start write", async () => {
  const parent = await issue();
  const ref = await issue(["todo.phase=Tasks", "todo.status=pending"], parent);
  let raced = false;
  const app = host(parent, async (args) => {
    if (!raced && args[0] === "meta" && args[3] === "work.attention") {
      raced = true;
      await set(ref, "work.attention", "needs-human");
    }
  });
  await app.begin();
  const result = await app.call({ op: "start", task: ref });
  expect(result.isError).toBe(true);
  expect((await show(ref)).metadata["work.attention"]).toBe("needs-human");
});
for (const race of ["owner", "teammate", "close", "owner-ABA"]) {
  test(`${race} handoff between ownership read and write prevents attention mutation`, async () => {
    const parent = await issue();
    const ref = await issue(
      ["todo.phase=Tasks", "todo.status=pending"],
      parent,
    );
    await claim(ref);
    let raced = false;
    const app = host(parent, async (args) => {
      if (!raced && args[0] === "meta" && args[3] === "work.attention") {
        raced = true;
        if (race === "teammate") await set(ref, "teammate", "worker-b");
        else if (race === "close")
          await kata([
            "close",
            ref,
            "--wontfix",
            "--message",
            "Retired this isolated fixture during the ownership race to verify closed metadata stays untouched.",
            "--json",
          ]);
        else {
          await kata(["claim", ref, "--as", "actor-b", "--force", "--json"]);
          if (race === "owner-ABA")
            await kata(["claim", ref, "--as", "actor-a", "--force", "--json"]);
        }
      }
    });
    await app.begin();
    const result = await app.call({ op: "start", task: ref });
    expect(result.isError).toBe(true);
    expect((await show(ref)).metadata["work.attention"]).toBeUndefined();
    expect((await show(ref)).metadata["todo.status"]).toBe("pending");
  });
}
for (const op of ["start", "block", "unblock"]) {
  test(`${op} preserves a changed decision before the guarded helper reread`, async () => {
    const parent = await issue();
    const status = op === "unblock" ? "blocked" : "pending";
    const ref = await issue(
      ["todo.phase=Tasks", `todo.status=${status}`],
      parent,
    );
    await claim(ref);
    let reads = 0,
      raced = false;
    const app = host(parent, async (args) => {
      const decisionRead = op === "start" ? 3 : 2;
      if (
        args[0] === "show" &&
        args[1] === ref &&
        ++reads === decisionRead
      ) {
        raced = true;
        await set(ref, "work.attention", "stuck");
        await set(
          ref,
          "work.attention_msg",
          "new task blocker before helper reread",
        );
      }
    });
    await app.begin();
    expect(
      (await app.call({ op, task: ref, reason: "replace the blocker" }))
        .isError,
    ).toBe(true);
    expect(raced).toBe(true);
    const final = await show(ref);
    expect(final.metadata["work.attention"]).toBe("stuck");
    expect(final.metadata["work.attention_msg"]).toBe(
      "new task blocker before helper reread",
    );
    expect(final.metadata["todo.status"]).toBe(status);
  });
}
test("an attention message race cannot be cleared during unblock", async () => {
  const parent = await issue();
  const ref = await issue(
    [
      "todo.phase=Tasks",
      "todo.status=blocked",
      "work.attention=stuck",
      "work.attention_msg=old blocker",
    ],
    parent,
  );
  await claim(ref);
  let raced = false;
  const app = host(parent, async (args) => {
    if (
      !raced &&
      args[0] === "meta" &&
      args[1] === "unset" &&
      args[3] === "work.attention_msg"
    ) {
      raced = true;
      await set(ref, "work.attention_msg", "new blocker from another writer");
    }
  });
  await app.begin();
  const result = await app.call({ op: "unblock", task: ref });
  expect(result.isError).toBe(true);
  expect((await show(ref)).metadata["work.attention_msg"]).toBe(
    "new blocker from another writer",
  );
});
test("slash command start reports ownership failure without attention writes", async () => {
  const parent = await issue();
  const ref = await issue(
    ["todo.phase=Tasks", "todo.status=pending", "work.attention=stuck"],
    parent,
  );
  await claim(ref, "actor-b");
  const app = host(parent);
  await app.begin();
  await app.input(`/todo start ${ref}`);
  expect(app.notices.some((message) => /not owned/.test(message))).toBe(true);
  expect((await show(ref)).metadata["work.attention"]).toBe("stuck");
});
test("init automatic advance cannot claim another teammate's task", async () => {
  const parent = await issue();
  const app = host(parent);
  await app.begin();
  expect(
    (
      await app.call({
        op: "init",
        ref: parent,
        items: ["A stable initialized task"],
      })
    ).isError,
  ).toBeUndefined();
  const ref = (await kata(["show", parent, "--json"])).children[0].short_id;
  await set(ref, "todo.status", "pending");
  await set(ref, "teammate", "worker-b");
  await set(ref, "work.attention", "needs-human");
  const result = await app.call({
    op: "init",
    ref: parent,
    items: ["A stable initialized task"],
  });
  expect(result.isError).toBe(true);
  expect((await show(ref)).metadata["work.attention"]).toBe("needs-human");
});
test("successful done does not overwrite the next foreign task", async () => {
  const parent = await issue();
  const first = await issue(
    ["todo.phase=Tasks", "todo.status=in_progress"],
    parent,
  );
  await claim(first);
  const next = await issue(
    ["todo.phase=Tasks", "todo.status=pending", "work.attention=needs-human"],
    parent,
  );
  await claim(next, "actor-b");
  const app = host(parent);
  await app.begin();
  const result = await app.call({
    op: "done",
    task: first,
    message:
      "Completed the first isolated fixture with real Kata ownership checks.",
    evidence: ["test:ownership harness"],
  });
  expect(result.isError).toBe(true);
  expect((await show(first)).status).toBe("closed");
  expect((await show(next)).metadata["work.attention"]).toBe("needs-human");
});

for (const op of ["done", "drop", "rm"])
  test(`${op} refuses a foreign actor before closing or removing`, async () => {
    const parent = await issue();
    const ref = await issue(
      ["todo.phase=Tasks", "todo.status=in_progress"],
      parent,
    );
    await claim(ref, "actor-b");
    const app = host(parent);
    await app.begin();
    const result = await app.call({
      op,
      task: ref,
      message:
        "Verified the requested fixture independently before attempting closure.",
      evidence: ["test:ownership"],
    });
    expect(result.isError).toBe(true);
    expect((await show(ref)).status).toBe("open");
    expect(
      app.calls.filter((args) => args[0] === "close" || args[0] === "meta"),
    ).toEqual([]);
  });
test("final status CAS refuses an owner change after attention succeeds", async () => {
  const parent = await issue();
  const ref = await issue(["todo.phase=Tasks", "todo.status=pending"], parent);
  await claim(ref);
  let raced = false;
  const app = host(parent, async (args) => {
    if (!raced && args[0] === "meta" && args[3] === "todo.status") {
      raced = true;
      await kata(["claim", ref, "--as", "actor-b", "--force", "--json"]);
    }
  });
  await app.begin();
  expect((await app.call({ op: "start", task: ref })).isError).toBe(true);
  expect(raced).toBe(true);
  expect((await show(ref)).owner).toBe("actor-b");
  expect((await show(ref)).metadata["todo.status"]).toBe("pending");
});
try {
  await runTests();
} finally {
  await close();
}
