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
	"go.kenn.io/kata/internal/metadata"
)

// SetProjectPause serializes policy with native dispatch and retains the first
// pause object on repeats, including malformed legacy state until resume.
func (a CronSQL) SetProjectPause(ctx context.Context, in SetCronProjectPauseIn) (PatchProjectMetadataOut, error) {
	var out PatchProjectMetadataOut
	if strings.TrimSpace(in.Actor) == "" || len(in.Actor) > 256 || len(in.Reason) > 256 {
		return out, fmt.Errorf("%w: actor required and reason limited to 256 bytes", cron.ErrInvalid)
	}
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		out = PatchProjectMetadataOut{}
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
		if in.DiscardMissed && (in.Paused || in.ExpectedRevision == nil) {
			return fmt.Errorf("%w: discard requires resume and retained project revision", cron.ErrInvalid)
		}
		if in.ExpectedRevision != nil && *in.ExpectedRevision != project.Revision {
			return &RevisionConflictError{CurrentRevision: project.Revision}
		}
		pause := CronProjectPauseFromMetadata(project.Metadata)
		if in.DiscardMissed {
			var pausedAt *time.Time
			if pause != nil && !pause.Invalid {
				at := pause.PausedAt.UTC()
				pausedAt = &at
			}
			// An unpaused project is a selected-revision no-op, except that a
			// malformed present cutoff can be repaired without a retained pause.
			var values map[string]jsontext.Value
			if project.Metadata != "" {
				if err := json.Unmarshal([]byte(project.Metadata), &values); err != nil {
					return err
				}
			}
			_, validErr := decodeCronPassover(values[CronPassoverMetadataKey])
			raw, present := values[CronPassoverMetadataKey]
			present = present && raw.Kind() != jsontext.KindNull
			if pause == nil && (!present || validErr == nil) {
				out.Project = project
				out.NewRevision = project.Revision
				return nil
			}
			patch, err := cronPassoverPatch(project, CronPassoverMetadataKey, actor, time.Now().UTC(), pausedAt)
			if err != nil {
				return err
			}
			if patch == nil {
				patch = map[string]jsontext.Value{}
			}
			if pause != nil {
				patch[CronProjectPauseMetadataKey] = jsontext.Value("null")
			}
			out, err = a.writeCronProjectMetadataTx(ctx, tx, project, actor, patch)
			return err
		}
		if (pause != nil) == in.Paused {
			out.Project = project
			out.NewRevision = project.Revision
			return nil
		}
		raw := jsontext.Value("null")
		if in.Paused {
			raw, err = json.Marshal(cronPauseValue{PausedBy: actor, PausedAt: time.Now().UTC(), Reason: in.Reason})
			if err != nil {
				return err
			}
			if _, err := decodeCronProjectPause(raw); err != nil {
				return err
			}
		}
		out, err = a.writeCronProjectMetadataTx(ctx, tx, project, actor, map[string]jsontext.Value{CronProjectPauseMetadataKey: raw})
		return err
	})
	if err != nil {
		return PatchProjectMetadataOut{}, err
	}
	return out, nil
}

func requireCronRootTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	var role FederationRole
	err := tx.QueryRowContext(ctx, `SELECT role FROM federation_bindings WHERE project_id=$1`, projectID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Disabling federation does not transfer policy authority to a replica.
	if role != FederationRoleHub {
		return errors.Join(ErrFederatedReadOnly, ErrFederatedSpokeUnsupported)
	}
	return nil
}

func (a CronSQL) writeCronProjectMetadataTx(ctx context.Context, tx *sql.Tx, project Project, actor string, patch map[string]jsontext.Value) (PatchProjectMetadataOut, error) {
	var out PatchProjectMetadataOut
	next, err := ApplyMetadataPatch(jsontext.Value(project.Metadata), patch)
	if err != nil {
		return out, err
	}
	diff, err := metadata.Diff(jsontext.Value(project.Metadata), next)
	if err != nil {
		return out, err
	}
	out.Project = project
	out.NewRevision = project.Revision
	if len(diff) == 0 {
		return out, nil
	}
	project.Revision++
	project.Metadata = JSONBlob(next)
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET metadata=$1,revision=$2 WHERE id=$3`, string(next), project.Revision, project.ID); err != nil {
		return out, err
	}
	type keyDiff struct {
		From jsontext.Value `json:"from"`
		To   jsontext.Value `json:"to"`
	}
	changes := make(map[string]keyDiff, len(diff))
	for key, value := range diff {
		changes[key] = keyDiff{From: value.From, To: value.To}
	}
	payload, err := json.Marshal(struct {
		Diff        map[string]keyDiff `json:"diff"`
		RevisionNew int64              `json:"revision_new"`
	}{changes, project.Revision})
	if err != nil {
		return out, err
	}
	event, err := a.InsertEvent(ctx, tx, CronEvent{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, Type: "project.metadata_updated", Actor: actor, Payload: string(payload)})
	if err != nil {
		return out, err
	}
	return PatchProjectMetadataOut{Project: project, NewRevision: project.Revision, Changed: true, Event: event}, nil
}
