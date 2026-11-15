package daemon_test

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testfix"
)

func TestFederationBridgePreflightCrossLinkFeatures(t *testing.T) {
	localStore, err := testfix.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, localStore.Close()) })
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
}
