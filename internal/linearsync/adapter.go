package linearsync

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

// Adapter supplies scoped Linear observations to the shared sync runner.
type Adapter struct {
	store   db.Storage
	fetcher Fetcher
}

// NewAdapter connects durable storage to a Linear source.
func NewAdapter(store db.Storage, fetcher Fetcher) *Adapter { return &Adapter{store, fetcher} }

// Provider identifies the bindings this adapter handles.
func (*Adapter) Provider() string { return "linear" }

// InitialPhase names scope validation in run progress.
func (*Adapter) InitialPhase() string { return "scope" }
func bindingConfig(b db.IssueSyncBinding) (Config, error) {
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return Config{}, err
	}
	if b.Provider != "linear" || b.SourceKey != c.SourceKey() || b.RemoteID != c.RemoteID() {
		return Config{}, fmt.Errorf("linear binding source identity does not match config")
	}
	return c, nil
}
func verifyScope(ctx context.Context, s Session, c Config) (Scope, error) {
	scope, err := s.Scope(ctx, c)
	if err != nil {
		return Scope{}, err
	}
	if !matchesScope(c, scope) {
		return Scope{}, fmt.Errorf("linear scope does not match binding")
	}
	return scope, nil
}

// Prepare validates source scope and builds a bounded native import batch.
func (a *Adapter) Prepare(ctx context.Context, b db.IssueSyncBinding, startedAt time.Time) (issuesync.Prepared, error) {
	prepared := issuesync.Prepared{Binding: b}
	if a.store == nil || a.fetcher == nil {
		return prepared, fmt.Errorf("linear adapter requires store and fetcher")
	}
	c, err := bindingConfig(b)
	if err != nil {
		return prepared, err
	}
	s, err := a.fetcher.ForRun(ctx, c)
	if err != nil {
		return prepared, err
	}
	scope, err := verifyScope(ctx, s, c)
	if err != nil {
		return prepared, err
	}
	issuesync.ReportProgress(ctx, "states", 0, 0)
	states, err := s.States(ctx, c)
	if err != nil {
		return prepared, err
	}
	issuesync.ReportProgress(ctx, "issues", 0, 0)
	items, err := s.Issues(ctx, c)
	if err != nil {
		return prepared, err
	}
	issuesync.ReportProgress(ctx, "issues", len(items), len(items))
	cutoff, err := ParseSince(c.Since)
	if err != nil {
		return prepared, err
	}
	eligible := make([]Issue, 0, len(items))
	for _, i := range items {
		if cutoff == nil || i.UpdatedAt.After(*cutoff) {
			eligible = append(eligible, i)
		}
	}
	batch, err := BuildImportBatch(b.SourceKey, c, scope, states, eligible)
	if err != nil {
		return prepared, err
	}
	batch.ProjectID = b.ProjectID
	name := strings.TrimSpace(scope.Name)
	if name == "" {
		name = "Linear team " + c.TeamID
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

// RunnerConfig supplies storage, source reads, and scheduling for Linear sync.
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

// NewRunner reuses durable claims, status intents, and scheduling from issuesync.
func NewRunner(c RunnerConfig) *issuesync.Runner {
	return issuesync.NewRunner(issuesync.RunnerConfig{Store: c.Store, Adapter: NewAdapter(c.Store, c.Fetcher), Progress: c.Progress, Clock: c.Clock, Logger: c.Logger, Interval: c.Interval, Wake: c.Wake, EventSink: c.EventSink, EventSinkFrom: c.EventSinkFrom, DrainAdmission: c.DrainAdmission, RunTimeout: 20 * time.Minute, StaleLockTTL: 30 * time.Minute, InitialBatchSize: 5000})
}
