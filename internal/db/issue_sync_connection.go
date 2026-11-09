package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/tracker"
)

const issueSyncConnectionKey = "_tracker_connection"

// IssueSyncConnection is the nonsecret identity of the explicitly selected
// credential owner. Its presence never grants permission to use the connection.
// Keep it in binding config so existing export and snapshot fencing carry it.
type IssueSyncConnection = tracker.Selection

// IssueSyncConnectionFromConfig returns nil only when the selection is absent.
// Malformed or unknown metadata must never downgrade a named binding to legacy.
func IssueSyncConnectionFromConfig(config jsontext.Value) (*IssueSyncConnection, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return nil, invalidIssueSyncConnection()
	}
	raw, present := fields[issueSyncConnectionKey]
	if !present {
		return nil, nil
	}
	var selection IssueSyncConnection
	if string(raw) == "null" || json.Unmarshal(raw, &selection, json.RejectUnknownMembers(true)) != nil {
		return nil, invalidIssueSyncConnection()
	}
	if err := selection.Validate(); err != nil {
		return nil, invalidIssueSyncConnection()
	}
	return &selection, nil
}

// SetIssueSyncConnectionConfig sets a validated selection after the caller has
// checked live grants and fenced the old binding. It cannot clear a selection.
func SetIssueSyncConnectionConfig(config jsontext.Value, selection IssueSyncConnection) (jsontext.Value, error) {
	if err := selection.Validate(); err != nil {
		return nil, invalidIssueSyncConnection()
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(config, &fields); err != nil || fields == nil {
		return nil, invalidIssueSyncConnection()
	}
	raw, err := json.Marshal(selection)
	if err != nil {
		return nil, invalidIssueSyncConnection()
	}
	fields[issueSyncConnectionKey] = raw
	return json.Marshal(fields, json.Deterministic(true))
}

// ProviderIssueSyncConfig removes binding-level identity before a provider's
// strict decoder runs. Public exports retain this identity, but no credentials.
func ProviderIssueSyncConfig(config jsontext.Value) (jsontext.Value, error) {
	public, err := PublicIssueSyncConfig(config)
	if err != nil {
		return nil, err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(public, &fields); err != nil || fields == nil {
		return nil, invalidIssueSyncConnection()
	}
	delete(fields, issueSyncConnectionKey)
	return json.Marshal(fields, json.Deterministic(true))
}

func invalidIssueSyncConnection() error {
	return fmt.Errorf("%w: invalid tracker connection identity", ErrImportValidation)
}

// ValidateIssueSyncConnectionMarker makes the physical named-account marker a
// fail-closed check before routing or credential resolution. Partial identity
// cannot be mistaken for a legacy binding by the current binary.
func ValidateIssueSyncConnectionMarker(marker *string, config jsontext.Value) error {
	selection, err := IssueSyncConnectionFromConfig(config)
	if err != nil {
		return err
	}
	if selection == nil {
		if marker != nil {
			return invalidIssueSyncConnection()
		}
		return nil
	}
	if marker == nil || *marker != selection.ConnectionUID {
		return invalidIssueSyncConnection()
	}
	return nil
}

// PrepareIssueSyncBindingConfig is shared by native stores. Named selection
// uses a typed admission, never raw provider JSON. Existing selection is stable
// through ordinary enable/refresh; replacement needs a separate recovery path.
func PrepareIssueSyncBindingConfig(previous *IssueSyncBinding, p UpsertIssueSyncBindingParams) (jsontext.Value, *string, error) {
	supplied, err := IssueSyncConnectionFromConfig(p.Config)
	if err != nil {
		return nil, nil, err
	}
	var old *IssueSyncConnection
	config := p.Config
	if previous != nil {
		if err := ValidateIssueSyncConnectionMarker(previous.ConnectionUID, previous.Config); err != nil {
			return nil, nil, err
		}
		old, err = IssueSyncConnectionFromConfig(previous.Config)
		if err != nil {
			return nil, nil, err
		}
		config, err = PreserveIssueStatusScanConfig(previous.Config, p.Config)
		if err != nil {
			return nil, nil, err
		}
	}
	if supplied != nil && (p.Connection == nil && (old == nil || *supplied != *old)) {
		return nil, nil, invalidIssueSyncConnection()
	}
	if p.Connection != nil {
		if p.Connection.Validate() != nil || (supplied != nil && *supplied != *p.Connection) {
			return nil, nil, invalidIssueSyncConnection()
		}
		if previous != nil && (old == nil || *old != *p.Connection) {
			return nil, nil, ErrIssueSyncBindingChanged
		}
		config, err = SetIssueSyncConnectionConfig(config, *p.Connection)
		if err != nil {
			return nil, nil, err
		}
	}
	if previous == nil {
		config, err = PublicIssueSyncConfig(config)
		if err != nil {
			return nil, nil, err
		}
	}
	selection, err := IssueSyncConnectionFromConfig(config)
	if err != nil {
		return nil, nil, err
	}
	if selection == nil {
		return config, nil, nil
	}
	marker := selection.ConnectionUID
	return config, &marker, nil
}
