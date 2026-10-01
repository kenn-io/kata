package issuesync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
)

// StatusAdapter opens a status-only session before content preparation.
// Nil preserves compatibility with a legacy one-way provider implementation.
type StatusAdapter interface {
	OpenStatus(context.Context, db.IssueSyncBinding, time.Time) (StatusRun, error)
}

// StatusRun performs independent metadata reads and verified status writes.
// Providers invoke admission after pacing immediately before PATCH dispatch.
type StatusRun interface {
	ReadStatus(context.Context, db.IssueStatusMapping) (StatusObservation, error)
	WriteStatus(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error)
}

const statusPageSize = 100
const statusRunTimeout = 25 * time.Minute

var errStatusIntentChanged = errors.New("issue status intent changed before dispatch")

func ambiguousStatusError(err error) bool {
	if classified, ok := err.(*StatusError); ok && classified.Ambiguous {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return slices.ContainsFunc(joined.Unwrap(), ambiguousStatusError)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return ambiguousStatusError(wrapped.Unwrap())
	}
	return false
}

func blockedStatusError(err error) bool {
	if classified, ok := err.(*StatusError); ok {
		return classified.Blocked && !classified.Ambiguous
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		return len(causes) > 0 && !slices.ContainsFunc(causes, func(cause error) bool { return !blockedStatusError(cause) })
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return blockedStatusError(wrapped.Unwrap())
	}
	return false
}

// IsBlockedStatusWarning reports whether err contains only blocked, non-ambiguous
// status failures. The content sync can still succeed and return its import result.
func IsBlockedStatusWarning(err error) bool {
	if _, ok := errors.AsType[*contentPreparationFailure](err); ok {
		return false
	}
	return blockedStatusError(err)
}

// Status reads/delivery have independent laps; content failures cannot starve
// them. Each cursor captures its mapping high-watermark and advances even when
// an individual provider read fails. A later lap retries without per-item state.
func (r *Runner) runStatuses(ctx context.Context, binding db.IssueSyncBinding, startedAt time.Time, eventFork activity.Admission, statusUpdated *int) (db.IssueSyncBinding, bool, error) {
	mode, err := db.IssueStatusMode(binding.Config)
	if err != nil {
		return binding, false, err
	}
	// One-way bindings take status from content imports, as before two-way
	// sync existed, so a local change stays until the provider changes.
	if mode != "two-way" {
		return binding, false, nil
	}
	adapter, ok := r.config.Adapter.(StatusAdapter)
	if !ok {
		return binding, true, &StatusError{Message: "provider does not support two-way status sync", Blocked: true}
	}
	statusCtx, cancel := context.WithTimeout(ctx, statusRunTimeout)
	defer cancel()
	run, err := adapter.OpenStatus(statusCtx, binding, startedAt)
	if err != nil {
		return binding, true, err
	}
	if run == nil {
		return binding, true, &StatusError{Message: "provider does not support two-way status sync", Blocked: true}
	}
	reader, readOK := r.config.Store.(db.IssueStatusReader)
	writer, writeOK := r.config.Store.(db.IssueStatusWriter)
	scans, scanOK := r.config.Store.(db.IssueStatusScanStore)
	if !readOK || !writeOK || !scanOK {
		return binding, true, fmt.Errorf("issue status storage is unavailable")
	}
	state, err := db.DecodeIssueStatusScan(binding.Config)
	if err != nil {
		return binding, true, err
	}
	pass := &statusPass{
		r: r, ctx: ctx, statusCtx: statusCtx, run: run, reader: reader, writer: writer, scans: scans,
		guard:   db.IssueSyncImportGuard{BindingID: binding.ID, Provider: binding.Provider, StartedAt: startedAt, BindingUpdatedAt: new(binding.UpdatedAt)},
		binding: binding, state: state, startedAt: startedAt, eventFork: eventFork, updated: statusUpdated,
	}
	err = pass.execute()
	return pass.binding, true, err
}

// statusPass carries one run's status session and claim-fenced storage.
type statusPass struct {
	r         *Runner
	ctx       context.Context
	statusCtx context.Context
	run       StatusRun
	reader    db.IssueStatusReader
	writer    db.IssueStatusWriter
	scans     db.IssueStatusScanStore
	guard     db.IssueSyncImportGuard
	binding   db.IssueSyncBinding
	state     db.IssueStatusScanState
	startedAt time.Time
	eventFork activity.Admission
	updated   *int
}

