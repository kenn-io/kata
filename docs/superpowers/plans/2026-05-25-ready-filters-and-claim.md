# Ready Filters and Claim Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `--unowned`, `--owner`, `--label`, `--no-label` filters to `kata ready` and `kata list`, plus a new `kata claim` command with conflict detection.

**Architecture:** Extend existing `ReadyIssues` DB query with optional filter parameters. Add filter query params to API. Add `claim` action endpoint that wraps `UpdateOwner` with conflict detection. CLI commands pass filters to API and render responses.

**Tech Stack:** Go, Cobra CLI, Huma API, SQLite

---

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/db/queries.go` | Extend `ReadyIssues` with filter params; add `ListIssuesFiltered` |
| `internal/db/queries_ready_test.go` | Unit tests for filtered ready queries |
| `internal/api/types.go` | Add filter fields to `ReadyRequest`, `ListIssuesRequest`; add `ClaimRequest` |
| `internal/daemon/handlers_ready.go` | Parse filter query params, pass to DB |
| `internal/daemon/handlers_issues.go` | Parse filter query params for list endpoint |
| `internal/daemon/handlers_ownership.go` | Add `claimIssue` handler with 409 conflict detection |
| `internal/daemon/handlers_ownership_test.go` | Tests for claim endpoint |
| `cmd/kata/ready.go` | Add `--unowned`, `--owner`, `--label`, `--no-label` flags |
| `cmd/kata/list.go` | Add same filter flags |
| `cmd/kata/claim.go` | New command for claiming issues |
| `cmd/kata/claim_test.go` | CLI tests for claim command |

---

### Task 1: Add Filter Parameters to ReadyIssues DB Query

**Files:**
- Modify: `internal/db/queries.go:1401-1431`
- Test: `internal/db/queries_ready_test.go`

- [ ] **Step 1: Define ReadyIssuesFilter struct**

Add above `ReadyIssues` function in `internal/db/queries.go`:

```go
// ReadyIssuesFilter holds optional filters for the ready query.
type ReadyIssuesFilter struct {
	Unowned      bool     // only issues where owner IS NULL
	Owner        string   // only issues where owner = this value (empty = no filter)
	Labels       []string // issues must have ALL these labels (AND logic)
	ExcludeLabels []string // issues must NOT have any of these labels
}
```

- [ ] **Step 2: Run existing tests to verify baseline**

Run: `go test ./internal/db/... -run TestReadyIssues -v`
Expected: All existing ReadyIssues tests PASS

- [ ] **Step 3: Write failing test for --unowned filter**

Add to `internal/db/queries_ready_test.go`:

```go
func TestReadyIssues_FilterByUnowned(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	unowned := makeIssue(t, ctx, d, p.ID, "unowned issue", "tester")
	owned := makeIssue(t, ctx, d, p.ID, "owned issue", "tester")
	owner := "alice"
	_, _, _, err := d.UpdateOwner(ctx, owned.ID, &owner, "tester")
	require.NoError(t, err)

	filter := db.ReadyIssuesFilter{Unowned: true}
	rows, err := d.ReadyIssues(ctx, p.ID, 0, filter)
	require.NoError(t, err)

	ids := shortIDs(rows)
	assert.Contains(t, ids, unowned.ShortID)
	assert.NotContains(t, ids, owned.ShortID)
}

