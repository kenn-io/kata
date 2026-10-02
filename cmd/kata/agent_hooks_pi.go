package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const piAgentHookMetadataPrefix = "// kata-native-pi "

type piAgentHookMetadata struct {
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

func planPiAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	plan := nativeAgentHookPlan{}
	if opts.ConfigPath != "" {
		return plan, fmt.Errorf("Pi uses an auto-discovered extension; --config is unsupported") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
	}
	scope := opts.Scope
	if scope == "" {
		scope = "user"
	}
	var root, workspace string
	switch scope {
	case "user":
		root = os.Getenv("PI_CODING_AGENT_DIR")
		if root == "" {
			root = filepath.Join(opts.Home, ".pi", "agent")
		} else if root == "~" || strings.HasPrefix(root, "~/") {
			root = filepath.Join(opts.Home, strings.TrimPrefix(root, "~/"))
			if os.Getenv("PI_CODING_AGENT_DIR") == "~" {
				root = opts.Home
			}
		}
		if !filepath.IsAbs(root) {
			return plan, fmt.Errorf("PI_CODING_AGENT_DIR must resolve to an absolute path")
		}
	case "project":
		var err error
		workspace, err = filepath.Abs(opts.Dir)
		if err != nil {
			return plan, err
		}
		root = filepath.Join(workspace, ".pi")
	default:
		return plan, fmt.Errorf("unsupported Pi scope %q", scope)
	}
	plan.Path = filepath.Join(root, "extensions", "kata.js")
	plan.Warnings = []string{"Requires Pi 0.99.1 or newer; reload extensions or restart Pi after installation. Offline status reports configured files, not runtime loading.", "Project extensions require native Pi project trust; --no-extensions or authored resource settings can prevent loading.", "A later authored whole-system-prompt override retains Pi precedence and can suppress Kata context. Native quit handles graceful exits; crashes and SIGKILL need launcher cleanup."}
	if !remove {
		if _, siblingExists, err := readNativeAgentHookFile(filepath.Join(root, "extensions", "kata.ts")); err != nil {
			return plan, err
		} else if siblingExists {
			return plan, fmt.Errorf("preserving native Pi kata.ts extension; move it aside before installing kata.js")
		}
	}
	data, exists, err := readNativeAgentHookFile(plan.Path)
	if err != nil {
		return plan, err
	}
	meta := piAgentHookMetadata{Format: "kata-pi", Version: 1, Scope: scope, Workspace: workspace, Executable: opts.Executable, Source: opts.Source, SourceSet: opts.SourceSet}
	if exists {
		previous, err := parsePiAgentHookMetadata(data)
		if err != nil {
			return plan, fmt.Errorf("preserving Pi extension %q: %w", plan.Path, err)
		}
		if previous.Scope != scope || previous.Workspace != workspace {
			return plan, fmt.Errorf("preserving Pi extension %q: generated scope or workspace differs", plan.Path)
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
	change.Content, err = generatePiAgentHooks(meta)
	if err != nil {
		return plan, err
	}
	plan.Changes = []nativeAgentHookChange{change}
	return plan, nil
}

func parsePiAgentHookMetadata(data []byte) (piAgentHookMetadata, error) {
	var meta piAgentHookMetadata
	first, _, ok := bytes.Cut(data, []byte("\n"))
	if !ok || !bytes.HasPrefix(first, []byte(piAgentHookMetadataPrefix)) {
		return meta, fmt.Errorf("authored or unrecognized adapter; move it aside before installing")
	}
	if err := json.Unmarshal(bytes.TrimPrefix(first, []byte(piAgentHookMetadataPrefix)), &meta, json.RejectUnknownMembers(true)); err != nil {
		return meta, fmt.Errorf("invalid ownership metadata: %w", err)
	}
	if meta.Format != "kata-pi" || meta.Version != 1 || (meta.Scope != "user" && meta.Scope != "project") || meta.Executable == "" {
		return meta, fmt.Errorf("unrecognized generated adapter version or options")
	}
	if !nativeAgentHookCodeUnedited(data) {
		return meta, fmt.Errorf("generated adapter was edited; preserve edits and rerun after moving it aside")
	}
	return meta, nil
}

func generatePiAgentHooks(meta piAgentHookMetadata) ([]byte, error) {
	options, err := json.Marshal(meta, nativeAgentHookOwnedJSONOptions)
	if err != nil {
		return nil, err
	}
	return sealNativeAgentHookCode([]byte(piAgentHookMetadataPrefix + string(options) + "\n" + "const options = " + string(options) + ";\n" + piAgentHookJS)), nil
}

const piAgentHookJS = `import path from "node:path";

// Shared within the native host, including resource reloads. Replacing a
// registration keeps the session baseline so a reload cannot erase a handoff.
const registryKey = Symbol.for("kata.pi.native.v1");
const registry = globalThis[registryKey] ||= { adapters: new Map(), active: new Map() };
const promptState = new WeakMap();

export default function kataHooks(pi) {
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
  async function attention(mode, baseline) {
    try {
      const result = await pi.exec(options.executable, ["agent-hooks", "attention-native", "pi", mode,
        "--session", baseline.id, "--host-pid", String(process.pid), "--ref", baseline.ref,
        "--workspace", baseline.cwd], { cwd: baseline.cwd, timeout: 10000 });
      return result.code === 0 && !result.killed;
    } catch { return false; /* Attention is best effort and never emits model context. */ }
  }
  async function transition(mode, baseline) {
    const pending = { mode };
    baseline.pending = pending;
    pending.promise = (async () => {
      const succeeded = await attention(mode, baseline);
      if (registry.active.get(baseline.cwd) === baseline && succeeded) {
        if (mode === "start") baseline.started = true;
        else baseline.ended = true;
      }
      if (baseline.pending === pending) delete baseline.pending;
    })();
    await pending.promise;
  }
  if (options.attention) {
    pi.on("session_start", async (event, ctx) => {
      if (!selected(ctx, "attention") || !["startup", "new", "resume", "fork"].includes(event.reason)) return;
      const id = ctx.sessionManager.getSessionId();
      if (typeof id !== "string" || !id) return;
      const cwd = path.resolve(ctx.cwd);
      const previous = registry.active.get(cwd);
      const baseline = previous?.id === id && !previous.ended ? previous :
        { id, cwd, ref: process.env.KATA_REF || "", started: false, ended: false };
      if (baseline.started || baseline.pending) return;
      registry.active.set(cwd, baseline);
      await transition("start", baseline);
    });
    pi.on("session_shutdown", async (event, ctx) => {
      if (!selected(ctx, "attention") || event.reason !== "quit") return;
      const baseline = registry.active.get(path.resolve(ctx.cwd));
      if (!baseline || baseline.ended || baseline.id !== ctx.sessionManager.getSessionId()) return;
      if (baseline.pending) {
        const pending = baseline.pending;
        await pending.promise;
        if (pending.mode === "end") return;
      }
      if (!selected(ctx, "attention") || registry.active.get(baseline.cwd) !== baseline || baseline.ended || baseline.pending) return;
      await transition("end", baseline);
    });
  }
  if (options.contract) pi.on("before_agent_start", async (event, ctx) => {
    if (!selected(ctx, "contract")) return;
    const target = event.systemPromptOptions;
    if (!target || !target.sections) {
      ctx.ui?.notify("Kata context unavailable: Pi 0.99.1 structured prompt API is required.", "warning");
      return;
    }
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
    const recipient = process.env.KATA_INBOX_USER;
    const inbox = recipient ? await read(["inbox", "--context", "--for", recipient], "inbox", stdout => stdout) : "";
    target.sections.kata_contract = contract;
    target.sections.kata_inbox = inbox;
    // Pi's forced prompt is opaque. Append to an earlier authored override,
    // while leaving a later authored override's native precedence intact.
    if (target.forceSystemPrompt !== undefined) {
      const previous = promptState.get(target);
      const base = previous && previous.result === target.forceSystemPrompt ? previous.base : target.forceSystemPrompt;
      const managed = [["kata_contract", contract], ["kata_inbox", inbox]].filter(([, text]) => text)
        .map(([name, text]) => "<" + name + ">\n" + text + "\n</" + name + ">").join("\n\n");
      target.forceSystemPrompt = base + (managed ? "\n\n" + managed : "");
      promptState.set(target, { base, result: target.forceSystemPrompt });
    }
  });
  // Pi destroys the runtime on session replacement as well as resource reload.
  // Retire every registration after terminal handling, retaining host baselines
  // so reload preserves handoffs and detached callbacks cannot erase new owners.
  pi.on("session_shutdown", (event, ctx) => {
    if (event.reason === "quit") {
      const baseline = registry.active.get(path.resolve(ctx.cwd));
      if (baseline && !baseline.ended && baseline.id === ctx.sessionManager.getSessionId()) return;
    }
    if (["new", "resume", "fork", "reload", "quit"].includes(event.reason) && registry.adapters.get(key) === adapter) registry.adapters.delete(key);
  });
}
`
