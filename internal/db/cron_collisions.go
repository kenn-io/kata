package db

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/cron"
)

const collisionPeerRows = 1024
const collisionPeerBytes = 8 << 20

// validateCollisions stays inside the candidate's snapshot. Ordinary list
// endpoints remain complete; only this informational fleet read has a cap.
func (a CronSQL) validateCollisions(ctx context.Context, tx *sql.Tx, in CronValidationInput, definition cron.JobDefinition, progress *cron.RecurringProgress) (cron.CronValidationCollisions, error) {
	candidate := cron.CollisionJob{UID: in.UID, Name: in.Name, Def: definition, Progress: progress}
	peers, skipped, limited, err := a.collisionPeers(ctx, tx, in, definition)
	if err != nil {
		return cron.CronValidationCollisions{}, err
	}
	out, err := cron.PreviewCollisionsContext(ctx, candidate, peers, in.From, cron.CollisionStepBudget)
	out.Skipped += skipped
	out.Truncated = out.Truncated || limited
	return out, err
}

func (a CronSQL) collisionPeers(ctx context.Context, tx *sql.Tx, in CronValidationInput, definition cron.JobDefinition) ([]cron.CollisionJob, int, bool, error) {
	collation := "BINARY"
	if a.Postgres {
		collation = `"C"`
	}
	// Limit the lookahead payload in bytes before Scan, even for multibyte
	// definitions. A prefix beyond the remaining allowance is never decoded.
	prefix := "SUBSTR(CAST(definition_json AS BLOB),1,$3)"
	if a.Postgres {
		prefix = "SUBSTRING(CONVERT_TO(definition_json,'UTF8'),1,$3)"
	}
	rows, err := tx.QueryContext(ctx, `SELECT uid,name,`+prefix+` FROM cron_jobs WHERE project_id=$1 AND deleted_at IS NULL AND uid<>$4 ORDER BY name COLLATE `+collation+`,uid LIMIT $2`, in.ProjectID, collisionPeerRows+1, collisionPeerBytes+1, in.UID)
	if err != nil {
		return nil, 0, false, err
	}
	type document struct{ uid, name, raw string }
	documents := []document{}
	remaining := collisionPeerBytes
	limited := false
	for rows.Next() {
		if len(documents) == collisionPeerRows {
			limited = true
			break
		}
		var doc document
		if err = rows.Scan(&doc.uid, &doc.name, &doc.raw); err != nil {
			break
		}
		cost := len(doc.uid) + len(doc.name) + len(doc.raw)
		if cost > remaining {
			limited = true
			break
		}
		remaining -= cost
		documents = append(documents, doc)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, 0, false, err
	}
	peers := []cron.CollisionJob{}
	skipped := 0
	for _, doc := range documents {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		job, err := cron.DecodeJob([]byte(doc.raw))
		if err != nil {
			skipped++
			continue
		}
		if !job.Enabled || cron.IsRecurringIssueJob(job) != cron.IsRecurringIssueJob(definition) {
			continue
		}
		peer := cron.CollisionJob{UID: doc.uid, Name: doc.name, Def: job}
		if cron.IsRecurringIssueJob(job) {
			parent, deleted, err := a.recurringParentRead(ctx, tx, in.ProjectID, job.Issue.ParentUID, false, remaining+1)
			if errors.Is(err, ErrNotFound) {
				skipped++
				continue
			}
			if err != nil {
				return nil, 0, false, err
			}
			if len(parent.Metadata) > remaining {
				limited = true
				break
			}
			remaining -= len(parent.Metadata)
			if deleted || parent.Status != "open" {
				skipped++
				continue
			}
			stored, err := readRecurringProgress(parent, doc.uid)
			if err != nil {
				skipped++
				continue
			}
			peer.Progress = &stored
		}
		peers = append(peers, peer)
	}
	return peers, skipped, limited, nil
}
