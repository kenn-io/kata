# Priority-Aware `kata next` Design

## Context

`kata ready` lists every open issue that has no active blocking predecessor.
That is useful for inspecting a queue, but humans and agents often want one
concrete issue to work on next. Requiring each caller to request the full ready
queue, interpret priorities, and extract one row duplicates scheduling logic
and produces unnecessarily large output.

This change adds a read-only `kata next` command that selects one ready issue.
It is tracked by issue `agfh`.

## Goals

- Return one ready issue in human, agent, or JSON output.
- Prefer explicitly prioritized ready work over unprioritized ready work.
- Preserve the existing ready order as the tie-breaker.
- Support the same scope and filter choices as `kata ready`.
- Optionally render the selected issue with the detail available from
  `kata show`.
- Leave the behavior and output of `kata ready` unchanged.

## Non-goals

- Changing the ready HTTP API or database query ordering.
- Changing the meaning or output ordering of `kata ready`.
- Claiming or otherwise mutating the selected issue.
- Adding configurable scheduling strategies or randomized selection.

## Command Interface

The command accepts no positional arguments:

```text
kata next [--unowned] [--owner NAME]
          [--label LABEL] [--no-label LABEL]
          [--all] [--full]
```

The ownership and label filters have the same meaning as their `kata ready`
counterparts. `--all` searches every non-archived project. As with `ready`,
`--all` cannot be combined with `--project`, `--unowned`, `--owner`, `--label`,
or `--no-label`, and `--unowned` and `--owner` are mutually exclusive.

There is no `--limit` flag because the command's result cardinality is always
zero or one. Global output flags such as `--agent`, `--json`, `--format`, and
`--quiet` retain their existing meanings. Because `next` has no redundant
summary or footer, `--quiet` does not suppress its selected row or its empty
result; it has no additional effect on this command.

## Candidate Retrieval and Selection

`next` uses the existing project-scoped or global ready endpoint without a
limit. Candidate membership therefore exactly matches `ready`: candidates are
open, not deleted, and have no open blocking predecessor in an active project.
Project-scoped filters are sent to the endpoint unchanged.

Selection is deterministic:

1. If at least one candidate has a priority, discard unprioritized candidates
   from consideration.
2. Select the candidate with the lowest numeric priority. For example, P0 is
   selected before P1.
3. When candidates share the selected priority, retain the endpoint's existing
   order and select the first one. That order is most recently updated first,
   then newest database ID.
4. If no candidate has a priority, select the first candidate in the existing
   ready order.

Priority affects only `next`; it does not reorder `ready`. Fetching the full
candidate set is an intentional client-side cost that avoids a new public API
and an existing-command behavior change. A future server-side endpoint can
replace the fetch if queue size makes that cost material, without changing the
CLI contract.

## Compact Output

The default human output is one row from the existing ready/list row renderer,
including the open glyph, ID, title, optional priority chip, and owner marker.
It has no list summary or legend footer. Global results use a qualified
`project#short_id` reference. The shared renderer displays `(-)` when the issue
has no owner, matching `ready`.

Agent output is a single key/value record:

```text
OK next issue=abc4 priority=1 owner=agent-a title="Fix callback race"
```

`priority` and `owner` are omitted when absent. A global result uses the
qualified reference in `issue`. The agent-output reference documents this
single-result shape as an intentional exception to the normal read-command
`count=<n>` header plus row sequence.

JSON output is a single-result envelope rather than the ready list envelope:

```json
{"issue":{"short_id":"abc4","title":"Fix callback race","priority":1}}
```

The issue value preserves the complete issue object returned by the ready
endpoint. Global results also preserve `project_name` and the other global row
fields.

An empty queue is a successful result because having no ready work is an
ordinary state:

- human: `No ready issues.`
- agent: `OK next found=false`
- JSON: `{"issue":null}`

## Full Output

With `--full`, selection happens exactly as in compact mode and then the client
requests the selected issue from the existing show endpoint.

- Human output uses the existing `show` detail renderer.
- Agent output uses the existing show fields and sections, with an
  `OK next <short_id>` header so the response still identifies the invoked
  operation.
- JSON output is the existing show response envelope, including issue,
  comments, labels, links, metadata, and lease-related fields.

For a global selection, the selected row supplies the project identity needed
for the detail request and qualified link rendering. If the queue is empty,
`--full` uses the same empty outputs as compact mode and performs no detail
request.

## Code Boundaries

Ready URL construction, validation, retrieval, and typed candidate decoding
move into focused CLI helpers shared by `ready` and `next`. The ready command
continues to own its list rendering and footer behavior. A pure selection helper
accepts the decoded candidates and returns zero or one candidate without doing
I/O.

The show command's fetch and render steps become reusable helpers. `show`
continues to produce its existing output, while `next --full` supplies the
operation name used by the agent header. No daemon, database, schema, or
OpenAPI changes are required.

## Error Handling

Flag validation errors retain the same kinds, messages, and exit codes as
`ready`. Project resolution, daemon connection, ready fetch, JSON decoding, and
detail fetch failures propagate through the normal CLI error path. If the issue
changes or disappears between ready selection and the `--full` detail request,
the show request's error is returned rather than silently falling back to a
compact result.

## Testing

Development follows red-green-refactor. Focused tests cover selection with:

- mixed prioritized and unprioritized candidates;
- different priority values;
- equal-priority ties;
- an entirely unprioritized queue; and
- an empty queue.

CLI integration tests cover compact human, agent, and JSON output; global
qualified references; empty output in every mode; ownership and label filters;
the validation combinations inherited from `ready`; and `--full` output in all
three modes. Existing ready and show tests guard their output contracts during
the helper refactors. Documentation updates add `next` and `--full` to the CLI
reference, document its single-result agent contract, and use the command in
the agent workflow where one issue is desired.
