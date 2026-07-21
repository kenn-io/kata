package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSameClaimGateHolderPreservesLegacyClientKindCompatibility(t *testing.T) {
	claim := IssueClaim{Holder: "worker", HolderInstanceUID: "instance", ClientKind: "old-cli"}
	for _, clientKind := range []string{"new-cli", "host:v1", "spoke-host:v1:caller-controlled"} {
		principal := ClaimPrincipal{Holder: "worker", HolderInstanceUID: "instance", ClientKind: clientKind}
		assert.True(t, SameClaimGateHolder(claim, principal))
	}
}

func TestSameClaimGateHolderIncludesMountedSubjectIdentity(t *testing.T) {
	claim := IssueClaim{Holder: "spoke", HolderInstanceUID: "instance", ClientKind: "spoke-host:v1:first"}
	owner := ClaimPrincipal{Holder: "spoke", HolderInstanceUID: "instance", ClientKind: "spoke-host:v1:first", AuthenticatedHost: true}
	other := ClaimPrincipal{Holder: "spoke", HolderInstanceUID: "instance", ClientKind: "spoke-host:v1:second", AuthenticatedHost: true}

	assert.True(t, SameClaimGateHolder(claim, owner))
	assert.False(t, SameClaimGateHolder(claim, other))
}
