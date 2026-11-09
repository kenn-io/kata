package tracker

import (
	"fmt"
	"testing"
	"time"

	"hegel.dev/go/hegel"
)

// Independently generated peer sequences must be acyclic and exclude the
// receiving/root node. Order/labels cannot repair a repeated transport node.
func TestTrackerRelayPathAcyclicProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		node := func(n int) string { return fmt.Sprintf("%026d", n) }
		now := time.Unix(1900000000, 0)
		count := hegel.Draw(ht, hegel.Integers(0, MaxControlHops+3))
		hops := make([]RelayHop, count)
		want := count >= 1 && count <= 8
		seen := map[int]bool{}
		for i := range hops {
			index := hegel.Draw(ht, hegel.Integers(0, 12))
			if index == 0 || index == 1 || seen[index] {
				want = false
			}
			seen[index] = true
			hops[i] = RelayHop{InstanceUID: node(index), BindingUID: node(30), ResetEpoch: 1}
		}
		a := RelayAssertion{Version: 1, RootInstanceUID: node(1), ProjectUID: node(20), OperationUID: node(21), AudienceInstanceUID: node(0), RequestDigest: selectionDigestFixture(), ExpiresAt: now.Add(30 * time.Second).Unix(), Path: hops}
		peer := node(2)
		if len(hops) > 0 {
			a.Source = Principal{AuthorityInstanceUID: hops[0].InstanceUID, Kind: SubjectAccount, Subject: "member"}
			peer = hops[len(hops)-1].InstanceUID
		}
		a.IssuerInstanceUID = peer
		got := a.Validate(node(0), peer, node(30), 1, true, now) == nil
		if got != want {
			ht.Fatalf("path=%v got=%v want=%v", hops, got, want)
		}
	}, hegel.WithTestCases(1000))
}
