# kata ready Filters and claim Command

**Status:** Design approved
**Date:** 2026-05-25
**Topic:** Add filtering to `kata ready`/`kata list` and a new `kata claim` command for agent task claiming

## 1. Problem Statement

When multiple agents query `kata ready`, they all see the same list. Without a way to:
1. Filter to unclaimed issues
2. Atomically claim an issue with conflict detection

...two agents can start working on the same task, wasting effort.

## 2. Design Constraints

From kata's design philosophy:

- **Two-state status model preserved**: `open` and `closed` only. No `in_progress` status.
- **Small, sharp surface**: Workflow state encoded in owner + labels, not status enum.
- **Composable primitives**: Filters are independently useful beyond claiming.

## 3. Solution Overview

1. **Filters for `kata ready` and `kata list`**: `--unowned`, `--owner`, `--label`, `--no-label`
2. **New `kata claim` command**: Atomic assign with conflict detection
3. **Documentation**: Agent workflow patterns, label conventions

## 4. Detailed Design

### 4.1 Filters for `kata ready` and `kata list`

#### New Flags

| Flag | Type | Description |
|------|------|-------------|
| `--unowned` | bool | Only issues where `owner IS NULL` |
| `--owner <name>` | string | Only issues where `owner = <name>` |
| `--label <label>` | string (repeatable) | Only issues with this label (AND logic if repeated) |
| `--no-label <label>` | string (repeatable) | Exclude issues with this label |

#### Behavior

**`kata ready`** (existing: open, not deleted, no open blockers):
```bash
kata ready                          # Current behavior unchanged
kata ready --unowned                # + owner IS NULL
kata ready --owner agent1           # + owner = 'agent1'
kata ready --label backlog          # + has label 'backlog'
kata ready --no-label wip           # + does NOT have label 'wip'
kata ready --unowned --no-label wip # Composable
```

**`kata list`** gets the same filters, applied to its existing query.

#### Flag Conflicts

- `--unowned` and `--owner` are mutually exclusive (error if both provided)
- `--label` and `--no-label` can be combined and repeated

#### API Changes

Extend existing endpoints with query params:

```
GET /api/v1/projects/{pid}/ready?unowned=true&label=backlog&exclude_label=wip
GET /api/v1/projects/{pid}/issues?owner=agent1&label=bug
```

#### SQL Changes

For label filtering, join to `issue_labels`:
```sql
-- --label backlog --label urgent (AND logic)
AND EXISTS (SELECT 1 FROM issue_labels WHERE issue_id = i.id AND label = 'backlog')
AND EXISTS (SELECT 1 FROM issue_labels WHERE issue_id = i.id AND label = 'urgent')

-- --no-label wip
AND NOT EXISTS (SELECT 1 FROM issue_labels WHERE issue_id = i.id AND label = 'wip')
```

### 4.2 `kata claim` Command

#### Interface

```bash
kata claim <issue-ref>              # Claim for current actor
kata claim <issue-ref> --as agent1  # Claim for specific actor
kata claim <issue-ref> --force      # Claim even if owned by someone else
kata claim <issue-ref> --comment "Starting work"  # Optional comment
```

#### Behavior

1. Resolve `<issue-ref>` to an issue (same resolution as other commands)
2. Check current owner:
   - **Unowned**: Set owner to actor, return success
   - **Owned by same actor**: No-op, return success (idempotent)
   - **Owned by different actor**: Return error (unless `--force`)
3. If `--force` and owned by different actor: Reassign to new actor
4. Update `updated_at` timestamp (bumps issue in ready ordering)
5. Emit `issue.assigned` event (reuse existing event type)

#### Output

**Human-readable (default):**
```
abc4 claimed by agent1
abc4 already claimed by agent1 (no-op)
```

**Error case:**
```
error: abc4 is already claimed by agent2
hint: use --force to reassign
```

**JSON mode (`--json`):**
```json
{
  "kata_api_version": 1,
  "issue": {
    "short_id": "abc4",
    "title": "fix login race",
    "owner": "agent1"
  },
  "changed": true,
  "previous_owner": null
}
```

#### API

New action endpoint (follows existing `assign`/`unassign` pattern):

```
POST /api/v1/projects/{pid}/issues/{ref}/actions/claim
Content-Type: application/json

{
  "actor": "agent1",
  "force": false
}
```