func (p *statusPass) execute() error {
	var failures []error
	if locatorRun, ok := p.run.(StatusLocatorRun); ok {
		if err := p.refreshLocators(locatorRun); err != nil {
			failures = append(failures, err)
			if p.stopsPass(err) {
				return errors.Join(failures...)
			}
		}
	}
	attempted := map[int64]bool{}
	for _, pending := range []bool{true, false} {
		stop, err := p.lap(pending, attempted, &failures)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		if stop {
			break
		}
	}
	return errors.Join(failures...)
}

// stopsPass reports errors after which no further item in this run can succeed.
func (p *statusPass) stopsPass(err error) bool {
	return errors.Is(err, db.ErrIssueSyncAlreadyRunning) || errors.Is(err, db.ErrIssueSyncBindingChanged) || errors.Is(err, db.ErrIssueSyncNotEnabled) || p.statusCtx.Err() != nil
}

func (p *statusPass) refreshLocators(run StatusLocatorRun) error {
	page := p.state.LocatorPage
	if page == 0 {
		page = 1
	}
	locators, next, err := run.Locators(p.statusCtx, page)
	if err != nil {
		return err
	}
	if len(locators) > statusPageSize || next < 0 || (next != 0 && next != page+1) {
		return fmt.Errorf("invalid bounded status locator page")
	}
	if err := p.r.saveStatusLocators(p.statusCtx, p.guard, locators); err != nil {
		return err
	}
	p.state.LocatorPage = next
	refreshed, err := p.scans.UpdateIssueStatusScan(p.statusCtx, p.guard, p.state)
	if err != nil {
		return err
	}
	p.binding = refreshed
	return nil
}

// lap visits one page of pending or sweep mappings, checkpointing each item.
// It reports stop when the rest of the pass must wait for another run.
func (p *statusPass) lap(pending bool, attempted map[int64]bool, failures *[]error) (bool, error) {
	cursor := p.state.Sweep
	if pending {
		cursor = p.state.Pending
	}
	page, err := p.reader.ListIssueStatusMappings(p.statusCtx, db.IssueStatusQuery{Guard: p.guard, AfterID: cursor.After, ThroughID: cursor.Through, Limit: statusPageSize, PendingOnly: pending})
	if err != nil {
		return true, err
	}
	cursor.Through = page.HighWaterID
	for _, m := range page.Mappings {
		if err := p.statusCtx.Err(); err != nil {
			return true, err
		}
		previousAfter := cursor.After
		cursor.After = m.Mapping.ID
		if attempted[m.Mapping.ID] || (!pending && m.State.PendingEventUID != "") {
			continue
		}
		attempted[m.Mapping.ID] = true
		itemErr := p.service(m, pending)
		// A newer local close or reopen superseded this write; the next lap
		// delivers it.
		if itemErr != nil && !errors.Is(itemErr, errStatusIntentChanged) {
			*failures = append(*failures, itemErr)
		}
		// A rate limit fails every later request fast. Leave this mapping for
		// the next run rather than consuming the rest of the lap as failures.
		limited, ok := errors.AsType[*StatusError](itemErr)
		rateLimited := ok && limited != nil && limited.RetryAfter > 0 && !limited.Ambiguous
		if rateLimited {
			cursor.After = previousAfter
		}
		if err := p.checkpoint(pending, cursor); err != nil {
			return true, err
		}
		if rateLimited || itemErr != nil && (ambiguousStatusError(itemErr) || p.stopsPass(itemErr)) {
			return true, nil
		}
	}
	if len(page.Mappings) < statusPageSize || cursor.After >= cursor.Through {
		cursor = db.IssueStatusScanCursor{}
	}
	p.setCursor(pending, cursor)
	refreshed, err := p.scans.UpdateIssueStatusScan(p.statusCtx, p.guard, p.state)
	if err != nil {
		return true, err
	}
	p.binding = refreshed
	return false, nil
}

func (p *statusPass) setCursor(pending bool, cursor db.IssueStatusScanCursor) {
	if pending {
		p.state.Pending = cursor
	} else {
		p.state.Sweep = cursor
	}
}

// checkpoint persists each attempted item before another provider call. A
// slow prefix must not reset the whole page when the run expires. The cleanup
// budget keeps canceled attempts durable under the same claim.
func (p *statusPass) checkpoint(pending bool, cursor db.IssueStatusScanCursor) error {
	p.setCursor(pending, cursor)
	checkpointCtx := p.statusCtx
	var cleanupCancel context.CancelFunc
	if p.statusCtx.Err() != nil {
		checkpointCtx, cleanupCancel = p.r.cleanupContext(p.statusCtx)
	}
	refreshed, err := p.scans.UpdateIssueStatusScan(checkpointCtx, p.guard, p.state)
	// Cancellation can race the checkpoint admission above. Retry once
	// with the detached budget; the store still rechecks live authority.
	if err != nil && cleanupCancel == nil && p.statusCtx.Err() != nil {
		checkpointCtx, cleanupCancel = p.r.cleanupContext(p.statusCtx)
		refreshed, err = p.scans.UpdateIssueStatusScan(checkpointCtx, p.guard, p.state)
	}
	if cleanupCancel != nil {
		cleanupCancel()
	}
	if err != nil {
		return err
	}
	p.binding = refreshed
	return nil
}

