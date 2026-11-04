package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"reflect"
)

type cronSummaryBindingKey struct{}
type cronSummaryBinding struct{ expected *FederationBinding }

// WithCronSummaryBinding fences the exact endpoint whose support was checked.
// A nil binding also fences standalone admission against a concurrent join.
func WithCronSummaryBinding(ctx context.Context, expected *FederationBinding) context.Context {
	return context.WithValue(ctx, cronSummaryBindingKey{}, cronSummaryBinding{expected})
}

func checkCronSummaryBinding(ctx context.Context, tx *sql.Tx, projectID int64) error {
	admission, ok := ctx.Value(cronSummaryBindingKey{}).(cronSummaryBinding)
	if !ok {
		return nil
	}
	var current FederationBinding
	var relay sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT role,hub_url,hub_project_id,hub_project_uid,push_enabled,bound_actor,allow_insecure,enabled,relay_config FROM federation_bindings WHERE project_id=$1`, projectID).Scan(&current.Role, &current.HubURL, &current.HubProjectID, &current.HubProjectUID, &current.PushEnabled, &current.Actor, &current.AllowInsecure, &current.Enabled, &relay)
	if errors.Is(err, sql.ErrNoRows) && admission.expected == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnsupportedEventFeatures
	}
	if err != nil {
		return err
	}
	if relay.Valid {
		if err := json.Unmarshal([]byte(relay.String), &current.RelayConfig); err != nil {
			return err
		}
	}
	b := admission.expected
	if b == nil || b.Role != current.Role || b.HubURL != current.HubURL || b.HubProjectID != current.HubProjectID || b.HubProjectUID != current.HubProjectUID || b.PushEnabled != current.PushEnabled || b.Actor != current.Actor || b.AllowInsecure != current.AllowInsecure || b.Enabled != current.Enabled || !reflect.DeepEqual(b.RelayConfig, current.RelayConfig) {
		return ErrUnsupportedEventFeatures
	}
	return nil
}
