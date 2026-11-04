package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/cron"
)

// ReapCronRun records an operator's unknown outcome without changing run identity.
type ReapCronRun struct {
	ProjectID, ExpectedRevision int64
	UID, Actor, Message         string
	Force                       bool
	Now                         time.Time
}

var (
	// ErrCronRunNotRunning means the run is no longer running and cannot be reaped.
	ErrCronRunNotRunning = errors.New("cron run is not running")
	// ErrCronRunNotStale means the run has not reached its stale deadline and cannot be reaped.
	ErrCronRunNotStale = errors.New("cron run is not stale")
)

// CronRunNotStaleError retains the evaluated deadline for an operator conflict.
type CronRunNotStaleError struct{ StaleAt time.Time }

func (e *CronRunNotStaleError) Error() string { return ErrCronRunNotStale.Error() }
func (e *CronRunNotStaleError) Unwrap() error { return ErrCronRunNotStale }

// ReapRun atomically checks current evidence and policy, then emits an ordinary
// observation attributed to the operator while keeping the starter's identity.
func (a CronSQL) ReapRun(ctx context.Context, in ReapCronRun) (CronRunObservationResult, error) {
	in.UID = strings.ToUpper(in.UID)
	in.Now = in.Now.UTC()
	// PostgreSQL JSONB cannot store U+0000. Keep new operator input portable
	// across backends without changing the existing observation format.
	if strings.ContainsRune(in.Actor, 0) || strings.ContainsRune(in.Message, 0) {
		return CronRunObservationResult{}, fmt.Errorf("%w: operator actor and message cannot contain U+0000", cron.ErrInvalid)
	}
	if !cronUID(in.UID) || in.ProjectID <= 0 || in.ExpectedRevision < 1 || strings.TrimSpace(in.Actor) == "" || len(in.Actor) > 256 || !utf8.ValidString(in.Actor) || len(in.Message) > 1024 || !utf8.ValidString(in.Message) || in.Now.IsZero() || in.Now.Year() < 0 || in.Now.Year() > 9999 {
		return CronRunObservationResult{}, fmt.Errorf("%w: invalid reap request", cron.ErrInvalid)
	}
	var out CronRunObservationResult
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		project, err := a.openCronProject(ctx, tx, in.ProjectID)
		if err != nil {
			return err
		}
		actor, err := a.effectiveMutationActor(ctx, tx, in.ProjectID, in.Actor)
		if err != nil {
			return err
		}
		value, err := (CronSQL{Query: tx}).Run(ctx, in.ProjectID, in.UID)
		if err != nil {
			return err
		}
		if value.Revision != in.ExpectedRevision {
			return ErrCronConflict
		}
		if value.Status != "running" {
			return ErrCronRunNotRunning
		}
		var timeout int64
		if value.JobUID != nil {
			job, err := (CronSQL{Query: tx}).Job(ctx, in.ProjectID, *value.JobUID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			timeout = job.Definition.TimeoutSeconds
		}
		verdict := cron.Staleness(value.Status, value.UpdatedAt, timeout, in.Now)
		if !verdict.Stale && !in.Force {
			return &CronRunNotStaleError{StaleAt: verdict.StaleAt}
		}
		value.Summary, err = reapedSummary(value.Summary, actor, in.Now, value.UpdatedAt, verdict.StaleAt, in.Message)
		if err != nil {
			return err
		}
		value.Status, value.UpdatedAt, value.Revision = "unknown", in.Now, value.Revision+1
		if err := validateCronRun(value); err != nil {
			return err
		}
		event, err := a.insertCronRunEvent(ctx, tx, project, value, actor)
		if err != nil {
			return err
		}
		if err := writeCronRunRow(ctx, tx, value, false, in.ExpectedRevision, in.Now); err != nil {
			return err
		}
		out = CronRunObservationResult{Run: value, Events: []Event{event}}
		return nil
	})
	if err != nil {
		return CronRunObservationResult{}, err
	}
	return out, nil
}

func reapedSummary(old cron.Summary, actor string, now, updated, staleAt time.Time, message string) (cron.Summary, error) {
	prefix := fmt.Sprintf("Marked unknown by %s at %s: no run evidence since %s (stale at %s).", actor, now.Format(time.RFC3339Nano), updated.Format(time.RFC3339Nano), staleAt.Format(time.RFC3339Nano))
	if message != "" {
		prefix += "\n" + message
	}
	if old.Message != "" {
		prefix += "\nPrevious: " + old.Message
	}
	// Bound the encoded summary, including JSON escaping and token counts.
	// Binary search keeps this linear in message size times a small log factor.
	lo, hi := 0, len(prefix)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		cut := mid
		for cut > 0 && cut < len(prefix) && !utf8.RuneStart(prefix[cut]) {
			cut--
		}
		old.Message = prefix[:cut]
		raw, err := json.Marshal(old)
		if err != nil {
			return old, err
		}
		if len(raw) <= cron.SummaryLimit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	for lo > 0 && lo < len(prefix) && !utf8.RuneStart(prefix[lo]) {
		lo--
	}
	old.Message = prefix[:lo]
	return old, nil
}
