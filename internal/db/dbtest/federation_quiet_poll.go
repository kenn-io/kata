package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/growthtest"
)

// RunQuietFederationPollGrowth checks that repeating an empty federation poll
// of a quiet hub project costs the same whatever the project's size: while an
// unrelated project keeps writing past the poll's cursor, after the hub wrote
// long past it, and while nothing on the hub writes at all. Polls follow the cursor
// the hub returns, as a spoke does. The caller chooses the cost measure and its
// minimum meaningful value.
func RunQuietFederationPollGrowth(t *testing.T, open func(*testing.T) db.Storage, measure growthtest.Measure, floor float64) {
	t.Helper()
	for _, hubCase := range []struct {
		name         string
		small, large int
		maxRatio     float64
		behind       int
		hubWrites    bool
	}{
		{name: "busy hub", small: 100, large: 1000, maxRatio: 2, hubWrites: true},
		// Ten times the issues, where a per-issue cost per poll would show.
		{name: "busy hub large project", small: 1000, large: 10000, maxRatio: 1.5, hubWrites: true},
		{name: "busy hub long after the cursor", small: 100, large: 1000, maxRatio: 2, behind: 3000, hubWrites: true},
		{name: "idle hub after writes past the cursor", small: 100, large: 1000, maxRatio: 2, behind: 500},
		{name: "idle hub", small: 100, large: 1000, maxRatio: 2},
	} {
		t.Run(hubCase.name, func(t *testing.T) {
			growthtest.Run(t, growthtest.Check{
				Small: hubCase.small, Large: hubCase.large,
				MaxRatio: hubCase.maxRatio, Floor: floor,
				Measure: measure,
				Build: func(_ testing.TB, size int) (run, reset func()) {
					hub := newQuietPollHub(t, open(t), size)
					hub.unrelatedEvents(hubCase.behind)
					reset = func() {}
					if hubCase.hubWrites {
						reset = hub.unrelatedComment
					}
					return func() { require.Empty(t, hub.poll().Events) }, reset
				},
			})
		})
	}
}

// RunResumedFederationPollGrowth checks that an empty poll resuming after a
// long gap of unrelated hub writes costs the same whatever the gap's length.
func RunResumedFederationPollGrowth(t *testing.T, open func(*testing.T) db.Storage, measure growthtest.Measure, floor float64) {
	t.Helper()
	growthtest.Run(t, growthtest.Check{
		Small: 2000, Large: 20000,
		MaxRatio: 2, Floor: floor,
		Measure: measure,
		Build: func(_ testing.TB, size int) (run, reset func()) {
			hub := newQuietPollHub(t, open(t), 100)
			// The project is caught up past an unrelated write before the gap
			// opens.
			hub.unrelatedComment()
			require.Empty(t, hub.poll().Events)
			return func() { require.Empty(t, hub.poll().Events) }, func() { hub.unrelatedEvents(size) }
		},
	})
}

type quietPollHub struct {
	t         *testing.T
	store     db.Storage
	quiet     db.Project
	busy      db.Project
	busyIssue db.Issue
	cursor    int64
	inserted  int
}

// newQuietPollHub seeds a quiet federated project with the given number of
// issues and a busy one, and catches a poll cursor up to the quiet project's feed.
func newQuietPollHub(t *testing.T, store db.Storage, issues int) *quietPollHub {
	t.Helper()
	ctx := t.Context()
	hub := &quietPollHub{t: t, store: store}
	var err error
	hub.quiet, err = store.CreateProject(ctx, "quiet-project")
	require.NoError(t, err)
	hub.busy, err = store.CreateProject(ctx, "busy-project")
	require.NoError(t, err)
	for _, p := range []db.Project{hub.quiet, hub.busy} {
		_, err = store.EnableProjectFederation(ctx, p.ID, "tester")
		require.NoError(t, err)
	}
	hub.busyIssue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hub.busy.ID, Title: "Busy", Author: "tester"})
	require.NoError(t, err)
	appendQuietIssues(t, store, hub.quiet, issues)
	hub.catchUp()
	return hub
}

// appendQuietIssues seeds issues without CreateIssue's per-issue cost. Like
// appendUnrelatedEvents, it uses a transaction; it writes one issue.updated
// event per issue. Keep its inserts in step with the issues and events schema.
func appendQuietIssues(t *testing.T, store db.Storage, project db.Project, n int) {
	t.Helper()
	ctx := t.Context()
	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok, "backend must expose its database handle")
	tx, err := beginner.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	issues, err := tx.PrepareContext(ctx, `INSERT INTO issues(uid,short_id,project_id,title,author) VALUES($1,$2,$3,'Quiet','tester')`)
	require.NoError(t, err)
	events, err := tx.PrepareContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 VALUES($1,$2,$3,$4,$5,'issue.updated','tester','{}',1,0,$6)`)
	require.NoError(t, err)
	hash := strings.Repeat("a", 64)
	for i := range n {
		uid := fmt.Sprintf("%026d", i)
		_, err = issues.ExecContext(ctx, uid, uid[20:], project.ID)
		require.NoError(t, err)
		_, err = events.ExecContext(ctx, fmt.Sprintf("01M7%022d", i), store.InstanceUID(), project.ID, project.Name, uid, hash)
		require.NoError(t, err)
	}
	require.NoError(t, issues.Close())
	require.NoError(t, events.Close())
	require.NoError(t, tx.Commit())
}

func (h *quietPollHub) catchUp() {
	h.t.Helper()
	for drained := false; !drained; {
		drained = len(h.poll().Events) == 0
	}
}

// poll reads one page and moves the cursor where the hub says, as a spoke
// does.
func (h *quietPollHub) poll() db.FederationReadResult {
	h.t.Helper()
	out, err := h.store.ReadFederation(h.t.Context(), db.FederationReadParams{ProjectID: h.quiet.ID, Features: db.SupportedEventFeatures, AfterID: h.cursor, Limit: 1000})
	require.NoError(h.t, err)
	h.cursor = out.NextAfterID
	return out
}

func (h *quietPollHub) unrelatedComment() {
	h.t.Helper()
	_, _, err := h.store.CreateComment(h.t.Context(), db.CreateCommentParams{IssueID: h.busyIssue.ID, Body: "busy", Author: "tester"})
	require.NoError(h.t, err)
}

// unrelatedEvents appends n busy-project events.
func (h *quietPollHub) unrelatedEvents(n int) {
	h.t.Helper()
	appendUnrelatedEvents(h.t, h.store, h.busy, h.busyIssue, h.inserted, n)
	h.inserted += n
}

// appendUnrelatedEvents appends n events about issue, stamped to project, in
// one transaction; creating thousands through the store API would dominate a
// test's run time. Event UIDs continue from first, so each caller keeps its
// own count.
func appendUnrelatedEvents(t *testing.T, store db.Storage, project db.Project, issue db.Issue, first, n int) {
	t.Helper()
	ctx := t.Context()
	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok, "backend must expose its database handle")
	tx, err := beginner.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 VALUES($1,$2,$3,$4,$5,'issue.updated','tester','{}',1,0,$6)`)
	require.NoError(t, err)
	hash := strings.Repeat("b", 64)
	for i := range n {
		_, err = stmt.ExecContext(ctx, fmt.Sprintf("01M9%022d", first+i+1), store.InstanceUID(), project.ID, project.Name, issue.UID, hash)
		require.NoError(t, err)
	}
	require.NoError(t, stmt.Close())
	require.NoError(t, tx.Commit())
}
