package daemon

import (
	"bytes"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/tracker"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestTrackerRelayPayloadSeparatesReadFromControlAndBindsDigest(t *testing.T) {
	p := api.TrackerRelayPayload{Kind: "execute", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Operation: &api.TrackerOperationBody{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Kind: tracker.ControlBind, Provider: "notion", Resource: "notion:source", Binding: &api.EnableIssueSyncRequestBody{Config: api.JSONMap{"data_source_id": "11111111-1111-4111-8111-111111111111"}}}}
	first, err := trackerRelayPayloadDigest(p, true)
	require.NoError(t, err)
	_, err = trackerRelayPayloadDigest(p, false)
	require.Error(t, err, "pull transport is not a mutation capability")
	p.Operation.Binding.Config["data_source_id"] = "22222222-2222-4222-8222-222222222222"
	second, err := trackerRelayPayloadDigest(p, true)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	p.Operation.Binding.Config["token_env"] = "FORBIDDEN_SELECTOR"
	_, err = trackerRelayPayloadDigest(p, true)
	require.Error(t, err, "selectors never enter assertions or operation digests")
	p = api.TrackerRelayPayload{Kind: "receipt", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ"}
	_, err = trackerRelayPayloadDigest(p, false)
	require.NoError(t, err)
	_, err = trackerRelayPayloadDigest(p, true)
	require.Error(t, err)
}

// A legacy enrollment bearer cannot acquire tracker control merely because it
// can transport project data. Authentication/capability precedes body decoding.
func TestTrackerTransportNeedsExplicitSignedCapability(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "member")
	require.NoError(t, err)
	grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Actor: "member", Token: "fixture-transport", Capabilities: "pull,push"})
	require.NoError(t, err)
	server := NewServer(ServerConfig{DB: store})
	t.Cleanup(func() { _ = server.Close() })
	for _, action := range []string{"read", "control"} {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1/api/v1/projects/%d/federation/tracker/%s", project.ID, action), bytes.NewBufferString("invalid-json"))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+grant.Token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "signing_unavailable")
	}
}

// Waiting relay calls must not consume their dependent negotiation capacity.
func TestTrackerRelaySaturationPreservesNegotiationIngress(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "admission.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	s := NewServer(ServerConfig{DB: store})
	t.Cleanup(func() { _ = s.Close() })
	relay, _ := s.federationSigningAdmission("executeTrackerRelay")
	for i := 0; i < cap(relay); i++ {
		relay <- struct{}{}
	}
	for _, operation := range []string{"getFederationProjectMetadata", "pollFederationProjectEvents", "getRelayReset"} {
		progress, _ := s.federationSigningAdmission(operation)
		select {
		case progress <- struct{}{}:
			<-progress
		default:
			t.Fatalf("waiting controls starved %s", operation)
		}
	}
	select {
	case relay <- struct{}{}:
		t.Fatal("relay admission must stay bounded")
	default:
	}
}
