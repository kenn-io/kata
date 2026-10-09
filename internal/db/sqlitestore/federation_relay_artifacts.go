package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// Manifests and complete misses use the existing inbox and artifact cursor.
// A durable hit after a miss is retained, but cannot skip that pending offer.
func (d *Store) acceptRelayArtifacts(ctx context.Context, bindingUID string, batch db.RelayBatch) (db.RelayAcceptance, error) {
	if len(batch.Envelopes) > 0 {
		ctx = db.WithRelayRequestedEpoch(ctx, batch.Envelopes[0].Epoch)
	}
	if batch.After < 0 || len(batch.Envelopes) > 32 || len(batch.Artifacts) > len(batch.Envelopes) {
		return db.RelayAcceptance{}, db.ErrFederationIngestValidation
	}
	var result db.RelayAcceptance
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.RelayAcceptance{}
		grant, err := d.relayGrantTx(ctx, tx, bindingUID, "push")
		if err != nil {
			return err
		}
		var projectUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL`, *grant.ProjectID).Scan(&projectUID); err != nil {
			return db.ErrNotFound
		}
		if !db.ProjectAttributionVisible(ctx, projectUID) {
			return db.ErrNotFound
		}
		pin, err := rootPinTx(ctx, tx, projectUID, "")
		if err != nil {
			return err
		}
		authority := db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: projectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: grant.SpokeInstanceUID, ReceiverInstanceUID: d.instanceUID, Epoch: grant.RelayResetEpoch}
		payloads := make(map[string]embedding.EmbeddingArtifact, len(batch.Artifacts))
		var payloadBytes int64
		for _, artifact := range batch.Artifacts {
			if artifact.ProjectUID != projectUID {
				return db.ErrFederationIngestValidation
			}
			if err := embedding.ValidateArtifact(artifact, "", ""); err != nil {
				return db.ErrFederationIngestValidation
			}
			if _, duplicate := payloads[artifact.Digest]; duplicate {
				return db.ErrFederationIngestValidation
			}
			payloadBytes += artifact.Manifest().VectorByteSize
			if payloadBytes > embedding.MaxArtifactBatchBytes {
				return db.ErrFederationIngestValidation
			}
			payloads[artifact.Digest] = artifact
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_cursors(project_uid,binding_uid,stream,reset_epoch) VALUES(?,?,?,?) ON CONFLICT(binding_uid,stream,reset_epoch) DO NOTHING`, projectUID, bindingUID, batch.Stream, grant.RelayResetEpoch); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT accepted_through FROM federation_relay_cursors WHERE binding_uid=? AND stream=? AND reset_epoch=?`, bindingUID, batch.Stream, grant.RelayResetEpoch).Scan(&result.Through); err != nil {
			return err
		}
		if batch.After > result.Through {
			return db.ErrFederationIngestValidation
		}
		previous := batch.After
		var declaredBytes int64
		manifestBytes := 0
		blocked := false
		for _, envelope := range batch.Envelopes {
			if envelope.Stream != batch.Stream || envelope.Sequence <= previous || len(envelope.Body) > (2<<20)-manifestBytes {
				return db.ErrFederationIngestValidation
			}
			previous = envelope.Sequence
			manifestBytes += len(envelope.Body)
			if err := db.ValidateRelayEnvelope(authority, envelope); err != nil {
				return err
			}
			var manifest embedding.ArtifactManifest
			if err := json.Unmarshal(envelope.Body, &manifest, json.RejectUnknownMembers(true)); err != nil {
				return db.ErrFederationIngestValidation
			}
			if err := embedding.ValidateArtifactManifest(manifest); err != nil {
				return db.ErrFederationIngestValidation
			}
			if manifest.ProjectUID != projectUID || !db.ValidRelayArtifactSourceUID(envelope.SourceUID, manifest.Digest) || manifest.Digest != envelope.SourceHash {
				return db.ErrFederationIngestValidation
			}
			declaredBytes += manifest.VectorByteSize
			if declaredBytes > embedding.MaxArtifactBatchBytes {
				return db.ErrFederationIngestValidation
			}
			var retainedDigest string
			var accepted int
			err := tx.QueryRowContext(ctx, `SELECT envelope_digest,accepted FROM federation_relay_inbox WHERE binding_uid=? AND stream=? AND reset_epoch=? AND sequence=?`, bindingUID, batch.Stream, grant.RelayResetEpoch, envelope.Sequence).Scan(&retainedDigest, &accepted)
			if err == nil && retainedDigest != envelope.Digest {
				return db.ErrFederationIngestValidation
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if envelope.Sequence <= result.Through && (err != nil || accepted != 1) {
				return db.ErrFederationIngestValidation
			}
			if err := validateRelaySourceSequence(ctx, tx, bindingUID, batch.Stream, grant.RelayResetEpoch, envelope.Sequence, envelope.SourceUID); err != nil {
				return err
			}
			// A known tombstone retires this exact offer, without claiming
			// durable vector presence. Unknown content still stages/retries.
			var issueID int64
			var deletedAt sql.NullString
			issueErr := tx.QueryRowContext(ctx, `SELECT id,deleted_at FROM issues WHERE project_id=? AND uid=?`, *grant.ProjectID, manifest.IssueUID).Scan(&issueID, &deletedAt)
			if issueErr != nil && !errors.Is(issueErr, sql.ErrNoRows) {
				return issueErr
			}
			retired := issueErr == nil && deletedAt.Valid
			if retired {
				db.RecordIssueScopeTarget(ctx, issueID)
				if err := db.ApplyTransactionFence(ctx, tx); err != nil {
					return err
				}
			}
			err = nil
			durable := false
			if artifact, ok := payloads[manifest.Digest]; ok {
				if !reflect.DeepEqual(artifact.Manifest(), manifest) {
					return db.ErrFederationIngestValidation
				}
				ingressCtx := db.WithRelayArtifactSourceUID(db.WithRelayForwardPath(ctx, append(slices.Clone(envelope.Path), d.instanceUID)), envelope.SourceUID)
				if !retired {
					durable, err = d.retainEmbeddingArtifactTx(ingressCtx, tx, artifact, true)
				}
				if err != nil && !errors.Is(err, db.ErrEmbeddingArtifactMiss) {
					return err
				}
				delete(payloads, manifest.Digest)
			} else if !retired {
				var raw string
				err := tx.QueryRowContext(ctx, `SELECT artifact FROM federation_embedding_artifacts WHERE project_uid=? AND digest=? AND (staging_expires_at IS NULL OR staging_expires_at>?)`, projectUID, manifest.Digest, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&raw)
				if err == nil {
					var artifact embedding.EmbeddingArtifact
					if json.Unmarshal([]byte(raw), &artifact, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifact(artifact, "", "") != nil || !reflect.DeepEqual(artifact.Manifest(), manifest) {
						return db.ErrFederationIngestValidation
					}
					// Reuse exact retained bytes under the same live issue/project
					// fence. Staging becomes durable only after content exists.
					ingressCtx := db.WithRelayArtifactSourceUID(db.WithRelayForwardPath(ctx, append(slices.Clone(envelope.Path), d.instanceUID)), envelope.SourceUID)
					durable, err = d.retainEmbeddingArtifactTx(ingressCtx, tx, artifact, true)
					if err != nil && !errors.Is(err, db.ErrEmbeddingArtifactMiss) {
						return err
					}
				} else if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			settled := accepted == 1 || durable || retired
			if !settled {
				// Distinct emitted offers may reuse the same exact artifact.
				// Download its bytes once, while settling each envelope separately.
				if !slices.Contains(result.MissingDigests, manifest.Digest) {
					result.MissingDigests = append(result.MissingDigests, manifest.Digest)
				}
				blocked = true
			}
			raw, err := json.Marshal(envelope)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_inbox(project_uid,binding_uid,stream,reset_epoch,sequence,source_uid,source_hash,envelope_digest,envelope,accepted) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(binding_uid,stream,reset_epoch,sequence) DO UPDATE SET accepted=excluded.accepted`, projectUID, bindingUID, batch.Stream, grant.RelayResetEpoch, envelope.Sequence, envelope.SourceUID, envelope.SourceHash, envelope.Digest, string(raw), boolInt(settled)); err != nil {
				return err
			}
			if settled && !blocked && envelope.Sequence > result.Through {
				var earlierMiss bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_inbox WHERE binding_uid=? AND stream=? AND reset_epoch=? AND sequence>? AND sequence<? AND accepted=0)`, bindingUID, batch.Stream, grant.RelayResetEpoch, result.Through, envelope.Sequence).Scan(&earlierMiss); err != nil {
					return err
				}
				blocked = earlierMiss
				if !blocked {
					result.Through = envelope.Sequence
					result.Digest = envelope.Digest
				}
			}
		}
		if len(payloads) != 0 {
			return db.ErrFederationIngestValidation
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_relay_cursors SET offered_through=MAX(offered_through,?),accepted_through=? WHERE binding_uid=? AND stream=? AND reset_epoch=?`, previous, result.Through, bindingUID, batch.Stream, grant.RelayResetEpoch); err != nil {
			return err
		}
		if result.Digest == "" && result.Through > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT envelope_digest FROM federation_relay_inbox WHERE binding_uid=? AND stream=? AND reset_epoch=? AND sequence=?`, bindingUID, batch.Stream, grant.RelayResetEpoch, result.Through).Scan(&result.Digest); err != nil {
				return err
			}
		}
		_, err = d.relayGrantTx(ctx, tx, bindingUID, "push")
		return err
	})
	if err != nil {
		return db.RelayAcceptance{}, err
	}
	return result, nil
}
