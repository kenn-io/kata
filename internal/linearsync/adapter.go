package linearsync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// Adapter supplies scoped Linear observations to the shared sync runner.
type Adapter struct {
	store   db.Storage
	fetcher Fetcher
	mu      sync.Mutex
	// observed holds each two-way binding's latest content read. The next
	// status sweep uses it instead of one Linear request per mapped issue.
	observed map[int64]contentObservations
}

type contentObservations struct {
	sourceKey string
	byID      map[string]issuesync.StatusObservation
}

// NewAdapter connects durable storage to a Linear source.
func NewAdapter(store db.Storage, fetcher Fetcher) *Adapter {
	return &Adapter{store: store, fetcher: fetcher, observed: map[int64]contentObservations{}}
}

func (a *Adapter) recordObservations(bindingID int64, sourceKey string, states []State, items []Issue) error {
	types, err := stateTypes(states)
	if err != nil {
		return err
	}
	byID := make(map[string]issuesync.StatusObservation, len(items))
	for _, i := range items {
		id, err := CanonicalID(i.ID)
		if err != nil {
			return err
		}
		obs, err := statusObservation(i, types[i.StateID])
		if err != nil {
			return err
		}
		byID[id] = obs
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observed[bindingID] = contentObservations{sourceKey: sourceKey, byID: byID}
	return nil
}

// takeObservation serves each content observation to at most one sweep, and
// only when it is not older than the status already stored for the mapping.
func (a *Adapter) takeObservation(bindingID int64, sourceKey, id string, stored *db.IssueStatusObservation) (issuesync.StatusObservation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	observed, ok := a.observed[bindingID]
	if !ok || observed.sourceKey != sourceKey {
		return issuesync.StatusObservation{}, false
	}
	obs, ok := observed.byID[id]
	delete(observed.byID, id)
	if !ok || stored != nil && obs.Version.Before(stored.Version) {
		return issuesync.StatusObservation{}, false
	}
	return obs, true
}

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
	if c.StatusSync == "two-way" {
		if err := a.recordObservations(b.ID, b.SourceKey, states, eligible); err != nil {
			return prepared, err
		}
	}
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
