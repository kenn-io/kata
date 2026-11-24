---
title: Comparisons with SaaS issue trackers
description: Understand where Kata fits alongside SaaS issue trackers and when to use each.
last_edited: 2026-11-20
---

# Comparisons with SaaS issue trackers

Kata is local-first rather than software-as-a-service. Linear, Jira, GitHub
Issues, ClickUp, and similar tools are shared online systems for planning,
assignment, reporting, and cross-team coordination. Kata is an issue ledger for
humans and coding agents working close to a codebase.

Both categories can coexist, though they do different jobs. A SaaS tracker is
usually the right place to manage product commitments across a company, while
Kata keeps operational work state fast, local, scriptable, auditable, and
agent-friendly.

## Quick comparison

| Dimension | Kata | SaaS issue trackers |
| --- | --- | --- |
| Primary job | Durable local issue ledger for humans and agents. | Shared online planning, triage, reporting, and collaboration system. |
| Operating model | Local-first daemon, CLI, TUI, and SQLite database under `KATA_HOME`. | Hosted web application backed by the vendor's service and workspace model. |
| User experience | Instant local UX with terminal-native commands; no browser session required for normal work. | Browser-first workflow optimized for teams, dashboards, notifications, and integrations. |
| Agent ergonomics | Agent-first ergonomics: stable refs, `--agent` and `--json` output, idempotency keys, claim flow, evidence-based closes, and predictable failure modes. | Human-first interface with APIs or integrations layered on top for automation. |
| Workspace fit | Resolves from the current repo, clone, worktree, or non-git directory through a small `.kata.toml` binding. | Usually organized around vendor workspaces, teams, projects, and remote issue URLs. |
| State location | Operational state stays local unless you opt into a remote daemon, backup, import/export, or federation workflow. | Issue state lives in the hosted service by default. |
| Offline and private work | Works locally without a network for ordinary issue operations. | Depends on network access to the hosted service for normal operation. |
| Audit discipline | Closing an issue is an explicit completion claim with reason, message, evidence, actor attribution, comments, and events. | Usually supports history and comments, while close discipline depends on team process and product configuration. |
| Reporting and management | Intentionally small: not a portfolio dashboard, roadmap suite, OKR system, or executive reporting layer. | Stronger fit for roadmaps, cycles, analytics, dashboards, notifications, permissions, and cross-functional reporting. |
| Best fit | Local development, multi-agent work, worktrees, lab/private workflows, verifiable close discipline, and durable context across chat sessions. | Company-wide product planning, stakeholder visibility, customer-facing prioritization, and management reporting. |

## When to choose kata

Choose Kata when the work needs to stay close to the machine doing it:

- coding agents need to discover, claim, update, and close work without
  scraping a web UI
- developers want an instant CLI and TUI loop in the terminal
- work spans local clones, worktrees, experiments, or private directories
- task state should survive chat compaction without becoming a Markdown plan
- close discipline matters, and completed work should carry evidence
- your team wants a small local system instead of another hosted product

Kata is especially useful when the issue tracker is part of your development
runtime. The daemon, database, CLI, TUI, and agent output formats are built for
automation first, with human review over the same state.

## When to choose a SaaS tracker

Choose a SaaS tracker when the problem is organizational coordination:

- many people across teams need the same hosted source of truth
- managers need dashboards, roadmaps, cycles, estimates, or portfolio views
- non-developers need browser-first issue creation and review
- permissions, audit policy, notifications, and integrations matter more than
  local operation
- issues are part of customer support, sales, product planning, or executive
  reporting

For many teams, the practical answer is to use both. Keep company commitments,
roadmaps, and stakeholder reporting in a SaaS tracker, and use Kata as the
local operational ledger while humans and agents do the work.
