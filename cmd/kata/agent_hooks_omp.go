package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
)

const ompAgentHookMetadataPrefix = "// kata-native-omp "

type ompAgentHookMetadata struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	Scope      string `json:"scope"`
	Workspace  string `json:"workspace"`
	Executable string `json:"executable"`
	Source     string `json:"source"`
	SourceSet  bool   `json:"sourceSet"`
	Contract   bool   `json:"contract"`
	Attention  bool   `json:"attention"`
}

func planOmpAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	plan := nativeAgentHookPlan{}
	if opts.ConfigPath != "" {
		return plan, fmt.Errorf("omp uses an auto-discovered extension; --config is unsupported")
	}
	scope := opts.Scope
	if scope == "" {
		scope = "user"
	}
	var root, workspace string
	switch scope {
	case "user":
		var err error
		root, err = resolveOmpUserRoot(context.Background(), opts.Home)
		if err != nil {
			return plan, err
		}
	case "project":
		var err error
		workspace, err = filepath.Abs(opts.Dir)
		if err != nil {
			return plan, err
		}
		root = filepath.Join(workspace, ".omp")
	default:
		return plan, fmt.Errorf("unsupported omp scope %q", scope)
	}
	plan.Path = filepath.Join(root, "extensions", "kata.js")
	plan.Warnings = []string{"Requires omp 18.6.3 or newer; restart omp after installation. Offline status reports configured files, not runtime loading.", "Project extension discovery is skipped when cwd is home. Native shutdown handles graceful exits; crashes and SIGKILL need launcher cleanup."}
	if !remove {
		if _, siblingExists, err := readNativeAgentHookFile(filepath.Join(root, "extensions", "kata.ts")); err != nil {
			return plan, err
		} else if siblingExists {
			return plan, fmt.Errorf("preserving native omp kata.ts extension; move it aside before installing kata.js")
		}
	}
	data, exists, err := readNativeAgentHookFile(plan.Path)
	if err != nil {
		return plan, err
	}
	meta := ompAgentHookMetadata{Format: "kata-omp", Version: 1, Scope: scope, Workspace: workspace, Executable: opts.Executable, Source: opts.Source, SourceSet: opts.SourceSet}
	if exists {
		previous, err := parseOmpAgentHookMetadata(data)
		if err != nil {
			return plan, fmt.Errorf("preserving omp extension %q: %w", plan.Path, err)
		}
		if previous.Scope != scope || previous.Workspace != workspace {
			return plan, fmt.Errorf("preserving omp extension %q: generated scope or workspace differs", plan.Path)
		}
		meta = previous
		plan.CurrentContract = meta.Contract
		plan.CurrentAttentionStart = meta.Attention
		plan.CurrentAttentionEnd = meta.Attention
	}
	if !remove {
		// User options select additions. Omitted source and attention retain the
		// installed choice, so a contract-only reinstall cannot disable tracking.
		if opts.Executable != "" {
			meta.Executable = opts.Executable
		}
		if meta.Executable == "" {
			meta.Executable = "kata"
		}
		if opts.SourceSet {
			meta.Source = opts.Source
			meta.SourceSet = true
		}
		meta.Contract = meta.Contract || opts.Contract
		meta.Attention = meta.Attention || opts.Attention
	} else {
		if opts.Contract {
			meta.Contract = false
		}
		if opts.Attention {
			meta.Attention = false
		}
	}
	plan.Contract = meta.Contract
	plan.AttentionStart = meta.Attention
	plan.AttentionEnd = meta.Attention
	if !exists && remove {
		return plan, nil
	}
	change := nativeAgentHookChange{Path: plan.Path, Original: data, OriginalExists: exists}
	if remove && !opts.Contract && !opts.Attention {
		// Inspection preserves an exact previous-version artifact until an
		// explicit install or removal upgrades it.
		change.Content = data
		plan.Changes = []nativeAgentHookChange{change}
		return plan, nil
	}
	if !meta.Contract && !meta.Attention {
		if exists {
			change.Remove = true
			plan.Changes = []nativeAgentHookChange{change}
		}
		return plan, nil
	}
	change.Content, err = generateOmpAgentHooks(meta)
	if err != nil {
		return plan, err
	}
	plan.Changes = []nativeAgentHookChange{change}
	if !remove {
		// Keep authored sibling absence in the publication snapshot too.
		plan.Changes = append(plan.Changes, nativeAgentHookChange{Path: filepath.Join(root, "extensions", "kata.ts"), Remove: true})
	}
	return plan, nil
}

