package db

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestScopedMembershipVerification(t *testing.T) {
	v, pins, keys := verificationView(t)
	principal := CustodyPrincipal{AuthorityUID: v.AuthorityUID, BindingUID: verificationID(7), Epoch: 1, AccountableActor: "member", RouteDigest: v.RouteDigest}
	proof := ScopedMembershipProof{Endpoint: ScopedEndpoint{ProjectUID: pins[0].ProjectUID, IssueUID: verificationID(40)}, Kind: "root_membership"}
	raw, err := ScopedMembershipSigningBytes(proof, principal)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"domain":"kata.scoped.membership.v1"`)
	require.Contains(t, string(raw), `"signatures":[]`)
	require.Equal(t, `{"domain":"kata.scoped.membership.v1","record":{"principal":{"accountable_actor":"member","authority_uid":"00000000000000000000000009","binding_uid":"00000000000000000000000007","epoch":1,"route_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"proof":{"creating_operation_uid":"","endpoint":{"issue_uid":"00000000000000000000000040","project_uid":"00000000000000000000000001"},"kind":"root_membership","signatures":[]}}}`, string(raw))
	proof.Signatures = verificationSign(raw, pins[:1], keys)
	require.NoError(t, VerifyScopedMembership(proof, principal, pins))
	require.Error(t, VerifyScopedMembership(proof, principal, nil))
	for _, mutate := range []func(*CustodyPrincipal){
		func(p *CustodyPrincipal) { p.BindingUID = verificationID(6) }, func(p *CustodyPrincipal) { p.Epoch++ },
		func(p *CustodyPrincipal) { p.AccountableActor = "another-member" }, func(p *CustodyPrincipal) { p.RouteDigest = strings.Repeat("c", 64) }, func(p *CustodyPrincipal) { p.AuthorityUID = verificationID(5) },
	} {
		changed := principal
		mutate(&changed)
		require.Error(t, VerifyScopedMembership(proof, changed, pins))
	}
	proof.Kind = "component_creation"
	proof.CreatingOperationUID = verificationID(50)
	raw, err = ScopedMembershipSigningBytes(proof, principal)
	require.NoError(t, err)
	proof.Signatures = verificationSign(raw, pins[:1], keys)
	require.Error(t, VerifyScopedMembership(proof, principal, pins), "staged creation is graph evidence, never a root membership signature")
}
