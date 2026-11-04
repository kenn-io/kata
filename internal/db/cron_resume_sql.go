package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// ResumeCronJobIn retains the selected job revision across a discard retry.
type ResumeCronJobIn struct {
	ProjectID                    int64
	UID, ExpectedEventUID, Actor string
	DiscardMissed                bool
}

// ResumeCronJobOut retains the job and exact committed events.
type ResumeCronJobOut struct {
	Job    CronJob
	Events []Event
}

// cronPassoverPatch retains the complete later value. It repairs only the
// selected scope; a malformed project cutoff requires project-level repair.
func cronPassoverPatch(project Project, key, actor string, now time.Time, pausedAt *time.Time) (map[string]jsontext.Value, error) {
	var values map[string]jsontext.Value
	if project.Metadata != "" {
		if err := json.Unmarshal([]byte(project.Metadata), &values); err != nil {
			return nil, err
		}
	}
	previous, err := decodeCronPassover(values[key])
	if err == nil && !previous.Through.Before(now) {
		return nil, nil
	}
	value := CronPassover{Version: 1, Through: now, By: actor, PausedAt: pausedAt}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if _, err = decodeCronPassover(raw); err != nil {
		return nil, err
	}
	return map[string]jsontext.Value{key: raw}, nil
}

// ResumeJob commits policy before enable in one project-serialized transaction.
func (a CronSQL) ResumeJob(ctx context.Context, in ResumeCronJobIn) (ResumeCronJobOut, error) {
	var out ResumeCronJobOut
	if strings.TrimSpace(in.Actor) == "" || len(in.Actor) > 256 || (in.DiscardMissed && in.ExpectedEventUID == "") {
		return out, fmt.Errorf("%w: actor and retained expected_event_uid required for discard", cron.ErrInvalid)
	}
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		out = ResumeCronJobOut{}
		if in.DiscardMissed {
			if err := LockCronProject(ctx, tx, in.ProjectID); err != nil {
				return err
			}
			if err := requireCronRootTx(ctx, tx, in.ProjectID); err != nil {
				return err
			}
		}
		project, err := a.openCronProject(ctx, tx, in.ProjectID)
		if err != nil {
			return err
		}
		row, err := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM cron_jobs WHERE project_id=$1 AND uid=$2", in.ProjectID, strings.ToUpper(in.UID)))
		if err != nil {
			return err
		}
		current, err := jobFromRow(row)
		if err != nil {
			return err
		}
		if current.DeletedAt != nil || (in.ExpectedEventUID != "" && in.ExpectedEventUID != current.DefinitionEventUID) {
			return ErrCronConflict
		}
		actor, err := a.effectiveMutationActor(ctx, tx, in.ProjectID, in.Actor)
		if err != nil {
			return err
		}
		out.Job = current
		if in.DiscardMissed {
			// An enabled job's valid cutoff makes a repeated selected-revision action
			// event-free. Malformed state still has an explicit repair path.
			var values map[string]jsontext.Value
			if project.Metadata != "" {
				if err := json.Unmarshal([]byte(project.Metadata), &values); err != nil {
					return err
				}
			}
			_, validErr := decodeCronPassover(values[CronPassoverMetadataPrefix+current.UID])
			_, present := values[CronPassoverMetadataPrefix+current.UID]
			if !current.Definition.Enabled || (present && validErr != nil) {
				patch, err := cronPassoverPatch(project, CronPassoverMetadataPrefix+current.UID, actor, time.Now().UTC(), nil)
				if err != nil {
					return err
				}
				if len(patch) > 0 {
					policy, err := a.writeCronProjectMetadataTx(ctx, tx, project, actor, patch)
					if err != nil {
						return err
					}
					if policy.Changed {
						out.Events = append(out.Events, policy.Event)
					}
				}
			}
		}
		if current.Definition.Enabled {
			return nil
		}
		definition := current.Definition
		definition.Enabled = true
		if err := definition.Validate(); err != nil {
			return err
		}
		document, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		var extra []Event
		next, event, err := a.putTx(ctx, tx, cronPutRequest{job: true, id: current.UID, projectID: in.ProjectID, name: current.Name, expected: current.DefinitionEventUID, actor: actor, definition: definition, document: document}, &extra)
		if err != nil {
			return err
		}
		out.Job, err = jobFromRow(next)
		if err != nil {
			return err
		}
		out.Events = append(out.Events, event)
		out.Events = append(out.Events, extra...)
		return nil
	})
	if err != nil {
		return ResumeCronJobOut{}, err
	}
	return out, nil
}
