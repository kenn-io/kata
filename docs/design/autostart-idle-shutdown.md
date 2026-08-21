# Auto-start daemon idle shutdown

Kata clients automatically start a local daemon when no compatible local
daemon is reachable. Auto-start is convenient for short CLI and agent
sessions, but a process that remains resident indefinitely is surprising on a
developer workstation. `autostart_idle_timeout` lets that narrowly scoped
daemon exit after foreground use stops without weakening the explicit-daemon
contract used by services, remote clients, federation hubs, or hosted
deployments.

This note records the lifecycle invariants behind the feature. User-facing
configuration is documented in [Configuration](../reference/configuration.md).

## Eligibility is process-local

The configured timeout is effective only when all of these conditions hold:

- the process carries Kata's private auto-start marker;
- every daemon and browser listener is a Unix socket or loopback address;
- `web.public_origin` does not describe a non-loopback origin;
- trusted-proxy and shared-listener host aliases do not expose the process.

An explicit `kata daemon start`, service-managed process, configured remote,
or locally bound listener behind a public proxy remains long-running. Listener
scope is checked after configuration and command-line overrides are resolved,
because a loopback socket alone does not prove that the effective service is
private.

Because the marker is read once at process start, residency cannot change
under a running process. `kata daemon start` therefore inspects the live
daemon's health: when it advertises `idle_shutdown`, the command stops that
process and starts an explicit replacement rather than reporting it as already
running. `kata daemon restart` always produces an explicit daemon. An idle exit
writes one line to the daemon log so the disappearance is diagnosable.

The auto-start marker is removed from hook subprocess environments. This keeps
an explicitly launched daemon command from inheriting the parent daemon's
auto-start provenance. An ordinary Kata client in a hook still uses normal
discovery and may auto-start a newly marked daemon when none is reachable.

## Activity classes

The idle controller distinguishes work by what it means for residency:

| Activity | Moves the deadline | Blocks shutdown while active |
| --- | --- | --- |
| CLI, TUI, browser request, or SSE stream | Yes, after completion | Yes |
| Marked MCP keepalive | Yes | Yes |
| Queued or running hook | No | Yes |
| Embedding reconciliation | No | Yes |
| GitHub or federation synchronization | No | Yes |
| Federation configuration reconciliation | No | Yes |
| Timed-claim sweep | No | Yes |
| Scheduler wait, backoff, telemetry, or watcher | No | No |
| Health, ping, or instance probe | No | No; HTTP shutdown joins the handler |

Foreground activity starts a complete new idle interval when the final
foreground lease releases. Drain activity preserves already-admitted finite
work but never moves the foreground deadline. This prevents periodic
maintenance from keeping an otherwise unused process alive forever.

Classification follows the request that reaches the daemon. If a hook invokes
`kata` or sends an ordinary API request, that new request is foreground work;
arbitrary subprocesses do not carry reliable causal metadata.

## Controller and admission

The controller exposes admission rather than unconditional acquisition. Once
terminal shutdown begins, HTTP receives `503` and background workers skip new
resource-using work. The observable states are:

- `armed`: an idle deadline is running;
- `foreground`: client work suspends the deadline;
- `blocked`: the deadline elapsed while finite drain work remained;
- `stopping`: admission is closed and shutdown is committed.

When expiration is blocked, unrelated top-level drains are denied. An admitted
drain may fork one generation of child leases before releasing its own lease.
That finite handoff lets a database or synchronization operation protect hook
jobs it caused without allowing recursive background work to derive an
unbounded lifetime.

Blocked expiration is reversible while drain work remains: a foreground
request clears the expired state and reopens admission. Long-lived schedulers
receive a retry channel atomically with a reversible denial. Foreground revival
closes that channel; terminal stop also closes outstanding waiters. This avoids
both missed wakeups and polling while ensuring schedulers do not become stuck
when a foreground request revives the daemon.

All admission, expiration, revival, fork, and release transitions serialize
under the controller mutex. Releases are idempotent, timer callbacks carry a
generation, and cancellation callbacks run outside the mutex so they may
safely inspect controller state.

## MCP process keepalive

`kata mcp serve` reads the effective timeout from daemon health rather than
local configuration. When idle shutdown is effective, it sends a marked ping
immediately and then waits half the timeout after each completed attempt.
Ordinary probes remain observational; only the marked MCP ping counts as
foreground activity. The marker is recognized only on `GET /api/v1/ping`,
after all applicable listener, session, and authentication policies accept the
request. Rejected requests do not reach idle admission.

The keepalive belongs to the MCP server process, in both stdio and
streamable-HTTP modes. A quiet stdio connection therefore remains usable, and
an HTTP bridge remains ready for future clients for as long as its process is
running. Session-aware HTTP residency would require a different connection
lifecycle and is not inferred from individual tool calls.

## Root shutdown and dependency safety

The first shutdown source establishes one absolute deadline, preserves hook
handoffs from admitted producers, closes top-level idle admission, and cancels
the daemon root context. Idle expiration, listener failure, explicit stop,
parent cancellation, and platform stop events converge on that coordinator.

HTTP handlers and daemon workers finish as hook producers before the dispatcher
stops accepting and drains hook jobs with the time remaining in the same
bounded budget. The dispatcher cancels in-flight hooks two grace windows before
that deadline so a hook slower than the budget is terminated, killed, and
reaped while a clean join is still possible; only work that survives that is
reported as unjoined. Platform event pumps join concurrently. The runtime discovery
record is removed after listeners stop serving and the HTTP drain completes or
times out, before waiting for any remaining background work. If any producer
remains unjoined at the deadline, the dedicated daemon exits without sealing
the hook queue or closing dependencies underneath that work; process exit
reclaims them.

`kata daemon restart` waits longer than the internal drain budget so final
cleanup and observable process exit do not race the operator-facing timeout.

## Operational consequence

Scheduled work is opportunistic on an idle-enabled auto-start daemon. Durable
cursors, pending rows, and reconciliation state resume the next time a client
starts the process, but a future timer does not keep it resident. Operators who
need continuous GitHub synchronization, federation, timed claims, embeddings,
or hooks should run an explicit daemon under a service manager.
