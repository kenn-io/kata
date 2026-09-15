package pgstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

const apiTokenSelect = `SELECT id, token_hash, actor, name, scope_kind, scope_project_uid, scope_root_issue_uid, expires_at, created_at, last_used_at, revoked_at FROM api_tokens` //nolint:gosec // SQL column name, not a credential.

type tokenRevokedPayload struct {
	TokenID     int64   `json:"token_id"`
	TargetActor string  `json:"target_actor"`
	Name        *string `json:"name,omitempty"`
}

// EnsureSystemProject creates the hidden project that owns daemon-global
// audit events. Existing rows must carry both reserved identity values.
func (s *Store) EnsureSystemProject(ctx context.Context) error {
	return s.RetryTransient(ctx, func() error {
		if _, err := s.ExecContext(ctx, `INSERT INTO projects(uid, name)
VALUES($1, $2) ON CONFLICT DO NOTHING`, db.SystemProjectUID, db.SystemProjectName); err != nil {
			return fmt.Errorf("ensure system project: %w", mapSQLError(err, nil))
		}
		project, err := s.SystemProject(ctx)
		if err != nil {
			return fmt.Errorf("ensure system project: %w", err)
		}
		if project.UID != db.SystemProjectUID {
			return fmt.Errorf("ensure system project: %s has uid %q, want %q",
				db.SystemProjectName, project.UID, db.SystemProjectUID)
		}
		return nil
	})
}

// SystemProject returns the hidden project used for daemon-global events.
func (s *Store) SystemProject(ctx context.Context) (db.Project, error) {
	return scanProject(s.QueryRowContext(ctx, projectSelect+` WHERE name = $1`, db.SystemProjectName))
}

// CreateAPIToken stores only the token digest and appends its audit event in
// the same transaction.
func (s *Store) CreateAPIToken(
	ctx context.Context,
	params db.CreateAPITokenParams,
) (db.APIToken, db.Event, error) {
	if strings.TrimSpace(params.PlaintextToken) == "" {
		return db.APIToken{}, db.Event{}, errors.New("token must be non-empty")
	}
	if err := db.ValidateTokenActor(params.Actor); err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	params.Actor = strings.TrimSpace(params.Actor)
	params.AdminActor = strings.TrimSpace(params.AdminActor)
	if params.AdminActor == "" {
		return db.APIToken{}, db.Event{}, errors.New("admin actor must be non-empty")
	}
	if params.Name != nil {
		name := strings.TrimSpace(*params.Name)
		if name == "" {
			return db.APIToken{}, db.Event{}, errors.New("token name must be non-empty")
		}
		params.Name = &name
	}
	if err := db.ValidateAPITokenGrant(params.Scope, params.ExpiresAt); err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	if params.ExpiresAt != nil {
		expiresAt := params.ExpiresAt.UTC()
		params.ExpiresAt = &expiresAt
	}

	var token db.APIToken
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		systemProject, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE name = $1 FOR SHARE`, db.SystemProjectName))
		if err != nil {
			return err
		}
		hash := apiTokenHash(params.PlaintextToken)
		var tokenID int64
		var scopeKind, scopeProjectUID, scopeRootIssueUID any
		if params.Scope != nil {
			scopeKind = string(params.Scope.Kind)
			scopeProjectUID = params.Scope.ProjectUID
			scopeRootIssueUID = params.Scope.RootIssueUID
		}
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO api_tokens(
				token_hash,actor,name,scope_kind,scope_project_uid,scope_root_issue_uid,expires_at
			) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			hash, params.Actor, params.Name, scopeKind, scopeProjectUID, scopeRootIssueUID,
			storedNullTime{Time: params.ExpiresAt},
		).Scan(&tokenID); err != nil {
			return mapSQLError(err, nil)
		}
		token, err = scanAPIToken(tx.QueryRowContext(ctx, apiTokenSelect+` WHERE id = $1`, tokenID))
		if err != nil {
			return err
		}
		payload, err := json.Marshal(db.ReplayTokenCreated{
			TokenID: token.ID, TokenHash: token.TokenHash, TargetActor: token.Actor, Name: token.Name,
			Scope: token.Scope, ExpiresAt: token.ExpiresAt,
		})
		if err != nil {
			return fmt.Errorf("marshal token.created payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: systemProject.ID, ProjectUID: systemProject.UID, ProjectName: systemProject.Name,
			Type: "token.created", Actor: params.AdminActor, Payload: string(payload),
		})
		return err
	})
	return token, event, err
}

// RevokeAPIToken revokes one active token and appends its audit event in the
// same transaction.
func (s *Store) RevokeAPIToken(
	ctx context.Context,
	id int64,
	adminActor string,
) (db.APIToken, db.Event, error) {
	adminActor = strings.TrimSpace(adminActor)
	if adminActor == "" {
		return db.APIToken{}, db.Event{}, errors.New("admin actor must be non-empty")
	}
	var token db.APIToken
	var event db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		systemProject, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE name = $1 FOR SHARE`, db.SystemProjectName))
		if err != nil {
			return err
		}
		revokedAt := storedTime(time.Now())
		token, err = scanAPIToken(tx.QueryRowContext(ctx,
			`UPDATE api_tokens SET revoked_at = $1 WHERE id = $2 AND revoked_at IS NULL
RETURNING id, token_hash, actor, name, scope_kind, scope_project_uid,
          scope_root_issue_uid, expires_at, created_at, last_used_at, revoked_at`, revokedAt, id))
		if err != nil {
			return err
		}
		payload, err := json.Marshal(tokenRevokedPayload{
			TokenID: token.ID, TargetActor: token.Actor, Name: token.Name,
		})
		if err != nil {
			return fmt.Errorf("marshal token.revoked payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: systemProject.ID, ProjectUID: systemProject.UID, ProjectName: systemProject.Name,
			Type: "token.revoked", Actor: adminActor, Payload: string(payload),
		})
		return err
	})
	return token, event, err
}

