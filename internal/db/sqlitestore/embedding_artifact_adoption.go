package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// The project UID FK is immediate. Stage bytes in a transaction-local temporary
// table before replacing the parent, then restore one bounded artifact at a time.
// No persisted staging table or schema change is needed; rollback restores both.
func stageArtifactAdoptionTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE _kata_artifact_adoption AS SELECT a.* FROM federation_embedding_artifacts a JOIN projects p ON p.uid=a.project_uid WHERE p.id=?`, projectID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM federation_embedding_artifacts WHERE project_uid=(SELECT uid FROM projects WHERE id=?)`, projectID)
	return err
}

func restoreArtifactAdoptionTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	after := ""
	for {
		var oldUID, digest, raw string
		var expiry sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT project_uid,digest,artifact,staging_expires_at FROM temp._kata_artifact_adoption WHERE digest>? ORDER BY digest LIMIT 1`, after).Scan(&oldUID, &digest, &raw, &expiry)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		after = digest
		var artifact embedding.EmbeddingArtifact
		if json.Unmarshal([]byte(raw), &artifact, json.RejectUnknownMembers(true)) != nil || artifact.ProjectUID != oldUID || artifact.Digest != digest {
			return db.ErrFederationIngestValidation
		}
		artifact, err = embedding.RebindArtifactProject(artifact, projectUID)
		if err != nil {
			return err
		}
		body, err := json.Marshal(artifact)
		if err != nil {
			return err
		}
		manifest := artifact.Manifest()
		metadata, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		var expiresAt any
		if expiry.Valid {
			expiresAt = expiry.String
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_embedding_artifacts(project_uid,digest,issue_uid,input_hash,recipe_fingerprint,vector_byte_size,manifest,artifact,staging_expires_at) VALUES(?,?,?,?,?,?,?,?,?)`, projectUID, artifact.Digest, artifact.IssueUID, artifact.InputHash, artifact.RecipeFingerprint, manifest.VectorByteSize, string(metadata), string(body), expiresAt); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DROP TABLE temp._kata_artifact_adoption`)
	return err
}
