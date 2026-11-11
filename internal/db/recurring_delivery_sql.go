package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
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

var errRecurringOccurrenceVersion = errors.New("unsupported recurring occurrence version")

func decodeRecurringOccurrence(occurrence string) (*recurringOccurrence, error) {
	var header struct {
		Version jsontext.Value `json:"version"`
	}
	if err := json.Unmarshal([]byte(occurrence), &header); err != nil {
		return nil, nil
	}
	version, ok := new(big.Int).SetString(string(header.Version), 10)
	if ok && version.Cmp(big.NewInt(1)) > 0 {
		return nil, errRecurringOccurrenceVersion
	}
	var value recurringOccurrence
	if err := json.Unmarshal([]byte(occurrence), &value); err != nil || value.Version != 1 || value.ScheduledAt.IsZero() {
		return nil, nil
	}
	return &value, nil
}

// LastRecurringChild finds the first valid authorized row in occurrence-key order.
func LastRecurringChild(ctx context.Context, rows *sql.Rows) (*RecurringChild, error) {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var uid, occurrence, key string
		if err := rows.Scan(&id, &uid, &occurrence, &key); err != nil {
			return nil, err
		}
		value, err := decodeRecurringOccurrence(occurrence)
		if err != nil || value != nil {
			RecordExposedIssue(ctx, id)
			if err != nil {
				return nil, err
			}
			stamp := value.ScheduledAt.UTC()
			// Keep the original offset when UTC would exceed JSON's year range.
			if stamp.Year() < 0 || stamp.Year() > 9999 {
				stamp = value.ScheduledAt
			}
			return &RecurringChild{UID: uid, ScheduledAt: stamp}, nil
		}
	}
	return nil, rows.Err()
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

// CronDeliveryQuery selects jobs whose delivery status is read together.
type CronDeliveryQuery struct {
	CronList
	JobUID string
}

// CronDeliveryJob pairs a job with its delivery status.
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
	out := make([]CronDeliveryJob, len(jobs))
	native := false
	for i, j := range jobs {
		out[i].Job = j
		native = native || cron.IsRecurringIssueJob(j.Definition)
	}
	if !native {
		return out, nil
	}
	delivers, role, err := recurringAuthority(ctx, a.Query, query.ProjectID)
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
		parent, deleted, err := a.recurringParent(ctx, a.Query, query.ProjectID, j.Definition.Issue.ParentUID, false)
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
			if errors.Is(err, errRecurringOccurrenceVersion) {
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
