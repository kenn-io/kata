package notionsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

type statusAdapterSession struct {
	*adapterSession
	writes  int
	page    string
	desired string
	readErr error
}

func (s *statusAdapterSession) ForRun(ctx context.Context) (Session, error) { return s, ctx.Err() }
func (s *statusAdapterSession) ReadStatus(_ context.Context, _ Config, pageID string) (issuesync.StatusObservation, error) {
	s.page = pageID
	if s.readErr != nil {
		return issuesync.StatusObservation{}, s.readErr
	}
	return issuesync.StatusObservation{RawStatus: new("complete-a"), Status: "closed", Version: time.Now().UTC()}, nil
}
func (s *statusAdapterSession) WriteStatus(ctx context.Context, c Config, pageID, desired string, admit func() error) (issuesync.StatusObservation, error) {
	if err := admit(); err != nil {
		return issuesync.StatusObservation{}, err
	}
	s.writes++
	s.desired = desired
	return s.ReadStatus(ctx, c, pageID)
}
func TestNotionStatusDeliveryUsesExistingPageIdentityBeforeContentFailure(t *testing.T) {
	store := adapterStore(t)
	c, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	b := adapterBinding(t, store, c)
	p := pageFixture()
	at := time.Now().UTC().Truncate(time.Millisecond)
	_, _, err = store.ImportBatch(t.Context(), db.ImportBatchParams{ProjectID: b.ProjectID, Source: b.SourceKey, Actor: "notion-sync", Items: []db.ImportItem{{ExternalID: "page:" + p.Page.ID, Title: "Example task", Author: "worker", Status: "open", CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Minute)}}})
	require.NoError(t, err)
	issue := importedIssue(t, store, b, p.Page.ID)
	_, _, _, err = store.CloseIssue(t.Context(), issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	session := &statusAdapterSession{adapterSession: newAdapterSession(p)}
	session.source = groupSchema()
	session.onPages = func(context.Context, *time.Time) error { return errors.New("content list unavailable") }
	_, err = adapterRunner(store, session, &at).RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "content list unavailable")
	require.Equal(t, 1, session.writes)
	require.Equal(t, p.Page.ID, session.page)
	require.Equal(t, "closed", session.desired)
	for record, err := range store.ExportImportMappings(t.Context(), db.ExportFilter{}) {
		require.NoError(t, err)
		if record.ID > 0 && record.ExternalID == "page:"+p.Page.ID {
			require.Nil(t, record.PendingEventUID)
			require.Nil(t, record.RemoteLocator)
			require.NotNil(t, record.ObservedStatusAt)
		}
	}
}

func TestBlockedNotionPageStatusStillImportsContentAndAdvancesCursor(t *testing.T) {
	store := adapterStore(t)
	config, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	binding := adapterBinding(t, store, config)
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	oldPageID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	_, _, err = store.ImportBatch(t.Context(), db.ImportBatchParams{ProjectID: binding.ProjectID, Source: binding.SourceKey, Actor: "notion-sync", Items: []db.ImportItem{{ExternalID: "page:" + oldPageID, Title: "Archived task", Author: "worker", Status: "open", CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Minute)}}})
	require.NoError(t, err)

	page := pageFixture()
	page.Page.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	page.Page.URL = "https://www.notion.so/" + page.Page.ID
	session := &statusAdapterSession{
		adapterSession: newAdapterSession(page),
		readErr:        blockedStatusPageError(errors.New("notion status page is unavailable or outside the selected data source")),
	}
	session.source = groupSchema()
	result, err := adapterRunner(store, session, &at).RunOnce(t.Context(), binding.ID)
	require.Error(t, err)
	require.True(t, issuesync.IsBlockedStatusWarning(err))
	require.Equal(t, 1, result.Import.Created)
	current, err := store.IssueSyncBindingByID(t.Context(), binding.ID)
	require.NoError(t, err)
	require.NotNil(t, current.LastCursorAt)
	require.Equal(t, at, *current.LastCursorAt)
}
