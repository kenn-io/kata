package daemon

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// A peer that already supports portable cron needs an actionable native-format hint.
func TestRecurringFederationFeatureRecoveryHint(t *testing.T) {
	err := federationFeatureError(db.RequireEventFeatures(db.CronEventFeature, db.SupportedEventFeatures))
	var response *api.APIError
	require.ErrorAs(t, err, &response)
	require.Equal(t, http.StatusConflict, response.Status)
	require.Equal(t, "unsupported_event_features", response.Code)
	require.Contains(t, response.Message, db.RecurringCronEventFeature)
	require.Contains(t, response.Hint, "required event features")
}