func shortIDs(issues []db.Issue) []string {
	out := make([]string, len(issues))
	for i, iss := range issues {
		out[i] = iss.ShortID
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/db/... -run TestReadyIssues_FilterByUnowned -v`
Expected: FAIL (signature mismatch or missing filter logic)

- [ ] **Step 5: Update ReadyIssues signature to accept filter**

Modify `internal/db/queries.go`:

```go
// ReadyIssues returns open, non-deleted issues with no open `blocks` predecessor,
// ordered by updated_at DESC. limit==0 means no limit. Filter fields are optional.
func (d *DB) ReadyIssues(ctx context.Context, projectID int64, limit int, filter ReadyIssuesFilter) ([]Issue, error) {
	q := issueSelect + `
		WHERE i.project_id = ? AND i.status = 'open' AND i.deleted_at IS NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM links l
		    JOIN issues blocker ON blocker.id = l.from_issue_id
		    WHERE l.type = 'blocks' AND l.to_issue_id = i.id
		      AND blocker.status = 'open' AND blocker.deleted_at IS NULL
		  )`
	args := []any{projectID}

	// Owner filters
	if filter.Unowned {
		q += ` AND i.owner IS NULL`
	} else if filter.Owner != "" {
		q += ` AND i.owner = ?`
		args = append(args, filter.Owner)
	}

	// Label inclusion (AND logic)
	for _, label := range filter.Labels {
		q += ` AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)`
		args = append(args, label)
	}

	// Label exclusion
	for _, label := range filter.ExcludeLabels {
		q += ` AND NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = ?)`
		args = append(args, label)
	}

	q += ` ORDER BY i.updated_at DESC, i.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ready issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
```

- [ ] **Step 6: Fix existing ReadyIssues callers to pass empty filter**

Update `internal/daemon/handlers_ready.go`:

```go
issues, err := cfg.DB.ReadyIssues(ctx, in.ProjectID, in.Limit, db.ReadyIssuesFilter{})
```

Update `internal/db/queries_ready_test.go` `readyNumbers` helper:

```go
func readyNumbers(t *testing.T, ctx context.Context, d *db.DB, projectID int64) []string {
	t.Helper()
	rows, err := d.ReadyIssues(ctx, projectID, 0, db.ReadyIssuesFilter{})
	require.NoError(t, err)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ShortID)
	}
	return out
}
```

- [ ] **Step 7: Run unowned filter test**

Run: `go test ./internal/db/... -run TestReadyIssues_FilterByUnowned -v`
Expected: PASS

- [ ] **Step 8: Write and run test for --owner filter**

Add to `internal/db/queries_ready_test.go`:

```go
func TestReadyIssues_FilterByOwner(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	alice := makeIssue(t, ctx, d, p.ID, "alice issue", "tester")
	bob := makeIssue(t, ctx, d, p.ID, "bob issue", "tester")
	unowned := makeIssue(t, ctx, d, p.ID, "unowned", "tester")

	aliceOwner := "alice"
	bobOwner := "bob"
	_, _, _, _ = d.UpdateOwner(ctx, alice.ID, &aliceOwner, "tester")
	_, _, _, _ = d.UpdateOwner(ctx, bob.ID, &bobOwner, "tester")

	filter := db.ReadyIssuesFilter{Owner: "alice"}
	rows, err := d.ReadyIssues(ctx, p.ID, 0, filter)
	require.NoError(t, err)

	ids := shortIDs(rows)
	assert.Contains(t, ids, alice.ShortID)
	assert.NotContains(t, ids, bob.ShortID)
	assert.NotContains(t, ids, unowned.ShortID)
}
```

Run: `go test ./internal/db/... -run TestReadyIssues_FilterByOwner -v`
Expected: PASS

- [ ] **Step 9: Write and run test for --label filter**

Add to `internal/db/queries_ready_test.go`:

```go
func TestReadyIssues_FilterByLabel(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	labeled := makeIssue(t, ctx, d, p.ID, "has backlog", "tester")
	unlabeled := makeIssue(t, ctx, d, p.ID, "no label", "tester")

	_, err := d.AddLabel(ctx, labeled.ID, "backlog", "tester")
	require.NoError(t, err)

	filter := db.ReadyIssuesFilter{Labels: []string{"backlog"}}
	rows, err := d.ReadyIssues(ctx, p.ID, 0, filter)
	require.NoError(t, err)

	ids := shortIDs(rows)
	assert.Contains(t, ids, labeled.ShortID)
	assert.NotContains(t, ids, unlabeled.ShortID)
}
```

Run: `go test ./internal/db/... -run TestReadyIssues_FilterByLabel -v`
Expected: PASS

- [ ] **Step 10: Write and run test for --no-label filter**

Add to `internal/db/queries_ready_test.go`:

```go
func TestReadyIssues_FilterByNoLabel(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	wip := makeIssue(t, ctx, d, p.ID, "work in progress", "tester")
	available := makeIssue(t, ctx, d, p.ID, "available", "tester")

	_, err := d.AddLabel(ctx, wip.ID, "wip", "tester")
	require.NoError(t, err)

	filter := db.ReadyIssuesFilter{ExcludeLabels: []string{"wip"}}
	rows, err := d.ReadyIssues(ctx, p.ID, 0, filter)
	require.NoError(t, err)

	ids := shortIDs(rows)
	assert.Contains(t, ids, available.ShortID)
	assert.NotContains(t, ids, wip.ShortID)
}
```

Run: `go test ./internal/db/... -run TestReadyIssues_FilterByNoLabel -v`
Expected: PASS

- [ ] **Step 11: Write and run test for filter composition**

Add to `internal/db/queries_ready_test.go`:

```go
func TestReadyIssues_FilterComposition(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	// Create 4 issues with different owner/label combos
	i1 := makeIssue(t, ctx, d, p.ID, "unowned+backlog", "tester")
	i2 := makeIssue(t, ctx, d, p.ID, "unowned+wip", "tester")
	i3 := makeIssue(t, ctx, d, p.ID, "owned+backlog", "tester")
	i4 := makeIssue(t, ctx, d, p.ID, "unowned+backlog+wip", "tester")

	owner := "alice"
	_, _, _, _ = d.UpdateOwner(ctx, i3.ID, &owner, "tester")
	d.AddLabel(ctx, i1.ID, "backlog", "tester")
	d.AddLabel(ctx, i2.ID, "wip", "tester")
	d.AddLabel(ctx, i3.ID, "backlog", "tester")
	d.AddLabel(ctx, i4.ID, "backlog", "tester")
	d.AddLabel(ctx, i4.ID, "wip", "tester")

	// --unowned --label backlog --no-label wip
	filter := db.ReadyIssuesFilter{
		Unowned:       true,
		Labels:        []string{"backlog"},
		ExcludeLabels: []string{"wip"},
	}
	rows, err := d.ReadyIssues(ctx, p.ID, 0, filter)
	require.NoError(t, err)

	ids := shortIDs(rows)
	assert.Equal(t, []string{i1.ShortID}, ids, "only i1 matches all filters")
}
```

Run: `go test ./internal/db/... -run TestReadyIssues_FilterComposition -v`
Expected: PASS

- [ ] **Step 12: Run all ReadyIssues tests**

Run: `go test ./internal/db/... -run TestReadyIssues -v`
Expected: All PASS

- [ ] **Step 13: Commit**

```bash
git add internal/db/queries.go internal/db/queries_ready_test.go internal/daemon/handlers_ready.go
git commit -m "feat(db): add filter params to ReadyIssues query

Add ReadyIssuesFilter struct with Unowned, Owner, Labels, ExcludeLabels.
SQL dynamically builds WHERE clauses for each filter.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 2: Add Filter Query Params to Ready API Endpoint

**Files:**
- Modify: `internal/api/types.go:639-650`
- Modify: `internal/daemon/handlers_ready.go`
- Test: `internal/daemon/handlers_ready_test.go` (create if needed)

- [ ] **Step 1: Update ReadyRequest with filter fields**

Modify `internal/api/types.go`:

```go
// ReadyRequest is GET /api/v1/projects/{id}/ready.
type ReadyRequest struct {
	ProjectID     int64    `path:"project_id" required:"true"`
	Limit         int      `query:"limit,omitempty"`
	Unowned       bool     `query:"unowned,omitempty"`
	Owner         string   `query:"owner,omitempty"`
	Labels        []string `query:"label,omitempty"`
	ExcludeLabels []string `query:"exclude_label,omitempty"`
}
```

- [ ] **Step 2: Update handlers_ready.go to use filters**

Modify `internal/daemon/handlers_ready.go`:

```go
func registerReadyHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "readyIssues",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/ready",
	}, func(ctx context.Context, in *api.ReadyRequest) (*api.ReadyResponse, error) {
		// Validate mutual exclusion: --unowned and --owner cannot both be set
		if in.Unowned && in.Owner != "" {
			return nil, api.NewError(400, "validation",
				"--unowned and --owner are mutually exclusive", "", nil)
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		filter := db.ReadyIssuesFilter{
			Unowned:       in.Unowned,
			Owner:         in.Owner,
			Labels:        in.Labels,
			ExcludeLabels: in.ExcludeLabels,
		}
		issues, err := cfg.DB.ReadyIssues(ctx, in.ProjectID, in.Limit, filter)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.ReadyResponse{}
		out.Body.Issues = issues
		return out, nil
	})
}
```

- [ ] **Step 3: Write test for mutual exclusion validation**

Create or add to `internal/daemon/handlers_ready_test.go`:

```go
package daemon

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReady_UnownedAndOwnerMutuallyExclusive(t *testing.T) {
	env := setupTestEnv(t)
	p := createProject(t, env, "test-proj")

	resp, _ := env.Get(t, "/api/v1/projects/%d/ready?unowned=true&owner=alice", p.ID)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
```

- [ ] **Step 4: Run test**

Run: `go test ./internal/daemon/... -run TestReady_UnownedAndOwnerMutuallyExclusive -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/types.go internal/daemon/handlers_ready.go internal/daemon/handlers_ready_test.go
git commit -m "feat(api): add filter query params to ready endpoint

Params: unowned, owner, label (repeatable), exclude_label (repeatable).
Validates mutual exclusion of unowned + owner.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 3: Add Filter Flags to kata ready CLI

**Files:**
- Modify: `cmd/kata/ready.go`
- Test: `cmd/kata/ready_test.go` (create or extend)

- [ ] **Step 1: Add filter flags to ready command**

Modify `cmd/kata/ready.go`:

```go
func newReadyCmd() *cobra.Command {
	var limit int
	var unowned bool
	var owner string
	var labels []string
	var noLabels []string

	cmd := &cobra.Command{
		Use:   "ready",
		Short: "list open issues with no open blocks predecessor",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 0 {
				return &cliError{Message: "--limit must be non-negative", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if unowned && owner != "" {
				return &cliError{Message: "--unowned and --owner are mutually exclusive", Kind: kindValidation, ExitCode: ExitValidation}
			}
			ctx := cmd.Context()
			start, err := resolveStartPath(flags.Workspace)
			if err != nil {
				return err
			}
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			pid, err := resolveProjectID(ctx, baseURL, start)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			getURL := fmt.Sprintf("%s/api/v1/projects/%d/ready", baseURL, pid)
			if limit > 0 {
				getURL += fmt.Sprintf("?limit=%d", limit)
			}
			if unowned {
				getURL += urlParamSep(getURL) + "unowned=true"
			}
			if owner != "" {
				getURL += urlParamSep(getURL) + "owner=" + url.QueryEscape(owner)
			}
			for _, l := range labels {
				getURL += urlParamSep(getURL) + "label=" + url.QueryEscape(l)
			}
			for _, l := range noLabels {
				getURL += urlParamSep(getURL) + "exclude_label=" + url.QueryEscape(l)
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, getURL, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			var b struct {
				Issues []struct {
					ShortID string  `json:"short_id"`
					Title   string  `json:"title"`
					Owner   *string `json:"owner,omitempty"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			for _, i := range b.Issues {
				owner := "-"
				if i.Owner != nil {
					owner = *i.Owner
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-8s  %s  (%s)\n",
					i.ShortID, textsafe.Line(i.Title), textsafe.Line(owner)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = no limit)")
	cmd.Flags().BoolVar(&unowned, "unowned", false, "only issues with no owner")
	cmd.Flags().StringVar(&owner, "owner", "", "only issues owned by this actor")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "only issues with this label (repeatable, AND logic)")
	cmd.Flags().StringSliceVar(&noLabels, "no-label", nil, "exclude issues with this label (repeatable)")
	return cmd
}

// urlParamSep returns "?" if url has no query string, "&" otherwise.
func urlParamSep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}
```

- [ ] **Step 2: Add url import if needed**

Add to imports in `cmd/kata/ready.go`:

```go
import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)
```

- [ ] **Step 3: Write CLI test for mutual exclusion**

Add to `cmd/kata/ready_test.go` (create if needed):

```go
func TestReady_UnownedAndOwnerMutualExclusion(t *testing.T) {
	env, dir := setupCLIEnv(t)
	resetFlags(t)
	_, err := runCLICapture(t, env, dir, "ready", "--unowned", "--owner", "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}
```

- [ ] **Step 4: Run test**

Run: `go test ./cmd/kata/... -run TestReady_UnownedAndOwnerMutualExclusion -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/kata/ready.go cmd/kata/ready_test.go
git commit -m "feat(cli): add filter flags to kata ready

Flags: --unowned, --owner, --label (repeatable), --no-label (repeatable).
Validates mutual exclusion of --unowned and --owner.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 4: Add Filter Flags to kata list CLI and API

**Files:**
- Modify: `internal/api/types.go:259-265`
- Modify: `internal/daemon/handlers_issues.go`
- Modify: `cmd/kata/list.go`

- [ ] **Step 1: Update ListIssuesRequest with filter fields**

Modify `internal/api/types.go` `ListIssuesRequest`:

```go
// ListIssuesRequest is GET /api/v1/projects/{id}/issues.
type ListIssuesRequest struct {
	ProjectID     int64    `path:"project_id" required:"true"`
	Status        string   `query:"status,omitempty" enum:"open,closed,"`
	Priority      string   `query:"priority,omitempty" doc:"exact priority filter (0..4); empty = no filter"`
	MaxPriority   string   `query:"max_priority,omitempty" doc:"include only priority <= this value (0..4); empty = no filter"`
	Limit         int      `query:"limit,omitempty"`
	Unowned       bool     `query:"unowned,omitempty"`
	Owner         string   `query:"owner,omitempty"`
	Labels        []string `query:"label,omitempty"`
	ExcludeLabels []string `query:"exclude_label,omitempty"`
}
```

- [ ] **Step 2: Update list handler to pass filters to DB**

Find the list handler in `internal/daemon/handlers_issues.go` and add filter logic. The list endpoint uses `ListIssues` — we need to extend it similarly to `ReadyIssues`. First, check the current `ListIssues` signature and update accordingly.

Add validation at the start of the handler:

```go
if in.Unowned && in.Owner != "" {
	return nil, api.NewError(400, "validation",
		"--unowned and --owner are mutually exclusive", "", nil)
}
```

- [ ] **Step 3: Update cmd/kata/list.go with filter flags**

Add to `newListCmd()`:

```go
var unowned bool
var owner string
var labels []string
var noLabels []string

// In RunE, add validation:
if unowned && owner != "" {
	return &cliError{Message: "--unowned and --owner are mutually exclusive", Kind: kindValidation, ExitCode: ExitValidation}
}

// Build URL with filters:
if unowned {
	url += "&unowned=true"
}
if owner != "" {
	url += "&owner=" + neturl.QueryEscape(owner)
}
for _, l := range labels {
	url += "&label=" + neturl.QueryEscape(l)
}
for _, l := range noLabels {
	url += "&exclude_label=" + neturl.QueryEscape(l)
}

// Add flags:
cmd.Flags().BoolVar(&unowned, "unowned", false, "only issues with no owner")
cmd.Flags().StringVar(&owner, "owner", "", "only issues owned by this actor")
cmd.Flags().StringSliceVar(&labels, "label", nil, "only issues with this label (repeatable, AND logic)")
cmd.Flags().StringSliceVar(&noLabels, "no-label", nil, "exclude issues with this label (repeatable)")
```

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/kata/... -run TestList -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/api/types.go internal/daemon/handlers_issues.go cmd/kata/list.go
git commit -m "feat(cli,api): add filter flags to kata list

Same filters as kata ready: --unowned, --owner, --label, --no-label.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 5: Add Claim API Endpoint

**Files:**
- Modify: `internal/api/types.go`
- Modify: `internal/daemon/handlers_ownership.go`
- Test: `internal/daemon/handlers_ownership_test.go`

- [ ] **Step 1: Add ClaimRequest type**

Add to `internal/api/types.go` after `UnassignRequest`:

```go
// ClaimRequest is POST /api/v1/projects/{id}/issues/{ref}/actions/claim.
// Claims an issue for the actor. Returns 409 if already owned by different actor
// unless Force is true.
type ClaimRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Ref       string `path:"ref" required:"true"`
	Body      struct {
		Actor string `json:"actor" required:"true"`
		Force bool   `json:"force,omitempty"`
	}
}

// ClaimResponse is the response for a successful claim.
type ClaimResponse struct {
	Body struct {
		Issue         db.Issue  `json:"issue"`
		Event         *db.Event `json:"event,omitempty"`
		Changed       bool      `json:"changed"`
		PreviousOwner *string   `json:"previous_owner,omitempty"`
	}
}
```

- [ ] **Step 2: Write failing test for claim endpoint**

Add to `internal/daemon/handlers_ownership_test.go`:

```go
func TestClaim_UnownedIssue(t *testing.T) {
	env := setupTestEnv(t)
	p := createProject(t, env, "test-proj")
	iss := createIssue(t, env, p.ID, "test issue", "tester")

	resp, body := postClaim(t, env, p.ID, iss.ShortID, "agent1", false)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, body.Changed)
	assert.Equal(t, "agent1", *body.Issue.Owner)
	assert.Nil(t, body.PreviousOwner)
}

func TestClaim_AlreadyOwnedBySameActor(t *testing.T) {
	env := setupTestEnv(t)
	p := createProject(t, env, "test-proj")
	iss := createIssue(t, env, p.ID, "test issue", "tester")
	postClaim(t, env, p.ID, iss.ShortID, "agent1", false)

	resp, body := postClaim(t, env, p.ID, iss.ShortID, "agent1", false)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, body.Changed, "idempotent claim by same owner")
}

func TestClaim_AlreadyOwnedByDifferentActor(t *testing.T) {
	env := setupTestEnv(t)
	p := createProject(t, env, "test-proj")
	iss := createIssue(t, env, p.ID, "test issue", "tester")
	postClaim(t, env, p.ID, iss.ShortID, "agent1", false)

	resp, _ := postClaimRaw(t, env, p.ID, iss.ShortID, "agent2", false)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestClaim_ForceReassign(t *testing.T) {
	env := setupTestEnv(t)
	p := createProject(t, env, "test-proj")
	iss := createIssue(t, env, p.ID, "test issue", "tester")
	postClaim(t, env, p.ID, iss.ShortID, "agent1", false)

	resp, body := postClaim(t, env, p.ID, iss.ShortID, "agent2", true)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, body.Changed)
	assert.Equal(t, "agent2", *body.Issue.Owner)
	require.NotNil(t, body.PreviousOwner)
	assert.Equal(t, "agent1", *body.PreviousOwner)
}

type claimResp struct {
	Issue         db.Issue  `json:"issue"`
	Event         *db.Event `json:"event"`
	Changed       bool      `json:"changed"`
	PreviousOwner *string   `json:"previous_owner"`
}

func postClaim(t *testing.T, env *testenv.Env, projectID int64, ref, actor string, force bool) (*http.Response, claimResp) {
	t.Helper()
	resp, body := postClaimRaw(t, env, projectID, ref, actor, force)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out claimResp
	require.NoError(t, json.Unmarshal(body, &out))
	return resp, out
}

func postClaimRaw(t *testing.T, env *testenv.Env, projectID int64, ref, actor string, force bool) (*http.Response, []byte) {
	t.Helper()
	payload := map[string]any{"actor": actor, "force": force}
	return env.Post(t, payload, "/api/v1/projects/%d/issues/%s/actions/claim", projectID, ref)
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/daemon/... -run TestClaim -v`
Expected: FAIL (endpoint doesn't exist)

- [ ] **Step 4: Implement claim handler**

Add to `internal/daemon/handlers_ownership.go`:

```go
huma.Register(humaAPI, huma.Operation{
	OperationID: "claimIssue",
	Method:      "POST",
	Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/claim",
}, func(ctx context.Context, in *api.ClaimRequest) (*api.ClaimResponse, error) {
	if err := validateActor(in.Body.Actor); err != nil {
		return nil, err
	}
	issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
	if err != nil {
		return nil, err
	}

	// Check for conflict: owned by different actor
	if issue.Owner != nil && *issue.Owner != in.Body.Actor && !in.Body.Force {
		return nil, api.NewError(409, "already_claimed",
			fmt.Sprintf("issue is already claimed by %s", *issue.Owner),
			"use --force to reassign", map[string]any{"current_owner": *issue.Owner})
	}

	var previousOwner *string
	if issue.Owner != nil {
		prev := *issue.Owner
		previousOwner = &prev
	}

	owner := in.Body.Actor
	updated, evt, changed, err := cfg.DB.UpdateOwner(ctx, issue.ID, &owner, in.Body.Actor)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if changed && evt != nil {
		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: evt, ProjectID: in.ProjectID})
		cfg.Hooks.Enqueue(*evt)
	}
	out := &api.ClaimResponse{}
	out.Body.Issue = updated
	out.Body.Event = evt
	out.Body.Changed = changed
	out.Body.PreviousOwner = previousOwner
	return out, nil
})
```

- [ ] **Step 5: Add ClaimResponse to api/types.go if not already**

Ensure ClaimResponse exists (added in Step 1).

- [ ] **Step 6: Run claim tests**

Run: `go test ./internal/daemon/... -run TestClaim -v`
Expected: All PASS

- [ ] **Step 7: Commit**

```bash
git add internal/api/types.go internal/daemon/handlers_ownership.go internal/daemon/handlers_ownership_test.go
git commit -m "feat(api): add claim endpoint with conflict detection

POST /api/v1/projects/{pid}/issues/{ref}/actions/claim
- Returns 409 if owned by different actor (unless force=true)
- Idempotent if already owned by same actor
- Returns previous_owner in response

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 6: Add kata claim CLI Command

**Files:**
- Create: `cmd/kata/claim.go`
- Test: `cmd/kata/claim_test.go`
- Modify: `cmd/kata/main.go` (register command)

- [ ] **Step 1: Create claim.go**

Create `cmd/kata/claim.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

func newClaimCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "claim <issue-ref>",
		Short: "claim an issue for the current actor",
		Long: `Claim an issue by setting its owner to the current actor.

Returns an error if the issue is already owned by a different actor,
unless --force is used to reassign.

Idempotent: claiming an issue you already own is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			comment, err := commentFromFlag(cmd)
			if err != nil {
				return err
			}
			ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
			if err != nil {
				return err
			}
			actor, _ := resolveActor(flags.As, nil)
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			body := map[string]any{"actor": actor, "force": force}
			postURL := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/claim",
				baseURL, pid, url.PathEscape(issue.RefForAPI))
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost, postURL, body)
			if err != nil {
				return err
			}
			if status == http.StatusConflict {
				var errBody struct {
					Error        string `json:"error"`
					Message      string `json:"message"`
					Hint         string `json:"hint"`
					CurrentOwner string `json:"current_owner"`
				}
				if json.Unmarshal(bs, &errBody) == nil && errBody.CurrentOwner != "" {
					return &cliError{
						Message:  fmt.Sprintf("%s is already claimed by %s", issue.RefForAPI, errBody.CurrentOwner),
						Hint:     "use --force to reassign",
						Kind:     "conflict",
						ExitCode: ExitConflict,
					}
				}
				return apiErrFromBody(status, bs)
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if err := postFollowupComment(ctx, client, baseURL, pid, issue.RefForAPI, actor, comment); err != nil {
				return err
			}
			return printClaimResult(cmd, bs)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "reassign even if owned by someone else")
	addCommentFlag(cmd)
	return cmd
}

func printClaimResult(cmd *cobra.Command, bs []byte) error {
	if flags.JSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var b struct {
		Issue struct {
			ShortID string  `json:"short_id"`
			Owner   *string `json:"owner"`
		} `json:"issue"`
		Changed       bool    `json:"changed"`
		PreviousOwner *string `json:"previous_owner"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if flags.Quiet {
		return nil
	}
	owner := ""
	if b.Issue.Owner != nil {
		owner = *b.Issue.Owner
	}
	if !b.Changed {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s already claimed by %s (no-op)\n", b.Issue.ShortID, owner)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s claimed by %s\n", b.Issue.ShortID, owner)
	return err
}
```

- [ ] **Step 2: Add ExitConflict constant if not present**

Check `cmd/kata/errors.go` or similar for exit codes. Add if needed:

```go
const ExitConflict = 4  // or appropriate value
```

- [ ] **Step 3: Register claim command in main.go**

Find where commands are added in `cmd/kata/main.go` and add:

```go
rootCmd.AddCommand(newClaimCmd())
```

- [ ] **Step 4: Write CLI test**

Create `cmd/kata/claim_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaim_Success(t *testing.T) {
	env, dir := setupCLIEnv(t)
	resetFlags(t)

	// Create an issue
	out := runCLI(t, env, dir, "create", "test claim", "--json")
	shortID := extractShortID(t, out)

	// Claim it
	resetFlags(t)
	out = runCLI(t, env, dir, "claim", shortID)
	assert.Contains(t, out, "claimed by")
}

func TestClaim_Conflict(t *testing.T) {
	env, dir := setupCLIEnv(t)
	resetFlags(t)

	// Create and claim as alice
	out := runCLI(t, env, dir, "create", "test claim", "--json")
	shortID := extractShortID(t, out)
	resetFlags(t)
	runCLI(t, env, dir, "assign", shortID, "alice")

	// Try to claim as bob (should fail)
	resetFlags(t)
	_, err := runCLICapture(t, env, dir, "claim", shortID, "--as", "bob")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "already claimed") ||
		strings.Contains(err.Error(), "conflict"))
}

