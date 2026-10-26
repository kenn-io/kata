import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { readFileSync, writeFileSync, mkdtempSync, mkdirSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { createOmpHost } from "./omp-host.mjs";
const tests = [];
export function test(name, fn) {
  tests.push({ name, fn });
}
export async function runTests(filter = process.argv[3]) {
  for (const { name, fn } of tests) {
    if (filter && !name.includes(filter)) continue;
    delete process.env.KATA_REF;
    await fn();
    console.log("PASS " + name);
  }
}
export function expect(actual) {
  const check = (invert = false) => ({
    get not() {
      return check(!invert);
    },
    toBe(value) {
      invert
        ? assert.notStrictEqual(actual, value)
        : assert.strictEqual(actual, value);
    },
    toEqual(value) {
      invert
        ? assert.notDeepStrictEqual(actual, value)
        : assert.deepStrictEqual(actual, value);
    },
    toBeUndefined() {
      this.toBe(undefined);
    },
    toBeDefined() {
      assert.notStrictEqual(actual, undefined);
    },
    toContain(value) {
      assert.equal(actual.includes(value), !invert);
    },
    toMatch(value) {
      assert.equal(value.test(actual), !invert);
    },
    toHaveLength(value) {
      assert.equal(actual.length, value);
    },
    toBeGreaterThan(value) {
      assert.ok(actual > value);
    },
  });
  return check();
}
let coreModule;
export async function loadCore() {
  if (coreModule) return coreModule;
  const dir = mkdtempSync(path.join(os.tmpdir(), "kata-plan-source-"));
  const file = path.join(dir, "core.mjs");
  writeFileSync(
    file,
    'import path from "node:path"; import {createHash} from "node:crypto"; import {appendFileSync,realpathSync} from "node:fs";\n' +
      readFileSync(process.argv[2], "utf8") +
      "\nexport {createKataCore,DESCRIPTION,render,phasesOf,digest,sessionKey,kataCommands,shellOptions,classifyTask,kataEnv};\n",
  );
  return (coreModule = await import(pathToFileURL(file)));
}
const { createKataCore } = await loadCore();
let seq = 0;
export function coreApp(options) {
  const sid = options.sid === null ? undefined : options.sid || "test-" + ++seq;
  const h = createOmpHost({
    cwd: options.cwd,
    sessionId: sid,
    agentKind: options.agent?.kind || "main",
  });
  const entries = options.entries || [],
    widgets = [];
  h.ctx.agent = options.agent || { kind: "main", id: sid };
  h.ctx.sessionManager.getSessionId = () => sid;
  h.ctx.sessionManager.getBranch = () => entries;
  h.ctx.getAsyncJobSnapshot = () =>
    options.snapshot?.() || { running: [], recent: [] };
  h.ctx.ui.setWidget = (...args) => widgets.push(args);
  if (options.parent && options.parent !== "unused")
    entries.push({
      type: "custom",
      customType: "io.kata.omp.parent",
      data: { parent: options.parent },
    });
  h.execute = async (_file, args, opts) =>
    (await options.before?.(
      args.map((a) =>
        options.parent && a.startsWith("example-workspace#")
          ? a.split("#")[1]
          : a,
      ),
      opts,
    )) ?? options.run(args, opts.cwd, opts.timeout);
  const core = createKataCore(
    {
      exec: (...args) => h.exec(...args),
      appendEntry: (customType, data) =>
        entries.push({ type: "custom", customType, data }),
      isMain: (ctx) => ctx.agent?.kind === "main",
      binding: options.binding,
      delegatedRunning: () =>
        h.ctx.getAsyncJobSnapshot().running.some((j) => j.type === "task"),
      beforeReconcile: options.beforeReconcile,
    },
    {
      namespace: "omp",
      executable: process.env.KATA_TEST_EXECUTABLE || "kata",
      minDaemonAPI: "0.15.0",
    },
  );
  h.on("session_start", (_e, ctx) => core.serial(() => core.bind(ctx)));
  h.on("session_shutdown", () => core.serial(() => core.reconcile()));
  h.on("session_stop", async (e, ctx) => {
    const additionalContext = await core.remind(e.messages || [], ctx);
    return additionalContext
      ? { continue: true, additionalContext }
      : undefined;
  });
  h.on("tool_result", (e, ctx) => core.observeToolResult(e, ctx));
  h.on("input", async (e, ctx) => {
    const m = /^\/todo(?:\s+([\s\S]*))?$/.exec(e.text.trim());
    if (m) {
      h.ctx.ui.notify(await core.command(m[1] || "", ctx), "info");
      return { handled: true };
    }
  });
  return {
    ctx: h.ctx,
    sid,
    entries,
    calls: h.calls.map((c) => c.args),
    widgets,
    notices: h.notifications,
    get calls() {
      return h.calls.map((c) =>
        c.args.map((a) =>
          options.parent && a.startsWith("example-workspace#")
            ? a.split("#")[1]
            : a,
        ),
      );
    },
    get notices() {
      return h.notifications.map((n) => n.message);
    },
    event: (name, data = {}) => h.emit(name, data),
    call: (params, signal) => core.execute(params, h.ctx, signal),
    async begin() {
      await h.emit("session_start");
      await core.execute({ op: "view" }, h.ctx);
    },
    input: (text) => h.emit("input", { text, source: "interactive" }),
    async init(items = ["Implement fixture", "Verify fixture"]) {
      const r = await core.execute({ op: "init", items }, h.ctx);
      assert.equal(r.isError, undefined, r.content[0].text);
      return r.details.kata.parent;
    },
    core,
  };
}
export async function realFixture(actor, teammate) {
  const binary = process.env.KATA_TEST_EXECUTABLE;
  assert.ok(
    path.isAbsolute(binary || ""),
    "absolute built Kata binary required",
  );
  const scratch = mkdtempSync(path.join(os.tmpdir(), "kata-plan-real-")),
    cwd = path.join(scratch, "workspace"),
    outside = path.join(scratch, "outside");
  mkdirSync(cwd);
  mkdirSync(outside);
  const env = {
    ...process.env,
    KATA_HOME: path.join(scratch, "home"),
    KATA_AUTHOR: actor,
  };
  for (const k of Object.keys(env))
    if (
      k.startsWith("KATA_") &&
      !["KATA_HOME", "KATA_AUTHOR", "KATA_HTTP_TIMEOUT"].includes(k)
    )
      delete env[k];
  if (teammate) env.KATA_TEAMMATE = teammate;
  const exec = promisify(execFile);
  async function run(args, workspace = cwd, timeout = 15000) {
    try {
      return {
        ...(await exec(binary, args, { env, cwd: workspace, timeout })),
        code: 0,
        killed: false,
      };
    } catch (e) {
      return {
        stdout: e.stdout || "",
        stderr: e.stderr || e.message,
        code: 1,
        killed: !!e.killed,
      };
    }
  }
  async function kata(args, workspace = cwd) {
    const r = await run(args, workspace);
    assert.equal(r.code, 0, r.stderr);
    return JSON.parse(r.stdout);
  }
  const init = await run(["init", "--project", "example-workspace"]);
  assert.equal(init.code, 0, init.stderr);
  return {
    cwd,
    outside,
    scratch,
    env,
    run,
    kata,
    close: () => run(["daemon", "stop"]),
  };
}
