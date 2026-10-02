package client

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// Required JSON string properties may be present but empty: a legacy grant can
// lack a configured hub URL, and a grant without pull has no runnable command.
func TestFederationEnrollmentJoinInstructionsValidateSupportedGrants(t *testing.T) {
	for _, tc := range []struct{ name, hubURL, command, capabilities string }{
		{name: "no configured origin", capabilities: "pull"},
		{name: "grant without pull", hubURL: "https://hub.example", capabilities: "push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := generated.FederationEnrollmentOut{
				ID: 1, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", ProjectID: 42,
				Actor: "tester", Capabilities: tc.capabilities, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
				Join: &generated.FederationJoinInstructions{
					HubURL: tc.hubURL, JoinCommand: tc.command, HubProjectID: 42, HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
					ProjectName: "hub-project", ReplayHorizonEventID: 7, Token: "join-token", Actor: "tester", Capabilities: tc.capabilities, PushEnabled: tc.capabilities == "push",
				},
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			var decoded generated.FederationEnrollmentOut
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.NoError(t, decoded.Validate(), "supported enrollment response must validate: %s", raw)
			require.Equal(t, tc.hubURL, decoded.Join.HubURL)
			require.Empty(t, decoded.Join.JoinCommand)
			// Other required authority fields must retain their validation.
			decoded.Join.Token = ""
			require.Error(t, decoded.Validate(), "a missing token must still fail validation")
		})
	}
}
