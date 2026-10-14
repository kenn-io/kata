package federation_test

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// R4/A6: durable native acceptance survives a lost HTTP reply or ACK.
// These are transport-fault checks; process-crash acceptance remains separate.
func TestRelayLostResponseRetainsDeliveryIdentity(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "company-member")
			personal := newRelayMatrixNode(t, backend, "personal-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, root.store.PinRootAuthority(t.Context(), pin))
			handler := root.http.Config.Handler
			failAccept, failAck := false, false
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failAccept && strings.HasSuffix(r.URL.Path, "/relay:accept") {
					failAccept = false
					committed := httptest.NewRecorder()
					handler.ServeHTTP(committed, r)
					if committed.Code != http.StatusOK {
						w.WriteHeader(committed.Code)
						_, _ = w.Write(committed.Body.Bytes())
						return
					}
					http.Error(w, "reply lost after durable commit", http.StatusServiceUnavailable)
					return
				}
				if failAck && strings.HasSuffix(r.URL.Path, "/relay:ack") {
					failAck = false
					http.Error(w, "ack unavailable", http.StatusServiceUnavailable)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)
			root.http = proxy // Selected origin before enrollment; tokens stay pinned here.
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			issue, source, err := personal.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: personal.project.ID, Author: "source-agent", Title: "Retry one immutable event"})
			require.NoError(t, err)
			offered, err := personal.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.Len(t, offered, 1)
			failAccept = true
			require.Error(t, federation.SyncFederationOnce(t.Context(), personal.store, binding, personal.credential))
			mirrored, err := root.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "company-member", mirrored.AccountableActor)
			proof, err := root.store.EntityAttribution(t.Context(), project.UID, "issue", issue.UID)
			require.NoError(t, err)
			require.Equal(t, source.UID, proof.EventUID)
			retry, err := personal.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.Equal(t, offered, retry, "lost root reply retains exact emitted bytes and IDs")
			rootClockBefore := []string{}
			for record, err := range root.store.ExportEvents(t.Context(), db.ExportFilter{ProjectID: &project.ID}) {
				require.NoError(t, err)
				rootClockBefore = append(rootClockBefore, record.UID)
			}
			syncRelayMatrixNode(t, personal)
			rootClockAfter := []string{}
			for record, err := range root.store.ExportEvents(t.Context(), db.ExportFilter{ProjectID: &project.ID}) {
				require.NoError(t, err)
				rootClockAfter = append(rootClockAfter, record.UID)
			}
			require.Equal(t, rootClockBefore, rootClockAfter, "receipt UI reset cannot cause relay metadata to emit a legacy replay baseline")
			retained, err := root.store.EntityAttribution(t.Context(), project.UID, "issue", issue.UID)
			require.NoError(t, err)
			require.Equal(t, proof, retained, "replay does not remint the root receipt")
			comments, commentSource, err := root.store.CreateComment(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateCommentParams{IssueID: mirrored.ID, Author: root.account, Body: "Pull commit before missing ACK"})
			require.NoError(t, err)
			failAck = true
			require.Error(t, federation.SyncFederationOnce(t.Context(), personal.store, binding, personal.credential))
			localComments, err := personal.store.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, localComments, 1, "local commit survives a missing remote ACK")
			require.Equal(t, comments.UID, localComments[0].UID)
			syncRelayMatrixNode(t, personal)
			localComments, err = personal.store.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, localComments, 1, "replayed pull is idempotent")
			rootCommentProof, err := personal.store.EntityAttribution(t.Context(), project.UID, "comment", comments.UID)
			require.NoError(t, err)
			require.Equal(t, commentSource.UID, rootCommentProof.EventUID)
			require.NoError(t, db.VerifyRootReceipt(pin, rootCommentProof))
		})
	}
}
