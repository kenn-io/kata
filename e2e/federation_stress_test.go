//go:build federation_stress && !windows

package e2e_test

import (
	"testing"

	"pgregory.net/rapid"
)

func TestFederationStressHarnessBootsHubAndThreeSpokes(t *testing.T) {
	fx := newFederationStressFixture(t, 3)
	fx.enableProject(t, "stress")
	fx.waitForAllSpokes(t)
	fx.assertAllFoldedProjectionsMatch(t)
}

func TestFederationStressRandomizedWorkload(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		fx := newFederationStressFixture(rt, 3)
		fx.enableProject(rt, "stress")
		steps := rapid.IntRange(6, 12).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			fx.applyRandomOperation(rt)
			fx.waitForConvergence(rt)
			fx.assertNoDuplicateLiveClaims(rt)
		}
		fx.waitForConvergence(rt)
		fx.assertNoDuplicateLiveClaims(rt)
	})
}
