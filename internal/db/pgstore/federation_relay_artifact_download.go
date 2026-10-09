package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"reflect"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// RelayArtifactPayloads downloads complete misses, never index approximations.
// Emission is a prerequisite, not authority: every download rechecks the grant.
func (d *Store) RelayArtifactPayloads(ctx context.Context, bindingUID string, epoch int64, digests []string) ([]embedding.EmbeddingArtifact, error) {
	ctx = db.WithRelayRequestedEpoch(ctx, epoch)
	if len(digests) < 1 || len(digests) > 32 {
		return nil, db.ErrFederationIngestValidation
	}
	var artifacts []embedding.EmbeddingArtifact
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		artifacts = nil
		grant, err := d.relayGrantTx(ctx, tx, bindingUID, "pull")
		if err != nil {
			return err
		}
		if epoch != grant.RelayResetEpoch {
			return db.ErrFederationIngestValidation
		}
		var projectUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL`, *grant.ProjectID).Scan(&projectUID); err != nil {
			return db.ErrNotFound
		}
		if !db.ProjectAttributionVisible(ctx, projectUID) {
			return db.ErrNotFound
		}
		seen := make(map[string]bool, len(digests))
		var total int64
		for _, digest := range digests {
			if len(digest) != 64 || seen[digest] {
				return db.ErrFederationIngestValidation
			}
			seen[digest] = true
			var raw, offered string
			var bytes, issueID int64
			var deletedAt sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT a.artifact,a.vector_byte_size,o.envelope,i.id,i.deleted_at FROM federation_embedding_artifacts a JOIN federation_relay_outbox o ON o.project_uid=a.project_uid AND o.source_hash=a.digest JOIN issues i ON i.uid=a.issue_uid AND i.project_id=$1 WHERE a.project_uid=$2 AND a.digest=$3 AND a.staging_expires_at IS NULL AND o.binding_uid=$4 AND o.stream='artifacts' AND o.reset_epoch=$5 AND o.emitted=1 AND o.acknowledged=0`, *grant.ProjectID, projectUID, digest, bindingUID, epoch).Scan(&raw, &bytes, &offered, &issueID, &deletedAt)
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrNotFound
			}
			if err != nil {
				return err
			}
			db.RecordIssueScopeTarget(ctx, issueID)
			if err := db.ApplyTransactionFence(ctx, tx); err != nil {
				return err
			}
			if deletedAt.Valid {
				return db.ErrEmbeddingArtifactMiss
			}
			if bytes < 1 || bytes > embedding.MaxArtifactVectorBytes || bytes > embedding.MaxArtifactBatchBytes-total {
				return db.ErrFederationIngestValidation
			}
			total += bytes
			var artifact embedding.EmbeddingArtifact
			if json.Unmarshal([]byte(raw), &artifact, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifact(artifact, "", "") != nil {
				return db.ErrFederationIngestValidation
			}
			var envelope db.RelayEnvelope
			var manifest embedding.ArtifactManifest
			if json.Unmarshal([]byte(offered), &envelope, json.RejectUnknownMembers(true)) != nil || json.Unmarshal(envelope.Body, &manifest, json.RejectUnknownMembers(true)) != nil {
				return db.ErrFederationIngestValidation
			}
			if artifact.ProjectUID != projectUID || artifact.Digest != digest || !db.ValidRelayArtifactSourceUID(envelope.SourceUID, digest) || envelope.SourceHash != digest || !reflect.DeepEqual(artifact.Manifest(), manifest) || artifact.Manifest().VectorByteSize != bytes {
				return db.ErrFederationIngestValidation
			}
			artifacts = append(artifacts, artifact)
		}
		_, err = d.relayGrantTx(ctx, tx, bindingUID, "pull")
		return err
	})
	if err != nil {
		return nil, err
	}
	return artifacts, nil
}
