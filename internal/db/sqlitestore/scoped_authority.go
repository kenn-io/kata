package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

var _ db.ScopedRelayEnrollmentStore = (*Store)(nil)

func scopedAuthorityLookupError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	return err
}

// scopedHop locates this store, never a generic view-indexed account or hop.
func scopedHop(v db.ScopedViewDescriptor, instance string) (db.ScopedHopBinding, int, error) {
	if err := v.Validate(); err != nil {
		return db.ScopedHopBinding{}, 0, err
	}
	position := -2
	if instance == v.AuthorityUID {
		position = -1
	}
	for i, hop := range v.Route {
		if hop.InstanceUID == v.AuthorityUID {
			return db.ScopedHopBinding{}, 0, db.ErrNotFound
		}
		if hop.InstanceUID == instance {
			position = i
		}
	}
	if position == -2 {
		return db.ScopedHopBinding{}, 0, db.ErrNotFound
	}
	if position+1 == len(v.Route) {
		return v.Route[position], position, nil
	}
	return v.Route[position+1], position, nil
}

// scopedOutgoingRoleAllowed applies native limits to the adjacent grant hop.
// A retained descriptor can continue only through a forwarding grant; the
// final leaf does not consume a hub slot.
func scopedOutgoingRoleAllowed(v db.ScopedViewDescriptor, position int, serveDownstream bool) bool {
	return len(v.Route)+1 <= db.MaxRelayHubs+1 &&
		(!serveDownstream || position+3 <= db.MaxRelayHubs) &&
		(serveDownstream || position+2 == len(v.Route))
}

func (s *Store) scopedParentTx(ctx context.Context, tx db.Transaction, id int64, locked bool) (db.APIToken, error) {
	query := apiTokenSelect + ` WHERE id=$1`
	_ = locked
	parent, err := scanAPIToken(tx.QueryRowContext(ctx, query, id))
	if err != nil {
		return parent, err
	}
	if parent.Scope != nil || parent.RevokedAt != nil || parent.ExpiresAt != nil && !time.Now().Before(*parent.ExpiresAt) || db.ValidateTokenActor(parent.Actor) != nil {
		return parent, db.ErrNotFound
	}
	return parent, nil
}

