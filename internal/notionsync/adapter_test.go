package notionsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/issuesync"
)

type adapterSession struct {
	source    DataSource
	database  *Database
	pages     []Page
	content   map[string]PageContent
	onSource  func(context.Context)
	onPages   func(context.Context, *time.Time) error
	onContent func(context.Context, Page) error
}

func (s *adapterSession) ForRun(ctx context.Context) (Session, error) { return s, ctx.Err() }
func (s *adapterSession) Database(ctx context.Context, id string) (Database, error) {
	if s.database != nil {
		return *s.database, ctx.Err()
	}
	return Database{ID: id, DataSources: []Option{{ID: s.source.ID, Name: s.source.Name}}}, ctx.Err()
}
func (s *adapterSession) DataSource(ctx context.Context, _ string) (DataSource, error) {
	if s.onSource != nil {
		s.onSource(ctx)
	}
	return s.source, ctx.Err()
}
func (s *adapterSession) Pages(ctx context.Context, _ Config, since *time.Time) ([]Page, error) {
	if s.onPages != nil {
		if err := s.onPages(ctx, since); err != nil {
			return nil, err
		}
	}
	return s.pages, ctx.Err()
}
func (s *adapterSession) Content(ctx context.Context, _ Config, p Page) (PageContent, error) {
	if s.onContent != nil {
		if err := s.onContent(ctx, p); err != nil {
			return PageContent{}, err
		}
	}
	return s.content[p.ID], ctx.Err()
}
func newAdapterSession(pages ...PageContent) *adapterSession {
	s := &adapterSession{source: schema(), content: map[string]PageContent{}}
	for _, p := range pages {
		s.pages = append(s.pages, p.Page)
		s.content[p.Page.ID] = p
	}
	return s
}
func adapterStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	s, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}
func adapterBinding(t *testing.T, s db.Storage, c Config) db.IssueSyncBinding {
	t.Helper()
	p, err := s.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	b, err := s.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "notion", SourceKey: "notion:" + sourceID, RemoteID: sourceID, DisplayName: "Example tasks", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
}
func importedIssue(t *testing.T, s db.Storage, b db.IssueSyncBinding, id string) db.Issue {
	t.Helper()
	m, err := s.ImportMappingBySource(context.Background(), b.ProjectID, b.SourceKey, "issue", "page:"+id)
	require.NoError(t, err)
	require.NotNil(t, m.IssueID)
	i, err := s.IssueByID(context.Background(), *m.IssueID)
	require.NoError(t, err)
	return i
}
func adapterRunner(s db.Storage, f Fetcher, at *time.Time) *issuesync.Runner {
	return NewRunner(RunnerConfig{Store: s, Fetcher: f, Clock: func() time.Time { return *at }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}
func seedCursor(t *testing.T, s db.Storage, b db.IssueSyncBinding, at time.Time) {
	t.Helper()
	_, ok, err := s.ClaimIssueSyncBinding(context.Background(), b.ID, b.Provider, at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.RecordIssueSyncSuccess(context.Background(), db.IssueSyncSuccessParams{BindingID: b.ID, StartedAt: at, At: at, CursorAt: at})
	require.NoError(t, err)
}

func TestNotionRunUsesLiveCompleteGroup(t *testing.T) {
	store := adapterStore(t)
	c, err := ResolveConfig(groupSchema(), Selectors{}, "")
	require.NoError(t, err)
	binding := adapterBinding(t, store, c)
	p := pageFixture()
	p.Page.StatusID = new("complete-a")
	session := newAdapterSession(p)
	session.source = groupSchema()
	at := p.Page.UpdatedAt.Add(time.Hour)
	_, err = adapterRunner(store, session, &at).RunOnce(t.Context(), binding.ID)
	require.NoError(t, err)
	issue := importedIssue(t, store, binding, p.Page.ID)
	require.Equal(t, "closed", issue.Status)
	require.Equal(t, "done", *issue.ClosedReason)
}

// Missing source-version guards would overwrite local edits or reopen an older replay.
func TestNotionRunImportsAndReplays(t *testing.T) {
	s := adapterStore(t)
	b := adapterBinding(t, s, configFixture(t))
	p := pageFixture()
	f := newAdapterSession(p)
	at := p.Page.UpdatedAt.Add(time.Hour)
	r := adapterRunner(s, f, &at)
	first, err := r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, first.Import.Created)
	require.Equal(t, 0, first.Status.LastComments)
	i := importedIssue(t, s, b, p.Page.ID)
	require.Equal(t, "[Notion] Example task", i.Title)
	localTitle := "Local task"
	priority := int64(2)
	_, err = s.EditIssueAtomic(context.Background(), db.EditIssueAtomicParams{IssueID: i.ID, Actor: "editor", Title: &localTitle, SetPriority: &priority})
	require.NoError(t, err)
	at = at.Add(time.Hour)
	replay, err := r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, replay.Import.Unchanged)
	i = importedIssue(t, s, b, p.Page.ID)
	require.Equal(t, localTitle, i.Title)
	require.Equal(t, &priority, i.Priority)
	p.Page.UpdatedAt = i.UpdatedAt.Add(time.Minute)
	p.Page.StatusID = new("complete-b")
	f.pages = []Page{p.Page}
	f.content[p.Page.ID] = p
	_, err = r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	i = importedIssue(t, s, b, p.Page.ID)
	require.Equal(t, "closed", i.Status)
	require.Nil(t, i.Priority)
	p.Page.UpdatedAt = p.Page.UpdatedAt.Add(time.Minute)
	p.Page.StatusID = new("active")
	f.pages = []Page{p.Page}
	f.content[p.Page.ID] = p
	_, err = r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	i = importedIssue(t, s, b, p.Page.ID)
	require.Equal(t, "open", i.Status)
	require.Nil(t, i.ClosedAt)
	require.Nil(t, i.ClosedReason)
	// Local closure survives an older replay; Session exposes no upstream mutation.
	i, _, _, err = s.CloseIssue(context.Background(), i.ID, "done", "editor", "", nil)
	require.NoError(t, err)
	p.Page.UpdatedAt = i.UpdatedAt.Add(-time.Second)
	f.pages = []Page{p.Page}
	f.content[p.Page.ID] = p
	_, err = r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", importedIssue(t, s, b, p.Page.ID).Status)
	f.pages = nil
	_, err = r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", importedIssue(t, s, b, p.Page.ID).Status)
}

type adapterFaultStore struct {
	db.Storage
	calls         int
	beforeImport  func(db.ImportBatchParams) error
	beforeSuccess func() error
}

func (s *adapterFaultStore) ImportBatch(ctx context.Context, p db.ImportBatchParams) (db.ImportBatchResult, []db.Event, error) {
	s.calls++
	if s.beforeImport != nil {
		if err := s.beforeImport(p); err != nil {
			return db.ImportBatchResult{}, nil, err
		}
	}
	return s.Storage.ImportBatch(ctx, p)
}
func (s *adapterFaultStore) RecordIssueSyncSuccess(ctx context.Context, p db.IssueSyncSuccessParams) (db.IssueSyncStatus, error) {
	if s.beforeSuccess != nil {
		if err := s.beforeSuccess(); err != nil {
			return db.IssueSyncStatus{}, err
		}
	}
	return s.Storage.RecordIssueSyncSuccess(ctx, p)
}
func manyPages(n int, body string) []PageContent {
	pages := make([]PageContent, n)
	for i := range pages {
		p := pageFixture()
		p.Page.ID = fmt.Sprintf("%08x-2222-4222-8222-222222222222", i)
		p.Markdown = body
		pages[i] = p
	}
	return pages
}

// A cursor based on fetch completion or missing preflight permits lost edits or partial fetch writes.
func TestNotionCursorAndFailure(t *testing.T) {
	t.Run("initial empty overlap", func(t *testing.T) {
		s := adapterStore(t)
		b := adapterBinding(t, s, configFixture(t))
		f := newAdapterSession()
		at := time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)
		started := at
		f.onPages = func(_ context.Context, since *time.Time) error {
			require.Nil(t, since)
			at = at.Add(time.Minute)
			return nil
		}
		r := adapterRunner(s, f, &at)
		res, err := r.RunOnce(context.Background(), b.ID)
		require.NoError(t, err)
		require.Equal(t, started, *res.Binding.LastCursorAt)
		f.onPages = func(_ context.Context, since *time.Time) error {
			require.Equal(t, started.Add(-2*time.Minute), *since)
			return nil
		}
		_, err = r.RunOnce(context.Background(), b.ID)
		require.NoError(t, err)
	})
	t.Run("explicit exclusive cutoff before content", func(t *testing.T) {
		s := adapterStore(t)
		c := configFixture(t)
		c.Since = "2026-09-28T00:00:00Z"
		b := adapterBinding(t, s, c)
		p := pageFixture()
		next := p
		next.Page.ID = "33333333-3333-4333-8333-333333333333"
		next.Page.UpdatedAt = next.Page.UpdatedAt.Add(time.Second)
		f := newAdapterSession(p, next)
		at := p.Page.UpdatedAt.Add(time.Hour)
		seedCursor(t, s, b, p.Page.UpdatedAt.Add(time.Minute))
		f.onPages = func(_ context.Context, since *time.Time) error {
			require.Equal(t, p.Page.UpdatedAt, *since)
			return nil
		}
		f.onContent = func(_ context.Context, page Page) error { require.Equal(t, next.Page.ID, page.ID); return nil }
		res, err := adapterRunner(s, f, &at).RunOnce(context.Background(), b.ID)
		require.NoError(t, err)
		require.Equal(t, 1, res.Import.Created)
	})
	for _, mode := range []string{"detail", "batch overflow", "second chunk", "success record", "cancellation", "deadline", "claim theft"} {
		t.Run(mode, func(t *testing.T) {
			s := adapterStore(t)
			b := adapterBinding(t, s, configFixture(t))
			at := pageFixture().Page.UpdatedAt.Add(time.Hour)
			previous := at.Add(-time.Hour)
			seedCursor(t, s, b, previous)
			wrapped := &adapterFaultStore{Storage: s}
			f := newAdapterSession(pageFixture())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callerDeadline time.Time
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond) //nolint:kennlint // the deadline is the expected result; onContent waits on ctx.Done until it fires
				callerDeadline, _ = ctx.Deadline()
				defer cancel()
			}
			completed := 0
			f.onSource = func(ctx context.Context) {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				if mode == "deadline" {
					require.True(t, callerDeadline.Equal(deadline), "runner must preserve the caller deadline")
				} else {
					require.InDelta(t, (25 * time.Minute).Seconds(), time.Until(deadline).Seconds(), 1)
				}
			}
			f.onContent = func(ctx context.Context, _ Page) error {
				require.Zero(t, wrapped.calls, "all preparation precedes issue writes")
				completed++
				switch mode {
				case "detail":
					if completed == 2 {
						return errors.New("detail unavailable")
					}
				case "deadline":
					<-ctx.Done()
					return ctx.Err()
				case "cancellation":
					cancel()
					return ctx.Err()
				case "claim theft":
					successor := at.Add(31 * time.Minute)
					_, ok, err := s.ClaimIssueSyncBinding(ctx, b.ID, b.Provider, successor, at.Add(time.Minute))
					require.NoError(t, err)
					require.True(t, ok)
				}
				return nil
			}
			if mode == "detail" {
				pages := manyPages(2, "")
				f.pages = []Page{pages[0].Page, pages[1].Page}
				f.content = map[string]PageContent{pages[0].Page.ID: pages[0], pages[1].Page.ID: pages[1]}
			}
			if mode == "batch overflow" {
				f = newAdapterSession(manyPages(70, strings.Repeat("x", 1<<20))...)
				f.onContent = func(context.Context, Page) error { require.Zero(t, wrapped.calls); completed++; return nil }
			}
			switch mode {
			case "second chunk":
				f = newAdapterSession(manyPages(5001, "")...)
				f.onContent = func(context.Context, Page) error { require.Zero(t, wrapped.calls); completed++; return nil }
				wrapped.beforeImport = func(p db.ImportBatchParams) error {
					require.Equal(t, 5001, completed)
					if wrapped.calls == 2 {
						return errors.New("second chunk unavailable")
					}
					require.Len(t, p.Items, 5000)
					return nil
				}
			}
			if mode == "success record" {
				wrapped.beforeSuccess = func() error { return errors.New("success record unavailable") }
			}
			res, err := adapterRunner(wrapped, f, &at).RunOnce(ctx, b.ID)
			require.Error(t, err)
			if mode == "detail" {
				require.ErrorContains(t, err, f.pages[1].ID)
			}
			if mode == "deadline" {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
			after, readErr := s.IssueSyncBindingByID(context.Background(), b.ID)
			require.NoError(t, readErr)
			require.Equal(t, previous, *after.LastCursorAt)
			switch mode {
			case "second chunk":
				require.Equal(t, 5000, res.Import.Created)
			case "success record":
				require.Equal(t, 1, res.Import.Created)
			case "claim theft":
				require.Equal(t, 1, wrapped.calls)
				require.Zero(t, res.Import.Created)
				_, lookupErr := s.ImportMappingBySource(context.Background(), b.ProjectID, b.SourceKey, "issue", "page:"+pageFixture().Page.ID)
				require.ErrorIs(t, lookupErr, db.ErrNotFound)
			default:
				require.Zero(t, wrapped.calls)
			}
			if mode == "batch overflow" {
				require.Contains(t, err.Error(), "64 MiB")
				require.Equal(t, 64, completed, "serialized item metadata makes 64 one-MiB bodies exceed the aggregate bound")
			}
			if mode == "claim theft" {
				status, readErr := s.IssueSyncStatusByProject(context.Background(), b.ProjectID)
				require.NoError(t, readErr)
				require.Equal(t, at.Add(31*time.Minute), *status.SyncStartedAt)
			}
		})
	}
}

