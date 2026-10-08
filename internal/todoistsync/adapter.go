package todoistsync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// Adapter prepares a complete bounded Todoist batch for the shared import runner.
type Adapter struct {
	store   db.Storage
	fetcher Fetcher
}

// NewAdapter connects daemon-owned reads to guarded native imports.
func NewAdapter(store db.Storage, fetcher Fetcher) *Adapter {
	return &Adapter{store: store, fetcher: fetcher}
}

// Provider identifies this collection importer.
func (*Adapter) Provider() string { return "todoist" }

// InitialPhase describes project validation before collection reads.
func (*Adapter) InitialPhase() string { return "project" }

// Prepare completes and validates every read before exposing a batch to storage.
func (a *Adapter) Prepare(ctx context.Context, binding db.IssueSyncBinding, startedAt time.Time) (issuesync.Prepared, error) {
	prepared := issuesync.Prepared{Binding: binding}
	if a.store == nil || a.fetcher == nil || binding.Provider != "todoist" {
		return prepared, fmt.Errorf("todoist adapter requires store, fetcher and Todoist binding")
	}
	c, err := DecodeConfig(binding.Config)
	if err != nil {
		return prepared, err
	}
	if binding.SourceKey != c.SourceKey() || binding.RemoteID != c.RemoteID() {
		return prepared, fmt.Errorf("todoist binding source identity does not match config")
	}
	session, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return prepared, err
	}
	project, err := session.Project(ctx, c)
	if err != nil {
		return prepared, err
	}
	if project.ID != c.ProjectID {
		return prepared, fmt.Errorf("todoist project identity does not match")
	}
	// After a successful run, completions resume from the saved cursor.
	var since time.Time
	if binding.LastCursorAt != nil {
		since = binding.LastCursorAt.Add(-historyOverlap)
	}
	issuesync.ReportProgress(ctx, "tasks", 0, 0)
	tasks, err := session.Tasks(ctx, c, since, startedAt)
	if err != nil {
		return prepared, err
	}
	issuesync.ReportProgress(ctx, "tasks", len(tasks), len(tasks))
	batch, err := BuildImportBatch(binding.SourceKey, c, project, tasks)
	if err != nil {
		return prepared, err
	}
	batch.ProjectID = binding.ProjectID
	issuesync.ReportProgress(ctx, "content", len(tasks), len(tasks))
	name := project.Name
	if strings.TrimSpace(name) == "" {
		name = "Todoist project " + project.ID
	}
	if name != binding.DisplayName {
		binding, err = a.store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{BindingID: binding.ID, DisplayName: name, Config: binding.Config, StartedAt: &startedAt, BindingUpdatedAt: new(binding.UpdatedAt)})
		if err != nil {
			return prepared, err
		}
		prepared.Binding = binding
	}
	prepared.Batch = batch
	return prepared, nil
}

// RunnerConfig supplies the existing durable worker, scheduling and event lifecycle.
type RunnerConfig struct {
	Store          db.Storage
	Fetcher        Fetcher
	Progress       *issuesync.ProgressTracker
	Clock          func() time.Time
	Logger         *slog.Logger
	Interval       time.Duration
	Wake           <-chan struct{}
	EventSink      func(context.Context, int64, []db.Event) error
	EventSinkFrom  func(context.Context, int64, []db.Event, activity.Admission) error
	DrainAdmission activity.WaitableAdmission
}

// NewRunner connects Todoist observations and verified status delivery to the shared worker.
func NewRunner(c RunnerConfig) *issuesync.Runner {
	return issuesync.NewRunner(issuesync.RunnerConfig{Store: c.Store, Adapter: NewAdapter(c.Store, c.Fetcher), Progress: c.Progress, Clock: c.Clock, Logger: c.Logger, Interval: c.Interval, Wake: c.Wake, EventSink: c.EventSink, EventSinkFrom: c.EventSinkFrom, DrainAdmission: c.DrainAdmission, RunTimeout: 20 * time.Minute, StatusTimeout: 5 * time.Minute, StaleLockTTL: 30 * time.Minute, InitialBatchSize: 5000})
}
