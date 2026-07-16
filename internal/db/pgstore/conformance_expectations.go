package pgstore

// ExpectedConformanceFailures returns the exact temporary scenario failure
// manifest for this backend. Full parity currently has no expected failures;
// keeping the explicit manifest makes any future temporary gap reviewable.
func ExpectedConformanceFailures() map[string]error {
	return map[string]error{}
}
