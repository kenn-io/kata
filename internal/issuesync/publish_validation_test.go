package issuesync

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Service callers need the same actionable text as HTTP clients. Only the
// dedicated local type may select a catalog entry; strings and unknown values
// cannot turn provider diagnostics into a public error.
func TestPublishValidationServiceError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input error
		want  string
	}{
		{"local", NewPublicationValidationError(PublicationNotionOpenTargetRequired), string(PublicationNotionOpenTargetRequired)},
		{"wrapped", fmt.Errorf("private-context: %w", NewPublicationValidationError(PublicationNotionOpenTargetRequired)), string(PublicationNotionOpenTargetRequired)},
		{"spoofed", errors.New("publication_notion_open_target_required private-context"), "publication_preflight_failed"},
		{"unknown", NewPublicationValidationError("private-context"), "publication_preflight_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := publishFailure(publicationValidationCode(tc.input, "publication_preflight_failed"), nil, "")
			require.NotContains(t, err.Error(), "private-context")
			require.Contains(t, err.Error(), tc.want)
			if tc.want == string(PublicationNotionOpenTargetRequired) {
				require.Contains(t, err.Error(), "Notion publication requires an open status target.")
				require.Contains(t, err.Error(), "--todo-group")
			} else {
				require.EqualError(t, err, tc.want)
			}
		})
	}
}
