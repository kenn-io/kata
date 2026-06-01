package sqlitestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// EnsureSystemProject creates the hidden project used to anchor daemon-global
// token lifecycle events. It is idempotent so every Open can call it after the
// normal schema bootstrap path.
func (d *Store) EnsureSystemProject(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO projects(uid, name)
		VALUES(?, ?)
		ON CONFLICT(name) DO NOTHING
	`, db.SystemProjectUID, db.SystemProjectName)
	if err != nil {
		return fmt.Errorf("ensure system project: %w", err)
	}
	sys, err := d.SystemProject(ctx)
	if err != nil {
		return fmt.Errorf("ensure system project: %w", err)
	}
	if sys.UID != db.SystemProjectUID {
		return fmt.Errorf("ensure system project: %s has uid %q, want %q",
			db.SystemProjectName, sys.UID, db.SystemProjectUID)
	}
	return nil
}

// SystemProject returns the hidden project row for internal token-event code.
func (d *Store) SystemProject(ctx context.Context) (db.Project, error) {
	return d.projectByIDOrNameIncludingSystem(ctx, 0, db.SystemProjectName)
}

func (d *Store) projectByIDOrNameIncludingSystem(ctx context.Context, id int64, name string) (db.Project, error) {
	var row *sql.Row
	if id != 0 {
		row = d.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, id)
	} else {
		row = d.QueryRowContext(ctx, projectSelect+` WHERE name = ?`, name)
	}
	return scanProject(row)
}

func isSystemProject(p db.Project) bool {
	return p.UID == db.SystemProjectUID || p.Name == db.SystemProjectName
}

func hideSystemProject(p db.Project, err error) (db.Project, error) {
	if err != nil {
		return p, err
	}
	if isSystemProject(p) {
		return db.Project{}, db.ErrNotFound
	}
	return p, nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HashTokenForTest exposes the token hash contract to black-box db tests.
func HashTokenForTest(token string) string {
	return tokenHash(token)
}

// CreateAPIToken stores a hashed API token and appends its token.created event.
func (d *Store) CreateAPIToken(ctx context.Context, p db.CreateAPITokenParams) (db.APIToken, db.Event, error) {
	if strings.TrimSpace(p.PlaintextToken) == "" {
		return db.APIToken{}, db.Event{}, fmt.Errorf("token must be non-empty")
	}
	if err := db.ValidateTokenActor(p.Actor); err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	if strings.TrimSpace(p.AdminActor) == "" {
		return db.APIToken{}, db.Event{}, fmt.Errorf("admin actor must be non-empty")
	}
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return db.APIToken{}, db.Event{}, fmt.Errorf("token name must be non-empty")
		}
		p.Name = &name
	}
	sys, err := d.SystemProject(ctx)
	if err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("begin create api token: %w", err)
	}
	defer rollbackUnlessCommitted(tx)

	hash := tokenHash(p.PlaintextToken)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO api_tokens(token_hash, actor, name) VALUES(?, ?, ?)`,
		hash, strings.TrimSpace(p.Actor), nullableString(p.Name))
	if err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("insert api token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("api token last id: %w", err)
	}
	tok, err := scanAPIToken(tx.QueryRowContext(ctx, apiTokenSelect+` WHERE id = ?`, id))
	if err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	payload, err := json.Marshal(tokenCreatedPayload{
		TokenID:     tok.ID,
		TokenHash:   tok.TokenHash,
		TargetActor: tok.Actor,
		Name:        tok.Name,
	})
	if err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("marshal token.created payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   sys.ID,
		ProjectName: sys.Name,
		Type:        "token.created",
		Actor:       strings.TrimSpace(p.AdminActor),
		Payload:     string(payload),
	})
	if err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("commit create api token: %w", err)
	}
	return tok, evt, nil
}

