package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

const commentSelect = `SELECT id, uid, issue_id, author, body, created_at FROM comments`

// CreateComment appends a comment and its issue.commented event atomically.
func (s *Store) CreateComment(ctx context.Context, params db.CreateCommentParams) (db.Comment, db.Event, error) {
	var comment db.Comment
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		issue, project, err := lockedIssueTx(ctx, tx, params.IssueID, false)
		if err != nil {
			return err
		}
		if err := ensureProjectWritableTx(ctx, tx, project.ID); err != nil {
			return err
		}
		effectiveActor, err := effectiveLocalMutationActorTx(ctx, tx, project.ID, params.Author)
		if err != nil {
			return err
		}
		commentUID, err := katauid.New()
		if err != nil {
			return fmt.Errorf("generate comment uid: %w", err)
		}
		createdAt := nowStoredTimestamp()
		comment, err = scanComment(tx.QueryRowContext(ctx,
			`INSERT INTO comments(uid, issue_id, author, body, created_at)
			 VALUES($1,$2,$3,$4,$5)
			 RETURNING id, uid, issue_id, author, body, created_at`,
			commentUID, issue.ID, effectiveActor, params.Body, createdAt,
		))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = $1 WHERE id = $2`, createdAt, issue.ID); err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			CommentUID string `json:"comment_uid"`
			Author     string `json:"author"`
			Body       string `json:"body"`
			CreatedAt  string `json:"created_at"`
		}{
			CommentUID: comment.UID,
			Author:     comment.Author,
			Body:       comment.Body,
			CreatedAt:  createdAt,
		})
		if err != nil {
			return fmt.Errorf("marshal comment payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx,
			issueEventInput(issue, project, "issue.commented", effectiveActor, string(payload)))
		return err
	})
	return comment, event, err
}

// EditComment changes only the body and emits an event when the value changes.
func (s *Store) EditComment(
	ctx context.Context,
	params db.EditCommentParams,
) (db.Comment, *db.Event, bool, error) {
	return s.editComment(ctx, params, s.withSerializableTx)
}

func (s *Store) editComment(
	ctx context.Context,
	params db.EditCommentParams,
	runTx func(context.Context, transactionFunc) error,
) (db.Comment, *db.Event, bool, error) {
	params.CommentUID = strings.TrimSpace(params.CommentUID)
	if params.CommentUID == "" {
		return db.Comment{}, nil, false, db.ErrNotFound
	}
	if strings.TrimSpace(params.Body) == "" {
		return db.Comment{}, nil, false, errors.New("comment body is required")
	}

	var comment db.Comment
	var event *db.Event
	var changed bool
	err := runTx(ctx, func(tx *sql.Tx) error {
		comment, event, changed = db.Comment{}, nil, false
		issue, project, err := lockedIssueTx(ctx, tx, params.IssueID, false)
		if err != nil {
			return err
		}
		comment, err = scanComment(tx.QueryRowContext(ctx,
			commentSelect+` WHERE issue_id = $1 AND uid = $2 FOR UPDATE`, issue.ID, params.CommentUID))
		if err != nil {
			return err
		}
		if comment.Body == params.Body {
			return nil
		}
		editedAt := nowStoredTimestamp()
		comment, err = scanComment(tx.QueryRowContext(ctx,
			`UPDATE comments SET body = $1 WHERE id = $2
			 RETURNING id, uid, issue_id, author, body, created_at`, params.Body, comment.ID))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = $1 WHERE id = $2`, editedAt, issue.ID); err != nil {
			return mapSQLError(err, nil)
		}
		payload, err := json.Marshal(struct {
			CommentUID string `json:"comment_uid"`
			Body       string `json:"body"`
			EditedAt   string `json:"edited_at"`
		}{
			CommentUID: comment.UID,
			Body:       comment.Body,
			EditedAt:   editedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal comment edit payload: %w", err)
		}
		created, err := s.insertEventTx(ctx, tx,
			issueEventInput(issue, project, "issue.comment_edited", params.Actor, string(payload)))
		if err != nil {
			return err
		}
		event, changed = &created, true
		return nil
	})
	return comment, event, changed, err
}

