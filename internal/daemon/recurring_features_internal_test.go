package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestRecurringHintPreservesOtherFederationErrors(t *testing.T) {
	for _, original := range []error{db.ErrFederatedReadOnly, errors.Join(db.ErrFederatedReadOnly, db.ErrFederatedSpokeUnsupported)} {
		t.Run(original.Error(), func(t *testing.T) {
			err := fmt.Errorf("write: %w", original)
			var response *api.APIError
			require.ErrorAs(t, federationReadOnlyError(err), &response)
			require.Equal(t, http.StatusConflict, response.Status)
			require.Equal(t, "federated_read_only", response.Code)
			require.Equal(t, err.Error(), response.Message)
			require.Empty(t, response.Hint)
		})
	}
}

func TestRecurringSpokeHubHintSurvivesWrapping(t *testing.T) {
	original := errors.Join(db.ErrFederatedReadOnly, db.ErrFederatedSpokeUnsupported, db.ErrRecurringHubRequired)
	err := fmt.Errorf("write: %w", original)
	var response *api.APIError
	require.ErrorAs(t, federationReadOnlyError(err), &response)
	require.Equal(t, http.StatusConflict, response.Status)
	require.Equal(t, "federated_read_only", response.Code)
	require.Equal(t, db.ErrRecurringHubRequired.Error(), response.Message)
	require.Equal(t, "Run the same command against the project's owning hub, for example with --daemon <hub-name>.", response.Hint)
}

// A peer without cron support needs an actionable upgrade hint.
func TestRecurringFederationFeatureRecoveryHint(t *testing.T) {
	err := federationFeatureError(db.RequireEventFeatures("", "cron_v1"))
	var response *api.APIError
	require.ErrorAs(t, err, &response)
	require.Equal(t, http.StatusConflict, response.Status)
	require.Equal(t, "unsupported_event_features", response.Code)
	require.Contains(t, response.Message, "cron_v1")
	require.Contains(t, response.Hint, "required event features")
}
