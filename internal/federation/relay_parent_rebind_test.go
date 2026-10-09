package federation_test

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R3 requires same-account credential rotation to preserve relay identity and cursors.
func TestRelayExplicitParentCredentialRebind(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRecoveryRoot(t, backend)
			personal := newRelayMatrixNode(t, backend, "personal-member")
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			syncRelayMatrixNode(t, personal)
			old, err := root.store.AuthorizeFederationToken(t.Context(), personal.credential.Token, root.project.ID, "pull")
			require.NoError(t, err)
			require.NotNil(t, old.ParentTokenID)
			replacement := "replacement-company-user-test-token"
			_, _, err = root.store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: root.account, AdminActor: "admin", PlaintextToken: replacement})
			require.NoError(t, err)
			_, _, err = root.store.RevokeAPIToken(t.Context(), *old.ParentTokenID, "admin")
			require.NoError(t, err)
			offline, _, err := personal.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: personal.project.ID, Title: "Retained offline work", Author: personal.account})
			require.NoError(t, err)
			body := map[string]any{"project_id": root.project.ID, "spoke_instance_uid": personal.store.InstanceUID(), "capabilities": "claim,pull,push", "token": personal.credential.Token, "relay": map[string]any{"protocol_version": 1, "serve_downstream": true, "rebind_parent": true}}
			const otherAccount = "other-account-test-token"
			_, _, err = root.store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: "other-member", AdminActor: "admin", PlaintextToken: otherAccount})
			require.NoError(t, err)
			for _, mismatch := range []string{"no-opt-in", "other-account", "other-peer", "other-options"} {
				t.Run(mismatch, func(t *testing.T) {
					bearer := replacement
					options := body["relay"].(map[string]any)
					switch mismatch {
					case "no-opt-in":
						options["rebind_parent"] = false
					case "other-account":
						bearer = otherAccount
					case "other-peer":
						body["spoke_instance_uid"] = "00000000000000000000000009"
					case "other-options":
						options["serve_downstream"] = false
					}
					code, _ := postRelayCredentialRequest(t, root.http, "/api/v1/federation/enrollments", bearer, body)
					require.Equal(t, http.StatusConflict, code)
					options["rebind_parent"], options["serve_downstream"] = true, true
					body["spoke_instance_uid"] = personal.store.InstanceUID()
					grants, err := root.store.ListFederationEnrollments(t.Context())
					require.NoError(t, err)
					require.Len(t, grants, 1)
					require.Equal(t, old.ParentTokenID, grants[0].ParentTokenID)
				})
			}
			payload, err := json.Marshal(body)
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, root.http.URL+"/api/v1/federation/enrollments", bytes.NewReader(payload))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+replacement)
			request.Header.Set("Content-Type", "application/json")
			response, err := root.http.Client().Do(request)
			require.NoError(t, err)
			raw, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			t.Logf("same-account enrollment retry with retained bridge token: HTTP %d %s", response.StatusCode, raw)
			lifecycle := personal.store.(db.RelayLifecycleStore)
			t.Logf("disconnect/reset lifecycle with retained offline intent: %v", lifecycle.ValidateRelayLifecycle(t.Context(), personal.project.ID))
			require.Equal(t, 200, response.StatusCode, "same-account explicit parent replacement must preserve retained relay state")
			after, err := root.store.AuthorizeFederationToken(t.Context(), personal.credential.Token, root.project.ID, "pull")
			require.NoError(t, err)
			require.Equal(t, old.ID, after.ID)
			require.Equal(t, old.RelayBindingUID, after.RelayBindingUID)
			require.Equal(t, old.RelayResetEpoch, after.RelayResetEpoch)
			require.Equal(t, old.RelayProtocolVersion, after.RelayProtocolVersion)
			require.NotEqual(t, old.ParentTokenID, after.ParentTokenID)
			code, _ := postRelayCredentialRequest(t, root.http, "/api/v1/federation/enrollments", replacement, body)
			require.Equal(t, http.StatusOK, code, "lost rebind response retries the same grant")
			syncRelayMatrixNode(t, personal)
			received, err := root.store.IssueByUID(t.Context(), offline.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, offline.Title, received.Title)
			require.NoError(t, root.store.RevokeFederationEnrollment(t.Context(), after.ID))
			replay, err := http.NewRequestWithContext(t.Context(), http.MethodPost, root.http.URL+"/api/v1/federation/enrollments", bytes.NewReader(payload))
			require.NoError(t, err)
			replay.Header.Set("Authorization", "Bearer "+replacement)
			replay.Header.Set("Content-Type", "application/json")
			denied, err := root.http.Client().Do(replay)
			require.NoError(t, err)
			require.NoError(t, denied.Body.Close())
			require.Equal(t, http.StatusConflict, denied.StatusCode, "explicit parent rebinding cannot resurrect an explicitly revoked grant")
		})
	}
}

// R3 explicitly requires enrollment rotation to preserve relay identity/cursors.
func TestRelayRejectsLegacyEnrollmentRotation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRecoveryRoot(t, backend)
			personal := newRelayMatrixNode(t, backend, "personal-member")
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			syncRelayMatrixNode(t, personal)
			before, err := root.store.AuthorizeFederationToken(t.Context(), personal.credential.Token, root.project.ID, "pull")
			require.NoError(t, err)
			replacement := "replacement-company-user-test-token"
			_, _, err = root.store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: root.account, AdminActor: "admin", PlaintextToken: replacement})
			require.NoError(t, err)
			_, _, err = root.store.RevokeAPIToken(t.Context(), *before.ParentTokenID, "admin")
			require.NoError(t, err)
			payload, err := json.Marshal(map[string]any{"project_id": root.project.ID, "spoke_instance_uid": personal.store.InstanceUID(), "capabilities": "claim,pull,push", "token": "replacement-relay-transport-test-token"})
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, root.http.URL+"/api/v1/federation/enrollments/actions/rotate", bytes.NewReader(payload))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+replacement)
			request.Header.Set("Content-Type", "application/json")
			response, err := root.http.Client().Do(request)
			require.NoError(t, err)
			raw, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusForbidden, response.StatusCode, string(raw))
			require.Contains(t, string(raw), "federation_enrollment_requires_relay")
			grants, err := root.store.ListFederationEnrollments(t.Context())
			require.NoError(t, err)
			require.Len(t, grants, 1)
			require.Equal(t, before.ID, grants[0].ID)
			require.Equal(t, before.RelayBindingUID, grants[0].RelayBindingUID)
			require.Equal(t, before.ParentTokenID, grants[0].ParentTokenID)
			require.Nil(t, grants[0].RevokedAt, "legacy rotation must reject before mutating the negotiated grant")
		})
	}
}

func postRelayCredentialRequest(t *testing.T, server *httptest.Server, path, bearer string, body any) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	return response.StatusCode, raw
}
