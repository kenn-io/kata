package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/db"
)

// AcceptRelayDeliveries retains source and hop identities in the transaction
// that materializes the domain state and queues onward delivery. The live hop
// comes from storage; none of the envelope's labels establish authority.
func (d *Store) AcceptRelayDeliveries(ctx context.Context, bindingUID string, batch db.RelayBatch) (db.RelayAcceptance, error) {
	if len(batch.Envelopes) > 0 {
		ctx = db.WithRelayRequestedEpoch(ctx, batch.Envelopes[0].Epoch)
	}
	if batch.Stream == db.RelayStreamArtifact {
		return d.acceptRelayArtifacts(ctx, bindingUID, batch)
	}
	if len(batch.Artifacts) != 0 {
		return db.RelayAcceptance{}, db.ErrFederationIngestValidation
	}
	if batch.After < 0 || len(batch.Envelopes) > 1024 || (batch.Stream != db.RelayStreamEvent && batch.Stream != db.RelayStreamReceipt) {
		return db.RelayAcceptance{}, fmt.Errorf("%w: invalid relay batch", db.ErrFederationIngestValidation)
	}
	total := 0
	previous := batch.After
	for _, envelope := range batch.Envelopes {
		if envelope.Stream != batch.Stream || envelope.Sequence <= previous || len(envelope.Body) > db.MaxRelayEnvelopeBytes-total {
			return db.RelayAcceptance{}, fmt.Errorf("%w: invalid relay offer order or batch size", db.ErrFederationIngestValidation)
		}
		previous = envelope.Sequence
		total += len(envelope.Body)
	}
	var result db.RelayAcceptance
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.RelayAcceptance{}
		grant, err := d.relayGrantTx(ctx, tx, bindingUID, "push")
		if err != nil {
			return err
		}
		projectID := *grant.ProjectID
		var projectUID, projectName string
		if err := tx.QueryRowContext(ctx, `SELECT uid,name FROM projects WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, projectID).Scan(&projectUID, &projectName); err != nil {
			return db.RelayProjectLookupError(err)
		}
		if !db.ProjectAttributionVisible(ctx, projectUID) {
			return db.ErrNotFound
		}
		pin, err := rootPinTx(ctx, tx, projectUID, "")
		if err != nil {
			return err
		}
		authority := db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: projectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: grant.SpokeInstanceUID, ReceiverInstanceUID: d.instanceUID, Epoch: grant.RelayResetEpoch}
		checkPushPath, err := relayPushPathCheckTx(ctx, tx, grant, pin.AuthorityUID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_cursors(project_uid,binding_uid,stream,reset_epoch) VALUES($1,$2,$3,$4) ON CONFLICT(binding_uid,stream,reset_epoch) DO NOTHING`, projectUID, bindingUID, batch.Stream, grant.RelayResetEpoch); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT accepted_through FROM federation_relay_cursors WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 FOR UPDATE`, bindingUID, batch.Stream, grant.RelayResetEpoch).Scan(&result.Through); err != nil {
			return err
		}
		if batch.After > result.Through {
			return fmt.Errorf("%w: relay offer skips the accepted prefix", db.ErrFederationIngestValidation)
		}
		// Folding new ingress must not prune current rows whose replay source
		// was compacted. Capture the root's existing native baseline first.
		if batch.Stream == db.RelayStreamEvent && pin.AuthorityUID == d.instanceUID && len(batch.Envelopes) > 0 {
			var missing bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues i WHERE i.project_id=$1 AND NOT EXISTS(SELECT 1 FROM events e WHERE e.project_id=$1 AND e.issue_uid=i.uid AND e.type IN ('issue.created','issue.snapshot')))`, projectID).Scan(&missing); err != nil {
				return err
			}
			if missing {
				project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, projectID))
				if err != nil {
					return err
				}
				if _, err := d.insertFederationBaselineEventsTx(db.WithRelayStateCapture(ctx), tx, project, grant.Actor); err != nil {
					return err
				}
			}
		}
		known, err := currentFederatedIssueUIDSet(ctx, tx, projectID)
		if err != nil {
			return err
		}
		// Relay ingress applies the same cron replay identity checks as legacy
		// ingest: a definition version or run reference must not name an
		// unrelated event or another project's definition.
		cronValidator := db.NewCronReplayValidator(tx, true)
		linksAffected := false
		for _, envelope := range batch.Envelopes {
			if err := db.ValidateRelayEnvelope(authority, envelope); err != nil {
				return err
			}
			if err := checkPushPath(envelope.Path); err != nil {
				return err
			}
			var retained string
			var accepted int
			err := tx.QueryRowContext(ctx, `SELECT envelope_digest,accepted FROM federation_relay_inbox WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND sequence=$4`, bindingUID, batch.Stream, grant.RelayResetEpoch, envelope.Sequence).Scan(&retained, &accepted)
			if err == nil {
				if retained != envelope.Digest || accepted != 1 || envelope.Sequence > result.Through {
					return fmt.Errorf("%w: relay retry differs from retained acceptance", db.ErrFederationIngestValidation)
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if envelope.Sequence <= result.Through {
				return fmt.Errorf("%w: relay retry is absent from accepted prefix", db.ErrFederationIngestValidation)
			}
			if err := validateRelaySourceSequence(ctx, tx, bindingUID, batch.Stream, grant.RelayResetEpoch, envelope.Sequence, envelope.SourceUID); err != nil {
				return err
			}
			path := append(slices.Clone(envelope.Path), d.instanceUID)
			ingressCtx := db.WithRelayForwardPath(ctx, path)
			switch batch.Stream {
			case db.RelayStreamEvent:
				source, err := db.DecodeRelaySourceEvent(envelope.Body)
				if err != nil {
					return err
				}
				if source.EventUID != envelope.SourceUID || source.ContentHash != envelope.SourceHash || source.ProjectUID != projectUID || source.OriginInstanceUID != envelope.Path[0] {
					return fmt.Errorf("%w: relay source differs from its hop commitment", db.ErrFederationIngestValidation)
				}
				if grant.ID > 0 && !grant.RelayServeDownstream && (source.OriginInstanceUID != grant.SpokeInstanceUID || len(envelope.Path) != 1) {
					return fmt.Errorf("%w: leaf enrollment cannot forward another instance's source", db.ErrFederationIngestValidation)
				}
				// The project/event validator is reused after the hop authenticates origin;
				// the legacy direct-spoke actor/origin contract remains unchanged.
				if err := validateFederationProjectEvent(projectUID, source.OriginInstanceUID, source, known, true); err != nil {
					return err
				}
				if err := validateRelayLinkEndpointsInProject(ctx, tx, projectID, source); err != nil {
					return err
				}
				producerAuthorityUID := pin.AuthorityUID
				if grant.ID > 0 {
					// Enrollment authenticates the direct downstream peer, not the
					// source identity it asserts in a forwarded path.
					producerAuthorityUID = ""
				}
				if err := db.ValidateEmbeddingProducerEvent(source, producerAuthorityUID); err != nil {
					return err
				}
				if err := db.ValidateFederationEntries(source.Type, source.EventUID, source.Payload); err != nil {
					return err
				}
				existing, err := federationEventHashByUID(ctx, tx, source.EventUID)
				if err == nil && existing != source.ContentHash {
					return db.ErrRemoteEventConflict
				}
				if err != nil && !errors.Is(err, db.ErrNotFound) {
					return err
				}
				sourceInserted := false
				if errors.Is(err, db.ErrNotFound) {
					if err := cronValidator.Validate(ctx, projectID, source); err != nil {
						return err
					}
					clock := db.EventHLCTimestamp{PhysicalMS: source.HLCPhysicalMS, Counter: source.HLCCounter}
					insertedEvent, err := d.insertEventTx(ingressCtx, tx, eventInsert{ProjectID: projectID, ProjectUID: projectUID, ProjectName: source.ProjectName, IssueUID: source.IssueUID, RelatedIssueUID: source.RelatedIssueUID, Type: source.Type, Actor: source.Actor, Payload: string(source.Payload), UID: source.EventUID, OriginInstanceUID: source.OriginInstanceUID, HLC: &clock, CreatedAt: formatStoredTime(source.CreatedAt), ContentHash: source.ContentHash})
					if err != nil {
						return err
					}
					result.InsertedEventUIDs = append(result.InsertedEventUIDs, source.EventUID)
					result.InsertedEvents = append(result.InsertedEvents, insertedEvent)
					linksAffected = linksAffected || db.FederationEventAffectsLinks(source.Type)
					sourceInserted = true
				}
				rememberIngestIssueUIDs(source, known)
				if pin.AuthorityUID == d.instanceUID {
					signer, _, ok := db.RootAttributionFromContext(ctx)
					if !ok {
						return errors.New("root relay acceptance requires local signing authority")
					}
					// Root receipts start a new root-origin path, so the accepted source's
					// ingress receives its proof while its own event is never echoed back.
					if _, err := d.recordRootAttributionTx(db.WithRelayForwardPath(ctx, nil), tx, grant.ID, source, signer); err != nil {
						return err
					}
					if sourceInserted {
						claimEvents, err := d.annotateFederationIngestClaimWorkTx(ctx, tx, projectID, source, db.ClaimPrincipal{
							HolderInstanceUID: grant.SpokeInstanceUID,
							Holder:            grant.Actor,
						})
						if err != nil {
							return err
						}
						for _, event := range claimEvents {
							result.InsertedEventUIDs = append(result.InsertedEventUIDs, event.UID)
							result.InsertedEvents = append(result.InsertedEvents, event)
						}
					}
				}
			case db.RelayStreamReceipt:
				var receipt db.AttributionReceipt
				if err := json.Unmarshal(envelope.Body, &receipt, json.RejectUnknownMembers(true)); err != nil {
					return fmt.Errorf("%w: invalid relay receipt", db.ErrFederationIngestValidation)
				}
				if receipt.ProjectUID != projectUID || receipt.EventUID != envelope.SourceUID || receipt.ContentHash != envelope.SourceHash {
					return fmt.Errorf("%w: relay receipt differs from hop commitment", db.ErrFederationIngestValidation)
				}
				historical, err := rootPinTx(ctx, tx, projectUID, receipt.KeyID)
				if err != nil {
					return err
				}
				if err := db.VerifyRootReceipt(historical, receipt); err != nil {
					return fmt.Errorf("%w: invalid relay receipt signature", db.ErrFederationIngestValidation)
				}
				source, err := attributionSourceTx(ctx, tx, projectID, receipt)
				var refs []db.EntityProvenance
				if err == nil {
					handle, creation, e := db.EventCreationProvenance(source)
					if e != nil {
						return e
					}
					if source.ContentHash != receipt.ContentHash || source.Actor != receipt.SourceActor || handle != receipt.Teammate {
						return db.ErrRemoteEventHashMismatch
					}
					refs = creation
				} else if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err := persistReceiptTx(ingressCtx, tx, receipt, refs, true); err != nil {
					return err
				}
			}
			raw, err := json.Marshal(envelope)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_inbox(project_uid,binding_uid,stream,reset_epoch,sequence,source_uid,source_hash,envelope_digest,envelope,accepted) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,1)`, projectUID, bindingUID, batch.Stream, grant.RelayResetEpoch, envelope.Sequence, envelope.SourceUID, envelope.SourceHash, envelope.Digest, string(raw)); err != nil {
				return err
			}
			result.Through = envelope.Sequence
			result.Digest = envelope.Digest
		}
		if len(result.InsertedEventUIDs) > 0 {
			if err := d.materializeFederatedProjectRelayTx(ctx, tx, projectID, linksAffected, result.InsertedEventUIDs, cronValidator); err != nil {
				return err
			}
			// Relayed rows are stored by issue UID; name the local issues the
			// materialization created so published events and hooks resolve them.
			if err := resolveEventIssueIDsTx(ctx, tx, result.InsertedEvents); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_relay_cursors SET offered_through=GREATEST(offered_through,$1),accepted_through=$2 WHERE binding_uid=$3 AND stream=$4 AND reset_epoch=$5`, result.Through, result.Through, bindingUID, batch.Stream, grant.RelayResetEpoch); err != nil {
			return err
		}
		if result.Digest == "" && result.Through > 0 {
			err := tx.QueryRowContext(ctx, `SELECT envelope_digest FROM federation_relay_inbox WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND sequence=$4`, bindingUID, batch.Stream, grant.RelayResetEpoch, result.Through).Scan(&result.Digest)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		// Recheck time-dependent expiry immediately before commit; row/policy locks
		// retain credential, binding and membership authority for this transaction.
		_, err = d.relayGrantTx(ctx, tx, bindingUID, "push")
		return err
	})
	if err != nil {
		return db.RelayAcceptance{}, err
	}
	return result, nil
}

