package todoistsync

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func statusSession(t *testing.T, f *apiFixture) (StatusSession, Config) {
	t.Helper()
	c := f.cfg()
	c.StatusSync = "two-way"
	s, err := f.client().ForRun(t.Context(), c)
	require.NoError(t, err)
	return s.(StatusSession), c
}

func admitAll() error { return nil }

// Contract: close and reopen each send one request, are verified by a fresh
// read, and a write that already matches Todoist sends nothing.
func TestStatusCloseVerifyAndReopen(t *testing.T) {
	f := newAPIFixture(t)
	s, c := statusSession(t, f)
	target := StatusTarget{ID: f.row.ID}
	before, err := s.ReadStatus(t.Context(), c, target)
	require.NoError(t, err)
	require.Equal(t, "open", before.Status)
	got, err := s.WriteStatus(t.Context(), c, target, "closed", admitAll)
	require.NoError(t, err)
	require.Equal(t, "closed", got.Status)
	require.NotNil(t, got.ClosedAt)
	require.True(t, got.Version.After(before.Version))
	_, err = s.WriteStatus(t.Context(), c, target, "closed", admitAll)
	require.NoError(t, err)
	require.Len(t, f.posts, 1)
	got, err = s.WriteStatus(t.Context(), c, target, "open", admitAll)
	require.NoError(t, err)
	require.Equal(t, "open", got.Status)
	require.Len(t, f.posts, 2)
}

// Contract: Kata refuses writes that Todoist would apply to more than the
// mapped task, and nothing is sent when delivery admission fails.
func TestStatusGuardsBeforeMutation(t *testing.T) {
	for _, kind := range []string{"recurring", "child cascade", "ancestor cascade", "archived section", "admission", "moved", "empty subtask reply", "empty section reply"} {
		t.Run(kind, func(t *testing.T) {
			f := newAPIFixture(t)
			s, c := statusSession(t, f)
			desired := "closed"
			admit := admitAll
			closed := func() { f.row.Checked, f.row.CompletedAt = true, new(f.row.UpdatedAt); desired = "open" }
			switch kind {
			case "recurring":
				f.row.Recurring = true
			case "child cascade":
				f.child = true
			case "ancestor cascade":
				closed()
				f.row.ParentID = "parent123"
			case "archived section":
				closed()
				f.row.SectionID = "section123"
				f.sectionArchived = true
			case "admission":
				admit = func() error { return errors.New("intent changed") }
			case "moved":
				f.row.ProjectID = "foreign"
			case "empty subtask reply":
				f.child = true
				f.empty = map[string]bool{"/api/v1/tasks": true}
			case "empty section reply":
				closed()
				f.row.SectionID = "section123"
				f.empty = map[string]bool{"/api/v1/sections/section123": true}
			}
			_, err := s.WriteStatus(t.Context(), c, StatusTarget{ID: f.row.ID}, desired, admit)
			require.Error(t, err)
			require.Empty(t, f.posts)
		})
	}
}

// Contract: a write whose response is lost is ambiguous, and the retry reads
// Todoist first instead of sending a duplicate.
func TestStatusAmbiguousWriteRecoversWithoutDuplicate(t *testing.T) {
	f := newAPIFixture(t)
	s, c := statusSession(t, f)
	f.lost = true
	_, err := s.WriteStatus(t.Context(), c, StatusTarget{ID: f.row.ID}, "closed", admitAll)
	statusErr, ok := errors.AsType[*issuesync.StatusError](err)
	require.True(t, ok)
	require.True(t, statusErr.Ambiguous)
	require.Len(t, f.posts, 1)
	got, err := s.WriteStatus(t.Context(), c, StatusTarget{ID: f.row.ID}, "closed", admitAll)
	require.NoError(t, err)
	require.Equal(t, "closed", got.Status)
	require.Len(t, f.posts, 1)
}

// Contract: a write that cannot be verified afterwards stays ambiguous.
func TestStatusWrongVerificationKeepsAmbiguousIntent(t *testing.T) {
	f := newAPIFixture(t)
	s, c := statusSession(t, f)
	f.wrong = true
	_, err := s.WriteStatus(t.Context(), c, StatusTarget{ID: f.row.ID}, "closed", admitAll)
	statusErr, ok := errors.AsType[*issuesync.StatusError](err)
	require.True(t, ok)
	require.True(t, statusErr.Ambiguous)
	require.Len(t, f.posts, 1)
}

// Contract: a task last seen closed may have been edited after completion, so
// a reopen searches its history from the floor and still finds the task.
func TestStatusReopenFindsTaskEditedAfterCompletion(t *testing.T) {
	f := newAPIFixture(t)
	s, c := statusSession(t, f)
	completed := f.row.UpdatedAt.Add(time.Hour)
	f.row.Checked, f.row.CompletedAt = true, new(completed)
	f.row.UpdatedAt = completed.Add(time.Hour)
	closed := "closed"
	prior := &db.IssueStatusObservation{Raw: &closed, Version: f.row.UpdatedAt}
	got, err := s.WriteStatus(t.Context(), c, StatusTarget{ID: f.row.ID, Prior: prior}, "open", admitAll)
	require.NoError(t, err)
	require.Equal(t, "open", got.Status)
	require.Len(t, f.posts, 1)
}
