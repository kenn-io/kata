package federation

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

// A new sender must not expose an origin reference to a receiver that predates
// the feature, even when that receiver cannot enforce a request header itself.
func TestDocbankOriginClientPreflight(t *testing.T) {
	for _, relay := range []bool{false, true} {
		for _, capable := range []bool{false, true} {
			t.Run(map[bool]string{false: "direct", true: "relay"}[relay]+map[bool]string{false: "/old", true: "/aware"}[capable], func(t *testing.T) {
				writes := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Contains(t, r.Header.Get(db.EventFeaturesHeader), "docbank_origin_v1")
					if r.Method == http.MethodGet {
						assert.Equal(t, "/api/v1/projects/42/federation/metadata", r.URL.Path)
						if capable {
							w.Header().Set(db.EventFeaturesHeader, "docbank_origin_v1")
						}
						_, _ = w.Write([]byte(`{}`))
						return
					}
					writes++
					_, _ = w.Write([]byte(`{}`))
				}))
				t.Cleanup(server.Close)
				client, err := NewClient(t.Context(), server.URL, "fixture-token", clientpkg.Opts{})
				require.NoError(t, err)
				payload := jsontext.Value(`{"metadata":{"attachment.id":{"origin":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}}}`)
				if relay {
					event := db.RemoteEvent{EventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", OriginInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", Type: "issue.created", Actor: "member", HLCPhysicalMS: 1, CreatedAt: time.Unix(1, 0).UTC(), Payload: payload}
					event.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, CreatedAt: event.CreatedAt.Format(db.EventTimestampFormat), Payload: event.Payload})
					require.NoError(t, err)
					var body []byte
					body, err = db.EncodeRelaySourceEvent(event)
					require.NoError(t, err)
					_, err = client.AcceptRelayDeliveries(t.Context(), 42, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{{Stream: db.RelayStreamEvent, Body: body}}})
				} else {
					_, err = client.IngestProjectEvents(t.Context(), 42, []api.FederationIngestEventEnvelope{{Type: "issue.created", Payload: payload}})
				}
				if capable {
					require.NoError(t, err)
					assert.Equal(t, 1, writes)
				} else {
					require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
					assert.Zero(t, writes, "old receiver must never receive origin-bearing bytes")
				}
			})
		}
	}
}

// Checkpoint sections are base64 byte arrays, not nested source JSON objects.
func TestDocbankOriginClientChecksResetPayload(t *testing.T) {
	// A malformed source is not a usable checkpoint, regardless of response headers.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		raw, err := json.Marshal(db.RelayResetCheckpoint{Snapshot: db.RootResetSnapshot{Events: []byte(`["aW52YWxpZA=="]`), Entities: []byte(`[]`)}})
		require.NoError(t, err)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(t.Context(), server.URL, "fixture-token", clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.RelayReset(t.Context(), 42)
	require.Error(t, err)
}