func validateRelaySourceSequence(ctx context.Context, tx *sql.Tx, bindingUID, stream string, epoch, sequence int64, sourceUID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_relay_inbox WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND source_uid=$4 AND sequence<>$5)`, bindingUID, stream, epoch, sourceUID, sequence).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: relay source UID already appears in this epoch", db.ErrFederationIngestValidation)
	}
	return nil
}

// relayPushPathCheckTx returns the path check for a push over grant. Pushes
// over this receiver's upstream binding need none; a downstream enrollment may
// not name the root or a hub above this receiver.
func relayPushPathCheckTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment, authorityUID string) (func([]string) error, error) {
	if grant.ID <= 0 {
		return func([]string) error { return nil }, nil
	}
	var upstream *db.RelayBindingConfig
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, *grant.ProjectID))
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, fmt.Errorf("reading relay upstream binding: %w", err)
	}
	if err == nil && binding.Role == db.FederationRoleSpoke {
		upstream = binding.RelayConfig
	}
	return func(path []string) error { return db.ValidateRelayDownstreamPath(authorityUID, upstream, path) }, nil
}

// resolveEventIssueIDsTx fills the local issue IDs of events stored by issue
// UID, reading the issues in the caller's transaction.
func resolveEventIssueIDsTx(ctx context.Context, tx *sql.Tx, events []db.Event) error {
	uids := db.EventIssueUIDs(events)
	if len(uids) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT uid,id FROM issues WHERE uid = ANY($1::text[])`, uids)
	if err != nil {
		return mapSQLError(err, nil)
	}
	local, err := scanIssueIDsByUID(rows)
	if err != nil {
		return mapSQLError(err, nil)
	}
	db.ResolveEventIssueIDs(events, local)
	return nil
}

func scanIssueIDsByUID(rows *sql.Rows) (map[string]int64, error) {
	defer func() { _ = rows.Close() }()
	local := map[string]int64{}
	for rows.Next() {
		var uid string
		var id int64
		if err := rows.Scan(&uid, &id); err != nil {
			return nil, err
		}
		local[uid] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return local, rows.Close()
}
