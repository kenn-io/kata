package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// planningDateResolver projects issue metadata into the scheduled_on_date and
// deadline_on_date row fields with db.RowPlanningDates, the rule the browser
// snapshot shares. It memoizes recurrence timezones for one response.
type planningDateResolver struct {
	store              db.Storage
	defaultTimezone    string
	recurrenceTimezone map[int64]string
	now                time.Time
}

func newPlanningDateResolver(store db.Storage, defaultTimezone string) *planningDateResolver {
	return &planningDateResolver{store: store, defaultTimezone: defaultTimezone, recurrenceTimezone: map[int64]string{}, now: time.Now()}
}

// resolve omits a field whose stored value is malformed; it fails when the
// recurrence timezone lookup fails or a saved schedule's tzdata is unsupported.
func (r *planningDateResolver) resolve(ctx context.Context, issue db.Issue) (scheduledOn, deadlineOn, scheduledToday, deadlineToday, scheduledDisplay, deadlineDisplay string, err error) {
	raw := string(issue.Metadata)
	if raw == "" {
		return "", "", "", "", "", "", nil
	}
	scheduledDefault := r.defaultTimezone
	if issue.RecurrenceID != nil {
		zone, ok := r.recurrenceTimezone[*issue.RecurrenceID]
		if !ok {
			recurrence, err := r.store.GetRecurrenceByID(ctx, *issue.RecurrenceID)
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return "", "", "", "", "", "", fmt.Errorf("read recurrence timezone for issue %s: %w", issue.UID, err)
			}
			zone = recurrence.Timezone
			r.recurrenceTimezone[*issue.RecurrenceID] = zone
		}
		if zone != "" {
			scheduledDefault = zone
		}
	}
	return db.RowPlanningProjection(raw, scheduledDefault, r.defaultTimezone, r.now)
}

// scheduleReadError reports a saved schedule this build cannot read as a client
// error naming the cause, since only a newer build or a rewrite can resolve it.
func scheduleReadError(err error) error {
	if !metadata.IsUnsupportedSchedule(err) {
		return internalAPIError(err)
	}
	return api.NewError(http.StatusBadRequest, "unsupported_schedule", err.Error(), "update kata, or clear the unreadable date", nil)
}

// applyRow sets a list row's planning dates. A saved schedule this build cannot
// read is reported on that row's schedule_error, so it never hides other rows.
func (r *planningDateResolver) applyRow(ctx context.Context, issue db.Issue, row *api.IssueOut) error {
	scheduledOn, deadlineOn, scheduledToday, deadlineToday, _, _, err := r.resolve(ctx, issue)
	if err != nil {
		if !metadata.IsUnsupportedSchedule(err) {
			return err
		}
		row.ScheduleError = err.Error()
	}
	row.ScheduledOnDate, row.DeadlineOnDate = scheduledOn, deadlineOn
	row.ScheduledOnToday, row.DeadlineOnToday = scheduledToday, deadlineToday
	return nil
}
