package federation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

func TestSupplementalProofStaysOnExactMount(t *testing.T) {
	for _, actor := range []string{"worker", "peer-member"} {
		t.Run(actor, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "source", "peer", "unrelated")
			spoke := newMoveDeliverySpoke(t, hub, projects, "worker", actor, "unrelated-member")
			peer, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "Peer", Author: actor})
			require.NoError(t, err)
			_, ev, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: peer.ID}}})
			require.NoError(t, err)
			binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
			require.NoError(t, err)
			tokens, err := federationMoveSourceTokens(t.Context(), spoke.env.DB, spoke.credentials, binding, hub.URL, []db.Event{ev}, clientpkg.Opts{})
			require.NoError(t, err)
			require.Len(t, tokens, 1)
			require.True(t, tokens[projects[1].UID] == spoke.credentials[projects[1].UID].Token)
			tokens, err = federationMoveSourceTokens(t.Context(), spoke.env.DB, spoke.credentials, binding, hub.URL+"/other", []db.Event{ev}, clientpkg.Opts{})
			require.NoError(t, err)
			require.Zero(t, len(tokens))
		})
	}
}

func TestLinkPeerProofUsesOwnPlaintextPolicy(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "route", "peer")
	spoke := newMoveDeliverySpoke(t, hub, projects, "route-member", "peer-member")
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "Parent", Author: "peer-member"})
	require.NoError(t, err)
	_, event, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Child", Author: "route-member", Links: []db.InitialLink{{Type: "parent", ToNumber: peer.ID}}})
	require.NoError(t, err)

	const target = "http://daemon.example"
	t.Setenv("KATA_SERVER", target)
	t.Setenv("KATA_ALLOW_INSECURE", "1")
	var route db.FederationBinding
	for i, project := range spoke.projects {
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		binding.HubURL = target
		binding.AllowInsecure = i == 0
		binding, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		if i == 0 {
			route = binding
		}
	}
	uid := projects[1].UID
	credential := spoke.credentials[uid]
	selectProof := func() (map[string]string, error) {
		spoke.credentials[uid] = credential
		return federationMoveSourceTokens(ctx, spoke.env.DB, spoke.credentials, route, target, []db.Event{event}, clientpkg.Opts{AllowInsecure: true})
	}
	credential.HubURL = target
	tokens, err := selectProof()
	require.Error(t, err)
	require.Empty(t, tokens)

	credential.AllowInsecure = true
	tokens, err = selectProof()
	require.NoError(t, err)
	require.Equal(t, map[string]string{uid: credential.Token}, tokens)

	credential.HubURL = target + "/other"
	tokens, err = selectProof()
	require.Error(t, err)
	require.Empty(t, tokens)
}

// A batch carrying supplemental proofs must treat a redirect as the hub's
// response. Even a redirected GET would attach the primary bearer again.
func TestProofBearingIngestReturnsHubRedirect(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Redirect(w, r, "/mount/status", http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(hub.Close)
	client, err := NewClient(t.Context(), hub.URL+"/mount", "route-token", clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.IngestProjectEventsWithOptions(t.Context(), 1, nil, IngestProjectEventsOptions{MoveSourceTokens: map[string]string{"peer": "peer-token"}})
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusSeeOther, status.StatusCode)
}
