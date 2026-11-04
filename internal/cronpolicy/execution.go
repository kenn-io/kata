// Package cronpolicy contains replica execution policy helpers shared by
// configuration and cron code.
package cronpolicy

import "strings"

// ExecuteMode controls which replica definitions a local adapter may execute.
type ExecuteMode string

const (
	// ModeNone excludes all replica jobs and workflows.
	ModeNone ExecuteMode = "none"
	// ModeOwned allows definitions authored by the upstream member actor.
	ModeOwned ExecuteMode = "owned"
	// ModeAll allows all replica jobs and workflows.
	ModeAll ExecuteMode = "all"
)

// ParseExecuteMode recognizes the supported local replica execution modes.
func ParseExecuteMode(value string) (ExecuteMode, bool) {
	mode := ExecuteMode(value)
	switch mode {
	case ModeNone, ModeOwned, ModeAll:
		return mode, true
	default:
		return "", false
	}
}

// ExecutionEligible compares the definition author with the upstream member
// identity. Unknown modes deny; owned compares exact, nonblank actor labels.
func ExecutionEligible(mode ExecuteMode, memberActor, author string) bool {
	switch mode {
	case ModeAll:
		return true
	case ModeOwned:
		return strings.TrimSpace(memberActor) != "" && memberActor == author
	default:
		return false
	}
}
