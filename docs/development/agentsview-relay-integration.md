# AgentsView relay CLI integration

The CLI draft provides retained consent, frozen-file handling and recovery
source. Live sharing and export still refuse. Connect the independently owned
adapters before describing the feature as usable. The public scope is
[project-scoped trace relay](https://github.com/kenn-io/kata/issues/1007).

## Dependency order

| Contract | Inspected public head | Integration responsibility |
| --- | --- | --- |
| [Root discovery #1009](https://github.com/kenn-io/kata/pull/1009) | `05ca436f9725fcaff8354f91a15b2009f75febb8` | Supply verified live discovery from the enrolled root pin and negotiated route. Reconcile the existing target command with prepare/share registration. |
| [Association/upload #1011](https://github.com/kenn-io/kata/pull/1011) | `c8e3dddeceb8ba6143e8a81bfef0ec935dde07fa` | Refresh generated close DTOs and provide root-signed receipt verification. Keep the live close adapter unavailable until these fields can be sent intact. |
| [Binary relay transport #983](https://github.com/kenn-io/kata/pull/983) | `93c063458f84aba2d876ff0dffdcf2754925292b` | Server-side bounded spool/signing transport; no CLI replacement transport. |
| [Direct close runner #982](https://github.com/kenn-io/kata/pull/982) | `c865c286a7d984b2ccf5a1b51656f8fefe0081f5` | Reconcile explicit sharing precedence before optional direct configuration or session discovery. Keep flags-absent personal behavior. |
| [Export companion #2847](https://github.com/kenn-io/agentsview/pull/2847) | `3a9f510f980891828411a2d5517c23572bd13071` | Replace its unavailable source with an owner-maintained consumer of the session codec. |
| [Session codec/ingest #2811](https://github.com/kenn-io/agentsview/pull/2811) | `d8ae9e8e37449a9d2dec40eee6834e84698e1f64` | Exact archive selection, complete normalized closure, positive quality evidence and immutable receiver acceptance. |

These are inspected revisions, not claims that a prerequisite merged. Re-read
the eventual integrated sources and generated contracts. Do not copy the codec,
receiver, service verification or server transport into the CLI.

## Connect the consumer ports

`agentsViewWorkflowPort.Select` must return live, verified discovery for the
actual resolved issue/project. The selected daemon instance, root UID/key,
service, locator and full typed route remain stable across retained consent.
The root is included in the eight-hub bound; a final leaf makes at most nine
observations. Root epoch omission and explicit zero are equivalent typed values;
nonroot enrollment epochs must remain valid. Discovery owns route-entry sanity,
root-pin verification and expiry checks.

The request carries `agentsview_protocol:"relay-v1"`, separate native transcript
provenance, `agentsview_association` and the transient `agentsview_discovery`.
It preserves the ordinary actor and `retry_protocol:"close-v1"` behavior.
The current typed-close adapter refuses because main's generated body cannot
carry the association. Refresh that generated contract remotely; never silently
round-trip the request through an older type that discards consent fields.

Envelope version and manifest version are independent. The inspected codec
uses push envelope v1 and accepts artifact manifest v2 through v7; its exporter
emits v5 normally, v6 for PR links and v7 for plan usage or capture coverage.
The CLI's bounded identity projection accepts those manifest versions without
re-encoding bytes. Structural object/member matching prevents literal slash
keys from impersonating nested identity. Full closure and quality validation
remain codec responsibilities.

`ResolveEvidence` expands attachment suffixes only after snapshot preflight,
under a parent-derived ten-second control budget. It performs reads only;
optional Docbank linking and follow-up comments remain explicit draft refusals.
`Share` may return success only after verifying the bounded root-signed receipt
against the enrolled pin and the full original association/event. A receiver's
JSON response alone cannot supply that authority.

## Remaining runtime evidence

Local compilation, tests, lint, typechecking and compiler-backed generation
were intentionally unrun. Source formatting and source review are not runtime
proof. CI and review findings are addressed per revision; an older passing job
cannot validate a newer head or an unavailable adapter.

The synthetic process fixture checks actual CLI rejection with isolated homes
and two destination counters. It does not implement or replace the required
root/relay/leaf plus two opted-in SQLite receiver proof. That combined privacy
matrix, mixed Kata PostgreSQL routes, epoch/revoke races, direct-runner parity,
root lag recovery and signed receipt integration remain pending. Keep both
partial drafts open until those integration claims have real evidence.
