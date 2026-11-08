package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/testenv"
)

// Interactive native job edits use the spoke's ordinary authenticated
// forwarding path, while the hub remains the authoritative definition writer.
func TestCronEditNativeSpokeForwarding(t *testing.T) {
	hub := testenv.New(t)
	project, err := hub.DB.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = hub.DB.EnableProjectFederation(t.Context(), project.ID, "tester")
	require.NoError(t, err)
	parent, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Periodic work", Author: "tester"})
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Inspection"}, Overlap: "allow", Catchup: "all"}
	id := cronCLIUID(t)
	original, _, err := hub.DB.PutCronJob(t.Context(), db.PutCronJob{UID: id, ProjectID: project.ID, Name: "Inspection", Definition: definition, Actor: "tester"})
	require.NoError(t, err)
	spoke := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.FederationPushPending = federation.DirectSpokePushPending(cfg.DB)
		cfg.FederationHubClient = federation.CredentialHTTPClient
	})
	localProject, err := spoke.DB.CreateProjectWithUID(t.Context(), "spoke-project", project.UID)
	require.NoError(t, err)
	enrollment, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "push,pull", Actor: "tester"})
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: localProject.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "tester", Enabled: true, PushEnabled: true, ReplayHorizonEventID: 1})
	require.NoError(t, err)
	credential := config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: enrollment.Token, Actor: "tester", Capabilities: "push,pull"}
	require.NoError(t, config.WriteFederationCredential(localProject.UID, credential))
	require.NoError(t, federation.SyncFederationOnce(t.Context(), spoke.DB, binding, credential))
	local, err := spoke.DB.CronJob(t.Context(), localProject.ID, id)
	require.NoError(t, err)
	require.Equal(t, original.DefinitionEventUID, local.DefinitionEventUID)
	cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) { return cronEditTestRename(raw, "Edited through spoke"), nil })
	// The edit PUT must outlive the ordinary read budget, just as update does.
	t.Setenv("KATA_HTTP_TIMEOUT", "1s")
	target, err := url.Parse(spoke.URL)
	require.NoError(t, err)
	forward := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// Preserve the direct loopback fixture's authentication path. Rewrite
			// does not add forwarding evidence that would change the principal.
			r.SetURL(target)
		},
	}
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			timer := time.NewTimer(1500 * time.Millisecond) //nolint:kennlint // deadlinetest: a forwarded write must outlive the ordinary 1s read budget.
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(delayed.Close)
	out, _, err := executeCronCLI(t, delayed.URL, nil, "job", "edit", id, "--as", "tester")
	require.NoError(t, err)
	require.Contains(t, out, "Edited through spoke")
	updated, err := hub.DB.CronJob(t.Context(), project.ID, id)
	require.NoError(t, err)
	require.Equal(t, "Edited through spoke", updated.Name)
	require.NotEqual(t, original.DefinitionEventUID, updated.DefinitionEventUID)
	local, err = spoke.DB.CronJob(t.Context(), localProject.ID, id)
	require.NoError(t, err)
	require.Equal(t, original.DefinitionEventUID, local.DefinitionEventUID, "forwarded response does not pretend the replica pulled")
}
