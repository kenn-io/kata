// Package agentsviewrelay defines the immutable close-to-snapshot contract.
// It has no archive, credential, transport or database implementation.
package agentsviewrelay

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/transcript"
	"go.kenn.io/kata/internal/uid"
)

// Relay bounds and identifiers apply to the version 1 protocol.
const (
	MaxSnapshotBytes int64 = 64 << 20
	MaxControlBytes        = 4096
	MaxProofBytes          = 16 << 10
	ControlHeader          = "X-Kata-Transcript-Control"
	EventFeature           = "agentsview_association_v1"
)

// RouteObservation identifies one pinned hop in a selected route.
type RouteObservation struct {
	InstanceUID         string `json:"instance_uid"`
	ProjectUID          string `json:"project_uid"`
	BindingUID          string `json:"binding_uid,omitempty"`
	UpstreamInstanceUID string `json:"upstream_instance_uid,omitempty"`
	ResetEpoch          int64  `json:"reset_epoch,omitzero"`
}

// Association is portable event data. Local paths, credentials and ephemeral
// discovery proof are deliberately absent.
type Association struct {
	Version         int                `json:"v"`
	IssueUID        string             `json:"issue_uid"`
	ProjectUID      string             `json:"project_uid"`
	AuthorityUID    string             `json:"authority_uid"`
	ServiceID       string             `json:"service_id"`
	RootKeyID       string             `json:"root_key_id"`
	Agent           string             `json:"agent"`
	NativeSessionID string             `json:"native_session_id"`
	SessionID       string             `json:"session_id"`
	SnapshotSHA256  string             `json:"snapshot_sha256"`
	SnapshotBytes   int64              `json:"snapshot_bytes"`
	LocatorBase     string             `json:"locator_base"`
	Route           []RouteObservation `json:"route"`
}

// Clone copies the association and its route.
func (a Association) Clone() Association { a.Route = slices.Clone(a.Route); return a }

// ValidDigest reports whether value is a canonical SHA-256 hex digest.
func ValidDigest(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && value == strings.ToLower(value)
}

// Validate checks the association identity, destination, and route.
func (a Association) Validate() error {
	if a.Version != 1 || !uid.Valid(a.IssueUID) || !uid.Valid(a.ProjectUID) || !uid.Valid(a.AuthorityUID) || !ValidDigest(a.ServiceID) || !ValidDigest(a.RootKeyID) || !ValidDigest(a.SnapshotSHA256) || a.SnapshotBytes <= 0 || a.SnapshotBytes > MaxSnapshotBytes {
		return errors.New("invalid AgentsView association identity, digest or size")
	}
	if err := (transcript.Transcript{Agent: a.Agent, SessionID: a.NativeSessionID}).Validate(); err != nil {
		return err
	}
	canonical := a.NativeSessionID
	if a.Agent != "claude" {
		canonical = a.Agent + ":" + canonical
	}
	if a.SessionID != canonical {
		return errors.New("AgentsView canonical session differs from native identity")
	}
	u, err := url.Parse(a.LocatorBase)
	if err != nil || len(a.LocatorBase) > 2048 || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("invalid AgentsView locator base")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return errors.New("AgentsView locator requires HTTPS")
		}
	}
	if len(a.Route) < 1 || len(a.Route) > 9 || a.Route[0].InstanceUID != a.AuthorityUID {
		return errors.New("invalid AgentsView route")
	}
	seen := make(map[string]bool, len(a.Route))
	for i, hop := range a.Route {
		if !uid.Valid(hop.InstanceUID) || hop.ProjectUID != a.ProjectUID || seen[hop.InstanceUID] {
			return errors.New("invalid AgentsView route identity")
		}
		seen[hop.InstanceUID] = true
		if i == 0 {
			if hop.BindingUID != "" || hop.UpstreamInstanceUID != "" || hop.ResetEpoch != 0 {
				return errors.New("invalid root observation")
			}
		} else if !uid.Valid(hop.BindingUID) || hop.UpstreamInstanceUID != a.Route[i-1].InstanceUID || hop.ResetEpoch <= 0 {
			return errors.New("invalid AgentsView route generation")
		}
	}
	return nil
}

