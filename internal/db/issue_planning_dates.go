package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/metadata"
)

// ErrPlanningDatesInvalid reports malformed native planning-date metadata.
var ErrPlanningDatesInvalid = errors.New("invalid issue planning dates")

// IssuePlanningDate retains the raw source value and native resolved UTC instant.
type IssuePlanningDate struct {
	Field    string    `json:"field"`
	Value    string    `json:"value"`
	Timezone string    `json:"timezone"`
	Instant  time.Time `json:"instant"`
}

// IssuePlanningDates contains nullable dates from one consistent issue revision.
type IssuePlanningDates struct {
	ProjectID   int64              `json:"project_id"`
	IssueUID    string             `json:"issue_uid"`
	Revision    int64              `json:"revision"`
	ScheduledOn *IssuePlanningDate `json:"scheduled_on"`
	DeadlineOn  *IssuePlanningDate `json:"deadline_on"`
}

// IssuePlanningDatesIn scopes the read and supplies the daemon timezone fallback.
type IssuePlanningDatesIn struct {
	ProjectID       int64
	IssueID         int64
	IncludeDeleted  bool
	DefaultTimezone string
}

// ReadIssuePlanningDatesSQL captures identity, revision, both metadata fields
// and recurrence timezone in one scoped statement. No live clock is evaluated.
func ReadIssuePlanningDatesSQL(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, in IssuePlanningDatesIn) (IssuePlanningDates, error) {
	out := IssuePlanningDates{ProjectID: in.ProjectID}
	var raw string
	var recurrence sql.NullString
	err := q.QueryRowContext(ctx, `SELECT i.uid,i.revision,i.metadata,r.timezone
 FROM issues i JOIN projects p ON p.id=i.project_id
 LEFT JOIN recurrences r ON r.id=i.recurrence_id AND r.project_id=i.project_id
 WHERE i.id=$1 AND i.project_id=$2 AND p.deleted_at IS NULL AND ($3 OR i.deleted_at IS NULL)`, in.IssueID, in.ProjectID, in.IncludeDeleted).Scan(&out.IssueUID, &out.Revision, &raw, &recurrence)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		fallback := in.DefaultTimezone
		if field == "scheduled_on" && recurrence.Valid && recurrence.String != "" {
			fallback = recurrence.String
		}
		value, timezone, instant, present, err := metadata.ScheduleFieldInstant(raw, field, fallback)
		if err != nil {
			return IssuePlanningDates{}, fmt.Errorf("%w: %v", ErrPlanningDatesInvalid, err)
		}
		if !present {
			continue
		}
		date := &IssuePlanningDate{Field: field, Value: value, Timezone: timezone, Instant: instant.UTC()}
		if field == "scheduled_on" {
			out.ScheduledOn = date
		} else {
			out.DeadlineOn = date
		}
	}
	return out, nil
}
