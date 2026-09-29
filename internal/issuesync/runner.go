package issuesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
)

const (
	// DefaultStaleLockTTL is the recovery horizon for an abandoned sync claim.
	DefaultStaleLockTTL = 30 * time.Minute
	// DefaultInitialBatchSize limits the number of imported issues per batch.
	DefaultInitialBatchSize = 5000
)

const (
	runnerDueLimit       = 100
	runnerCleanupTimeout = 10 * time.Second
)

// Runner runs durable issue sync bindings through the import pipeline.
type Runner struct {
	config RunnerConfig
}

// RunnerConfig configures an issue sync runner.
type RunnerConfig struct {
	Progress *ProgressTracker
	Store    db.Storage
	Adapter  Adapter
	// RunTimeout bounds preparation, imports, and finalization after claim.
	// Zero preserves detached GitHub finalization after caller cancellation.
	RunTimeout time.Duration
	Clock      func() time.Time
	EventSink  func(context.Context, int64, []db.Event) error
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
type RunResult struct {
	Binding       db.IssueSyncBinding
	Status        db.IssueSyncStatus
	Import        db.ImportBatchResult
	StatusUpdated int
}

type contentPreparationFailure struct {
	cause error
}

func (e *contentPreparationFailure) Error() string { return e.cause.Error() }
func (e *contentPreparationFailure) Unwrap() error { return e.cause }

// Adapter prepares provider data after the engine acquires a durable claim.
type Adapter interface {
	Provider() string
	InitialPhase() string
	Prepare(context.Context, db.IssueSyncBinding, time.Time) (Prepared, error)
}

// Prepared contains the complete batch and optional post-import finalizer.
// Finalize must fence provider-owned binding updates with the claimed timestamp.
type Prepared struct {
	Binding  db.IssueSyncBinding
	Batch    db.ImportBatchParams
	Locators []db.IssueStatusLocator
	Finalize func(context.Context) (db.IssueSyncBinding, error)
}

// NewRunner returns a runner with defaults applied.
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
	if config.Adapter != nil {
		config.Logger = config.Logger.With("provider", config.Adapter.Provider())
	}
	return &Runner{config: config}
}

// RunOnce syncs one binding if it can claim the binding lock.
func (r *Runner) RunOnce(ctx context.Context, bindingID int64) (RunResult, error) {
	return r.runOnce(ctx, bindingID, nil)
}

func (r *Runner) runOnce(
	ctx context.Context,
	bindingID int64,
	eventFork activity.Admission,
) (RunResult, error) {
	if err := r.validate(); err != nil {
		return RunResult{}, err
	}
	syncStartedAt := r.now().Truncate(time.Millisecond)
	binding, claimed, err := r.config.Store.ClaimIssueSyncBinding(ctx, bindingID, r.config.Adapter.Provider(), syncStartedAt, syncStartedAt.Add(-r.staleLockTTL()))
	if err != nil {
		return RunResult{}, err
	}
	if !claimed {
		return RunResult{Binding: binding}, db.ErrIssueSyncAlreadyRunning
	}

	r.config.Progress.BeginWithPhase(bindingID, syncStartedAt, r.config.Adapter.InitialPhase())
	defer r.config.Progress.Finish(bindingID, syncStartedAt)
	timeout := r.config.RunTimeout
	if mode, _ := db.IssueStatusMode(binding.Config); mode == "two-way" && (timeout <= 0 || timeout > statusRunTimeout) {
		timeout = statusRunTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ctx = WithProgressReporter(ctx, func(phase string, completed, total int) {
		r.config.Progress.Update(bindingID, syncStartedAt, phase, completed, total, r.now())
	})
	result, err := r.runClaimed(ctx, binding, syncStartedAt, eventFork)
	if err != nil {
		return result, err
	}
	return result, nil
}

// Run processes currently due bindings serially, then repeats on Interval when
// configured. With Interval <= 0 it performs one admitted due scan and returns;
// reversible admission denial waits for reopening before that one scan.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	var ticker *time.Ticker
	if r.config.Interval > 0 {
		ticker = time.NewTicker(r.config.Interval)
		defer ticker.Stop()
	}
	for {
		retry, denied, err := r.runDue(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if ticker == nil {
				return err
			}
			r.config.Logger.Warn("issue sync due pass failed", "error", err)
		}
		if denied && retry != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-retry:
				continue
			}
		}
		if denied {
			if ticker == nil {
				return nil
			}
			<-ctx.Done()
			return ctx.Err()
		}
		if ticker == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-r.config.Wake:
		}
	}
}

