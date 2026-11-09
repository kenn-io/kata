package tracker

import (
	"fmt"
	"testing"
)

func registryFixture() RegistryPolicy {
	return RegistryPolicy{Selection: Selection{OwnerInstanceUID: owner, ConnectionUID: conn, Generation: "first"}, Provider: "notion", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
}

// Contract: neither reload nor remove/re-add can reuse an opaque generation for
// changed credential selectors, grants or provider authority.
func TestRegistryGenerationHistoryAndRevocation(t *testing.T) {
	policy := registryFixture()
	record, err := NextRegistryRecord(nil, &policy, root)
	if err != nil {
		t.Fatal(err)
	}
	changed := policy
	changed.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := NextRegistryRecord(&record, &changed, root); err != ErrGenerationChanged {
		t.Fatal("changed authority reused generation", err)
	}
	revoked, err := NextRegistryRecord(&record, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NextRegistryRecord(&revoked, &policy, root); err != ErrGenerationRetired {
		t.Fatal("revoked generation reused", err)
	}
	changed.Selection.Generation = "second"
	second, err := NextRegistryRecord(&revoked, &changed, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NextRegistryRecord(&second, &policy, root); err != ErrGenerationRetired {
		t.Fatal("rolled back to used generation", err)
	}
	if second.Policy.Provider != record.Policy.Provider || len(second.Retired) != 1 {
		t.Fatal("history lost")
	}
}

func TestRegistryRestoredGenerationNeverAutoActivates(t *testing.T) {
	p := registryFixture()
	r, err := NextRegistryRecord(nil, &p, root)
	if err != nil {
		t.Fatal(err)
	}
	r.State = RegistryRestored
	if _, err := NextRegistryRecord(&r, &p, root); err != ErrGenerationRetired {
		t.Fatal("restored generation autoactivated", err)
	}
	p.Selection.Generation = "fresh"
	next, err := NextRegistryRecord(&r, &p, root)
	if err != nil || next.State != RegistryActive {
		t.Fatal(err)
	}
}

func TestRegistryCapacityBoundaries(t *testing.T) {
	for _, count := range []int{4095, 4096, 4097} {
		r := RegistryRecord{Version: 1, Policy: registryFixture(), ProcessEpoch: root, State: RegistryActive}
		for i := 0; i < count; i++ {
			r.Retired = append(r.Retired, RetiredGeneration{Generation: fmt.Sprintf("old-%d", i), Digest: r.Policy.Digest})
		}
		next := r.Policy
		next.Selection.Generation = "fresh"
		_, err := NextRegistryRecord(&r, &next, root)
		switch count {
		case 4095:
			if err != nil {
				t.Fatal(err)
			}
		case 4096:
			if err != ErrHistoryFull {
				t.Fatal("capacity must fail closed", err)
			}
		case 4097:
			if err != ErrInvalid {
				t.Fatal("malformed over-capacity history", err)
			}
		}
	}
}
