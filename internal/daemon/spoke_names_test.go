package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

const spokeNameTestUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"

func spokeNameBearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func TestFederationSpokeNamesAPI(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-fixture"))
	path := "/api/v1/federation/enrollments"
	resp, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"project_id": nil, "spoke_instance_uid": spokeNameTestUID, "spoke_name": "laptop", "capabilities": "pull", "actor": "member"}, spokeNameBearer("owner-fixture"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var result map[string]any
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, "laptop", result["spoke_name"])
	namePath := path + "/spokes/" + spokeNameTestUID + "/name"
	for _, name := range []string{"build-box", ""} {
		resp, raw = envDoRaw(t, env, http.MethodPut, namePath, map[string]any{"name": name}, spokeNameBearer("owner-fixture"))
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		resp, raw = envDoRaw(t, env, http.MethodGet, path, nil, spokeNameBearer("owner-fixture"))
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		var list struct {
			Enrollments []map[string]any `json:"enrollments"`
		}
		require.NoError(t, json.Unmarshal(raw, &list))
		require.Len(t, list.Enrollments, 1)
		if name == "" {
			require.NotContains(t, list.Enrollments[0], "spoke_name")
		} else {
			require.Equal(t, name, list.Enrollments[0]["spoke_name"])
		}
	}
	resp, raw = envDoRaw(t, env, http.MethodPut, namePath, map[string]any{"name": "Bad"}, spokeNameBearer("owner-fixture"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	resp, _ = envDoRaw(t, env, http.MethodPut, namePath, map[string]any{"name": "phone"}, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
func TestFederationSpokeNameRequiresOwner(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-fixture"), func(cfg *daemon.ServerConfig) { cfg.Auth.RequireTokenIdentity = true })
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "account-fixture", Actor: "member", AdminActor: "bootstrap"})
	require.NoError(t, err)
	_, err = env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: spokeNameTestUID, SpokeName: "laptop", Capabilities: "pull", Actor: "member"})
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/federation/enrollments", "/api/v1/federation/enrollments/spokes/" + spokeNameTestUID + "/name"} {
		method := http.MethodGet
		if path != "/api/v1/federation/enrollments" {
			method = http.MethodPut
		}
		resp, _ := envDoRaw(t, env, method, path, map[string]any{"name": "phone"}, spokeNameBearer("account-fixture"))
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	}
	names, err := env.DB.ListFederationSpokeNames(t.Context())
	require.NoError(t, err)
	require.Equal(t, "laptop", names[spokeNameTestUID])
}

func TestFederationSpokeNamesEvents(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	issue, event, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Named origin", Author: "member"})
	require.NoError(t, err)
	// Enroll this fixture's origin so ordinary events exercise the same hub lookup.
	_, err = env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: event.OriginInstanceUID, Actor: "member", Capabilities: "pull", SpokeName: "laptop"})
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/events", fmt.Sprintf("/api/v1/projects/%d/events", project.ID), fmt.Sprintf("/api/v1/projects/%d/events?issue=%s", project.ID, issue.UID)} {
		resp, raw := envDoRaw(t, env, http.MethodGet, path, nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		var body struct {
			Events []map[string]any `json:"events"`
		}
		require.NoError(t, json.Unmarshal(raw, &body))
		require.NotEmpty(t, body.Events)
		for _, e := range body.Events {
			require.Equal(t, "laptop", e["origin_spoke_name"])
		}
	}
	require.NoError(t, env.DB.SetFederationSpokeName(t.Context(), event.OriginInstanceUID, ""))
	_, raw := envDoRaw(t, env, http.MethodGet, "/api/v1/events", nil, nil)
	require.NotContains(t, string(raw), "origin_spoke_name")
}