func (s *Store) scopedProjectAccessTx(ctx context.Context, tx db.Transaction, actor, project string) error {
	if db.ValidateTokenActor(actor) != nil || !db.ProjectAttributionVisible(ctx, project) {
		return db.ErrNotFound
	}
	var allowed int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects p LEFT JOIN project_access_policies a ON a.project_uid=p.uid WHERE p.uid=$1 AND p.uid<>$2 AND (COALESCE(a.visibility,'all')='all' OR EXISTS(SELECT 1 FROM project_access_teams pat JOIN team_memberships tm ON tm.team_uid=pat.team_uid WHERE pat.project_uid=p.uid AND tm.actor=$3 AND tm.actor<>''))`, project, db.SystemProjectUID, actor).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	return err
}

// Lock selected native project/binding rows before enrollment/parent rows.
func (s *Store) scopedBindingLocksTx(ctx context.Context, tx db.Transaction, v db.ScopedViewDescriptor) error {
	for _, project := range v.Projects {
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1 AND deleted_at IS NULL`+``, project.ProjectUID).Scan(&id); err != nil {
			return db.RelayProjectLookupError(err)
		}
		if _, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`+``, id)); err != nil {
			return err
		}
	}
	return nil
}

// scopedSelectionTx uses the native project policy, pins, floor and path. It
// does not turn prepared context into active ownership or serving permission.
func (s *Store) scopedSelectionTx(ctx context.Context, tx db.Transaction, v db.ScopedViewDescriptor, actor string, position int, capability string) ([]int64, error) {
	ids := make([]int64, 0, len(v.Projects))
	incomingTerminal := position+1 == len(v.Route)
	var upstreamActor string
	for _, selected := range v.Projects {
		if !incomingTerminal {
			if err := s.scopedProjectAccessTx(ctx, tx, actor, selected.ProjectUID); err != nil {
				return nil, scopedAuthorityLookupError(err)
			}
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1 AND deleted_at IS NULL`, selected.ProjectUID).Scan(&id); err != nil {
			return nil, db.RelayProjectLookupError(err)
		}
		ids = append(ids, id)
		pin, err := rootPinTx(ctx, tx, selected.ProjectUID, "")
		if err != nil {
			return nil, scopedAuthorityLookupError(err)
		}
		if !reflect.DeepEqual(pin, selected.Pin) {
			return nil, db.ErrRelayRootChanged
		}
		var floor int64
		var cutover, owner *string
		var generation *int64
		var fenced int
		err = tx.QueryRowContext(ctx, `SELECT protocol_floor,root_cutover_uid,owner_view_uid,owner_generation,serving_fenced FROM federation_scoped_projects WHERE project_uid=$1`, selected.ProjectUID).Scan(&floor, &cutover, &owner, &generation, &fenced)
		if err != nil {
			return nil, scopedAuthorityLookupError(err)
		}
		if floor != db.ScopedRelayProtocolVersion || cutover == nil || *cutover != v.RootCutoverUID || fenced != 0 {
			return nil, db.ErrNotFound
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, id))
		if err != nil {
			return nil, scopedAuthorityLookupError(err)
		}
		if !binding.Enabled {
			return nil, db.ErrNotFound
		}
		if position == -1 {
			if binding.Role != db.FederationRoleHub || owner != nil || generation != nil || pin.AuthorityUID != s.instanceUID {
				return nil, db.ErrNotFound
			}
			continue
		}
		config := binding.RelayConfig
		if binding.Role != db.FederationRoleSpoke || (!incomingTerminal || capability != "pull") && !binding.PushEnabled || config == nil || config.Validate(s.instanceUID) != nil || config.ProtocolVersion != db.ScopedRelayProtocolVersion || config.UpstreamRevoked || config.ScopedView == nil || config.AuthorityUID != v.AuthorityUID || config.RootCutoverUID != v.RootCutoverUID || binding.HubProjectUID != selected.ProjectUID || owner == nil || generation == nil || *owner != config.ScopedView.ViewUID || *generation != config.ScopedView.Generation {
			return nil, db.ErrNotFound
		}
		if db.ValidateTokenActor(binding.Actor) != nil {
			return nil, db.ErrNotFound
		}
		if upstreamActor == "" {
			upstreamActor = binding.Actor
		}
		if binding.Actor != upstreamActor || incomingTerminal && binding.Actor != actor {
			return nil, db.ErrNotFound
		}
		ownerDescriptor, ownerRow, err := s.scopedDescriptorTx(ctx, tx, *config.ScopedView)
		if err != nil {
			return nil, scopedAuthorityLookupError(err)
		}
		if ownerRow.ActiveGeneration == nil || *ownerRow.ActiveGeneration != config.ScopedView.Generation || ownerDescriptor.DestinationUID != s.instanceUID || ownerDescriptor.AuthorityUID != v.AuthorityUID || ownerDescriptor.RootCutoverUID != v.RootCutoverUID || !slices.Equal(ownerDescriptor.Route, v.Route[:position+1]) || !slices.ContainsFunc(ownerDescriptor.Projects, func(p db.ScopedProjectPin) bool { return reflect.DeepEqual(p, selected) }) {
			return nil, db.ErrNotFound
		}
		if ownerRow.BindingUID == config.BindingUID && (ownerRow.Epoch != config.ResetEpoch || ownerRow.AccountableActor != binding.Actor) {
			return nil, db.ErrNotFound
		}
		path := []string{v.AuthorityUID}
		for _, hop := range v.Route[:position+1] {
			path = append(path, hop.InstanceUID)
		}
		incoming := v.Route[position]
		if !slices.Equal(config.HubPath, path) || config.BindingUID != incoming.BindingUID || config.ResetEpoch != incoming.Epoch || config.UpstreamInstanceUID != path[len(path)-2] || position+1 < len(v.Route) && !config.ServeDownstream {
			return nil, db.ErrNotFound
		}
		if err := s.scopedProjectAccessTx(ctx, tx, config.LocalActor, selected.ProjectUID); err != nil {
			return nil, scopedAuthorityLookupError(err)
		}
		// Destination bindings must identify this exact retained owner generation.
		if position+1 == len(v.Route) && *config.ScopedView != v.Key {
			return nil, db.ErrNotFound
		}
	}
	var committed int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM federation_protocol_cutovers WHERE cutover_uid=$1 AND state='committed'`, v.RootCutoverUID).Scan(&committed)
	if err != nil {
		return nil, scopedAuthorityLookupError(err)
	}
	return ids, nil
}

func (s *Store) scopedDescriptorTx(ctx context.Context, tx db.Transaction, key db.ScopedViewKey) (db.ScopedViewDescriptor, db.ScopedViewExport, error) {
	var v db.ScopedViewDescriptor
	var row db.ScopedViewExport
	var raw []byte
	var digest, state string
	err := tx.QueryRowContext(ctx, `SELECT view_uid,catalog_entry_uid,authority_uid,binding_uid,epoch,accountable_actor,route_digest,destination_uid,active_generation,prepared_generation,state,reason_code,request_digest FROM federation_scoped_views WHERE view_uid=$1`, key.ViewUID).Scan(&row.ViewUID, &row.CatalogEntryUID, &row.AuthorityUID, &row.BindingUID, &row.Epoch, &row.AccountableActor, &row.RouteDigest, &row.DestinationUID, &row.ActiveGeneration, &row.PreparedGeneration, &row.State, &row.ReasonCode, &row.RequestDigest)
	if err != nil {
		return v, row, scopedAuthorityLookupError(err)
	}
	err = tx.QueryRowContext(ctx, `SELECT descriptor_bytes,descriptor_digest,state FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, key.ViewUID, key.Generation).Scan(&raw, &digest, &state)
	if err != nil {
		return v, row, scopedAuthorityLookupError(err)
	}
	if db.ScopedOfferedDigest(raw) != digest || db.DecodeScopedRecord(raw, &v) != nil || v.Key != key || v.AuthorityUID != row.AuthorityUID || v.RouteDigest != row.RouteDigest || v.DestinationUID != row.DestinationUID || row.State == "retired" || row.State == "fenced" || state == "retired" || state == "fenced" {
		return v, row, db.ErrNotFound
	}
	// Active-old context remains usable while the summary names preparation.
	if state == "active" && (row.ActiveGeneration == nil || *row.ActiveGeneration != key.Generation) || state == "prepared" && (row.PreparedGeneration == nil || *row.PreparedGeneration != key.Generation) {
		return v, row, db.ErrNotFound
	}
	return v, row, nil
}

