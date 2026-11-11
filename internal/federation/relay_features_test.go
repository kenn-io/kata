package federation

import (
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

func TestRelayClientRefusesUnsupportedBatchBeforePost(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(t.Context(), srv.URL, "fixture-token", clientpkg.Opts{})
	require.NoError(t, err)
	event := db.RemoteEvent{Type: "cron.workflow.created", Payload: jsontext.Value(`{"definition":{"version":1,"steps":[{"key":"run","kind":"argv","argv":["echo"]}]}}`)}
	event.ContentHash, err = db.RemoteEventContentHash(event)
	require.NoError(t, err)
	body, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	batch := db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{{BindingUID: "binding-a", Body: body}}}
	ctx := db.WithRelayPeerFeatures(t.Context(), "binding-a", "")
	_, err = client.AcceptRelayDeliveries(ctx, 42, batch)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	require.Zero(t, posts)

	ctx = db.WithRelayPeerFeatures(t.Context(), "binding-b", db.CronWorkflowArgvEventFeature)
	_, err = client.AcceptRelayDeliveries(ctx, 42, batch)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Zero(t, posts)
}
