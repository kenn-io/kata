package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssueScopedIncompleteBodyDoesNotBlockAnotherMutation(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(strconv.FormatBool(scoped), func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
			expiresAt := time.Now().UTC().Add(time.Hour)
			params := db.CreateAPITokenParams{
				PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
				Scope: &db.APITokenScope{
					Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
				}, ExpiresAt: &expiresAt,
			}
			if !scoped {
				params.Scope, params.ExpiresAt = nil, nil
			}
			_, _, err = env.DB.CreateAPIToken(t.Context(), params)
			require.NoError(t, err)
			commentPath := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) +
				"/issues/" + root.ShortID + "/comments"
			host := strings.TrimPrefix(env.URL, "http://")
			connection, err := net.DialTimeout("tcp", host, time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = connection.Close() })
			_, err = io.WriteString(connection,
				"POST "+commentPath+" HTTP/1.1\r\n"+
					"Host: "+host+"\r\n"+
					"Authorization: Bearer worker-token\r\n"+
					"Content-Type: application/json\r\n"+
					"Content-Length: 128\r\n\r\n{")
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				tokens, listErr := env.DB.ListAPITokens(t.Context())
				return listErr == nil && len(tokens) == 1 && tokens[0].LastUsedAt != nil
			}, time.Second, 10*time.Millisecond)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+commentPath,
				strings.NewReader(`{"actor":"worker-a","body":"bounded follow-up"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer worker-token")
			req.Header.Set("Content-Type", "application/json")
			response := make(chan *http.Response, 1)
			requestErr := make(chan error, 1)
			go func() {
				resp, doErr := env.HTTP.Do(req) //nolint:gosec // test request to loopback daemon
				if doErr != nil {
					requestErr <- doErr
					return
				}
				response <- resp
			}()
			select {
			case doErr := <-requestErr:
				require.NoError(t, doErr)
			case resp := <-response:
				defer func() { _ = resp.Body.Close() }()
				body, readErr := io.ReadAll(resp.Body)
				require.NoError(t, readErr)
				require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
			case <-time.After(time.Second):
				t.Fatal("incomplete request body blocked an independent mutation")
			}
		})
	}
}

func TestIssueScopedTokenNarrowsListBeforeLimitAndHidesDirectLookups(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside", nil)
	_, _, err = env.DB.AddLabelAndEvent(t.Context(), child.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "inside", Actor: "coordinator",
	})
	require.NoError(t, err)
	_, _, err = env.DB.AddLabelAndEvent(t.Context(), outside.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "outside", Actor: "coordinator",
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	healthResp, healthBody := envDoRaw(t, env, http.MethodGet, "/api/v1/health", nil, headers)
	require.Equalf(t, http.StatusOK, healthResp.StatusCode, "body: %s", healthBody)
	var health map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(healthBody, &health))
	require.NotContains(t, health, "db_path")

	resp, body := envDoRaw(t, env, http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues?limit=1", nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var listed struct {
		Issues []db.Issue `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(body, &listed))
	require.Len(t, listed.Issues, 1)
	require.Equal(t, child.UID, listed.Issues[0].UID)

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/search?q=Outside&mode=lexical&status=open", nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var searched struct {
		Results []any `json:"results"`
	}
	require.NoError(t, json.Unmarshal(body, &searched))
	require.Empty(t, searched.Results)

	resp, body = envDoRaw(t, env, http.MethodGet, "/api/v1/ready?limit=1", nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.NoError(t, json.Unmarshal(body, &listed))
	require.Len(t, listed.Issues, 1)
	require.Equal(t, child.UID, listed.Issues[0].UID)

	for _, tc := range []struct {
		ref  string
		want int
	}{
		{ref: root.ShortID, want: http.StatusOK},
		{ref: child.ShortID, want: http.StatusOK},
		{ref: outside.ShortID, want: http.StatusNotFound},
	} {
		resp, _ := envDoRaw(t, env, http.MethodGet,
			"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+tc.ref, nil, headers)
		require.Equal(t, tc.want, resp.StatusCode)
	}
	resp, _ = envDoRaw(t, env, http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+outside.ShortID+"/lease",
		nil, headers)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/labels", nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.Contains(t, string(body), `"inside"`)
	require.NotContains(t, string(body), `"outside"`)
	resp, body = envDoRaw(t, env, http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+child.ShortID+"/actions/close",
		map[string]any{
			"reason": "duplicate", "message": "Same delegated work is tracked elsewhere.",
			"evidence": []map[string]any{{"type": "duplicate-of", "issue_ref": outside.ShortID}},
		}, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
}

func TestIssueScopedTokenCannotUseLeaseRoutesOutsideSubtree(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(t.Context(), project.ID, "coordinator")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside", nil)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	basePath := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" + outside.ShortID + "/lease"

	resp, body := envDoRaw(t, env, http.MethodGet, basePath, nil, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")
	for _, action := range []string{"acquire", "renew", "release"} {
		resp, body = envDoRaw(t, env, http.MethodPost, basePath+"/actions/"+action,
			map[string]any{"holder": "worker-a", "client_kind": "cli"}, headers)
		assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")
	}
	resp, body = envDoRaw(t, env, http.MethodPost, basePath+"/actions/force_release",
		map[string]any{"actor": "worker-a"}, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusForbidden, "scoped_operation_forbidden")
}

func TestIssueScopedTokenCannotRemoveParentLink(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	parentLink, err := env.DB.ParentOf(t.Context(), child.ID)
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	path := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" + child.ShortID +
		"/links/" + strconv.FormatInt(parentLink.ID, 10) + "?actor=worker-a"

	resp, body := envDoRaw(t, env, http.MethodDelete, path, nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	assertAPIError(t, resp.StatusCode, body, http.StatusForbidden, "scoped_operation_forbidden")
	got, err := env.DB.ParentOf(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, parentLink.ID, got.ID)
}

func TestIssueScopedTokenStopsWorkingWhenProjectBecomesSpoke(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, Enabled: true,
		HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID,
		ReplayHorizonEventID: 1,
	})
	require.NoError(t, err)

	resp, body := envDoRaw(t, env, http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+root.ShortID, nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	assertAPIError(t, resp.StatusCode, body, http.StatusUnauthorized, "unauthorized")
}

func TestIssueScopedMutationRechecksExpiryAfterReadingBody(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	expiresAt := time.Now().UTC().Add(300 * time.Millisecond)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)

	reader, writer := io.Pipe()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		env.URL+"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+root.ShortID+"/comments",
		reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer worker-token")
	req.Header.Set("Content-Type", "application/json")
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, doErr := env.HTTP.Do(req) //nolint:gosec // test request to loopback daemon
		if doErr != nil {
			requestErr <- doErr
			return
		}
		response <- resp
	}()
	require.Eventually(t, func() bool {
		tokens, listErr := env.DB.ListAPITokens(t.Context())
		return listErr == nil && len(tokens) == 1 && tokens[0].LastUsedAt != nil
	}, time.Second, 10*time.Millisecond)
	time.Sleep(time.Until(expiresAt) + 25*time.Millisecond)
	_, err = io.WriteString(writer, `{"actor":"worker-a","body":"late comment"}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	select {
	case err := <-requestErr:
		require.NoError(t, err)
	case resp := <-response:
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		assertAPIError(t, resp.StatusCode, body, http.StatusUnauthorized, "unauthorized")
	case <-time.After(2 * time.Second):
		t.Fatal("delayed-body request did not complete")
	}
}

func TestIssueScopedReceiptReplayRechecksCurrentMembership(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	commentTarget := createScopedHTTPTestIssue(t, env, project.ID, "Comment target", &root)
	closeTarget := createScopedHTTPTestIssue(t, env, project.ID, "Close target", &root)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{
		"Authorization": "Bearer worker-token", "Idempotency-Key": "scoped-replay",
	}
	commentPath := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" +
		commentTarget.ShortID + "/comments"
	commentBody := map[string]any{"actor": "worker-a", "body": "Completed the delegated investigation."}
	resp, body := envDoRaw(t, env, http.MethodPost, commentPath, commentBody, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	commentParent, err := env.DB.ParentOf(t.Context(), commentTarget.ID)
	require.NoError(t, err)
	require.NoError(t, env.DB.DeleteLinkByID(t.Context(), commentParent.ID))
	resp, body = envDoRaw(t, env, http.MethodPost, commentPath, commentBody, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")
	commentBody["body"] = "Changed retry body must not reveal the hidden receipt."
	resp, body = envDoRaw(t, env, http.MethodPost, commentPath, commentBody, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")

	closeHeaders := map[string]string{
		"Authorization": "Bearer worker-token", "Idempotency-Key": "scoped-close-replay",
	}
	closePath := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" +
		closeTarget.ShortID + "/actions/close"
	closeBody := map[string]any{
		"actor": "worker-a", "reason": "done", "retry_protocol": "close-v1",
		"message":  "Implemented and verified the delegated change successfully.",
		"evidence": []map[string]any{{"type": "test", "command": "go test ./..."}},
	}
	resp, body = envDoRaw(t, env, http.MethodPost, closePath, closeBody, closeHeaders)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	closeParent, err := env.DB.ParentOf(t.Context(), closeTarget.ID)
	require.NoError(t, err)
	require.NoError(t, env.DB.DeleteLinkByID(t.Context(), closeParent.ID))
	resp, body = envDoRaw(t, env, http.MethodPost, closePath, closeBody, closeHeaders)
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")
	closeBody["message"] = "A changed retry still cannot reveal a hidden close receipt."
	resp, body = envDoRaw(t, env, http.MethodPost, closePath, closeBody, closeHeaders)
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")
}

func TestIssueScopedUIDPrefixAmbiguityIgnoresHiddenMatches(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "Root", Author: "coordinator",
	})
	require.NoError(t, err)
	hidden, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD5YZ", Title: "Hidden", Author: "coordinator",
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)

	resp, body := envDoRaw(t, env, http.MethodGet, "/api/v1/issues/01HZNQ7V", nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.Contains(t, string(body), root.UID)
	require.NotContains(t, string(body), hidden.UID)
	require.NotContains(t, string(body), hidden.ShortID)
}

func TestIssueScopedCloseResponseAndRefusalHideOutsideRelationships(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	outsideParent := createScopedHTTPTestIssue(t, env, project.ID, "Outside parent", nil)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", &outsideParent)
	otherProject, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	hiddenChild := createScopedHTTPTestIssue(t, env, otherProject.ID, "Hidden child title", nil)
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: hiddenChild.ID, ToIssueID: root.ID, Type: "parent", Author: "coordinator",
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	closePath := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" + root.ShortID + "/actions/close"
	closeBody := map[string]any{
		"actor": "worker-a", "reason": "done",
		"message":  "Implemented and verified the delegated change successfully.",
		"evidence": []map[string]any{{"type": "test", "command": "go test ./..."}},
	}
	resp, body := envDoRaw(t, env, http.MethodPost, closePath, closeBody, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusConflict, "parent_has_open_children")
	for _, secret := range []string{hiddenChild.UID, hiddenChild.ShortID, hiddenChild.Title, otherProject.Name} {
		require.NotContains(t, string(body), secret)
	}

	_, _, _, err = env.DB.CloseIssue(t.Context(), hiddenChild.ID, "done", "coordinator",
		"Completed hidden prerequisite independently.", nil)
	require.NoError(t, err)
	resp, body = envDoRaw(t, env, http.MethodPost, closePath, closeBody, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	for _, secret := range []string{outsideParent.UID, outsideParent.ShortID, outsideParent.Title} {
		require.NotContains(t, string(body), secret)
	}
}

func TestIssueScopedTokenReceivesOnlyItsMinimalProjectIdentity(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}

	resp, body := envDoRaw(t, env, http.MethodGet, "/api/v1/projects?include=stats,archived", nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var listed struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(body, &listed))
	require.Len(t, listed.Projects, 1)
	assertMinimalScopedProject(t, listed.Projects[0], project)

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10), nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var shown map[string]any
	require.NoError(t, json.Unmarshal(body, &shown))
	require.Empty(t, shown["aliases"])
	assertMinimalScopedProject(t, shown["project"].(map[string]any), project)

	resp, _ = envDoRaw(t, env, http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(other.ID, 10), nil, headers)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestIssueScopedTokenCreatesOnlyExplicitChildrenOfAccessibleIssues(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside", nil)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	path := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	resp, body := envDoRaw(t, env, http.MethodPost, path,
		map[string]any{"title": "Unparented"}, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusForbidden, "scoped_parent_required")

	resp, body = envDoRaw(t, env, http.MethodPost, path, map[string]any{
		"title": "Escaped child",
		"links": []map[string]any{{"type": "parent", "to_ref": outside.ShortID}},
	}, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")

	resp, body = envDoRaw(t, env, http.MethodPost, path, map[string]any{
		"title": "Delegated child",
		"links": []map[string]any{{"type": "parent", "to_ref": root.ShortID}},
	}, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var created struct {
		Issue db.Issue `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	require.Equal(t, "worker-a", created.Issue.Author)
	require.NoError(t, authorizeCreatedScopedChild(t, env, created.Issue, root))
}

func TestIssueScopedTokenNarrowsBrowserSnapshotAndReferences(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside", nil)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	// Warm the shared snapshot caches under broad authority first. A scoped
	// request on the same server must still execute against its own candidate set.
	wideResp, wideBody := envDoRaw(t, env, http.MethodGet,
		"/api/v1/ui/snapshot?view=all-open&limit=1&include_graph=true", nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, wideResp.StatusCode, "body: %s", wideBody)

	resp, body := envDoRaw(t, env, http.MethodGet,
		"/api/v1/ui/snapshot?view=all-open&limit=1&include_graph=true", nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var snapshot struct {
		Catalog []struct {
			Project map[string]any `json:"project"`
		} `json:"catalog"`
		Collection []db.UIIssue `json:"collection"`
		Graph      struct {
			Issues []db.UIIssue `json:"issues"`
		} `json:"graph"`
	}
	require.NoError(t, json.Unmarshal(body, &snapshot))
	require.Len(t, snapshot.Catalog, 1)
	assertMinimalScopedProject(t, snapshot.Catalog[0].Project, project)
	require.Len(t, snapshot.Collection, 1)
	require.Equal(t, child.UID, snapshot.Collection[0].UID)
	require.ElementsMatch(t, []string{root.UID, child.UID}, uiIssueUIDs(snapshot.Graph.Issues))

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/ui/snapshot?view=all-open&selected_issue_uid="+outside.UID, nil, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/ui/references?q=Outside", nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var references struct {
		Projects []map[string]any      `json:"projects"`
		Issues   []db.UIIssueReference `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(body, &references))
	require.Len(t, references.Projects, 1)
	assertMinimalScopedProject(t, references.Projects[0], project)
	require.Empty(t, references.Issues)

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/ui/issue-reference?project_id="+strconv.FormatInt(project.ID, 10)+"&ref="+outside.ShortID,
		nil, headers)
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")
}

func TestIssueScopedTokenProjectsEventsWithoutOutsideIdentities(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside", nil)
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: outside.ID,
		Type: "related", Author: "coordinator",
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}

	resp, body := envDoRaw(t, env, http.MethodGet, "/api/v1/events?after_id=0&limit=100", nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var polled struct {
		Events      []map[string]any `json:"events"`
		NextAfterID int64            `json:"next_after_id"`
	}
	require.NoError(t, json.Unmarshal(body, &polled))
	require.NotEmpty(t, polled.Events)
	allowedUIDs := map[string]bool{root.UID: true, child.UID: true}
	for _, event := range polled.Events {
		issueUID, ok := event["issue_uid"].(string)
		require.True(t, ok)
		require.True(t, allowedUIDs[issueUID], "unexpected event: %#v", event)
		require.Empty(t, event["content_hash"])
		require.NotContains(t, string(body), outside.UID)
	}
	scannable, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{AfterID: 0, Limit: 100})
	require.NoError(t, err)
	require.NotEmpty(t, scannable)
	require.Equal(t, scannable[len(scannable)-1].ID, polled.NextAfterID,
		"hidden events must still advance the caller's scan cursor")

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+child.ShortID+"/graph?depth=full",
		nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.NotContains(t, string(body), outside.UID)
	require.NotContains(t, string(body), outside.ShortID)
}

func TestIssueScopedPollResetsWhenHiddenBlockerChangesReadiness(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	otherProject, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	outside := createScopedHTTPTestIssue(t, env, otherProject.ID, "Outside blocker", nil)
	unrelated := createScopedHTTPTestIssue(t, env, otherProject.ID, "Unrelated work", nil)
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: outside.ID,
		ToIssueID:   child.ID,
		Type:        "blocks",
		Author:      "coordinator",
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "coordinator-token", Actor: "coordinator", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	resp, body := envDoRaw(t, env, http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(otherProject.ID, 10)+"/issues/"+unrelated.ShortID+"/actions/close",
		map[string]any{
			"reason": "done", "message": "The unrelated work is complete and independently verified.",
			"evidence": []map[string]any{{"type": "test", "command": "go test ./..."}},
		}, map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id="+strconv.FormatInt(afterID, 10), nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var unrelatedPoll struct {
		ResetRequired bool             `json:"reset_required"`
		NextAfterID   int64            `json:"next_after_id"`
		Events        []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &unrelatedPoll))
	require.False(t, unrelatedPoll.ResetRequired)
	require.Greater(t, unrelatedPoll.NextAfterID, afterID)
	require.Empty(t, unrelatedPoll.Events)
	require.NotContains(t, string(body), unrelated.UID)
	afterID = unrelatedPoll.NextAfterID

	resp, body = envDoRaw(t, env, http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(otherProject.ID, 10)+"/issues/"+outside.ShortID+"/actions/close",
		map[string]any{
			"reason": "done", "message": "The prerequisite work is complete and independently verified.",
			"evidence": []map[string]any{{"type": "test", "command": "go test ./..."}},
		}, map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id="+strconv.FormatInt(afterID, 10), nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var polled struct {
		ResetRequired bool             `json:"reset_required"`
		ResetAfterID  int64            `json:"reset_after_id"`
		NextAfterID   int64            `json:"next_after_id"`
		Events        []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &polled))
	require.True(t, polled.ResetRequired)
	require.Greater(t, polled.ResetAfterID, afterID)
	require.Equal(t, polled.ResetAfterID, polled.NextAfterID)
	require.Empty(t, polled.Events)
	require.NotContains(t, string(body), outside.UID)
}

func TestIssueScopedPollResetsWhenHiddenCreatedIssueBlocksAccessibleIssue(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	otherProject, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "coordinator-token", Actor: "coordinator", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	// A hidden issue is created in another project carrying an initial
	// blocks link to the accessible child, so the child's derived readiness
	// changes because of an issue the scoped client can never see.
	resp, body := envDoRaw(t, env, http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(otherProject.ID, 10)+"/issues",
		map[string]any{
			"actor": "coordinator", "title": "Hidden cross-project blocker",
			"links": []map[string]any{
				{"type": "blocks", "to_ref": project.Name + "#" + child.ShortID},
			},
		}, map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var created struct {
		Issue db.Issue `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(body, &created))

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id="+strconv.FormatInt(afterID, 10), nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var polled struct {
		ResetRequired bool             `json:"reset_required"`
		ResetAfterID  int64            `json:"reset_after_id"`
		NextAfterID   int64            `json:"next_after_id"`
		Events        []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &polled))
	require.True(t, polled.ResetRequired,
		"a hidden issue.created whose initial link touches an accessible issue must force a scoped reset")
	require.Greater(t, polled.ResetAfterID, afterID)
	require.Equal(t, polled.ResetAfterID, polled.NextAfterID)
	require.Empty(t, polled.Events)
	for _, secret := range []string{created.Issue.UID, created.Issue.ShortID, created.Issue.Title} {
		require.NotContains(t, string(body), secret,
			"the reset must be identity-free")
	}
}

func TestIssueScopedPollResetsWhenScopedProjectRenamed(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	otherProject, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	_ = createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "coordinator-token", Actor: "coordinator", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	// Renaming the scoped project changes the project name and qualified IDs
	// the scoped client renders, so its stream must invalidate projections.
	resp, body := envDoRaw(t, env, http.MethodPatch,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10),
		map[string]any{"actor": "coordinator", "name": "renamed-project"},
		map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id="+strconv.FormatInt(afterID, 10), nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var polled struct {
		ResetRequired bool             `json:"reset_required"`
		ResetAfterID  int64            `json:"reset_after_id"`
		NextAfterID   int64            `json:"next_after_id"`
		Events        []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &polled))
	require.True(t, polled.ResetRequired,
		"a project.renamed event for the scoped project must force a scoped reset")
	require.Greater(t, polled.ResetAfterID, afterID)
	require.Equal(t, polled.ResetAfterID, polled.NextAfterID)
	require.Empty(t, polled.Events)
	require.NotContains(t, string(body), "renamed-project",
		"the reset must be identity-free and must not forward the raw rename payload")
	require.NotContains(t, string(body), "example-project#",
		"the reset must not leak qualified IDs rendered from the old name")

	// A rename confined to another project cannot change any projection the
	// scoped client renders, so it must not force another reset.
	afterID = polled.NextAfterID
	resp, body = envDoRaw(t, env, http.MethodPatch,
		"/api/v1/projects/"+strconv.FormatInt(otherProject.ID, 10),
		map[string]any{"actor": "coordinator", "name": "other-project-renamed"},
		map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id="+strconv.FormatInt(afterID, 10), nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var foreignPolled struct {
		ResetRequired bool             `json:"reset_required"`
		NextAfterID   int64            `json:"next_after_id"`
		Events        []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &foreignPolled))
	require.False(t, foreignPolled.ResetRequired,
		"a project.renamed event outside the scoped project must not force a reset")
	require.Greater(t, foreignPolled.NextAfterID, afterID,
		"the hidden foreign rename must still advance the scan cursor")
	require.Empty(t, foreignPolled.Events)
	require.NotContains(t, string(body), "other-project-renamed")
}

