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
// of a quiet hub project costs the same whatever the project's size, both
// while an unrelated project keeps writing past the poll's cursor and while
// nothing on the hub writes at all. The caller chooses the cost measure and
// its minimum meaningful value.
func RunQuietFederationPollGrowth(t *testing.T, open func(*testing.T) db.Storage, measure growthtest.Measure, floor float64) {
	t.Helper()
	for _, busy := range []bool{true, false} {
		name := "idle hub"
		if busy {
			name = "busy hub"
		}
		t.Run(name, func(t *testing.T) {
			growthtest.Run(t, growthtest.Check{
				Small: 100, Large: 1000,
				MaxRatio: 2, Floor: floor,
				Measure: measure,
				Build: func(_ testing.TB, size int) (run, reset func()) {
					hub := newQuietPollHub(t, open(t), size)
					reset = func() {}
					if busy {
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

// RunRememberedFederationPollCost checks that what a store remembers from
// earlier polls never makes an empty poll cost more than the same poll on a
// store that remembers nothing, after a short gap of unrelated writes and after
// a long one. forget clears what a store remembers about earlier polls.
func RunRememberedFederationPollCost(t *testing.T, open func(*testing.T) db.Storage, forget func(db.Storage), measure growthtest.Measure, floor float64) {
	t.Helper()
	for _, gap := range []int{100, 5000} {
		t.Run(fmt.Sprintf("gap %d", gap), func(t *testing.T) {
			cost := func(remember bool) float64 {
				hub := newQuietPollHub(t, open(t), 100)
				hub.unrelatedComment()
				require.Empty(t, hub.poll().Events)
				return measure(func() { require.Empty(t, hub.poll().Events) }, func() {
					hub.unrelatedEvents(gap)
					if !remember {
						forget(hub.store)
					}
				})
			}
			remembered, forgotten := cost(true), cost(false)
			t.Logf("remembered=%v forgotten=%v", remembered, forgotten)
			require.LessOrEqual(t, remembered, max(forgotten*1.2, floor))
		})
	}
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

// newQuietPollHub seeds a quiet federated project with issues issues and a
// busy one, and catches a poll cursor up to the quiet project's feed.
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
	for range issues {
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hub.quiet.ID, Title: "Quiet", Author: "tester"})
		require.NoError(t, err)
	}
	for page := hub.poll(); len(page.Events) > 0; page = hub.poll() {
		hub.cursor = page.NextAfterID
	}
	return hub
}

func (h *quietPollHub) poll() db.FederationReadResult {
	h.t.Helper()
	out, err := h.store.ReadFederation(h.t.Context(), db.FederationReadParams{ProjectID: h.quiet.ID, Features: db.SupportedEventFeatures, AfterID: h.cursor, Limit: 1000})
	require.NoError(h.t, err)
	return out
}

func (h *quietPollHub) unrelatedComment() {
	h.t.Helper()
	_, _, err := h.store.CreateComment(h.t.Context(), db.CreateCommentParams{IssueID: h.busyIssue.ID, Body: "busy", Author: "tester"})
	require.NoError(h.t, err)
}

// unrelatedEvents appends n busy-project events in one transaction; creating
// thousands through the store API would dominate the test's run time.
func (h *quietPollHub) unrelatedEvents(n int) {
	h.t.Helper()
	ctx := h.t.Context()
	beginner, ok := h.store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(h.t, ok, "backend must expose its database handle")
	tx, err := beginner.BeginTx(ctx, nil)
	require.NoError(h.t, err)
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 VALUES($1,$2,$3,$4,$5,'issue.updated','tester','{}',1,0,$6)`)
	require.NoError(h.t, err)
	hash := strings.Repeat("b", 64)
	for range n {
		h.inserted++
		_, err = stmt.ExecContext(ctx, fmt.Sprintf("01M9%022d", h.inserted), h.store.InstanceUID(), h.busy.ID, h.busy.Name, h.busyIssue.UID, hash)
		require.NoError(h.t, err)
	}
	require.NoError(h.t, stmt.Close())
	require.NoError(h.t, tx.Commit())
}
