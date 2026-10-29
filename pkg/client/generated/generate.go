package generated

//go:generate sh -c "go run ../../../cmd/kata openapi --version 3.0 --format yaml > ../openapi.yaml"
//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.76.5 -config config.yaml ../openapi.yaml

import (
	"fmt"
	"regexp"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
)

// ReadyGlobalIssue is the pre-0.10 name of the cross-project ready row,
// renamed when ready responses were hydrated to full IssueOut. It lives in
// this file rather than the generated ones because regeneration deletes
// every other .go file in this package.
//
// Deprecated: use ReadyGlobalIssueOut.
type ReadyGlobalIssue = ReadyGlobalIssueOut

// TranscriptAgent names the coding agent behind a closing-session transcript.
// The schema accepts any agent token, so agent_overlay.yaml keeps this
// released type and its constants instead of a generated enum.
type TranscriptAgent string

// Built-in agents, whose session IDs are UUIDs.
const (
	Claude TranscriptAgent = "claude"
	Codex  TranscriptAgent = "codex"
)

var transcriptAgentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Validate checks the API's agent token rule.
func (t TranscriptAgent) Validate() error {
	if len(t) > 32 || !transcriptAgentPattern.MatchString(string(t)) {
		return runtime.NewValidationErrorsFromString("Pattern", fmt.Sprintf("must be a lowercase alphanumeric or hyphen token of at most 32 characters, got: %v", t))
	}
	return nil
}
