package federation_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func relayMatrixRequest(t *testing.T, node *relayMatrixNode, method, path, token string, payload any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, node.http.URL+path, body)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := node.http.Client().Do(request)
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	return response.StatusCode, raw
}

// A root hub refuses to let a project grow past the point where its signed
// checkpoint could not be built, reports the measured size, and keeps smaller
// projects enrolling.
func TestRelayEnrollmentRefusesProjectOverCheckpointCapacity(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			const capacity = 8192
			root := newRelayMatrixNodeFromStore(t, openRelayMatrixStore(t, backend, false, db.WithRelayCheckpointCapacity(capacity)), "root-member")
			newRoot := func(name string) db.Project {
				project, err := root.store.CreateProject(ctx, name)
				require.NoError(t, err)
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				return project
			}
			small := newRoot("small-project")
			large := newRoot("large-project")
			_, _, err := root.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: large.ID, Title: "Large body", Body: strings.Repeat("x", 3000), Author: "member"})
			require.NoError(t, err)
			_, _, err = root.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: small.ID, Title: "Small body", Author: "member"})
			require.NoError(t, err)

			enrollment := map[string]any{"project_id": large.ID, "spoke_instance_uid": "00000000000000000000000006", "capabilities": "claim,pull,push", "relay": api.RelayEnrollmentOptions{ProtocolVersion: 1}}
			status, raw := relayMatrixRequest(t, root, http.MethodPost, "/api/v1/federation/enrollments", root.userToken, enrollment)
			require.Equal(t, http.StatusConflict, status, string(raw))
			var envelope api.ErrorEnvelope
			require.NoError(t, json.Unmarshal(raw, &envelope))
			require.Equal(t, "relay_project_too_large", envelope.Error.Code)
			require.Contains(t, envelope.Error.Message, "the capacity is 8192 bytes")
			require.Contains(t, envelope.Error.Hint, "purge issues the project no longer needs")
			enrollments, err := root.store.ListFederationEnrollments(ctx)
			require.NoError(t, err)
			require.Empty(t, enrollments, "a refused enrollment issues no grant")

			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			root.project = small
			enrollRelayMatrixReplica(t, root, leaf, "small-alias", false)
			syncRelayMatrixNode(t, leaf)
			issues, err := leaf.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: leaf.project.ID})
			require.NoError(t, err)
			require.Len(t, issues, 1, "a project under capacity enrolls and syncs")

			status, raw = relayMatrixRequest(t, root, http.MethodGet, "/api/v1/federation/status", "bootstrap-test-token", nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			var body api.FederationStatusBody
			require.NoError(t, json.Unmarshal(raw, &body))
			sizes := map[int64]api.FederationProjectStatus{}
			for _, s := range body.Statuses {
				sizes[s.ProjectID] = s
			}
			require.Zero(t, sizes[large.ID].RelayCheckpointCapacity, "a project without a live relay is not measured; the 409 already carried its size")
			require.Zero(t, sizes[large.ID].RelayCheckpointBytes)
			require.EqualValues(t, capacity, sizes[small.ID].RelayCheckpointCapacity)
			require.Positive(t, sizes[small.ID].RelayCheckpointBytes)
			require.LessOrEqual(t, sizes[small.ID].RelayCheckpointBytes, int64(capacity))
		})
	}
}
