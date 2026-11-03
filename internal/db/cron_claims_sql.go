package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/cron"
)

const cronClaimColumns = `project_id,job_uid,occurrence_key,run_uid,holder_instance_uid,actor,teammate,executor_label,definition_event_uid,workflow_uid,workflow_definition_event_uid,authority_instance_uid,created_at`

func scanCronClaim(row interface{ Scan(...any) error }) (CronOccurrenceClaim, error) {
	var c CronOccurrenceClaim
	var at string
	err := row.Scan(&c.ProjectID, &c.JobUID, &c.OccurrenceKey, &c.RunUID, &c.HolderInstanceUID, &c.Actor, &c.Teammate, &c.ExecutorLabel, &c.DefinitionEventUID, &c.WorkflowUID, &c.WorkflowDefinitionEventUID, &c.AuthorityInstanceUID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err == nil {
		c.CreatedAt, err = time.Parse(time.RFC3339Nano, at)
	}
	return c, err
}

// ValidateCronClaim is shared by native writes and owner-backup ingest.
func ValidateCronClaim(c CronOccurrenceClaim) error {
	if c.ProjectID <= 0 || !cronUID(c.JobUID) || !cronUID(c.RunUID) || !cronUID(c.DefinitionEventUID) || !cronUID(c.HolderInstanceUID) || !cronUID(c.AuthorityInstanceUID) || c.CreatedAt.IsZero() || c.CreatedAt.Year() < 0 || c.CreatedAt.Year() > 9999 {
		return fmt.Errorf("%w: invalid claim identity", cron.ErrInvalid)
	}
	if len(c.OccurrenceKey) == 0 || len(c.OccurrenceKey) > 1024 || !utf8.ValidString(c.OccurrenceKey) || strings.ContainsRune(c.OccurrenceKey, 0) {
		return fmt.Errorf("%w: occurrence key must be 1-1024 UTF-8 bytes", cron.ErrInvalid)
	}
	v := CronRun{ProjectID: c.ProjectID, UID: c.RunUID, JobUID: &c.JobUID, DefinitionEventUID: &c.DefinitionEventUID, WorkflowUID: c.WorkflowUID, WorkflowDefinitionEventUID: c.WorkflowDefinitionEventUID, Actor: c.Actor, Teammate: c.Teammate, ExecutorLabel: c.ExecutorLabel, Status: "running", Summary: cron.Summary{Version: 1}}
	if err := validateCronRun(v); err != nil {
		return err
	}
	for _, s := range []*string{&c.Actor, c.Teammate, c.ExecutorLabel} {
		if s != nil && strings.ContainsRune(*s, 0) {
			return fmt.Errorf("%w: NUL in claim label", cron.ErrInvalid)
		}
	}
	return nil
}

// Claim reads a retained decision without conferring execution permission.
func (a CronSQL) Claim(ctx context.Context, projectID int64, runUID string) (CronOccurrenceClaim, error) {
	return scanCronClaim(a.Query.QueryRowContext(ctx, "SELECT "+cronClaimColumns+" FROM cron_occurrence_claims WHERE project_id=$1 AND run_uid=$2", projectID, strings.ToUpper(runUID)))
}

// CronClaimAuthority rejects replica/copy namespaces even if their binding was removed.
// It reads only; capability discovery must never create coordination state.
func CronClaimAuthority(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectID int64, instanceUID string) (string, error) {
	var projectUID string
	if err := q.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL AND name<>$2`, projectID, SystemProjectName).Scan(&projectUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return "", err
	}
	var role string
	var enabled int
	err := q.QueryRowContext(ctx, `SELECT role,enabled FROM federation_bindings WHERE project_id=$1`, projectID).Scan(&role, &enabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err == nil && (role != string(FederationRoleHub) || enabled != 1) {
		return "", ErrCronClaimPending
	}
	var retained string
	err = q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, CronClaimAuthorityPrefix+projectUID).Scan(&retained)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err == nil && retained != instanceUID {
		return "", ErrCronClaimPending
	}
	var foreign bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_root_keys WHERE project_uid=$1 AND active=1 AND authority_uid<>$2)`, projectUID, instanceUID).Scan(&foreign)
	if err != nil {
		return "", err
	}
	if foreign {
		return "", ErrCronClaimPending
	}
	return instanceUID, nil
}

