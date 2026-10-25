package pgstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"reflect"
	"slices"
	"strings"
)

func (s *Store) scopedProjectOwnerTx(ctx context.Context, tx db.Transaction, project string) (db.ScopedProjectExport, error) {
	var owner db.ScopedProjectExport
	var fenced int
	err := tx.QueryRowContext(ctx, `SELECT project_uid,owner_view_uid,owner_generation,protocol_floor,root_cutover_uid,conversion_uid,serving_fenced FROM federation_scoped_projects WHERE project_uid=$1`, project).Scan(&owner.ProjectUID, &owner.OwnerViewUID, &owner.OwnerGeneration, &owner.ProtocolFloor, &owner.RootCutoverUID, &owner.ConversionUID, &fenced)
	if err != nil {
		return owner, scopedAuthorityLookupError(err)
	}
	if fenced != 0 && fenced != 1 {
		return owner, errors.New("invalid scoped serving fence")
	}
	owner.ServingFenced = fenced == 1
	return owner, db.ValidateScopedImportRecord(&owner)
}

func (s *Store) checkScopedOwnerFenceTx(ctx context.Context, tx db.Transaction, want db.ScopedOwnerFence) error {
	// Transport/native account authority is distinct from a narrow caller's
	// candidate ceiling. The caller ceiling is intersected separately below.
	authorityCtx := db.WithAuthorizedProjects(ctx, nil)
	view, err := s.scopedAuthorityTx(authorityCtx, tx, want.View.Key, "pull")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(view, want.View) || view.DestinationUID != s.instanceUID || len(want.Owners) != len(view.Projects) {
		return db.ErrNotFound
	}
	_, row, err := s.scopedDescriptorTx(ctx, tx, view.Key)
	if err != nil {
		return err
	}
	if row.State != "active" || row.ActiveGeneration == nil || *row.ActiveGeneration != view.Key.Generation {
		return db.ErrNotFound
	}
	for n, selected := range view.Projects {
		owner, err := s.scopedProjectOwnerTx(ctx, tx, selected.ProjectUID)
		if err != nil {
			return err
		}
		if owner.ServingFenced || owner.OwnerViewUID == nil || owner.OwnerGeneration == nil || *owner.OwnerViewUID != view.Key.ViewUID || *owner.OwnerGeneration != view.Key.Generation || owner.ProtocolFloor != db.ScopedRelayProtocolVersion || owner.RootCutoverUID == nil || *owner.RootCutoverUID != view.RootCutoverUID || !reflect.DeepEqual(owner, want.Owners[n]) {
			return db.ErrNotFound
		}
	}
	// Cut is the consistent snapshot identity. A newer broad cut does not
	// invalidate still-authorized output or reveal hidden activity via a reset.
	return nil
}

func (s *Store) scopedReadTransaction(ctx context.Context, read func(*sql.Tx) error) error {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := read(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) scopedPreparationAuthorityTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey) (db.ScopedViewDescriptor, db.ScopedViewExport, error) {
	view, row, err := s.scopedDescriptorTx(ctx, tx, key)
	if err != nil {
		return view, row, err
	}
	authorityKey := key
	if view.DestinationUID == s.instanceUID && row.ActiveGeneration != nil && *row.ActiveGeneration != key.Generation {
		authorityKey.Generation = *row.ActiveGeneration
	}
	live, err := s.fenceScopedAuthorityTx(ctx, tx, authorityKey, "pull")
	if err != nil {
		return view, row, err
	}
	if live.AuthorityUID != view.AuthorityUID || live.DestinationUID != view.DestinationUID ||
		live.RootCutoverUID != view.RootCutoverUID || live.RouteDigest != view.RouteDigest ||
		!slices.Equal(live.Route, view.Route) {
		return view, row, db.ErrNotFound
	}
	return view, row, nil
}

func scopedMaterializationPinsTx(ctx context.Context, tx *sql.Tx) ([]db.RootKeyPin, error) {
	rows, err := tx.QueryContext(ctx, `SELECT project_uid,authority_uid,key_id,public_key,active FROM federation_root_keys ORDER BY project_uid,key_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	pins := []db.RootKeyPin{}
	for rows.Next() {
		pin, err := scanRootPin(rows)
		if err != nil {
			return nil, err
		}
		pins = append(pins, pin)
	}
	return pins, rows.Err()
}

func (s *Store) PrepareScopedCheckpoint(ctx context.Context, checkpoint db.ScopedCheckpoint) error {
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(checkpoint, json.Deterministic(true))
	if err != nil {
		return err
	}
	return s.prepareScopedCheckpoint(ctx, checkpoint, raw)
}

func (s *Store) PrepareScopedCheckpointBytes(ctx context.Context, body []byte) error {
	if err := db.ValidateScopedCheckpointSize(int64(len(body))); err != nil {
		return err
	}
	raw := bytes.Clone(body)
	var checkpoint db.ScopedCheckpoint
	if err := db.DecodeScopedRecord(raw, &checkpoint); err != nil {
		return err
	}
	return s.prepareScopedCheckpoint(ctx, checkpoint, raw)
}

func (s *Store) prepareScopedCheckpoint(ctx context.Context, checkpoint db.ScopedCheckpoint, raw []byte) error {
	if len(raw) > db.MaxScopedCheckpointBytes {
		return errors.New("scoped checkpoint exceeds capacity")
	}
	return s.relayTx(ctx, func(tx *sql.Tx) error {
		view, row, err := s.scopedPreparationAuthorityTx(ctx, tx, checkpoint.View.Key)
		if err != nil {
			return err
		}
		return s.prepareScopedCheckpointTx(ctx, tx, checkpoint, raw, view, row)
	})
}

func (s *Store) prepareScopedCheckpointTx(ctx context.Context, tx *sql.Tx, checkpoint db.ScopedCheckpoint, raw []byte, view db.ScopedViewDescriptor, row db.ScopedViewExport) error {
	if !reflect.DeepEqual(view, checkpoint.View) {
		return db.ErrRemoteEventConflict
	}
	pins, err := scopedMaterializationPinsTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := db.VerifyScopedCheckpoint(checkpoint, view, pins); err != nil {
		return err
	}
	var retained []byte
	if err := tx.QueryRowContext(ctx, `SELECT checkpoint_bytes FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, view.Key.ViewUID, view.Key.Generation).Scan(&retained); err != nil {
		return err
	}
	if retained != nil {
		if !bytes.Equal(retained, raw) {
			return db.ErrRemoteEventConflict
		}
		return nil
	}
	if row.PreparedGeneration == nil || *row.PreparedGeneration != view.Key.Generation {
		return db.ErrNotFound
	}
	effects, err := db.ScopedCheckpointRecordEffects(checkpoint)
	if err != nil {
		return err
	}
	for _, effect := range effects {
		project, err := db.ScopedEffectProjectUID(effect)
		if err != nil {
			return err
		}
		subject, peerProject, peer, err := db.ScopedRecordIndexes(effect, nil)
		if err != nil {
			return err
		}
		recordBytes, err := json.Marshal(effect)
		if err != nil {
			return err
		}
		record := &db.ScopedRecordExport{ViewUID: view.Key.ViewUID, Generation: view.Key.Generation, Collection: effect.Collection, RecordKey: effect.Key, ProjectUID: project, SubjectUID: subject, PeerProjectUID: peerProject, PeerUID: peer, VisibleRevision: effect.Revision, Present: true, RecordBytes: recordBytes, Digest: db.ScopedOfferedDigest(recordBytes)}
		if err := db.ReplayScopedRecord(ctx, tx, record); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE federation_scoped_generations SET checkpoint_bytes=$1,checkpoint_digest=$2,cut_sequence=$3,cut_digest=$2,assembled_bytes=$4 WHERE view_uid=$5 AND generation=$6`, raw, checkpoint.Cut.Digest, checkpoint.Cut.Sequence, len(raw), view.Key.ViewUID, view.Key.Generation); err != nil {
		return err
	}
	return s.scopedCapacityTx(ctx, tx, view.Key.ViewUID)
}

func (s *Store) ActivateScopedCheckpoint(ctx context.Context, key db.ScopedViewKey, checkpointDigest string) error {
	if err := key.Validate(); err != nil {
		return err
	}
	return s.relayTx(ctx, func(tx *sql.Tx) error {
		view, row, err := s.scopedPreparationAuthorityTx(ctx, tx, key)
		if err != nil {
			return err
		}
		return s.activateScopedCheckpointTx(ctx, tx, key, checkpointDigest, view, row)
	})
}

func (s *Store) activateScopedCheckpointTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey, checkpointDigest string, view db.ScopedViewDescriptor, row db.ScopedViewExport) error {
	materialized, err := s.loadScopedMaterializationTx(ctx, tx, key)
	if err != nil {
		return err
	}
	var raw []byte
	var retainedDigest string
	if err := tx.QueryRowContext(ctx, `SELECT checkpoint_bytes,checkpoint_digest FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation).Scan(&raw, &retainedDigest); err != nil {
		return err
	}
	if checkpointDigest != retainedDigest {
		return db.ErrRemoteEventConflict
	}
	if row.ActiveGeneration != nil && *row.ActiveGeneration == key.Generation {
		return nil
	}
	if row.PreparedGeneration == nil || *row.PreparedGeneration != key.Generation {
		return db.ErrNotFound
	}
	var prior *db.ScopedMaterialization
	if row.ActiveGeneration != nil {
		old, err := s.loadScopedMaterializationTx(ctx, tx, db.ScopedViewKey{ViewUID: key.ViewUID, Generation: *row.ActiveGeneration})
		if err != nil {
			return err
		}
		prior = &old
	}
	if row.ActiveGeneration != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE federation_scoped_generations SET state='retired' WHERE view_uid=$1 AND generation=$2`, key.ViewUID, *row.ActiveGeneration); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE federation_scoped_generations SET state='active' WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE federation_scoped_views SET active_generation=$1,prepared_generation=NULL,state='active',reason_code='',request_digest=$2 WHERE view_uid=$3`, key.Generation, view.RequestDigest, key.ViewUID); err != nil {
		return err
	}
	if view.DestinationUID == s.instanceUID {
		if err := s.scopedSwitchOwnersTx(ctx, tx, view, row.ActiveGeneration); err != nil {
			return err
		}
		if _, err := s.fenceScopedAuthorityTx(ctx, tx, key, "pull"); err != nil {
			return err
		}
		materialized, err = s.loadScopedMaterializationTx(ctx, tx, key)
		if err != nil {
			return err
		}
		if err := s.materializeScopedOwnedTx(ctx, tx, materialized); err != nil {
			return err
		}
		if err := s.scopedCurrentChangedTx(ctx, tx, prior, materialized); err != nil {
			return err
		}
	} else {
		if _, err := s.fenceScopedAuthorityTx(ctx, tx, key, "pull"); err != nil {
			return err
		}
		if err := s.scopedPublishCheckpointTx(ctx, tx, materialized, raw); err != nil {
			return err
		}
	}
	return s.scopedCapacityTx(ctx, tx, key.ViewUID)
}

