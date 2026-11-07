package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

// Request admits a new immutable root request or returns a retained exact retry.
func (a CronSQL) Request(ctx context.Context, in RequestCronIn) (RequestCronOut, error) {
	var result RequestCronOut
	if !cronUID(in.UID) || !cronUID(in.TargetUID) || strings.TrimSpace(in.Actor) == "" || len(in.Actor) > 256 {
		return result, fmt.Errorf("%w: request UID, target UID and actor are required", cron.ErrInvalid)
	}
	if in.Kind != CronRequestKindRun && in.Kind != CronRequestKindCancel {
		return result, fmt.Errorf("%w: unknown request kind", cron.ErrInvalid)
	}
	if in.TTLSeconds == 0 {
		in.TTLSeconds = CronRequestDefaultTTL
	}
	if in.TTLSeconds < 60 || in.TTLSeconds > 86400 {
		return result, fmt.Errorf("%w: ttl_seconds must be 60..86400", cron.ErrInvalid)
	}
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		result = RequestCronOut{}
		if err := LockCronProject(ctx, tx, in.ProjectID); err != nil {
			return err
		}
		if err := requireCronRootTx(ctx, tx, in.ProjectID); err != nil {
			return err
		}
		project, err := a.openCronProject(ctx, tx, in.ProjectID)
		if err != nil {
			return err
		}
		actor, err := a.effectiveMutationActor(ctx, tx, in.ProjectID, in.Actor)
		if err != nil {
			return err
		}
		values, err := cronRequestMetadataValues(string(project.Metadata))
		if err != nil {
			return err
		}
		key := CronRequestMetadataPrefix + in.UID
		if raw, exists := values[key]; exists {
			retained, err := decodeCronRequest(in.UID, raw)
			if err != nil {
				return fmt.Errorf("%w: malformed retained request", ErrCronRequestConflict)
			}
			target := retained.JobUID
			if retained.Kind == CronRequestKindCancel && retained.RunUID != nil {
				target = *retained.RunUID
			}
			if retained.Kind != in.Kind || target != in.TargetUID || retained.RequestedBy != actor || retained.ExpiresAt.Sub(retained.RequestedAt) != time.Duration(in.TTLSeconds)*time.Second {
				return ErrCronRequestConflict
			}
			result = RequestCronOut{Request: retained, Replayed: true}
			return nil
		}
		// Sample after project locking on each attempt: queue/lock waits must
		// count toward freshness. Retained replay needs no current clock.
		now := in.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		if err := validateCronRequestFreshness(in.UID, now); err != nil {
			return err
		}
		request := CronRequest{UID: in.UID, Kind: in.Kind, RequestedBy: actor, RequestedAt: uid.MustTime(in.UID)}
		request.ExpiresAt = request.RequestedAt.Add(time.Duration(in.TTLSeconds) * time.Second)
		switch in.Kind {
		case CronRequestKindRun:
			row, err := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM cron_jobs WHERE project_id=$1 AND uid=$2", in.ProjectID, in.TargetUID))
			if err != nil {
				return err
			}
			job, err := jobFromRow(row)
			if err != nil {
				return err
			}
			if cron.IsRecurringIssueJob(job.Definition) || job.Definition.Action.Kind != "execute" {
				return fmt.Errorf("%w: run requests require an adapter execute job", cron.ErrInvalid)
			}
			if job.DeletedAt != nil || !job.Definition.Enabled || CronProjectPauseFromMetadata(project.Metadata) != nil {
				return ErrCronRequestUnavailable
			}
			request.JobUID, request.DefinitionEventUID = job.UID, job.DefinitionEventUID
		case CronRequestKindCancel:
			run, err := scanCronRunRow(tx.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE project_id=$1 AND uid=$2", in.ProjectID, in.TargetUID))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if run.Status != "running" {
				return ErrCronRequestUnavailable
			}
			request.RunUID = new(in.TargetUID)
			if run.JobUID != nil && run.DefinitionEventUID != nil {
				request.JobUID, request.DefinitionEventUID = *run.JobUID, *run.DefinitionEventUID
			}
		}
		live := 0
		for key, raw := range values {
			if !strings.HasPrefix(key, CronRequestMetadataPrefix) {
				continue
			}
			retained, err := decodeCronRequest(strings.TrimPrefix(key, CronRequestMetadataPrefix), raw)
			if err != nil {
				return fmt.Errorf("%w: malformed retained request %s", cron.ErrInvalid, key)
			}
			if now.Before(retained.ExpiresAt) {
				live++
			}
		}
		if live >= CronRequestLimit {
			return ErrCronRequestCapacity
		}
		raw, err := encodeCronRequest(request)
		if err != nil {
			return err
		}
		// Reuse strict ingest validation on every locally generated value.
		if _, err := decodeCronRequest(in.UID, raw); err != nil {
			return err
		}
		written, err := a.writeCronProjectMetadataTx(ctx, tx, project, actor, map[string]jsontext.Value{key: raw})
		if err != nil {
			return err
		}
		result = RequestCronOut{Request: request, Event: &written.Event}
		return nil
	})
	if err != nil {
		return RequestCronOut{}, err
	}
	return result, nil
}

