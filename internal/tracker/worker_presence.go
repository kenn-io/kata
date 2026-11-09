package tracker

import (
	"go.kenn.io/kata/internal/uid"
	"time"
)

// WorkerPresence is credential-free reachability, never provider permission.
// The immediate peer authenticates Path's last hop; upgraded mutually trusted
// relays preserve the originating owner. Exact root tickets plus independent
// owner mappings/grants still authorize every method.
type WorkerPresence struct {
	Version          int                `json:"version"`
	RootInstanceUID  string             `json:"root_instance_uid"`
	ProjectUID       string             `json:"project_uid"`
	OwnerInstanceUID string             `json:"owner_instance_uid"`
	OwnerEpoch       string             `json:"owner_epoch"`
	ExpiresAt        int64              `json:"expires_at"`
	Path             []RelayHop         `json:"path"`
	Connections      []PublicDescriptor `json:"connections"`
}

func (p WorkerPresence) Validate(local, peer, binding string, epoch int64, canRelay bool, now time.Time) error {
	if p.Version != WorkerProtocolVersion || !uid.Valid(local) || !uid.Valid(p.RootInstanceUID) || !uid.Valid(p.ProjectUID) || !uid.Valid(p.OwnerInstanceUID) || !uid.Valid(p.OwnerEpoch) || !uid.Valid(binding) || epoch <= 0 || len(p.Path) == 0 || len(p.Path) > MaxControlHops || len(p.Connections) > 128 || p.Path[0].InstanceUID != p.OwnerInstanceUID {
		return ErrInvalid
	}
	if p.ExpiresAt <= now.Unix() || p.ExpiresAt > now.Add(2*time.Minute).Unix() {
		return ErrUnavailable
	}
	if len(p.Path) > 1 && !canRelay {
		return ErrDenied
	}
	seen := map[string]bool{local: true}
	for _, hop := range p.Path {
		if !uid.Valid(hop.InstanceUID) || !uid.Valid(hop.BindingUID) || hop.ResetEpoch <= 0 || hop.InstanceUID == p.RootInstanceUID || seen[hop.InstanceUID] {
			return ErrInvalid
		}
		seen[hop.InstanceUID] = true
	}
	last := p.Path[len(p.Path)-1]
	if last.InstanceUID != peer || last.BindingUID != binding || last.ResetEpoch != epoch {
		return ErrDenied
	}
	connections := map[string]bool{}
	for _, d := range p.Connections {
		if d.Selection.Validate() != nil || d.Selection.OwnerInstanceUID != p.OwnerInstanceUID || !SupportedProvider(d.Provider) || d.Authority.Validate() != nil || !ValidIdentityText(d.Label, 128) || len(d.Actions) != 0 || connections[d.Selection.ConnectionUID] {
			return ErrInvalid
		}
		connections[d.Selection.ConnectionUID] = true
	}
	return nil
}
