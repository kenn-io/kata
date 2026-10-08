package db

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"

	"go.kenn.io/kata/internal/embedding"
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
	rootKeys := make(map[string]map[string]RootKeyPin)
	currentArtifactManifests := make(map[string]map[string]embedding.ArtifactManifest)
	preparedCheckpoints := make(map[string]RelayResetCheckpoint)
	type retainedHop struct {
		project, peer string
		epoch         int64
	}
	hops := make(map[string]retainedHop)
	for _, record := range records {
		switch r := record.(type) {
		case *MetaKV:
			if r.Key == "instance_uid" {
				instanceUID = r.Value
			}
			if strings.HasPrefix(r.Key, RelayResetMetadataPrefix) {
				suffix := strings.TrimPrefix(r.Key, RelayResetMetadataPrefix)
				if strings.Contains(suffix, ".") {
					var checkpoint RelayResetCheckpoint
					if err := json.Unmarshal([]byte(r.Value), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
						return errors.New("backup relay checkpoint is invalid")
					}
					binding := checkpoint.Translation.Authority.BindingUID
					key := RelayResetMetadataPrefix + checkpoint.Manifest.ProjectUID + "." + binding
					if binding == "" || r.Key != key {
						return errors.New("backup relay checkpoint metadata changes its project or binding")
					}
					if _, exists := preparedCheckpoints[binding]; exists {
						return errors.New("duplicate prepared relay checkpoint in backup")
					}
					preparedCheckpoints[binding] = checkpoint
				}
			}
		case *ProjectExport:
			projects[r.ID] = r.UID
		case *RootKeyPin:
			if rootKeys[r.ProjectUID] == nil {
				rootKeys[r.ProjectUID] = make(map[string]RootKeyPin)
			}
			rootKeys[r.ProjectUID][r.KeyID] = *r
			if !r.Retired {
				pins[r.ProjectUID] = r.AuthorityUID
			}
		case *EmbeddingArtifactExport:
			artifact := r.EmbeddingArtifact
			manifest := artifact.Manifest()
			if currentArtifactManifests[artifact.ProjectUID] == nil {
				currentArtifactManifests[artifact.ProjectUID] = make(map[string]embedding.ArtifactManifest)
			}
			if prior, exists := currentArtifactManifests[artifact.ProjectUID][artifact.Digest]; exists && !reflect.DeepEqual(prior, manifest) {
				return errors.New("backup artifact digest changes its retained manifest")
			}
			currentArtifactManifests[artifact.ProjectUID][artifact.Digest] = manifest
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
				hops[c.BindingUID] = retainedHop{project: project, peer: c.UpstreamInstanceUID, epoch: c.ResetEpoch}
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
	type preparedReset struct {
		epoch             int64
		artifactManifests map[string]embedding.ArtifactManifest
	}
	prepared := make(map[string]preparedReset)
	for binding, checkpoint := range preparedCheckpoints {
		hop, ok := hops[binding]
		if !ok {
			continue // Detached historical namespaces do not retain active authority.
		}
		if checkpoint.Manifest.ProjectUID != hop.project {
			return errors.New("backup prepared relay checkpoint differs from its retained hop")
		}
		epoch := checkpoint.Translation.Authority.Epoch
		if epoch <= hop.epoch {
			continue // A checkpoint already activated by the peer is retained for retry.
		}
		if epoch != hop.epoch+1 {
			return errors.New("backup prepared relay checkpoint skips the retained hop epoch")
		}
		pin, ok := rootKeys[hop.project][checkpoint.Manifest.KeyID]
		if !ok || pin.AuthorityUID != pins[hop.project] {
			return errors.New("backup prepared relay checkpoint has no matching root pin")
		}
		authority := RelayHopAuthority{
			BindingUID: binding, ProjectUID: hop.project, AuthorityUID: pins[hop.project],
			SenderInstanceUID: instanceUID, ReceiverInstanceUID: hop.peer, Epoch: epoch,
		}
		if err := ValidateRelayResetTranslation(authority, checkpoint.Manifest, checkpoint.Translation); err != nil {
			return errors.New("backup prepared relay checkpoint differs from its retained hop")
		}
		if err := VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot); err != nil {
			return errors.New("backup prepared relay checkpoint signature is invalid")
		}
		if _, _, err := DecodeRootResetPayload(pin, checkpoint.Snapshot); err != nil {
			return errors.New("backup prepared relay checkpoint payload is invalid")
		}
		var manifests []embedding.ArtifactManifest
		if err := json.Unmarshal(checkpoint.Snapshot.Artifacts, &manifests, json.RejectUnknownMembers(true)); err != nil || manifests == nil {
			return errors.New("backup prepared relay checkpoint artifact manifest is invalid")
		}
		state := preparedReset{epoch: epoch, artifactManifests: make(map[string]embedding.ArtifactManifest, len(manifests))}
		for _, manifest := range manifests {
			if embedding.ValidateArtifactManifest(manifest) != nil || manifest.ProjectUID != hop.project {
				return errors.New("backup prepared relay checkpoint artifact manifest is invalid")
			}
			if prior, exists := state.artifactManifests[manifest.Digest]; exists && !reflect.DeepEqual(prior, manifest) {
				return errors.New("backup prepared relay checkpoint changes an artifact manifest")
			}
			state.artifactManifests[manifest.Digest] = manifest
		}
		prepared[binding] = state
	}
	for _, record := range records {
		var project, binding, stream, raw string
		var epoch int64
		var outgoing, cursor bool
		switch r := record.(type) {
		case *RelayOutboxExport:
			project, binding, stream, raw, epoch, outgoing = r.ProjectUID, r.BindingUID, r.Stream, r.Envelope, r.ResetEpoch, true
		case *RelayInboxExport:
			project, binding, stream, raw, epoch = r.ProjectUID, r.BindingUID, r.Stream, r.Envelope, r.ResetEpoch
		case *RelayCursorExport:
			project, binding, stream, epoch, cursor = r.ProjectUID, r.BindingUID, r.Stream, r.ResetEpoch, true
		default:
			continue
		}
		// Detached historical namespaces remain in full backups after leave or
		// reconnect. Validate delivery identity only when the binding is still
		// retained as an active hop.
		hop, ok := hops[binding]
		if !ok {
			continue
		}
		if hop.project != project {
			return errors.New("backup relay delivery has no matching retained hop")
		}
		preparedReset, hasPreparedReset := prepared[binding]
		if epoch > hop.epoch {
			if !hasPreparedReset || epoch != preparedReset.epoch || (!outgoing && !cursor) {
				return errors.New("backup relay delivery has no matching retained hop")
			}
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
		if epoch > hop.epoch && stream == RelayStreamArtifact {
			var manifest embedding.ArtifactManifest
			if json.Unmarshal([]byte(envelope.Body), &manifest, json.RejectUnknownMembers(true)) != nil || embedding.ValidateArtifactManifest(manifest) != nil || manifest.ProjectUID != project || manifest.Digest != envelope.SourceHash || !ValidRelayArtifactSourceUID(envelope.SourceUID, manifest.Digest) {
				return errors.New("backup prepared relay artifact offer is invalid")
			}
			checkpointManifest, inCheckpoint := preparedReset.artifactManifests[manifest.Digest]
			currentManifest, currentlyRetained := currentArtifactManifests[project][manifest.Digest]
			if (!inCheckpoint || !reflect.DeepEqual(checkpointManifest, manifest)) && (!currentlyRetained || !reflect.DeepEqual(currentManifest, manifest)) {
				return errors.New("backup prepared relay artifact is absent from its retained checkpoint")
			}
		}
	}
	return nil
}