// Schema drift must stop the run rather than silently map a removed status as open.
func TestNotionSchemaDrift(t *testing.T) {
	for _, mode := range []string{"rename", "empty name", "property removed", "option removed", "unknown status", "null status", "new open option"} {
		t.Run(mode, func(t *testing.T) {
			s := adapterStore(t)
			b := adapterBinding(t, s, configFixture(t))
			p := pageFixture()
			f := newAdapterSession(p)
			switch mode {
			case "rename":
				f.source.Name = "Renamed tasks"
				f.source.DatabaseID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
				f.source.Properties[1].Name = "Renamed workflow"
				f.source.Properties[1].Options[0].Name = "Renamed done"
			case "empty name":
				f.source.Name = ""
			case "property removed":
				f.source.Properties = f.source.Properties[:2]
			case "option removed":
				f.source.Properties[1].Options = f.source.Properties[1].Options[1:]
			case "unknown status":
				p.Page.StatusID = new("missing")
			case "new open option":
				p.Page.StatusID = new("new")
				f.source.Properties[1].Options = append(f.source.Properties[1].Options, Option{ID: "new", Name: "New"})
			}
			f.pages = []Page{p.Page}
			f.content[p.Page.ID] = p
			at := p.Page.UpdatedAt.Add(time.Hour)
			res, err := adapterRunner(s, f, &at).RunOnce(context.Background(), b.ID)
			if mode == "property removed" || mode == "option removed" || mode == "unknown status" {
				require.Error(t, err)
				require.Zero(t, res.Import.Created)
				if mode == "unknown status" {
					require.ErrorContains(t, err, p.Page.ID)
				}
				require.Nil(t, res.Binding.LastCursorAt)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, res.Import.Created)
			switch mode {
			case "rename":
				require.Equal(t, "Renamed tasks", res.Binding.DisplayName)
				c, err := DecodeConfig(res.Binding.Config)
				require.NoError(t, err)
				require.Equal(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", c.DatabaseID)
			case "empty name":
				require.Equal(t, "Notion data source "+sourceID, res.Binding.DisplayName)
			}
		})
	}
}

func TestNotionRunSkipsUnavailablePages(t *testing.T) {
	s := adapterStore(t)
	b := adapterBinding(t, s, configFixture(t))
	pages := manyPages(4, "")
	pages[0].Page.IsArchived = true
	pages[1].Page.InTrash = true
	pages[2].Page.DataSourceID = databaseID
	f := newAdapterSession(pages...)
	f.onContent = func(_ context.Context, p Page) error { require.Equal(t, pages[3].Page.ID, p.ID); return nil }
	at := pageFixture().Page.UpdatedAt.Add(time.Hour)
	res, err := adapterRunner(s, f, &at).RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, res.Import.Created)
}

