package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSameClaimGateHolderPreservesLegacyClientKindCompatibility(t *testing.T) {
	claim := IssueClaim{Holder: "worker", HolderInstanceUID: "instance", ClientKind: "old-cli"}
	principal := ClaimPrincipal{Holder: "worker", HolderInstanceUID: "instance", ClientKind: "new-cli"}

	assert.True(t, SameClaimGateHolder(claim, principal))
}

func TestSameClaimGateHolderIncludesMountedSubjectIdentity(t *testing.T) {
	claim := IssueClaim{Holder: "spoke", HolderInstanceUID: "instance", ClientKind: "spoke-host:v1:first"}
	owner := ClaimPrincipal{Holder: "spoke", HolderInstanceUID: "instance", ClientKind: "spoke-host:v1:first"}
	other := ClaimPrincipal{Holder: "spoke", HolderInstanceUID: "instance", ClientKind: "spoke-host:v1:second"}

	assert.True(t, SameClaimGateHolder(claim, owner))
	assert.False(t, SameClaimGateHolder(claim, other))
}