func parseOmpAgentHookMetadata(data []byte) (ompAgentHookMetadata, error) {
	var meta ompAgentHookMetadata
	first, _, ok := bytes.Cut(data, []byte("\n"))
	if !ok || !bytes.HasPrefix(first, []byte(ompAgentHookMetadataPrefix)) {
		return meta, fmt.Errorf("authored or unrecognized adapter; move it aside before installing; if PI_CODING_AGENT_DIR is shared by pi and omp, unset it for one harness")
	}
	if err := json.Unmarshal(bytes.TrimPrefix(first, []byte(ompAgentHookMetadataPrefix)), &meta, json.RejectUnknownMembers(true)); err != nil {
		return meta, fmt.Errorf("invalid ownership metadata: %w", err)
	}
	if meta.Format != "kata-omp" || meta.Version != 1 || (meta.Scope != "user" && meta.Scope != "project") || meta.Executable == "" {
		return meta, fmt.Errorf("unrecognized generated adapter version or options")
	}
	if !nativeAgentHookCodeUnedited(data) {
		return meta, fmt.Errorf("generated adapter was edited; preserve edits and rerun after moving it aside")
	}
	return meta, nil
}

func generateOmpAgentHooks(meta ompAgentHookMetadata) ([]byte, error) {
	options, err := json.Marshal(meta, nativeAgentHookOwnedJSONOptions)
	if err != nil {
		return nil, err
	}
	return sealNativeAgentHookCode([]byte(ompAgentHookMetadataPrefix + string(options) + "\n" + "const options = " + string(options) + ";\n" + nativeAttentionHostGuardJS + ompAgentHookJS)), nil
}

