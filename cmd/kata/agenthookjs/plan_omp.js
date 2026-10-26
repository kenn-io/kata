// Host port of kata-plugin a1a1050; MIT notice is preserved in plan_core.js.
const workers = new Map();

function ompMarkers(ctx) {
  return {
    OMPCODE: "1",
    PI_SESSION_ID: ctx.sessionManager?.getSessionId?.() || "",
  };
}

function leadSessionKey(ctx) {
  const file = ctx.sessionManager?.getSessionFile?.();
  const id = ctx.sessionManager?.getSessionId?.();
  const identity =
    typeof file === "string" && file
      ? `file:${sessionKey(file, ctx.cwd)}`
      : typeof id === "string" && id
        ? `id:${id}`
        : "";
  return identity ? createHash("sha256").update(identity).digest("hex") : "";
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
  const assignments = new Map();
  function begin(ctx) {
    const started = ++epoch;
    const id = ctx.sessionManager?.getSessionId?.();
    const markers = ompMarkers(ctx);
    const state = { ctx, id, leadSessionKey: leadSessionKey(ctx) };
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
        beforeReconcile: async () => {
          const core = state.core,
            planWorkspace = core.plan.workspace || state.ctx.cwd;
          if (!core.plan.parent || core.plan.local) return;
          const running = state.ctx.getAsyncJobSnapshot?.()?.running || [];
          for (const child of core.plan.tasks) {
            const id = child.metadata?.["omp.subagent_id"];
            const ownerSessionKey = child.metadata?.["omp.lead_session_key"];
            const savedTerminal = child.metadata?.["omp.terminal_state"];
            const durableTerminal =
              savedTerminal &&
              typeof savedTerminal === "object" &&
              !Array.isArray(savedTerminal) &&
              savedTerminal.id === id &&
              savedTerminal.leadSessionKey === ownerSessionKey &&
              savedTerminal.planParent === core.plan.parent &&
              savedTerminal.ref === child.ref &&
              ["completed", "failed", "aborted"].includes(
                savedTerminal.state,
              )
                ? savedTerminal
                : undefined;
            const worker = [...workers.values()].find(
              (w) => w.id === id && w.leadSessionKey === ownerSessionKey,
            );
            // A closed task is durable terminal evidence even if the old
            // plugin stopped before it could persist the worker's event.
            const closedIssue =
              child.status === "completed" || child.status === "abandoned";
            const samePlanTerminal =
              durableTerminal ||
              closedIssue ||
              (worker?.terminalState &&
                !worker.running &&
                worker.planParent === core.plan.parent &&
                worker.ref === child.ref);
            if (
              !id ||
              !ownerSessionKey ||
              (ownerSessionKey !== state.leadSessionKey && !samePlanTerminal)
            )
              continue;
            if (
              worker?.running ||
              (!durableTerminal &&
                running.some((job) => job.id === id || job.agentId === id))
            )
              continue;
            const cwd =
              durableTerminal?.workspace || worker?.workspace || planWorkspace;
            await core.settleDelegate(child.ref, cwd, {
              id,
              state:
                durableTerminal?.state || worker?.terminalState || "aborted",
              lead:
                durableTerminal?.leadTeammate ||
                worker?.leadTeammate ||
                process.env.KATA_TEAMMATE ||
                "",
              leadSessionKey: ownerSessionKey,
            });
            for (const [key, entry] of workers)
              if (entry.id === id && entry.leadSessionKey === ownerSessionKey)
                workers.delete(key);
          }
        },
        allowUI: state.live,
        delegatedRunning: () =>
          [...workers.values()].some(
            (w) => w.leadSessionKey === state.leadSessionKey && w.running,
          ) ||
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
  function self(ctx, sessionFile) {
    const file =
      sessionFile || ctx.sessionManager?.getSessionFile?.();
    if (typeof file !== "string" || !file) return;
    const matches = [...workers.values()].filter(
      (entry) =>
        typeof entry.sessionFile === "string" &&
        sessionKey(entry.sessionFile, entry.workspace) ===
          sessionKey(file, entry.workspace),
    );
    if (matches.length === 1) return matches[0];
    if (matches.length > 1) return undefined;
    return workers.get(sessionKey(file, ctx.cwd));
  }
  const ompRegistry = globalThis[Symbol.for("kata.omp.native.v1")]?.get(
    pi.events,
  );
  if (ompRegistry?.workerInboxRecipients instanceof Map)
    ompRegistry.workerInboxRecipients.set(
      `${options.scope}:${options.workspace}`,
      async (ctx, sessionFile) => {
        const entry = self(ctx, sessionFile);
        if (!entry) return null;
        await entry.ready;
        if (!entry.actor || !entry.teammate) return null;
        return {
          recipient: `${entry.actor}/${entry.teammate}`,
          workspace: entry.workspace,
        };
      },
    );
  pi.events?.on?.("task:subagent:lifecycle", async (event) => {
    const activeState = active();
    if (!event?.id) return;
    if (event.status === "started") {
      if (!activeState || !main(activeState.ctx)) return;
      if (!event.sessionFile) return;
      const assignment = assignments.get(event.parentToolCallId);
      const core = assignment ? assignment.core : activeState.core;
      const planParent = assignment
        ? assignment.planParent
        : activeState.core.plan.parent;
      const workspace = assignment
        ? assignment.workspace
        : activeState.core.plan.workspace || activeState.ctx.cwd;
      const key = sessionKey(event.sessionFile, workspace);
      const entry = {
        id: event.id,
        sessionFile: event.sessionFile,
        workspace,
        core,
        planParent,
        teammate: teammateFor(event.id),
        leadSessionKey: assignment
          ? assignment.leadSessionKey
          : activeState.leadSessionKey,
        leadTeammate: assignment
          ? assignment.leadTeammate
          : process.env.KATA_TEAMMATE || "",
        running: true,
      };
      if (!entry.leadSessionKey) return;
      workers.set(key, entry);
      const index = event.index ?? 0,
        label = assignment?.labels[index];
      if (assignment) delete assignment.labels[index];
      if (assignment && !assignment.labels.some(() => true))
        assignments.delete(event.parentToolCallId);
      entry.ready = core
        .serial(async () => {
          const cwd =
            entry.workspace || core.plan.workspace || activeState.ctx.cwd;
          entry.workspace = cwd;
          const parent = assignment ? planParent : core.plan.parent;
          const local = assignment ? assignment.planLocal : core.plan.local;
          entry.actor = await core.effectiveActor(parent, cwd);
          if (assignment && (!planParent || assignment.planLocal)) return;
          if (!parent || local) return;
          const tasks = assignment
            ? assignment.planTasks
            : (await core.load(cwd), core.plan.tasks);
          const task = matchDelegation(
            tasks,
            [assignment?.texts[index], event.description].join("\n"),
            {
              label,
              single: (assignment?.texts.length ?? 1) === 1,
            },
          );
          if (!task) return;
          entry.planParent = parent;
          entry.ref = task.ref;
          await core.delegate(task.ref, cwd, {
            ...entry,
            expectedStatus: task.status,
          });
        })
        .catch((error) => {
          entry.error = error.message;
          activeState.core.debug(error);
        });
      await entry.ready;
    } else if (["completed", "failed", "aborted"].includes(event.status)) {
      // Resume changes the lead session ID before its session_start handler
      // has rebound the plan. Retain worker terminal state during that gap so
      // the resumed session's reconciliation can settle the assignment.
      if (!current() || !armed || !main(context)) return;
      const entry = event.sessionFile
        ? self(context, event.sessionFile)
        : [...workers.values()].find((worker) => worker.id === event.id);
      if (!entry || entry.id !== event.id) return;
      const key = [...workers.entries()].find(
        ([, worker]) => worker === entry,
      )?.[0];
      if (!key) return;
      entry.terminalState ||= event.status;
      await entry.ready;
      entry.running = false;
      try {
        if (entry.ref && entry.core) {
          const cwd =
            entry.workspace ||
            entry.core.plan.workspace ||
            entry.core.currentCtx?.cwd ||
            context.cwd;
          const recorded = await entry.core.recordDelegateTerminal(
            entry.ref,
            cwd,
            {
              id: entry.id,
              state: entry.terminalState,
              leadTeammate: entry.leadTeammate,
              leadSessionKey: entry.leadSessionKey,
              planParent: entry.planParent,
              workspace: cwd,
              durable: true,
            },
          );
          if (!recorded)
            throw new Error(
              `Kata task ${entry.ref} no longer has delegate ${entry.id}`,
            );
        }
        const settlementState = active();
        if (!settlementState) return;
        if (
          settlementState?.core.plan.parent &&
          settlementState.core.plan.parent !== entry.planParent
        )
          return;
        const samePlan =
          settlementState?.core.plan.parent === entry.planParent &&
          settlementState.core.plan.tasks.some(
            (task) => task.ref === entry.ref,
          );
        if (entry.ref && samePlan)
          await settlementState.core.serial(async () => {
            const cwd =
              entry.workspace ||
              settlementState.core.plan.workspace ||
              settlementState.ctx.cwd;
            await settlementState.core.settleDelegate(entry.ref, cwd, {
              id: entry.id,
              state: entry.terminalState,
              lead: entry.leadTeammate,
              leadSessionKey: entry.leadSessionKey,
            });
            await settlementState.core.load(cwd);
            settlementState.core.refresh(settlementState.ctx);
          });
        else if (entry.ref && entry.core) {
          const cwd =
            entry.workspace ||
            entry.core.plan.workspace ||
            entry.core.currentCtx?.cwd ||
            context.cwd;
          await entry.core.settleDelegate(entry.ref, cwd, {
            id: entry.id,
            state: entry.terminalState,
            lead: entry.leadTeammate,
            leadSessionKey: entry.leadSessionKey,
            durable: true,
          });
        }
        workers.delete(key);
      } catch (error) {
        entry.core?.debug(error);
      }
    }
  });
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
  pi.on("tool_call", async (event, ctx) => {
    if (!main(ctx)) {
      const entry = self(ctx);
      if (!entry || event.toolName !== "bash") return;
      await entry.ready;
      const inbox = entry.actor
        ? `export KATA_INBOX_USER=${quote(`${entry.actor}/${entry.teammate}`)};`
        : "unset KATA_INBOX_USER;";
      return {
        input: {
          ...event.input,
          command: `export KATA_TEAMMATE=${quote(entry.teammate)}; ${inbox} ${
            entry.ref && !entry.error
              ? `export KATA_REF=${quote(entry.ref)};`
              : "unset KATA_REF;"
          }\n${event.input.command}`,
        },
      };
    }
    const state = active(ctx);
    if (!state) return;
    state.core.currentCtx = ctx;
    if (event.toolName === "task") {
      await state.core.serial(() => {});
      if (!state.live() || active(ctx) !== state) return;
      const items = Array.isArray(event.input?.tasks)
        ? event.input.tasks
        : [event.input];
      const plan = state.core.plan;
      assignments.set(event.toolCallId, {
        labels: items.map((item) => item?.name || ""),
        texts: items.map((item) =>
          [item?.task, item?.description]
            .filter((v) => typeof v === "string")
            .join("\n"),
        ),
        core: state.core,
        planParent: plan.parent,
        workspace: plan.workspace || ctx.cwd,
        planLocal: plan.local,
        planTasks: plan.tasks.map((task) => ({ ...task })),
        leadSessionKey: state.leadSessionKey,
        leadTeammate: process.env.KATA_TEAMMATE || "",
      });
    }
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
    if (!main(ctx)) {
      const entry = self(ctx);
      if (!entry) return;
      await entry.ready;
      const binding =
        entry.ref && !entry.error
          ? `Assigned Kata issue: ${entry.ref}. Close it yourself with evidence:\n${quote(options.executable)} --workspace ${quote(entry.workspace || ctx.cwd)} --project ${quote(entry.ref.split("#")[0])} --teammate ${quote(entry.teammate)} close ${quote(entry.ref)} --done --message '<what changed and how verified>' --evidence 'test:<command>'\nIf unfinished, leave it open and say what remains.`
          : `No Kata task was assigned.${entry.error ? " " + entry.error : ""}`;
      const prompt = Array.isArray(event.systemPrompt)
        ? event.systemPrompt
        : [event.systemPrompt || ""];
      return {
        systemPrompt: [
          ...prompt.filter((p) => !/^<kata_binding>/.test(p)),
          `<kata_binding>\n${binding}\n</kata_binding>`,
        ],
      };
    }
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
