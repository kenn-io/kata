// Package diagnostics defines sanitized, read-only setup findings.
package diagnostics

// Check is one finding with a stable machine-readable identity.
type Check struct {
	ID       string   `json:"id"`
	Category string   `json:"category"`
	Status   string   `json:"status"`
	Summary  string   `json:"summary"`
	Details  []string `json:"details"`
	Fix      string   `json:"fix"`
}

// Counts summarizes finding statuses.
type Counts struct {
	OK   int `json:"ok"`
	Info int `json:"info"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

// Report is the versioned doctor output contract.
type Report struct {
	Version int     `json:"version"`
	Checks  []Check `json:"checks"`
	Summary Counts  `json:"summary"`
}

// NewReport counts findings without treating warnings as failures.
func NewReport(checks []Check) Report {
	if checks == nil {
		checks = []Check{}
	}
	r := Report{Version: 1, Checks: checks}
	for i := range r.Checks {
		if r.Checks[i].Details == nil {
			r.Checks[i].Details = []string{}
		}
		switch r.Checks[i].Status {
		case "ok":
			r.Summary.OK++
		case "info":
			r.Summary.Info++
		case "warn":
			r.Summary.Warn++
		case "fail":
			r.Summary.Fail++
		}
	}
	return r
}

// Failed reports whether the report should exit unsuccessfully.
func (r Report) Failed() bool { return r.Summary.Fail > 0 }

// Run isolates a check panic without disclosing the panic's possibly secret value.
func Run(id, category string, fn func() Check) (result Check) {
	defer func() {
		if recover() != nil {
			result = Check{Status: "fail", Summary: "Diagnostic check could not complete", Fix: "Report this check ID with the CLI version."}
		}
		result.ID, result.Category = id, category
	}()
	return fn()
}
