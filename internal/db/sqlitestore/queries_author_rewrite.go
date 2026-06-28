package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// RewriteAuthorIdentity rewrites one exact author identity across current
// project rows before federation enrollment snapshots are created.
func (d *Store) RewriteAuthorIdentity(
	ctx context.Context,
	p db.RewriteAuthorIdentityParams,
) (db.RewriteAuthorIdentityResult, error) {
	return retryWrite1(ctx, d, func() (db.RewriteAuthorIdentityResult, error) {
		return d.rewriteAuthorIdentity(ctx, p)
	})
}

func (d *Store) rewriteAuthorIdentity(
	ctx context.Context,
	p db.RewriteAuthorIdentityParams,
) (db.RewriteAuthorIdentityResult, error) {
	from := strings.TrimSpace(p.From)
	to := strings.TrimSpace(p.To)
	if from == "" {
		return db.RewriteAuthorIdentityResult{}, fmt.Errorf("from author is required")
	}
	if to == "" {
		return db.RewriteAuthorIdentityResult{}, fmt.Errorf("to author is required")
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRowContext(ctx,
		projectSelect+` WHERE id = ? AND deleted_at IS NULL`, p.ProjectID))
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, err
	}
	if binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, p.ProjectID)); err == nil {
		return db.RewriteAuthorIdentityResult{}, &db.ProjectFederatedError{Role: binding.Role}
	} else if err != nil && !errors.Is(err, db.ErrNotFound) {
		return db.RewriteAuthorIdentityResult{}, err
	}

	actor, err := d.effectiveLocalMutationActorTx(ctx, tx, p.ProjectID, p.Actor)
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, err
	}
	ts := nowTimestamp()
	var out db.RewriteAuthorIdentityResult

	out.IssueAuthors, err = execRowsAffected(ctx, tx, `
		UPDATE issues
		   SET author = ?, updated_at = ?
		 WHERE project_id = ? AND author = ?`,
		to, ts, p.ProjectID, from)
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, fmt.Errorf("rewrite issue authors: %w", err)
	}
	out.IssueOwners, err = execRowsAffected(ctx, tx, `
		UPDATE issues
		   SET owner = ?, updated_at = ?
		 WHERE project_id = ? AND owner = ?`,
		to, ts, p.ProjectID, from)
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, fmt.Errorf("rewrite issue owners: %w", err)
	}
	out.CommentAuthors, err = execRowsAffected(ctx, tx, `
		UPDATE comments
		   SET author = ?
		 WHERE author = ?
		   AND issue_id IN (SELECT id FROM issues WHERE project_id = ?)`,
		to, from, p.ProjectID)
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, fmt.Errorf("rewrite comment authors: %w", err)
	}
	out.LinkAuthors, err = execRowsAffected(ctx, tx, `
		UPDATE links
		   SET author = ?
		 WHERE author = ?
		   AND from_issue_id IN (SELECT id FROM issues WHERE project_id = ?)`,
		to, from, p.ProjectID)
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, fmt.Errorf("rewrite link authors: %w", err)
	}
	out.TotalCount = out.Total()
	out.Changed = out.TotalCount > 0
	if !out.Changed {
		if err := tx.Commit(); err != nil {
			return db.RewriteAuthorIdentityResult{}, err
		}
		return out, nil
	}

	payload, err := json.Marshal(struct {
		ProjectUID     string `json:"project_uid"`
		From           string `json:"from"`
		To             string `json:"to"`
		IssueAuthors   int64  `json:"issue_authors"`
		IssueOwners    int64  `json:"issue_owners"`
		CommentAuthors int64  `json:"comment_authors"`
		LinkAuthors    int64  `json:"link_authors"`
		Total          int64  `json:"total"`
		UpdatedAt      string `json:"updated_at"`
	}{
		ProjectUID:     project.UID,
		From:           from,
		To:             to,
		IssueAuthors:   out.IssueAuthors,
		IssueOwners:    out.IssueOwners,
		CommentAuthors: out.CommentAuthors,
		LinkAuthors:    out.LinkAuthors,
		Total:          out.TotalCount,
		UpdatedAt:      ts,
	})
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, fmt.Errorf("marshal author rewrite payload: %w", err)
	}
	event, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   project.ID,
		ProjectUID:  project.UID,
		ProjectName: project.Name,
		Type:        "project.author_rewritten",
		Actor:       actor,
		Payload:     string(payload),
	})
	if err != nil {
		return db.RewriteAuthorIdentityResult{}, err
	}
	out.Event = &event

	if err := tx.Commit(); err != nil {
		return db.RewriteAuthorIdentityResult{}, err
	}
	return out, nil
}

func execRowsAffected(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
