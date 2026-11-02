package db

// Verification is derived from live, authorized comment edges, never persisted
// as issue state. Confirmed claims have no unresolved dispute.
type Verification struct {
	Evidence        []VerdictEvidence       `json:"evidence,omitempty"`
	Claims          map[string]ClaimVerdict `json:"-"`
	Required        bool                    `json:"required"`
	Confirmed       []ConfirmedClaim        `json:"confirmed"`
	OpenRefutations []OpenRefutation        `json:"open_refutations"`
	Redacted        int                     `json:"redacted"`
	// Activity keeps self-confirmations and resolved disputes visible.
	Activity bool `json:"-"`
}

// ConfirmedClaim counts replies from principals other than the claim's author.
type ConfirmedClaim struct {
	Comment       string `json:"comment"`
	Confirmations int    `json:"confirmations"`
}

// OpenRefutation identifies a visible dispute. Summary is a bounded excerpt.
type OpenRefutation struct {
	Refutation string `json:"refutation"`
	Target     string `json:"target"`
	Author     string `json:"author"`
	Teammate   string `json:"teammate,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// VerificationWarning is retained with the close receipt and event.
type VerificationWarning struct {
	Code        string           `json:"code"`
	Refutations []OpenRefutation `json:"refutations"`
}

// ClaimVerdict retains evaluator details for validating explicit close citations.
// It is internal derivation state, not part of show output.
type ClaimVerdict struct {
	Eligible               bool
	Confirmations          []string
	OpenRefutations        []string
	IndependentRefutations []string
}

// VerdictEvidence is a cited claim retained on a closed issue's summary.
type VerdictEvidence struct {
	Type    string `json:"type"`
	Comment string `json:"comment"`
}