func (s *Store) FenceScopedView(ctx context.Context, key db.ScopedViewKey, reasonCode string) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if len(reasonCode) == 0 || len(reasonCode) > 64 {
		return errors.New("invalid scoped fence reason code")
	}
	for _, c := range reasonCode {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return errors.New("invalid scoped fence reason code")
		}
	}
	return s.relayTx(ctx, func(tx *sql.Tx) error {
		if confirmed, err := s.confirmScopedFenceRetryTx(ctx, tx, key, reasonCode); err != nil || confirmed {
			return err
		}
		view, err := s.fenceScopedAuthorityTx(ctx, tx, key, "pull")
		if err != nil {
			return err
		}
		_, row, err := s.scopedDescriptorTx(ctx, tx, key)
		if err != nil {
			return err
		}
		if row.ActiveGeneration == nil || *row.ActiveGeneration != key.Generation {
			return db.ErrScopedCapturePending
		}
		ownedProjects := []db.ScopedProjectExport{}
		for _, selected := range view.Projects {
			owner, err := s.scopedProjectOwnerTx(ctx, tx, selected.ProjectUID)
			if err != nil {
				return err
			}
			if owner.OwnerViewUID != nil && owner.OwnerGeneration != nil && *owner.OwnerViewUID == key.ViewUID && *owner.OwnerGeneration == key.Generation {
				ownedProjects = append(ownedProjects, owner)
			}
		}
		return s.fenceScopedViewTx(ctx, tx, key, reasonCode, ownedProjects)
	})
}

func (s *Store) fenceScopedViewTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey, reasonCode string, ownedProjects []db.ScopedProjectExport) error {
	result, err := tx.ExecContext(ctx, `UPDATE federation_scoped_views SET state='fenced',reason_code=$1 WHERE view_uid=$2 AND active_generation=$3`, reasonCode, key.ViewUID, key.Generation)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return db.ErrNotFound
	}
	for _, owner := range ownedProjects {
		fenced := 0
		if owner.ServingFenced {
			fenced = 1
		}
		result, err := tx.ExecContext(ctx, `UPDATE federation_scoped_projects SET serving_fenced=1 WHERE project_uid=$1 AND owner_view_uid=$2 AND owner_generation=$3 AND root_cutover_uid IS NOT DISTINCT FROM $4 AND protocol_floor=$5 AND serving_fenced=$6 AND conversion_uid IS NOT DISTINCT FROM $7`, owner.ProjectUID, key.ViewUID, key.Generation, owner.RootCutoverUID, owner.ProtocolFloor, fenced, owner.ConversionUID)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return db.ErrNotFound
		}
	}
	// Check retained capacity before reserving native cursor identities.
	// Canonical projects and an upstream view owned by another generation
	// are untouched when only an outgoing downstream view is fenced.
	if err := s.scopedCapacityTx(ctx, tx, key.ViewUID); err != nil {
		return err
	}
	for _, owner := range ownedProjects {
		if err := reserveAttributionUIResetTx(ctx, tx, owner.ProjectUID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) fenceScopedReplicaDepartureTx(ctx context.Context, tx *sql.Tx, projectUID string, binding db.FederationBinding) error {
	before, err := s.scopedProjectOwnerTx(ctx, tx, projectUID)
	if errors.Is(err, db.ErrNotFound) {
		if binding.RelayConfig != nil && binding.RelayConfig.ProtocolVersion == db.ScopedRelayProtocolVersion {
			return db.ErrNotFound
		}
		return nil
	}
	if err != nil {
		return err
	}
	if before.OwnerViewUID == nil {
		return nil
	}
	if before.ProtocolFloor != db.ScopedRelayProtocolVersion || before.OwnerGeneration == nil || before.RootCutoverUID == nil {
		return db.ErrNotFound
	}
	key := db.ScopedViewKey{ViewUID: *before.OwnerViewUID, Generation: *before.OwnerGeneration}
	// No late policy/cohort project locks: activation already conflicts with
	// the departing project lock, or (when omitting it) with this view row.
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT view_uid FROM federation_scoped_views WHERE view_uid=$1 FOR UPDATE`, key.ViewUID).Scan(&locked); err != nil {
		return scopedAuthorityLookupError(err)
	}
	view, row, err := s.scopedViewDeliveryContextTx(ctx, tx, key)
	if err != nil {
		return err
	}
	owner, err := s.scopedProjectOwnerTx(ctx, tx, projectUID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, owner) || row.ActiveGeneration == nil || *row.ActiveGeneration != key.Generation || view.DestinationUID != s.instanceUID || *owner.RootCutoverUID != view.RootCutoverUID {
		return db.ErrNotFound
	}
	current, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, binding.ProjectID))
	if err != nil {
		return scopedAuthorityLookupError(err)
	}
	if !reflect.DeepEqual(current, binding) {
		return db.ErrNotFound
	}
	config := current.RelayConfig
	hop := view.Route[len(view.Route)-1]
	path := []string{view.AuthorityUID}
	for _, h := range view.Route {
		path = append(path, h.InstanceUID)
	}
	if current.Role != db.FederationRoleSpoke || current.HubProjectUID != projectUID || db.ValidateTokenActor(current.Actor) != nil || row.AccountableActor != current.Actor || row.BindingUID != hop.BindingUID || row.Epoch != hop.Epoch ||
		config == nil || config.Validate(s.instanceUID) != nil || config.ProtocolVersion != db.ScopedRelayProtocolVersion || config.ScopedView == nil || *config.ScopedView != key ||
		config.AuthorityUID != view.AuthorityUID || config.RootCutoverUID != view.RootCutoverUID || config.BindingUID != hop.BindingUID || config.ResetEpoch != hop.Epoch || !slices.Equal(config.HubPath, path) || config.UpstreamInstanceUID != path[len(path)-2] {
		return db.ErrNotFound
	}
	var generationState, cutover string
	if err := tx.QueryRowContext(ctx, `SELECT state,root_cutover_uid FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation).Scan(&generationState, &cutover); err != nil {
		return scopedAuthorityLookupError(err)
	}
	if generationState != "active" || cutover != view.RootCutoverUID {
		return db.ErrNotFound
	}
	if row.State == "active" && row.PreparedGeneration != nil || row.State == "prepared" && (row.PreparedGeneration == nil || row.ReasonCode != "selection_narrowing" && row.ReasonCode != "generation_refresh") {
		return db.ErrNotFound
	}
	// A prepared narrowing already fences the old whole audience. Refresh
	// preparation does not. Mixed or otherwise partial fences are not repaired.
	fenced := row.State == "fenced" || row.State == "prepared" && row.ReasonCode == "selection_narrowing"
	if row.State != "active" && row.State != "prepared" && row.State != "fenced" {
		return db.ErrNotFound
	}
	ownedProjects := []db.ScopedProjectExport{}
	found := false
	for _, selected := range view.Projects {
		owner, err := s.scopedProjectOwnerTx(ctx, tx, selected.ProjectUID)
		if err != nil {
			return err
		}
		if owner.OwnerViewUID == nil || owner.OwnerGeneration == nil || *owner.OwnerViewUID != key.ViewUID || *owner.OwnerGeneration != key.Generation {
			continue
		}
		if owner.ProtocolFloor != db.ScopedRelayProtocolVersion || owner.RootCutoverUID == nil || *owner.RootCutoverUID != view.RootCutoverUID || owner.ServingFenced != fenced {
			return db.ErrNotFound
		}
		found = found || selected.ProjectUID == projectUID
		ownedProjects = append(ownedProjects, owner)
	}
	if !found {
		return db.ErrNotFound
	}
	if fenced {
		return nil
	}
	return s.fenceScopedViewTx(ctx, tx, key, "local_departure", ownedProjects)
}

func (s *Store) confirmScopedFenceRetryTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey, reasonCode string) (bool, error) {
	if err := lockProjectAccess(ctx, tx); err != nil {
		return false, err
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM federation_scoped_views WHERE view_uid=$1`, key.ViewUID).Scan(&state); err != nil {
		return false, scopedAuthorityLookupError(err)
	}
	if state != "fenced" {
		return false, nil
	}
	view, row, err := s.scopedViewDeliveryContextTx(ctx, tx, key)
	if err != nil {
		return false, err
	}
	if row.State != "fenced" || row.ReasonCode != reasonCode || row.ActiveGeneration == nil || *row.ActiveGeneration != key.Generation {
		return false, db.ErrNotFound
	}
	var cutover string
	if err := tx.QueryRowContext(ctx, `SELECT state,root_cutover_uid FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation).Scan(&state, &cutover); err != nil {
		return false, scopedAuthorityLookupError(err)
	}
	if state != "active" || cutover != view.RootCutoverUID {
		return false, db.ErrNotFound
	}
	if err := s.scopedBindingLocksTx(ctx, tx, view); err != nil {
		return false, scopedAuthorityLookupError(err)
	}
	hop, position, err := scopedHop(view, s.instanceUID)
	if err != nil {
		return false, scopedAuthorityLookupError(err)
	}
	if len(view.Route)+1 > db.MaxRelayHubs+1 {
		return false, db.ErrNotFound
	}
	if position+1 == len(view.Route) {
		if err := s.confirmScopedIncomingFenceTx(ctx, tx, view, row, hop); err != nil {
			return false, err
		}
		return true, nil
	}
	grant, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE relay_binding_uid=$1`+` FOR UPDATE`, hop.BindingUID))
	if err != nil {
		return false, scopedAuthorityLookupError(err)
	}
	if grant.RelayProtocolVersion != db.ScopedRelayProtocolVersion || grant.RevokedAt != nil || grant.ParentTokenID == nil || grant.ProjectID == nil || grant.SpokeInstanceUID != hop.InstanceUID || grant.RelayResetEpoch != hop.Epoch || !scopedOutgoingRoleAllowed(view, position, grant.RelayServeDownstream) || !slices.Contains(strings.Split(grant.Capabilities, ","), "pull") {
		return false, db.ErrNotFound
	}
	parent, err := s.scopedParentTx(ctx, tx, *grant.ParentTokenID, true)
	if err != nil {
		return false, scopedAuthorityLookupError(err)
	}
	if parent.Actor != grant.Actor || row.BindingUID == hop.BindingUID && (row.Epoch != hop.Epoch || row.AccountableActor != parent.Actor) {
		return false, db.ErrNotFound
	}
	// The serving selection remains strict. A downstream view's fence cannot
	// excuse a different upstream owner's fence or weaken exact current pins.
	ids, err := s.scopedSelectionTx(ctx, tx, view, parent.Actor, position, "pull")
	if err != nil {
		return false, scopedAuthorityLookupError(err)
	}
	if !slices.Contains(ids, *grant.ProjectID) {
		return false, db.ErrNotFound
	}
	return true, nil
}

func (s *Store) confirmScopedIncomingFenceTx(ctx context.Context, tx *sql.Tx, view db.ScopedViewDescriptor, row db.ScopedViewExport, hop db.ScopedHopBinding) error {
	if view.DestinationUID != s.instanceUID || len(view.Projects) == 0 {
		return db.ErrNotFound
	}
	path := []string{view.AuthorityUID}
	for _, h := range view.Route {
		path = append(path, h.InstanceUID)
	}
	var actor string
	for _, selected := range view.Projects {
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1 AND deleted_at IS NULL`, selected.ProjectUID).Scan(&id); err != nil {
			return db.RelayProjectLookupError(err)
		}
		pin, err := rootPinTx(ctx, tx, selected.ProjectUID, "")
		if err != nil {
			return scopedAuthorityLookupError(err)
		}
		if !reflect.DeepEqual(pin, selected.Pin) {
			return db.ErrRelayRootChanged
		}
		owner, err := s.scopedProjectOwnerTx(ctx, tx, selected.ProjectUID)
		if err != nil {
			return err
		}
		if !owner.ServingFenced || owner.ProtocolFloor != db.ScopedRelayProtocolVersion || owner.RootCutoverUID == nil || *owner.RootCutoverUID != view.RootCutoverUID || owner.OwnerViewUID == nil || *owner.OwnerViewUID != view.Key.ViewUID || owner.OwnerGeneration == nil || *owner.OwnerGeneration != view.Key.Generation {
			return db.ErrNotFound
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, id))
		if err != nil {
			return scopedAuthorityLookupError(err)
		}
		config := binding.RelayConfig
		if !binding.Enabled || binding.Role != db.FederationRoleSpoke || binding.HubProjectUID != selected.ProjectUID || config == nil || config.Validate(s.instanceUID) != nil || config.ProtocolVersion != db.ScopedRelayProtocolVersion || config.UpstreamRevoked || config.ScopedView == nil || *config.ScopedView != view.Key || config.AuthorityUID != view.AuthorityUID || config.RootCutoverUID != view.RootCutoverUID || config.BindingUID != hop.BindingUID || config.ResetEpoch != hop.Epoch || !slices.Equal(config.HubPath, path) || config.UpstreamInstanceUID != path[len(path)-2] || db.ValidateTokenActor(binding.Actor) != nil {
			return db.ErrNotFound
		}
		if actor == "" {
			actor = binding.Actor
		}
		if binding.Actor != actor {
			return db.ErrNotFound
		}
		if err := s.scopedProjectAccessTx(ctx, tx, config.LocalActor, selected.ProjectUID); err != nil {
			return scopedAuthorityLookupError(err)
		}
	}
	if row.BindingUID == hop.BindingUID && (row.Epoch != hop.Epoch || row.AccountableActor != actor) {
		return db.ErrNotFound
	}
	var committed int
	return scopedAuthorityLookupError(tx.QueryRowContext(ctx, `SELECT 1 FROM federation_protocol_cutovers WHERE cutover_uid=$1 AND state='committed'`, view.RootCutoverUID).Scan(&committed))
}

func (s *Store) CompleteScopedViewCapture(ctx context.Context, key db.ScopedViewKey, captureUID string) error {
	if err := key.Validate(); err != nil {
		return err
	}
	return s.relayTx(ctx, func(tx *sql.Tx) error {
		view, err := s.fenceScopedAuthorityTx(ctx, tx, key, "pull")
		if err != nil {
			return err
		}
		if view.AuthorityUID != s.instanceUID {
			return db.ErrNotFound
		}
		_, row, err := s.scopedDescriptorTx(ctx, tx, key)
		if err != nil {
			return err
		}
		if row.ActiveGeneration == nil || *row.ActiveGeneration != key.Generation {
			return db.ErrScopedCapturePending
		}
		var work db.ScopedWorkExport
		if err := tx.QueryRowContext(ctx, `SELECT view_uid,dirty_revision,captured_revision,capture_uid,capture_bytes,capture_digest,state FROM federation_scoped_work WHERE view_uid=$1 FOR UPDATE`, key.ViewUID).Scan(&work.ViewUID, &work.DirtyRevision, &work.CapturedRevision, &work.CaptureUID, &work.CaptureBytes, &work.CaptureDigest, &work.State); err != nil {
			return err
		}
		if err := db.ValidateScopedImportRecord(&work); err != nil {
			return err
		}
		if work.CaptureUID == nil || work.CapturedRevision == nil || *work.CaptureUID != captureUID {
			return db.ErrScopedCapturePending
		}
		captured, err := db.DecodeScopedCanonicalCapture(work.CaptureBytes)
		if err != nil {
			return err
		}
		if captured.CaptureUID != captureUID || len(captured.ProjectCuts) != len(view.Projects) {
			return db.ErrScopedCapturePending
		}
		for _, project := range view.Projects {
			if _, present := captured.ProjectCuts[project.ProjectUID]; !present {
				return db.ErrScopedCapturePending
			}
		}
		prior, err := s.loadScopedMaterializationTx(ctx, tx, key)
		if err != nil {
			return err
		}
		projected, err := db.ProjectScopedState(captured, view)
		if err != nil {
			return err
		}
		want, err := db.ScopedStateDigest(prior.Collections)
		if err != nil {
			return err
		}
		got, err := db.ScopedStateDigest(projected)
		if err != nil {
			return err
		}
		if got != want {
			return db.ErrRemoteEventConflict
		}
		completed, err := completeScopedCaptureWorkTx(ctx, tx, key.ViewUID, captureUID, *work.CapturedRevision)
		if err != nil {
			return err
		}
		if !completed {
			return db.ErrScopedCapturePending
		}
		return nil
	})
}