func TestIssueScopedPollResetsWhenCompoundEditMovesChildOutsideSubtree(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside parent", nil)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "coordinator-token", Actor: "coordinator", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	resp, body := envDoRaw(t, env, http.MethodPatch,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+child.ShortID,
		map[string]any{
			"actor": "coordinator", "links_delta": map[string]any{"set_parent": outside.ShortID},
		}, map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	resp, body = envDoRaw(t, env, http.MethodGet,
		"/api/v1/events?after_id="+strconv.FormatInt(afterID, 10), nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var polled struct {
		ResetRequired bool             `json:"reset_required"`
		Events        []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(body, &polled))
	require.True(t, polled.ResetRequired)
	require.Empty(t, polled.Events)
	for _, secret := range []string{outside.UID, outside.ShortID, outside.Title} {
		require.NotContains(t, string(body), secret)
	}
}

func TestIssueScopedSSELiveResetsWhenHiddenBlockerChangesReadiness(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	otherProject, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	outside := createScopedHTTPTestIssue(t, env, otherProject.ID, "Outside blocker", nil)
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: outside.ID,
		ToIssueID:   child.ID,
		Type:        "blocks",
		Author:      "coordinator",
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		}, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "coordinator-token", Actor: "coordinator", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer worker-token")
	stream := openSSE(t, env,
		"project_id="+strconv.FormatInt(project.ID, 10)+"&after_id="+strconv.FormatInt(afterID, 10),
		headers)
	defer func() { _ = stream.Body.Close() }()
	framer := newSSEFramer(stream.Body)

	resp, body := envDoRaw(t, env, http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(otherProject.ID, 10)+"/issues/"+outside.ShortID+"/actions/close",
		map[string]any{
			"reason": "done", "message": "The prerequisite work is complete and independently verified.",
			"evidence": []map[string]any{{"type": "test", "command": "go test ./..."}},
		}, map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	reset, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "hidden blocker close should produce a terminal refresh signal")
	require.Equal(t, "sync.reset_required", reset.event)
	require.NotContains(t, reset.data, outside.UID)
}

