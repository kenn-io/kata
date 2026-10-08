package todoistsync

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func statusSession(t *testing.T, f *apiFixture) StatusSession {
	t.Helper()
	c := f.cfg()
	c.StatusSync = "two-way"
	s, err := f.client().ForRun(context.Background(), c)
	require.NoError(t, err)
	return s.(StatusSession)
}
func TestStatusCloseVerifyFreshHistoryAndReopen(t *testing.T) {
	f := newAPIFixture(t)
	s := statusSession(t, f)
	c := f.cfg()
	c.StatusSync = "two-way"
	ctx := context.Background()
	admitted := 0
	admit := func() error { admitted++; return nil }
	before, err := s.ReadStatus(ctx, c, StatusTarget{ID: f.row.ID})
	require.NoError(t, err)
	require.Equal(t, "open", before.Status)
	got, err := s.WriteStatus(ctx, c, StatusTarget{ID: f.row.ID}, "closed", admit)
	require.NoError(t, err)
	require.Equal(t, "closed", got.Status)
	require.NotNil(t, got.ClosedAt)
	require.True(t, got.Version.After(before.Version))
	require.Len(t, f.posts, 1)
	require.Equal(t, 1, admitted)
	// The subtask guard asks Todoist for this task's children only.
	require.True(t, slices.ContainsFunc(f.queries, func(q url.Values) bool { return q.Get("parent_id") == f.row.ID }))
	_, err = s.WriteStatus(ctx, c, StatusTarget{ID: f.row.ID}, "closed", admit)
	require.NoError(t, err)
	require.Len(t, f.posts, 1)
	got, err = s.WriteStatus(ctx, c, StatusTarget{ID: f.row.ID}, "open", admit)
	require.NoError(t, err)
	require.Equal(t, "open", got.Status)
	require.Len(t, f.posts, 2)
}
func TestStatusGuardsBeforeMutation(t *testing.T) {
	for _, kind := range []string{"one-way", "recurring", "child cascade", "ancestor cascade", "archived section", "admission", "wrong project", "account changed", "missing"} {
		t.Run(kind, func(t *testing.T) {
			f := newAPIFixture(t)
			c := f.cfg()
			c.StatusSync = "two-way"
			s := statusSession(t, f)
			desired := "closed"
			admit := func() error { return nil }
			switch kind {
			case "one-way":
				c.StatusSync = "one-way"
			case "recurring":
				f.recurring = true
			case "child cascade":
				f.child = true
			case "ancestor cascade":
				f.active = false
				f.row.Checked = new(true)
				f.row.CompletedAt = new(f.row.UpdatedAt)
				f.row.ParentID = "parent123"
				desired = "open"
			case "archived section":
				f.active = false
				f.row.Checked = new(true)
				f.row.CompletedAt = new(f.row.UpdatedAt)
				f.row.SectionID = "section123"
				f.sectionArchived = true
				desired = "open"
			case "admission":
				admit = func() error { return errors.New("intent changed") }
			case "wrong project":
				f.row.ProjectID = "foreign"
			case "account changed":
				f.account = "different"
			case "missing":
				f.active = false
			}
			_, err := s.WriteStatus(context.Background(), c, StatusTarget{ID: f.row.ID}, desired, admit)
			require.Error(t, err)
			require.Empty(t, f.posts)
			require.NotContains(t, err.Error(), "fixture-secret")
		})
	}
}
func TestStatusAmbiguousWriteRecoversWithoutDuplicate(t *testing.T) {
	f := newAPIFixture(t)
	s := statusSession(t, f)
	c := f.cfg()
	c.StatusSync = "two-way"
	f.lost = true
	_, err := s.WriteStatus(context.Background(), c, StatusTarget{ID: f.row.ID}, "closed", func() error { return nil })
	var classified *issuesync.StatusError
	require.ErrorAs(t, err, &classified)
	require.True(t, classified.Ambiguous)
	require.Len(t, f.posts, 1)
	f.lost = false
	got, err := s.WriteStatus(context.Background(), c, StatusTarget{ID: f.row.ID}, "closed", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", got.Status)
	require.Len(t, f.posts, 1)
}
func TestStatusWrongVerificationKeepsAmbiguousIntent(t *testing.T) {
	f := newAPIFixture(t)
	s := statusSession(t, f)
	c := f.cfg()
	c.StatusSync = "two-way"
	f.wrong = true
	_, err := s.WriteStatus(context.Background(), c, StatusTarget{ID: f.row.ID}, "closed", func() error { return nil })
	var classified *issuesync.StatusError
	require.ErrorAs(t, err, &classified)
	require.True(t, classified.Ambiguous)
	require.Len(t, f.posts, 1)
}

// Contract: a fresh active read wins when a task reopens during history pagination.
func TestStatusHistoryCannotHideReopen(t *testing.T) {
	f := newAPIFixture(t)
	f.active = false
	f.row.Checked = new(true)
	f.row.CompletedAt = new(f.row.UpdatedAt)
	f.reopenHistory = true
	c := f.cfg()
	session, err := f.client().ForRun(context.Background(), c)
	require.NoError(t, err)
	observed, err := session.(StatusSession).ReadStatus(context.Background(), c, StatusTarget{ID: f.row.ID})
	require.NoError(t, err)
	require.Equal(t, "open", observed.Status)
	require.Empty(t, f.posts)
}

// Contract: Todoist may keep updated_at null across completion and reopening;
// a fresh open observation must still advance the stored completion version.
func TestStatusNullUpdatedAtReopenAdvancesVersion(t *testing.T) {
	for _, mode := range []string{"incoming", "outbound"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPIFixture(t)
			completedAt := f.now.Add(-time.Hour)
			f.row.AddedAt = f.now.Add(-48 * time.Hour)
			f.nullFields = []string{"updated_at"}
			if mode == "incoming" {
				f.active = true
				f.row.Checked = new(false)
				f.row.CompletedAt = nil
				f.row.UpdatedAt = f.now
			} else {
				f.active = false
				f.row.Checked = new(true)
				f.row.CompletedAt = new(completedAt)
				f.row.UpdatedAt = completedAt
			}
			c := f.cfg()
			c.StatusSync = "two-way"
			session, err := f.client().ForRun(context.Background(), c)
			require.NoError(t, err)
			status := session.(StatusSession)
			closed := "closed"
			prior := &db.IssueStatusObservation{Raw: &closed, Version: completedAt}
			target := StatusTarget{ID: f.row.ID, Prior: prior}

			var observed issuesync.StatusObservation
			if mode == "incoming" {
				observed, err = status.ReadStatus(context.Background(), c, target)
			} else {
				observed, err = status.WriteStatus(context.Background(), c, target, "open", func() error { return nil })
				require.Len(t, f.posts, 1)
			}
			require.NoError(t, err)
			require.Equal(t, "open", observed.Status)
			require.True(t, observed.Version.After(prior.Version), "the verified reopen must advance past the prior completion")
		})
	}
}