// Locator derives the receiver URL for the associated session.
func (a Association) Locator() (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	return (transcript.Transcript{Agent: a.Agent, SessionID: a.NativeSessionID}).Link(a.LocatorBase)
}

// Equal compares all stable association fields.
func (a Association) Equal(b Association) bool {
	return a.Version == b.Version && a.IssueUID == b.IssueUID && a.ProjectUID == b.ProjectUID && a.AuthorityUID == b.AuthorityUID && a.ServiceID == b.ServiceID && a.RootKeyID == b.RootKeyID && a.Agent == b.Agent && a.NativeSessionID == b.NativeSessionID && a.SessionID == b.SessionID && a.SnapshotSHA256 == b.SnapshotSHA256 && a.SnapshotBytes == b.SnapshotBytes && a.LocatorBase == b.LocatorBase && slices.Equal(a.Route, b.Route)
}

// Control binds an upload to its selected close and snapshot.
type Control struct {
	Version         int                `json:"v"`
	ProjectUID      string             `json:"project_uid"`
	AuthorityUID    string             `json:"authority_uid"`
	IssueUID        string             `json:"issue_uid"`
	CloseEventUID   string             `json:"close_event_uid"`
	ServiceID       string             `json:"service_id"`
	Agent           string             `json:"agent"`
	NativeSessionID string             `json:"native_session_id"`
	SessionID       string             `json:"session_id"`
	SnapshotSHA256  string             `json:"snapshot_sha256"`
	SnapshotBytes   int64              `json:"snapshot_bytes"`
	Route           []RouteObservation `json:"route"`
	ClientKind      string             `json:"client_kind"`
}

// Association reconstructs the selection using its separately retained locator.
func (c Control) Association(locatorBase string) Association {
	return Association{Version: c.Version, IssueUID: c.IssueUID, ProjectUID: c.ProjectUID, AuthorityUID: c.AuthorityUID, ServiceID: c.ServiceID, Agent: c.Agent, NativeSessionID: c.NativeSessionID, SessionID: c.SessionID, SnapshotSHA256: c.SnapshotSHA256, SnapshotBytes: c.SnapshotBytes, LocatorBase: locatorBase, Route: slices.Clone(c.Route)}
}

// DecodeControl validates a bounded control document.
func DecodeControl(raw []byte) (Control, error) {
	var c Control
	if len(raw) > MaxControlBytes {
		return c, errors.New("AgentsView control too large")
	}
	if err := json.Unmarshal(raw, &c, json.RejectUnknownMembers(true)); err != nil {
		return Control{}, err
	}
	if !uid.Valid(c.CloseEventUID) || !validForwardedClientKind(c.ClientKind) {
		return Control{}, errors.New("invalid AgentsView close event or client kind")
	}
	a := c.Association("https://locator.example")
	a.RootKeyID = strings.Repeat("0", 64)
	if err := a.Validate(); err != nil {
		return Control{}, err
	}
	return c, nil
}

// validForwardedClientKind only recognizes kinds emitted by claim remapping.
// Ordinary requests are separately restricted to cli/tui at the handler.
func validForwardedClientKind(kind string) bool {
	if kind == "cli" || kind == "tui" || kind == "host:v1" {
		return true
	}
	for _, prefix := range []string{"relay:v1:", "spoke-host:v1:"} {
		if digest, ok := strings.CutPrefix(kind, prefix); ok {
			raw, err := base64.RawURLEncoding.Strict().DecodeString(digest)
			return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == digest
		}
	}
	return false
}

// ParseControlHeader decodes exactly one canonical control header.
func ParseControlHeader(values []string) (Control, error) {
	if len(values) != 1 || len(values[0]) > base64.RawURLEncoding.EncodedLen(MaxControlBytes) {
		return Control{}, errors.New("one bounded AgentsView control header required")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(values[0])
	if err != nil {
		return Control{}, err
	}
	if base64.RawURLEncoding.EncodeToString(raw) != values[0] {
		return Control{}, errors.New("noncanonical AgentsView control")
	}
	return DecodeControl(raw)
}
