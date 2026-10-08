package twentysync

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

// Adapter connects authenticated workspace observations to guarded imports.
type Adapter struct {
	store   db.Storage
	fetcher Fetcher
}

// NewAdapter uses the shared store and daemon-owned client for workspace imports.
func NewAdapter(store db.Storage, fetcher Fetcher) *Adapter {
	return &Adapter{store: store, fetcher: fetcher}
}

// Provider identifies Twenty bindings in the shared runner.
func (*Adapter) Provider() string { return "twenty" }

// InitialPhase names the first authenticated read in each run.
func (*Adapter) InitialPhase() string { return "workspace" }

func bindingConfig(b db.IssueSyncBinding) (Config, error) {
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return Config{}, err
	}
	if b.Provider != "twenty" || b.SourceKey != c.SourceKey() || b.RemoteID != c.RemoteID() {
		return Config{}, fmt.Errorf("twenty binding source identity does not match config")
	}
	return c, nil
}

func verifyWorkspace(ctx context.Context, session Session, c Config) (Workspace, error) {
	workspace, err := session.Workspace(ctx, c)
	if err != nil {
		return Workspace{}, err
	}
	if workspace.ID != c.WorkspaceID {
		return Workspace{}, fmt.Errorf("twenty API key workspace does not match binding")
	}
	return workspace, nil
}

// Prepare validates the complete collection before committing an import.
func (a *Adapter) Prepare(ctx context.Context, b db.IssueSyncBinding, startedAt time.Time) (issuesync.Prepared, error) {
	prepared := issuesync.Prepared{Binding: b}
	if a.store == nil || a.fetcher == nil {
		return prepared, fmt.Errorf("twenty adapter requires store and fetcher")
	}
	c, err := bindingConfig(b)
	if err != nil {
		return prepared, err
	}
	session, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return prepared, err
	}
	workspace, err := verifyWorkspace(ctx, session, c)
	if err != nil {
		return prepared, err
	}
	issuesync.ReportProgress(ctx, "schema", 0, 0)
	schema, err := session.Schema(ctx, c)
	if err != nil {
		return prepared, err
	}
	issuesync.ReportProgress(ctx, "tasks", 0, 0)
	tasks, err := session.Tasks(ctx, c)
	if err != nil {
		return prepared, err
	}
	issuesync.ReportProgress(ctx, "tasks", len(tasks), len(tasks))
	issuesync.ReportProgress(ctx, "content", 0, len(tasks))
	batch, err := BuildImportBatch(b.SourceKey, c, schema, tasks)
	if err != nil {
		return prepared, err
	}
	batch.ProjectID = b.ProjectID
	issuesync.ReportProgress(ctx, "content", len(tasks), len(tasks))
	name := workspace.DisplayName
	if strings.TrimSpace(name) == "" {
		name = "Twenty workspace " + workspace.ID
	}
	if name != b.DisplayName {
		b, err = a.store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{BindingID: b.ID, DisplayName: name, Config: b.Config, StartedAt: &startedAt, BindingUpdatedAt: new(b.UpdatedAt)})
		if err != nil {
			return prepared, err
		}
		prepared.Binding = b
	}
	prepared.Batch = batch
	return prepared, nil
}

// RunnerConfig supplies the existing shared claim, progress and event machinery.
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

// NewRunner supplies Twenty observations to the provider-neutral sync engine.
func NewRunner(c RunnerConfig) *issuesync.Runner {
	return issuesync.NewRunner(issuesync.RunnerConfig{Store: c.Store, Adapter: NewAdapter(c.Store, c.Fetcher), Progress: c.Progress, Clock: c.Clock, Logger: c.Logger, Interval: c.Interval, Wake: c.Wake, EventSink: c.EventSink, EventSinkFrom: c.EventSinkFrom, DrainAdmission: c.DrainAdmission, RunTimeout: 20 * time.Minute, StaleLockTTL: 30 * time.Minute, InitialBatchSize: 5000})
}