// Contract: writeback requires positive evidence of non-recurring task state.
func TestStatusBlocksUnknownRecurrenceBeforeDispatch(t *testing.T) {
	for _, omitDue := range []bool{true, false} {
		f := newAPIFixture(t)
		f.omitDue = omitDue
		f.omitRecurring = !omitDue
		c := f.cfg()
		c.StatusSync = "two-way"
		session, err := f.client().ForRun(context.Background(), c)
		require.NoError(t, err)
		_, err = session.(StatusSession).WriteStatus(context.Background(), c, StatusTarget{ID: f.row.ID}, "closed", func() error { return nil })
		require.Error(t, err)
		require.Empty(t, f.posts)
	}
}

// Contract: completion/reopen cannot dispatch without known task hierarchy.
func TestStatusBlocksUnknownHierarchyBeforeDispatch(t *testing.T) {
	f := newAPIFixture(t)
	f.omitHierarchy = true
	c := f.cfg()
	c.StatusSync = "two-way"
	session, err := f.client().ForRun(context.Background(), c)
	require.NoError(t, err)
	_, err = session.(StatusSession).WriteStatus(context.Background(), c, StatusTarget{ID: f.row.ID}, "closed", func() error { return nil })
	require.Error(t, err)
	require.Empty(t, f.posts)
}

// Contract: missing parent evidence in an active child page cannot authorize cascading completion.
func TestStatusBlocksIncompleteChildHierarchy(t *testing.T) {
	f := newAPIFixture(t)
	f.child = true
	f.omitChildHierarchy = true
	c := f.cfg()
	c.StatusSync = "two-way"
	session, err := f.client().ForRun(context.Background(), c)
	require.NoError(t, err)
	_, err = session.(StatusSession).WriteStatus(context.Background(), c, StatusTarget{ID: f.row.ID}, "closed", func() error { return nil })
	require.Error(t, err)
	require.Empty(t, f.posts)
}

// Contract: a task last observed open completed after that observation, so its
// history lookup starts there; a repeated lookup in the same run reads nothing.
func TestStatusHistoryLookupStartsAtLastOpenObservation(t *testing.T) {
	f := newAPIFixture(t)
	f.active = false
	f.row.Checked = new(true)
	f.row.CompletedAt = new(f.now.Add(-time.Hour))
	f.row.UpdatedAt = *f.row.CompletedAt
	c := f.cfg()
	session, err := f.client().ForRun(context.Background(), c)
	require.NoError(t, err)
	open := "open"
	prior := &db.IssueStatusObservation{Raw: &open, Version: f.now.Add(-2 * time.Hour)}
	for range 2 {
		observed, err := session.(StatusSession).ReadStatus(context.Background(), c, StatusTarget{ID: f.row.ID, Prior: prior})
		require.NoError(t, err)
		require.Equal(t, "closed", observed.Status)
	}
	var since []string
	for _, q := range f.queries {
		since = append(since, q.Get("since"))
	}
	require.Equal(t, []string{prior.Version.Add(-2 * time.Minute).Format(time.RFC3339Nano)}, since)
	require.Equal(t, 1, f.requestsTo("/api/v1/projects/"+c.ProjectID), "project scope checked once per session")
}