// service reads or delivers one mapping's status and records the result.
func (p *statusPass) service(m db.IssueStatusMapping, pending bool) error {
	if m.LoadError != nil {
		// Invalid local state cannot change until repaired, so content proceeds.
		return &StatusError{Message: m.LoadError.Error(), Blocked: true}
	}
	if !pending {
		obs, err := p.run.ReadStatus(p.statusCtx, m)
		if err != nil {
			return err
		}
		return p.observe(m, obs, "")
	}
	if m.PendingEvent == nil {
		return fmt.Errorf("pending issue status event is unavailable")
	}
	desired := "open"
	if m.PendingEvent.Type == "issue.closed" {
		desired = "closed"
	}
	obs, err := p.run.WriteStatus(p.statusCtx, m, desired, p.admission(m))
	if err != nil {
		return err
	}
	return p.observe(m, obs, m.PendingEvent.UID)
}

// admission runs immediately before a PATCH. It refuses dispatch when the
// claim may soon be recovered or a newer local mutation replaced the intent.
func (p *statusPass) admission(m db.IssueStatusMapping) func() error {
	return func() error {
		if err := p.statusCtx.Err(); err != nil {
			return err
		}
		// Request timeouts are at most30s. Leave five minutes before abandoned
		// claims can be recovered, including queued pacing and delayed cleanup.
		horizon := min(statusRunTimeout, p.r.staleLockTTL()-5*time.Minute)
		if horizon <= 0 || !p.r.now().Before(p.startedAt.Add(horizon)) {
			return fmt.Errorf("issue status dispatch recovery horizon expired")
		}
		fresh, err := p.reader.IssueStatusMappingByID(p.statusCtx, p.guard, m.Mapping.ID)
		if err != nil {
			return err
		}
		if fresh.Mapping.ExternalID != m.Mapping.ExternalID || fresh.Mapping.IssueID == nil || m.Mapping.IssueID == nil || *fresh.Mapping.IssueID != *m.Mapping.IssueID || fresh.State.PendingEventUID != m.PendingEvent.UID || fresh.PendingEvent == nil || fresh.PendingEvent.Type != m.PendingEvent.Type {
			return errStatusIntentChanged
		}
		return nil
	}
}

func (p *statusPass) observe(m db.IssueStatusMapping, obs StatusObservation, serviced string) error {
	issue, err := p.r.config.Store.IssueByID(p.statusCtx, *m.Mapping.IssueID)
	if err != nil {
		return err
	}
	params := db.IssueStatusObservationParams{Guard: p.guard, MappingID: m.Mapping.ID, ExternalID: m.Mapping.ExternalID, IssueUID: issue.UID, Observation: db.IssueStatusObservation{Raw: obs.RawStatus, Version: obs.Version}, Status: obs.Status, ClosedReason: obs.ClosedReason, ClosedAt: obs.ClosedAt, ServicedEventUID: serviced}
	// Notion's page UUID is already external_id. Only an additional provider
	// API identifier, such as the GitHub issue number, belongs in the locator.
	if p.binding.Provider == "github" && obs.Locator != "" {
		params.RemoteLocator = new(obs.Locator)
	}
	changed, events, err := p.writer.ObserveIssueStatus(p.statusCtx, params)
	if err != nil {
		return err
	}
	if changed {
		(*p.updated)++
	}
	p.r.emitEvents(p.ctx, p.binding.ProjectID, events, p.eventFork)
	return nil
}

// StatusLocatorRun enumerates one verified provider page without fetching any
// title/body/comments or applying the content since cutoff.
type StatusLocatorRun interface {
	Locators(context.Context, int) ([]db.IssueStatusLocator, int, error)
}

func (r *Runner) saveStatusLocators(ctx context.Context, guard db.IssueSyncImportGuard, locators []db.IssueStatusLocator) error {
	store, ok := r.config.Store.(db.IssueStatusLocatorStore)
	if !ok {
		return fmt.Errorf("issue status locator storage is unavailable")
	}
	for _, locator := range locators {
		if _, err := store.SaveIssueStatusLocator(ctx, guard, locator); err != nil {
			return err
		}
	}
	return nil
}
