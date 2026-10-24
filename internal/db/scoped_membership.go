package db

import "errors"

func ScopedMembershipSigningBytes(proof ScopedMembershipProof, principal CustodyPrincipal) ([]byte, error) {
	proof.Signatures = nil
	return scopedTranscript(ScopedMembershipDomain, struct {
		Principal CustodyPrincipal      `json:"principal"`
		Proof     ScopedMembershipProof `json:"proof"`
	}{principal, proof})
}

// Membership attests only the endpoint fact under the expected authenticated
// principal. Live grants, claims and component creation remain native duties.
func VerifyScopedMembership(proof ScopedMembershipProof, expected CustodyPrincipal, trusted []RootKeyPin) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := proof.Endpoint.Validate(); err != nil {
		return err
	}
	if proof.Kind != "root_membership" || proof.CreatingOperationUID != "" {
		return errors.New("scoped membership requires a root endpoint proof")
	}
	pins, err := scopedTrustedPins(trusted)
	if err != nil {
		return err
	}
	raw, err := ScopedMembershipSigningBytes(proof, expected)
	if err != nil {
		return err
	}
	return verifyScopedEndorsements(raw, proof.Signatures, []string{proof.Endpoint.ProjectUID}, expected.AuthorityUID, pins, nil)
}
