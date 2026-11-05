//go:build federation_stress && !windows

package e2e_test

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"testing"
	"testing/quick"
	"time"
)

func TestFederationStressHarnessBootsHubAndThreeSpokes(t *testing.T) {
	fx := newFederationStressFixture(t, 3)
	fx.enableProject(t, "stress")
	fx.waitForAllSpokes(t)
	fx.assertAllFoldedProjectionsMatch(t)
}

type federationStressWorkload struct{ Actions []federationStressAction }

func (federationStressWorkload) Generate(r *rand.Rand, _ int) reflect.Value {
	actions := make([]federationStressAction, 6+r.Intn(7))
	for i := range actions {
		actions[i] = federationStressAction{
			Operation: uint8(r.Intn(6)), //nolint:gosec // G115: Intn bounds the operation to 0-5.
			Spoke:     r.Uint64(), Issue: r.Uint64(), Claim: r.Uint64(),
			Actor: uint8(r.Intn(4)), EditKind: uint8(r.Intn(4)), //nolint:gosec // G115: Intn bounds both choices to 0-3.
			Priority: uint8(r.Intn(4)), Label: uint8(r.Intn(6)), //nolint:gosec // G115: Intn bounds these choices below 6.
			CommentOnHub: r.Intn(2) == 0,
		}
	}
	return reflect.ValueOf(federationStressWorkload{Actions: actions})
}

var federationStressSeed = flag.Int64("federation-stress-seed", 0, "replay a federation stress workload; zero chooses a new seed")

func TestFederationStressRandomizedWorkload(t *testing.T) {
	checks := 0 // Zero keeps testing/quick's default of 100, or -quickchecks.
	if raw := os.Getenv("KATA_FEDERATION_STRESS_CHECKS"); raw != "" {
		var err error
		checks, err = strconv.Atoi(raw)
		if err != nil || checks <= 0 {
			t.Fatalf("invalid KATA_FEDERATION_STRESS_CHECKS %q: want a positive integer", raw)
		}
	}
	seed := *federationStressSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	t.Logf("replay with -federation-stress-seed=%d", seed)
	trial := 0
	if err := quick.Check(func(workload federationStressWorkload) bool {
		trial++
		return t.Run(fmt.Sprintf("trial-%d", trial), func(t *testing.T) {
			t.Logf("actions=%#v", workload.Actions)
			fx := newFederationStressFixture(t, 3)
			fx.enableProject(t, "stress")
			for _, action := range workload.Actions {
				fx.applyOperation(t, action)
				fx.waitForConvergence(t)
				fx.assertNoDuplicateLiveClaims(t)
			}
			fx.waitForConvergence(t)
			fx.assertNoDuplicateLiveClaims(t)
		})
	}, &quick.Config{
		MaxCount: checks,
		Rand:     rand.New(rand.NewSource(seed)), //nolint:gosec // G404: reproducible synthetic workload, not security randomness.
	}); err != nil {
		t.Fatal(err)
	}
}
