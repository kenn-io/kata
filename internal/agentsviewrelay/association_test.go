package agentsviewrelay

import (
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
		func(v *Association) { v.Route = append(v.Route, v.Route[0]) },
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
	for _, raw := range []string{`{"v":1,"v":1}`, `{"unknown":true}`, strings.Repeat("x", MaxControlBytes+1)} {
		if _, err := DecodeControl([]byte(raw)); err == nil {
			t.Fatal("accepted invalid control")
		}
	}
	if _, err := ParseControlHeader([]string{"one", "two"}); err == nil {
		t.Fatal("accepted repeated header")
	}
}