func (r *Runner) runDue(ctx context.Context) (<-chan struct{}, bool, error) {
	var scan *activity.Lease
	if r.config.DrainAdmission != nil {
		var admitted bool
		var retry <-chan struct{}
		scan, admitted, retry = r.config.DrainAdmission()
		if !admitted {
			return retry, true, nil
		}
	}
	now := r.now()
	bindings, err := func() ([]db.IssueSyncBinding, error) {
		if scan != nil {
			defer scan.Release()
		}
		return r.config.Store.ListDueIssueSyncBindings(
			ctx, r.config.Adapter.Provider(), now, now.Add(-r.staleLockTTL()), runnerDueLimit,
		)
	}()
	if err != nil {
		return nil, false, err
	}
	var errs []error
	for _, binding := range bindings {
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		default:
		}
		var drain *activity.Lease
		if r.config.DrainAdmission != nil {
			var admitted bool
			var retry <-chan struct{}
			drain, admitted, retry = r.config.DrainAdmission()
			if !admitted {
				return retry, true, errors.Join(errs...)
			}
		}
		_, runErr := func() (RunResult, error) {
			if drain != nil {
				defer drain.Release()
				return r.runOnce(ctx, binding.ID, drain.Fork)
			}
			return r.runOnce(ctx, binding.ID, nil)
		}()
		if runErr != nil {
			err := runErr
			if errorsIsAlreadyRunning(err) {
				continue
			}
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			r.config.Logger.Warn("issue sync binding failed", "binding_id", binding.ID, "error", err)
			errs = append(errs, err)
		}
	}
	return nil, false, errors.Join(errs...)
}

func (r *Runner) runClaimed(ctx context.Context, binding db.IssueSyncBinding, syncStartedAt time.Time, eventFork activity.Admission) (result RunResult, runErr error) {
	statusUpdated := 0
	defer func() { result.StatusUpdated = statusUpdated }()
	binding, separateStatus, statusErr := r.runStatuses(ctx, binding, syncStartedAt, eventFork, &statusUpdated)
	prepared, err := r.config.Adapter.Prepare(ctx, binding, syncStartedAt)
	if err != nil {
		// A provider may have refreshed mutable metadata before a later read failed.
		if sameBindingIdentity(binding, prepared.Binding) {
			binding = prepared.Binding
		}
		return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, &contentPreparationFailure{cause: err}), db.ImportBatchResult{})
	}
	if !sameBindingIdentity(binding, prepared.Binding) {
		return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, fmt.Errorf("issue sync adapter returned a different binding identity")), db.ImportBatchResult{})
	}
	binding = prepared.Binding
	batch := prepared.Batch
	batch.ManageStatusSeparately = separateStatus
	batch.ProjectID = binding.ProjectID
	batch.Source = binding.SourceKey
	batch.IssueSyncGuard = &db.IssueSyncImportGuard{BindingID: binding.ID, Provider: binding.Provider, StartedAt: syncStartedAt, BindingUpdatedAt: new(binding.UpdatedAt)}
	importResult, err := r.importChunks(ctx, binding, batch, eventFork)
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, err), importResult)
	}
	if separateStatus && len(prepared.Locators) > 0 {
		if err := r.saveStatusLocators(ctx, *batch.IssueSyncGuard, prepared.Locators); err != nil {
			return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, err), importResult)
		}
	}
	ReportProgress(ctx, "finalizing", 0, 0)
	// GitHub keeps one detached cleanup budget shared by its finalizer and
	// success record. Bounded providers finalize within their run context.
	finalizeCtx := ctx
	if r.config.RunTimeout <= 0 {
		cleanupCtx, cleanupCancel := r.cleanupContext(ctx)
		defer cleanupCancel()
		finalizeCtx = cleanupCtx
	} else if err := ctx.Err(); err != nil {
		return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, err), importResult)
	}
	if prepared.Finalize != nil {
		refreshed, err := prepared.Finalize(finalizeCtx)
		if err != nil {
			return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, err), importResult)
		}
		if !sameBindingIdentity(binding, refreshed) {
			return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, fmt.Errorf("issue sync finalizer returned a different binding identity")), importResult)
		}
		binding = refreshed
	}
	successCtx := finalizeCtx
	if r.config.RunTimeout > 0 {
		if err := ctx.Err(); err != nil {
			return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, err), importResult)
		}
		// Start the success-recording budget after finalization, clipped to the
		// remaining run deadline so an expired run cannot advance its cursor.
		var cleanupCancel context.CancelFunc
		successCtx, cleanupCancel = r.cleanupContext(ctx)
		defer cleanupCancel()
		if deadline, ok := ctx.Deadline(); ok {
			var cancel context.CancelFunc
			successCtx, cancel = context.WithDeadline(successCtx, deadline)
			defer cancel()
		}
	}
	if statusErr != nil && !blockedStatusError(statusErr) {
		return r.recordError(ctx, binding, syncStartedAt, statusErr, importResult)
	}
	statusWarning := ""
	if statusErr != nil {
		statusWarning = statusErr.Error()
	}
	status, err := r.config.Store.RecordIssueSyncSuccess(successCtx, db.IssueSyncSuccessParams{
		StatusError:      statusWarning,
		BindingID:        binding.ID,
		BindingUpdatedAt: new(binding.UpdatedAt),
		StartedAt:        syncStartedAt,
		At:               r.now(),
		CursorAt:         syncStartedAt,
		LastCreated:      importResult.Created,
		LastUpdated:      importResult.Updated,
		LastUnchanged:    importResult.Unchanged,
		LastComments:     importResult.Comments,
	})
	if err != nil {
		return r.recordError(ctx, binding, syncStartedAt, errors.Join(statusErr, err), importResult)
	}
	binding.LastCursorAt = &syncStartedAt
	return RunResult{Binding: binding, Status: status, Import: importResult}, statusErr
}