func (s *Store) scopedAuthorityTx(ctx context.Context, tx db.Transaction, key db.ScopedViewKey, capability string) (db.ScopedViewDescriptor, error) {
	v, row, err := s.scopedDescriptorTx(ctx, tx, key)
	if err != nil {
		return v, scopedAuthorityLookupError(err)
	}
	hop, position, err := scopedHop(v, s.instanceUID)
	if err != nil {
		return v, scopedAuthorityLookupError(err)
	}
	if len(v.Route)+1 > db.MaxRelayHubs+1 || !db.IsSupportedFederationCapability(capability) {
		return v, db.ErrNotFound
	}
	if position+1 == len(v.Route) {
		// Incoming authority uses the real upstream account, not a remote parent ID.
		if len(v.Projects) == 0 {
			return v, db.ErrNotFound
		}
		var actor string
		err = tx.QueryRowContext(ctx, `SELECT b.bound_actor FROM federation_bindings b JOIN projects p ON p.id=b.project_id WHERE p.uid=$1`, v.Projects[0].ProjectUID).Scan(&actor)
		if err != nil {
			return v, scopedAuthorityLookupError(err)
		}
		if row.BindingUID == hop.BindingUID && (row.Epoch != hop.Epoch || row.AccountableActor != actor) {
			return v, db.ErrNotFound
		}
		_, err = s.scopedSelectionTx(ctx, tx, v, actor, position, capability)
		return v, scopedAuthorityLookupError(err)
	}
	grant, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE relay_binding_uid=$1`, hop.BindingUID))
	if err != nil {
		return v, scopedAuthorityLookupError(err)
	}
	if grant.RelayProtocolVersion != db.ScopedRelayProtocolVersion || grant.RevokedAt != nil || grant.ParentTokenID == nil || grant.ProjectID == nil || grant.SpokeInstanceUID != hop.InstanceUID || grant.RelayResetEpoch != hop.Epoch || !scopedOutgoingRoleAllowed(v, position, grant.RelayServeDownstream) || !slices.Contains(strings.Split(grant.Capabilities, ","), capability) {
		return v, db.ErrNotFound
	}
	parent, err := s.scopedParentTx(ctx, tx, *grant.ParentTokenID, false)
	if err != nil {
		return v, scopedAuthorityLookupError(err)
	}
	if parent.Actor != grant.Actor || row.BindingUID == hop.BindingUID && (row.Epoch != hop.Epoch || row.AccountableActor != parent.Actor) {
		return v, db.ErrNotFound
	}
	ids, err := s.scopedSelectionTx(ctx, tx, v, parent.Actor, position, capability)
	if err != nil {
		return v, scopedAuthorityLookupError(err)
	}
	if !slices.Contains(ids, *grant.ProjectID) {
		return v, db.ErrNotFound
	}
	return v, nil
}

func (s *Store) fenceScopedAuthorityTx(ctx context.Context, tx db.Transaction, key db.ScopedViewKey, capability string) (db.ScopedViewDescriptor, error) {
	if err := lockProjectAccess(ctx, tx); err != nil {
		return db.ScopedViewDescriptor{}, err
	}
	v, _, err := s.scopedDescriptorTx(ctx, tx, key)
	if err != nil {
		return v, err
	}
	if err := s.scopedBindingLocksTx(ctx, tx, v); err != nil {
		return v, err
	}
	hop, position, err := scopedHop(v, s.instanceUID)
	if err != nil {
		return v, err
	}
	if position+1 < len(v.Route) {
		query := federationEnrollmentSelect + ` WHERE relay_binding_uid=$1` + ``
		grant, err := scanFederationEnrollment(tx.QueryRowContext(ctx, query, hop.BindingUID))
		if err != nil {
			return v, err
		}
		if grant.ParentTokenID == nil {
			return v, db.ErrNotFound
		}
		if _, err := s.scopedParentTx(ctx, tx, *grant.ParentTokenID, true); err != nil {
			return v, err
		}
	}
	return s.scopedAuthorityTx(ctx, tx, key, capability)
}

// CreateScopedRelayEnrollment performs native initial admission only. T06/T13
// retain activation/readiness and T10 owns wire negotiation; v1 stays unchanged.
func (s *Store) CreateScopedRelayEnrollment(ctx context.Context, p db.CreateScopedRelayEnrollmentParams) (db.CreatedFederationEnrollment, error) {
	var result db.CreatedFederationEnrollment
	if p.AnchorProjectID <= 0 || p.ParentTokenID <= 0 || !uid.Valid(p.CatalogEntryUID) || !uid.Valid(p.View.RootCutoverUID) {
		return result, db.ErrNotFound
	}
	raw, err := json.Marshal(p.View)
	if err != nil {
		return result, err
	}
	var v db.ScopedViewDescriptor
	if err := db.DecodeScopedRecord(raw, &v); err != nil {
		return result, err
	}
	hop, position, err := scopedHop(v, s.instanceUID)
	if err != nil {
		return result, err
	}
	if position+1 == len(v.Route) || hop.InstanceUID == s.instanceUID {
		return result, db.ErrNotFound
	}
	// ServeDownstream describes the adjacent peer, not the final leaf.
	if !scopedOutgoingRoleAllowed(v, position, p.ServeDownstream) {
		return result, db.ErrNotFound
	}
	explicit := p.Token != ""
	if !explicit {
		p.Token, err = db.NewFederationToken()
		if err != nil {
			return result, err
		}
	}
	err = s.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.CreatedFederationEnrollment{}
		if err := lockProjectAccess(ctx, tx); err != nil {
			return err
		}
		if err := s.scopedBindingLocksTx(ctx, tx, v); err != nil {
			return err
		}
		grant, grantErr := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE token_hash=$1 OR relay_binding_uid=$2`+``, db.FederationTokenHash(p.Token), hop.BindingUID))
		if grantErr != nil && !errors.Is(grantErr, db.ErrNotFound) {
			return grantErr
		}
		parent, err := s.scopedParentTx(ctx, tx, p.ParentTokenID, true)
		if err != nil {
			return err
		}
		ids, err := s.scopedSelectionTx(ctx, tx, v, parent.Actor, position, "push")
		if err != nil {
			return err
		}
		if !slices.Contains(ids, p.AnchorProjectID) {
			return db.ErrNotFound
		}
		projectUIDs := make([]string, len(v.Projects))
		for i, project := range v.Projects {
			projectUIDs[i] = project.ProjectUID
		}
		if err := s.ProjectAccessTransactionFence(parent.Actor, projectUIDs)(ctx, tx); err != nil {
			return err
		}
		if grantErr == nil {
			if !explicit || grant.TokenHash != db.FederationTokenHash(p.Token) || grant.RelayBindingUID != hop.BindingUID || grant.RelayProtocolVersion != db.ScopedRelayProtocolVersion || grant.ProjectID == nil || *grant.ProjectID != p.AnchorProjectID || grant.ParentTokenID == nil || !p.RebindParent && *grant.ParentTokenID != p.ParentTokenID || grant.Actor != parent.Actor || grant.SpokeInstanceUID != hop.InstanceUID || grant.RevokedAt != nil || grant.RelayServeDownstream != p.ServeDownstream || grant.RelayResetEpoch != hop.Epoch || grant.Capabilities != "claim,pull,push" {
				return db.ErrFederationEnrollmentTokenConflict
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_scoped_views WHERE view_uid=$1`, v.Key.ViewUID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			old, row, err := s.scopedDescriptorTx(ctx, tx, v.Key)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(old, v) || row.CatalogEntryUID != p.CatalogEntryUID || row.PreparedGeneration == nil || *row.PreparedGeneration != v.Key.Generation {
				return db.ErrRemoteEventConflict
			}
		} else {
			if v.Key.Generation != 1 {
				return db.ErrRemoteEventConflict
			}
			var active int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_scoped_views WHERE authority_uid=$1 AND binding_uid=$2 AND state IN ('active','prepared')`, v.AuthorityUID, hop.BindingUID).Scan(&active); err != nil {
				return err
			}
			if active >= db.MaxScopedActiveViews {
				return errors.New("scoped active view quota exceeded")
			}
			generation := v.Key.Generation
			rows := []db.ImportRecord{
				&db.ScopedViewExport{ViewUID: v.Key.ViewUID, CatalogEntryUID: p.CatalogEntryUID, AuthorityUID: v.AuthorityUID, BindingUID: hop.BindingUID, Epoch: hop.Epoch, AccountableActor: parent.Actor, RouteDigest: v.RouteDigest, DestinationUID: v.DestinationUID, PreparedGeneration: &generation, State: "prepared", RequestDigest: v.RequestDigest},
				&db.ScopedGenerationExport{ViewUID: v.Key.ViewUID, Generation: generation, DescriptorBytes: raw, DescriptorDigest: db.ScopedOfferedDigest(raw), State: "prepared", RootCutoverUID: v.RootCutoverUID},
				&db.ScopedWorkExport{ViewUID: v.Key.ViewUID, DirtyRevision: 1, State: "dirty"},
			}
			var retained, inheritedPrivate int64
			operationBytes := map[string]int64{}
			operationPrincipal := map[string]db.CustodyPrincipal{}
			sameRoute := func(p db.CustodyPrincipal) bool {
				return p.BindingUID == hop.BindingUID && p.RouteDigest == v.RouteDigest
			}
			for record, err := range db.ExportScopedStateSQL(ctx, tx.QueryContext) {
				if err != nil {
					return err
				}
				size := db.ScopedRetainedBytes(record)
				retained += size
				switch r := record.(type) {
				case *db.ScopedSourceOperationExport:
					var principal db.CustodyPrincipal
					if err := db.DecodeScopedRecord(r.PrincipalBytes, &principal); err != nil {
						return err
					}
					operationPrincipal[r.OperationUID] = principal
					operationBytes[r.OperationUID] += size
				case *db.ScopedPrivateSourceExport:
					if r.OperationUID != nil {
						operationBytes[*r.OperationUID] += size
					}
				case *db.ScopedControlCustodyExport:
					var principal db.CustodyPrincipal
					if err := db.DecodeScopedRecord(r.PrincipalBytes, &principal); err != nil {
						return err
					}
					if sameRoute(principal) {
						inheritedPrivate += size
					}
				}
			}
			for operation, size := range operationBytes {
				if sameRoute(operationPrincipal[operation]) {
					inheritedPrivate += size
				}
			}
			inheritedPrivate += db.ScopedRetainedBytes(rows[2])
			if inheritedPrivate > db.MaxScopedRecoveryBytes {
				return errors.New("scoped private recovery quota exceeded")
			}

			for _, record := range rows {
				retained += db.ScopedRetainedBytes(record)
			}
			if retained > db.MaxScopedDaemonBytes || len(raw) > db.MaxScopedCheckpointBytes {
				return errors.New("scoped retained byte quota exceeded")
			}
			for _, record := range rows {
				if err := db.ReplayScopedRecord(ctx, tx, record); err != nil {
					return err
				}
			}
		}
		if grantErr != nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO federation_enrollments(token_hash,spoke_instance_uid,project_id,capabilities,bound_actor,relay_binding_uid,relay_protocol_version,parent_token_id,relay_reset_epoch,relay_serve_downstream) VALUES($1,$2,$3,'claim,pull,push',$4,$5,2,$6,$7,$8)`, db.FederationTokenHash(p.Token), hop.InstanceUID, p.AnchorProjectID, parent.Actor, hop.BindingUID, p.ParentTokenID, hop.Epoch, p.ServeDownstream)
			if err != nil {
				return err
			}
		} else if *grant.ParentTokenID != p.ParentTokenID {
			if _, err = tx.ExecContext(ctx, `UPDATE federation_enrollments SET parent_token_id=$1 WHERE id=$2`, p.ParentTokenID, grant.ID); err != nil {
				return err
			}
		}
		grant, err = scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE token_hash=$1`, db.FederationTokenHash(p.Token)))
		if err != nil {
			return err
		}
		result = db.CreatedFederationEnrollment{Enrollment: grant, Token: p.Token}
		return nil
	})
	return result, err
}

