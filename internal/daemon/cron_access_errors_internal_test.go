package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// Legacy spokes quarantine validation-class 400s; grant denial must remain
// recognizable without turning transient policy reads into permanent errors.
func TestCronAccessFederationErrorContract(t *testing.T) {
	denied := fmt.Errorf("%w: %w", db.ErrFederationIngestValidation, &db.CronPermissionError{Grant: "observe"})
	for _, classify := range []func(error) error{federationIngestError, relayTransportError} {
		var result *api.APIError
		require.ErrorAs(t, classify(denied), &result)
		require.Equal(t, 400, result.Status)
		require.Equal(t, "cron_permission_denied", result.Code)
		require.Contains(t, result.Hint, "observe grant")
		require.ErrorAs(t, classify(fmt.Errorf("read cron policy: unavailable")), &result)
		require.Equal(t, 500, result.Status)
	}
}