// ClaimOccurrence serializes exclusion with project edits and authority transitions.
func (a CronSQL) ClaimOccurrence(ctx context.Context, in CronOccurrenceClaim) (CronClaimResult, error) {
	in.RunUID = strings.ToUpper(in.RunUID)
	in.JobUID = strings.ToUpper(in.JobUID)
	in.DefinitionEventUID = strings.ToUpper(in.DefinitionEventUID)
	in.AuthorityInstanceUID = a.InstanceUID
	in.CreatedAt = time.Now().UTC()
	if err := ValidateCronClaim(in); err != nil {
		return CronClaimResult{}, err
	}
	var out CronClaimResult
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		if err := LockCronProject(ctx, tx, in.ProjectID); err != nil {
			return err
		}
		if _, err := CronClaimAuthority(ctx, tx, in.ProjectID, a.InstanceUID); err != nil {
			return err
		}
		project, err := a.openCronProject(ctx, tx, in.ProjectID)
		if err != nil {
			return err
		}
		current, err := scanCronClaim(tx.QueryRowContext(ctx, "SELECT "+cronClaimColumns+" FROM cron_occurrence_claims WHERE run_uid=$1", in.RunUID))
		if err == nil {
			if current.ProjectID != in.ProjectID {
				return ErrCronClaimIdentity
			}
			in.CreatedAt = current.CreatedAt
			in.AuthorityInstanceUID = current.AuthorityInstanceUID
			if !reflect.DeepEqual(current, in) {
				return ErrCronClaimIdentity
			}
			out = CronClaimResult{Claim: current, Replayed: true}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		current, err = scanCronClaim(tx.QueryRowContext(ctx, "SELECT "+cronClaimColumns+" FROM cron_occurrence_claims WHERE project_id=$1 AND job_uid=$2 AND occurrence_key=$3", in.ProjectID, in.JobUID, in.OccurrenceKey))
		if err == nil {
			out.Claim = current
			return ErrCronOccurrenceClaimed
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		job, err := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM cron_jobs WHERE project_id=$1 AND uid=$2", in.ProjectID, in.JobUID))
		if errors.Is(err, ErrNotFound) {
			return ErrCronClaimPending
		}
		if err != nil {
			return err
		}
		if job.DeletedAt != nil {
			return fmt.Errorf("%w: job is deleted", cron.ErrInvalid)
		}
		if job.DefinitionEventUID != in.DefinitionEventUID {
			return ErrCronClaimPending
		}
		parsed, err := jobFromRow(job)
		if err != nil {
			return err
		}
		if !parsed.Definition.Enabled || parsed.Definition.Action.Kind != "execute" {
			return fmt.Errorf("%w: claim requires an enabled execute job", cron.ErrInvalid)
		}
		if in.WorkflowUID != nil {
			wf, err := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM cron_workflows WHERE project_id=$1 AND uid=$2", in.ProjectID, *in.WorkflowUID))
			if errors.Is(err, ErrNotFound) {
				return ErrCronClaimPending
			}
			if err != nil {
				return err
			}
			if wf.DeletedAt != nil || wf.DefinitionEventUID != *in.WorkflowDefinitionEventUID {
				return ErrCronClaimPending
			}
		}
		// Owner metadata persists through full restore and blocks a different instance.
		_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, CronClaimAuthorityPrefix+project.UID, a.InstanceUID)
		if err != nil {
			return err
		}
		if err = insertCronClaim(ctx, tx, in); err != nil {
			return err
		}
		out.Claim = in
		return nil
	})
	return out, err
}

func insertCronClaim(ctx context.Context, tx *sql.Tx, c CronOccurrenceClaim) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO cron_occurrence_claims("+cronClaimColumns+") VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)", c.ProjectID, c.JobUID, c.OccurrenceKey, c.RunUID, c.HolderInstanceUID, c.Actor, c.Teammate, c.ExecutorLabel, c.DefinitionEventUID, c.WorkflowUID, c.WorkflowDefinitionEventUID, c.AuthorityInstanceUID, c.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// MarkCronReplica retains the namespace boundary after its transport binding ends.
func MarkCronReplica(ctx context.Context, tx Transaction, projectUID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,'replica') ON CONFLICT(key) DO NOTHING`, CronClaimAuthorityPrefix+projectUID)
	return err
}

// RejectCronClaimTransfer refuses to erase or reparent root-local reservations.
func RejectCronClaimTransfer(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectID int64) error {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cron_occurrence_claims WHERE project_id=$1)`, projectID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: retained occurrence claims cannot transfer project authority", ErrFederatedSpokeUnsupported)
	}
	return nil
}

// CheckCronClaimReplicaTransition runs under the project's lifecycle lock.
func CheckCronClaimReplicaTransition(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if err := RejectCronClaimTransfer(ctx, tx, projectID); err != nil {
		return err
	}
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&id); err != nil {
		return err
	}
	return MarkCronReplica(ctx, tx, id)
}

// CheckCronClaimUpstream reads the existing namespace pin without changing state.
func CheckCronClaimUpstream(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, p int64, authority string) error {
	if !cronUID(authority) {
		return ErrCronClaimPending
	}
	var projectUID string
	if err := q.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL`, p).Scan(&projectUID); err != nil {
		return err
	}
	var role, hubUID string
	var enabled int
	if err := q.QueryRowContext(ctx, `SELECT role,enabled,hub_project_uid FROM federation_bindings WHERE project_id=$1`, p).Scan(&role, &enabled, &hubUID); err != nil {
		return err
	}
	if role != string(FederationRoleSpoke) || enabled != 1 || hubUID != projectUID {
		return ErrCronClaimPending
	}
	var retained string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, CronClaimAuthorityPrefix+projectUID).Scan(&retained)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.HasPrefix(retained, "root:") && retained != "root:"+authority {
		return ErrCronClaimPending
	}
	var foreign bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_root_keys WHERE project_uid=$1 AND active=1 AND authority_uid<>$2)`, projectUID, authority).Scan(&foreign); err != nil {
		return err
	}
	if foreign {
		return ErrCronClaimPending
	}
	return nil
}

// AdmitClaim runs incoming fences before forwarding and pins the root identity.
// The transaction ends before any remote mutation; admitted requests can finish in flight.
func (a CronSQL) AdmitClaim(ctx context.Context, p int64, authority string) error {
	return a.transactProject(ctx, p, func(tx *sql.Tx) error {
		if err := LockCronProject(ctx, tx, p); err != nil {
			return err
		}
		if err := CheckCronClaimUpstream(ctx, tx, p, authority); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) SELECT $1||uid,$2 FROM projects WHERE id=$3 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, CronClaimAuthorityPrefix, "root:"+authority, p)
		return err
	})
}
