package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

func TestNotifyRelayPreservesSourceActor(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		credentials := newReplicaCredentialStore()
		params := relayReplicaParams(t, store)
		params.HubURL = "http://127.0.0.1:1"
		params.Credential.HubURL = params.HubURL
		replica, err := daemon.EnsureFederationReplica(ctx, store, credentials, nil, params)
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: replica.Project.ID, Title: "Review finding", Author: "source-assistant", Owner: new("sync-agent"),
		})
		require.NoError(t, err)
		for _, actor := range []string{"source-assistant", "reader"} {
			_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: actor, Body: "Finding"})
			require.NoError(t, err)
		}
		d := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now(), FederationCredentials: credentials})
		t.Cleanup(func() { _ = d.Close() })
		server := httptest.NewServer(d.Handler())
		t.Cleanup(server.Close)

		response, body := postJSON(t, server,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", replica.Project.ID, issue.ShortID),
			map[string]any{"actor": "source-assistant", "broadcast": true, "message": "Inspect the finding"})
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
		var result struct {
			Recipients []string `json:"recipients"`
			Event      db.Event `json:"event"`
		}
		require.NoError(t, json.Unmarshal(body, &result))
		assert.Equal(t, []string{"reader", "sync-agent"}, result.Recipients,
			"a relay broadcast excludes its source actor, not its upstream account")
		assert.Equal(t, "source-assistant", result.Event.Actor)

		current, err := store.IssueByID(ctx, issue.ID)
		require.NoError(t, err)
		var slots map[string]notification.Value
		require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
		assert.Equal(t, "source-assistant", slots[notification.MetadataKey("reader")].From,
			"the stored notification retains the same source actor as its event")
	})
}