func (s *Store) CommitScopedBatch(ctx context.Context, batch db.ScopedBatch, captureUID string) error {
	if err := batch.Validate(); err != nil {
		return err
	}
	if captureUID != batch.Cut.CaptureUID {
		return db.ErrScopedCapturePending
	}
	raw, err := json.Marshal(batch, json.Deterministic(true))
	if err != nil {
		return err
	}
	if err := db.ValidateScopedCheckpointSize(int64(len(raw))); err != nil {
		return err
	}
	return s.relayTx(ctx, func(tx *sql.Tx) error {
		view, err := s.fenceScopedAuthorityTx(ctx, tx, batch.View.Key, "pull")
		if err != nil {
			return err
		}
		if view.AuthorityUID != s.instanceUID {
			return db.ErrNotFound
		}
		_, row, err := s.scopedDescriptorTx(ctx, tx, view.Key)
		if err != nil {
			return err
		}
		if row.ActiveGeneration == nil || *row.ActiveGeneration != view.Key.Generation {
			return db.ErrScopedCapturePending
		}
		pins, err := scopedMaterializationPinsTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := db.VerifyScopedBatch(batch, view, pins); err != nil {
			return err
		}
		prior, err := s.loadScopedMaterializationTx(ctx, tx, view.Key)
		if err != nil {
			return err
		}
		var retained []byte
		err = tx.QueryRowContext(ctx, `SELECT batch_bytes FROM federation_scoped_batches WHERE view_uid=$1 AND generation=$2 AND sequence=$3`, view.Key.ViewUID, view.Key.Generation, batch.Cut.Sequence).Scan(&retained)
		if err == nil {
			if !bytes.Equal(retained, raw) {
				return db.ErrRemoteEventConflict
			}
			return nil // Exact retry does not consume another capture or sequence.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if prior.Fence.Cut.Sequence == int64(^uint64(0)>>1) || batch.Cut.Sequence != prior.Fence.Cut.Sequence+1 || batch.PreviousDigest != prior.Fence.Cut.Digest {
			return db.ErrRemoteEventConflict
		}
		var work db.ScopedWorkExport
		if err := tx.QueryRowContext(ctx, `SELECT view_uid,dirty_revision,captured_revision,capture_uid,capture_bytes,capture_digest,state FROM federation_scoped_work WHERE view_uid=$1`+` FOR UPDATE`, view.Key.ViewUID).Scan(&work.ViewUID, &work.DirtyRevision, &work.CapturedRevision, &work.CaptureUID, &work.CaptureBytes, &work.CaptureDigest, &work.State); err != nil {
			return err
		}
		if err := db.ValidateScopedImportRecord(&work); err != nil {
			return err
		}
		if work.CaptureUID == nil || work.CapturedRevision == nil || *work.CaptureUID != captureUID {
			return db.ErrScopedCapturePending
		}
		if err := db.ReplayScopedRecord(ctx, tx, &db.ScopedBatchExport{ViewUID: view.Key.ViewUID, Generation: view.Key.Generation, Sequence: batch.Cut.Sequence, Digest: batch.Cut.Digest, PreviousDigest: batch.PreviousDigest, CaptureUID: batch.Cut.CaptureUID, BatchBytes: raw}); err != nil {
			return err
		}
		for _, effect := range batch.Effects {
			if err := s.scopedApplyRecordTx(ctx, tx, view.Key, effect); err != nil {
				return err
			}
		}
		next, err := s.loadScopedMaterializationTx(ctx, tx, view.Key)
		if err != nil {
			return err
		}
		// The existing publisher verifies exact capture/project-cut association
		// and projected content, retains the hop body, then runs the UID/revision
		// CAS. No mutable canonical content is read or materialized here.
		if err := s.scopedPublishCheckpointTx(ctx, tx, next, raw); err != nil {
			return err
		}
		return s.scopedCapacityTx(ctx, tx, view.Key.ViewUID)
	})
}

func (s *Store) ApplyScopedBatch(ctx context.Context, batch db.ScopedBatch) error {
	if err := batch.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(batch, json.Deterministic(true))
	if err != nil {
		return err
	}
	return s.applyScopedBatch(ctx, batch, raw)
}

func (s *Store) ApplyScopedBatchBytes(ctx context.Context, body []byte) error {
	if err := db.ValidateScopedCheckpointSize(int64(len(body))); err != nil {
		return err
	}
	raw := bytes.Clone(body)
	var batch db.ScopedBatch
	if err := db.DecodeScopedRecord(raw, &batch); err != nil {
		return err
	}
	return s.applyScopedBatch(ctx, batch, raw)
}

func (s *Store) applyScopedBatch(ctx context.Context, batch db.ScopedBatch, raw []byte) error {
	if err := db.ValidateScopedCheckpointSize(int64(len(raw))); err != nil {
		return err
	}
	return s.relayTx(ctx, func(tx *sql.Tx) error {
		view, err := s.fenceScopedAuthorityTx(ctx, tx, batch.View.Key, "pull")
		if err != nil {
			return err
		}
		if view.DestinationUID != s.instanceUID {
			return db.ErrNotFound
		}
		pins, err := scopedMaterializationPinsTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := db.VerifyScopedBatch(batch, view, pins); err != nil {
			return err
		}
		prior, err := s.loadScopedMaterializationTx(ctx, tx, view.Key)
		if err != nil {
			return err
		}
		if err := s.checkScopedOwnerFenceTx(ctx, tx, prior.Fence); err != nil {
			return err
		}
		if err := s.validateScopedOwnedTx(ctx, tx, prior); err != nil {
			return err
		}
		next, retained, err := s.retainScopedBatchTx(ctx, tx, view, batch, raw, prior)
		if err != nil || retained {
			return err
		}
		if err := s.materializeScopedOwnedTx(ctx, tx, next); err != nil {
			return err
		}
		if err := s.scopedCurrentChangedTx(ctx, tx, &prior, next); err != nil {
			return err
		}
		return s.scopedCapacityTx(ctx, tx, view.Key.ViewUID)
	})
}

func (s *Store) retainScopedBatchTx(ctx context.Context, tx *sql.Tx, view db.ScopedViewDescriptor, batch db.ScopedBatch, raw []byte, prior db.ScopedMaterialization) (db.ScopedMaterialization, bool, error) {
	var retained []byte
	err := tx.QueryRowContext(ctx, `SELECT batch_bytes FROM federation_scoped_batches WHERE view_uid=$1 AND generation=$2 AND sequence=$3`, view.Key.ViewUID, view.Key.Generation, batch.Cut.Sequence).Scan(&retained)
	if err == nil {
		if !bytes.Equal(retained, raw) {
			return db.ScopedMaterialization{}, false, db.ErrRemoteEventConflict
		}
		return prior, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.ScopedMaterialization{}, false, err
	}
	if prior.Fence.Cut.Sequence == int64(^uint64(0)>>1) || batch.Cut.Sequence != prior.Fence.Cut.Sequence+1 || batch.PreviousDigest != prior.Fence.Cut.Digest {
		return db.ScopedMaterialization{}, false, db.ErrRemoteEventConflict
	}
	row := &db.ScopedBatchExport{ViewUID: view.Key.ViewUID, Generation: view.Key.Generation, Sequence: batch.Cut.Sequence, Digest: batch.Cut.Digest, PreviousDigest: batch.PreviousDigest, CaptureUID: batch.Cut.CaptureUID, BatchBytes: raw}
	if err := db.ReplayScopedRecord(ctx, tx, row); err != nil {
		return db.ScopedMaterialization{}, false, err
	}
	for _, effect := range batch.Effects {
		if err := s.scopedApplyRecordTx(ctx, tx, view.Key, effect); err != nil {
			return db.ScopedMaterialization{}, false, err
		}
	}
	// The checkpoint remains its exact baseline. Shared inventory validation
	// applies the contiguous signed batch chain, without another canonical fold.
	next, err := s.loadScopedMaterializationTx(ctx, tx, view.Key)
	return next, false, err
}

