package agentsviewrelay

import (
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"testing"
)

// Product contract: association identity is immutable, canonical and scoped to
// one selected project/root/path. A display name is never an identity.
func TestAssociationValidatesCanonicalSessionAndRoute(t *testing.T) {
	a := Association{Version: 1, IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", AuthorityUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "codex", NativeSessionID: "11111111-2222-3333-4444-555555555555", SessionID: "codex:11111111-2222-3333-4444-555555555555", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 123, LocatorBase: "https://archive.example"}
	a.Route = []RouteObservation{{InstanceUID: a.AuthorityUID, ProjectUID: a.ProjectUID}}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Association){
		func(v *Association) { v.SessionID = "codex:other" },
		func(v *Association) { v.SnapshotBytes = MaxSnapshotBytes + 1 },
		func(v *Association) {
			v.Route = append(v.Route, RouteObservation{InstanceUID: v.AuthorityUID, ProjectUID: v.ProjectUID, BindingUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", UpstreamInstanceUID: v.AuthorityUID, ResetEpoch: 1})
		},
		func(v *Association) { v.LocatorBase = "https://user:secret@archive.example" },
	} {
		changed := a.Clone()
		change(&changed)
		if err := changed.Validate(); err == nil {
			t.Fatal("accepted changed invalid association")
		}
	}
}

func TestControlStrictBoundedAndSingleHeader(t *testing.T) {
	a := validAssociation()
	c := Control{Version: 1, IssueUID: a.IssueUID, ProjectUID: a.ProjectUID, AuthorityUID: a.AuthorityUID, CloseEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route, ClientKind: "cli"}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeControl(raw); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"duplicate": `{"v":1,` + string(raw[1:]),
		"unknown":   `{"unknown":true,` + string(raw[1:]),
		"oversize":  string(raw) + strings.Repeat(" ", MaxControlBytes),
	} {
		if _, err := DecodeControl([]byte(bad)); err == nil {
			t.Fatalf("accepted %s control", name)
		}
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := ParseControlHeader([]string{encoded}); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseControlHeader([]string{encoded, encoded}); err == nil {
		t.Fatal("accepted repeated valid header")
	}
}