func uiIssueUIDs(issues []db.UIIssue) []string {
	uids := make([]string, 0, len(issues))
	for _, issue := range issues {
		uids = append(uids, issue.UID)
	}
	return uids
}

func authorizeCreatedScopedChild(t *testing.T, env *testenv.Env, child, parent db.Issue) error {
	t.Helper()
	link, err := env.DB.ParentOf(t.Context(), child.ID)
	if err != nil {
		return err
	}
	require.Equal(t, parent.ID, link.ToIssueID)
	return nil
}

func assertMinimalScopedProject(t *testing.T, raw map[string]any, project db.Project) {
	t.Helper()
	require.EqualValues(t, project.ID, raw["id"])
	require.Equal(t, project.UID, raw["uid"])
	require.Equal(t, project.Name, raw["name"])
	require.EqualValues(t, project.Revision, raw["revision"])
	require.Equal(t, true, raw["active"])
	require.Equal(t, map[string]any{}, raw["metadata"])
	require.Equal(t, "0001-01-01T00:00:00Z", raw["created_at"])
	for _, forbidden := range []string{"deleted_at", "stats"} {
		require.NotContains(t, raw, forbidden)
	}
}

func createScopedHTTPTestIssue(t *testing.T, env *testenv.Env, projectID int64, title string, parent *db.Issue) db.Issue {
	t.Helper()
	params := db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "coordinator"}
	if parent != nil {
		params.Links = []db.InitialLink{{Type: "parent", ToNumber: parent.ID}}
	}
	issue, _, err := env.DB.CreateIssue(t.Context(), params)
	require.NoError(t, err)
	return issue
}
