package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// The recovery hint is a command an operator pastes into a shell, so a
// project name with spaces or quotes must survive as one argument.
func TestFederationRelayLeaveHintQuotesProjectName(t *testing.T) {
	store := openAuthTestDB(t)
	project, err := store.CreateProject(t.Context(), "spoke project 'two'")
	require.NoError(t, err)
	binding := db.FederationBinding{ProjectID: project.ID, RelayConfig: &db.RelayBindingConfig{}}
	err = federationRelayLeaveAPIError(t.Context(), store, binding, db.ErrFederationResetBlockedByPendingPush)
	var apiErr *api.APIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, "federation_lifecycle_blocked", apiErr.Code)
	require.Contains(t, apiErr.Hint, `kata federation bridge disconnect --project 'spoke project '"'"'two'"'"'' --local-only`)
}