**Response 200:**
```json
{
  "issue": { ... },
  "changed": true,
  "previous_owner": null
}
```

**Response 409 (conflict):**
```json
{
  "error": "already_claimed",
  "message": "issue is already claimed by agent2",
  "current_owner": "agent2"
}
```

#### Implementation Note

`claim` is essentially `assign` with:
- Conflict detection (409 if owned by different actor)
- `--force` to override
- Slightly different output messaging

Shares handler logic with existing `handlers_actions.go`.

### 4.3 Agent Workflow & Documentation

#### Recommended Agent Workflow

```bash
# 1. Find available work
ISSUE=$(kata ready --unowned --json | jq -r '.issues[0].short_id')

# 2. Claim it (fails if someone else got there first)
if ! kata claim "$ISSUE"; then
  # Retry with next issue, or exit
  exit 1
fi

# 3. Do the work
# ...

# 4. Close when done
kata close "$ISSUE" --done --message "Fixed" --commit "$SHA"
```

#### Handling Claim Conflicts

When `kata claim` returns a conflict:
1. Query `kata ready --unowned` again for a fresh list
2. Pick another issue
3. Retry claim

#### Optional Label Conventions

| Label | Meaning |
|-------|---------|
| `wip` | Work in progress (agent convention) |
| `blocked:manual` | Needs human intervention |
| `deferred` | Postponed intentionally |

Agents can filter: `kata ready --unowned --no-label wip --no-label blocked:manual`

#### Release of Claimed Issues

```bash
kata unassign <ref>  # Existing command, releases the claim
```

Or force-reassign:
```bash
kata claim <ref> --force --as agent2
```

## 5. Testing Strategy

### Unit Tests (db layer)

**`queries_ready_test.go`** — extend existing tests:
- `TestReadyIssues_FilterByUnowned` — returns only owner=NULL issues
- `TestReadyIssues_FilterByOwner` — returns only owner=X issues
- `TestReadyIssues_FilterByLabel` — returns only issues with label
- `TestReadyIssues_FilterByNoLabel` — excludes issues with label
- `TestReadyIssues_FilterComposition` — multiple filters combine correctly

**`queries_list_test.go`** — same filter tests for list endpoint

### Handler Tests (daemon layer)

**`handlers_ready_test.go`**:
- Query param parsing for `unowned`, `owner`, `label`, `exclude_label`
- Mutual exclusion error for `--unowned` + `--owner`

**`handlers_actions_test.go`** — new claim action:
- `TestClaim_UnownedIssue` — succeeds, sets owner
- `TestClaim_AlreadyOwnedBySameActor` — no-op, returns success
- `TestClaim_AlreadyOwnedByDifferentActor` — returns 409
- `TestClaim_ForceReassign` — succeeds with force flag
- `TestClaim_UpdatesTimestamp` — `updated_at` changes

### CLI Tests

**`ready_test.go`**:
- Flag parsing for new filters
- Mutual exclusion error output

**`claim_test.go`** (new):
- Success case output
- Conflict case output and exit code
- `--force` behavior
- `--json` output format
- `--comment` adds follow-up comment

### Integration/E2E

```go
func TestAgentClaimWorkflow(t *testing.T) {
    // Create two issues, one owned, one unowned
    // kata ready --unowned returns only the unowned one
    // kata claim succeeds
    // kata ready --unowned no longer returns it
    // Second claim attempt returns conflict
}
```

## 6. Files to Change

| Area | Files |
|------|-------|
| CLI | `cmd/kata/ready.go`, `cmd/kata/list.go`, `cmd/kata/claim.go` (new) |
| Daemon | `internal/daemon/handlers_ready.go`, `internal/daemon/handlers_issues.go`, `internal/daemon/handlers_actions.go` |
| DB | `internal/db/queries.go` (extend `ReadyIssues`, `ListIssues`) |
| Tests | `*_test.go` counterparts for each |
| Docs | `README.md`, `cmd/kata/quickstart.go` |

## 7. Not In Scope

- No new status values (stays `open`/`closed`)
- No schema migration (uses existing `owner` and `issue_labels`)
- No `--claim` flag on `kata ready` (atomic claim-from-ready deferred)
- No `defer_until` timestamp field (scheduling feature, orthogonal)
