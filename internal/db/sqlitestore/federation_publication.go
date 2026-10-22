package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

var _ db.FederationPublicationStore = (*Store)(nil)

const publicationMetadataPrefix = "federation.publication.v1/"

// The marker is immutable and account-bound. It carries no credential and is
// never portable content. Retries resolve its project by numeric ID and UID,
// independently of subsequent project names, edits, or relay configuration.
type publicationCompletion struct {
	OperationUID, Digest, SourceInstanceUID, ProjectUID, TargetInstanceUID, Account string
	ProjectID                                                                       int64
}

// PublishFederationProject writes the project and frozen history in one transaction.
func (d *Store) PublishFederationProject(ctx context.Context, p db.FederationPublication, account string) (db.FederationPublicationResult, error) {
	if _, err := db.ValidateFederationPublication(p); err != nil {
		return db.FederationPublicationResult{}, err
	}
	if !db.ValidReplayContentHash(p.Digest) || strings.TrimSpace(account) == "" {
		return db.FederationPublicationResult{}, db.ErrFederationPublicationInvalid
	}
	if p.TargetInstanceUID != d.instanceUID {
		return db.FederationPublicationResult{}, db.ErrFederationPublicationConflict
	}
	// The wire codec owns canonical digest/64 MiB validation. Pure record and
	// projection checks happen above, before the native writer/event locks.
	var result db.FederationPublicationResult
	err := d.RetryTransient(ctx, func() error {
		result = db.FederationPublicationResult{}
		// BeginTx applies the same request-scoped token/host fences as native
		// project creation; their locks remain held through commit and exact retry.
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		// Reserve SQLite's writer before inspecting the marker or collisions.
		if _, err = tx.ExecContext(ctx, `UPDATE meta SET value=value WHERE 0`); err != nil {
			return err
		}
		result, err = d.publishFederationProjectTx(ctx, tx, p, account)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	if isUniqueConstraint(err) {
		err = db.ErrFederationPublicationConflict
	}
	return result, err
}

func (d *Store) publishFederationProjectTx(ctx context.Context, tx *sql.Tx, p db.FederationPublication, account string) (db.FederationPublicationResult, error) {
	var result db.FederationPublicationResult
	want := publicationCompletion{OperationUID: p.OperationUID, Digest: p.Digest, SourceInstanceUID: p.SourceInstanceUID, ProjectUID: p.ProjectUID, TargetInstanceUID: p.TargetInstanceUID, Account: account}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, publicationMetadataPrefix+p.OperationUID).Scan(&raw)
	if err == nil {
		var retained publicationCompletion
		if json.Unmarshal([]byte(raw), &retained) != nil || retained.ProjectID <= 0 {
			return result, db.ErrFederationPublicationConflict
		}
		want.ProjectID = retained.ProjectID
		if retained != want {
			return result, db.ErrFederationPublicationConflict
		}
		result.Project, err = projectByIDTx(ctx, tx, retained.ProjectID)
		if errors.Is(err, db.ErrNotFound) || (err == nil && result.Project.UID != retained.ProjectUID) {
			return db.FederationPublicationResult{}, db.ErrFederationPublicationConflict
		}
		result.Replayed = true
		return result, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	var collision int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE uid=$1 OR name=$2`, p.ProjectUID, p.ProjectName).Scan(&collision); err != nil {
		return result, err
	}
	if collision != 0 {
		return result, db.ErrFederationPublicationConflict
	}
	// Explicit source-to-target maps never consult unrelated destination rows.
	// Only numeric keys change; payloads, UIDs, short IDs, hashes and clocks stay.
	issueIDs := map[int64]int64{}
	var projectID int64
	for _, record := range db.OrderImportRecords(p.Records) {
		switch r := record.(type) {
		case *db.ProjectExport:
			row := *r
			row.ID = 0
			if err = insertReplayProject(ctx, tx, &row, p.ProjectName); err != nil {
				return result, err
			}
			projectID, err = publicationLastIDTx(ctx, tx)
			if err != nil {
				return result, err
			}
		case *db.IssueExport:
			row := *r
			row.ProjectID = projectID
			row.ID = 0
			if err = importIssue(ctx, tx, &row); err != nil {
				return result, err
			}
			issueIDs[r.ID], err = publicationLastIDTx(ctx, tx)
			if err != nil {
				return result, err
			}
		case *db.CommentExport:
			row := *r
			row.IssueID = issueIDs[r.IssueID]
			if row.IssueID == 0 {
				return result, db.ErrFederationPublicationInvalid
			}
			row.ID = 0
			if err = importComment(ctx, tx, &row); err != nil {
				return result, err
			}
		case *db.IssueLabelExport:
			row := *r
			row.IssueID = issueIDs[r.IssueID]
			if row.IssueID == 0 {
				return result, db.ErrFederationPublicationInvalid
			}
			if err = importLabel(ctx, tx, &row); err != nil {
				return result, err
			}
		case *db.LinkExport:
			row := *r
			row.FromIssueID = issueIDs[r.FromIssueID]
			row.ToIssueID = issueIDs[r.ToIssueID]
			if row.FromIssueID == 0 || row.ToIssueID == 0 {
				return result, db.ErrFederationPublicationInvalid
			}
			if row.Type == "related" && row.FromIssueID > row.ToIssueID {
				row.FromIssueID, row.ToIssueID = row.ToIssueID, row.FromIssueID
				row.FromIssueUID, row.ToIssueUID = row.ToIssueUID, row.FromIssueUID
			}
			row.ID = 0
			if err = insertReplayLink(ctx, tx, &row); err != nil {
				return result, err
			}
		case *db.EventExport:
			row := *r
			row.ID = 0
			row.ProjectID = projectID
			if r.IssueID != nil {
				mapped := issueIDs[*r.IssueID]
				if mapped == 0 {
					return result, db.ErrFederationPublicationInvalid
				}
				row.IssueID = &mapped
			}
			if r.RelatedIssueID != nil {
				mapped := issueIDs[*r.RelatedIssueID]
				if mapped == 0 {
					return result, db.ErrFederationPublicationInvalid
				}
				row.RelatedIssueID = &mapped
			}
			if err = importEvent(ctx, tx, &row, db.ImportOptions{}); err != nil {
				return result, err
			}
		default:
			return result, db.ErrFederationPublicationInvalid
		}
	}
	result.Project, err = projectByIDTx(ctx, tx, projectID)
	if err != nil {
		return result, err
	}
	payload, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: p.ProjectName})
	if err != nil {
		return result, err
	}
	event, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: projectID, ProjectUID: p.ProjectUID, ProjectName: p.ProjectName, Type: "project.created", Actor: account, Payload: string(payload)})
	if err != nil {
		return result, err
	}
	// Check the destination's actual checkpoint budget, including the newly
	// attributed catalog event, atomically with all imported content.
	size, err := d.relayCheckpointSizeTx(ctx, tx, result.Project, account)
	if err != nil {
		return result, err
	}
	if err = size.CapacityError(p.ProjectUID); err != nil {
		return result, fmt.Errorf("%w: %w", db.ErrFederationPublicationTooLarge, err)
	}
	want.ProjectID = projectID
	marker, err := json.Marshal(want)
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)`, publicationMetadataPrefix+p.OperationUID, string(marker)); err != nil {
		return result, err
	}
	if err = bumpProjectAccess(ctx, tx); err != nil {
		return result, err
	}
	// Only this event is delivered by the caller. Historical rows never become
	// newly emitted hook actions.
	result.CatalogEvent = &event
	return result, nil
}

func publicationLastIDTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&id)
	return id, err
}