// Requests derives state from a consistent metadata/evidence snapshot. Replicas
// may read their local snapshot without acquiring root mutation authority.
func (a CronSQL) Requests(ctx context.Context, in CronRequestList) ([]CronRequest, error) {
	if in.State != "" && in.State != "pending" && in.State != "answered" && in.State != "expired" {
		return nil, fmt.Errorf("%w: unknown request state", cron.ErrInvalid)
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	var result []CronRequest
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		result = []CronRequest{}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT metadata FROM projects WHERE id=$1 AND deleted_at IS NULL AND name<>$2`, in.ProjectID, SystemProjectName).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		values, err := cronRequestMetadataValues(raw)
		if err != nil {
			return err
		}
		for key, value := range values {
			if !strings.HasPrefix(key, CronRequestMetadataPrefix) {
				continue
			}
			request, err := decodeCronRequest(strings.TrimPrefix(key, CronRequestMetadataPrefix), value)
			if err != nil {
				return fmt.Errorf("request %s: %w", key, err)
			}
			var answered bool
			if request.Kind == CronRequestKindRun {
				err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cron_runs WHERE project_id=$1 AND job_uid=$2 AND definition_event_uid=$3 AND occurrence_key=$4)`, in.ProjectID, request.JobUID, request.DefinitionEventUID, "request:"+request.UID).Scan(&answered)
			} else {
				err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cron_runs WHERE project_id=$1 AND uid=$2 AND status<>$3)`, in.ProjectID, *request.RunUID, "running").Scan(&answered)
			}
			if err != nil {
				return err
			}
			request.State = cronRequestReadState(answered, request.ExpiresAt, in.Now)
			if in.State == "" || request.State == in.State {
				result = append(result, request)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UID < result[j].UID })
	return result, nil
}

func cronRequestMetadataValues(raw string) (map[string]jsontext.Value, error) {
	values := map[string]jsontext.Value{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return nil, err
		}
	}
	for key, value := range values {
		if strings.HasPrefix(key, CronRequestMetadataPrefix) && value.Kind() == jsontext.KindNull {
			delete(values, key)
		}
	}
	return values, nil
}

// RequestProjects finds candidates only. SweepRequests rechecks every decision
// under the project lock so stale candidates never grant deletion authority.
func (a CronSQL) RequestProjects(ctx context.Context) ([]int64, error) {
	rows, err := a.Query.QueryContext(ctx, `SELECT id FROM projects WHERE deleted_at IS NULL AND name<>$1 AND metadata LIKE $2 ORDER BY id`, SystemProjectName, "%cron.request.%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SweepRequests expires valid requests and deletes malformed legacy values.
// Malformed values are reported with their exact cleanup event. Callers
// publish that event before handling the error or starting another project.
func (a CronSQL) SweepRequests(ctx context.Context, projectID int64, now time.Time) (*Event, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var event *Event
	var malformed []error
	err := a.transactProject(ctx, projectID, func(tx *sql.Tx) error {
		event = nil
		malformed = nil
		if err := LockCronProject(ctx, tx, projectID); errors.Is(err, ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if err := requireCronRootTx(ctx, tx, projectID); errors.Is(err, ErrFederatedSpokeUnsupported) {
			return nil
		} else if err != nil {
			return err
		}
		project, err := a.openCronProject(ctx, tx, projectID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		values, err := cronRequestMetadataValues(string(project.Metadata))
		if err != nil {
			return err
		}
		patch := map[string]jsontext.Value{}
		for key, raw := range values {
			if !strings.HasPrefix(key, CronRequestMetadataPrefix) {
				continue
			}
			request, err := decodeCronRequest(strings.TrimPrefix(key, CronRequestMetadataPrefix), raw)
			if err != nil {
				malformed = append(malformed, fmt.Errorf("request %s: %w", key, err))
			} else if now.Before(request.ExpiresAt) {
				continue
			}
			patch[key] = jsontext.Value("null")
		}
		if len(patch) == 0 {
			return nil
		}
		written, err := a.writeCronProjectMetadataTx(ctx, tx, project, "system", patch)
		if err != nil {
			return err
		}
		if written.Changed {
			event = &written.Event
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return event, errors.Join(malformed...)
}
