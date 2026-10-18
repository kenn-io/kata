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

// RecurringProgressOf reads the native checkpoint using dispatch's validation.
func RecurringProgressOf(parent Issue, jobUID string) (cron.RecurringProgress, error) {
	return readRecurringProgress(parent, jobUID)
}

// RecurringChildFrom parses only the occurrence timestamp from selected evidence.
func RecurringChildFrom(uid, occurrence string) (RecurringChild, error) {
	var value struct {
		ScheduledAt time.Time `json:"scheduled_at"`
	}
	if err := json.Unmarshal([]byte(occurrence), &value); err != nil {
		return RecurringChild{}, err
	}
	return RecurringChild{UID: uid, ScheduledAt: value.ScheduledAt.UTC()}, nil
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

func (a CronSQL) recurringDeliveryAuthority(ctx context.Context, projectID int64) (string, error) {
	var role string
	var enabled int
	err := a.Query.QueryRowContext(ctx, `SELECT role,enabled FROM federation_bindings WHERE project_id=$1`, projectID).Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "local", nil
	}
	if err != nil {
		return "", err
	}
	if role == string(FederationRoleHub) {
		if enabled == 1 {
			return "hub", nil
		}
		return "hub_disabled", nil
	}
	return "spoke", nil
}

// RecurringDelivery returns one status per input; adapter entries remain nil.
func (a CronSQL) RecurringDelivery(ctx context.Context, projectID int64, jobs []CronJob, now time.Time) ([]*RecurringDelivery, error) {
	out := make([]*RecurringDelivery, len(jobs))
	native := false
	for _, j := range jobs {
		native = native || cron.IsRecurringIssueJob(j.Definition)
	}
	if !native {
		return out, nil
	}
	authority, err := a.recurringDeliveryAuthority(ctx, projectID)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	for i, j := range jobs {
		if !cron.IsRecurringIssueJob(j.Definition) {
			continue
		}
		d := &RecurringDelivery{State: "active", Authority: authority, DeliversHere: authority == "local" || authority == "hub", EvaluatedAt: now}
		out[i] = d
		var parent Issue
		var deleted bool
		// Unlike dispatch this is a read-only projection: never lock the parent.
		err := a.Query.QueryRowContext(ctx, `SELECT id,uid,status,owner,author,metadata,revision,deleted_at IS NOT NULL FROM issues WHERE uid=$1 AND project_id=$2`, j.Definition.Issue.ParentUID, projectID).Scan(&parent.ID, &parent.UID, &parent.Status, &parent.Owner, &parent.Author, &parent.Metadata, &parent.Revision, &deleted)
		missing := errors.Is(err, sql.ErrNoRows)
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
			progress, evalErr := RecurringProgressOf(parent, j.UID)
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
			children, queryErr := a.RecurringChildren(ctx, parent.ID, j.UID)
			if queryErr != nil {
				return nil, queryErr
			}
			for _, child := range children {
				if child.ScheduledAt.IsZero() {
					continue
				}
				if d.LastChild == nil || child.ScheduledAt.After(d.LastChild.ScheduledAt) || (child.ScheduledAt.Equal(d.LastChild.ScheduledAt) && child.UID > d.LastChild.UID) {
					copy := child
					d.LastChild = &copy
				}
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