// ResolveAPIToken resolves an active token and best-effort records recent use.
func (s *Store) ResolveAPIToken(ctx context.Context, plaintext string) (db.APIToken, error) {
	hash := apiTokenHash(plaintext)
	now := time.Now().UTC()
	token, err := scanAPIToken(s.QueryRowContext(ctx,
		apiTokenSelect+` WHERE token_hash = $1 AND revoked_at IS NULL
 AND (expires_at IS NULL OR expires_at > $2)`, hash, storedTime(now)))
	if err != nil {
		return db.APIToken{}, err
	}
	result, err := s.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = $1
 WHERE token_hash = $2 AND revoked_at IS NULL
   AND (expires_at IS NULL OR expires_at > $4)
   AND (last_used_at IS NULL OR last_used_at < $3)`,
		storedTime(now), hash, storedTime(now.Add(-time.Hour)), storedTime(now))
	if err != nil {
		return token, nil
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil || rowsAffected == 0 {
		return token, nil
	}
	updated, err := scanAPIToken(s.QueryRowContext(ctx, apiTokenSelect+` WHERE id = $1`, token.ID))
	if err != nil {
		return token, nil
	}
	return updated, nil
}

// APITokenByID reads one credential without scanning unrelated tokens.
func (s *Store) APITokenByID(ctx context.Context, id int64) (db.APIToken, error) {
	token, err := scanAPIToken(s.QueryRowContext(ctx, apiTokenSelect+` WHERE id = $1`, id))
	token.TokenHash = ""
	return token, err
}

// ListAPITokens returns token metadata with every digest redacted.
func (s *Store) ListAPITokens(ctx context.Context) ([]db.APIToken, error) {
	rows, err := s.QueryContext(ctx, apiTokenSelect+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var tokens []db.APIToken
	for rows.Next() {
		token, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		token.TokenHash = ""
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api tokens: %w", mapSQLError(err, nil))
	}
	return tokens, nil
}

// IssueScopedTokenTransactionFence rechecks a scoped credential and its
// immutable root after a write transaction begins and before its first write.
func (s *Store) IssueScopedTokenTransactionFence(admitted db.APIToken) db.TransactionFence {
	return func(ctx context.Context, transaction db.Transaction) error {
		if admitted.Scope == nil || admitted.ExpiresAt == nil {
			return db.ErrNotFound
		}
		current, err := scanAPIToken(transaction.QueryRowContext(ctx,
			apiTokenSelect+` WHERE id = $1 FOR SHARE`, admitted.ID))
		if err != nil {
			return err
		}
		if !db.ActiveAPITokenGrantMatches(current, admitted, time.Now()) {
			return db.ErrNotFound
		}
		var active int
		err = transaction.QueryRowContext(ctx, `
			SELECT 1
			FROM projects AS project
			JOIN issues AS root
			  ON root.uid = $1 AND root.project_id = project.id
			WHERE project.uid = $2
			  AND project.deleted_at IS NULL
			  AND root.deleted_at IS NULL
			  AND NOT EXISTS (
			    SELECT 1 FROM federation_bindings AS binding
			    WHERE binding.project_id = project.id AND binding.role = 'spoke'
			  )
			FOR SHARE OF project, root`, admitted.Scope.RootIssueUID, admitted.Scope.ProjectUID).Scan(&active)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		if active != 1 {
			return db.ErrNotFound
		}
		if err := checkIssueScopeTargets(ctx, transaction, *admitted.Scope, db.IssueScopeTargets(ctx)); err != nil {
			return err
		}
		if !time.Now().UTC().Before(*admitted.ExpiresAt) {
			return db.ErrNotFound
		}
		return nil
	}
}

func scanAPIToken(row rowScanner) (db.APIToken, error) {
	var token db.APIToken
	var scopeKind, scopeProjectUID, scopeRootIssueUID sql.NullString
	var expiresAt, lastUsedAt, revokedAt storedNullTime
	err := row.Scan(
		&token.ID, &token.TokenHash, &token.Actor, &token.Name,
		&scopeKind, &scopeProjectUID, &scopeRootIssueUID, &expiresAt,
		(*storedTime)(&token.CreatedAt), &lastUsedAt, &revokedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.APIToken{}, db.ErrNotFound
	}
	if err != nil {
		return db.APIToken{}, fmt.Errorf("scan api token: %w", mapSQLError(err, nil))
	}
	token.LastUsedAt = lastUsedAt.Time
	token.RevokedAt = revokedAt.Time
	token.ExpiresAt = expiresAt.Time
	if scopeKind.Valid || scopeProjectUID.Valid || scopeRootIssueUID.Valid || token.ExpiresAt != nil {
		token.Scope = &db.APITokenScope{
			Kind:         db.APITokenScopeKind(scopeKind.String),
			ProjectUID:   scopeProjectUID.String,
			RootIssueUID: scopeRootIssueUID.String,
		}
	}
	if err := db.ValidateAPITokenGrant(token.Scope, token.ExpiresAt); err != nil {
		return db.APIToken{}, fmt.Errorf("scan api token grant: %w", err)
	}
	return token, nil
}

func apiTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
