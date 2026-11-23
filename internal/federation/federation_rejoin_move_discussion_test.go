package federation

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/testenv"
)

func TestRejoinMoveWithOlderDiscussion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published bool
		pushOnly  bool
		restored  bool
		offline   bool
	}{
		{name: "unpublished"},
		{name: "published", published: true},
		{name: "push only", published: true, pushOnly: true},
		{name: "former instance", published: true, restored: true},
		{name: "hub history offline", published: true, offline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "discussion"})
			require.NoError(t, err)
			if tc.published {
				require.NoError(t, spoke.sync(t, 0))
			}
			if tc.restored {
				records, err := dbtest.CollectImportRecords(ctx, spoke.env.DB, db.ExportFilter{IncludeDeleted: true})
				require.NoError(t, err)
				restored := testenv.New(t)
				require.NoError(t, restored.DB.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
				spoke.env = restored
			}
			capabilities := "push,pull,claim"
			if tc.pushOnly {
				capabilities = "push,claim"
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, capabilities)
			_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[1].ID)
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull,claim", Actor: "new-actor"})
			require.NoError(t, err)
			_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[1].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
			require.NoError(t, err)
			spoke.credentials[projects[1].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "new-actor"}
			if tc.pushOnly {
				require.NoError(t, pushRejoin(t, spoke))
			} else {
				require.NoError(t, spoke.sync(t, 0))
			}
			if tc.offline {
				base, err := url.Parse(hub.URL)
				require.NoError(t, err)
				reverse := httputil.NewSingleHostReverseProxy(base)
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/events") && r.URL.Query().Get("issue_uid") != "" {
						http.Error(w, "offline", http.StatusServiceUnavailable)
						return
					}
					reverse.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				for _, project := range projects {
					binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
					require.NoError(t, err)
					binding.HubURL = proxy.URL
					_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
					require.NoError(t, err)
					credential := spoke.credentials[project.UID]
					credential.HubURL = proxy.URL
					spoke.credentials[project.UID] = credential
				}
			}
			t.Setenv("KATA_HOME", spoke.env.Home)
			for _, project := range projects {
				credential := spoke.credentials[project.UID]
				credential.Capabilities = "push,pull,claim"
				if project.UID == projects[0].UID {
					credential.Capabilities = capabilities
				}
				require.NoError(t, config.WriteFederationCredential(project.UID, credential))
			}
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			body := []byte(fmt.Sprintf(`{"actor":"new-actor","to_project_uid":%q}`, projects[1].UID))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.UID), bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
			resp, err := spoke.env.HTTP.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if tc.offline {
				require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
				return
			}
			if !tc.published || tc.pushOnly {
				require.Equal(t, http.StatusConflict, resp.StatusCode, string(raw))
				var envelope api.ErrorEnvelope
				require.NoError(t, json.Unmarshal(raw, &envelope))
				require.Equal(t, "federated_move_unsupported", envelope.Error.Code)
				return
			}
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			require.NoError(t, spoke.sync(t, 1))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, remote.ProjectID)
		})
	}
}