func sameBindingIdentity(a, b db.IssueSyncBinding) bool {
	return a.ID == b.ID && a.ProjectID == b.ProjectID && a.Provider == b.Provider && a.SourceKey == b.SourceKey && a.RemoteID == b.RemoteID
}

func (r *Runner) importChunks(
	ctx context.Context,
	binding db.IssueSyncBinding,
	batch db.ImportBatchParams,
	eventFork activity.Admission,
) (db.ImportBatchResult, error) {
	ReportProgress(ctx, "importing", 0, len(batch.Items))
	chunkSize := r.initialBatchSize()
	aggregate := db.ImportBatchResult{Source: batch.Source, Errors: []string{}}
	if len(batch.Items) == 0 {
		res, events, err := r.config.Store.ImportBatch(ctx, batch)
		if err != nil {
			return aggregate, err
		}
		mergeImportResult(&aggregate, res)
		r.emitEvents(ctx, binding.ProjectID, events, eventFork)
		return aggregate, nil
	}
	for start := 0; start < len(batch.Items); start += chunkSize {
		end := min(start+chunkSize, len(batch.Items))
		chunk := batch
		chunk.Items = append([]db.ImportItem(nil), batch.Items[start:end]...)
		res, events, err := r.config.Store.ImportBatch(ctx, chunk)
		if err != nil {
			return aggregate, err
		}
		mergeImportResult(&aggregate, res)
		r.emitEvents(ctx, binding.ProjectID, events, eventFork)
		ReportProgress(ctx, "importing", end, len(batch.Items))
	}
	return aggregate, nil
}

func (r *Runner) emitEvents(
	ctx context.Context,
	projectID int64,
	events []db.Event,
	eventFork activity.Admission,
) {
	if len(events) == 0 {
		return
	}
	var err error
	if r.config.EventSinkFrom != nil {
		err = r.config.EventSinkFrom(ctx, projectID, events, eventFork)
	} else if r.config.EventSink != nil {
		err = r.config.EventSink(ctx, projectID, events)
	}
	if err != nil {
		r.config.Logger.Warn("issue sync event sink failed", "error", err)
	}
}

func (r *Runner) recordError(ctx context.Context, binding db.IssueSyncBinding, startedAt time.Time, cause error, importResult db.ImportBatchResult) (RunResult, error) {
	cleanupCtx, cleanupCancel := r.cleanupContext(ctx)
	defer cleanupCancel()
	status, recordErr := r.config.Store.RecordIssueSyncError(cleanupCtx, db.IssueSyncErrorParams{
		RetainClaim: ambiguousStatusError(cause),
		BindingID:   binding.ID,
		StartedAt:   startedAt,
		At:          r.now(),
		Error:       cause.Error(),
	})
	if recordErr != nil {
		return RunResult{Binding: binding, Import: importResult}, recordErr
	}
	return RunResult{Binding: binding, Status: status, Import: importResult}, cause
}

func (r *Runner) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), runnerCleanupTimeout)
}

func (r *Runner) now() time.Time {
	return r.config.Clock().UTC()
}

func (r *Runner) validate() error {
	if r.config.Store == nil {
		return fmt.Errorf("issue sync runner requires store")
	}
	if r.config.Adapter == nil {
		return fmt.Errorf("issue sync runner requires adapter")
	}
	return nil
}

func (r *Runner) staleLockTTL() time.Duration {
	if r.config.StaleLockTTL <= 0 {
		return DefaultStaleLockTTL
	}
	return r.config.StaleLockTTL
}

func (r *Runner) initialBatchSize() int {
	if r.config.InitialBatchSize <= 0 {
		return DefaultInitialBatchSize
	}
	return r.config.InitialBatchSize
}

func mergeImportResult(dst *db.ImportBatchResult, src db.ImportBatchResult) {
	dst.Created += src.Created
	dst.Updated += src.Updated
	dst.Unchanged += src.Unchanged
	dst.Comments += src.Comments
	dst.Links += src.Links
	dst.Items = append(dst.Items, src.Items...)
	dst.Errors = append(dst.Errors, src.Errors...)
}

func errorsIsAlreadyRunning(err error) bool {
	return errors.Is(err, db.ErrIssueSyncAlreadyRunning)
}
