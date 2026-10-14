package daemon

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// R4: transient storage failures remain server errors; they must never be
// reported as a permanently invalid protocol item and quarantined by a sender.
func TestRelayTransportErrorClassification(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{
		{errors.New("database temporarily unavailable"), http.StatusInternalServerError},
		{db.ErrTransactionFinalizationFailed, http.StatusServiceUnavailable},
		{db.ErrFederationIngestValidation, http.StatusBadRequest},
		{db.ErrRemoteEventConflict, http.StatusConflict},
	} {
		err := relayTransportError(test.err)
		var apiErr *api.APIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, test.status, apiErr.Status)
		require.NotContains(t, apiErr.Message, test.err.Error(), "wire errors must not expose storage or source details")
	}
}