func TestNotionProgressPhases(t *testing.T) {
	s := adapterStore(t)
	b := adapterBinding(t, s, configFixture(t))
	f := newAdapterSession(pageFixture())
	tracker := issuesync.NewProgressTracker()
	at := pageFixture().Page.UpdatedAt.Add(time.Hour)
	assertPhase := func(phase string, completed, total int) {
		p := tracker.Snapshot(b.ID, at)
		require.NotNil(t, p)
		require.Equal(t, phase, p.Phase)
		require.Equal(t, completed, p.Completed)
		require.Equal(t, total, p.Total)
	}
	f.onSource = func(context.Context) { assertPhase("source", 0, 0) }
	f.onPages = func(context.Context, *time.Time) error { assertPhase("pages", 0, 0); return nil }
	f.onContent = func(context.Context, Page) error { assertPhase("content", 0, 1); return nil }
	wrapped := &adapterFaultStore{Storage: s, beforeImport: func(db.ImportBatchParams) error { assertPhase("importing", 0, 1); return nil }, beforeSuccess: func() error { assertPhase("finalizing", 0, 0); return nil }}
	r := NewRunner(RunnerConfig{Store: wrapped, Fetcher: f, Clock: func() time.Time { return at }, Progress: tracker})
	_, err := r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Nil(t, tracker.Snapshot(b.ID, at))
}

