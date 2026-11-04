package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestCronSummaryRelayError(t *testing.T) {
	err := relayTransportError(db.ErrUnsupportedEventFeatures)
	apiErr, ok := err.(*api.APIError)
	require.True(t, ok)
	require.Equal(t, 409, apiErr.Status)
	require.Equal(t, "unsupported_event_features", apiErr.Code)
}
