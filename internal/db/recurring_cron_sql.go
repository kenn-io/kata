package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/notification"
)

// RecurringCronCandidate avoids decoding a page's documents before individual dispatch.
type RecurringCronCandidate struct {
	ProjectID        int64
	JobUID           string
	ExpectedEventUID string
}

// DispatchRecurringCronIn fences dispatch to the enumerated definition and clock.
type DispatchRecurringCronIn struct {
	ProjectID        int64
	JobUID           string
	ExpectedEventUID string
	Now              time.Time
}

// DispatchRecurringCronOut contains only events from the committed transaction.
type DispatchRecurringCronOut struct {
	Events []Event
	More   bool
}

type recurringWriteKey struct{}

// CheckRecurringMetadataWrite protects replayable delivery evidence from client edits.
func CheckRecurringMetadataWrite(ctx context.Context, values map[string]jsontext.Value) error {
	if ctx.Value(recurringWriteKey{}) == true {
		return nil
	}
	for key := range values {
		if recurringMetadataKey(key) {
			return fmt.Errorf("%w: %s is system delivery state", cron.ErrInvalid, key)
		}
	}
	return nil
}
func recurringMetadataKey(key string) bool {
	return key == "cron.occurrence" || strings.HasPrefix(key, "cron.progress.")
}
func recurringWriteContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, recurringWriteKey{}, true)
}

func recurringAuthority(ctx context.Context, tx *sql.Tx, projectID int64) (bool, FederationRole, error) {
	var role FederationRole
	var enabled int
	err := tx.QueryRowContext(ctx, `SELECT role,enabled FROM federation_bindings WHERE project_id=$1`, projectID).Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return role == FederationRoleHub && enabled == 1, role, nil
}

// recurringParent reads the bound parent and whether it is soft-deleted.
func (a CronSQL) recurringParent(ctx context.Context, tx *sql.Tx, projectID int64, parentUID string) (Issue, bool, error) {
	// Parent lock precedes reading owner/status/progress, sharing normal mutation serialization.
	query := `SELECT id,uid,status,owner,author,metadata,revision,deleted_at IS NOT NULL FROM issues WHERE uid=$1 AND project_id=$2`
	if a.Postgres {
		query += " FOR UPDATE"
	}
	var p Issue
	var deleted bool
	err := tx.QueryRowContext(ctx, query, parentUID, projectID).Scan(&p.ID, &p.UID, &p.Status, &p.Owner, &p.Author, &p.Metadata, &p.Revision, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, ErrNotFound
	}
	return p, deleted, err
}
func readRecurringProgress(parent Issue, jobUID string) (cron.RecurringProgress, error) {
	var values map[string]jsontext.Value
	var p cron.RecurringProgress
	if err := json.Unmarshal([]byte(parent.Metadata), &values); err != nil {
		return p, err
	}
	raw, ok := values["cron.progress."+jobUID]
	if !ok {
		return p, fmt.Errorf("%w: missing recurring progress for job %s", cron.ErrInvalid, jobUID)
	}
	return cron.DecodeRecurringProgress(raw)
}
func (a CronSQL) checkRecurringPutTx(ctx context.Context, tx *sql.Tx, req cronPutRequest, current cronRow) error {
	next := req.definition.(cron.JobDefinition)
	var old cron.JobDefinition
	if len(current.document) > 0 {
		var err error
		old, err = cron.DecodeJob(current.document)
		if err != nil && !req.deleted {
			return err
		}
	}
	oldNative, newNative := cron.IsRecurringIssueJob(old), cron.IsRecurringIssueJob(next)
	if !oldNative && !newNative {
		return nil
	}
	authority, role, err := recurringAuthority(ctx, tx, req.projectID)
	if err != nil {
		return err
	}
	if !authority {
		if role == FederationRoleSpoke {
			return errors.Join(ErrFederatedReadOnly, ErrFederatedSpokeUnsupported, ErrRecurringHubRequired)
		}
		return errors.Join(ErrFederatedReadOnly, ErrFederatedSpokeUnsupported)
	}
	if oldNative && (!newNative || old.Issue.ParentUID != next.Issue.ParentUID) {
		return fmt.Errorf("%w: native parent binding and ownership are immutable", cron.ErrInvalid)
	}
	if oldNative && !req.deleted {
		parent, _, err := a.recurringParent(ctx, tx, req.projectID, old.Issue.ParentUID)
		if err != nil {
			return err
		}
		_, err = readRecurringProgress(parent, req.id)
		return err
	}
	return nil
}
func (a CronSQL) initializeRecurringTx(ctx context.Context, tx *sql.Tx, req cronPutRequest, current cronRow, event Event) ([]Event, error) {
	next := req.definition.(cron.JobDefinition)
	if req.deleted || !cron.IsRecurringIssueJob(next) {
		return nil, nil
	}
	if len(current.document) > 0 {
		old, err := cron.DecodeJob(current.document)
		if err != nil {
			return nil, err
		}
		if cron.IsRecurringIssueJob(old) {
			return nil, nil
		}
	}
	parent, _, err := a.recurringParent(ctx, tx, req.projectID, next.Issue.ParentUID)
	if err != nil {
		return nil, err
	}
	var values map[string]jsontext.Value
	if err := json.Unmarshal([]byte(parent.Metadata), &values); err != nil {
		return nil, err
	}
	key := "cron.progress." + req.id
	if _, exists := values[key]; exists {
		return nil, fmt.Errorf("%w: recurring progress already exists", cron.ErrInvalid)
	}
	anchor := time.Now().UTC()
	raw, err := json.Marshal(cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor})
	if err != nil {
		return nil, err
	}
	actor, err := a.effectiveMutationActor(ctx, tx, req.projectID, event.Actor)
	if err != nil {
		return nil, err
	}
	out, err := a.PatchIssueMetadataTx(recurringWriteContext(ctx), tx, PatchIssueMetadataIn{IssueID: parent.ID, Actor: actor, Patch: map[string]jsontext.Value{key: raw}})
	if err != nil {
		return nil, err
	}
	return []Event{out.Event}, nil
}

