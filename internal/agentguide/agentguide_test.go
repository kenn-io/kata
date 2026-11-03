package agentguide

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The owner limits injected closing guidance to this exact verdict summary;
// the workflow guide owns the longer review and dispute-resolution examples.
func TestClosingVerdictSummary(t *testing.T) {
	_, afterEvidence, ok := strings.Cut(fragClosing, "verdict types follow the reason pairings below.\n\n")
	require.True(t, ok)
	summary, _, ok := strings.Cut(afterEvidence, "\n   Close each issue")
	require.True(t, ok)
	require.Equal(t, `   --confirmed c:<claim> cites a completion claim that another teammate
   confirmed (kata comment --confirm); required on done when
   verify.required=true. --refuted c:<claim> cites an independent
   refutation on --wontfix/--duplicate-of/--superseded-by.
   Refused closes print the next command to run.
`, summary)
	require.Contains(t, fragClosing, "confirmed:c:<claim>")
	require.Contains(t, fragClosing, "refuted:c:<claim>")
	require.Contains(t, fragClosing, "--confirmed and --refuted")
}