func (s *Store) scopedApplyRecordTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey, effect db.ScopedEffect) error {
	var old db.ScopedRecordExport
	var present int
	err := tx.QueryRowContext(ctx, `SELECT view_uid,generation,collection,record_key,project_uid,subject_uid,peer_project_uid,peer_uid,visible_revision,present,record_bytes,digest FROM federation_scoped_records WHERE view_uid=$1 AND generation=$2 AND collection=$3 AND record_key=$4`, key.ViewUID, key.Generation, effect.Collection, effect.Key).Scan(&old.ViewUID, &old.Generation, &old.Collection, &old.RecordKey, &old.ProjectUID, &old.SubjectUID, &old.PeerProjectUID, &old.PeerUID, &old.VisibleRevision, &present, &old.RecordBytes, &old.Digest)
	var prior *db.ScopedRecordExport
	if err == nil {
		if present != 0 && present != 1 {
			return errors.New("invalid scoped record presence")
		}
		old.Present = present == 1
		if err := db.ValidateScopedRecordEnvelope(old); err != nil {
			return err
		}
		prior = &old
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	project, err := db.ScopedEffectProjectUID(effect)
	if err != nil {
		return err
	}
	subject, peerProject, peer, err := db.ScopedRecordIndexes(effect, prior)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(effect)
	if err != nil {
		return err
	}
	record := db.ScopedRecordExport{ViewUID: key.ViewUID, Generation: key.Generation, Collection: effect.Collection, RecordKey: effect.Key, ProjectUID: project, SubjectUID: subject, PeerProjectUID: peerProject, PeerUID: peer, VisibleRevision: effect.Revision, Present: effect.Action == "replace", RecordBytes: raw, Digest: db.ScopedOfferedDigest(raw)}
	if err := db.ValidateScopedRecordEnvelope(record); err != nil {
		return err
	}
	present = 0
	if record.Present {
		present = 1
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO federation_scoped_records(view_uid,generation,collection,record_key,project_uid,subject_uid,peer_project_uid,peer_uid,visible_revision,present,record_bytes,digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(view_uid,generation,collection,record_key) DO UPDATE SET project_uid=excluded.project_uid,subject_uid=excluded.subject_uid,peer_project_uid=excluded.peer_project_uid,peer_uid=excluded.peer_uid,visible_revision=excluded.visible_revision,present=excluded.present,record_bytes=excluded.record_bytes,digest=excluded.digest`, record.ViewUID, record.Generation, record.Collection, record.RecordKey, record.ProjectUID, record.SubjectUID, record.PeerProjectUID, record.PeerUID, record.VisibleRevision, present, record.RecordBytes, record.Digest)
	return err
}

func (s *Store) scopedSwitchOwnersTx(ctx context.Context, tx *sql.Tx, view db.ScopedViewDescriptor, previous *int64) error {
	for _, project := range view.Projects {
		owner, err := s.scopedProjectOwnerTx(ctx, tx, project.ProjectUID)
		if err != nil {
			return err
		}
		if owner.ProtocolFloor != db.ScopedRelayProtocolVersion || owner.RootCutoverUID == nil || *owner.RootCutoverUID != view.RootCutoverUID ||
			owner.OwnerViewUID != nil && (*owner.OwnerViewUID != view.Key.ViewUID || owner.OwnerGeneration == nil || previous == nil || *owner.OwnerGeneration != *previous) {
			return db.ErrRemoteEventConflict
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=(SELECT id FROM projects WHERE uid=$1)`, project.ProjectUID))
		if err != nil {
			return err
		}
		if binding.RelayConfig == nil || binding.RelayConfig.ScopedView == nil || binding.RelayConfig.ScopedView.ViewUID != view.Key.ViewUID {
			return db.ErrNotFound
		}
		config := *binding.RelayConfig
		key := view.Key
		config.ScopedView = &key
		raw, err := json.Marshal(config)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_bindings SET relay_config=$1 WHERE project_id=$2`, string(raw), binding.ProjectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_scoped_projects SET owner_view_uid=$1,owner_generation=$2,serving_fenced=0 WHERE project_uid=$3`, view.Key.ViewUID, view.Key.Generation, project.ProjectUID); err != nil {
			return err
		}
	}
	// Withdrawn projects retain their exact old owner, native/local data and
	// obligations, but cannot serve a generation no longer selected as active.
	_, err := tx.ExecContext(ctx, `UPDATE federation_scoped_projects SET serving_fenced=1 WHERE owner_view_uid=$1 AND owner_generation<>$2`, view.Key.ViewUID, view.Key.Generation)
	return err
}

