import assert from "node:assert/strict";
import { loadCore } from "./plan-test-utils.mjs";
const { createKataCore } = await loadCore();

async function check(name, failure, existing = ["unrelated"]) {
  if (process.argv[3] && !name.includes(process.argv[3])) return;
  const parent = {
    status: "open",
    revision: 1,
    owner: "actor",
    project_id: 1,
    metadata: { "omp.auto_parent": true, "omp.session_id": "main" },
  };
  const child = {
    uid: "child-uid",
    short_id: "child",
    project_id: failure.includes("project") ? 2 : 1,
    revision: 2,
    status: "closed",
    closed_reason: "done",
    closed_at: "2026-01-01T00:00:00Z",
    title: "Child",
  };
  const labels = new Set(existing),
    labelWrites = [],
    closes = [];
  let armed = false,
    failed = false,
    recovered = false;
  const evidenceFailure = failure.startsWith("evidence:");
  const ctx = {
    cwd: process.cwd(),
    sessionManager: {
      getSessionId: () => "main",
      getBranch: () => [
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
    },
  };
  const core = createKataCore(
    {
      appendEntry() {},
      delegatedRunning: () => !armed,
      async exec(_file, args) {
        const op = args[0];
        if (
          armed &&
          !failed &&
          !evidenceFailure &&
          op === failure.split(":")[0]
        ) {
          failed = true;
          return {
            code: 1,
            stderr:
              failure === "close:throttle"
                ? "close throttled"
                : failure === "close:cas"
                  ? "revision conflict"
                  : "transport unavailable",
          };
        }
        const bad = armed && !recovered && evidenceFailure;
        let value;
        if (op === "show") value = { issue: parent, children: [child] };
        else if (op === "status") value = { actor: "actor" };
        else if (op === "health") value = { api_schema_version: "0.15.0" };
        else if (op === "projects") {
          value = { projects: [{ id: 2, name: "other-project" }] };
          if (bad && failure === "evidence:missing-project")
            value.projects = [];
          if (bad && failure === "evidence:malformed-projects")
            value.projects = {};
        } else if (op === "audit") {
          value = { rows: [{ issue: "child", event_id: 10 }] };
          if (bad && failure === "evidence:missing") value.rows = [];
          if (bad && failure === "evidence:malformed-audit") value.rows = {};
        } else if (op === "events") {
          value = {
            events: [
              {
                event_id: 10,
                type: "issue.closed",
                issue_uid: child.uid,
                payload: {
                  closed_at: child.closed_at,
                  evidence: [{ type: "test", command: "focused-check" }],
                },
              },
            ],
          };
          if (bad && failure === "evidence:reset") value.reset_required = true;
          if (bad && failure === "evidence:mismatch")
            value.events[0].issue_uid = "different-child";
          if (bad && failure === "evidence:malformed-events") value.events = {};
          if (bad && failure === "evidence:malformed-item")
            value.events[0].payload.evidence = [null];
          if (bad && failure === "evidence:malformed-proof")
            value.events[0].payload.evidence = [{ type: "test", command: 7 }];
          if (bad && failure === "evidence:empty")
            value.events[0].payload.evidence = [];
        } else if (op === "label") {
          labelWrites.push(args.slice(0, 4));
          assert.equal(args[1], "add", "reconciliation never removes labels");
          labels.add(args[3]);
          value = {};
        } else if (op === "close") {
          closes.push(args);
          parent.status = "closed";
          parent.revision++;
          value = {};
        } else throw Error("Unexpected command " + args.join(" "));
        if (bad && failure === "evidence:malformed-json" && op === "events")
          return { code: 0, stdout: "{invalid-json" };
        return { code: 0, stdout: JSON.stringify(value) };
      },
    },
    { namespace: "omp", executable: "kata", minDaemonAPI: "0.15.0" },
  );
  await core.bind(ctx);
  armed = true;
  await core.reconcile();
  assert.equal(parent.status, "open", name + ": first attempt stays open");
  assert.deepEqual(closes, [], name + ": no accepted close on failure");
  const expected = new Set(existing);
  if (evidenceFailure)
    expected.add("needs-review");
  assert.deepEqual(labels, expected, name + ": exact labels after failure");
  if (!evidenceFailure)
    assert.deepEqual(
      labelWrites,
      [],
      name + ": transient failure never labels",
    );
  recovered = true;
  await core.reconcile();
  assert.equal(
    parent.status,
    "closed",
    name + ": retry closes after valid evidence",
  );
  assert.equal(closes.length, 1);
  const close = closes[0];
  assert.equal(close[close.indexOf("--if-match") + 1], "1");
  assert.deepEqual(
    close.flatMap((v, i) => (v === "--evidence" ? [close[i + 1]] : [])),
    ["test:focused-check"],
  );
  assert.deepEqual(
    labels,
    expected,
    name + ": retry preserves every existing label",
  );
  console.log("PASS " + name);
}
for (const failure of [
  "show",
  "audit",
  "events",
  "close:cas",
  "close:throttle",
])
  await check(`transient ${failure} retries without review label`, failure);
for (const reason of [
  "missing",
  "reset",
  "mismatch",
  "malformed-audit",
  "malformed-events",
  "malformed-item",
  "malformed-proof",
  "malformed-json",
  "missing-project",
  "malformed-projects",
  "empty",
])
  await check(
    "explicit evidence " + reason + " retains review label on recovery",
    "evidence:" + reason,
  );
await check("preexisting review label survives transient recovery", "events", [
  "unrelated",
  "needs-review",
]);
await check(
  "preexisting review label survives evidence recovery",
  "evidence:missing",
  ["unrelated", "needs-review"],
);