func TestNotionEventsAfterCommit(t *testing.T) {
	s := adapterStore(t)
	b := adapterBinding(t, s, configFixture(t))
	f := newAdapterSession(manyPages(5001, "")...)
	wrapped := &adapterFaultStore{Storage: s}
	at := pageFixture().Page.UpdatedAt.Add(time.Hour)
	deliveries := 0
	wrapped.beforeImport = func(db.ImportBatchParams) error {
		if wrapped.calls == 2 {
			require.Equal(t, 1, deliveries)
		}
		return nil
	}
	r := NewRunner(RunnerConfig{Store: wrapped, Fetcher: f, Clock: func() time.Time { return at }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), EventSink: func(ctx context.Context, project int64, events []db.Event) error {
		require.Equal(t, b.ProjectID, project)
		require.NotEmpty(t, events)
		for _, e := range events {
			require.Positive(t, e.ID)
			if e.IssueID != nil {
				_, err := s.IssueByID(ctx, *e.IssueID)
				require.NoError(t, err)
			}
		}
		deliveries++
		return errors.New("sink unavailable")
	}})
	res, err := r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 5001, res.Import.Created)
	require.Equal(t, 2, deliveries)
	require.Equal(t, at, *res.Binding.LastCursorAt)
}

// Page progress counts accepted query rows, even when the exclusive cutoff omits content reads.
func TestNotionProgressPagesBeforeCutoff(t *testing.T) {
	s := adapterStore(t)
	c := configFixture(t)
	c.Since = "2026-09-28T00:00:00Z"
	b := adapterBinding(t, s, c)
	p := pageFixture()
	next := p
	next.Page.ID = "33333333-3333-4333-8333-333333333333"
	next.Page.UpdatedAt = next.Page.UpdatedAt.Add(time.Second)
	f := newAdapterSession(p, next)
	at := p.Page.UpdatedAt.Add(time.Hour)
	_, ok, err := s.ClaimIssueSyncBinding(context.Background(), b.ID, "notion", at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	pagesCompleted := -1
	ctx := issuesync.WithProgressReporter(context.Background(), func(phase string, completed, total int) {
		if phase == "pages" {
			pagesCompleted = completed
			require.Zero(t, total)
		}
	})
	_, err = NewAdapter(s, f).Prepare(ctx, b, at)
	require.NoError(t, err)
	require.Equal(t, 2, pagesCompleted)
}

func TestNotionRunValidatesBindingBeforeReads(t *testing.T) {
	for _, mode := range []string{"provider", "config", "source key", "remote id", "fetcher", "store"} {
		t.Run(mode, func(t *testing.T) {
			s := adapterStore(t)
			b := adapterBinding(t, s, configFixture(t))
			f := newAdapterSession()
			f.onSource = func(context.Context) { t.Fatal("invalid binding reached upstream") }
			a := NewAdapter(s, f)
			switch mode {
			case "provider":
				b.Provider = "github"
			case "config":
				b.Config = []byte(`{"token":"secret-marker"}`)
			case "source key":
				b.SourceKey = "notion:other"
			case "remote id":
				b.RemoteID = databaseID
			case "fetcher":
				a = NewAdapter(s, nil)
			case "store":
				a = NewAdapter(nil, f)
			}
			_, err := a.Prepare(context.Background(), b, time.Now())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-marker")
		})
	}
}

func TestNotionRunValidatesCompleteBatch(t *testing.T) {
	s := adapterStore(t)
	b := adapterBinding(t, s, configFixture(t))
	f := newAdapterSession(pageFixture(), pageFixture())
	at := pageFixture().Page.UpdatedAt.Add(time.Hour)
	res, err := adapterRunner(s, f, &at).RunOnce(context.Background(), b.ID)
	require.Error(t, err)
	require.Zero(t, res.Import.Created)
	require.Nil(t, res.Binding.LastCursorAt)
}

func TestNotionSchemaSourceAndParentIdentity(t *testing.T) {
	for _, mode := range []string{"source mismatch", "parent mismatch", "parent membership", "content status"} {
		t.Run(mode, func(t *testing.T) {
			s := adapterStore(t)
			b := adapterBinding(t, s, configFixture(t))
			p := pageFixture()
			f := newAdapterSession(p)
			switch mode {
			case "source mismatch":
				f.source.ID = databaseID
			case "parent mismatch":
				f.database = &Database{ID: sourceID, DataSources: []Option{{ID: sourceID}}}
			case "parent membership":
				f.database = &Database{ID: databaseID}
			case "content status":
				p.Page.StatusID = new("missing")
				f.content[p.Page.ID] = p
			}
			at := p.Page.UpdatedAt.Add(time.Hour)
			wrapped := &adapterFaultStore{Storage: s}
			res, err := adapterRunner(wrapped, f, &at).RunOnce(context.Background(), b.ID)
			require.Error(t, err)
			require.Zero(t, wrapped.calls)
			if mode == "content status" {
				require.ErrorContains(t, err, p.Page.ID)
			}
			require.Zero(t, res.Import.Created)
			require.Nil(t, res.Binding.LastCursorAt)
		})
	}
}

func TestNotionRunStaleClaimHorizon(t *testing.T) {
	s := adapterStore(t)
	b := adapterBinding(t, s, configFixture(t))
	started := pageFixture().Page.UpdatedAt.Add(time.Hour)
	_, ok, err := s.ClaimIssueSyncBinding(context.Background(), b.ID, "notion", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	at := started.Add(29 * time.Minute)
	f := newAdapterSession(pageFixture())
	r := adapterRunner(s, f, &at)
	_, err = r.RunOnce(context.Background(), b.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	at = started.Add(31 * time.Minute)
	res, err := r.RunOnce(context.Background(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, res.Import.Created)
	require.Equal(t, at, *res.Binding.LastCursorAt)
}

func TestNotionRunTitlePrefixPresentation(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	config := configFixture(t)
	binding := adapterBinding(t, store, config)
	page := pageFixture()
	page.Page.CreatedAt = time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	page.Page.UpdatedAt = page.Page.CreatedAt.Add(time.Hour)
	fetcher := newAdapterSession(page)
	at := page.Page.UpdatedAt.Add(time.Hour)
	runner := adapterRunner(store, fetcher, &at)
	_, err := runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	issue := importedIssue(t, store, binding, page.Page.ID)
	_, err = store.AddLabel(ctx, issue.ID, "local", "editor")
	require.NoError(t, err)
	for _, prefix := range []bool{false, true, false} {
		config.TitlePrefix = new(prefix)
		raw, err := EncodeConfig(config)
		require.NoError(t, err)
		saved, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: binding.ProjectID, Provider: binding.Provider, SourceKey: binding.SourceKey, RemoteID: binding.RemoteID, DisplayName: binding.DisplayName, Config: raw, IntervalSeconds: 300})
		require.NoError(t, err)
		require.Nil(t, saved.LastCursorAt)
		fetcher.onPages = func(_ context.Context, since *time.Time) error { require.Nil(t, since); return nil }
		fetcher.source.Name = "Renamed tasks"
		fetcher.source.DatabaseID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		at = at.Add(time.Hour)
		_, err = runner.RunOnce(ctx, binding.ID)
		require.NoError(t, err)
		issue = importedIssue(t, store, binding, page.Page.ID)
		wantTitle := "Example task"
		wantLabels := []string{"local", "notion"}
		if prefix {
			wantTitle = "[Notion] Example task"
			wantLabels = []string{"local"}
		}
		require.Equal(t, wantTitle, issue.Title)
		labels, err := store.LabelsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		names := []string{}
		for _, label := range labels {
			names = append(names, label.Label)
		}
		require.ElementsMatch(t, wantLabels, names)
		refreshed, err := store.IssueSyncBindingByID(ctx, binding.ID)
		require.NoError(t, err)
		decoded, err := DecodeConfig(refreshed.Config)
		require.NoError(t, err)
		require.Equal(t, prefix, decoded.UseTitlePrefix())
		require.Equal(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", decoded.DatabaseID)
	}
	local := "Local task"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Actor: "editor", Title: &local})
	require.NoError(t, err)
	config.TitlePrefix = new(true)
	raw, err := EncodeConfig(config)
	require.NoError(t, err)
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: binding.ProjectID, Provider: binding.Provider, SourceKey: binding.SourceKey, RemoteID: binding.RemoteID, DisplayName: binding.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, local, importedIssue(t, store, binding, page.Page.ID).Title)
	labels, err := store.LabelsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	require.Equal(t, "local", labels[0].Label)
}

func TestNotionMetadataRefreshIgnoresPrivateScanProgress(t *testing.T) {
	s := adapterStore(t)
	c := configFixture(t)
	b := adapterBinding(t, s, c)
	f := newAdapterSession()
	at := pageFixture().Page.UpdatedAt.Add(time.Hour)
	_, claimed, err := s.ClaimIssueSyncBinding(t.Context(), b.ID, "notion", at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	// First prepare normalizes the live display name and parent identity.
	prepared, err := NewAdapter(s, f).Prepare(t.Context(), b, at)
	require.NoError(t, err)
	b = prepared.Binding
	guard := db.IssueSyncImportGuard{BindingID: b.ID, Provider: b.Provider, StartedAt: at, BindingUpdatedAt: new(b.UpdatedAt)}
	b, err = s.UpdateIssueStatusScan(t.Context(), guard, db.IssueStatusScanState{Sweep: db.IssueStatusScanCursor{After: 1, Through: 2}})
	require.NoError(t, err)
	prepared, err = NewAdapter(s, f).Prepare(t.Context(), b, at)
	require.NoError(t, err)
	require.Equal(t, b.UpdatedAt, prepared.Binding.UpdatedAt, "scan progress alone must not refresh provider metadata")
}