func (s *Store) scopedCurrentChangedTx(ctx context.Context, tx *sql.Tx, prior *db.ScopedMaterialization, next db.ScopedMaterialization) error {
	before := db.ScopedCollections{Projects: []db.ScopedProjectState{}, Issues: []db.ScopedIssueState{}, Comments: []db.ScopedCommentState{}, Labels: []db.ScopedLabelState{}, Definitions: []db.ScopedDefinitionState{}, References: []db.ScopedReference{}, Provenance: []db.ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}
	if prior != nil {
		before = prior.Collections
	}
	keys, err := db.ScopedChangedProjects(before, next.Collections)
	if err != nil {
		return err
	}
	for _, project := range keys {
		if err := reserveAttributionUIResetTx(ctx, tx, project); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) scopedPublishCheckpointTx(ctx context.Context, tx *sql.Tx, materialized db.ScopedMaterialization, body []byte) error {
	view := materialized.Fence.View
	hop, position, err := scopedHop(view, s.instanceUID)
	if err != nil {
		return err
	}
	if position+1 >= len(view.Route) {
		return db.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_cursors(binding_uid,epoch,direction,channel,received_sequence,received_digest,accepted_sequence,accepted_digest,emitted_sequence,emitted_digest,acknowledged_sequence,acknowledged_digest) VALUES($1,$2,'outbound','view',0,'',0,'',0,'',0,'') ON CONFLICT(binding_uid,epoch,direction,channel) DO NOTHING`, hop.BindingUID, hop.Epoch); err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM federation_scoped_delivery WHERE binding_uid=$1 AND epoch=$2 AND direction='outbound' AND channel='view'`, hop.BindingUID, hop.Epoch).Scan(&sequence); err != nil {
		return err
	}
	path := []string{view.AuthorityUID}
	for _, h := range view.Route[:position+1] {
		path = append(path, h.InstanceUID)
	}
	header := db.ScopedDeliveryHeader{Version: db.ScopedRelayProtocolVersion, Domain: db.ScopedDeliveryDomain, BindingUID: hop.BindingUID, Epoch: hop.Epoch, Channel: db.ScopedDeliveryView, Sequence: sequence, AuthorityUID: view.AuthorityUID, SenderInstanceUID: s.instanceUID, ReceiverInstanceUID: hop.InstanceUID, Path: path, ObjectUID: view.Key.ViewUID, ObjectDigest: materialized.Fence.Cut.Digest}
	envelope, err := db.EncodeScopedDelivery(header, body)
	if err != nil {
		return err
	}
	transcript, err := db.ScopedDeliveryHeaderBytes(envelope)
	if err != nil {
		return err
	}
	record := &db.ScopedDeliveryRecord{ScopedDeliveryRef: db.ScopedDeliveryRef{ScopedDeliveryStream: db.ScopedDeliveryStream{BindingUID: hop.BindingUID, Epoch: hop.Epoch, Direction: db.ScopedDeliveryOutbound, Channel: db.ScopedDeliveryView}, Sequence: sequence}, ObjectUID: view.Key.ViewUID, ObjectDigest: materialized.Fence.Cut.Digest, OfferedDigest: db.ScopedOfferedDigest(envelope), HeaderBytes: transcript, EnvelopeBytes: envelope}
	if err := db.ReplayScopedRecord(ctx, tx, record); err != nil {
		return err
	}
	if view.AuthorityUID != s.instanceUID {
		// Forwarding retains root-produced bytes. It never captures or completes
		// canonical work from an incoming replica.
		return nil
	}
	var work db.ScopedWorkExport
	if err := tx.QueryRowContext(ctx, `SELECT view_uid,dirty_revision,captured_revision,capture_uid,capture_bytes,capture_digest,state FROM federation_scoped_work WHERE view_uid=$1`, view.Key.ViewUID).Scan(&work.ViewUID, &work.DirtyRevision, &work.CapturedRevision, &work.CaptureUID, &work.CaptureBytes, &work.CaptureDigest, &work.State); err != nil {
		return err
	}
	if work.CaptureUID == nil || work.CapturedRevision == nil || work.CaptureDigest == nil || db.ScopedOfferedDigest(work.CaptureBytes) != *work.CaptureDigest || *work.CaptureUID != materialized.Fence.Cut.CaptureUID {
		return db.ErrScopedCapturePending
	}
	captured, err := db.DecodeScopedCanonicalCapture(work.CaptureBytes)
	if err != nil {
		return err
	}
	if captured.CaptureUID != *work.CaptureUID || len(captured.ProjectCuts) != len(view.Projects) {
		return db.ErrScopedCapturePending
	}
	for _, project := range view.Projects {
		if _, present := captured.ProjectCuts[project.ProjectUID]; !present {
			return db.ErrScopedCapturePending
		}
	}
	projected, err := db.ProjectScopedState(captured, view)
	if err != nil {
		return err
	}
	want, err := db.ScopedStateDigest(materialized.Collections)
	if err != nil {
		return err
	}
	got, err := db.ScopedStateDigest(projected)
	if err != nil {
		return err
	}
	if want != got {
		return db.ErrRemoteEventConflict
	}
	// An independently owed capture remains pinned even after publication.
	// The exact CAS preserves newer dirty work and refuses pinned inventory.
	_, err = completeScopedCaptureWorkTx(ctx, tx, view.Key.ViewUID, *work.CaptureUID, *work.CapturedRevision)
	return err
}

func (s *Store) PendingScopedViewDeliveries(ctx context.Context, key db.ScopedViewKey, limit int) ([]db.ScopedDeliveryRecord, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1024 {
		return nil, db.ErrFederationIngestValidation
	}
	var output []db.ScopedDeliveryRecord
	err := s.relayTx(ctx, func(tx *sql.Tx) error {
		output = nil
		view, _, err := s.scopedViewDeliveryContextTx(ctx, tx, key)
		if err != nil {
			return err
		}
		if err := s.scopedViewDeliveryAuthorityTx(ctx, tx, view, false); err != nil {
			return err
		}
		hop, _, err := scopedHop(view, s.instanceUID)
		if err != nil {
			return err
		}
		stream := db.ScopedDeliveryStream{BindingUID: hop.BindingUID, Epoch: hop.Epoch, Direction: db.ScopedDeliveryOutbound, Channel: db.ScopedDeliveryView}
		pins, err := scopedMaterializationPinsTx(ctx, tx)
		if err != nil {
			return err
		}
		cursor, err := s.scopedViewDeliveryCursorTx(ctx, tx, stream, pins)
		if err != nil {
			return err
		}
		rows, err := scopedViewDeliveryRowsTx(ctx, tx, stream, cursor.AcknowledgedSequence, 0, limit)
		if err != nil {
			return err
		}
		evidence := []db.ScopedDeliveryRecord{}
		for n := range rows {
			row := &rows[n]
			if row.Sequence != cursor.AcknowledgedSequence+int64(n)+1 || row.ObjectUID != key.ViewUID || row.Acknowledged || row.EnvelopeBytes == nil || row.Emitted != (row.Sequence <= cursor.EmittedSequence) {
				return db.ErrScopedCapturePending
			}
			ownView, err := s.scopedViewDeliveryObjectTx(ctx, tx, *row, pins)
			if err != nil {
				return err
			}
			if err := s.scopedViewDeliveryAuthorityTx(ctx, tx, ownView, !row.Emitted); err != nil {
				return err
			}
			row.Emitted = true
			if row.Sequence > cursor.EmittedSequence {
				evidence = append(evidence, *row)
			}
		}
		if len(evidence) > 0 {
			last := evidence[len(evidence)-1]
			if err := db.ValidateScopedDeliveryPrefix(cursor, last.Sequence, "emitted", evidence); err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `UPDATE federation_scoped_delivery SET emitted=1 WHERE binding_uid=$1 AND epoch=$2 AND direction='outbound' AND channel='view' AND sequence>$3 AND sequence<=$4 AND emitted=0 AND acknowledged=0`, stream.BindingUID, stream.Epoch, cursor.EmittedSequence, last.Sequence)
			if err := scopedViewDeliveryUpdated(result, err, int64(len(evidence))); err != nil {
				return err
			}
			result, err = tx.ExecContext(ctx, `UPDATE federation_scoped_cursors SET emitted_sequence=$1,emitted_digest=$2 WHERE binding_uid=$3 AND epoch=$4 AND direction='outbound' AND channel='view' AND emitted_sequence=$5 AND emitted_digest=$6`, last.Sequence, last.OfferedDigest, stream.BindingUID, stream.Epoch, cursor.EmittedSequence, cursor.EmittedDigest)
			if err := scopedViewDeliveryUpdated(result, err, 1); err != nil {
				return err
			}
			if err := s.scopedCapacityTx(ctx, tx, key.ViewUID); err != nil {
				return err
			}
		}
		output = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}

func (s *Store) AckScopedViewDeliveries(ctx context.Context, key db.ScopedViewKey, sequence int64, offeredDigest string) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if sequence <= 0 || len(offeredDigest) != 64 {
		return db.ErrFederationIngestValidation
	}
	return s.relayTx(ctx, func(tx *sql.Tx) error {
		return s.ackScopedViewDeliveriesTx(ctx, tx, key, sequence, offeredDigest)
	})
}

func (s *Store) ackScopedViewDeliveriesTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey, sequence int64, offeredDigest string) error {
	view, _, err := s.scopedViewDeliveryContextTx(ctx, tx, key)
	if err != nil {
		return err
	}
	// This is actual native hop authentication, not active-view eligibility.
	// Unsupported historical epochs retain all state and refuse here.
	if err := s.scopedViewDeliveryAuthorityTx(ctx, tx, view, false); err != nil {
		return err
	}
	hop, _, err := scopedHop(view, s.instanceUID)
	if err != nil {
		return err
	}
	stream := db.ScopedDeliveryStream{BindingUID: hop.BindingUID, Epoch: hop.Epoch, Direction: db.ScopedDeliveryOutbound, Channel: db.ScopedDeliveryView}
	pins, err := scopedMaterializationPinsTx(ctx, tx)
	if err != nil {
		return err
	}
	cursor, err := s.scopedViewDeliveryCursorTx(ctx, tx, stream, pins)
	if err != nil {
		return err
	}
	endpoint, err := scopedViewDeliveryRowTx(ctx, tx, db.ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: sequence})
	if err != nil {
		return err
	}
	if endpoint.ObjectUID != key.ViewUID || !endpoint.Emitted || endpoint.OfferedDigest != offeredDigest || sequence > cursor.EmittedSequence {
		return db.ErrRemoteEventConflict
	}
	ownView, err := s.scopedViewDeliveryObjectTx(ctx, tx, endpoint, pins)
	if err != nil {
		return err
	}
	if err := s.scopedViewDeliveryAuthorityTx(ctx, tx, ownView, false); err != nil {
		return err
	}
	if sequence <= cursor.AcknowledgedSequence {
		if !endpoint.Acknowledged {
			return db.ErrRemoteEventConflict
		}
		return nil
	}
	rows, err := scopedViewDeliveryRowsTx(ctx, tx, stream, cursor.AcknowledgedSequence, sequence, 1025)
	if err != nil {
		return err
	}
	if len(rows) > 1024 {
		return db.ErrScopedCapturePending
	}
	for n := range rows {
		row := &rows[n]
		if row.ObjectUID != key.ViewUID || !row.Emitted || row.Acknowledged {
			return db.ErrScopedCapturePending
		}
		ownView, err := s.scopedViewDeliveryObjectTx(ctx, tx, *row, pins)
		if err != nil {
			return err
		}
		if err := s.scopedViewDeliveryAuthorityTx(ctx, tx, ownView, false); err != nil {
			return err
		}
		row.Acknowledged = true
	}
	if err := db.ValidateScopedDeliveryPrefix(cursor, sequence, "acknowledged", rows); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE federation_scoped_delivery SET acknowledged=1 WHERE binding_uid=$1 AND epoch=$2 AND direction='outbound' AND channel='view' AND sequence>$3 AND sequence<=$4 AND emitted=1 AND acknowledged=0`, stream.BindingUID, stream.Epoch, cursor.AcknowledgedSequence, sequence)
	if err := scopedViewDeliveryUpdated(result, err, int64(len(rows))); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE federation_scoped_cursors SET acknowledged_sequence=$1,acknowledged_digest=$2 WHERE binding_uid=$3 AND epoch=$4 AND direction='outbound' AND channel='view' AND acknowledged_sequence=$5 AND acknowledged_digest=$6 AND emitted_sequence=$7 AND emitted_digest=$8`, sequence, offeredDigest, stream.BindingUID, stream.Epoch, cursor.AcknowledgedSequence, cursor.AcknowledgedDigest, cursor.EmittedSequence, cursor.EmittedDigest)
	if err := scopedViewDeliveryUpdated(result, err, 1); err != nil {
		return err
	}
	return s.scopedCapacityTx(ctx, tx, key.ViewUID)
}

