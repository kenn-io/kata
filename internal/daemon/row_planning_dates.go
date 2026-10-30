package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// planningDateResolver projects issue metadata into the scheduled_on_date and
// deadline_on_date row fields with db.RowPlanningDates, the rule the browser
// snapshot shares. It memoizes recurrence timezones for one response.
type planningDateResolver struct {
	store              db.Storage
	defaultTimezone    string
	recurrenceTimezone map[int64]string
}

func newPlanningDateResolver(store db.Storage, defaultTimezone string) *planningDateResolver {
	return &planningDateResolver{store: store, defaultTimezone: defaultTimezone, recurrenceTimezone: map[int64]string{}}
}

func (r *planningDateResolver) resolve(ctx context.Context, issue db.Issue) (scheduledOn, deadlineOn string, err error) {
	raw := string(issue.Metadata)
	if raw == "" {
		return "", "", nil
	}
	scheduledDefault := r.defaultTimezone
	if issue.RecurrenceID != nil {
		zone, ok := r.recurrenceTimezone[*issue.RecurrenceID]
		if !ok {
			recurrence, err := r.store.GetRecurrenceByID(ctx, *issue.RecurrenceID)
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return "", "", fmt.Errorf("read recurrence timezone for issue %s: %w", issue.UID, err)
			}
			zone = recurrence.Timezone
			r.recurrenceTimezone[*issue.RecurrenceID] = zone
		}
		if zone != "" {
			scheduledDefault = zone
		}
	}
	scheduledOn, deadlineOn, err = db.RowPlanningDates(raw, scheduledDefault, r.defaultTimezone)
	if err != nil {
		return "", "", fmt.Errorf("read issue %s planning dates: %w", issue.UID, err)
	}
	return scheduledOn, deadlineOn, nil
}
