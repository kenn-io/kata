package tracker

import (
	"go.kenn.io/kata/internal/uid"
	"time"
)

const ControlProtocolVersion = 1
const MaxControlHops = 8

// RelayHop is negotiated transport identity, never a provider-use grant.
type RelayHop struct {
	InstanceUID string `json:"instance_uid"`
	BindingUID  string `json:"binding_uid"`
	ResetEpoch  int64  `json:"reset_epoch"`
}

// RelayAssertion is asserted only by a live upgraded immediate peer. Within
// Kata's mutual-hub trust model, relays preserve the original authenticated
// subject. Root mapping AND exact connection grants remain mandatory; neither
// enrollment nor a matching actor label supplies either permission.
type RelayAssertion struct {
	Version             int        `json:"version"`
	RootInstanceUID     string     `json:"root_instance_uid"`
	ProjectUID          string     `json:"project_uid"`
	Source              Principal  `json:"source"`
	IssuerInstanceUID   string     `json:"issuer_instance_uid"`
	AudienceInstanceUID string     `json:"audience_instance_uid"`
	OperationUID        string     `json:"operation_uid"`
	RequestDigest       string     `json:"request_digest"`
	ExpiresAt           int64      `json:"expires_at"`
	Path                []RelayHop `json:"path"`
}

func (a RelayAssertion) Validate(local, peer, binding string, epoch int64, canRelay bool, now time.Time) error {
	if a.Version != ControlProtocolVersion || !uid.Valid(local) || !uid.Valid(a.RootInstanceUID) || !uid.Valid(a.ProjectUID) || !uid.Valid(a.OperationUID) || !DigestValid(a.RequestDigest) || a.Source.Validate() != nil || a.AudienceInstanceUID != local || a.IssuerInstanceUID != peer || len(a.Path) == 0 || len(a.Path) > MaxControlHops || !uid.Valid(binding) || epoch <= 0 {
		return ErrInvalid
	}
	if a.ExpiresAt <= now.Unix() || a.ExpiresAt > now.Add(32*time.Second).Unix() {
		return ErrUnavailable
	}
	switch a.Source.Kind {
	case SubjectAccount:
	case SubjectOwnerLocal, SubjectStaticOwner:
		if a.Source.Subject != a.Source.AuthorityInstanceUID {
			return ErrInvalid
		}
	default:
		return ErrDenied
	}
	if a.Source.AuthorityInstanceUID != a.Path[0].InstanceUID || (len(a.Path) > 1 && !canRelay) {
		return ErrDenied
	}
	seen := map[string]bool{local: true}
	for _, hop := range a.Path {
		if !uid.Valid(hop.InstanceUID) || !uid.Valid(hop.BindingUID) || hop.ResetEpoch <= 0 || seen[hop.InstanceUID] || hop.InstanceUID == a.RootInstanceUID {
			return ErrInvalid
		}
		seen[hop.InstanceUID] = true
	}
	last := a.Path[len(a.Path)-1]
	if last.InstanceUID != peer || last.BindingUID != binding || last.ResetEpoch != epoch {
		return ErrDenied
	}
	return nil
}
