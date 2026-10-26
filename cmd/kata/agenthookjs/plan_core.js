/*
MIT License

Copyright (c) 2026 salmonumbrella

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/
// Port of kata-plugin a1a1050: canonical core, excluding contract cache.

// Host adapters own their event, schema and identity APIs.

function sessionKey(file, cwd) {
  const absolute = path.resolve(cwd, file);
  try {
    return realpathSync(absolute);
  } catch {
    try {
      return path.join(
        realpathSync(path.dirname(absolute)),
        path.basename(absolute),
      );
    } catch {
      return absolute;
    }
  }
}

function quote(value) {
  return `'${value.replace(/'/g, "'\\''")}'`;
}

const OPEN = new Set(["pending", "in_progress", "blocked"]);
const MARK = {
  pending: "[ ]",
  in_progress: "[/]",
  completed: "[x]",
  abandoned: "[~]",
  blocked: "[!]",
};

const DESCRIPTION = `Track multi-step work as a todo list stored in Kata. Tasks are identified by verbatim content (or Kata ref), NEVER generated IDs. Unique, stable task/phase names; lost text: view, NEVER guess.
Before work, init for 3+ steps, requested task sets, or new instructions. MUST list EVERY user item separately; NEVER omit or remember leftovers.
After each change: with no active task the earliest pending starts (phase order). Blocked NEVER starts automatically; unblock returns pending. Mark done immediately; follow phase order.
External waits (user/agent/service): block with a reason; it suppresses the stop reminder and starts the next pending task.
NEVER call todo alone: init with first work; done/start with next action.

Kata specifics:
- Each task is a Kata child issue of one parent issue. On init pass \`ref\` with the Kata issue this plan serves (search first: kata search "<terms>" --agent); otherwise KATA_REF, the session's existing parent, or a new parent titled by \`message\` is used.
- done closes the issue. Kata requires \`evidence\` (commit:<sha>, test:<cmd>, pr:<url>, reviewed-paths:<path>) and a \`message\` of 40+ characters saying what changed and how it was verified. Unverified work: block it instead.
- drop/rm take the reason in \`message\`.
- drop and rm close as wontfix (Kata never deletes). Automatic session parents close from their children’s Kata-recorded evidence once all work has settled. Explicit/claimed parents remain yours to close. Outside Kata projects, todo keeps a session-local plan.`;

function digest(...parts) {
  return createHash("sha256")
    .update(parts.join("\u0000"))
    .digest("hex")
    .slice(0, 24);
}

// Inspect command positions only. Quoted arguments and heredoc bodies are data;
// this lexer never infers whether a test/commit command proves completion.
function kataCommands(command, configuredExecutable = "kata") {
  const segments = [[]];
  let i = 0,
    heredoc,
    wantDelimiter = false;
  while (i < command.length) {
    if (command[i] === "\n" && heredoc) {
      let start = i + 1,
        end;
      while (start <= command.length) {
        const newline = command.indexOf("\n", start);
        if (
          command
            .slice(start, newline < 0 ? command.length : newline)
            .replace(/\r$/, "") === heredoc
        ) {
          end = newline < 0 ? command.length : newline;
          break;
        }
        if (newline < 0) break;
        start = newline + 1;
      }
      if (end === undefined) break;
      i = end;
      heredoc = undefined;
    }
    if (/\s/.test(command[i])) {
      if (command[i] === "\n") segments.push([]);
      i++;
      continue;
    }
    if (command[i] === "#") {
      const end = command.indexOf("\n", i);
      if (end < 0) break;
      i = end;
      continue;
    }
    if (";&|()".includes(command[i])) {
      segments.push([]);
      i++;
      continue;
    }
    if (command.slice(i, i + 2) === "<<") {
      i += command[i + 2] === "-" ? 3 : 2;
      wantDelimiter = true;
      continue;
    }
    const start = i;
    let value = "",
      quote = "";
    while (i < command.length) {
      const char = command[i];
      if (!quote && /[\s;&|()]/.test(char)) break;
      if (char === "\\" && quote !== "'") {
        value += command[i + 1] || "";
        i += 2;
        continue;
      }
      if (char === quote) {
        quote = "";
        i++;
        continue;
      }
      if (!quote && (char === "'" || char === '"')) {
        quote = char;
        i++;
        continue;
      }
      value += char;
      i++;
    }
    if (wantDelimiter) {
      heredoc = value;
      wantDelimiter = false;
      continue;
    }
    segments.at(-1).push({ value, start, end: i });
  }
  const commands = [];
  for (const segment of segments) {
    let j = 0;
    while (
      j < segment.length &&
      (/^[A-Za-z_][A-Za-z0-9_]*=/.test(segment[j].value) ||
        [
          "if",
          "then",
          "elif",
          "do",
          "else",
          "!",
          "command",
          "env",
          "time",
        ].includes(segment[j].value))
    )
      j++;
    const executable = segment[j];
    if (
      executable &&
      (path.basename(executable.value) === "kata" ||
        executable.value === configuredExecutable ||
        [
          "$KATA_OMP_EXECUTABLE",
          "${KATA_OMP_EXECUTABLE}",
          "$KATA_PI_EXECUTABLE",
          "${KATA_PI_EXECUTABLE}",
        ].includes(executable.value))
    )
      commands.push({
        executable,
        words: segment.slice(j + 1),
        routedByEnvironment: segment
          .slice(0, j)
          .some((word) =>
            /^KATA_(?:SERVER|DAEMON|HOME|DB|DSN)=/.test(word.value),
          ),
      });
  }
  return commands;
}

const VALUE_OPTIONS = new Set([
  "--body",
  "--body-file",
  "--message",
  "-m",
  "--comment",
  "--evidence",
  "--test",
  "--commit",
  "--pr",
  "--reviewed",
  "--reason",
  "--idempotency-key",
  "--parent",
  "--related",
  "--blocked-by",
  "--blocks",
  "--meta",
  "--owner",
  "--label",
  "--priority",
  "--if-match",
  "--if-value",
  "--workspace",
  "--project",
  "--teammate",
  "--as",
  "--for",
  "--format",
  "--daemon",
]);
function shellOptions(words) {
  const options = new Map(),
    occurrences = new Map(),
    args = [];
  for (let i = 0; i < words.length; i++) {
    const word = words[i];
    if (word.value === "--") {
      args.push(...words.slice(i + 1).map((w) => w.value));
      break;
    }
    const flag = word.value.split("=")[0];
    if (word.value.startsWith("-")) {
      if (VALUE_OPTIONS.has(flag) && !word.value.includes("=") && words[i + 1])
        options.set(flag, [word, words[++i]]);
      else options.set(flag, [word]);
      occurrences.set(flag, [
        ...(occurrences.get(flag) || []),
        options.get(flag),
      ]);
    } else args.push(word.value);
  }
  const value = (key) => {
    const pair = options.get(key);
    return pair?.[1]?.value ?? pair?.[0]?.value.slice(key.length + 1);
  };
  return { options, occurrences, args, value };
}

// The salt (a session id) keeps reused agent names distinct across sessions.
function teammateFor(id, salt) {
  const base =
    id
      .toLowerCase()
      .replace(/[^a-z0-9_-]/g, "-")
      .replace(/^[^a-z0-9]+/, "") || "agent";
  return `${base.slice(0, 57)}-${digest(salt ? `${salt}:${id}` : id).slice(0, 6)}`;
}

const STOPWORDS = new Set([
  "a",
  "an",
  "and",
  "are",
  "as",
  "at",
  "be",
  "by",
  "do",
  "for",
  "from",
  "in",
  "into",
  "is",
  "it",
  "its",
  "of",
  "on",
  "or",
  "per",
  "the",
  "then",
  "this",
  "that",
  "to",
  "with",
  "you",
  "your",
  "above",
  "below",
  "all",
  "any",
  "please",
  "task",
  "plan",
  "subagent",
  "agent",
  "current",
  "complete",
  "finish",
]);
// Lowercase words with camelCase split and simple suffixes stripped; snake_case
// identifiers stay whole because they usually name the code under change.
function taskTokens(text) {
  const words = text
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .toLowerCase()
    .split(/[^a-z0-9_]+/);
  const tokens = [];
  for (let word of words) {
    word = word.replace(/^_+|_+$/g, "");
    if (word.length < 2 || STOPWORDS.has(word)) continue;
    if (word.length > 5 && word.endsWith("ing")) word = word.slice(0, -3);
    else if (word.length > 4 && word.endsWith("ed")) word = word.slice(0, -2);
    else if (word.length > 3 && word.endsWith("s") && !word.endsWith("ss"))
      word = word.slice(0, -1);
    if (STOPWORDS.has(word)) continue;
    tokens.push(word);
    if (word.length > 4 && word.endsWith("test")) tokens.push("test"); // unittest, pytest
  }
  return [...new Set(tokens)];
}

const escape = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
// The open task a text names: its title (case-insensitive, whole words; a one-
// or two-word title only quoted or as the whole text), its qualified ref, or its
// short id after a marker, since a bare id can be an ordinary word ("test"). A
// title inside a longer named title yields to the longer one.
function namedTask(tasks, text) {
  const titled = (title) => {
    const phrase = escape(title.trim());
    if (text.trim().toLowerCase() === title.trim().toLowerCase()) return true;
    if (
      new RegExp(
        `["'\`\u201c\u2018]\\s*${phrase}\\s*["'\`\u201d\u2019]`,
        "i",
      ).test(text)
    )
      return true;
    return (
      title.trim().split(/\s+/).length >= 3 &&
      new RegExp(`(?<![\\w])${phrase}(?![\\w])`, "i").test(text)
    );
  };
  const named = tasks.filter(
    (t) =>
      OPEN.has(t.status) &&
      (titled(t.content) ||
        new RegExp(`(?<![\\w.#-])${escape(t.ref)}(?![\\w-])`, "i").test(text) ||
        new RegExp(
          `(?:\\b(?:issue|kata|ref|task)\\b\\s*[:=]?\\s*|(?<![\\w.-])#)${escape(t.ref.split("#").pop())}(?![\\w-])`,
          "i",
        ).test(text)),
  );
  const longest = named.sort((a, b) => b.content.length - a.content.length)[0];
  if (
    longest &&
    named.every(
      (t) =>
        t === longest ||
        longest.content.toLowerCase().includes(t.content.toLowerCase()),
    )
  )
    return longest;
}

// Pick the plan task a delegation serves. A named task wins, and a named task
// that is taken binds nothing else. Then a clear weighted token overlap with
// the short label (omp's task name) wins; then, for a single delegation, the one
// open task the lead marked for delegation ("... (delegated to subagent)") if it
// shares a word with the spawn; then a clear overlap with the full text. Overlap
// counts the task's leading word twice. The lead's active task scores
// at half weight and is the fallback only for an explicit current-task handoff.
function matchDelegation(
  tasks,
  text,
  { unavailable = new Set(), single = true, label = "" } = {},
) {
  const available = (t) =>
    OPEN.has(t.status) &&
    t.status !== "blocked" &&
    !t.delegated &&
    !unavailable.has(t.ref);
  const full = label ? `${label}\n${text}` : text;
  const named = namedTask(tasks, full);
  if (named) return available(named) ? named : undefined;
  const candidates = tasks.filter(available);
  const open = tasks
    .filter((t) => OPEN.has(t.status))
    .map((t) => taskTokens(t.content));
  const df = (token) => open.filter((tokens) => tokens.includes(token)).length;
  const weight = (token) =>
    Math.log((open.length + 1) / Math.max(1, df(token)));
  const overlap = (source) => {
    const wanted = new Set(taskTokens(source));
    // Coverage counts against the task and against the source's plan vocabulary.
    const vocabulary = [...wanted]
      .filter((token) => df(token) > 0)
      .reduce((sum, token) => sum + weight(token), 0);
    const scored = candidates
      .map((task) => {
        const own = taskTokens(task.content),
          shared = own.filter((token) => wanted.has(token));
        const total = own.reduce((sum, token) => sum + weight(token), 0),
          common = shared.reduce((sum, token) => sum + weight(token), 0);
        const score =
          (common + (own[0] && wanted.has(own[0]) ? weight(own[0]) : 0)) *
          (task.status === "in_progress" ? 0.5 : 1);
        return {
          task,
          score,
          eligible:
            shared.length >= 2 &&
            common > 0 &&
            Math.max(common / total, common / vocabulary) >= 0.3,
        };
      })
      .sort((a, b) => b.score - a.score);
    const [best, runner] = scored;
    return best?.eligible && (!runner || best.score >= runner.score * 1.5)
      ? best.task
      : undefined;
  };
  const byLabel = label && overlap(label);
  if (byLabel) return byLabel;
  const wanted = new Set(taskTokens(full));
  const marked = candidates.filter((t) =>
    /\b(?:delegat\w*|sub-?agents?)\b/i.test(t.content),
  );
  if (
    single &&
    marked.length === 1 &&
    taskTokens(marked[0].content).some((token) => wanted.has(token))
  )
    return marked[0];
  const found = overlap(full);
  if (found) return found;
  const active = candidates.filter((t) => t.status === "in_progress");
  if (
    single &&
    active.length === 1 &&
    /\b(?:current|active|this)\s+(?:task|todo|item|step)\b/i.test(full)
  )
    return active[0];
}

function phasesOf(tasks) {
  const phases = [];
  for (const task of tasks) {
    let phase = phases.find((p) => p.name === task.phase);
    if (!phase) phases.push((phase = { name: task.phase, tasks: [] }));
    phase.tasks.push(task);
  }
  return phases;
}

function render(plan) {
  if (plan.error)
    return `Kata state unavailable${plan.parent ? ` for ${plan.parent}` : ""}: ${plan.error}. Use todo op=view to refresh.`;
  if (!plan.parent && !plan.local) return "No todo plan. Use todo op=init.";
  if (plan.tasks.length === 0) return `Kata parent ${plan.parent}: no tasks.`;
  const done = plan.tasks.filter((t) => t.status === "completed").length;
  const lines = [
    `${plan.local ? "Session-local plan" : `Kata parent ${plan.parent}`}: ${done}/${plan.tasks.length} done`,
  ];
  for (const phase of phasesOf(plan.tasks)) {
    lines.push(`${phase.name}:`);
    for (const t of phase.tasks)
      lines.push(
        ` ${MARK[t.status]} ${t.content} (${t.ref})${t.blocker ? ` — blocked: ${t.blocker}` : ""}`,
      );
  }
  return lines.join("\n");
}

function createKataCore(host, options) {
  const namespace = options.namespace;
  const STATE_ENTRY = `io.kata.${namespace}.parent`;
  const SNAPSHOT_ENTRY = `io.kata.${namespace}.state`;
  const WIDGET = `${namespace}-kata`;
  const MAX_REMINDERS = 3;
  const main = (ctx) => host.isMain?.(ctx) ?? true;
  let plan = { tasks: [] };
  let queue = Promise.resolve();
  let operationSignal;
  let scope = "unknown";
  let currentId;
  let bound = false;
  let assignedRef;
  let requestedRef;
  let sessionTeammate;
  let boundAgent;
  const agentKey = (ctx) => JSON.stringify([ctx?.agent?.kind, ctx?.agent?.id]);
  let currentCtx;
  let reminders = 0;
  let progressed = true;
  const autoParents = new Map();
  const retries = new Map();
  let retryTimer;
  let lastSnapshot;
  function persist() {
    if (host.current?.() === false || !currentId || !main(currentCtx)) return;
    const data = {
      version: 1,
      sessionId: currentId,
      plan: structuredClone(plan),
      autoParents: [...autoParents.values()],
      retries: [...retries.entries()],
    };
    const encoded = JSON.stringify(data);
    if (encoded === lastSnapshot) return;
    host.appendEntry(SNAPSHOT_ENTRY, data);
    lastSnapshot = encoded;
  }
  function checkCancellation() {
    if (operationSignal?.aborted)
      throw operationSignal.reason || new Error("Kata todo operation canceled");
  }
  function debug(error) {
    if (!options.debugFile) return;
    try {
      appendFileSync(
        options.debugFile,
        `${new Date().toISOString()} ${String(error)}\n`,
      );
    } catch {
      /* Debugging must never interrupt work. */
    }
  }
  async function bounded(promise, ms = 250) {
    let timer;
    try {
      return await Promise.race([
        promise.catch((error) => {
          debug(error);
          return undefined;
        }),
        new Promise((resolve) => {
          timer = setTimeout(() => resolve(undefined), ms);
        }),
      ]);
    } finally {
      clearTimeout(timer);
    }
  }

  async function kata(
    args,
    cwd,
    timeout = 120_000,
    workspace = true,
    { durable = false } = {},
  ) {
    if (!durable) checkCancellation();
    const routing = [];
    if (sessionTeammate !== undefined && args[0] !== "agent-contract-hook")
      routing.push("--teammate", sessionTeammate);
    if (workspace) routing.push("--workspace", path.resolve(cwd));
    if (!durable && host.current?.() === false)
      throw new Error("Kata plan generation inactive");
    if (!durable) checkCancellation();
    if (
      [
        "create",
        "claim",
        "meta",
        "close",
        "reopen",
        "edit",
        "label",
        "notify",
        "comment",
        "unassign",
      ].includes(args[0])
    )
      await checkDaemonFloor(cwd, { durable });
    if (!durable && host.current?.() === false)
      throw new Error("Kata plan generation inactive");
    const separator = args.indexOf("--");
    const invocation =
      separator < 0
        ? [...args, ...routing]
        : [
            ...args.slice(0, separator),
            ...routing,
            "--",
            ...args.slice(separator + 1),
          ];
    const result = await host.exec(
      options.executable,
      invocation,
      { cwd, timeout, signal: durable ? undefined : operationSignal },
    );
    if (!durable) checkCancellation();
    if (!durable && host.current?.() === false)
      throw new Error("Kata plan generation inactive");
    if (result.code !== 0 || result.killed) {
      const detail = (result.stderr || result.stdout || "")
        .trim()
        .split("\n")
        .slice(-3)
        .join(" ");
      throw new Error(
        `kata ${args[0]} ${result.killed ? `timed out after ${timeout}ms` : "failed"}${detail ? `: ${detail}` : ""}`,
      );
    }
    return result.stdout;
  }

  // Serialize every plan mutation: tool calls, /todo, and session reloads.
  function serial(fn) {
    const run = queue.then(() => {
      if (host.current?.() === false)
        throw new Error("Kata plan generation inactive");
      return fn();
    });
    queue = run.catch(() => undefined);
    return run;
  }

  let actorSelection, actorValue, floorSelection;
  function selection(cwd) {
    return (process.env.KATA_SERVER ?? "local") + "\0" + path.resolve(cwd);
  }
  async function effectiveActor(ref, cwd) {
    const key = selection(cwd);
    if (actorSelection !== key) {
      actorSelection = key;
      actorValue = undefined;
    }
    if (!actorValue) {
      const status = JSON.parse(
        await kata(["status", ...(ref ? [ref] : []), "--json"], cwd),
      );
      if (typeof status.actor !== "string" || !status.actor)
        throw new Error("Kata effective actor unavailable");
      actorValue = status.actor;
    }
    return actorValue;
  }
  async function checkDaemonFloor(cwd, { durable = false } = {}) {
    const key = selection(cwd);
    if (floorSelection === key) return;
    const health = JSON.parse(
      await kata(["health", "--json"], cwd, 120_000, true, { durable }),
    );
    const version = health.api_schema_version;
    const parse = (value) =>
      /^\d+\.\d+\.\d+$/.test(value || "") ? value.split(".").map(Number) : [];
    const actual = parse(version),
      required = parse(options.minDaemonAPI || "0.15.0");
    let comparison = 0;
    for (let i = 0; i < 3 && !comparison; i++)
      comparison = actual[i] - required[i];
    if (actual.length !== 3 || comparison < 0)
      throw new Error(
        `Kata daemon ${version || "unknown"} is too old for the plan layer (needs ${options.minDaemonAPI || "0.15.0"})`,
      );
    floorSelection = key;
  }

  // A successful claim is required for unowned tasks. Already-owned tasks
  // require both the actor and teammate, since delegates share an actor.
  async function owned(ref, cwd, mayClaim = false, allowClosed = false) {
    let issue = JSON.parse(await kata(["show", ref, "--json"], cwd)).issue;
    const currentActor = await effectiveActor(ref, cwd);
    const teammate = sessionTeammate ?? process.env.KATA_TEAMMATE ?? "";
    if (
      issue?.status !== "open" &&
      !(allowClosed && issue?.status === "closed")
    )
      throw new Error(
        `Kata issue ${ref} is closed or unavailable; no work metadata was written.`,
      );
    const assignedTeammate = issue.metadata?.teammate || "";
    const ownTeammate = (value) =>
      sessionTeammate !== undefined
        ? (value.metadata?.teammate || "") === sessionTeammate
        : classifyTask(value, {
            namespace,
            leadTeammate: teammate,
            harness: namespace,
          }) === "lead";
    if (!ownTeammate(issue) && (issue.owner || assignedTeammate)) {
      throw new Error(
        `Kata issue ${ref} belongs to teammate ${assignedTeammate || "(none)"}, not ${teammate || "(none)"}; no work metadata was written.`,
      );
    }
    if (!issue.owner && mayClaim) {
      // Never swallow a claim failure, including an ownership race.
      await kata(["claim", ref, "--if-unowned"], cwd);
      issue = JSON.parse(await kata(["show", ref, "--json"], cwd)).issue;
      if (issue.status !== "open" || issue.owner !== currentActor)
        throw new Error(
          `Kata issue ${ref} is no longer owned by ${currentActor}; no work metadata was written.`,
        );
      if (teammate && !issue.metadata?.teammate) {
        await kata(
          [
            "meta",
            "set",
            ref,
            "teammate",
            teammate,
            "--if-absent",
            "--if-match",
            String(issue.revision),
          ],
          cwd,
        );
        issue = JSON.parse(await kata(["show", ref, "--json"], cwd)).issue;
      }
    }
    if (
      (issue.status !== "open" &&
        !(allowClosed && issue.status === "closed")) ||
      issue.owner !== currentActor ||
      !ownTeammate(issue)
    ) {
      throw new Error(
        `Kata issue ${ref} is not owned by actor ${currentActor} and teammate ${teammate || "(none)"}; no work metadata was written.`,
      );
    }
    if (!Number.isInteger(issue.revision) || issue.revision < 0)
      throw new Error(
        `Kata issue ${ref} has no valid metadata revision; no work metadata was written.`,
      );
    return issue;
  }

  // Keep the decision snapshot through every key write. A fresh lookup may
  // validate it, but must not rebase the intended transition onto new state.
  async function work(ref, key, value, cwd, expected, allowClosed = false) {
    const issue = await owned(ref, cwd, false, allowClosed);
    const meta = expected.metadata ?? {};
    if (
      issue.revision !== expected.revision ||
      Object.hasOwn(issue.metadata ?? {}, key) !== Object.hasOwn(meta, key) ||
      issue.metadata?.[key] !== meta[key]
    ) {
      throw new Error(
        `Kata metadata changed for ${ref}; no further work metadata was written.`,
      );
    }
    const exists = Object.hasOwn(meta, key);
    if ((!exists && value === undefined) || meta[key] === value)
      return expected;
    const args =
      value === undefined
        ? ["meta", "unset", ref, key]
        : ["meta", "set", ref, key, JSON.stringify(value)];
    args.push("--if-match", String(expected.revision));
    if (exists)
      args.push("--if-value", JSON.stringify(meta[key]), "--json-value");
    else args.push("--if-absent", "--json-value");
    await kata(args, cwd);
    const changed = { ...meta };
    if (value === undefined) delete changed[key];
    else changed[key] = value;
    return { ...expected, revision: expected.revision + 1, metadata: changed };
  }

  // -------------------------------------------------------------- plan state

  function issueTaskStatus(issue) {
    const meta = issue.metadata ?? {};
    if (issue.status === "closed") {
      return issue.closed_reason === "wontfix" ||
        meta["todo.status"] === "abandoned"
        ? "abandoned"
        : "completed";
    }
    return ["in_progress", "blocked"].includes(meta["todo.status"])
      ? meta["todo.status"]
      : "pending";
  }

  function taskOrder(issue) {
    const value = issue?.metadata?.["todo.order"];
    if (
      typeof value !== "number" &&
      (typeof value !== "string" || !value.trim())
    )
      return undefined;
    const order = typeof value === "number" ? value : Number(value);
    return Number.isSafeInteger(order) && order >= 0 ? order : undefined;
  }

  async function setTaskOrder(task, order, cwd) {
    if (task.order === order) return;
    const decision = await owned(task.ref, cwd, task.status === "pending", true);
    if (taskOrder(decision) !== order)
      await work(task.ref, "todo.order", order, cwd, decision, true);
    task.order = order;
  }

  async function load(cwd) {
    if (plan.local) return;
    if (!plan.parent) {
      plan.tasks = [];
      return;
    }
    try {
      const { issue, children = [] } = JSON.parse(
        await kata(["show", plan.parent, "--json"], cwd),
      );
      if (
        !issue ||
        !Array.isArray(children) ||
        children.some(
          (c) =>
            !c || typeof c.title !== "string" || typeof c.short_id !== "string",
        )
      ) {
        throw new Error(
          "kata show returned an invalid issue or children response",
        );
      }
      plan.tasks = children
        .filter(
          (c) =>
            typeof c?.metadata?.["todo.phase"] === "string" &&
            c.metadata["todo.removed"] !== true,
        )
        .sort((a, b) => {
          const left = taskOrder(a);
          const right = taskOrder(b);
          if (left !== undefined && right !== undefined && left !== right)
            return left - right;
          if (left !== undefined && right === undefined) return -1;
          if (left === undefined && right !== undefined) return 1;
          return String(a.created_at).localeCompare(String(b.created_at));
        })
        .map((c) => {
          const meta = c.metadata;
          const status = issueTaskStatus(c);
          return {
            ref: c.qualified_id || `${plan.parent.split("#")[0]}#${c.short_id}`,
            content: c.title,
            phase: meta["todo.phase"],
            order: taskOrder(c),
            metadata: meta,
            delegated:
              classifyTask(c, {
                namespace,
                leadTeammate:
                  sessionTeammate ?? process.env.KATA_TEAMMATE ?? "",
                harness: namespace,
              }) === "delegate",
            review: (c.labels ?? []).some(
              (l) => (typeof l === "string" ? l : l?.label) === "needs-review",
            ),
            status,
            blocker:
              status === "blocked" ? meta["work.attention_msg"] : undefined,
          };
        });
      delete plan.error;
    } catch (error) {
      plan.error = error.message;
      throw error;
    }
  }

  function refresh(ctx) {
    if (
      host.current?.() === false ||
      !main(ctx) ||
      host.allowUI?.(ctx) === false
    )
      return;
    const ui = ctx?.ui;
    if (!ui?.setWidget) return;
    if (plan.error) {
      ui.setWidget(WIDGET, [render(plan)]);
      return;
    }
    if (!plan.parent || plan.tasks.length === 0) {
      ui.setWidget(WIDGET, undefined);
      return;
    }
    const done = plan.tasks.filter((t) => t.status === "completed").length;
    const active = plan.tasks.find((t) => t.status === "in_progress");
    const blocked = plan.tasks.filter((t) => t.status === "blocked").length;
    const line = `Kata ${plan.parent} · ${done}/${plan.tasks.length} done${blocked ? ` · ${blocked} blocked` : ""}${active ? ` · ▶ ${active.content}` : ""}`;
    ui.setWidget(WIDGET, [line]);
  }

  async function probe(cwd) {
    if (scope !== "unknown") return scope === "bound";
    try {
      await kata(["list", "--limit", "1", "--json"], cwd);
      await checkDaemonFloor(cwd);
      await effectiveActor(undefined, cwd);
      scope = "bound";
      return true;
    } catch (error) {
      if (/\bproject_not_initialized\b/.test(String(error))) {
        scope = "local";
        return false;
      }
      throw error;
    }
  }

  async function pin(ref, cwd) {
    const shown = JSON.parse(await kata(["show", ref, "--json"], cwd));
    await checkDaemonFloor(cwd);
    await effectiveActor(ref, cwd);
    if (ref.includes("#")) return ref;
    const projects = JSON.parse(
      await kata(["projects", "list", "--json"], cwd),
    ).projects;
    const project = projects.find((p) => p.id === shown.issue.project_id);
    if (!project) throw new Error("Kata issue project could not be pinned.");
    return `${project.name}#${shown.issue.short_id}`;
  }

  async function sessionBinding(ctx) {
    const id = ctx.sessionManager?.getSessionId?.();
    if (!id) return;
    const list =
      JSON.parse(
        await kata(
          [
            "list",
            "--status",
            "all",
            "--meta",
            `${namespace}.session_id=${id}`,
            "--label",
            `${namespace}-session`,
            "--limit",
            "2",
            "--json",
          ],
          ctx.cwd,
        ),
      ).issues ?? [];
    const candidates = list.filter(
      (issue) =>
        issue.metadata?.[`${namespace}.auto_parent`] === true &&
        issue.metadata?.[`${namespace}.session_id`] === id &&
        sessionKey(
          issue.metadata?.[`${namespace}.workspace`] || ".",
          ctx.cwd,
        ) === sessionKey(ctx.cwd, ctx.cwd),
    );
    if (candidates.length === 1)
      return {
        parent: candidates[0].qualified_id,
        workspace: candidates[0].metadata[`${namespace}.workspace`],
        sessionId: id,
        auto: true,
      };
  }

  async function bind(
    ctx,
    explicit = options.explicitRef?.() ?? process.env.KATA_REF,
    preferExplicit = false,
  ) {
    if (!main(ctx) && !host.binding) return;
    bound = false;
    boundAgent = agentKey(ctx);
    const assigned = await host.binding?.(ctx);
    sessionTeammate = assigned?.teammate;
    explicit = preferExplicit && main(ctx)
      ? explicit ?? assigned?.ref
      : assigned?.ref ?? explicit;
    assignedRef = assigned?.ref;
    requestedRef = explicit;
    const id = ctx.sessionManager?.getSessionId?.();
    currentId = id;
    lastSnapshot = undefined;
    currentCtx = ctx;
    plan = { tasks: [] };
    scope = "unknown";
    retries.clear();
    autoParents.clear();
    if (retryTimer) {
      clearTimeout(retryTimer);
      retryTimer = undefined;
    }
    let snapshot;
    for (const entry of ctx.sessionManager?.getBranch?.() ?? []) {
      if (
        entry?.type === "custom" &&
        entry.customType === SNAPSHOT_ENTRY &&
        entry.data?.version === 1 &&
        entry.data.sessionId === id
      )
        snapshot = entry.data;
    }
    if (snapshot) {
      for (const binding of snapshot.autoParents ?? [])
        autoParents.set(binding.parent, binding);
      for (const [ref, pending] of snapshot.retries ?? [])
        retries.set(ref, pending);
      if (retries.size) scheduleRetry();
      if (snapshot.plan?.local && !explicit)
        plan = structuredClone(snapshot.plan);
    }
    let stored = snapshot?.plan?.parent ? snapshot.plan : undefined;
    for (const entry of ctx.sessionManager?.getBranch?.() ?? []) {
      if (
        entry?.type === "custom" &&
        entry.customType === STATE_ENTRY &&
        typeof entry.data?.parent === "string" &&
        (!entry.data.sessionId || entry.data.sessionId === id)
      )
        stored = entry.data;
    }
    if (!explicit && !stored) {
      if (!(await probe(ctx.cwd))) {
        await drainRetries();
        bound = true;
        refresh(ctx);
        return;
      }
      if (id) stored = await sessionBinding(ctx);
    }
    const parent = explicit || stored?.parent;
    if (parent) {
      const workspace =
        preferExplicit && explicit
          ? path.resolve(ctx.cwd)
          : assigned?.workspace ||
            (explicit
              ? path.resolve(ctx.cwd)
              : stored.workspace || path.resolve(ctx.cwd));
      const qualified = await pin(parent, workspace);
      const issue = JSON.parse(
        await kata(["show", qualified, "--json"], workspace),
      ).issue;
      const auto =
        !explicit &&
        stored?.auto !== false &&
        issue.metadata?.[`${namespace}.auto_parent`] === true &&
        issue.metadata?.[`${namespace}.session_id`] === id;
      setParent({ parent: qualified, workspace, sessionId: id, auto });
      await load(workspace);
    }
    await drainRetries();
    bound = true;
    refresh(ctx);
  }

  function setParent(binding) {
    scope = "bound";
    if (!binding.auto) autoParents.delete(binding.parent);
    if (
      plan.parent === binding.parent &&
      plan.workspace === binding.workspace &&
      plan.auto === binding.auto
    )
      return;
    plan = {
      ...binding,
      tasks: plan.parent === binding.parent ? plan.tasks : [],
    };
    if (binding.auto) autoParents.set(binding.parent, binding);
    host.appendEntry(STATE_ENTRY, binding);
  }

  async function adopt(ref, ctx, workspace = ctx.cwd) {
    const parent = await pin(ref, workspace);
    if (plan.auto && plan.parent && plan.parent !== parent) {
      // An automatic parent with no children only marks the session; retire it
      // rather than leave an empty child that can never auto-close.
      const shown = JSON.parse(
        await kata(["show", plan.parent, "--json"], plan.workspace),
      );
      const oldOwner = await owned(plan.parent, plan.workspace, false, true);
      if (
        oldOwner.revision !== shown.issue.revision ||
        oldOwner.status !== shown.issue.status
      )
        throw new Error(
          `Automatic parent ${plan.parent} changed before adoption; no parent update was written.`,
        );
      if (shown.issue.status === "open" && !shown.children?.length) {
        await kata(
          [
            "close",
            plan.parent,
            "--superseded-by",
            parent,
            "--if-match",
            String(oldOwner.revision),
            "--message",
            `This session adopted ${parent} as its plan parent.`,
          ],
          plan.workspace,
        );
        autoParents.delete(plan.parent);
      }
      // `kata edit` has no revision precondition. Leave populated automatic
      // parent links untouched; the session binding below still follows the
      // selected issue without racing another owner’s relationship update.
    }
    if (plan.parent === parent) autoParents.delete(parent);
    setParent({
      parent,
      workspace: path.resolve(workspace),
      sessionId: ctx.sessionManager?.getSessionId?.(),
      auto: false,
    });
    await load(workspace);
  }

  // ------------------------------------------------------------- transitions

  async function create(phase, content, cwd, order) {
    const project = plan.parent.split("#")[0];
    const identity = `${namespace}:${digest(plan.parent, phase, content)}`;
    const shown = JSON.parse(
      await kata(["show", plan.parent, "--json"], cwd),
    );
    const matches = (shown.children ?? []).filter(
      (child) =>
        child.metadata?.["todo.identity"] === identity ||
        (!child.metadata?.["todo.identity"] &&
          child.title === content &&
          child.metadata?.["todo.phase"] === phase),
    );
    if (matches.length > 1)
      throw new Error(
        `Kata task "${content}" has multiple children with the same durable identity; no child was changed.`,
      );
    const existing = matches[0];
    const existingRef = existing
      ? existing.qualified_id ||
        (existing.project_id === shown.issue?.project_id
          ? `${project}#${existing.short_id}`
          : undefined)
      : undefined;
    if (existing && !existingRef)
      throw new Error("existing Kata task has no qualified project reference");
    const issue = existing
      ? JSON.parse(await kata(["show", existingRef, "--json"], cwd)).issue
      : JSON.parse(
          await kata(
            [
              "create",
              "--parent",
              plan.parent,
              "--project",
              project,
              "--body",
              `Todo task in phase "${phase}".`,
              "--meta",
              `todo.phase=${phase}`,
              "--meta",
              "todo.status=pending",
              "--meta",
              `todo.identity=${identity}`,
              ...(order === undefined ? [] : ["--meta", `todo.order=${order}`]),
              "--idempotency-key",
              `${namespace}-kata:${digest(plan.parent, phase, content)}`,
              // Sibling tasks often read alike ("write x" / "verify x");
              // they are deliberate plan items, so skip the look-alike soft-block.
              "--force-new",
              "--json",
              "--",
              content,
            ],
            cwd,
          ),
        ).issue;
    if (typeof issue?.short_id !== "string")
      throw new Error("kata create returned no short_id");
    const taskRef = existing
      ? existingRef
      : `${project}#${issue.short_id}`;
    let decision;
    // Create's stable key can return an existing closed/removed child. Freeze
    // that replay decision before restoring it, just as explicit restart does.
    if (
      issue.status === "closed" ||
      issue.metadata?.["todo.removed"] === true
    ) {
      decision = await owned(taskRef, cwd, false, true);
      if (
        decision.revision !== issue.revision ||
        decision.status !== issue.status
      ) {
        throw new Error(
          `Kata task ${taskRef} changed before replay restoration; no restoration was written.`,
        );
      }
      if (decision.status === "closed") {
        const closed = decision;
        // Reopen has no conditional API. A handoff across it can leave the
        // child open, but must never authorize following metadata writes.
        await kata(["reopen", taskRef], cwd);
        decision = await owned(taskRef, cwd);
        if (decision.revision !== closed.revision + 1) {
          throw new Error(
            `Kata task ${taskRef} changed during replay restart; no status or removal metadata was written.`,
          );
        }
        decision = await work(
          taskRef,
          "todo.status",
          "pending",
          cwd,
          decision,
        );
      }
      if (issue.metadata?.["todo.removed"] === true)
        decision = await work(
          taskRef,
          "todo.removed",
          undefined,
          cwd,
          decision,
        );
    }
    const metadata = decision?.metadata ?? issue.metadata ?? {};
    if (
      metadata["todo.identity"] !== identity ||
      (order !== undefined && taskOrder({ metadata }) !== order)
    ) {
      decision ??= await owned(
        taskRef,
        cwd,
        issue.status === "open",
        issue.status === "closed",
      );
      if (decision.metadata?.["todo.identity"] !== identity)
        decision = await work(
          taskRef,
          "todo.identity",
          identity,
          cwd,
          decision,
          true,
        );
      if (order !== undefined && taskOrder(decision) !== order)
        await work(taskRef, "todo.order", order, cwd, decision, true);
    }
  }

  async function transition(task, to, change, cwd) {
    const from = task.status;
    if (
      !change.remove &&
      ["in_progress", "pending", "blocked"].includes(to) &&
      retries.delete(task.ref)
    )
      persist();
    if (from === to && to !== "blocked") return;
    const ref = task.ref;
    if (task.delegated || host.isDelegated?.(ref)) {
      throw new Error(
        `Kata task ${ref} is delegated. Its subagent must manage and close it; use view to refresh its state.`,
      );
    }
    if (plan.local) {
      task.status = to;
      task.blocker =
        to === "blocked"
          ? change.reason || "Blocked in the local plan."
          : undefined;
      return;
    }
    let decision;
    if (!OPEN.has(from) && to === "in_progress") {
      // Explicit start may restart only this actor's and teammate's closed
      // task. Kata reopen has no CAS flag; validate its revision and owner
      // again afterward before any guarded work metadata is written.
      const closed = await owned(ref, cwd, false, true);
      if (
        closed.status !== "closed" ||
        issueTaskStatus(closed) !== from
      )
        throw new Error(
          `Kata task ${ref} changed since the plan was loaded; refresh before restarting it.`,
        );
      await kata(["reopen", ref], cwd);
      const reopened = await owned(ref, cwd);
      if (
        reopened.status !== "open" ||
        reopened.revision !== closed.revision + 1
      )
        throw new Error(
          `Kata task ${ref} changed during restart; no work metadata was written.`,
        );
      decision = reopened;
    } else {
      decision = await owned(ref, cwd, true);
      if (issueTaskStatus(decision) !== from)
        throw new Error(
          `Kata task ${ref} changed since the plan was loaded; refresh before changing its status.`,
        );
    }
    switch (to) {
      case "in_progress":
        decision = await work(ref, "work.attention", "ok", cwd, decision);
        if (from === "blocked")
          decision = await work(
            ref,
            "work.attention_msg",
            undefined,
            cwd,
            decision,
          );
        await work(ref, "todo.status", "in_progress", cwd, decision);
        break;
      case "pending":
        if (from === "blocked") {
          decision = await work(ref, "work.attention", "ok", cwd, decision);
          decision = await work(
            ref,
            "work.attention_msg",
            undefined,
            cwd,
            decision,
          );
        }
        await work(ref, "todo.status", "pending", cwd, decision);
        break;
      case "blocked": {
        const reason = change.reason || "Blocked in the todo plan.";
        decision = await work(ref, "work.attention", "stuck", cwd, decision);
        decision = await work(ref, "work.attention_msg", reason, cwd, decision);
        await work(ref, "todo.status", "blocked", cwd, decision);
        task.blocker = reason;
        break;
      }
      case "completed": {
        const message = change.message
          ? `${change.message} (task: ${task.content}; ${task.ref})`
          : "";
        if (
          !change.evidence?.length ||
          (change.message || "").replace(/\s+/g, " ").trim().length < 40
        ) {
          throw new Error(
            `Kata needs evidence and a 40+ character message to close ${ref} ("${task.content}") as done: pass evidence (commit:<sha>, test:<cmd>, pr:<url>, reviewed-paths:<path>) and a message saying what changed and how it was verified. If it is not verified, block it instead.`,
          );
        }
        const args = [
          "close",
          ref,
          "--done",
          "--if-match",
          String(decision.revision),
          "--message",
          message,
        ];
        for (const e of change.evidence) args.push("--evidence", e);
        await closeTask(args, task, to, change, cwd, decision);
        const closed = await owned(ref, cwd, false, true);
        if (closed.revision !== decision.revision + 1)
          throw new Error(
            `Kata task ${ref} changed after close; no status metadata was written.`,
          );
        await work(ref, "todo.status", "completed", cwd, closed, true);
        // Kata accepted the close with evidence; an earlier review request is settled.
        if (task.review) {
          await kata(["label", "rm", ref, "needs-review"], cwd);
          task.review = false;
        }
        break;
      }
      case "abandoned": {
        await closeTask(
          [
            "close",
            ref,
            "--wontfix",
            "--if-match",
            String(decision.revision),
            "--message",
            wontfix("Dropped", task, change),
          ],
          task,
          to,
          change,
          cwd,
          decision,
        );
        const closed = await owned(ref, cwd, false, true);
        if (closed.revision !== decision.revision + 1)
          throw new Error(
            `Kata task ${ref} changed after close; no status metadata was written.`,
          );
        await work(ref, "todo.status", "abandoned", cwd, closed, true);
        break;
      }
    }
    task.status = to;
    if (!change.remove) retries.delete(ref);
    if (to !== "blocked") task.blocker = undefined;
  }

  async function preflightStart(task, cwd) {
    const ref = task.ref;
    if (task.delegated || host.isDelegated?.(ref))
      throw new Error(
        `Kata task ${ref} is delegated. Its subagent must manage and close it; use view to refresh its state.`,
      );
    if (task.status === "blocked")
      throw new Error(`"${task.content}" is blocked; unblock it first.`);
    if (plan.local) return;
    const closed = !OPEN.has(task.status);
    const decision = await owned(
      ref,
      cwd,
      task.status === "pending",
      closed,
    );
    if (issueTaskStatus(decision) !== task.status)
      throw new Error(
        `Kata task ${ref} changed since the plan was loaded; refresh before starting it.`,
      );
  }

  // Kata wants 60+ characters for wontfix; the task and plan context supply
  // that around the caller's reason.
  function wontfix(verb, task, change) {
    const why = change.message || change.reason || "no longer needed";
    return `${verb} from the todo plan under ${plan.parent}: "${task.content}". Reason: ${why}. This task was intentionally retired from the plan without asserting completion.`;
  }

  // A delegated task is the subagent's; it never occupies the lead's slot.
  const leads = (t) => !t.delegated && !host.isDelegated?.(t.ref);

  async function delegate(
    ref,
    cwd,
    { id, teammate, leadSessionKey, expectedStatus },
  ) {
    let decision = await owned(ref, cwd, true);
    const status = issueTaskStatus(decision);
    if (status === "blocked" || (expectedStatus && status !== expectedStatus))
      throw new Error(
        `Kata task ${ref} changed from ${expectedStatus || "available"} to ${status}; no worker assignment was written.`,
      );
    if (decision.metadata?.["omp.subagent_id"] !== undefined)
      throw new Error(
        `Kata task ${ref} already has a worker assignment; no worker assignment was written.`,
      );
    const pendingTerminal = decision.metadata?.["omp.terminal_state"];
    if (pendingTerminal !== undefined) {
      await clearDelegateTerminal(ref, cwd, {
        id: pendingTerminal?.id,
        leadSessionKey: pendingTerminal?.leadSessionKey,
      });
      decision = JSON.parse(await kata(["show", ref, "--json"], cwd)).issue;
      if (decision.metadata?.["omp.terminal_state"] !== undefined)
        throw new Error(
          `Kata task ${ref} has pending delegate hand-back state; no new worker was assigned.`,
        );
    }
    if (typeof decision.owner !== "string" || !decision.owner)
      throw new Error(`Kata task ${ref} has no assignment actor.`);
    decision = await work(
      ref,
      "omp.lead_actor",
      decision.owner,
      cwd,
      decision,
    );
    if (typeof leadSessionKey === "string" && leadSessionKey)
      decision = await work(
        ref,
        "omp.lead_session_key",
        leadSessionKey,
        cwd,
        decision,
      );
    const marked = await work(ref, "omp.subagent_id", id, cwd, decision);
    await kata(
      [
        "meta",
        "set",
        ref,
        "teammate",
        teammate,
        "--if-match",
        String(marked.revision),
      ],
      cwd,
    );
  }

  async function recordDelegateTerminal(
    ref,
    cwd,
    {
      id,
      state,
      leadTeammate,
      leadSessionKey,
      planParent,
      workspace,
      durable = false,
    },
  ) {
    if (
      !id ||
      !leadSessionKey ||
      !["completed", "failed", "aborted"].includes(state)
    )
      throw new Error("Invalid terminal worker record");
    // Persist terminal assignment metadata even after its lead generation is
    // replaced. The guarded writes below verify the exact worker assignment
    // and issue revision before they can survive that lifecycle boundary.
    const terminalKata = (args) =>
      kata(args, cwd, 120_000, true, { durable });
    const decision = JSON.parse(
      await terminalKata(["show", ref, "--json"]),
    ).issue;
    if (
      decision.metadata?.["omp.subagent_id"] !== id ||
      decision.metadata?.["omp.lead_session_key"] !== leadSessionKey
    )
      return false;
    if (decision.status === "closed") return true;
    const assignmentActor = decision.metadata?.["omp.lead_actor"];
    if (
      typeof assignmentActor !== "string" ||
      !assignmentActor ||
      decision.owner !== assignmentActor
    )
      return false;
    const status = JSON.parse(
      await terminalKata(["status", ref, "--json"]),
    );
    if (status.actor !== assignmentActor) return false;
    const terminal = {
      id,
      state,
      leadTeammate: leadTeammate || "",
      leadSessionKey,
      planParent: planParent || plan.parent,
      ref,
      workspace: path.resolve(workspace || cwd),
    };
    const terminalKey = "omp.terminal_state";
    const args = ["meta", "set", ref, terminalKey, JSON.stringify(terminal)];
    args.push("--if-match", String(decision.revision));
    if (Object.hasOwn(decision.metadata ?? {}, terminalKey))
      args.push(
        "--if-value",
        JSON.stringify(decision.metadata[terminalKey]),
        "--json-value",
      );
    else args.push("--if-absent", "--json-value");
    await terminalKata(args);
    const saved = JSON.parse(
      await terminalKata(["show", ref, "--json"]),
    ).issue;
    const confirmed = saved.metadata?.[terminalKey];
    if (
      confirmed?.id !== id ||
      confirmed?.leadSessionKey !== leadSessionKey ||
      confirmed?.state !== state ||
      confirmed?.planParent !== terminal.planParent ||
      confirmed?.ref !== ref ||
      confirmed?.workspace !== terminal.workspace
    )
      throw new Error(`Kata task ${ref} did not retain its terminal worker state`);
    return true;
  }

  async function clearDelegateTerminal(
    ref,
    cwd,
    { id, leadSessionKey, durable = false },
  ) {
    const terminalKata = (args) =>
      kata(args, cwd, 120_000, true, { durable });
    const issue = JSON.parse(
      await terminalKata(["show", ref, "--json"]),
    ).issue;
    const terminal = issue.metadata?.["omp.terminal_state"];
    if (
      terminal?.id !== id ||
      terminal?.leadSessionKey !== leadSessionKey
    )
      return;
    await terminalKata(
      [
        "meta",
        "unset",
        ref,
        "omp.terminal_state",
        "--if-match",
        String(issue.revision),
        "--if-value",
        JSON.stringify(terminal),
        "--json-value",
      ],
      cwd,
    );
  }

  async function clearDelegateActor(
    ref,
    cwd,
    { actor, durable = false },
  ) {
    if (typeof actor !== "string" || !actor) return;
    const terminalKata = (args) =>
      kata(args, cwd, 120_000, true, { durable });
    const issue = JSON.parse(
      await terminalKata(["show", ref, "--json"]),
    ).issue;
    if (
      issue.owner !== actor ||
      issue.metadata?.["omp.lead_actor"] !== actor ||
      issue.metadata?.["omp.subagent_id"] !== undefined ||
      issue.metadata?.["omp.terminal_state"] !== undefined
    )
      return;
    await terminalKata(
      [
        "meta",
        "unset",
        ref,
        "omp.lead_actor",
        "--if-match",
        String(issue.revision),
        "--if-value",
        actor,
      ],
      cwd,
    );
  }

  // Clear the durable marker last so a partial hand-back is retried on reconcile.
  async function settleDelegate(
    ref,
    cwd,
    { id, state, lead, leadSessionKey, durable = false },
  ) {
    const terminalKata = (args) =>
      kata(args, cwd, 120_000, true, { durable });
    const issue = JSON.parse(
      await terminalKata(["show", ref, "--json"]),
    ).issue;
    if (issue.metadata?.["omp.subagent_id"] !== id) return;
    if (
      leadSessionKey &&
      issue.metadata?.["omp.lead_session_key"] !== leadSessionKey
    )
      return;
    if (issue.status === "closed") {
      await terminalKata(
        [
          "meta",
          "unset",
          ref,
          "omp.subagent_id",
          "--if-value",
          id,
          "--if-match",
          String(issue.revision),
        ],
        cwd,
      );
      await clearDelegateTerminal(ref, cwd, { id, leadSessionKey, durable });
      await clearDelegateActor(ref, cwd, {
        actor: issue.metadata?.["omp.lead_actor"],
        durable,
      });
      return;
    }
    const assignmentActor = issue.metadata?.["omp.lead_actor"];
    if (
      typeof assignmentActor !== "string" ||
      !assignmentActor ||
      issue.owner !== assignmentActor
    )
      throw new Error(
        `Kata task ${ref} changed owner; delegate hand-back was left unchanged.`,
      );
    const status = JSON.parse(
      await terminalKata(["status", ref, "--json"]),
    );
    const actor = status.actor;
    if (typeof actor !== "string" || !actor)
      throw new Error("Kata effective actor unavailable");
    if (actor !== assignmentActor)
      throw new Error(
        `Kata task ${ref} is no longer under the delegate's original actor; hand-back was left unchanged.`,
      );
    const workerTeammate = teammateFor(id);
    const finalTeammate = lead || undefined;
    const initialTeammate = issue.metadata?.teammate;
    const assertSettlementIssue = (current, operation, expectedTeammate) => {
      if (current.metadata?.["omp.subagent_id"] !== id) {
        throw new Error(
          `Kata task ${ref} lost delegate ${id} during ${operation}`,
        );
      }
      if (
        leadSessionKey &&
        current.metadata?.["omp.lead_session_key"] !== leadSessionKey
      )
        throw new Error(
          `Kata task ${ref} changed lead session during ${operation}`,
        );
      if (current.owner !== assignmentActor)
        throw new Error(
          `Kata task ${ref} changed owner during delegate ${operation}`,
        );
      if (current.metadata?.teammate !== expectedTeammate)
        throw new Error(
          `Kata task ${ref} delegate assignment changed during ${operation}`,
        );
    };
    const mutationIssue = (
      output,
      previousRevision,
      operation,
      advancesRevision = false,
    ) => {
      const mutation = JSON.parse(output);
      const current = mutation.issue;
      const expectedRevision =
        previousRevision + (advancesRevision && mutation.changed ? 1 : 0);
      if (
        !Number.isSafeInteger(current?.revision) ||
        current.revision !== expectedRevision
      )
        throw new Error(`Kata task ${ref} changed during delegate ${operation}`);
      assertSettlementIssue(current, operation, initialTeammate);
      return current;
    };

    const comment = await terminalKata(
      [
        "comment",
        ref,
        "--idempotency-key",
        `omp-settle:${ref}:${id}`,
        "--body",
        `Subagent ${id} stopped without closing its Kata issue. Review the work and supply evidence before closing.`,
        "--json",
      ],
      cwd,
    );
    let current = mutationIssue(comment, issue.revision, "comment");
    const label = await terminalKata(
      ["label", "add", ref, "needs-review", "--json"],
      cwd,
    );
    current = mutationIssue(label, current.revision, "review label");
    const notification = await terminalKata(
      [
        "notify",
        ref,
        "--to",
        lead ? `${actor}/${lead}` : actor,
        "--message",
        `Subagent ${id} ${state}; review the unfinished task.`,
        "--json",
      ],
      cwd,
    );
    current = mutationIssue(
      notification,
      current.revision,
      "notification",
      true,
    );

    const latest = JSON.parse(
      await terminalKata(["show", ref, "--json"]),
    ).issue;
    if (latest.revision !== current.revision)
      throw new Error(`Kata task ${ref} changed after delegate notification`);
    assertSettlementIssue(latest, "settlement", initialTeammate);

    let revision = latest.revision;
    if (latest.metadata?.teammate === workerTeammate) {
      const assignment = await terminalKata(
        [
          "meta",
          lead ? "set" : "unset",
          ref,
          "teammate",
          ...(lead ? [lead] : []),
          "--if-match",
          String(revision),
          "--if-value",
          workerTeammate,
          "--json",
        ],
        cwd,
      );
      current = JSON.parse(assignment).issue;
      if (
        current.revision !== revision + 1 ||
        current.metadata?.teammate !== finalTeammate
      )
        throw new Error(
          `Kata task ${ref} delegate assignment changed during hand-back`,
        );
      assertSettlementIssue(current, "hand-back", finalTeammate);
      revision = current.revision;
    }

    const marker = await terminalKata(
      [
        "meta",
        "unset",
        ref,
        "omp.subagent_id",
        "--if-value",
        id,
        "--if-match",
        String(revision),
        "--json",
      ],
      cwd,
    );
    const cleared = JSON.parse(marker);
    if (
      cleared.changed &&
      cleared.issue.metadata?.["omp.subagent_id"] !== undefined
    )
      throw new Error(`Kata task ${ref} did not clear delegate ${id}`);
    await clearDelegateTerminal(ref, cwd, { id, leadSessionKey, durable });
    await clearDelegateActor(ref, cwd, { actor: assignmentActor, durable });
  }

  async function remove(task, change, cwd) {
    if (plan.local) {
      plan.tasks = plan.tasks.filter((t) => t !== task);
      return;
    }
    if (OPEN.has(task.status)) {
      await transition(task, "abandoned", { ...change, remove: true }, cwd);
    }
    await work(
      task.ref,
      "todo.removed",
      true,
      cwd,
      await owned(task.ref, cwd, false, true),
      true,
    );
    retries.delete(task.ref);
  }

  // With no active task, start the earliest pending one (phase order).
  async function advance(cwd) {
    if (plan.tasks.some((t) => t.status === "in_progress" && leads(t))) return;
    const next = phasesOf(plan.tasks)
      .flatMap((p) => p.tasks)
      .find((t) => t.status === "pending" && leads(t));
    if (next) await transition(next, "in_progress", {}, cwd);
  }

  function resolve(target, phase, fallback) {
    if (phase && !target) {
      const tasks = plan.tasks.filter((t) => t.phase === phase);
      if (tasks.length === 0)
        throw new Error(
          `No phase "${phase}". Phases: ${
            phasesOf(plan.tasks)
              .map((p) => p.name)
              .join(", ") || "none"
          }`,
        );
      return tasks;
    }
    if (!target) {
      // Never guess between two active tasks: closing one closes it with this evidence.
      const active = plan.tasks.filter(
        (t) => t.status === "in_progress" && leads(t),
      );
      if (fallback === "active" && active.length === 1) return active;
      if (fallback === "active" && active.length > 1)
        throw new Error(
          `Several tasks are in progress (${active.map((t) => `"${t.content}"`).join(", ")}); name the task.`,
        );
      throw new Error("Name the task (verbatim content or Kata ref).");
    }
    const exact = plan.tasks.filter(
      (t) =>
        t.content === target ||
        t.ref === target ||
        t.ref.split("#")[1] === target,
    );
    if (exact.length === 1) return exact;
    const needle = target.toLowerCase();
    const fuzzy = plan.tasks.filter((t) =>
      t.content.toLowerCase().includes(needle),
    );
    if (fuzzy.length === 1) return fuzzy;
    throw new Error(
      `${fuzzy.length ? "Ambiguous" : "No"} task "${target}". Tasks: ${plan.tasks.map((t) => `"${t.content}"`).join(", ") || "none"}`,
    );
  }

  async function prepareAutoParent(cwd) {
    if (!plan.parent || !plan.auto) return;
    const claimed = await owned(plan.parent, cwd, true);
    await work(plan.parent, "work.attention", "ok", cwd, claimed);
  }

  async function ensureParent(params, ctx) {
    const initRef = params.op === "init" ? params.ref : undefined;
    const explicit = main(ctx)
      ? options.explicitRef?.() ?? initRef ?? assignedRef ?? process.env.KATA_REF
      : assignedRef ?? options.explicitRef?.() ?? initRef ?? process.env.KATA_REF;
    const target = explicit || params.ref;
    if (
      target &&
      ((params.op === "init" && params.ref) ||
        !plan.parent ||
        (target.includes("#")
          ? plan.parent !== target
          : plan.parent.split("#").pop() !== target))
    ) {
      requestedRef = target;
      try {
        await adopt(target, ctx);
      } catch (error) {
        bound = false;
        throw error;
      }
    }
    if (!plan.parent && !(await probe(ctx.cwd))) {
      plan.local = true;
      delete plan.error;
      return;
    }
    // A Pi child may establish the session parent before the lead uses todo.
    if (!plan.parent && !explicit && !params.ref) {
      const recovered = await sessionBinding(ctx);
      if (recovered) {
        setParent(recovered);
        await load(recovered.workspace);
      }
    }
    if (plan.parent) {
      await reopenParent();
      await prepareAutoParent(plan.workspace || ctx.cwd);
      return plan.parent;
    }
    const sessionId = ctx.sessionManager?.getSessionId?.();
    if (typeof sessionId !== "string" || !sessionId.trim())
      throw new Error(
        "Kata needs a real session id before creating an automatic parent.",
      );
    const title =
      params.message?.trim() ||
      host.getSessionName?.() ||
      `Todo plan: ${(params.list ?? []).map((p) => p.phase).join(", ") || "tasks"}`.slice(
        0,
        120,
      );
    const cwd = path.resolve(ctx.cwd);
    const out = JSON.parse(
      await kata(
        [
          "create",
          "--body",
          `Todo plan from ${namespace} session ${sessionId}.`,
          "--meta",
          `${namespace}.session_id=${sessionId}`,
          "--meta",
          `${namespace}.workspace=${cwd}`,
          "--label",
          `${namespace}-session`,
          "--idempotency-key",
          `${namespace}-session:${sessionId}`,
          "--force-new",
          "--json",
          "--",
          title,
        ],
        cwd,
      ),
    );
    const createEvent = out.event ?? out.original_event;
    if (!out.issue?.short_id || !createEvent?.project_name)
      throw new Error("kata create returned no parent identity");
    const ref = `${createEvent.project_name}#${out.issue.short_id}`;
    // Create only accepts string metadata. Type the authority marker before
    // attention setup so a failed claim can still be recovered by session.
    await kata(
      ["meta", "set", ref, `${namespace}.auto_parent`, "true", "--json-value"],
      cwd,
    );
    setParent({ parent: ref, workspace: cwd, sessionId, auto: true });
    await prepareAutoParent(cwd);
    return ref;
  }

  async function reopenParent() {
    if (!plan.parent || !plan.auto) return;
    const shown = JSON.parse(
      await kata(["show", plan.parent, "--json"], plan.workspace),
    );
    if (shown.issue.status !== "closed") return;
    const closed = await owned(plan.parent, plan.workspace, false, true);
    if (
      closed.status !== "closed" ||
      closed.revision !== shown.issue.revision
    )
      throw new Error(
        `Automatic parent ${plan.parent} changed before restart; no reopen was attempted.`,
      );
    await kata(["reopen", plan.parent], plan.workspace);
    const reopened = await owned(plan.parent, plan.workspace);
    if (
      reopened.status !== "open" ||
      reopened.revision !== closed.revision + 1
    )
      throw new Error(
        `Automatic parent ${plan.parent} changed during restart; no further updates were written.`,
      );
  }

  class DeferredClose extends Error {}
  function scheduleRetry() {
    if (retryTimer) return;
    retryTimer = setTimeout(() => {
      retryTimer = undefined;
      void serial(drainRetries).catch(debug);
    }, 60_100);
    retryTimer.unref?.();
  }
  async function closeTask(args, task, to, change, cwd, decision) {
    try {
      await kata(args, cwd);
    } catch (error) {
      if (!/throttl|sibling.*(burst|rate)|too many.*clos/i.test(String(error)))
        throw error;
      retries.set(task.ref, {
        task: { ...task },
        to,
        change: { ...change },
        cwd,
        revision: decision.revision,
        at: Date.now() + 60_000,
      });
      persist();
      scheduleRetry();
      throw new DeferredClose(
        `Kata deferred closing "${task.content}" under its sibling throttle. It remains open and will retry after 60 seconds.`,
      );
    }
  }
  async function drainRetries() {
    for (const [ref, pending] of retries) {
      if (pending.at > Date.now()) continue;
      try {
        const current = JSON.parse(
          await kata(["show", ref, "--json"], pending.cwd),
        ).issue;
        if (current.status === "closed") {
          if (pending.change.remove)
            await work(
              ref,
              "todo.removed",
              true,
              pending.cwd,
              await owned(ref, pending.cwd, false, true),
              true,
            );
          retries.delete(ref);
          continue;
        }
        if (current.revision !== pending.revision) {
          retries.delete(ref);
          currentCtx?.ui?.notify?.(
            `Kata deferred close for "${pending.task.content}" was canceled because the issue changed; inspect the current task before closing it.`,
            "warning",
          );
          continue;
        }
        await owned(ref, pending.cwd);
        await transition(pending.task, pending.to, pending.change, pending.cwd);
        if (pending.change.remove)
          await work(
            ref,
            "todo.removed",
            true,
            pending.cwd,
            await owned(ref, pending.cwd, false, true),
            true,
          );
        retries.delete(ref);
        if (plan.tasks.some((task) => task.ref === ref)) {
          await load(plan.workspace);
          await advance(plan.workspace);
        }
      } catch (error) {
        debug(error);
      }
    }
    if (retries.size) scheduleRetry();
    persist();
    await reconcile();
  }

  class ParentEvidenceError extends Error {}
  async function evidenceJSON(args, workspace) {
    const text = await kata(args, workspace);
    try {
      return JSON.parse(text);
    } catch {
      throw new ParentEvidenceError("Malformed Kata evidence response");
    }
  }
  function proofArgs(items) {
    const values = new Set();
    const doneEvidence = new Set([
      "commit",
      "pr",
      "test",
      "reviewed-paths",
      "external",
    ]);
    for (const item of items) {
      if (!item || typeof item !== "object")
        throw new ParentEvidenceError("Malformed Kata evidence");
      if (!doneEvidence.has(item.type))
        throw new ParentEvidenceError(
          `Kata child evidence type ${item.type} cannot support automatic parent --done close`,
        );
      const key = {
        commit: "sha",
        pr: "url",
        test: "command",
        external: "account",
        "no-change-audit": "rationale",
        "duplicate-of": "issue_ref",
        "superseded-by": "issue_ref",
      };
      const value = item[key[item.type]];
      if (typeof value === "string" && value.trim())
        values.add(`${item.type}:${value}`);
      else if (item.type === "reviewed-paths" && Array.isArray(item.paths))
        for (const file of item.paths) {
          if (typeof file !== "string" || !file.trim())
            throw new ParentEvidenceError("Malformed Kata evidence");
          values.add(`reviewed-paths:${file}`);
        }
      else throw new ParentEvidenceError("Malformed or unknown Kata evidence");
    }
    return [...values].flatMap((value) => ["--evidence", value]);
  }

  const delegatedRunning = () => host.delegatedRunning?.() ?? false;
  async function reconcile() {
    await host.beforeReconcile?.();
    for (const binding of autoParents.values()) {
      const { parent, workspace } = binding;
      try {
        if (delegatedRunning()) return;
        const initial = JSON.parse(
          await kata(["show", parent, "--json"], workspace),
        );
        if (
          initial.issue.status === "closed" ||
          initial.issue.metadata?.[`${namespace}.auto_parent`] !== true ||
          initial.issue.metadata?.[`${namespace}.session_id`] !==
            binding.sessionId
        )
          continue;
        const initialOwner = await owned(parent, workspace);
        if (initialOwner.revision !== initial.issue.revision) continue;
        if (
          !initial.children?.length ||
          initial.children.some((c) => c.status !== "closed")
        )
          continue;
        const evidence = [];
        const audits = new Map();
        let projects;
        for (const child of initial.children) {
          if (child.closed_reason === "wontfix") continue;
          let project = child.qualified_id?.split("#")[0];
          if (!project && child.project_id === initial.issue.project_id)
            project = parent.split("#")[0];
          if (!project) {
            projects ??= (
              await evidenceJSON(["projects", "list", "--json"], workspace)
            )?.projects;
            if (!Array.isArray(projects))
              throw new ParentEvidenceError("Kata child project unavailable");
            project = projects.find((p) => p?.id === child.project_id)?.name;
          }
          if (!project)
            throw new ParentEvidenceError("Kata child project unavailable");
          if (!audits.has(project))
            audits.set(
              project,
              (
                await evidenceJSON(
                  ["audit", "closes", "--project", project, "--json"],
                  workspace,
                )
              )?.rows,
            );
          const rows = audits.get(project);
          if (
            !Array.isArray(rows) ||
            rows.some((row) => !row || typeof row !== "object")
          )
            throw new ParentEvidenceError("Malformed Kata close audit");
          const row = rows
            .filter((r) => r.issue === child.short_id)
            .sort((a, b) => b.event_id - a.event_id)[0];
          if (!row?.event_id)
            throw new ParentEvidenceError("Kata child close event unavailable");
          const page = await evidenceJSON(
            [
              "events",
              "--project",
              project,
              "--after",
              String(row.event_id - 1),
              "--limit",
              "1",
              "--json",
            ],
            workspace,
          );
          const event = Array.isArray(page?.events)
            ? page.events[0]
            : undefined;
          if (
            page?.reset_required ||
            event?.event_id !== row.event_id ||
            event.type !== "issue.closed" ||
            event.issue_uid !== child.uid ||
            Date.parse(event.payload?.closed_at) !==
              Date.parse(child.closed_at) ||
            !Array.isArray(event.payload?.evidence)
          )
            throw new ParentEvidenceError(
              "Kata current child evidence unavailable",
            );
          evidence.push(...event.payload.evidence);
        }
        const proof = proofArgs(evidence);
        if (!proof.length)
          throw new ParentEvidenceError(
            "Kata child close evidence union is empty",
          );
        const final = JSON.parse(
          await kata(["show", parent, "--json"], workspace),
        );
        if (final.issue.status === "closed" || delegatedRunning()) continue;
        const finalOwner = await owned(parent, workspace);
        if (
          finalOwner.revision !== final.issue.revision ||
          final.issue.revision !== initial.issue.revision ||
          final.children?.length !== initial.children.length ||
          final.children.some(
            (c) =>
              c.status !== "closed" ||
              !initial.children.some(
                (old) => old.uid === c.uid && old.revision === c.revision,
              ),
          )
        )
          continue;
        await kata(
          [
            "close",
            parent,
            "--done",
            "--if-match",
            String(final.issue.revision),
            "--idempotency-key",
            `${namespace}-parent-close:${parent}:${final.issue.revision}`,
            "--message",
            `${namespace.toUpperCase()} session ${binding.sessionId}: all ${final.children.length} child issues are closed in Kata and delegated work has settled. Evidence is copied from the children's close events; each child's own close remains authoritative.`,
            ...proof,
          ],
          workspace,
        );
      } catch (error) {
        debug(error);
        if (/throttl|sibling.*(burst|rate)|too many.*clos/i.test(String(error)))
          scheduleRetry();
        if (
          !(error instanceof ParentEvidenceError) &&
          /evidence type "[^"]+" not allowed for reason=done|evidence required for reason=done/i.test(
            String(error),
          )
        )
          error = new ParentEvidenceError(String(error));
        if (!(error instanceof ParentEvidenceError)) continue;
        try {
          const live = JSON.parse(
            await kata(["show", parent, "--json"], workspace),
          );
          if (live.issue.status === "open")
            await kata(["label", "add", parent, "needs-review"], workspace);
        } catch (error) {
          debug(error);
        }
      }
    }
  }

  // ---------------------------------------------------------------------- ops

  async function apply(params, ctx) {
    if (!bound || currentId !== ctx.sessionManager?.getSessionId?.())
      await bind(
        ctx,
        options.explicitRef?.() ??
          (params.op === "init" ? params.ref : undefined) ??
          process.env.KATA_REF ??
          (!bound ? requestedRef : undefined),
        params.op === "init" &&
          !!params.ref &&
          !options.explicitRef?.(),
      );
    currentCtx = ctx;
    currentId ||= ctx.sessionManager?.getSessionId?.();
    let cwd = plan.workspace || ctx.cwd;
    const change = {
      message: params.message?.trim() || undefined,
      evidence: params.evidence,
      reason: params.reason?.trim() || undefined,
    };
    const op = params.op;
    if (op === "view") {
      if (plan.parent) {
        await load(cwd);
        await reconcile();
      }
      return render(plan);
    }
    if (op === "init") {
      const desired = params.list?.length
        ? params.list.flatMap((p) =>
            p.items.map((content) => ({ phase: p.phase, content })),
          )
        : (params.items ?? []).map((content) => ({
            phase: params.phase || "Tasks",
            content,
          }));
      if (desired.length === 0)
        throw new Error("init needs list (phases with items) or items.");
      if (
        desired.some(
          (task) => typeof task.content !== "string" || !task.content.trim(),
        )
      )
        throw new Error("init task titles must not be empty or whitespace-only.");
      await ensureParent(params, ctx);
      cwd = plan.workspace || ctx.cwd;
      await load(cwd);
      const wanted = new Set(
        desired.map((d) => `${d.phase}\u0000${d.content}`),
      );
      let canceledClose = false;
      for (const task of plan.tasks) {
        if (
          wanted.has(`${task.phase}\u0000${task.content}`) &&
          retries.get(task.ref)?.change?.remove
        )
          canceledClose = retries.delete(task.ref) || canceledClose;
      }
      if (canceledClose) persist();
      for (const task of plan.tasks) {
        if (!wanted.has(`${task.phase}\u0000${task.content}`))
          await remove(task, { message: "replaced by a new todo plan" }, cwd);
      }
      const keyFor = (task) => `${task.phase}\u0000${task.content}`;
      const orderByKey = new Map();
      desired.forEach((task, order) => {
        const key = keyFor(task);
        if (!orderByKey.has(key)) orderByKey.set(key, order);
      });
      const have = new Map(plan.tasks.map((task) => [keyFor(task), task]));
      for (const [key, order] of orderByKey) {
        const task = have.get(key);
        if (!task) continue;
        if (plan.local) task.order = order;
        else await setTaskOrder(task, order, cwd);
      }
      for (const [key, order] of orderByKey) {
        if (have.has(key)) continue;
        const task = desired.find((item) => keyFor(item) === key);
        if (plan.local) {
          const local = {
            ...task,
            ref: `local-${digest(task.phase, task.content)}`,
            status: "pending",
            order,
          };
          plan.tasks.push(local);
          have.set(key, local);
        } else {
          await create(task.phase, task.content, cwd, order);
          have.set(key, true);
        }
      }
      if (plan.local)
        plan.tasks.sort((a, b) => (a.order ?? Infinity) - (b.order ?? Infinity));
      await load(cwd);
      await advance(cwd);
      return render(plan);
    }
    if (!plan.parent && !plan.local)
      throw new Error("No todo plan yet. Call todo op=init first.");
    await load(cwd);
    switch (op) {
      case "append": {
        const items = params.items?.length
          ? params.items
          : params.task
            ? [params.task]
            : [];
        if (items.length === 0) throw new Error("append needs items.");
        const phase =
          params.phase || plan.tasks[plan.tasks.length - 1]?.phase || "Tasks";
        await reopenParent();
        let order = plan.tasks.every((task) => task.order !== undefined)
          ? Math.max(-1, ...plan.tasks.map((task) => task.order)) + 1
          : undefined;
        if (plan.local && order === undefined) {
          plan.tasks.forEach((task, index) => {
            task.order = index;
          });
          order = plan.tasks.length;
        }
        for (const content of items) {
          const taskOrder = order === undefined ? undefined : order++;
          if (plan.local)
            plan.tasks.push({
              content,
              phase,
              ref: `local-${digest(phase, content)}`,
              status: "pending",
              order: taskOrder,
            });
          else await create(phase, content, cwd, taskOrder);
        }
        await load(cwd);
        break;
      }
      case "start": {
        const [task] = resolve(params.task, undefined, "none");
        await preflightStart(task, cwd);
        if (!OPEN.has(task.status)) await reopenParent();
        for (const other of plan.tasks)
          if (other !== task && other.status === "in_progress" && leads(other))
            await transition(other, "pending", {}, cwd);
        await transition(task, "in_progress", change, cwd);
        break;
      }
      case "done":
      case "drop": {
        const tasks = resolve(params.task, params.phase, "active").filter((t) =>
          OPEN.has(t.status),
        );
        change.many = tasks.length > 1;
        for (const task of tasks)
          await transition(
            task,
            op === "done" ? "completed" : "abandoned",
            change,
            cwd,
          );
        break;
      }
      case "rm":
        for (const task of resolve(params.task, params.phase, "none"))
          await remove(task, change, cwd);
        await load(cwd);
        break;
      case "block":
        for (const task of resolve(params.task, undefined, "active"))
          await transition(task, "blocked", change, cwd);
        break;
      case "unblock":
        for (const task of resolve(params.task, undefined, "none"))
          if (task.status === "blocked")
            await transition(task, "pending", change, cwd);
        break;
      default:
        throw new Error(`Unknown op "${op}".`);
    }
    await advance(cwd);
    await reconcile();
    return render(plan);
  }

  function details(op) {
    return {
      op,
      kata: { parent: plan.parent },
      phases: phasesOf(plan.error ? [] : plan.tasks).map((p) => ({
        name: p.name,
        tasks: p.tasks.map((t) =>
          t.blocker
            ? { content: t.content, status: t.status, blocker: t.blocker }
            : { content: t.content, status: t.status },
        ),
      })),
    };
  }

  async function execute(params, ctx, signal) {
    return serial(async () => {
      const previousSignal = operationSignal;
      operationSignal = signal;
      if (boundAgent && boundAgent !== agentKey(ctx)) {
        operationSignal = previousSignal;
        return {
          content: [
            {
              type: "text",
              text: "Kata agent identity changed; use this worker's own extension context.",
            },
          ],
          details: { op: params.op, kata: {}, phases: [] },
          isError: true,
        };
      }
      try {
        checkCancellation();
        if (!main(ctx) && !host.binding)
          throw new Error("The todo plan belongs to the main agent.");
        const text = await apply(params, ctx);
        checkCancellation();
        return {
          content: [{ type: "text", text }],
          details: details(params.op),
        };
      } catch (err) {
        const canceled = signal?.aborted === true;
        // A failed initial binding has no authoritative plan to render yet.
        if (!bound || (!plan.parent && !plan.local) || canceled)
          plan.error = canceled
            ? "Todo operation was canceled; use view to refresh."
            : err.message;
        // Kata may have applied part of a batch. A failed reload marks the
        // cached tasks unavailable instead of presenting them as current.
        if (plan.parent && !canceled)
          await load(plan.workspace || ctx.cwd).catch(debug);
        return {
          content: [
            {
              type: "text",
              text: `${err instanceof Error ? err.message : String(err)}\n\n${render(plan)}`,
            },
          ],
          details: details(params.op),
          isError: err instanceof DeferredClose ? undefined : true,
        };
      } finally {
        persist();
        refresh(ctx);
        operationSignal = previousSignal;
      }
    });
  }

  async function command(args, ctx) {
    const match = /^(\S+)?(?:\s+([\s\S]*))?$/.exec(args.trim());
    const sub = match?.[1] ?? "";
    const arg = match?.[2]?.trim() || undefined;
    const actor = process.env.USER || "user";
    const supported = {
      "": { op: "view" },
      start: { op: "start", task: arg },
      done: {
        op: "done",
        task: arg,
        message: `Marked done by ${actor} through /todo in ${namespace}; the user attests the work is complete.`,
        evidence: [`external:${actor}`],
      },
      drop: {
        op: "drop",
        task: arg,
        message: `dropped by ${actor} through /todo`,
      },
      rm: { op: "rm", task: arg, message: `removed by ${actor} through /todo` },
      append: { op: "append", items: arg ? [arg] : [] },
      block: { op: "block", reason: arg },
      unblock: { op: "unblock", task: arg },
    };
    const params = supported[sub];
    if (!params)
      return `/todo ${sub} is not available with Kata. Use /todo [start|done|drop|rm|append|block|unblock], or kata tui.`;
    const text = await serial(async () => {
      try {
        return await apply(params, ctx);
      } catch (err) {
        return err.message;
      } finally {
        persist();
        refresh(ctx);
      }
    });
    return text;
  }

  async function observeToolResult(event, ctx) {
    progressed = true;
    if (
      !main(ctx) ||
      (options.explicitRef?.() ?? process.env.KATA_REF) ||
      event.toolName !== "bash" ||
      event.isError ||
      (event.details?.exitCode !== undefined && event.details.exitCode !== 0)
    )
      return;
    const command = event.input?.command;
    if (typeof command !== "string" || /[;|\n]/.test(command)) return;
    for (const invocation of kataCommands(command, options.executable)) {
      const parsed = shellOptions(invocation.words),
        args = parsed.args;
      let ref =
        args[0] === "claim"
          ? args[1]
          : args[0] === "meta" &&
              args[1] === "set" &&
              args[3] === "work.attention" &&
              args[4] === "ok"
            ? args[2]
            : undefined;
      if (
        !ref ||
        parsed.options.has("--as") ||
        parsed.options.has("--daemon") ||
        invocation.routedByEnvironment
      )
        continue;
      if (!ref.includes("#") && parsed.value("--project"))
        ref = `${parsed.value("--project")}#${ref}`;
      const workspace = path.resolve(
        event.input.cwd || ctx.cwd,
        parsed.value("--workspace") || ".",
      );
      const target = ref;
      await bounded(
        serial(async () => {
          if (
            plan.tasks.some(
              (t) => t.ref === target || t.ref.split("#")[1] === target,
            )
          )
            return;
          await owned(target, workspace);
          await adopt(target, ctx, workspace);
        }),
      );
    }
  }

  async function remind(messages, ctx) {
    if (!main(ctx)) return;
    const refreshed = await bounded(
      serial(async () => {
        await load(plan.workspace || ctx.cwd);
        await drainRetries();
        return true;
      }),
    );
    if (!refreshed || plan.error) return;
    const open = plan.tasks.filter(
      (t) => t.status === "pending" || t.status === "in_progress",
    );
    if (open.length === 0 || reminders >= MAX_REMINDERS || !progressed) return;
    // Do not push past a question the assistant just asked the user.
    const last = [...messages].reverse().find((m) => m?.role === "assistant");
    const text = Array.isArray(last?.content)
      ? last.content
          .filter((c) => c?.type === "text")
          .map((c) => c.text)
          .join("")
      : String(last?.content ?? "");
    if (text.trim().endsWith("?")) return;
    reminders++;
    progressed = false;
    return [
      "<system-reminder>",
      `Open Kata todo tasks under ${plan.parent} (reminder ${reminders}/${MAX_REMINDERS}):`,
      ...open.map((t) => `- ${t.content} (${t.ref}, ${t.status})`),
      "Continue the work, or update the plan: done with evidence, block with a reason when waiting on someone, or drop with a reason.",
      "If the user asked you to stop or pause, do not continue: block the remaining tasks with that reason and stop.",
      "</system-reminder>",
    ].join("\n");
  }

  return {
    get plan() {
      return plan;
    },
    get currentId() {
      return currentId;
    },
    get currentCtx() {
      return currentCtx;
    },
    set currentCtx(ctx) {
      currentCtx = ctx;
    },
    stateEntry: STATE_ENTRY,
    resetReminders() {
      reminders = 0;
    },
    debug,
    bounded,
    serial,
    kata,
    effectiveActor,
    checkDaemonFloor,
    owned,
    work,
    delegate,
    recordDelegateTerminal,
    settleDelegate,
    load,
    refresh,
    probe,
    bind,
    ensureParent,
    adopt,
    reconcile,
    drainRetries,
    apply,
    execute,
    command,
    observeToolResult,
    remind,
  };
}

function classifyTask(task, { namespace, leadTeammate = "", harness }) {
  const meta = task.metadata || {};
  if (Object.hasOwn(meta, namespace + ".subagent_id")) return "delegate";
  const teammate = meta.teammate || "";
  if (teammate === leadTeammate || (!teammate && !leadTeammate)) return "lead";
  if (
    !leadTeammate &&
    (teammate === harness || teammate.startsWith(harness + "."))
  )
    return "lead";
  return "other";
}
function kataEnv(markers) {
  return { ...process.env, ...markers };
}
