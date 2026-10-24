package db

import (
	"errors"
	"go.kenn.io/kata/internal/uid"
	"time"
)

type ScopedControlAction struct {
	TTLSeconds int64  `json:"ttl_seconds"`
	ClaimKind  string `json:"claim_kind"`
	Reason     string `json:"reason"`
	Force      bool   `json:"force"`
	IfUnowned  bool   `json:"if_unowned"`
	LeaseUID   string `json:"lease_uid"`
}

type ScopedControlRequest struct {
	RequestUID string                 `json:"request_uid"`
	Digest     string                 `json:"digest"`
	Kind       string                 `json:"kind"`
	Subject    ScopedEndpoint         `json:"subject"`
	Principal  ScopedControlPrincipal `json:"principal"`
	Action     ScopedControlAction    `json:"action"`
}

type ScopedControlResult struct {
	RequestUID string                 `json:"request_uid"`
	Digest     string                 `json:"digest"`
	State      string                 `json:"state"`
	Lease      *ScopedLeaseState      `json:"lease"`
	Assignment *ScopedAssignmentState `json:"assignment"`
	ReasonCode string                 `json:"reason_code"`
}

type ScopedLeaseState struct {
	UID               string     `json:"uid"`
	Kind              string     `json:"kind"`
	Holder            string     `json:"holder"`
	HolderInstanceUID string     `json:"holder_instance_uid"`
	ExpiresAt         *time.Time `json:"expires_at"`
	Active            bool       `json:"active"`
}

type ScopedAssignmentState struct {
	Owner     *string    `json:"owner"`
	ExpiresAt *time.Time `json:"expires_at"`
	Changed   bool       `json:"changed"`
}

// ScopedControlPrincipal is an asserted holder tuple, never authentication.
// Admission reconstructs db.ClaimPrincipal from live authenticated context;
// AuthenticatedHost is request-only provenance and is absent from this DTO.
type ScopedControlPrincipal struct {
	HolderInstanceUID string `json:"holder_instance_uid"`
	Holder            string `json:"holder"`
	ClientKind        string `json:"client_kind"`
}

func (c ScopedControlRequest) Validate() error {
	if !uid.Valid(c.RequestUID) || !scopedDigest(c.Digest) {
		return errors.New("invalid control request identity")
	}
	if err := c.Subject.Validate(); err != nil {
		return err
	}
	if !uid.Valid(c.Principal.HolderInstanceUID) || ValidateTokenActor(c.Principal.Holder) != nil || c.Principal.ClientKind == "" {
		return errors.New("invalid control holder tuple")
	}
	switch c.Kind {
	case "claim", "acquire", "renew", "release", "status":
	default:
		return errors.New("unknown scoped control kind")
	}
	if c.Action.TTLSeconds < 0 || (c.Action.LeaseUID != "" && !uid.Valid(c.Action.LeaseUID)) {
		return errors.New("invalid control action")
	}
	return nil
}
func (c ScopedControlResult) Validate() error {
	if !uid.Valid(c.RequestUID) || !scopedDigest(c.Digest) {
		return errors.New("invalid control result identity")
	}
	switch c.State {
	case "pending", "accepted", "rejected":
	default:
		return errors.New("unknown scoped control result state")
	}
	if c.Lease != nil && c.Assignment != nil {
		return errors.New("ambiguous control result")
	}
	if c.State != "accepted" && (c.Lease != nil || c.Assignment != nil) {
		return errors.New("nonaccepted control result carries authority")
	}
	return nil
}
