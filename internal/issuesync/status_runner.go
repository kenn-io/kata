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
	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: binding.Provider, StartedAt: startedAt, BindingUpdatedAt: new(binding.UpdatedAt)}
	attempted := map[int64]bool{}
	var failures []error
	if locatorRun, ok := run.(StatusLocatorRun); ok {
		page := state.LocatorPage
		if page == 0 {
			page = 1
		}
		locators, next, err := locatorRun.Locators(statusCtx, page)
		if err == nil && (len(locators) > statusPageSize || next < 0 || (next != 0 && next != page+1)) {
			err = fmt.Errorf("invalid bounded status locator page")
		}
		if err == nil {
			err = r.saveStatusLocators(statusCtx, guard, locators)
		}
		if err == nil {
			state.LocatorPage = next
			var refreshed db.IssueSyncBinding
			if refreshed, err = scans.UpdateIssueStatusScan(statusCtx, guard, state); err == nil {
				binding = refreshed
			}
		}
		if err != nil {
			failures = append(failures, err)
			if errors.Is(err, db.ErrIssueSyncAlreadyRunning) || errors.Is(err, db.ErrIssueSyncBindingChanged) || errors.Is(err, db.ErrIssueSyncNotEnabled) || statusCtx.Err() != nil {
				return binding, true, errors.Join(failures...)
			}
		}
	}
	for _, pending := range []bool{true, false} {
		cursor := state.Sweep
		if pending {
			cursor = state.Pending
		}
		page, err := reader.ListIssueStatusMappings(statusCtx, db.IssueStatusQuery{Guard: guard, AfterID: cursor.After, ThroughID: cursor.Through, Limit: statusPageSize, PendingOnly: pending})
		if err != nil {
			return binding, true, errors.Join(append(failures, err)...)
		}
		cursor.Through = page.HighWaterID
		for _, m := range page.Mappings {
			if err := statusCtx.Err(); err != nil {
				return binding, true, errors.Join(append(failures, err)...)
			}
			previousAfter := cursor.After
			cursor.After = m.Mapping.ID
			if attempted[m.Mapping.ID] {
				continue
			}
			if !pending && m.State.PendingEventUID != "" {
				continue
			}
			attempted[m.Mapping.ID] = true
			serviced := ""
			var obs StatusObservation
			if m.LoadError != nil {
				// Invalid local state cannot change until repaired, so content proceeds.
				err = &StatusError{Message: m.LoadError.Error(), Blocked: true}
			} else if pending {
				if m.PendingEvent == nil {
					err = fmt.Errorf("pending issue status event is unavailable")
				} else {
					serviced = m.PendingEvent.UID
					desired := "open"
					if m.PendingEvent.Type == "issue.closed" {
						desired = "closed"
					}
					admit := func() error {
						if err := statusCtx.Err(); err != nil {
							return err
						}
						// Request timeouts are at most30s. Leave five minutes before abandoned
						// claims can be recovered, including queued pacing and delayed cleanup.
						horizon := min(statusRunTimeout, r.staleLockTTL()-5*time.Minute)
						if horizon <= 0 || !r.now().Before(startedAt.Add(horizon)) {
							return fmt.Errorf("issue status dispatch recovery horizon expired")
						}
						fresh, err := reader.IssueStatusMappingByID(statusCtx, guard, m.Mapping.ID)
						if err != nil {
							return err
						}
						if fresh.Mapping.ExternalID != m.Mapping.ExternalID || fresh.Mapping.IssueID == nil || m.Mapping.IssueID == nil || *fresh.Mapping.IssueID != *m.Mapping.IssueID || fresh.State.PendingEventUID != serviced || fresh.PendingEvent == nil || fresh.PendingEvent.Type != m.PendingEvent.Type {
							return errStatusIntentChanged
						}
						return nil
					}
					obs, err = run.WriteStatus(statusCtx, m, desired, admit)
				}
			} else {
				obs, err = run.ReadStatus(statusCtx, m)
			}
			if err == nil {
				issue, loadErr := r.config.Store.IssueByID(statusCtx, *m.Mapping.IssueID)
				err = loadErr
				if err == nil {
					p := db.IssueStatusObservationParams{Guard: guard, MappingID: m.Mapping.ID, ExternalID: m.Mapping.ExternalID, IssueUID: issue.UID, Observation: db.IssueStatusObservation{Raw: obs.RawStatus, Version: obs.Version}, Status: obs.Status, ClosedReason: obs.ClosedReason, ClosedAt: obs.ClosedAt, ServicedEventUID: serviced, Authoritative: true}
					// Notion's page UUID is already external_id. Only an additional provider
					// API identifier, such as the GitHub issue number, belongs in the locator.
					if binding.Provider == "github" && obs.Locator != "" {
						p.RemoteLocator = new(obs.Locator)
					}
					changed, events, saveErr := writer.ObserveIssueStatus(statusCtx, p)
					err = saveErr
					if saveErr == nil {
						if changed {
							(*statusUpdated)++
						}
						r.emitEvents(ctx, binding.ProjectID, events, eventFork)
					}
				}
			}
			// A newer local close or reopen superseded this write; the next lap
			// delivers it.
			if err != nil && !errors.Is(err, errStatusIntentChanged) {
				failures = append(failures, err)
			}
			// A rate limit fails every later request fast. Leave this mapping for
			// the next run rather than consuming the rest of the lap as failures.
			var limited *StatusError
			rateLimited := errors.As(err, &limited) && limited.RetryAfter > 0 && !limited.Ambiguous
			if rateLimited {
				cursor.After = previousAfter
			}
			// Persist each attempted item before another provider call. A slow
			// prefix must not reset the whole page when the run expires. The
			// cleanup budget keeps canceled attempts durable under the same claim.
			if pending {
				state.Pending = cursor
			} else {
				state.Sweep = cursor
			}
			checkpointCtx := statusCtx
			var cleanupCancel context.CancelFunc
			if statusCtx.Err() != nil {
				checkpointCtx, cleanupCancel = r.cleanupContext(statusCtx)
			}
			refreshed, checkpointErr := scans.UpdateIssueStatusScan(checkpointCtx, guard, state)
			// Cancellation can race the checkpoint admission above. Retry once
			// with the detached budget; the store still rechecks live authority.
			if checkpointErr != nil && cleanupCancel == nil && statusCtx.Err() != nil {
				checkpointCtx, cleanupCancel = r.cleanupContext(statusCtx)
				refreshed, checkpointErr = scans.UpdateIssueStatusScan(checkpointCtx, guard, state)
			}
			if cleanupCancel != nil {
				cleanupCancel()
			}
			if checkpointErr != nil {
				return binding, true, errors.Join(append(failures, checkpointErr)...)
			}
			binding = refreshed
			if rateLimited {
				return binding, true, errors.Join(failures...)
			}
			if err != nil {
				if ambiguousStatusError(err) {
					return binding, true, errors.Join(failures...)
				}
				if errors.Is(err, db.ErrIssueSyncAlreadyRunning) || errors.Is(err, db.ErrIssueSyncBindingChanged) || errors.Is(err, db.ErrIssueSyncNotEnabled) || statusCtx.Err() != nil {
					return binding, true, errors.Join(failures...)
				}
			}
		}
		if len(page.Mappings) < statusPageSize || cursor.After >= cursor.Through {
			cursor = db.IssueStatusScanCursor{}
		}
		if pending {
			state.Pending = cursor
		} else {
			state.Sweep = cursor
		}
		refreshed, err := scans.UpdateIssueStatusScan(statusCtx, guard, state)
		if err != nil {
			return binding, true, errors.Join(append(failures, err)...)
		}
		binding = refreshed
	}
	return binding, true, errors.Join(failures...)
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
