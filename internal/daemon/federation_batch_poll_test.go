package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testfix"
)

const batchPollPath = "/api/v1/federation/events:poll"

type batchPollFixture struct {
	store               db.Storage
	server              *httptest.Server
	visible, restricted db.Project
	other               db.Project
	team                db.Team
	token               string
}

// newBatchPollFixture serves a hub with three federated projects and one
// shared pull credential. The credential's actor may read visible and other,
// but not restricted.
func newBatchPollFixture(t *testing.T, store db.Storage, cfg daemon.ServerConfig) batchPollFixture {
	t.Helper()
	ctx := t.Context()
	f := batchPollFixture{store: store, token: "batch-poll-shared-token"}
	for i, project := range []*db.Project{&f.visible, &f.restricted, &f.other} {
		var err error
		*project, err = store.CreateProject(ctx, fmt.Sprintf("hub-project-%d", i))
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, project.ID, "admin")
		require.NoError(t, err)
	}
	var err error
	f.team, _, err = store.CreateTeam(ctx, "other-team", "admin")
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{
		ProjectUID: f.restricted.UID, Visibility: "teams", TeamUIDs: []string{f.team.UID},
	}, "admin")
	require.NoError(t, err)
	_, err = store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: f.token, SpokeInstanceUID: federationTestSpokeUID, Capabilities: "pull", Actor: "example-actor",
	})
	require.NoError(t, err)
	cfg.DB = store
	cfg.FederationBatchPoll = true
	server := daemon.NewServer(cfg)
	f.server = httptest.NewServer(server.Handler())
	t.Cleanup(f.server.Close)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return f
}

func (f batchPollFixture) single(t *testing.T, projectID, afterID int64) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d/federation/events?after_id=%d&limit=100", f.server.URL, projectID, afterID), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+f.token)
	request.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
	return doBatchPollRequest(t, f.server, request)
}

func (f batchPollFixture) batch(t *testing.T, body any, headers ...string) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+batchPollPath, bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.token)
	request.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	return doBatchPollRequest(t, f.server, request)
}

func doBatchPollRequest(t *testing.T, server *httptest.Server, request *http.Request) (*http.Response, []byte) {
	t.Helper()
	// A direct loopback peer is the daemon owner. A forwarded request is not,
	// so it gets the project checks a remote spoke gets.
	request.Header.Set("Forwarded", "for=192.0.2.10")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	return response, raw
}

func batchPollCursors(afterID int64, projectIDs ...int64) api.FederationBatchPollRequestBody {
	var body api.FederationBatchPollRequestBody
	for _, id := range projectIDs {
		body.Projects = append(body.Projects, api.FederationBatchPollCursor{ProjectID: id, AfterID: afterID})
	}
	return body
}

func decodeBatchPoll(t *testing.T, raw []byte) api.FederationBatchPollBody {
	t.Helper()
	var out api.FederationBatchPollBody
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	return out
}

func TestFederationBatchPollReportsEachProjectAsTheSingleRouteWithoutEvents(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newBatchPollFixture(t, store, daemon.ServerConfig{})
		// Catch the visible project up first, so the batch's one-event read
		// from there is the issue below and a leak of that read shows its title.
		startResponse, startRaw := f.single(t, f.visible.ID, 0)
		require.Equal(t, http.StatusOK, startResponse.StatusCode, string(startRaw))
		var start api.PollEventsBody
		require.NoError(t, json.Unmarshal(startRaw, &start))
		after := map[int64]int64{f.visible.ID: start.NextAfterID}
		_, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.visible.ID, Title: "visible-work-canary", Author: "admin"})
		require.NoError(t, err)
		_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.restricted.ID, Title: "restricted-canary", Author: "admin"})
		require.NoError(t, err)
		const missing = int64(987654)

		body := batchPollCursors(0, f.visible.ID, f.restricted.ID, f.other.ID, missing)
		body.Projects[0].AfterID = after[f.visible.ID]
		response, raw := f.batch(t, body)
		require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		assert.NotContains(t, string(raw), "visible-work-canary", "the batch reports whether a project has events, never the events")
		assert.NotContains(t, string(raw), "restricted-canary")
		assert.NotContains(t, string(raw), f.restricted.Name)
		got := decodeBatchPoll(t, raw)
		require.Len(t, got.Projects, 4)

		caughtUp := map[int64]int64{}
		for i, projectID := range []int64{f.visible.ID, f.restricted.ID, f.other.ID, missing} {
			entry := got.Projects[i]
			require.Equal(t, projectID, entry.ProjectID)
			singleResponse, singleRaw := f.single(t, projectID, after[projectID])
			require.Equal(t, singleResponse.StatusCode, entry.Status, "project %d: %s", projectID, singleRaw)
			if singleResponse.StatusCode == http.StatusOK {
				var page api.PollEventsBody
				require.NoError(t, json.Unmarshal(singleRaw, &page))
				require.NotEmpty(t, page.Events)
				assert.False(t, entry.Idle, "project %d has events after %d", projectID, after[projectID])
				assert.Nil(t, entry.Error)
				caughtUp[projectID] = page.NextAfterID
				continue
			}
			var want api.ErrorEnvelope
			require.NoError(t, json.Unmarshal(singleRaw, &want))
			require.NotNil(t, entry.Error)
			assert.Equal(t, want, *entry.Error)
			assert.False(t, entry.Idle)
		}
		assert.NotEqual(t, http.StatusOK, got.Projects[1].Status, "the restricted project is refused")
		require.Len(t, caughtUp, 2)

		body = api.FederationBatchPollRequestBody{Projects: []api.FederationBatchPollCursor{
			{ProjectID: f.visible.ID, AfterID: caughtUp[f.visible.ID]},
			{ProjectID: f.other.ID, AfterID: caughtUp[f.other.ID]},
		}}
		response, raw = f.batch(t, body)
		require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		got = decodeBatchPoll(t, raw)
		require.Len(t, got.Projects, 2)
		for _, entry := range got.Projects {
			assert.Equal(t, http.StatusOK, entry.Status)
			assert.True(t, entry.Idle, "project %d has nothing after its last event", entry.ProjectID)
		}
	})
}

