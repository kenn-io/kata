package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/notification"
)

// RecurringDelivery describes durable evidence, evaluated locally without writes.
type RecurringDelivery struct {
	State        string                    `json:"state"`
	Reason       string                    `json:"reason,omitempty"`
	Error        string                    `json:"error,omitempty"`
	Authority    string                    `json:"authority"`
	DeliversHere bool                      `json:"delivers_here"`
	EvaluatedAt  time.Time                 `json:"evaluated_at"`
	Anchor       *time.Time                `json:"anchor,omitempty"`
	Through      *time.Time                `json:"through,omitempty"`
	Scan         *RecurringDeliveryScan    `json:"scan,omitempty"`
	NextDueAt    *time.Time                `json:"next_due_at,omitempty"`
	Pending      *RecurringDeliveryPending `json:"pending,omitempty"`
	LastChild    *RecurringChild           `json:"last_child,omitempty"`
}

// RecurringDeliveryScan exposes the durable scan cursor, without internal fencing.
type RecurringDeliveryScan struct {
	Until  time.Time `json:"until"`
	Cursor time.Time `json:"cursor"`
}

// RecurringDeliveryPending is a bounded hypothetical next sweep.
type RecurringDeliveryPending struct {
	Count  int        `json:"count"`
	More   bool       `json:"more"`
	Oldest *time.Time `json:"oldest,omitempty"`
}

// RecurringChild is visible occurrence evidence. Ordering uses ScheduledAt then UID.
type RecurringChild struct {
	UID         string    `json:"uid"`
	ScheduledAt time.Time `json:"scheduled_at"`
}

var ErrRecurringOccurrenceVersion = errors.New("unsupported recurring occurrence version")

// LastRecurringChild consumes authorized child rows, retaining only the newest
// supported occurrence. Only the returned evidence participates in response release.
func LastRecurringChild(ctx context.Context, rows *sql.Rows) (*RecurringChild, error) {
	defer func() { _ = rows.Close() }()
	var latest *RecurringChild
	var latestID int64
	for rows.Next() {
		var id int64
		var uid, occurrence string
		if err := rows.Scan(&id, &uid, &occurrence); err != nil {
			return nil, err
		}
		var header struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal([]byte(occurrence), &header); err != nil {
			continue
		}
		if header.Version > 1 {
			return nil, ErrRecurringOccurrenceVersion
		}
		var value recurringOccurrence
		if err := json.Unmarshal([]byte(occurrence), &value); err != nil {
			continue
		}
		if value.Version != 1 || value.ScheduledAt.IsZero() {
			continue
		}
		child := RecurringChild{UID: uid, ScheduledAt: value.ScheduledAt.UTC()}
		if latest == nil || child.ScheduledAt.After(latest.ScheduledAt) || (child.ScheduledAt.Equal(latest.ScheduledAt) && child.UID > latest.UID) {
			latest, latestID = &child, id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if latest != nil {
		RecordExposedIssue(ctx, latestID)
	}
	return latest, nil
}

func recurringRecipient(j cron.JobDefinition, parent Issue) (string, error) {
	recipient := j.Action.Recipient
	if recipient == "current-owner-or-author" {
		recipient = parent.Author
		if parent.Owner != nil && *parent.Owner != "" {
			recipient = *parent.Owner
		}
	}
	return notification.NormalizeRecipient(recipient)
}

type CronDeliveryQuery struct {
	CronList
	JobUID string
}

type CronDeliveryJob struct {
	Job      CronJob
	Delivery *RecurringDelivery
}

// RecurringDelivery selects jobs and evaluates their delivery in the same read.
func (a CronSQL) RecurringDelivery(ctx context.Context, query CronDeliveryQuery, now time.Time) ([]CronDeliveryJob, error) {
	var jobs []CronJob
	if query.JobUID != "" {
		job, err := a.Job(ctx, query.ProjectID, query.JobUID)
		if err != nil {
			return nil, err
		}
		jobs = []CronJob{job}
	} else {
		var err error
		jobs, err = a.Jobs(ctx, query.CronList)
		if err != nil {
			return nil, err
		}
	}
	projectID := query.ProjectID
	out := make([]CronDeliveryJob, len(jobs))
	for i, job := range jobs {
		out[i].Job = job
	}
	native := false
	for _, j := range jobs {
		native = native || cron.IsRecurringIssueJob(j.Definition)
	}
	if !native {
		return out, nil
	}
	delivers, role, err := recurringAuthority(ctx, a.Query, projectID)
	if err != nil {
		return nil, err
	}
	authority := "spoke"
	switch role {
	case "":
		authority = "local"
	case FederationRoleHub:
		authority = "hub_disabled"
		if delivers {
			authority = "hub"
		}
	}
	now = now.UTC()
	for i, j := range jobs {
		if !cron.IsRecurringIssueJob(j.Definition) {
			continue
		}
		d := &RecurringDelivery{State: "active", Authority: authority, DeliversHere: delivers, EvaluatedAt: now}
		out[i].Delivery = d
		// Unlike dispatch this projection must not lock the parent.
		parent, deleted, err := a.recurringParent(ctx, a.Query, projectID, j.Definition.Issue.ParentUID, false)
		missing := errors.Is(err, ErrNotFound)
		if err != nil && !missing {
			return nil, err
		}
		switch {
		case authority == "hub_disabled":
			d.Reason = "not_owning_hub"
		case j.DeletedAt != nil:
			d.Reason = "job_deleted"
		case !j.Definition.Enabled:
			d.Reason = "disabled"
		case missing:
			d.Reason = "parent_missing"
		case deleted:
			d.Reason = "parent_deleted"
		case parent.Status != "open":
			d.Reason = "parent_not_open"
		}
		if !missing {
			RecordExposedIssue(ctx, parent.ID)
			progress, evalErr := readRecurringProgress(parent, j.UID)
			if evalErr == nil {
				d.Anchor = &progress.Anchor
				d.Through = &progress.Through
				if progress.Scan != nil {
					d.Scan = &RecurringDeliveryScan{Until: progress.Scan.Until, Cursor: progress.Scan.Cursor}
				}
				var desc cron.RecurringDescription
				desc, evalErr = cron.DescribeRecurring(j.Definition, j.DefinitionEventUID, progress, now)
				if evalErr == nil {
					if len(desc.Batch.Due) > 0 || desc.Batch.More {
						d.Pending = &RecurringDeliveryPending{Count: len(desc.Batch.Due), More: desc.Batch.More}
						if len(desc.Batch.Due) > 0 {
							d.Pending.Oldest = &desc.Batch.Due[0]
						}
					}
					_, evalErr = recurringRecipient(j.Definition, parent)
					if evalErr != nil {
						evalErr = fmt.Errorf("%w: %v", cron.ErrInvalid, evalErr)
					} else {
						d.NextDueAt = &desc.NextDueAt
					}
				}
			}
			if evalErr != nil {
				d.Error = evalErr.Error()
			}
			d.LastChild, err = a.RecurringChild(ctx, parent.ID, j.UID)
			if errors.Is(err, ErrRecurringOccurrenceVersion) {
				d.Error = err.Error()
			} else if err != nil {
				return nil, err
			}
		}
		switch {
		case d.Reason != "":
			d.State = "paused"
		case d.Error != "":
			d.State = "error"
		case d.NextDueAt != nil && !d.NextDueAt.After(now.Add(-cron.RecurringBehindAfter)):
			d.State = "behind"
		}
	}
	return out, nil
}
