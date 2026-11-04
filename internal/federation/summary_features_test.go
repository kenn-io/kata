package federation

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

func TestSummaryBatchFeatureUnion(t *testing.T) {
	for _, support := range []string{"cron_v1", "cron_run_summary_v2,cron_v1"} {
		for _, order := range []string{"v2-v1", "v1-v2", "v1-only"} {
			t.Run(support+"/"+order, func(t *testing.T) {
				posts := 0
				hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						w.Header().Set(db.EventFeaturesHeader, support)
						_, _ = fmt.Fprint(w, `{}`)
						return
					}
					posts++
					_, _ = fmt.Fprint(w, `{"accepted":2}`)
				}))
				t.Cleanup(hub.Close)
				client, err := NewClient(t.Context(), hub.URL, "fixture-token", clientpkg.Opts{})
				require.NoError(t, err)
				v1 := api.FederationIngestEventEnvelope{Type: "cron.run.observed", Payload: jsontext.Value(`{"summary":{"version":1}}`)}
				v2 := api.FederationIngestEventEnvelope{Type: "cron.run.observed", Payload: jsontext.Value(`{"summary":{"version":1,"reason":"failed"}}`)}
				events := []api.FederationIngestEventEnvelope{v2, v1}
				if order == "v1-v2" {
					events = []api.FederationIngestEventEnvelope{v1, v2}
				}
				if order == "v1-only" {
					events = []api.FederationIngestEventEnvelope{v1}
				}
				_, err = client.IngestProjectEvents(t.Context(), 1, events)
				if support == "cron_v1" && strings.Contains(order, "v2") {
					require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
					require.Zero(t, posts)
				} else {
					require.NoError(t, err)
					require.Equal(t, 1, posts)
				}
			})
		}
	}
}

func TestSummaryRelayOutboundFeatureGate(t *testing.T) {
	for _, support := range []string{"cron_v1", "cron_run_summary_v2,cron_v1"} {
		t.Run(support, func(t *testing.T) {
			posts := 0
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set(db.EventFeaturesHeader, support)
					_, _ = fmt.Fprint(w, `{}`)
					return
				}
				posts++
				_, _ = fmt.Fprint(w, `{"through":1}`)
			}))
			t.Cleanup(hub.Close)
			client, err := NewClient(t.Context(), hub.URL, "fixture-token", clientpkg.Opts{})
			require.NoError(t, err)
			body, err := json.Marshal(map[string]any{"event": map[string]any{"type": "cron.run.observed"}, "payload": []byte(`{"summary":{"version":1,"steps":[]}}`)})
			require.NoError(t, err)
			_, err = client.AcceptRelayDeliveries(t.Context(), 1, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{{Stream: db.RelayStreamEvent, Body: body}}})
			if support == "cron_v1" {
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				require.Zero(t, posts)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, posts)
			}
		})
	}
}

// Preflight must inspect every structured summary before sending the batch,
// even after an earlier envelope already required v2.
func TestSummaryRelayValidatesWholeBatch(t *testing.T) {
	posts := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
			_, _ = fmt.Fprint(w, `{}`)
			return
		}
		posts++
		_, _ = fmt.Fprint(w, `{"through":2}`)
	}))
	t.Cleanup(hub.Close)
	client, err := NewClient(t.Context(), hub.URL, "fixture-token", clientpkg.Opts{})
	require.NoError(t, err)
	var envelopes []db.RelayEnvelope
	for _, summary := range []string{`{"version":1,"steps":[]}`, `{"version":1,"reason":"INVALID"}`} {
		body, err := json.Marshal(map[string]any{"event": map[string]any{"type": "cron.run.observed"}, "payload": []byte(`{"summary":` + summary + `}`)})
		require.NoError(t, err)
		envelopes = append(envelopes, db.RelayEnvelope{Stream: db.RelayStreamEvent, Body: body})
	}
	_, err = client.AcceptRelayDeliveries(t.Context(), 1, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: envelopes})
	require.Error(t, err)
	require.Zero(t, posts)
}
