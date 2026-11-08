package agentsviewrelay

import (
	"encoding/base64"
	"encoding/json/v2"
	"math"
	"reflect"
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

func TestControlHeaderRoundTripPreservesExactSelection(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		a := validAssociation()
		a.SnapshotBytes = hegel.Draw(ht, hegel.Integers[int64](1, MaxSnapshotBytes))
		c := Control{Version: 1, IssueUID: a.IssueUID, ProjectUID: a.ProjectUID, AuthorityUID: a.AuthorityUID, CloseEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route, ClientKind: hegel.Draw(ht, hegel.SampledFrom([]string{"cli", "tui"}))}
		raw, err := json.Marshal(c)
		if err != nil {
			ht.Fatal(err)
		}
		got, err := ParseControlHeader([]string{base64.RawURLEncoding.EncodeToString(raw)})
		if err != nil {
			ht.Fatal(err)
		}
		if !reflect.DeepEqual(c, got) {
			ht.Fatalf("decoded selection changed: %#v", got)
		}
	}, hegel.WithTestCases(2000))
}

func validAssociation() Association {
	project, root := "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	return Association{Version: 1, IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: project, AuthorityUID: root, ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 12, LocatorBase: "https://archive.example", Route: []RouteObservation{{InstanceUID: root, ProjectUID: project}}}
}
