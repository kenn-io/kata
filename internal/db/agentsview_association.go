package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/agentsviewrelay"
)

// ErrAgentsViewRouteChanged means the stored route no longer matches the selection.
var ErrAgentsViewRouteChanged = errors.New("AgentsView selection no longer matches the stored project route")

// CheckAgentsViewAssociationTx validates the local selection in the actual
// mutation transaction. It does no network or credential I/O and uses only
// existing project, root-pin and binding state. Both native backends call it.
func CheckAgentsViewAssociationTx(ctx context.Context, tx *sql.Tx, issue Issue, instanceUID string, a *agentsviewrelay.Association) error {
	if a == nil {
		return nil
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if issue.UID != a.IssueUID || a.Route[len(a.Route)-1].InstanceUID != instanceUID {
		return ErrAgentsViewRouteChanged
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL`, issue.ProjectID).Scan(&projectUID); err != nil {
		return ErrAgentsViewRouteChanged
	}
	if projectUID != a.ProjectUID || !ProjectAttributionVisible(ctx, projectUID) {
		return ErrAgentsViewRouteChanged
	}
	var authority, keyID string
	if err := tx.QueryRowContext(ctx, `SELECT authority_uid,key_id FROM federation_root_keys WHERE project_uid=$1 AND active=1`, projectUID).Scan(&authority, &keyID); err != nil || authority != a.AuthorityUID || keyID != a.RootKeyID {
		return ErrAgentsViewRouteChanged
	}
	var role FederationRole
	var hubUID string
	var enabled, push bool
	var relay *string
	if err := tx.QueryRowContext(ctx, `SELECT role,hub_project_uid,enabled,push_enabled,relay_config FROM federation_bindings WHERE project_id=$1`, issue.ProjectID).Scan(&role, &hubUID, &enabled, &push, &relay); err != nil || !enabled || hubUID != projectUID {
		return ErrAgentsViewRouteChanged
	}
	if role == FederationRoleHub {
		if a.AuthorityUID != instanceUID || len(a.Route) != 1 {
			return ErrAgentsViewRouteChanged
		}
		return nil
	}
	if role != FederationRoleSpoke || !push || relay == nil {
		return ErrAgentsViewRouteChanged
	}
	var config RelayBindingConfig
	if err := json.Unmarshal([]byte(*relay), &config, json.RejectUnknownMembers(true)); err != nil || config.Validate(instanceUID) != nil || config.UpstreamRevoked || config.AuthorityUID != a.AuthorityUID {
		return ErrAgentsViewRouteChanged
	}
	path := make([]string, len(a.Route))
	for i, hop := range a.Route {
		path[i] = hop.InstanceUID
	}
	local := a.Route[len(a.Route)-1]
	if !slices.Equal(path, config.HubPath) || local.BindingUID != config.BindingUID || local.UpstreamInstanceUID != config.UpstreamInstanceUID || local.ResetEpoch != config.ResetEpoch {
		return ErrAgentsViewRouteChanged
	}
	return nil
}