// revokeOnSecondFederationRead revokes the actor's access to one project when
// the batch reaches that project's read, after an earlier project was served.
type revokeOnSecondFederationRead struct {
	db.Storage
	reads  int
	revoke func() error
}

func (s *revokeOnSecondFederationRead) ReadFederation(ctx context.Context, params db.FederationReadParams) (db.FederationReadResult, error) {
	s.reads++
	if s.reads == 2 && s.revoke != nil {
		if err := s.revoke(); err != nil {
			return db.FederationReadResult{}, err
		}
	}
	return s.Storage.ReadFederation(ctx, params)
}

func TestFederationBatchPollRevocationRefusesOnlyTheRevokedProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		wrapped := &revokeOnSecondFederationRead{Storage: store}
		f := newBatchPollFixture(t, wrapped, daemon.ServerConfig{})
		ctx := t.Context()
		_, err := store.SetTeamMembership(ctx, f.team.UID, "example-actor", true, "admin")
		require.NoError(t, err)
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.visible.ID, Title: "first project work", Author: "admin"})
		require.NoError(t, err)
		const canary = "after-revocation-canary"
		wrapped.revoke = func() error {
			if _, err := store.SetTeamMembership(ctx, f.team.UID, "example-actor", false, "admin"); err != nil {
				return err
			}
			_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.restricted.ID, Title: canary, Author: "admin"})
			return err
		}

		response, raw := f.batch(t, batchPollCursors(0, f.visible.ID, f.restricted.ID))
		require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		require.Equal(t, 2, wrapped.reads)
		assert.NotContains(t, string(raw), canary)
		got := decodeBatchPoll(t, raw)
		require.Len(t, got.Projects, 2)
		assert.Equal(t, http.StatusOK, got.Projects[0].Status)
		assert.False(t, got.Projects[0].Idle)
		assert.Equal(t, http.StatusNotFound, got.Projects[1].Status)
	})
}

func TestFederationBatchPollRejectsMoreProjectsThanTheBound(t *testing.T) {
	// Both checks run before any project is read, so one backend covers them.
	store, err := testfix.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := newBatchPollFixture(t, store, daemon.ServerConfig{})
	ids := make([]int64, api.FederationBatchPollMaxProjects+1)
	for i := range ids {
		ids[i] = f.visible.ID + int64(i)
	}
	response, raw := f.batch(t, batchPollCursors(0, ids...))
	assert.Equal(t, http.StatusBadRequest, response.StatusCode, string(raw))

	response, raw = f.batch(t, batchPollCursors(0, f.visible.ID, f.visible.ID))
	assert.Equal(t, http.StatusBadRequest, response.StatusCode, string(raw))
}

func TestFederationBatchPollIsAdvertisedOnlyWhenSigningIsOptional(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		optional := newBatchPollFixture(t, store, daemon.ServerConfig{})
		response, raw := optional.single(t, optional.visible.ID, 0)
		require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		assert.Contains(t, strings.Split(response.Header.Get(db.EventFeaturesHeader), ","), db.FederationBatchPollFeature)

		response, raw = optional.batch(t, batchPollCursors(0, optional.visible.ID), "Signature", "sig1=:AAAA:", "Signature-Input", `sig1=("@method")`)
		assert.Equal(t, http.StatusBadRequest, response.StatusCode, "a signature the batch route cannot verify is refused: %s", raw)

		required := daemon.NewServer(daemon.ServerConfig{DB: store, FederationBatchPoll: true, FederationSigningRequired: true})
		defer func() { require.NoError(t, required.Close()) }()
		server := httptest.NewServer(required.Handler())
		defer server.Close()
		required2 := optional
		required2.server = server
		response, raw = required2.batch(t, batchPollCursors(0, optional.visible.ID))
		assert.Equal(t, http.StatusNotFound, response.StatusCode, string(raw))
	})
}

func TestFederationBatchPollIsOffUnlessEnabled(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newBatchPollFixture(t, store, daemon.ServerConfig{})
		disabled := daemon.NewServer(daemon.ServerConfig{DB: store})
		defer func() { require.NoError(t, disabled.Close()) }()
		server := httptest.NewServer(disabled.Handler())
		defer server.Close()
		f.server = server

		response, raw := f.single(t, f.visible.ID, 0)
		require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		assert.NotContains(t, strings.Split(response.Header.Get(db.EventFeaturesHeader), ","), db.FederationBatchPollFeature)
		response, raw = f.batch(t, batchPollCursors(0, f.visible.ID, f.other.ID))
		assert.Equal(t, http.StatusNotFound, response.StatusCode, string(raw))
	})
}
