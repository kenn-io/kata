# Comparisons with SaaS issue trackers

kata is local-first, not software-as-a-service. Linear, Jira, GitHub Issues,
ClickUp, and similar tools are shared online systems for planning, assignment,
reporting, and cross-team coordination. kata is an issue ledger for humans and
coding agents working close to a codebase.

Those categories can coexist, but they are not interchangeable. A SaaS tracker
is usually the right place to manage product commitments across a company. kata
is the right place to keep operational work state fast, local, scriptable,
auditable, and agent-friendly.

## Quick comparison

| Dimension | kata | SaaS issue trackers |
| --- | --- | --- |
| Primary job | Durable local issue ledger for humans and agents. | Shared online planning, triage, reporting, and collaboration system. |
| Operating model | Local-first daemon, CLI, TUI, and SQLite database under `KATA_HOME`. | Hosted web application backed by the vendor's service and workspace model. |
| User experience | Instant local UX with terminal-native commands; no browser session required for normal work. | Browser-first workflow optimized for teams, dashboards, notifications, and integrations. |
| Agent ergonomics | Agent-first ergonomics: stable refs, `--agent` and `--json` output, idempotency keys, claim flow, evidence-based closes, and predictable failure modes. | Human-first product surface with APIs or integrations layered on top for automation. |
| Workspace fit | Resolves from the current repo, clone, worktree, or non-git directory through a small `.kata.toml` binding. | Usually organized around vendor workspaces, teams, projects, and remote issue URLs. |
| State location | Operational state stays local unless you opt into a remote daemon, backup, import/export, or federation workflow. | Issue state lives in the hosted service by default. |
| Offline and private work | Works locally without a network for ordinary issue operations. | Depends on network access to the hosted service for normal operation. |
| Audit discipline | Closing an issue is an explicit completion claim with reason, message, evidence, actor attribution, comments, and events. | Usually supports history and comments, but close discipline depends on team process and product configuration. |
| Reporting and management | Intentionally small: not a portfolio dashboard, roadmap suite, OKR system, or executive reporting layer. | Stronger fit for roadmaps, cycles, analytics, dashboards, notifications, permissions, and cross-functional reporting. |
| Best fit | Local development, multi-agent work, worktrees, lab/private workflows, verifiable close discipline, and durable context across chat sessions. | Company-wide product planning, stakeholder visibility, customer-facing prioritization, and management reporting. |

## When to choose kata

Choose kata when the work needs to stay close to the machine doing it:

- coding agents need to discover, claim, update, and close work without scraping
  a web UI;
- developers want an instant CLI/TUI loop in the terminal;
- work spans local clones, worktrees, experiments, or private directories;
- task state should survive chat compaction without becoming a markdown plan;
- close discipline matters, and completed work should carry evidence;
- the team wants a small local system instead of another hosted product.

kata is especially useful when the issue tracker is part of the development
runtime. The daemon, database, CLI, TUI, and agent output formats are designed
for automation first, with human review over the same state.

## When to choose a SaaS tracker

Choose a SaaS tracker when the problem is organizational coordination:

- many people across teams need the same hosted source of truth;
- managers need dashboards, roadmaps, cycles, estimates, or portfolio views;
- non-developers need browser-first issue creation and review;
- permissions, audit policy, notifications, and integrations matter more than
  local operation;
- issues are part of customer support, sales, product planning, or executive
  reporting.

For many teams, the practical answer is both. Keep company commitments,
roadmaps, and stakeholder reporting in a SaaS tracker. Use kata for the local
operational ledger that humans and agents use while doing the work.