func scopedViewDeliveryUpdated(result sql.Result, err error, count int64) error {
	if err != nil {
		return err
	}
	got, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if got != count {
		return db.ErrRemoteEventConflict
	}
	return nil
}

func (s *Store) scopedViewDeliveryContextTx(ctx context.Context, tx *sql.Tx, key db.ScopedViewKey) (db.ScopedViewDescriptor, db.ScopedViewExport, error) {
	var view db.ScopedViewDescriptor
	var row db.ScopedViewExport
	var raw []byte
	var digest string
	err := tx.QueryRowContext(ctx, `SELECT view_uid,catalog_entry_uid,authority_uid,binding_uid,epoch,accountable_actor,route_digest,destination_uid,active_generation,prepared_generation,state,reason_code,request_digest FROM federation_scoped_views WHERE view_uid=$1`, key.ViewUID).Scan(&row.ViewUID, &row.CatalogEntryUID, &row.AuthorityUID, &row.BindingUID, &row.Epoch, &row.AccountableActor, &row.RouteDigest, &row.DestinationUID, &row.ActiveGeneration, &row.PreparedGeneration, &row.State, &row.ReasonCode, &row.RequestDigest)
	if err != nil {
		return view, row, scopedAuthorityLookupError(err)
	}
	if err := db.ValidateScopedImportRecord(&row); err != nil {
		return view, row, err
	}
	err = tx.QueryRowContext(ctx, `SELECT descriptor_bytes,descriptor_digest FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation).Scan(&raw, &digest)
	if err != nil {
		return view, row, scopedAuthorityLookupError(err)
	}
	if db.ScopedOfferedDigest(raw) != digest || db.DecodeScopedRecord(raw, &view) != nil || view.Validate() != nil || view.Key != key || view.AuthorityUID != row.AuthorityUID || view.RouteDigest != row.RouteDigest || view.DestinationUID != row.DestinationUID {
		return view, row, db.ErrRemoteEventConflict
	}
	return view, row, nil
}

func (s *Store) scopedViewDeliveryAuthorityTx(ctx context.Context, tx *sql.Tx, view db.ScopedViewDescriptor, fresh bool) error {
	if fresh {
		live, err := s.fenceScopedAuthorityTx(ctx, tx, view.Key, "pull")
		if err != nil {
			return err
		}
		_, row, err := s.scopedDescriptorTx(ctx, tx, view.Key)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(live, view) || row.State != "active" || row.ActiveGeneration == nil || *row.ActiveGeneration != view.Key.Generation {
			return db.ErrNotFound
		}
		return nil
	}
	if err := lockProjectAccess(ctx, tx); err != nil {
		return err
	}
	if err := s.scopedBindingLocksTx(ctx, tx, view); err != nil {
		return err
	}
	hop, position, err := scopedHop(view, s.instanceUID)
	if err != nil || position+1 >= len(view.Route) {
		return db.ErrNotFound
	}
	grant, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE relay_binding_uid=$1`+` FOR SHARE`, hop.BindingUID))
	if err != nil {
		return scopedAuthorityLookupError(err)
	}
	// No retained-only or requested-epoch fallback authenticates this call.
	if grant.RelayProtocolVersion != db.ScopedRelayProtocolVersion || grant.RevokedAt != nil || grant.ParentTokenID == nil || grant.ProjectID == nil || grant.SpokeInstanceUID != hop.InstanceUID || grant.RelayResetEpoch != hop.Epoch || !scopedOutgoingRoleAllowed(view, position, grant.RelayServeDownstream) || !slices.Contains(strings.Split(grant.Capabilities, ","), "pull") {
		return db.ErrNotFound
	}
	parent, err := s.scopedParentTx(ctx, tx, *grant.ParentTokenID, true)
	if err != nil {
		return scopedAuthorityLookupError(err)
	}
	_, row, err := s.scopedViewDeliveryContextTx(ctx, tx, view.Key)
	if err != nil {
		return err
	}
	if parent.Actor != grant.Actor || row.BindingUID == hop.BindingUID && (row.Epoch != hop.Epoch || row.AccountableActor != parent.Actor) {
		return db.ErrNotFound
	}
	var anchor string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, *grant.ProjectID).Scan(&anchor); err != nil {
		return scopedAuthorityLookupError(err)
	}
	if !slices.ContainsFunc(view.Projects, func(p db.ScopedProjectPin) bool { return p.ProjectUID == anchor }) {
		return db.ErrNotFound
	}
	path := []string{view.AuthorityUID}
	for _, h := range view.Route[:position+1] {
		path = append(path, h.InstanceUID)
	}
	var upstreamActor string
	for _, project := range view.Projects {
		if err := s.scopedProjectAccessTx(ctx, tx, parent.Actor, project.ProjectUID); err != nil {
			return scopedAuthorityLookupError(err)
		}
		pin, err := rootPinTx(ctx, tx, project.ProjectUID, "")
		if err != nil {
			return scopedAuthorityLookupError(err)
		}
		if pin.AuthorityUID != view.AuthorityUID {
			return db.ErrNotFound
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=(SELECT id FROM projects WHERE uid=$1)`, project.ProjectUID))
		if err != nil {
			return scopedAuthorityLookupError(err)
		}
		if !binding.Enabled {
			return db.ErrNotFound
		}
		if position == -1 {
			if binding.Role != db.FederationRoleHub || pin.AuthorityUID != s.instanceUID {
				return db.ErrNotFound
			}
			continue
		}
		config := binding.RelayConfig
		incoming := view.Route[position]
		if binding.Role != db.FederationRoleSpoke || !binding.PushEnabled || binding.HubProjectUID != project.ProjectUID || config == nil || config.Validate(s.instanceUID) != nil || config.ProtocolVersion != db.ScopedRelayProtocolVersion || config.UpstreamRevoked || !config.ServeDownstream || config.AuthorityUID != view.AuthorityUID || config.RootCutoverUID != view.RootCutoverUID || config.BindingUID != incoming.BindingUID || config.ResetEpoch != incoming.Epoch || !slices.Equal(config.HubPath, path) || config.UpstreamInstanceUID != path[len(path)-2] {
			return db.ErrNotFound
		}
		if db.ValidateTokenActor(binding.Actor) != nil || upstreamActor != "" && upstreamActor != binding.Actor || row.BindingUID == incoming.BindingUID && (row.Epoch != incoming.Epoch || row.AccountableActor != binding.Actor) {
			return db.ErrNotFound
		}
		upstreamActor = binding.Actor
		if err := s.scopedProjectAccessTx(ctx, tx, config.LocalActor, project.ProjectUID); err != nil {
			return scopedAuthorityLookupError(err)
		}
	}
	return nil
}

func scopedViewDeliveryScan(scanner rowScanner) (db.ScopedDeliveryRecord, error) {
	var row db.ScopedDeliveryRecord
	var accepted, emitted, acknowledged int
	var binding, direction, channel sql.NullString
	var epoch, sequence sql.NullInt64
	err := scanner.Scan(&row.BindingUID, &row.Epoch, &row.Direction, &row.Channel, &row.Sequence, &row.ObjectUID, &row.ObjectDigest, &row.OfferedDigest, &row.HeaderBytes, &row.EnvelopeBytes, &accepted, &emitted, &acknowledged, &binding, &epoch, &direction, &channel, &sequence)
	if err != nil {
		return row, scopedAuthorityLookupError(err)
	}
	if accepted < 0 || accepted > 1 || emitted < 0 || emitted > 1 || acknowledged < 0 || acknowledged > 1 {
		return row, db.ErrRemoteEventConflict
	}
	row.Accepted, row.Emitted, row.Acknowledged = accepted == 1, emitted == 1, acknowledged == 1
	if binding.Valid || epoch.Valid || direction.Valid || channel.Valid || sequence.Valid {
		if !(binding.Valid && epoch.Valid && direction.Valid && channel.Valid && sequence.Valid) {
			return row, db.ErrRemoteEventConflict
		}
		row.Upstream = &db.ScopedDeliveryRef{ScopedDeliveryStream: db.ScopedDeliveryStream{BindingUID: binding.String, Epoch: epoch.Int64, Direction: db.ScopedDeliveryDirection(direction.String), Channel: db.ScopedDeliveryChannel(channel.String)}, Sequence: sequence.Int64}
	}
	row.HeaderBytes, row.EnvelopeBytes = bytes.Clone(row.HeaderBytes), bytes.Clone(row.EnvelopeBytes)
	return row, row.Validate()
}

const scopedViewDeliverySelect = `SELECT binding_uid,epoch,direction,channel,sequence,object_uid,object_digest,offered_digest,header_bytes,envelope_bytes,accepted,emitted,acknowledged,upstream_binding_uid,upstream_epoch,upstream_direction,upstream_channel,upstream_sequence FROM federation_scoped_delivery`

func scopedViewDeliveryRowTx(ctx context.Context, tx *sql.Tx, ref db.ScopedDeliveryRef) (db.ScopedDeliveryRecord, error) {
	return scopedViewDeliveryScan(tx.QueryRowContext(ctx, scopedViewDeliverySelect+` WHERE binding_uid=$1 AND epoch=$2 AND direction=$3 AND channel=$4 AND sequence=$5`, ref.BindingUID, ref.Epoch, ref.Direction, ref.Channel, ref.Sequence))
}

func scopedViewDeliveryRowsTx(ctx context.Context, tx *sql.Tx, stream db.ScopedDeliveryStream, after, through int64, limit int) ([]db.ScopedDeliveryRecord, error) {
	// Never filter this sequence interval by object UID or generation.
	rows, err := tx.QueryContext(ctx, scopedViewDeliverySelect+` WHERE binding_uid=$1 AND epoch=$2 AND direction=$3 AND channel=$4 AND sequence>$5 AND (sequence<=$6 OR $6=0) ORDER BY sequence LIMIT $7`, stream.BindingUID, stream.Epoch, stream.Direction, stream.Channel, after, through, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	output := []db.ScopedDeliveryRecord{}
	total := 0
	for rows.Next() {
		row, err := scopedViewDeliveryScan(rows)
		if err != nil {
			return nil, err
		}
		total += len(row.HeaderBytes) + len(row.EnvelopeBytes)
		if total > db.MaxScopedEnvelopeBytes {
			return nil, db.ErrScopedCapturePending
		}
		output = append(output, row)
	}
	return output, rows.Err()
}

func (s *Store) scopedViewDeliveryCursorTx(ctx context.Context, tx *sql.Tx, stream db.ScopedDeliveryStream, pins []db.RootKeyPin) (db.ScopedDeliveryCursor, error) {
	cursor := db.ScopedDeliveryCursor{ScopedDeliveryStream: stream}
	err := tx.QueryRowContext(ctx, `SELECT received_sequence,received_digest,accepted_sequence,accepted_digest,emitted_sequence,emitted_digest,acknowledged_sequence,acknowledged_digest FROM federation_scoped_cursors WHERE binding_uid=$1 AND epoch=$2 AND direction=$3 AND channel=$4`+` FOR UPDATE`, stream.BindingUID, stream.Epoch, stream.Direction, stream.Channel).Scan(&cursor.ReceivedSequence, &cursor.ReceivedDigest, &cursor.AcceptedSequence, &cursor.AcceptedDigest, &cursor.EmittedSequence, &cursor.EmittedDigest, &cursor.AcknowledgedSequence, &cursor.AcknowledgedDigest)
	if err != nil {
		return cursor, scopedAuthorityLookupError(err)
	}
	if err := cursor.Validate(); err != nil {
		return cursor, err
	}
	for _, endpoint := range []struct {
		sequence int64
		digest   string
		acked    bool
	}{{cursor.EmittedSequence, cursor.EmittedDigest, false}, {cursor.AcknowledgedSequence, cursor.AcknowledgedDigest, true}} {
		if endpoint.sequence == 0 {
			continue
		}
		row, err := scopedViewDeliveryRowTx(ctx, tx, db.ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: endpoint.sequence})
		if err != nil {
			return cursor, err
		}
		if !row.Emitted || endpoint.acked && !row.Acknowledged || row.OfferedDigest != endpoint.digest {
			return cursor, db.ErrRemoteEventConflict
		}
		if _, err := s.scopedViewDeliveryObjectTx(ctx, tx, row, pins); err != nil {
			return cursor, err
		}
	}
	return cursor, nil
}

func (s *Store) scopedViewDeliveryObjectTx(ctx context.Context, tx *sql.Tx, row db.ScopedDeliveryRecord, pins []db.RootKeyPin) (db.ScopedViewDescriptor, error) {
	var view db.ScopedViewDescriptor
	if err := row.Validate(); err != nil {
		return view, err
	}
	var header db.ScopedDeliveryHeader
	if err := db.DecodeScopedRecord(row.HeaderBytes, &header); err != nil {
		return view, err
	}
	if row.Direction != db.ScopedDeliveryOutbound || row.Channel != db.ScopedDeliveryView || header.SenderInstanceUID != s.instanceUID {
		return view, db.ErrRemoteEventConflict
	}
	var body []byte
	if row.EnvelopeBytes != nil {
		var err error
		_, body, err = db.DecodeScopedDelivery(row.EnvelopeBytes)
		if err != nil {
			return view, err
		}
	} else {
		// Compacted settled rows lack a generation/sequence body key. Resolve
		// their independent exact body without reconstructing an old envelope.
		rows, err := tx.QueryContext(ctx, `SELECT checkpoint_bytes FROM federation_scoped_generations WHERE view_uid=$1 AND checkpoint_digest=$2 UNION ALL SELECT batch_bytes FROM federation_scoped_batches WHERE view_uid=$1 AND digest=$2`, row.ObjectUID, row.ObjectDigest)
		if err != nil {
			return view, err
		}
		count := 0
		for rows.Next() {
			var candidate []byte
			if err = rows.Scan(&candidate); err != nil {
				break
			}
			if len(candidate) == 0 || len(candidate) > db.MaxScopedCheckpointBytes || db.ScopedOfferedDigest(candidate) != header.BodyDigest {
				continue
			}
			body, count = candidate, count+1
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return view, err
		}
		if count != 1 {
			return view, db.ErrRemoteEventConflict
		}
	}
	var checkpoint db.ScopedCheckpoint
	var key db.ScopedViewKey
	var batch db.ScopedBatch
	isCheckpoint := db.DecodeScopedRecord(body, &checkpoint) == nil
	if isCheckpoint {
		key = checkpoint.View.Key
	} else {
		if err := db.DecodeScopedRecord(body, &batch); err != nil {
			return view, err
		}
		key = batch.View.Key
	}
	view, _, err := s.scopedViewDeliveryContextTx(ctx, tx, key)
	if err != nil {
		return view, err
	}
	digest := batch.Cut.Digest
	if isCheckpoint {
		digest = checkpoint.Cut.Digest
		err = db.VerifyScopedCheckpoint(checkpoint, view, pins)
	} else {
		err = db.VerifyScopedBatch(batch, view, pins)
	}
	if err != nil {
		return view, err
	}
	// A full offer carries its own exact object key, so the common path uses
	// primary-key lookups rather than rescanning retained generation history.
	var retained []byte
	if isCheckpoint {
		err = tx.QueryRowContext(ctx, `SELECT checkpoint_bytes FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation).Scan(&retained)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT batch_bytes FROM federation_scoped_batches WHERE view_uid=$1 AND generation=$2 AND sequence=$3`, key.ViewUID, key.Generation, batch.Cut.Sequence).Scan(&retained)
	}
	if err != nil {
		return view, scopedAuthorityLookupError(err)
	}
	if !bytes.Equal(retained, body) || db.ScopedOfferedDigest(retained) != header.BodyDigest {
		return view, db.ErrRemoteEventConflict
	}
	hop, position, err := scopedHop(view, s.instanceUID)
	if err != nil || position+1 >= len(view.Route) {
		return view, db.ErrNotFound
	}
	path := []string{view.AuthorityUID}
	for _, h := range view.Route[:position+1] {
		path = append(path, h.InstanceUID)
	}
	if row.ObjectUID != view.Key.ViewUID || digest != row.ObjectDigest || header.AuthorityUID != view.AuthorityUID || header.BindingUID != hop.BindingUID || header.Epoch != hop.Epoch || header.ReceiverInstanceUID != hop.InstanceUID || !slices.Equal(header.Path, path) {
		return view, db.ErrRemoteEventConflict
	}
	if row.Upstream != nil {
		if position < 0 || row.Upstream.BindingUID != view.Route[position].BindingUID || row.Upstream.Epoch != view.Route[position].Epoch || row.Upstream.Channel != db.ScopedDeliveryView {
			return view, db.ErrRemoteEventConflict
		}
		upstream, err := scopedViewDeliveryRowTx(ctx, tx, *row.Upstream)
		if err != nil {
			return view, err
		}
		var upstreamHeader db.ScopedDeliveryHeader
		if err := db.DecodeScopedRecord(upstream.HeaderBytes, &upstreamHeader); err != nil {
			return view, err
		}
		if !upstream.Accepted || upstream.ObjectUID != row.ObjectUID || upstream.ObjectDigest != row.ObjectDigest || upstreamHeader.AuthorityUID != header.AuthorityUID || upstreamHeader.BodyDigest != header.BodyDigest || upstreamHeader.ReceiverInstanceUID != s.instanceUID || upstreamHeader.SenderInstanceUID != path[len(path)-2] || !slices.Equal(upstreamHeader.Path, path[:len(path)-1]) {
			return view, db.ErrRemoteEventConflict
		}
	}
	return view, nil
}
