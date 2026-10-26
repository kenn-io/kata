package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestScopedEventDisplayContribution(t *testing.T) {
	for _, kind := range []string{"issue.created", "issue.updated", "issue.commented", "issue.closed", "issue.links_changed", "issue.moved"} {
		t.Run(kind, func(t *testing.T) {
			payload, ok := scopedEventPayload(db.Event{Type: kind, Payload: `{"attributed_teammate":"worker-1","secret":"hidden-peer"}`})
			require.True(t, ok)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(payload), &got))
			require.Equal(t, "worker-1", got["attributed_teammate"])
			require.NotContains(t, got, "secret")
		})
	}
}

func TestClaimForwardDisplayContribution(t *testing.T) {
	var requests []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.Header.Get("X-Kata-Teammate"))
		require.Equal(t, "Bearer enrollment-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"granted":true}`))
	}))
	defer hub.Close()
	client, err := newClaimHubClient(t.Context(), hub.URL, "enrollment-token", false, nil)
	require.NoError(t, err)
	ctx := db.WithEventTeammate(t.Context(), "worker-1")
	_, err = client.AcquireClaim(ctx, 42, "abc1", api.ClaimActionBody{})
	require.NoError(t, err)
	_, err = client.ClaimIssue(ctx, 42, "abc1", api.ClaimRequestBody{})
	require.NoError(t, err)
	_, err = client.ClaimStatus(ctx, 42, "abc1")
	require.NoError(t, err)
	require.Equal(t, []string{"POST worker-1", "POST worker-1", "GET "}, requests)
}

// Every request owns its contribution, including requests with no header.
func TestEventTeammateShadowsParent(t *testing.T) {
	for _, tc := range []struct{ name, method, header, want string }{
		{"absent", "POST", "", ""}, {"read", "GET", "worker-2", ""}, {"override", "POST", "worker-2", "worker-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(db.WithEventTeammate(t.Context(), "parent-worker"))
			defer cancel()
			req := httptest.NewRequest(tc.method, "/", nil).WithContext(ctx)
			if tc.header != "" {
				req.Header.Set("X-Kata-Teammate", tc.header)
			}
			called := false
			withEventTeammate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				require.Equal(t, tc.want, db.EventTeammateFromContext(r.Context()))
				cancel()
				require.ErrorIs(t, r.Context().Err(), context.Canceled)
			})).ServeHTTP(httptest.NewRecorder(), req)
			require.True(t, called)
		})
	}
}
