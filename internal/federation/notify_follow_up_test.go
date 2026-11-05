package federation_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

// A combined follow-up travels leaf -> relay -> root and survives replay.
func TestNotifyFollowUpRelayDelivery(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRecoveryRoot(t, backend)
			issue, _, err := root.store.CreateIssue(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Title: "Await decision", Author: root.account, Owner: new("waiting-owner")})
			require.NoError(t, err)
			relay := newRelayMatrixNode(t, backend, "relay-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			enrollRelayMatrixReplica(t, root, relay, "relay-project", true)
			syncRelayMatrixNode(t, relay)
			enrollRelayMatrixReplica(t, relay, leaf, "leaf-project", false)
			syncRelayMatrixNode(t, leaf)
			status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "cli")
			require.Equal(t, 200, status)
			require.True(t, acquired.Granted)
			local, err := leaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			body := []byte(`{"to":"alex-outside","message":"Please decide","scheduled_on":"2099-11-10"}`)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/notifications", leaf.http.URL, leaf.project.ID, local.ShortID), bytes.NewReader(body))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+leaf.userToken)
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			raw, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, 200, response.StatusCode, string(raw))
			now := time.Date(2099, 11, 10, 0, 0, 0, 0, time.UTC)
			// Enabled spokes leave reminder generation to the root.
			sweeper := daemon.NewDueNotificationSweeper(leaf.store, daemon.NewEventPublisher(daemon.NewEventBroadcaster(), nil), "UTC")
			require.NoError(t, sweeper.RunOnce(t.Context(), now))
			local, err = leaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.NotContains(t, string(local.Metadata), notification.MetadataKey("waiting-owner"))
			for range 2 { // A second synchronization must neither lose nor duplicate the mutation.
				syncRelayMatrixNode(t, leaf)
				syncRelayMatrixNode(t, relay)
			}
			for _, node := range []*relayMatrixNode{root, relay, leaf} {
				saved, err := node.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				var values map[string]any
				require.NoError(t, json.Unmarshal([]byte(saved.Metadata), &values))
				require.Equal(t, "2099-11-10", values["scheduled_on"])
				require.Equal(t, map[string]any{"from": leaf.account, "message": "Please decide"}, values[notification.MetadataKey("alex-outside")])
				events, err := node.store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: node.project.ID, IssueUID: issue.UID, Limit: 100})
				require.NoError(t, err)
				foldEvents := make([]db.FoldEvent, 0, len(events))
				for _, event := range events {
					var eventIssueUID string
					if event.IssueUID != nil {
						eventIssueUID = *event.IssueUID
					}
					foldEvents = append(foldEvents, db.FoldEvent{UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, IssueUID: eventIssueUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: jsontext.Value(event.Payload)})
				}
				projection := db.FoldEvents(foldEvents)
				require.JSONEq(t, string(saved.Metadata), string(projection.IssueMetadata[issue.UID]))
			}
			rootSweeper := daemon.NewDueNotificationSweeper(root.store, daemon.NewEventPublisher(daemon.NewEventBroadcaster(), nil), "UTC")
			require.NoError(t, rootSweeper.RunOnce(t.Context(), now))
			saved, err := root.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Contains(t, string(saved.Metadata), notification.MetadataKey("waiting-owner"))
		})
	}
}