// RevokeAPIToken revokes an active API token and appends its token.revoked event.
func (d *Store) RevokeAPIToken(ctx context.Context, id int64, adminActor string) (db.APIToken, db.Event, error) {
	if strings.TrimSpace(adminActor) == "" {
		return db.APIToken{}, db.Event{}, fmt.Errorf("admin actor must be non-empty")
	}
	sys, err := d.SystemProject(ctx)
	if err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("begin revoke api token: %w", err)
	}
	defer rollbackUnlessCommitted(tx)

	res, err := tx.ExecContext(ctx,
		`UPDATE api_tokens
		    SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		  WHERE id = ? AND revoked_at IS NULL`, id)
	if err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("revoke api token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("revoke api token rows affected: %w", err)
	}
	if n == 0 {
		return db.APIToken{}, db.Event{}, db.ErrNotFound
	}
	tok, err := scanAPIToken(tx.QueryRowContext(ctx, apiTokenSelect+` WHERE id = ?`, id))
	if err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	payload, err := json.Marshal(tokenRevokedPayload{
		TokenID:     tok.ID,
		TargetActor: tok.Actor,
		Name:        tok.Name,
	})
	if err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("marshal token.revoked payload: %w", err)
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   sys.ID,
		ProjectName: sys.Name,
		Type:        "token.revoked",
		Actor:       strings.TrimSpace(adminActor),
		Payload:     string(payload),
	})
	if err != nil {
		return db.APIToken{}, db.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.APIToken{}, db.Event{}, fmt.Errorf("commit revoke api token: %w", err)
	}
	return tok, evt, nil
}

// ResolveAPIToken resolves a plaintext bearer token to its active token row.
func (d *Store) ResolveAPIToken(ctx context.Context, plaintext string) (db.APIToken, error) {
	hash := tokenHash(plaintext)
	tok, err := scanAPIToken(d.QueryRowContext(ctx,
		apiTokenSelect+` WHERE token_hash = ? AND revoked_at IS NULL`,
		hash))
	if err != nil {
		return db.APIToken{}, err
	}
	res, err := d.ExecContext(ctx, `
		UPDATE api_tokens
		   SET last_used_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE token_hash = ?
		   AND revoked_at IS NULL
		   AND (
		     last_used_at IS NULL OR
		     last_used_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-1 hour')
		   )`, hash)
	if err != nil {
		return tok, nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return tok, nil
	}
	if n > 0 {
		tok, err = scanAPIToken(d.QueryRowContext(ctx, apiTokenSelect+` WHERE id = ?`, tok.ID))
		if err != nil {
			return tok, nil
		}
	}
	return tok, nil
}

// ListAPITokens returns redacted token metadata for token-admin listing.
func (d *Store) ListAPITokens(ctx context.Context) ([]db.APIToken, error) {
	rows, err := d.QueryContext(ctx, apiTokenSelect+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.APIToken
	for rows.Next() {
		tok, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		tok.TokenHash = ""
		out = append(out, tok)
	}
	return out, rows.Err()
}

const apiTokenSelect = `SELECT id, token_hash, actor, name, created_at, last_used_at, revoked_at FROM api_tokens` //nolint:gosec // SQL projection includes a token_hash column name, not a hardcoded secret.

func scanAPIToken(r rowScanner) (db.APIToken, error) {
	var tok db.APIToken
	err := r.Scan(&tok.ID, &tok.TokenHash, &tok.Actor, &tok.Name,
		&tok.CreatedAt, &tok.LastUsedAt, &tok.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.APIToken{}, db.ErrNotFound
	}
	if err != nil {
		return db.APIToken{}, fmt.Errorf("scan api token: %w", err)
	}
	return tok, nil
}

type tokenCreatedPayload struct {
	TokenID     int64   `json:"token_id"`
	TokenHash   string  `json:"token_hash"`
	TargetActor string  `json:"target_actor"`
	Name        *string `json:"name,omitempty"`
}

type tokenRevokedPayload struct {
	TokenID     int64   `json:"token_id"`
	TargetActor string  `json:"target_actor"`
	Name        *string `json:"name,omitempty"`
}

func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func rollbackUnlessCommitted(tx *sql.Tx) {
	_ = tx.Rollback()
}