// scopedIncomingConfigTx checks a candidate against retained active owner proof
// and every real native binding before installing any shared-hop configuration.
func (s *Store) scopedIncomingConfigTx(ctx context.Context, tx db.Transaction, projectID int64, config db.RelayBindingConfig) error {
	if config.ScopedView == nil || config.UpstreamRevoked {
		return db.ErrNotFound
	}
	v, row, err := s.scopedDescriptorTx(ctx, tx, *config.ScopedView)
	if err != nil {
		return scopedAuthorityLookupError(err)
	}
	hop, position, err := scopedHop(v, s.instanceUID)
	if err != nil {
		return scopedAuthorityLookupError(err)
	}
	if position+1 != len(v.Route) || len(v.Route)+1 > db.MaxRelayHubs+1 || row.ActiveGeneration == nil || *row.ActiveGeneration != v.Key.Generation || config.AuthorityUID != v.AuthorityUID || config.RootCutoverUID != v.RootCutoverUID || config.BindingUID != hop.BindingUID || config.ResetEpoch != hop.Epoch {
		return db.ErrNotFound
	}
	path := []string{v.AuthorityUID}
	for _, h := range v.Route {
		path = append(path, h.InstanceUID)
	}
	if !slices.Equal(config.HubPath, path) || config.UpstreamInstanceUID != path[len(path)-2] {
		return db.ErrNotFound
	}
	var upstream string
	found := false
	for _, selected := range v.Projects {
		if err := s.scopedProjectAccessTx(ctx, tx, config.LocalActor, selected.ProjectUID); err != nil {
			return scopedAuthorityLookupError(err)
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1 AND deleted_at IS NULL`, selected.ProjectUID).Scan(&id); err != nil {
			return db.RelayProjectLookupError(err)
		}
		found = found || id == projectID
		b, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, id))
		if err != nil {
			return scopedAuthorityLookupError(err)
		}
		if b.Role != db.FederationRoleSpoke || !b.Enabled || b.HubProjectUID != selected.ProjectUID || db.ValidateTokenActor(b.Actor) != nil {
			return db.ErrNotFound
		}
		if upstream == "" {
			upstream = b.Actor
		}
		if upstream != b.Actor {
			return db.ErrNotFound
		}
		if b.RelayConfig != nil && !reflect.DeepEqual(b.RelayConfig, &config) {
			return db.ErrRemoteEventConflict
		}
		pin, err := rootPinTx(ctx, tx, selected.ProjectUID, "")
		if err != nil {
			return scopedAuthorityLookupError(err)
		}
		if !reflect.DeepEqual(pin, selected.Pin) {
			return db.ErrRelayRootChanged
		}
		var floor, gen int64
		var owner, cutover string
		var fenced int
		if err := tx.QueryRowContext(ctx, `SELECT protocol_floor,owner_view_uid,owner_generation,root_cutover_uid,serving_fenced FROM federation_scoped_projects WHERE project_uid=$1`, selected.ProjectUID).Scan(&floor, &owner, &gen, &cutover, &fenced); err != nil {
			return scopedAuthorityLookupError(err)
		}
		if floor != db.ScopedRelayProtocolVersion || owner != v.Key.ViewUID || gen != v.Key.Generation || cutover != v.RootCutoverUID || fenced != 0 {
			return db.ErrNotFound
		}
	}
	if !found || row.BindingUID == hop.BindingUID && (row.AccountableActor != upstream || row.Epoch != hop.Epoch) {
		return db.ErrNotFound
	}
	var committed int
	return scopedAuthorityLookupError(tx.QueryRowContext(ctx, `SELECT 1 FROM federation_protocol_cutovers WHERE cutover_uid=$1 AND state='committed'`, v.RootCutoverUID).Scan(&committed))
}
