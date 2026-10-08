package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// RetainEmbeddingArtifact commits complete portable bytes. Only an existing
// authorized issue can establish durable presence; pre-content staging is not
// an accepted relay prefix. Index compatibility is checked by the reconciler.
func (d *Store) RetainEmbeddingArtifact(ctx context.Context, artifact embedding.EmbeddingArtifact) (bool, error) {
	if !db.ProjectAttributionVisible(ctx, artifact.ProjectUID) {
		return false, db.ErrNotFound
	}
	durable := false
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		var err error
		durable, err = d.retainEmbeddingArtifactTx(ctx, tx, artifact)
		return err
	})
	return durable && err == nil, err
}

// The receiver uses the same transaction for portable bytes and hop acceptance.
func (d *Store) retainEmbeddingArtifactTx(ctx context.Context, tx *sql.Tx, artifact embedding.EmbeddingArtifact) (bool, error) {
	if !db.ProjectAttributionVisible(ctx, artifact.ProjectUID) {
		return false, db.ErrNotFound
	}
	durable := false
	err := func() error {
		durable = false
		var projectID, issueID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=? AND deleted_at IS NULL`, artifact.ProjectUID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrNotFound
			}
			return err
		}
		var title, body string
		var deletedAt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id,title,body,deleted_at FROM issues WHERE project_id=? AND uid=?`, projectID, artifact.IssueUID).Scan(&issueID, &title, &body, &deletedAt)
		contentExists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if contentExists {
			db.RecordIssueScopeTarget(ctx, issueID)
		}
		if err := db.ApplyTransactionFence(ctx, tx); err != nil {
			return err
		}
		if deletedAt.Valid {
			return db.ErrEmbeddingArtifactMiss
		}
		input := ""
		if contentExists {
			current := embedding.EmbedText(title, body)
			if artifact.InputHash == embedding.ArtifactInputHash(current) {
				input = current
			}
		}
		if err := embedding.ValidateArtifact(artifact, input, ""); err != nil {
			return fmt.Errorf("%w: %v", db.ErrFederationIngestValidation, err)
		}
		raw, err := json.Marshal(artifact)
		if err != nil {
			return err
		}
		manifest := artifact.Manifest()
		manifestRaw, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		if len(raw) > 32<<20 || len(manifestRaw) > 2<<20 {
			return db.ErrFederationIngestValidation
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `DELETE FROM federation_embedding_artifacts WHERE project_uid=? AND staging_expires_at IS NOT NULL AND staging_expires_at<=?`, artifact.ProjectUID, now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		var retained string
		var expiry sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT artifact,staging_expires_at FROM federation_embedding_artifacts WHERE project_uid=? AND digest=?`, artifact.ProjectUID, artifact.Digest).Scan(&retained, &expiry)
		if err == nil {
			if retained != string(raw) {
				return db.ErrRemoteEventConflict
			}
			if expiry.Valid && contentExists {
				if _, err := tx.ExecContext(ctx, `UPDATE federation_embedding_artifacts SET staging_expires_at=NULL WHERE project_uid=? AND digest=?`, artifact.ProjectUID, artifact.Digest); err != nil {
					return err
				}
			}
			durable = !expiry.Valid || contentExists
			if durable {
				return queueRelayBodyTx(ctx, tx, projectID, artifact.ProjectUID, db.RelayStreamArtifact, artifact.Digest, artifact.Digest, manifestRaw, d.instanceUID)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var expiresAt any
		if !contentExists {
			var count, bytes int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(artifact AS BLOB))+length(CAST(manifest AS BLOB))),0) FROM federation_embedding_artifacts WHERE project_uid=? AND staging_expires_at IS NOT NULL`, artifact.ProjectUID).Scan(&count, &bytes); err != nil {
				return err
			}
			if count >= db.MaxStagedEmbeddingArtifacts || int64(len(raw))+int64(len(manifestRaw)) > db.MaxStagedEmbeddingBytes-bytes {
				return db.ErrEmbeddingArtifactMiss
			}
			expiresAt = now.Add(db.EmbeddingArtifactStagingLifetime).Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_embedding_artifacts(project_uid,digest,issue_uid,input_hash,recipe_fingerprint,vector_byte_size,manifest,artifact,staging_expires_at) VALUES(?,?,?,?,?,?,?,?,?)`, artifact.ProjectUID, artifact.Digest, artifact.IssueUID, artifact.InputHash, artifact.RecipeFingerprint, manifest.VectorByteSize, string(manifestRaw), string(raw), expiresAt); err != nil {
			return err
		}
		durable = contentExists
		if durable {
			return queueRelayBodyTx(ctx, tx, projectID, artifact.ProjectUID, db.RelayStreamArtifact, artifact.Digest, artifact.Digest, manifestRaw, d.instanceUID)
		}
		return nil
	}()
	return durable && err == nil, err
}

// StoredEmbeddingArtifact reads a durably retained, validated artifact by its exact digest.
func (d *Store) StoredEmbeddingArtifact(ctx context.Context, projectUID, digest string) (embedding.EmbeddingArtifact, error) {
	var artifact embedding.EmbeddingArtifact
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return artifact, db.ErrNotFound
	}
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT a.artifact FROM federation_embedding_artifacts a JOIN projects p ON p.uid=a.project_uid WHERE a.project_uid=? AND a.digest=? AND a.staging_expires_at IS NULL AND p.deleted_at IS NULL`, projectUID, digest).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrNotFound
			}
			return err
		}
		if err := json.Unmarshal([]byte(raw), &artifact, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if artifact.ProjectUID != projectUID || artifact.Digest != digest {
			return db.ErrFederationIngestValidation
		}
		return embedding.ValidateArtifact(artifact, "", "")
	})
	return artifact, err
}

