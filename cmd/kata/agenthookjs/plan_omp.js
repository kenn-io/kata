// Host port of kata-plugin a1a1050; MIT notice is preserved in plan_core.js.
function ompMarkers(ctx) {
  return {
    OMPCODE: "1",
    PI_SESSION_ID: ctx.sessionManager?.getSessionId?.() || "",
  };
}

function kataPlanOmp(pi) {
  if (!pi.zod) return;
  const registry = (globalThis[Symbol.for("kata.plan.v1")] ||= {
    generation: 0,
    host: "omp",
    hosts: new WeakMap(),
  });
  const mine = ++registry.generation;
  registry.hosts ||= new WeakMap();
  registry.hosts.set(pi.events, mine);
  const current = () => registry.hosts.get(pi.events) === mine;
  const main = (ctx) => ctx.agent?.kind === "main";
  let armed = false,
    inactive = false,
    notified = false,
    context,
    epoch = 0,
    session;
  function disarm(reason, ctx = context) {
    armed = false;
    inactive = true;
    if (!current() || notified) return;
    notified = true;
    ctx?.ui?.notify?.(reason, "warning");
    ctx?.ui?.setWidget?.("omp-kata", undefined);
  }
  const nativeReason =
    "Kata plan inactive: omp's built-in todo is on (todo.enabled). Run: omp config set todo.enabled false, then restart omp.";
  const canonical = (file) => {
    try {
      return realpathSync(file);
    } catch {
      return file;
    }
  };
  const ownPath = canonical(fileURLToPath(import.meta.url));
  function subprocess(file, args, opts, markers) {
    return new Promise((resolve) =>
      execFile(
        file,
        args,
        {
          ...opts,
          env: kataEnv(markers),
          maxBuffer: 4 * 1024 * 1024,
        },
        (error, stdout, stderr) =>
          resolve({
            code: error ? error.code || 1 : 0,
            killed: !!error?.killed,
            stdout,
            stderr,
          }),
      ),
    );
  }
  function begin(ctx) {
    const started = ++epoch;
    const id = ctx.sessionManager?.getSessionId?.();
    const markers = ompMarkers(ctx);
    const state = { ctx, id };
    // The session manager may change before the base attention handler finishes
    // and this layer receives the lifecycle event. Never recapture that id for
    // already-running work, even while its explicit epoch is still current.
    state.live = () =>
      current() &&
      armed &&
      epoch === started &&
      ctx.sessionManager?.getSessionId?.() === id;
    state.core = createKataCore(
      {
        exec: (file, args, opts) => subprocess(file, args, opts, markers),
        current: state.live,
        appendEntry: (...args) => {
          if (state.live()) pi.appendEntry(...args);
        },
        getSessionName: () => pi.getSessionName?.(),
        isMain: main,
        allowUI: state.live,
        delegatedRunning: () =>
          (state.core.currentCtx?.getAsyncJobSnapshot?.()?.running || []).some(
            (job) => job.type === "task",
          ),
      },
      {
        namespace: "omp",
        executable: options.executable,
        minDaemonAPI: options.minDaemonAPI,
        debugFile: process.env.KATA_OMP_DEBUG,
      },
    );
    session = state;
    context = ctx;
    if (main(ctx))
      return state.core.bounded(state.core.serial(() => state.core.bind(ctx)));
  }
  function active(ctx = context) {
    const state = session;
    return state?.live() && ctx?.sessionManager?.getSessionId?.() === state.id
      ? state
      : undefined;
  }
  const z = pi.zod;
  pi.registerTool({
    name: "todo",
    label: "Todo (kata カタ)",
    description: DESCRIPTION,
    approval: "read",
    parameters: z.object({
      op: z.enum([
        "init",
        "start",
        "done",
        "rm",
        "drop",
        "block",
        "unblock",
        "append",
        "view",
      ]),
      list: z
        .array(
          z.object({ phase: z.string(), items: z.array(z.string()).min(1) }),
        )
        .optional()
        .describe("phases for init"),
      task: z.string().optional().describe("verbatim task content or Kata ref"),
      phase: z.string().optional(),
      items: z
        .array(z.string())
        .optional()
        .describe("tasks for flat init or append"),
      reason: z.string().optional().describe("blocker note for block"),
      ref: z
        .string()
        .optional()
        .describe("existing Kata issue this plan serves (init)"),
      message: z
        .string()
        .optional()
        .describe(
          "done: what changed and how it was verified (40+ chars); drop/rm: reason; init without ref: title for the new Kata parent",
        ),
      evidence: z
        .array(z.string())
        .optional()
        .describe(
          "required for done: commit:<sha>, test:<cmd>, pr:<url>, reviewed-paths:<path>",
        ),
    }),
    async execute(_id, params, signal, _onUpdate, ctx) {
      if (ctx.invokeTool) {
        disarm(nativeReason, ctx);
        return ctx.invokeTool(
          Object.fromEntries(
            Object.entries(params).filter(([key]) =>
              ["op", "list", "task", "phase", "items"].includes(key),
            ),
          ),
        );
      }
      const state = active(ctx);
      if (!state)
        return {
          content: [
            {
              type: "text",
              text: "Kata plan inactive; restart omp after resolving the todo provider conflict.",
            },
          ],
          isError: true,
        };
      return state.core.execute(params, ctx, signal);
    },
  });
  pi.on("session_start", async (_event, ctx) => {
    if (!current() || inactive) return;
    context = ctx;
    const startup = ++epoch;
    const id = ctx.sessionManager?.getSessionId?.();
    session = undefined;
    if (ctx.invokeTool) {
      disarm(nativeReason, ctx);
      return;
    }
    const tool = pi.getAllTools().find((tool) => tool.name === "todo");
    if (canonical(tool?.sourceInfo?.path) !== ownPath) {
      disarm(
        "Kata plan inactive: another todo provider owns this session. Remove the conflicting provider and restart omp.",
        ctx,
      );
      return;
    }
    // getAllTools exposes only the winning registration. A losing released
    // plugin still has live handlers, so provenance alone cannot make it safe.
    const plugins = await subprocess(
      "omp",
      ["plugin", "list", "--json"],
      {
        cwd: ctx.cwd,
        timeout: 10000,
      },
      ompMarkers(ctx),
    );
    if (
      !current() ||
      inactive ||
      epoch !== startup ||
      ctx.sessionManager?.getSessionId?.() !== id
    )
      return;
    try {
      const rows = JSON.parse(plugins.stdout).marketplace;
      if (plugins.code !== 0 || plugins.killed || !Array.isArray(rows))
        throw new Error("plugin inventory unavailable");
      if (
        rows.some(
          (row) => row.id === "kata@kata-plugin" && row.enabled !== false,
        )
      ) {
        disarm(
          "Kata plan inactive: kata-plugin is enabled. Run: omp plugin uninstall kata@kata-plugin, then restart omp.",
          ctx,
        );
        return;
      }
    } catch {
      disarm(
        "Kata plan inactive: unable to check omp plugin conflicts. Restart omp when plugin list is available.",
        ctx,
      );
      return;
    }
    armed = true;
    return begin(ctx);
  });
  pi.on("input", async (event, ctx) => {
    const state = active(ctx);
    if (!state || !main(ctx)) return;
    const match = /^\/todo(?:\s+([\s\S]*))?$/.exec(event.text.trim());
    if (!match) {
      if (event.source === "interactive" && !event.text.trim().startsWith("/"))
        state.core.resetReminders();
      return;
    }
    const text = await state.core.command(match[1] || "", ctx);
    if (!state.live()) return;
    ctx.ui?.notify?.(text, "info");
    return { handled: true };
  });
  pi.on("tool_call", (_event, ctx) => {
    const state = active(ctx);
    if (!state) return;
    if (main(ctx)) state.core.currentCtx = ctx;
  });
  pi.on("tool_result", (event, ctx) => {
    const state = active(ctx);
    if (!state) return;
    return state.core.observeToolResult(event, ctx);
  });
  pi.on("session_stop", async (event, ctx) => {
    const state = active(ctx);
    if (!state) return;
    const additionalContext = await state.core.remind(
      event.messages || [],
      ctx,
    );
    if (state.live() && additionalContext)
      return { continue: true, additionalContext };
  });
  pi.on("before_agent_start", async (event, ctx) => {
    const state = active(ctx);
    if (!state) return;
    if (main(ctx) && state.core.currentId !== state.id)
      await state.core.bounded(state.core.serial(() => state.core.bind(ctx)));
    if (!state.live()) return;
    const prompt = (
      Array.isArray(event.systemPrompt)
        ? event.systemPrompt
        : [event.systemPrompt || ""]
    ).filter((entry) => !/^<kata_(todo|binding)>/.test(entry));
    if (main(ctx) && state.core.plan.parent)
      prompt.push(
        `<kata_todo>\nThe todo tool is backed by Kata. Current plan:\n${render(state.core.plan)}\n</kata_todo>`,
      );
    return { systemPrompt: prompt };
  });
  for (const name of ["session_switch", "session_branch", "session_tree"])
    pi.on(name, (_event, ctx) => {
      if (!current() || !armed || !main(ctx)) return;
      // begin changes the epoch before queueing any work. A new queue also keeps
      // the replacement session independent of the old subprocess deadline.
      return begin(ctx);
    });
  pi.on("session_shutdown", (_event, ctx) => {
    const state = active(ctx);
    if (!state) return;
    return state.core.bounded(state.core.serial(() => state.core.reconcile()));
  });
}
