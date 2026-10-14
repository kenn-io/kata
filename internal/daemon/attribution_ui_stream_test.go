package daemon

import (
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// R5: an already-open idle stream refreshes late proof without another write.
func TestAttributionReceiptLiveHeartbeatReset(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "stream.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	_, event, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "assistant", Title: "Source issue"})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "personal-member", Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: "00000000000000000000000001", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(t.Context(), pin))
	before, err := store.UIEventCursor(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response := httptest.NewRecorder()
	messages := make(chan StreamMsg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLivePhase(ctx, livePhaseDeps{w: response, flusher: response, cfg: ServerConfig{DB: store}, ch: messages}, project.ID, before)
	}()
	proof, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, EventUID: event.UID, ContentHash: event.ContentHash, AccountableActor: "company-member", SourceActor: event.Actor, IngressInstanceUID: "00000000000000000000000002", AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1}, private)
	require.NoError(t, err)
	require.NoError(t, store.ApplyUpstreamAttribution(t.Context(), pin, proof))
	after, err := store.UIEventCursor(t.Context())
	require.NoError(t, err)
	require.Greater(t, after, before)
	select {
	case <-done:
	case <-time.After(heartbeatInterval + 5*time.Second):
		cancel()
		<-done
	}
	require.Contains(t, response.Body.String(), "event: sync.reset_required", "late proof must not leave an idle connected browser at old authority")
	require.Contains(t, response.Body.String(), string(resetFrameBytes(after)))
}