// EmbeddingArtifactManifests exposes bounded descriptors, never vector bytes.
func (d *Store) EmbeddingArtifactManifests(ctx context.Context, projectUID string, limit int) ([]embedding.ArtifactManifest, error) {
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return nil, db.ErrNotFound
	}
	if limit < 1 || limit > 32 {
		return nil, db.ErrFederationIngestValidation
	}
	manifests := []embedding.ArtifactManifest{}
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		manifests = []embedding.ArtifactManifest{}
		rows, err := tx.QueryContext(ctx, `SELECT a.manifest FROM federation_embedding_artifacts a
			JOIN projects p ON p.uid=a.project_uid
			JOIN issues i ON i.uid=a.issue_uid AND i.project_id=p.id
			WHERE a.project_uid=? AND a.staging_expires_at IS NULL AND p.deleted_at IS NULL AND i.deleted_at IS NULL
			ORDER BY a.digest LIMIT ?`, projectUID, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var manifest embedding.ArtifactManifest
			if err := json.Unmarshal([]byte(raw), &manifest, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			if manifest.ProjectUID != projectUID {
				return db.ErrFederationIngestValidation
			}
			if err := embedding.ValidateArtifactManifest(manifest); err != nil {
				return err
			}
			manifests = append(manifests, manifest)
		}
		return rows.Err()
	})
	return manifests, err
}

// ExportEmbeddingArtifacts uses the export snapshot and selected project fence.
func (d *Store) ExportEmbeddingArtifacts(ctx context.Context, filter db.ExportFilter, issueUIDs ...string) iter.Seq2[db.ImportRecord, error] {
	args := []any{}
	bind := func(value any) string { args = append(args, value); return "?" }
	query := `SELECT a.project_uid,a.digest,a.artifact FROM federation_embedding_artifacts a JOIN projects p ON p.uid=a.project_uid JOIN issues i ON i.uid=a.issue_uid AND i.project_id=p.id WHERE a.staging_expires_at IS NULL AND ` + db.AuthorizedProjectPredicate(ctx, "p.uid", bind)
	// The embedding worker supplies only its bounded pending-page UIDs.
	// Apply this before reading, decoding or hashing unrelated portable bytes.
	if len(issueUIDs) > 0 {
		placeholders := make([]string, len(issueUIDs))
		for i, issueUID := range issueUIDs {
			placeholders[i] = bind(issueUID)
		}
		query += " AND a.issue_uid IN (" + strings.Join(placeholders, ",") + ")"
	}
	if filter.ProjectID != nil {
		query += " AND p.id=" + bind(*filter.ProjectID)
	}
	if !filter.IncludeDeleted {
		query += " AND p.deleted_at IS NULL AND i.deleted_at IS NULL"
	}
	query += " ORDER BY a.project_uid,a.digest"
	scan := func(rows *sql.Rows) (db.ImportRecord, error) {
		var projectUID, digest, raw string
		if err := rows.Scan(&projectUID, &digest, &raw); err != nil {
			return nil, err
		}
		var record db.EmbeddingArtifactExport
		if err := json.Unmarshal([]byte(raw), &record.EmbeddingArtifact, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		if record.ProjectUID != projectUID || record.Digest != digest {
			return nil, db.ErrFederationIngestValidation
		}
		if err := embedding.ValidateArtifact(record.EmbeddingArtifact, "", ""); err != nil {
			return nil, err
		}
		return &record, nil
	}
	return streamRows(ctx, d.readQ, "embedding artifacts", query, args, scan)
}

// Owner replay runs inside the existing atomic restore transaction and retains
// valid stale/deleted artifacts without attempting backend index conversion.
func importEmbeddingArtifact(ctx context.Context, tx *sql.Tx, record *db.EmbeddingArtifactExport) error {
	artifact := record.EmbeddingArtifact
	var title, body string
	if err := tx.QueryRowContext(ctx, `SELECT i.title,i.body FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.uid=? AND i.uid=?`, artifact.ProjectUID, artifact.IssueUID).Scan(&title, &body); err != nil {
		return err
	}
	input := embedding.EmbedText(title, body)
	if artifact.InputHash != embedding.ArtifactInputHash(input) {
		input = ""
	}
	if err := embedding.ValidateArtifact(artifact, input, ""); err != nil {
		return err
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	manifest := artifact.Manifest()
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO federation_embedding_artifacts(project_uid,digest,issue_uid,input_hash,recipe_fingerprint,vector_byte_size,manifest,artifact) VALUES(?,?,?,?,?,?,?,?)`, artifact.ProjectUID, artifact.Digest, artifact.IssueUID, artifact.InputHash, artifact.RecipeFingerprint, manifest.VectorByteSize, string(manifestRaw), string(raw))
	return err
}