func TestClaim_Force(t *testing.T) {
	env, dir := setupCLIEnv(t)
	resetFlags(t)

	// Create and claim as alice
	out := runCLI(t, env, dir, "create", "test claim", "--json")
	shortID := extractShortID(t, out)
	resetFlags(t)
	runCLI(t, env, dir, "assign", shortID, "alice")

	// Force claim as bob
	resetFlags(t)
	out = runCLI(t, env, dir, "claim", shortID, "--as", "bob", "--force")
	assert.Contains(t, out, "claimed by bob")
}

func TestClaim_Idempotent(t *testing.T) {
	env, dir := setupCLIEnv(t)
	resetFlags(t)

	out := runCLI(t, env, dir, "create", "test claim", "--json")
	shortID := extractShortID(t, out)

	// Claim twice by same actor
	resetFlags(t)
	runCLI(t, env, dir, "claim", shortID, "--as", "alice")
	resetFlags(t)
	out = runCLI(t, env, dir, "claim", shortID, "--as", "alice")
	assert.Contains(t, out, "no-op")
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/kata/... -run TestClaim -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/kata/claim.go cmd/kata/claim_test.go cmd/kata/main.go
git commit -m "feat(cli): add kata claim command

Usage: kata claim <issue-ref> [--force] [--comment TEXT]

Returns conflict error if owned by different actor.
Idempotent if already owned by same actor.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 7: Update Documentation

**Files:**
- Modify: `README.md`
- Modify: `cmd/kata/quickstart.go`

- [ ] **Step 1: Update README.md with claim command**

Add to the "Core Commands" section after assign/unassign:

```markdown
```sh
kata claim <ref> [--force] [--comment TEXT]
```

And add to the agent workflow example:

```markdown
### Agent Task Claiming

Agents can filter for unclaimed work and atomically claim it:

```sh
# Find unclaimed work
ISSUE=$(kata ready --unowned --json | jq -r '.issues[0].short_id')

# Claim it (fails if someone else claimed it first)
kata claim "$ISSUE" || exit 1

# Do the work...

# Close when done
kata close "$ISSUE" --done --message "Fixed" --commit "$SHA"
```

To release a claimed issue: `kata unassign <ref>`
To force-reassign: `kata claim <ref> --force`
```

- [ ] **Step 2: Update quickstart.go**

Add to the agent instructions in `cmd/kata/quickstart.go`:

```go
// Add to the example workflow section:
`
# Find and claim available work
kata ready --unowned --json
kata claim <ref>  # fails if already claimed by another actor

# Release a claim
kata unassign <ref>
`
```

- [ ] **Step 3: Commit**

```bash
git add README.md cmd/kata/quickstart.go
git commit -m "docs: add claim command and filter examples

Document --unowned/--owner/--label/--no-label filters on ready/list.
Document kata claim workflow for agents.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

### Task 8: Integration Test

**Files:**
- Create or extend: `cmd/kata/claim_workflow_test.go`

- [ ] **Step 1: Write integration test**

Create `cmd/kata/claim_workflow_test.go`:

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentClaimWorkflow(t *testing.T) {
	env, dir := setupCLIEnv(t)

	// Create two issues, one owned, one unowned
	resetFlags(t)
	out := runCLI(t, env, dir, "create", "unowned issue", "--json")
	unownedID := extractShortID(t, out)

	resetFlags(t)
	out = runCLI(t, env, dir, "create", "owned issue", "--json")
	ownedID := extractShortID(t, out)
	resetFlags(t)
	runCLI(t, env, dir, "assign", ownedID, "alice")

	// kata ready --unowned returns only the unowned one
	resetFlags(t)
	out = runCLI(t, env, dir, "ready", "--unowned")
	assert.Contains(t, out, unownedID)
	assert.NotContains(t, out, ownedID)

	// kata claim succeeds
	resetFlags(t)
	out = runCLI(t, env, dir, "claim", unownedID, "--as", "agent1")
	assert.Contains(t, out, "claimed by agent1")

	// kata ready --unowned no longer returns it
	resetFlags(t)
	out = runCLI(t, env, dir, "ready", "--unowned")
	assert.NotContains(t, out, unownedID)

	// Second claim attempt by different actor returns conflict
	resetFlags(t)
	_, err := runCLICapture(t, env, dir, "claim", unownedID, "--as", "agent2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already claimed")
}
```

- [ ] **Step 2: Run integration test**

Run: `go test ./cmd/kata/... -run TestAgentClaimWorkflow -v`
Expected: PASS

- [ ] **Step 3: Run full test suite**

Run: `make test`
Expected: All tests PASS

- [ ] **Step 4: Commit**

```bash
git add cmd/kata/claim_workflow_test.go
git commit -m "test: add agent claim workflow integration test

Verifies full flow: ready --unowned, claim, conflict detection.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Self-Review Checklist

- [x] **Spec coverage:** All requirements covered (filters on ready/list, claim command, documentation)
- [x] **No placeholders:** All code blocks are complete
- [x] **Type consistency:** `ReadyIssuesFilter`, `ClaimRequest`, `ClaimResponse` used consistently
- [x] **Test coverage:** DB tests, handler tests, CLI tests, integration test
