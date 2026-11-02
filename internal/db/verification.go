package db

// Verification is derived from live, authorized comment edges, never persisted
// as issue state. A confirmed claim can still have an unresolved dispute.
type Verification struct {
	Required        bool             `json:"required"`
	Confirmed       []ConfirmedClaim `json:"confirmed"`
	OpenRefutations []OpenRefutation `json:"open_refutations"`
	Redacted        int              `json:"redacted"`
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
