package federationsigning

import "go.kenn.io/kata/internal/federationsigning/signingpolicy"

// NewVerifierWithElapsedStartupForTest starts a test verifier after its startup quarantine.
// Signature timestamps, replay state, and the startup barrier use the real clock.
func NewVerifierWithElapsedStartupForTest(base string, keys []signingpolicy.Key, statePath string) (*Verifier, error) {
	verifier, err := NewVerifier(base, keys, statePath)
	if err != nil {
		return nil, err
	}
	verifier.started = verifier.started.Add(-Quarantine)
	return verifier, nil
}