// RewriteAuthorIdentity rewrites exact current-state attribution within one
// active, non-federated project and emits a project audit event when rows move.
func (s *Store) RewriteAuthorIdentity(
	ctx context.Context,
	params db.RewriteAuthorIdentityParams,
) (db.RewriteAuthorIdentityResult, error) {
	from := strings.TrimSpace(params.From)
	to := strings.TrimSpace(params.To)
	if from == "" {
		return db.RewriteAuthorIdentityResult{}, errors.New("from author is required")
	}
	if to == "" {
		return db.RewriteAuthorIdentityResult{}, errors.New("to author is required")
	}

	var result db.RewriteAuthorIdentityResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result = db.RewriteAuthorIdentityResult{}
		project, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, params.ProjectID))
		if err != nil {
			return err
		}
		var role string
		err = tx.QueryRowContext(ctx,
			`SELECT role FROM federation_bindings WHERE project_id = $1`, project.ID).Scan(&role)
		if err == nil {
			return &db.ProjectFederatedError{Role: db.FederationRole(role)}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return mapSQLError(err, nil)
		}

		updatedAt := nowStoredTimestamp()
		result.IssueAuthors, err = execRowsAffected(ctx, tx, `
			UPDATE issues SET author = $1, updated_at = $2
			 WHERE project_id = $3 AND author = $4`, to, updatedAt, project.ID, from)
		if err != nil {
			return fmt.Errorf("rewrite issue authors: %w", err)
		}
		result.IssueOwners, err = execRowsAffected(ctx, tx, `
			UPDATE issues SET owner = $1, updated_at = $2
			 WHERE project_id = $3 AND owner = $4`, to, updatedAt, project.ID, from)
		if err != nil {
			return fmt.Errorf("rewrite issue owners: %w", err)
		}
		result.CommentAuthors, err = execRowsAffected(ctx, tx, `
			UPDATE comments SET author = $1
			 WHERE author = $2
			   AND issue_id IN (SELECT id FROM issues WHERE project_id = $3)`, to, from, project.ID)
		if err != nil {
			return fmt.Errorf("rewrite comment authors: %w", err)
		}
		result.LinkAuthors, err = execRowsAffected(ctx, tx, `
			UPDATE links SET author = $1
			 WHERE author = $2
			   AND from_issue_id IN (SELECT id FROM issues WHERE project_id = $3)`, to, from, project.ID)
		if err != nil {
			return fmt.Errorf("rewrite link authors: %w", err)
		}
		result.TotalCount = result.Total()
		result.Changed = result.TotalCount > 0
		if !result.Changed {
			return nil
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
			ProjectUID: project.UID, From: from, To: to,
			IssueAuthors: result.IssueAuthors, IssueOwners: result.IssueOwners,
			CommentAuthors: result.CommentAuthors, LinkAuthors: result.LinkAuthors,
			Total: result.TotalCount, UpdatedAt: updatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal author rewrite payload: %w", err)
		}
		created, err := s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			Type: "project.author_rewritten", Actor: params.Actor, Payload: string(payload),
		})
		if err != nil {
			return err
		}
		result.Event = &created
		return nil
	})
	return result, err
}

// CommentBodyByID returns the current body for hook payload expansion.
func (s *Store) CommentBodyByID(ctx context.Context, id int64) (string, error) {
	var body string
	if err := s.QueryRowContext(ctx, `SELECT body FROM comments WHERE id = $1`, id).Scan(&body); err != nil {
		return "", mapSQLError(err, nil)
	}
	return body, nil
}

// CommentsByIssue returns comments in stable chronological order.
func (s *Store) CommentsByIssue(ctx context.Context, issueID int64) ([]db.Comment, error) {
	rows, err := s.QueryContext(ctx,
		commentSelect+` WHERE issue_id = $1 ORDER BY created_at ASC, id ASC`, issueID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	var comments []db.Comment
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	return comments, mapSQLError(rows.Err(), nil)
}

func scanComment(row rowScanner) (db.Comment, error) {
	var comment db.Comment
	var createdAt string
	err := row.Scan(
		&comment.ID, &comment.UID, &comment.IssueID, &comment.Author, &comment.Body, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Comment{}, db.ErrNotFound
	}
	if err != nil {
		return db.Comment{}, mapSQLError(err, nil)
	}
	comment.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.Comment{}, fmt.Errorf("parse comment created_at: %w", err)
	}
	return comment, nil
}

func execRowsAffected(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, mapSQLError(err, nil)
	}
	return result.RowsAffected()
}
