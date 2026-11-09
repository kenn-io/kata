package tracker

import (
	"errors"
	"go.kenn.io/kata/internal/uid"
	"time"
)

type ControlKind string

const (
	ControlBind         ControlKind = "bind"
	ControlRebind       ControlKind = "rebind"
	ControlDisable      ControlKind = "disable"
	ControlRun          ControlKind = "run"
	ControlActivate     ControlKind = "activate"
	ControlReconcile    ControlKind = "reconcile"
	ControlAcceptRemote ControlKind = "accept_remote"
)

func (k ControlKind) Action() Action {
	switch k {
	case ControlBind, ControlRebind:
		return ActionBind
	case ControlDisable:
		return ActionDisable
	case ControlRun:
		return ActionRead
	case ControlActivate, ControlReconcile, ControlAcceptRemote:
		return ActionRecover
	}
	return ""
}

type ControlState string

const (
	ControlPending  ControlState = "pending"
	ControlComplete ControlState = "complete"
	ControlFailed   ControlState = "failed"
	ControlRestored ControlState = "restored"
)

var (
	ErrControlCapacity = errors.New("tracker pending operation capacity reached")
	ErrControlConflict = errors.New("tracker operation UID belongs to a different request")
	ErrControlPending  = errors.New("tracker operation outcome is pending; do not resubmit with another UID")
)

// ControlResult contains classified public facts only. No raw provider body,
// config/selector, transport ticket, task content or credentials are retained.
type ControlResult struct {
	ActivationRequired bool   `json:"activation_required,omitempty"`
	Code               string `json:"code"`
	Enabled            bool   `json:"enabled"`
	Created            int    `json:"created,omitempty"`
	Updated            int    `json:"updated,omitempty"`
	Unchanged          int    `json:"unchanged,omitempty"`
	Comments           int    `json:"comments,omitempty"`
	StatusUpdated      int    `json:"status_updated,omitempty"`
}

func (r ControlResult) Validate() error {
	switch r.Code {
	case "ok", "validation", "tracker_denied", "tracker_unavailable", "tracker_outcome_unknown", "tracker_activation_required", "tracker_binding_changed", "tracker_operation_unsupported", "tracker_restored_inactive", "tracker_binding_disabled", "tracker_running", "tracker_project_already_bound", "tracker_timeout", "not_found":
	default:
		return ErrInvalid
	}
	if r.Created < 0 || r.Updated < 0 || r.Unchanged < 0 || r.Comments < 0 || r.StatusUpdated < 0 {
		return ErrInvalid
	}
	return nil
}

// ControlReceipt remembers arbitrary caller UIDs independently of semantic
// status-write identity. Completed history never expires into new work.
// ControlSettlement records an operator decision about an unresolved caller
// control. It is not provider acknowledgement and cannot release write fences.
type ControlSettlement struct {
	Operator   Principal `json:"operator"`
	RecordedAt time.Time `json:"recorded_at"`
	Kind       string    `json:"kind"`
}
type ControlReceipt struct {
	ProcessEpoch    string             `json:"process_epoch,omitempty"`
	Settlement      *ControlSettlement `json:"settlement,omitempty"`
	Version         int                `json:"version"`
	RootInstanceUID string             `json:"root_instance_uid"`
	ProjectUID      string             `json:"project_uid"`
	UID             string             `json:"uid"`
	Principal       Principal          `json:"principal"`
	Selection       Selection          `json:"selection"`
	Kind            ControlKind        `json:"kind"`
	SourceKey       string             `json:"source_key"`
	RequestDigest   string             `json:"request_digest"`
	State           ControlState       `json:"state"`
	Result          *ControlResult     `json:"result,omitempty"`
}

func (c ControlReceipt) Validate() error {
	if c.ProcessEpoch != "" && !uid.Valid(c.ProcessEpoch) {
		return ErrInvalid
	}
	if s := c.Settlement; s != nil {
		if c.ProcessEpoch == "" || c.State != ControlFailed || c.Result == nil || c.Result.Code != "tracker_outcome_unknown" || s.Kind != "abandon_unresolved_control" || s.RecordedAt.IsZero() || s.Operator.Validate() != nil || s.Operator.AuthorityInstanceUID != c.RootInstanceUID || (s.Operator.Kind != SubjectOwnerLocal && s.Operator.Kind != SubjectStaticOwner) || s.Operator.Subject != c.RootInstanceUID {
			return ErrInvalid
		}
	}
	if c.Version != 1 || !uid.Valid(c.RootInstanceUID) || !uid.Valid(c.ProjectUID) || !uid.Valid(c.UID) || c.Principal.Validate() != nil || c.Selection.Validate() != nil || !validAction(c.Kind.Action()) || !ValidIdentityText(c.SourceKey, 2048) || !DigestValid(c.RequestDigest) {
		return ErrInvalid
	}
	switch c.State {
	case ControlPending:
		if c.Result != nil {
			return ErrInvalid
		}
	case ControlComplete, ControlFailed:
		if c.Result == nil || c.Result.Validate() != nil || (c.State == ControlComplete && c.Result.Code != "ok") || (c.State == ControlFailed && c.Result.Code == "ok") {
			return ErrInvalid
		}
	case ControlRestored:
		if c.Result == nil || c.Result.Code != "tracker_restored_inactive" || c.Result.Validate() != nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
func SameControlRequest(a, b ControlReceipt) bool {
	return a.Version == b.Version && a.RootInstanceUID == b.RootInstanceUID && a.ProjectUID == b.ProjectUID && a.UID == b.UID && a.Principal == b.Principal && a.Selection == b.Selection && a.Kind == b.Kind && a.SourceKey == b.SourceKey && a.RequestDigest == b.RequestDigest
}

// IsPublicErrorCode bounds error facts that a client may expose. Provider and
// transport diagnostics, including arbitrary envelope codes, are not classes.
func IsPublicErrorCode(code string) bool {
	if code != "ok" && (ControlResult{Code: code}).Validate() == nil {
		return true
	}
	switch code {
	case "tracker_principal_unsupported", "tracker_capability_unavailable", "tracker_ambiguous", "tracker_selection_required", "tracker_operation_conflict", "tracker_operation_pending", "tracker_capacity":
		return true
	}
	return false
}
