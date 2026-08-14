package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client/generated"
)

func TestLeaseToolsArePublished(t *testing.T) {
	session := connectTestServer(t)
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{"kata.lease", "kata.lease_force_release", "kata.lease_status", "kata.lease_steal"} {
		require.True(t, names[name], name)
	}
}

func TestLeaseReleaseResponsesReportNotHeld(t *testing.T) {
	releasedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		require.True(t,
			strings.HasSuffix(request.URL.Path, "/actions/release") ||
				strings.HasSuffix(request.URL.Path, "/actions/force_release"),
			request.URL.Path,
		)
		writeJSON(writer, releasedLeaseResponse(releasedAt))
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}

	t.Run("release", func(t *testing.T) {
		_, output, callErr := handlers.lease(t.Context(), nil, LeaseInput{Ref: "abc4", Action: "release"})
		require.NoError(t, callErr)
		require.False(t, output.Held)
		require.Equal(t, "example-agent", output.Holder)
		require.EqualValues(t, 2, output.Revision)
	})
	t.Run("force release", func(t *testing.T) {
		_, output, callErr := handlers.leaseForceRelease(t.Context(), nil, LeaseForceReleaseInput{
			Ref: "abc4", Reason: "operator handoff",
		})
		require.NoError(t, callErr)
		require.False(t, output.Held)
		require.Equal(t, "example-agent", output.Holder)
		require.EqualValues(t, 2, output.Revision)
	})
}

func TestLeaseQueuedAcquireReportsPendingRequest(t *testing.T) {
	requestUID := "01HQQQQQQQQQQQQQQQQQQQQQQQ"
	pending := true
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		require.True(t, strings.HasSuffix(request.URL.Path, "/actions/acquire"), request.URL.Path)
		writeJSON(writer, generated.ClaimActionResponseBody{
			Granted:    false,
			Pending:    &pending,
			RequestUID: &requestUID,
			Holder: generated.ClaimPrincipalOut{
				Holder: "example-agent", HolderInstanceUID: "01HIIIIIIIIIIIIIIIIIIIIIII", ClientKind: "mcp",
			},
		})
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}

	_, output, err := handlers.lease(t.Context(), nil, LeaseInput{Ref: "abc4", Action: "acquire"})
	require.NoError(t, err)
	require.False(t, output.Held)
	require.False(t, output.Granted)
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	require.Equal(t, true, fields["pending"])
	require.Equal(t, requestUID, fields["request_uid"])
}

func releasedLeaseResponse(releasedAt time.Time) generated.ClaimActionResponseBody {
	lease := &generated.IssueClaimOut{
		AcquiredAt: time.Date(2026, 8, 14, 11, 0, 0, 0, time.UTC),
		ClaimKind:  "hard", ClaimUID: "01HCCCCCCCCCCCCCCCCCCCCCCC", ClientKind: "mcp",
		Holder: "example-agent", HolderInstanceUID: "01HIIIIIIIIIIIIIIIIIIIIIII",
		IssueUID: "01HSSSSSSSSSSSSSSSSSSSSSSS", ProjectID: 1, Purpose: "work",
		ReleasedAt: &releasedAt, Revision: 2, UpdatedAt: releasedAt,
	}
	return generated.ClaimActionResponseBody{
		Granted: true,
		Holder: generated.ClaimPrincipalOut{
			Holder: "example-agent", HolderInstanceUID: "01HIIIIIIIIIIIIIIIIIIIIIII", ClientKind: "mcp",
		},
		Lease: lease,
	}
}
