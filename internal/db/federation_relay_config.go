package db

import (
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

// RelayBindingConfig is negotiated state on the existing upstream binding.
// LocalActor authorizes local setup; Binding.Actor remains the upstream account.
// Source actor labels never establish either authority.
type RelayBindingConfig struct {
	ProtocolVersion     int      `json:"protocol_version"`
	BindingUID          string   `json:"binding_uid"`
	UpstreamInstanceUID string   `json:"upstream_instance_uid"`
	AuthorityUID        string   `json:"authority_uid"`
	HubPath             []string `json:"hub_path"`
	LocalActor          string   `json:"local_actor"`
	ServeDownstream     bool     `json:"serve_downstream"`
	ResetEpoch          int64    `json:"reset_epoch"`
	UpstreamRevoked     bool     `json:"upstream_revoked"`
}

// Validate checks the negotiated protocol and acyclic authority path.
func (c RelayBindingConfig) Validate(instanceUID string) error {
	if c.ProtocolVersion != RelayProtocolVersion || !uid.Valid(c.BindingUID) || !uid.Valid(c.AuthorityUID) || !uid.Valid(c.UpstreamInstanceUID) || c.UpstreamInstanceUID == instanceUID || c.AuthorityUID == instanceUID || c.ResetEpoch <= 0 || strings.TrimSpace(c.LocalActor) == "" || c.LocalActor != strings.TrimSpace(c.LocalActor) {
		return errors.New("invalid negotiated relay identity or account")
	}
	maxNodes := MaxRelayHubs
	if !c.ServeDownstream {
		maxNodes++
	} // A final leaf does not consume a hub slot.
	if len(c.HubPath) < 2 || len(c.HubPath) > maxNodes || c.HubPath[0] != c.AuthorityUID || c.HubPath[len(c.HubPath)-1] != instanceUID || c.HubPath[len(c.HubPath)-2] != c.UpstreamInstanceUID {
		return errors.New("invalid relay authority path")
	}
	seen := make(map[string]bool, len(c.HubPath))
	for _, node := range c.HubPath {
		if !uid.Valid(node) || seen[node] {
			return errors.New("invalid or cyclic relay authority path")
		}
		seen[node] = true
	}
	return nil
}

// DecodeRelayBindingConfig parses and validates retained binding configuration.
func DecodeRelayBindingConfig(raw *string) (*RelayBindingConfig, error) {
	if raw == nil {
		return nil, nil
	}
	var config RelayBindingConfig
	if err := json.Unmarshal([]byte(*raw), &config, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	return &config, nil
}

// CheckRelayBindingUpdate prevents a legacy control write from changing the
// negotiated peer or installing unchecked relay state. Dedicated setup/reset
// paths own configuration; normal cursor updates retain the stored value.
func CheckRelayBindingUpdate(previous *FederationBinding, next FederationBinding) error {
	if next.RelayConfig != nil {
		if previous == nil {
			return errors.New("relay configuration requires negotiated setup")
		}
		stored, err := json.Marshal(previous.RelayConfig)
		if err != nil {
			return err
		}
		provided, err := json.Marshal(next.RelayConfig)
		if err != nil {
			return err
		}
		if string(stored) != string(provided) {
			return errors.New("relay configuration requires negotiated setup")
		}
	}
	if previous != nil && previous.RelayConfig != nil && (previous.Role != next.Role || previous.HubURL != next.HubURL || previous.HubProjectID != next.HubProjectID || previous.HubProjectUID != next.HubProjectUID || previous.Actor != strings.TrimSpace(next.Actor) || previous.AllowInsecure != next.AllowInsecure) {
		return errors.New("negotiated relay upstream cannot change through a generic binding update")
	}
	return nil
}

// EncodeRelayBindingConfig validates and serializes the retained relay binding configuration.
func EncodeRelayBindingConfig(config *RelayBindingConfig) (*string, error) {
	if config == nil {
		return nil, nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	value := string(raw)
	return &value, nil
}

// Full owner restores validate negotiated topology against the retained source
// instance, project and root pin before either native backend clears its target.
func validateRelayConfigurationReplay(records []ImportRecord) error {
	var instanceUID string
	projects := make(map[int64]string)
	pins := make(map[string]string)
	configs := make(map[string]*RelayBindingConfig)
	type retainedHop struct {
		project, peer string
		epoch         int64
		upstream      bool
	}
	hops := make(map[string]retainedHop)
	for _, record := range records {
		switch r := record.(type) {
		case *MetaKV:
			if r.Key == "instance_uid" {
				instanceUID = r.Value
			}
		case *ProjectExport:
			projects[r.ID] = r.UID
		case *RootKeyPin:
			if !r.Retired {
				pins[r.ProjectUID] = r.AuthorityUID
			}
		}
	}
	for _, record := range records {
		switch r := record.(type) {
		case *FederationBindingExport:
			if c := r.RelayConfig; c != nil {
				if err := c.Validate(instanceUID); err != nil {
					return err
				}
				project := projects[r.ProjectID]
				if project == "" || project != r.HubProjectUID || r.Role != string(FederationRoleSpoke) || pins[project] != c.AuthorityUID || strings.TrimSpace(r.Actor) == "" {
					return errors.New("backup relay differs from its project, upstream or root pin")
				}
				if _, ok := hops[c.BindingUID]; ok {
					return errors.New("duplicate relay binding in backup")
				}
				hops[c.BindingUID] = retainedHop{project: project, peer: c.UpstreamInstanceUID, epoch: c.ResetEpoch, upstream: true}
				configs[project] = c
			}
		case *FederationEnrollmentExport:
			if r.RelayProtocolVersion == RelayProtocolVersion {
				if r.RelayBindingUID == nil || !uid.Valid(*r.RelayBindingUID) {
					return errors.New("backup relay enrollment lacks a binding identity")
				}
				if _, ok := hops[*r.RelayBindingUID]; ok {
					return errors.New("duplicate relay binding in backup")
				}
				if r.ProjectID == nil {
					return errors.New("backup relay enrollment lacks a project")
				}
				hops[*r.RelayBindingUID] = retainedHop{project: projects[*r.ProjectID], peer: r.SpokeInstanceUID, epoch: r.RelayResetEpoch}
			}
		}
	}
	for _, record := range records {
		var project, binding, raw string
		var epoch int64
		var outgoing bool
		switch r := record.(type) {
		case *RelayOutboxExport:
			project, binding, raw, epoch, outgoing = r.ProjectUID, r.BindingUID, r.Envelope, r.ResetEpoch, true
		case *RelayInboxExport:
			project, binding, raw, epoch = r.ProjectUID, r.BindingUID, r.Envelope, r.ResetEpoch
		case *RelayCursorExport:
			project, binding, epoch = r.ProjectUID, r.BindingUID, r.ResetEpoch
		default:
			continue
		}
		// Detached historical state is retained; active negotiated projects must
		// resolve every mapping to their exact current hop rather than source labels.
		if configs[project] == nil {
			continue
		}
		hop, ok := hops[binding]
		if !ok || hop.project != project || epoch > hop.epoch {
			return errors.New("backup relay delivery has no matching retained hop")
		}
		if raw == "" {
			continue
		}
		var envelope RelayEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return err
		}
		sender, receiver := instanceUID, hop.peer
		if !outgoing {
			sender, receiver = receiver, sender
		}
		if envelope.AuthorityUID != pins[project] || envelope.SenderInstanceUID != sender || envelope.ReceiverInstanceUID != receiver {
			return errors.New("backup relay delivery differs from retained transport authority")
		}
	}
	return nil
}
