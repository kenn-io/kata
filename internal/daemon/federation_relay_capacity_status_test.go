package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// countingCheckpointMeasurer records which projects the status route measures
// and can make one measurement fail.
type countingCheckpointMeasurer struct {
	db.Storage
	measurer db.RelayCheckpointMeasurer
	mu       sync.Mutex
	measured map[int64]int
	failFor  int64
}

var errMeasurementBroken = errors.New("retained artifact manifest is malformed")

func (m *countingCheckpointMeasurer) RelayCheckpointSize(ctx context.Context, projectID int64) (db.RelayCheckpointSize, error) {
	m.mu.Lock()
	m.measured[projectID]++
	m.mu.Unlock()
	if projectID == m.failFor {
		return db.RelayCheckpointSize{}, errMeasurementBroken
	}
	return m.measurer.RelayCheckpointSize(ctx, projectID)
}

func (m *countingCheckpointMeasurer) count(projectID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.measured[projectID]
}

// Status measures rooted relay projects, reports measurement failures, and
// skips checkpoint measurement for compact project reads.
func TestFederationStatusRelayMeasurementAndCompactRead(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	wrapped := &countingCheckpointMeasurer{Storage: store, measurer: store, measured: map[int64]int{}}
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	newHub := func(name string, pin bool) db.Project {
		project, err := store.CreateProject(ctx, name)
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
		if pin {
			require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		}
		return project
	}
	plain := newHub("plain-hub-project", false)
	rootedIdle := newHub("rooted-idle-project", true)
	relayed := newHub("relayed-project", true)
	broken := newHub("broken-project", true)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "capacity-status-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	for project, spoke := range map[*db.Project]string{&relayed: "00000000000000000000000006", &broken: "00000000000000000000000007"} {
		_, err = store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: spoke, ProtocolVersion: db.RelayProtocolVersion})
		require.NoError(t, err)
	}
	wrapped.failFor = broken.ID

	server := daemon.NewServer(daemon.ServerConfig{DB: wrapped, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/federation/status", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer bootstrap-test-token")
	response, err := httpServer.Client().Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
	var body api.FederationStatusBody
	require.NoError(t, json.UnmarshalRead(response.Body, &body))
	require.Equal(t, http.StatusOK, response.StatusCode, "a failed measurement must not fail the status response")

	statuses := map[int64]api.FederationProjectStatus{}
	for _, status := range body.Statuses {
		statuses[status.ProjectID] = status
	}
	require.Len(t, statuses, 4)
	require.Zero(t, wrapped.count(plain.ID), "a hub project without a root pin is never measured")
	require.Zero(t, wrapped.count(rootedIdle.ID), "a rooted project without relay enrollments is never measured")
	require.Equal(t, 1, wrapped.count(relayed.ID), "a rooted project with a relay enrollment is measured once")
	require.Zero(t, statuses[plain.ID].RelayCheckpointCapacity)
	require.Zero(t, statuses[rootedIdle.ID].RelayCheckpointCapacity)
	require.Positive(t, statuses[relayed.ID].RelayCheckpointBytes)
	require.EqualValues(t, db.RelayCheckpointCapacityBytes, statuses[relayed.ID].RelayCheckpointCapacity)
	require.Empty(t, statuses[relayed.ID].RelayCheckpointError)
	require.Equal(t, 1, wrapped.count(broken.ID))
	require.Zero(t, statuses[broken.ID].RelayCheckpointBytes, "a failed measurement reports no size")
	require.Zero(t, statuses[broken.ID].RelayCheckpointCapacity)
	require.Contains(t, statuses[broken.ID].RelayCheckpointError, "retained artifact manifest is malformed")

	readProject := func(query string) api.FederationProjectStatus {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/status%s", httpServer.URL, relayed.ID, query), nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer bootstrap-test-token")
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		var raw struct {
			Statuses []map[string]any `json:"statuses"`
		}
		require.NoError(t, json.Unmarshal(data, &raw))
		require.Len(t, raw.Statuses, 1)
		if query == "" {
			require.Contains(t, raw.Statuses[0], "relay_checkpoint_bytes")
			require.Contains(t, raw.Statuses[0], "relay_checkpoint_capacity_bytes")
		} else {
			require.NotContains(t, raw.Statuses[0], "relay_checkpoint_bytes")
			require.NotContains(t, raw.Statuses[0], "relay_checkpoint_capacity_bytes")
			require.NotContains(t, raw.Statuses[0], "relay_checkpoint_error")
		}
		var body api.FederationStatusBody
		require.NoError(t, json.Unmarshal(data, &body))
		require.Len(t, body.Statuses, 1)
		return body.Statuses[0]
	}
	full := readProject("")
	require.Equal(t, 2, wrapped.count(relayed.ID))
	require.Positive(t, full.RelayCheckpointBytes)
	require.EqualValues(t, db.RelayCheckpointCapacityBytes, full.RelayCheckpointCapacity)
	compact := readProject("?compact=true")
	require.Equal(t, 2, wrapped.count(relayed.ID), "compact status must skip checkpoint measurement")
	full.RelayCheckpointBytes = 0
	full.RelayCheckpointCapacity = 0
	require.Equal(t, full, compact)
}
