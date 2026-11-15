package daemon_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestFederationBridgePreflightCrossLinkFeatures(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, localStore db.Storage) {
		ctx := t.Context()
		hub := newBridgeHubFixture(t)
		peer, err := hub.store.CreateProject(ctx, "other-shared-project")
		require.NoError(t, err)
		_, err = hub.store.EnableProjectFederation(ctx, peer.ID, "admin")
		require.NoError(t, err)
		pin, err := hub.store.RootAuthority(ctx, hub.root.private.UID)
		require.NoError(t, err)
		pin.ProjectUID = peer.UID
		require.NoError(t, hub.store.PinRootAuthority(ctx, pin))
		source, _, err := hub.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hub.root.private.ID, Author: "member", Title: "Source"})
		require.NoError(t, err)
		target, _, err := hub.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peer.ID, Author: "member", Title: "Target"})
		require.NoError(t, err)
		_, _, err = hub.store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: source.ID, ToIssueID: target.ID, Type: "blocks", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: source.ID, Actor: "member", FromUID: source.UID, ToUID: target.UID, FromShortID: source.ShortID, ToShortID: target.ShortID})
		require.NoError(t, err)
		local := newBridgeLocalDaemon(t, localStore, hub.catalog(hub.root.server.URL))
		code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, true), bridgeOwnerHeaders)
		require.Equal(t, http.StatusOK, code, string(raw))
	})
}