// RecurringJobs enumerates only native jobs on live authoritative projects.
func (a CronSQL) RecurringJobs(ctx context.Context, after string, limit int) ([]RecurringCronCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	action := `json_extract(j.definition_json,'$.action.kind')`
	issue := `json_extract(j.definition_json,'$.issue.kind')`
	enabled := `json_extract(j.definition_json,'$.enabled')=1`
	if a.Postgres {
		action = `j.definition_json::jsonb #>> '{action,kind}'`
		issue = `j.definition_json::jsonb #>> '{issue,kind}'`
		enabled = `j.definition_json::jsonb ->> 'enabled'='true'`
	}
	query := `SELECT j.project_id,j.uid,j.definition_event_uid FROM cron_jobs j JOIN projects p ON p.id=j.project_id WHERE j.uid>$1 AND j.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name<>$2 AND NOT EXISTS(SELECT 1 FROM federation_bindings fb WHERE fb.project_id=p.id AND (fb.role<>'hub' OR fb.enabled<>1)) AND ` + action + `='notify' AND ` + issue + `='per-run' AND ` + enabled + ` ORDER BY j.uid LIMIT $3`
	rows, err := a.Query.QueryContext(ctx, query, after, SystemProjectName, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []RecurringCronCandidate{}
	for rows.Next() {
		var c RecurringCronCandidate
		if err := rows.Scan(&c.ProjectID, &c.JobUID, &c.ExpectedEventUID); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// DispatchRecurring composes ordinary issue writes and progress in one fenced transaction.
func (a CronSQL) DispatchRecurring(ctx context.Context, in DispatchRecurringCronIn) (DispatchRecurringCronOut, error) {
	var out DispatchRecurringCronOut
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		out = DispatchRecurringCronOut{}
		if err := LockCronProject(ctx, tx, in.ProjectID); errors.Is(err, ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		authority, _, err := recurringAuthority(ctx, tx, in.ProjectID)
		if err != nil || !authority {
			return err
		}
		project, err := a.openCronProject(ctx, tx, in.ProjectID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		row, err := scanCron(tx.QueryRowContext(ctx, `SELECT `+cronDefinitionColumns+` FROM cron_jobs WHERE project_id=$1 AND uid=$2`, in.ProjectID, in.JobUID))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.DeletedAt != nil || row.DefinitionEventUID != in.ExpectedEventUID {
			return nil
		}
		job, err := jobFromRow(row)
		if err != nil {
			return err
		}
		if !cron.IsRecurringIssueJob(job.Definition) || !job.Definition.Enabled {
			return nil
		}
		parent, deleted, err := a.recurringParent(ctx, tx, project.ID, job.Definition.Issue.ParentUID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if deleted || parent.Status != "open" {
			return nil
		}
		progress, err := readRecurringProgress(parent, job.UID)
		if err != nil {
			return err
		}
		batch, err := cron.PlanRecurring(job.Definition, job.DefinitionEventUID, progress, in.Now)
		if err != nil {
			return err
		}
		actor, err := a.effectiveMutationActor(ctx, tx, project.ID, "system")
		if err != nil {
			return err
		}
		recipient := job.Definition.Action.Recipient
		if recipient == "current-owner-or-author" {
			recipient = parent.Author
			if parent.Owner != nil && *parent.Owner != "" {
				recipient = *parent.Owner
			}
		}
		recipient, err = notification.NormalizeRecipient(recipient)
		if err != nil {
			return err
		}
		for _, due := range batch.Due {
			stamp := due.UTC().Format(time.RFC3339Nano)
			occurrence, err := json.Marshal(struct {
				Version            int       `json:"version"`
				JobUID             string    `json:"job_uid"`
				DefinitionEventUID string    `json:"definition_event_uid"`
				ScheduledAt        time.Time `json:"scheduled_at"`
			}{1, job.UID, job.DefinitionEventUID, due.UTC()})
			if err != nil {
				return err
			}
			message := job.Definition.Action.Message
			if strings.TrimSpace(message) == "" {
				message = "Recurring work due: " + stamp
			}
			notice, err := json.Marshal(notification.Value{From: "system", Teammate: "cron", Message: message})
			if err != nil {
				return err
			}
			_, event, err := a.CreateIssueTx(recurringWriteContext(ctx), tx, CreateIssueParams{ProjectID: project.ID, Title: job.Definition.Issue.Title + " — " + stamp, Body: job.Definition.Issue.Body, Author: actor, Owner: parent.Owner, Links: []InitialLink{{Type: "parent", ToNumber: parent.ID, ExpectedProjectUID: project.UID}}, Metadata: map[string]jsontext.Value{"cron.occurrence": occurrence, notification.MetadataKey(recipient): notice}})
			if err != nil {
				return err
			}
			out.Events = append(out.Events, event)
		}
		before, err := json.Marshal(progress)
		if err != nil {
			return err
		}
		after, err := json.Marshal(batch.Progress)
		if err != nil {
			return err
		}
		if string(before) != string(after) {
			changed, err := a.PatchIssueMetadataTx(recurringWriteContext(ctx), tx, PatchIssueMetadataIn{IssueID: parent.ID, Actor: actor, Patch: map[string]jsontext.Value{"cron.progress." + job.UID: after}})
			if err != nil {
				return err
			}
			if changed.Changed {
				out.Events = append(out.Events, changed.Event)
			}
		}
		out.More = batch.More
		return nil
	})
	if err != nil {
		return DispatchRecurringCronOut{}, err
	}
	return out, nil
}

// CheckRecurringSpokeEvent prevents replicas from rewriting hub-owned delivery state.
// Already accepted events may be retried without becoming a new mutation.
func CheckRecurringSpokeEvent(ctx context.Context, tx *sql.Tx, event RemoteEvent) error {
	protected, err := payloadHasNativeRecurringState(event.Type, string(event.Payload))
	if err != nil {
		return err
	}
	if !protected && !strings.HasPrefix(event.Type, "cron.job.") {
		return nil
	}
	var existing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE uid=$1)`, event.EventUID).Scan(&existing); err != nil {
		return err
	}
	if existing {
		return nil
	}
	if protected {
		return fmt.Errorf("%w: native recurring definitions and delivery metadata are hub-owned", ErrFederationIngestValidation)
	}
	if strings.HasPrefix(event.Type, "cron.job.") {
		var body struct {
			UID string `json:"uid"`
		}
		if err := json.Unmarshal(event.Payload, &body); err != nil {
			return err
		}
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT definition_json FROM cron_jobs WHERE uid=$1`, body.UID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		definition, err := cron.DecodeJob([]byte(raw))
		if err != nil {
			return err
		}
		if cron.IsRecurringIssueJob(definition) {
			return fmt.Errorf("%w: native recurring job cannot change from a spoke", ErrFederationIngestValidation)
		}
	}
	return nil
}

// CheckNativeRecurringParentMove keeps a bound parent in the project that owns
// its native job. Deleted jobs retain that binding and can be restored.
func CheckNativeRecurringParentMove(ctx context.Context, tx *sql.Tx, postgres bool, projectID int64, parentUID string) error {
	action := `json_extract(definition_json,'$.action.kind')`
	issue := `json_extract(definition_json,'$.issue.kind')`
	parent := `json_extract(definition_json,'$.issue.parent_uid')`
	if postgres {
		action = `definition_json::jsonb #>> '{action,kind}'`
		issue = `definition_json::jsonb #>> '{issue,kind}'`
		parent = `definition_json::jsonb #>> '{issue,parent_uid}'`
	}
	var pinned bool
	query := `SELECT EXISTS(SELECT 1 FROM cron_jobs WHERE project_id=$1 AND ` + action + `='notify' AND ` + issue + `='per-run' AND ` + parent + `=$2)`
	if err := tx.QueryRowContext(ctx, query, projectID, parentUID).Scan(&pinned); err != nil {
		return err
	}
	if pinned {
		return &NativeRecurringParentPinnedError{}
	}
	return nil
}