const ompAgentHookJS = `import path from "node:path";

// Omp binds factories again for same-process workers, each with its own
// event bus. Share scope selection and generations only within one host.
const registryKey = Symbol.for("kata.omp.native.v1");
const registries = globalThis[registryKey] ||= new WeakMap();

export default function kataHooks(pi) {
  if (!pi.zod) return;
  let registry = registries.get(pi.events);
  if (!registry) {
    registry = {
      adapters: new Map(), active: new Map(), workerInboxRecipients: new Map(),
      sessionGenerations: new Map(), shuttingDown: new Map(),
    };
    registries.set(pi.events, registry);
  }
  // The plan adapter registers scope-specific resolvers for worker bindings.
  registry.workerInboxRecipients ||= new Map();
  registry.sessionGenerations ||= new Map();
  if (!(registry.shuttingDown instanceof Map)) registry.shuttingDown = new Map();
  const key = options.scope + ":" + options.workspace;
  const adapter = { options, pi };
  registry.adapters.set(key, adapter);
  function selected(ctx, capability) {
    const cwd = path.resolve(ctx.cwd);
    let winner;
    for (const candidate of registry.adapters.values()) {
      if (!candidate.options[capability]) continue;
      if (candidate.options.scope === "project") {
        const root = candidate.options.workspace;
        const relative = path.relative(root, cwd);
        if (relative === "" || (!relative.startsWith(".." + path.sep) && relative !== ".." && !path.isAbsolute(relative))) {
          if (!winner || winner.options.scope !== "project" || root.length > winner.options.workspace.length) winner = candidate;
        }
      } else if (!winner) winner = candidate;
    }
    return winner === adapter;
  }
  async function workerInboxResolver(ctx) {
    let sessionFile;
    try { sessionFile = ctx.sessionManager?.getSessionFile?.(); } catch { return null; }
    if (typeof sessionFile !== "string" || !sessionFile) return null;
    const candidates = [...registry.workerInboxRecipients.entries()]
      .filter(([, resolver]) => typeof resolver === "function")
      .sort(([left], [right]) => {
        const rank = candidate => candidate.startsWith("project:")
          ? (candidate === key ? 0 : 1)
          : (candidate === key ? 2 : 3);
        return rank(left) - rank(right);
      });
    for (const [, resolver] of candidates) {
      try {
        const binding = await resolver(ctx, sessionFile);
        if (typeof binding?.recipient !== "string" || !binding.recipient.trim() ||
            typeof binding.workspace !== "string" || !path.isAbsolute(binding.workspace)) continue;
        return {recipient: binding.recipient, workspace: binding.workspace};
      } catch {
        // A worker without a ready binding must not inherit the lead's inbox.
      }
    }
    return null;
  }
  async function attention(mode, baseline) {
    try {
      const result = await pi.exec(baseline.executable, ["agent-hook", "attention-native", "omp", mode,
        "--session", baseline.id, "--host-pid", String(process.pid), "--ref", baseline.ref,
        "--workspace", baseline.cwd], { cwd: baseline.cwd, timeout: 10000 });
      return result.code === 0 && !result.killed;
    } catch { return false; /* Attention is best effort and never emits model context. */ }
  }
  async function attentionIdentity(adapter, baseline) {
    try {
      const ref = typeof baseline.ref === "string" ? baseline.ref.trim() : "";
      if (!ref || ref.startsWith("-")) return "";
      const options = { cwd: baseline.cwd, timeout: 10000 };
      const issue = await adapter.pi.exec(baseline.executable, ["--json", "--workspace", baseline.cwd, "show", ref], options);
      if (issue.code !== 0 || issue.killed) return "";
      const resolved = JSON.parse(issue.stdout)?.issue;
      if (!Number.isSafeInteger(resolved?.project_id) || resolved.project_id <= 0 ||
          typeof resolved.uid !== "string" || !resolved.uid) return "";
      const daemon = await adapter.pi.exec(baseline.executable, ["--json", "--workspace", baseline.cwd, "daemon", "locate"], options);
      if (daemon.code !== 0 || daemon.killed) return "";
      const route = JSON.parse(daemon.stdout);
      const daemonIdentity = route.network === "unix" ? route.address :
        route.network === "tcp" ? route.request_base_url : "";
      if (typeof daemonIdentity !== "string" || !daemonIdentity) return "";
      return JSON.stringify({ daemon: daemonIdentity, project: resolved.project_id, issue: resolved.uid });
    } catch { return ""; /* Without a resolved route and issue, do not claim ownership. */ }
  }
  async function transition(mode, baseline, adapter) {
    const pending = { mode };
    baseline.pending = pending;
    pending.promise = (async () => {
      try {
        if (registry.active.get(baseline.cwd) !== baseline) return;
        const identity = mode === "start" ? await attentionIdentity(adapter, baseline) : baseline.identity;
        if (mode === "start" && (!identity || kataAttentionHostSkip(identity))) return;
        if (mode === "start") baseline.identity = identity;
        if (mode === "start") baseline.startAttempted = true;
        const succeeded = await attention(mode, baseline);
        if (registry.active.get(baseline.cwd) === baseline && succeeded) {
          if (mode === "start") {
            baseline.started = true;
            kataAttentionHostClaim(identity);
          } else {
            baseline.ended = true;
            kataAttentionHostRelease(baseline.identity);
          }
        }
      } finally {
        if (baseline.pending === pending) delete baseline.pending;
      }
    })();
    await pending.promise;
  }
  const main = ctx => !ctx.agent?.kind || ctx.agent.kind === "main";
  async function start(event, ctx) {
    if (!selected(ctx, "attention") || !main(ctx)) return;
    const cwd = path.resolve(ctx.cwd);
    const generation = registry.sessionGenerations.get(cwd) || 0;
    const canStart = () => !registry.shuttingDown.has(cwd) &&
      (registry.sessionGenerations.get(cwd) || 0) === generation;
    if (!canStart()) return;
    const id = ctx.sessionManager.getSessionId();
    if (typeof id !== "string" || !id) return;
    const previous = registry.active.get(cwd);
    if (previous?.pending) {
      await previous.pending.promise;
      if (!canStart() || !selected(ctx, "attention") || registry.active.get(cwd) !== previous) return;
    }
    if (previous && previous.id !== id && !previous.ended && (previous.started || previous.startAttempted)) {
      await transition("end", previous, adapter);
      if (!canStart() || !selected(ctx, "attention") || registry.active.get(cwd) !== previous) return;
      if (!previous.ended) return;
    }
    const baseline = previous?.id === id && !previous.ended ? previous :
      { id, cwd, executable: options.executable, ref: process.env.KATA_REF || "", started: false, startAttempted: false, ended: false };
    if (baseline.started || baseline.pending) return;
    registry.active.set(cwd, baseline);
    await transition("start", baseline, adapter);
  }
  function beginSession(event, ctx, fresh = false) {
    if (!selected(ctx, "attention") || !main(ctx)) return;
    const cwd = path.resolve(ctx.cwd);
    const id = ctx.sessionManager.getSessionId();
    if (registry.shuttingDown.has(cwd)) {
      if (!fresh && (typeof id !== "string" || !id || registry.shuttingDown.get(cwd) === id)) return;
      registry.shuttingDown.delete(cwd);
    }
    registry.sessionGenerations.set(cwd, (registry.sessionGenerations.get(cwd) || 0) + 1);
    return start(event, ctx);
  }
  if (options.attention) {
    pi.on("session_start", (event, ctx) => beginSession(event, ctx, true));
    pi.on("session_branch", beginSession);
    pi.on("session_switch", async (event, ctx) => {
      if (["new", "resume", "fork"].includes(event.reason)) await beginSession(event, ctx);
    });
    pi.on("session_shutdown", async (event, ctx) => {
      if (!selected(ctx, "attention") || !main(ctx)) return;
      const cwd = path.resolve(ctx.cwd);
      const generation = registry.sessionGenerations.get(cwd) || 0;
      const id = ctx.sessionManager.getSessionId();
      registry.shuttingDown.set(cwd, typeof id === "string" ? id : "");
      const baseline = registry.active.get(cwd);
      if (!baseline || baseline.ended) return;
      if (baseline.pending) await baseline.pending.promise;
      if ((registry.sessionGenerations.get(cwd) || 0) !== generation ||
          !selected(ctx, "attention") || registry.active.get(cwd) !== baseline ||
          (!baseline.started && !baseline.startAttempted) || baseline.ended || baseline.pending) return;
      await transition("end", baseline, adapter);
    });
  }
  if (options.contract) pi.on("before_agent_start", async (event, ctx) => {
    if (!selected(ctx, "contract")) return;
    async function read(args, kind, parse, cwd = ctx.cwd) {
      try {
        const result = await pi.exec(options.executable, args, { cwd, timeout: 10000 });
        if (result.code !== 0 || result.killed) throw new Error("command failed");
        return parse(result.stdout);
      } catch {
        ctx.ui?.notify("Kata " + kind + " context unavailable; refresh on the next prompt.", "warning");
        return "Kata " + kind + " context is unavailable for this prompt.";
      }
    }
    const args = ["agent-contract-hook"];
    if (options.sourceSet) args.push("--source", options.source);
    const sourceCwd = options.scope === "project" && options.sourceSet && !path.isAbsolute(options.source)
      ? options.workspace : ctx.cwd;
    const contract = await read(args, "contract", stdout => {
      const text = JSON.parse(stdout)?.hookSpecificOutput?.additionalContext;
      if (typeof text !== "string") throw new Error("invalid contract response");
      return text;
    }, sourceCwd);
    let recipient = "";
    let inboxCwd = ctx.cwd;
    if (main(ctx)) {
      recipient = process.env.KATA_INBOX_USER || "";
    } else {
      const binding = await workerInboxResolver(ctx);
      if (binding) {
        recipient = binding.recipient;
        inboxCwd = binding.workspace;
      }
    }
    const inbox = recipient ? await read(["inbox", "--context", "--for", recipient], "inbox", stdout => stdout, inboxCwd) : "";
    if (!selected(ctx, "contract")) return;
    const systemPrompt = (event.systemPrompt || []).filter(entry =>
      !entry.startsWith("<kata_contract>") && !entry.startsWith("<kata_inbox>"));
    const blocks = [["kata_contract", contract], ["kata_inbox", inbox]].filter(([, text]) => text)
      .map(([name, text]) => "<" + name + ">\n" + text + "\n</" + name + ">").join("\n\n");
    if (blocks) systemPrompt.push(blocks);
    return { systemPrompt };
  });
}
`
