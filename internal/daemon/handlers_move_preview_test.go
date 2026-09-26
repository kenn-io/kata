package daemon_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMoveIssue_Preview(t *testing.T) {
	sink := &recordingSink{}
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) { cfg.Hooks = sink })
	src, tgt, iss := seedMovePair(t, env)
	cursor, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()
	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q,"dry_run":true}`, tgt.UID)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, "")
	raw := readClose(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Equal(t, `"rev-1"`, resp.Header.Get("ETag"))
	assert.Contains(t, string(raw), `"changed":false`)
	assert.Contains(t, string(raw), `"event_id":0`)
	assert.Contains(t, string(raw), `"new_short_id":""`)
	stored, err := env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	assert.Equal(t, iss, stored)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	assert.Equal(t, cursor, after)
	assert.Empty(t, sink.snapshot())
	select {
	case event := <-sub.Ch:
		t.Fatalf("preview broadcast %+v", event)
	default:
	}
}

func TestMoveIssue_PreviewValidation(t *testing.T) {
	cases := []struct {
		name, ifMatch string
		status        int
		prepare       func(*testing.T, *testenv.Env, db.Project, db.Project, db.Issue)
	}{
		{name: "valid revision", ifMatch: `"rev-1"`, status: 200},
		{name: "stale revision", ifMatch: `"rev-99"`, status: 412},
		{name: "malformed revision", ifMatch: `"bad"`, status: 400},
		{name: "source archived", status: 404, prepare: func(t *testing.T, e *testenv.Env, s, _ db.Project, _ db.Issue) {
			_, _, err := e.DB.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: s.ID, Actor: "tester", Force: true})
			require.NoError(t, err)
		}},
		{name: "target archived", status: 404, prepare: func(t *testing.T, e *testenv.Env, _, d db.Project, _ db.Issue) {
			_, _, err := e.DB.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: d.ID, Actor: "tester", Force: true})
			require.NoError(t, err)
		}},
		{name: "recurrence", status: 409, prepare: func(t *testing.T, e *testenv.Env, s, _ db.Project, i db.Issue) {
			r, _, err := e.DB.CreateRecurrence(t.Context(), db.CreateRecurrenceIn{ProjectID: s.ID, Actor: "tester", Rule: "FREQ=WEEKLY", DTStart: "2026-09-28", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "recurring"}})
			require.NoError(t, err)
			_, err = e.DB.ExecContext(t.Context(), `UPDATE issues SET recurrence_id=?, occurrence_key='2026-09-28' WHERE id=?`, r.ID, i.ID)
			require.NoError(t, err)
		}},
		{name: "federation", status: 409, prepare: func(t *testing.T, e *testenv.Env, s, _ db.Project, _ db.Issue) {
			_, err := e.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: s.ID, Role: db.FederationRoleHub, HubProjectUID: s.UID, Enabled: true})
			require.NoError(t, err)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			src, tgt, iss := seedMovePair(t, env)
			if c.prepare != nil {
				c.prepare(t, env, src, tgt, iss)
			}
			body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q,"dry_run":true}`, tgt.UID)
			resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, c.ifMatch)
			raw := readClose(t, resp)
			assert.Equal(t, c.status, resp.StatusCode, string(raw))
		})
	}
}

func TestMoveIssue_PreviewRequiresAuthentication(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)
	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q,"dry_run":true}`, tgt.UID)
	req, err := http.NewRequest(http.MethodPost, moveURL(env, src.ID, iss.ShortID), strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))
}

func TestMoveIssue_PreviewRejectsSameProject(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, _, iss := seedMovePair(t, env)
	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q,"dry_run":true}`, src.UID)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, "")
	raw := readClose(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), `"code":"same_project"`)
}

func TestMoveIssue_PreviewDoesNotWidenScopedTokenAuthority(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	src, tgt, iss := seedMovePair(t, env)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope:     &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: src.UID, RootIssueUID: iss.UID},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	body := fmt.Sprintf(`{"to_project_uid":%q,"dry_run":true}`, tgt.UID)
	req, err := http.NewRequest(http.MethodPost, moveURL(env, src.ID, iss.ShortID), strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer worker-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))
}
