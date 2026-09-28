package githubsync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

const (
	// DefaultStaleLockTTL is the recovery horizon for an abandoned sync claim.
	DefaultStaleLockTTL = issuesync.DefaultStaleLockTTL
	// DefaultInitialBatchSize limits the number of imported issues per batch.
	DefaultInitialBatchSize = issuesync.DefaultInitialBatchSize
)

// Runner runs durable GitHub sync bindings through the shared lifecycle.
type Runner struct{ config RunnerConfig }

// RunnerConfig configures a GitHub sync runner.
type RunnerConfig struct {
	Progress  *ProgressTracker
	Store     db.Storage
	Fetcher   Fetcher
	Clock     func() time.Time
	EventSink func(context.Context, int64, []db.Event) error
	// EventSinkFrom takes precedence over EventSink when set. Its fork source
	// is nil when the pass has no parent activity lease.
	EventSinkFrom    func(context.Context, int64, []db.Event, activity.Admission) error
	Logger           *slog.Logger
	Interval         time.Duration
	Wake             <-chan struct{}
	StaleLockTTL     time.Duration
	InitialBatchSize int
	DrainAdmission   activity.WaitableAdmission
}

// RunResult summarizes one completed sync attempt.
type RunResult = issuesync.RunResult

// NewRunner preserves the GitHub entry point and its defaults.
func NewRunner(config RunnerConfig) *Runner {
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.StaleLockTTL <= 0 {
		config.StaleLockTTL = DefaultStaleLockTTL
	}
	if config.InitialBatchSize <= 0 {
		config.InitialBatchSize = DefaultInitialBatchSize
	}
	return &Runner{config: config}
}

func (r *Runner) shared() (*issuesync.Runner, error) {
	if r.config.Store == nil {
		return nil, fmt.Errorf("github sync runner requires store")
	}
	if r.config.Fetcher == nil {
		return nil, fmt.Errorf("github sync runner requires fetcher")
	}
	return issuesync.NewRunner(issuesync.RunnerConfig{
		Store:            r.config.Store,
		Adapter:          &adapter{config: r.config},
		Progress:         r.config.Progress,
		Clock:            r.config.Clock,
		EventSink:        r.config.EventSink,
		EventSinkFrom:    r.config.EventSinkFrom,
		Logger:           r.config.Logger,
		Interval:         r.config.Interval,
		Wake:             r.config.Wake,
		StaleLockTTL:     r.config.StaleLockTTL,
		InitialBatchSize: r.config.InitialBatchSize,
		DrainAdmission:   r.config.DrainAdmission,
	}), nil
}

// Run processes due GitHub bindings and repeats on the configured interval.
func (r *Runner) Run(ctx context.Context) error {
	shared, err := r.shared()
	if err != nil {
		return err
	}
	return shared.Run(ctx)
}

// RunOnce syncs one GitHub binding if it can claim the binding lock.
func (r *Runner) RunOnce(ctx context.Context, id int64) (RunResult, error) {
	shared, err := r.shared()
	if err != nil {
		return RunResult{}, err
	}
	return shared.RunOnce(ctx, id)
}
