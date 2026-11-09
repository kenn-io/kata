package daemon

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/tracker"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestTrackerWorkerTransportAuthenticatesBeforeBodyDecode(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "worker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "worker-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "member")
	require.NoError(t, err)
	grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Actor: "member", Token: "fixture-transport", Capabilities: "pull,push"})
	require.NoError(t, err)
	server := NewServer(ServerConfig{DB: store})
	t.Cleanup(func() { _ = server.Close() })
	for _, action := range []string{"poll", "reply", "chunk", "admit", "metadata"} {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1/api/v1/projects/%d/federation/tracker/worker/%s", project.ID, action), bytes.NewBufferString("invalid-json"))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+grant.Token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "signing_unavailable")
	}
}

func TestFederationMetadataAdvertisesConfiguredTrackerWorkerProtocol(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "metadata-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "member")
	require.NoError(t, err)
	grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Actor: "member", Token: "fixture-metadata-transport", Capabilities: "pull,push"})
	require.NoError(t, err)
	runtime, err := NewTrackerRuntime(ctx, store, nil, nil)
	require.NoError(t, err)
	// Metadata negotiation is an unsigned legacy read here. The verifier marker
	// advertises implemented signed worker routes; no key/credential is installed
	// and no verification or provider method is invoked by this fixture request.
	server := NewServer(ServerConfig{DB: store, TrackerRuntime: runtime, FederationSigning: &federationsigning.Verifier{}})
	t.Cleanup(func() { _ = server.Close() })
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1/api/v1/projects/%d/federation/metadata", project.ID), nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer "+grant.Token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var metadata api.ProjectFederationBody
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &metadata))
	require.Equal(t, tracker.WorkerProtocolVersion, metadata.TrackerWorkerVersion)
	require.Zero(t, trackerWorkerProtocol(ServerConfig{DB: store, TrackerRuntime: runtime}), "unsigned peers must fail negotiation before credential resolution")
}

// Contract: relay callers waiting for owner results cannot occupy the bounded
// ingress capacity needed by that owner to negotiate, poll, reply or recover.
func TestTrackerRelaySaturationPreservesWorkerIngress(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "admission.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	s := NewServer(ServerConfig{DB: store})
	t.Cleanup(func() { _ = s.Close() })
	relay, _ := s.federationSigningAdmission("executeTrackerRelay")
	for i := 0; i < cap(relay); i++ {
		relay <- struct{}{}
	}
	for _, operation := range []string{"getFederationProjectMetadata", "pollTrackerWorker", "replyTrackerWorker", "admitTrackerWorker", "settleTrackerWorkerMetadata", "uploadTrackerWorkerChunk"} {
		progress, _ := s.federationSigningAdmission(operation)
		select {
		case progress <- struct{}{}:
			<-progress
		default:
			t.Fatalf("waiting relay calls starved %s", operation)
		}
	}
	select {
	case relay <- struct{}{}:
		t.Fatal("relay admission must still be bounded")
	default:
	}
}
