package agentsviewrelay

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
)

// Product contract: byte bounds apply to the full signed int64 input domain,
// not just the expected positive inputs supplied by a well-behaved CLI.
func TestAssociationSnapshotByteBounds(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		a := validAssociation()
		a.SnapshotBytes = n
		valid := n > 0 && n <= MaxSnapshotBytes
		if (a.Validate() == nil) != valid {
			ht.Fatalf("snapshot size %d acceptance differs from 1..64MiB", n)
		}
	}, hegel.WithTestCases(2000))
}

func TestControlHeaderAcceptsTUI(t *testing.T) {
	a := validAssociation()
	c := Control{Version: 1, IssueUID: a.IssueUID, ProjectUID: a.ProjectUID, AuthorityUID: a.AuthorityUID, CloseEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route, ClientKind: "tui"}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseControlHeader([]string{base64.RawURLEncoding.EncodeToString(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientKind != "tui" {
		t.Fatalf("client kind changed: %q", got.ClientKind)
	}
}

func validAssociation() Association {
	project, root := "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	return Association{Version: 1, IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: project, AuthorityUID: root, ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 12, LocatorBase: "https://archive.example", Route: []RouteObservation{{InstanceUID: root, ProjectUID: project}}}
}

// A member spelling that resembles a path cannot override the identity in the
// actual nested object, even when the selected digest covers the forged bytes.
func TestSnapshotIdentityRejectsForgedPathMembers(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		alias := hegel.Draw(ht, hegel.SampledFrom([]string{"manifest/session/id", "manifest/native_session_id"}))
		n := hegel.Draw(ht, hegel.Integers[int](0, 1000000))
		selected, other := fmt.Sprintf("pi:selected%d", n), fmt.Sprintf("pi:other%d", n)
		native, id := other, selected
		if alias == "manifest/session/id" {
			native, id = selected, other
		}
		body := []byte(fmt.Sprintf(`{"v":1,"session_id":%q,"manifest":{"v":5,"native_session_id":%q,"session":{"id":%q,"agent":"pi"}},%q:%q,"segments":[]}`, selected, native, id, alias, selected))
		sum := sha256.Sum256(body)
		c := Control{Agent: "pi", SessionID: selected, SnapshotBytes: int64(len(body)), SnapshotSHA256: hex.EncodeToString(sum[:])}
		if err := VerifySnapshot(t.Context(), bytes.NewReader(body), c); err == nil {
			ht.Fatalf("accepted forged identity using %q", alias)
		}
	})
}

// The envelope and artifact manifest have independent version ranges.
func TestSnapshotManifestVersionRange(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		version := hegel.Draw(ht, hegel.Integers[int](0, 10))
		body := []byte(fmt.Sprintf(`{"v":1,"session_id":"pi:selected","manifest":{"v":%d,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`, version))
		sum := sha256.Sum256(body)
		c := Control{Agent: "pi", SessionID: "pi:selected", SnapshotBytes: int64(len(body)), SnapshotSHA256: hex.EncodeToString(sum[:])}
		if accepted := VerifySnapshot(t.Context(), bytes.NewReader(body), c) == nil; accepted != (version >= 2 && version <= 7) {
			ht.Fatalf("manifest version %d acceptance: %v", version, accepted)
		}
	})
}
