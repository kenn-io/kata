package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// R3/R4/R9: retain the exact candidate enrollment credential across a crash,
// without reporting it as active before root/hop setup finishes.
func TestRelayEnrollmentPendingCredentialRoundTrip(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	pending := config.FederationCredential{HubURL: "https://daemon.example", HubProjectID: 42, Token: "pending-enrollment-test-token", Capabilities: "claim,pull,push", Actor: "member", RelayEnrollmentPending: true}
	require.NoError(t, config.WriteFederationCredential(projectUID, pending))
	saved, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	require.Equal(t, pending, saved.Projects[projectUID])
	require.Equal(t, "enrollment_pending", pending.Metadata().Status)
	active := pending
	active.RelayEnrollmentPending = false
	require.False(t, pending.Equal(active))
	require.NoError(t, config.WriteFederationCredential(projectUID, active))
	saved, err = config.ReadFederationCredentials()
	require.NoError(t, err)
	require.Equal(t, active, saved.Projects[projectUID])
	require.Equal(t, "present", active.Metadata().Status)
}
