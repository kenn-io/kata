package tracker

import (
	"fmt"
	"hegel.dev/go/hegel"
	"testing"
)

func TestRegistryNeverReusesRetiredGenerationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(2, 30))
		policy := registryFixture()
		policy.Selection.Generation = "generation-0"
		record, err := NextRegistryRecord(nil, &policy, root)
		if err != nil {
			ht.Fatal(err)
		}
		for i := 1; i < count; i++ {
			next := policy
			next.Selection.Generation = fmt.Sprintf("generation-%d", i)
			record, err = NextRegistryRecord(&record, &next, root)
			if err != nil {
				ht.Fatal(err)
			}
		}
		replay := hegel.Draw(ht, hegel.Integers(0, count-2))
		policy.Selection.Generation = fmt.Sprintf("generation-%d", replay)
		if _, err := NextRegistryRecord(&record, &policy, root); err != ErrGenerationRetired {
			ht.Fatal("retired generation was reusable")
		}
	}, hegel.WithTestCases(1000))
}

// Contract: capacity exhaustion refuses new authority instead of evicting any
// generation. Construct full histories directly so all 4096 slots are covered
// without an O(n squared) setup sequence.
func TestRegistryHistoryCapacityPreservesEveryGenerationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(0, 4096))
		policy := registryFixture()
		record := RegistryRecord{Version: 1, Policy: policy, ProcessEpoch: root, State: RegistryActive}
		for i := 0; i < count; i++ {
			record.Retired = append(record.Retired, RetiredGeneration{Generation: fmt.Sprintf("retired-%d", i), Digest: policy.Digest})
		}
		fresh := policy
		fresh.Selection.Generation = "fresh-generation"
		next, err := NextRegistryRecord(&record, &fresh, owner)
		if count == 4096 {
			if err != ErrHistoryFull {
				ht.Fatalf("full history admitted new authority: %v", err)
			}
			return
		}
		if err != nil {
			ht.Fatal(err)
		}
		if len(next.Retired) != count+1 {
			ht.Fatal("history was evicted")
		}
		for i, old := range record.Retired {
			if next.Retired[i] != old {
				ht.Fatal("retired evidence changed")
			}
		}
		if next.Retired[count] != (RetiredGeneration{Generation: policy.Selection.Generation, Digest: policy.Digest}) {
			ht.Fatal("previous active generation was not retained")
		}
	}, hegel.WithTestCases(100))
}
