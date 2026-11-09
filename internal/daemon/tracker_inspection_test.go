package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
)

// Recovery inspection consumes no provider authority. Actual daemon owners can
// inspect retained uncertainty even when the named policy snapshot is suspended.
func TestTrackerOwnerInspectsUnknownAfterPolicySuspension(t *testing.T) {
	f := newTrackerCoordinatorFixture(t)
	_, err := f.runtime.WriteNamedStatus(f.ctx, f.binding, f.mapping, "closed", f.guard, func(_ context.Context, admit func() error) (issuesync.StatusObservation, error) {
		if err := admit(); err != nil {
			return issuesync.StatusObservation{}, err
		}
		return issuesync.StatusObservation{}, errors.New("fixture lost response")
	}, func(context.Context) error { return nil })
	require.Error(t, err)
	f.runtime.Suspend()
	server := NewServer(ServerConfig{DB: f.store, TrackerRuntime: f.runtime, Auth: config.AuthConfig{Token: "fixture-owner"}})
	t.Cleanup(func() { _ = server.Close() })
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1/api/v1/projects/%d/tracker-recovery", f.binding.ProjectID), nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer fixture-owner")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"phase":"unknown"`)
	require.Contains(t, response.Body.String(), `"writes_blocked":true`)
	require.Contains(t, response.Body.String(), `"provider_acknowledged":false`)
	require.NotContains(t, response.Body.String(), "EXAMPLE_NOTION_TOKEN")
	request.Header.Set("X-Kata-Tracker-Client", "mcp")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
}

func TestTrackerSettlementRejectsMemberAndMCPSelfCallWithoutChangingReceipt(t *testing.T) {
	f := newTrackerCoordinatorFixture(t)
	ctx := t.Context()
	project, err := f.store.ProjectByID(ctx, f.binding.ProjectID)
	require.NoError(t, err)
	selection, err := db.IssueSyncConnectionFromConfig(f.binding.Config)
	require.NoError(t, err)
	principal := tracker.Principal{AuthorityInstanceUID: f.store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}
	original := tracker.ControlReceipt{Version: 1, RootInstanceUID: f.store.InstanceUID(), ProjectUID: project.UID, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Principal: principal, Selection: *selection, Kind: tracker.ControlRun, SourceKey: f.binding.SourceKey, RequestDigest: selectionDigestForControlTest(), State: tracker.ControlPending, ProcessEpoch: f.runtime.epoch}
	_, _, err = f.store.BeginTrackerControl(ctx, original)
	require.NoError(t, err)
	_, _, err = f.store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "fixture-member", Actor: "member", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	_, err = f.runtime.settleTrackerControl(ctx, project.UID, original.UID, principal)
	require.ErrorIs(t, err, tracker.ErrDenied)
	for _, mode := range []string{"member", "mcp-owner"} {
		auth := config.AuthConfig{Token: "fixture-owner", RequireTokenIdentity: mode == "member"}
		server := NewServer(ServerConfig{DB: f.store, TrackerRuntime: f.runtime, Auth: auth})
		t.Cleanup(func() { _ = server.Close() })
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1/api/v1/projects/%d/tracker-operations/%s/settle", project.ID, original.UID), bytes.NewBufferString(`{}`))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer fixture-member")
		if mode == "mcp-owner" {
			request.Header.Set("Authorization", "Bearer fixture-owner")
			request.Header.Set("X-Kata-Tracker-Client", "mcp")
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		retained, err := f.store.ReadTrackerControl(ctx, project.UID, original.UID)
		require.NoError(t, err)
		require.Equal(t, original, retained)
	}
}

func TestTrackerRecoveryRefreshRejectsMemberAndMCPOwner(t *testing.T) {
	f := newTrackerCoordinatorFixture(t)
	ctx := t.Context()
	_, _, err := f.store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "fixture-refresh-member", Actor: "member", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	for _, mode := range []string{"member", "mcp-owner"} {
		server := NewServer(ServerConfig{DB: f.store, TrackerRuntime: f.runtime, Auth: config.AuthConfig{Token: "fixture-owner", RequireTokenIdentity: mode == "member"}})
		t.Cleanup(func() { _ = server.Close() })
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1/api/v1/projects/%d/tracker-recovery/refresh", f.binding.ProjectID), bytes.NewBufferString(`{}`))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer fixture-refresh-member")
		if mode == "mcp-owner" {
			request.Header.Set("Authorization", "Bearer fixture-owner")
			request.Header.Set("X-Kata-Tracker-Client", "mcp")
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		project, err := f.store.ProjectByID(ctx, f.binding.ProjectID)
		require.NoError(t, err)
		_, found, err := f.store.LoadTrackerExecutionFence(ctx, f.store.InstanceUID(), project.UID, "coordinator")
		require.NoError(t, err)
		require.False(t, found)
	}
}
