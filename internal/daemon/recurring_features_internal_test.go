package daemon

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

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
