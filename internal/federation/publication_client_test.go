package federation_test

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/jsonl"
)

const publicationTarget = "01K00000000000000000000006"

func clientPublicationEnvelope(t *testing.T) []byte {
	t.Helper()
	p := dbtest.PublicationFixture(t, publicationTarget)
	p.Digest = ""
	raw, err := jsonl.EncodeFederationPublication(p)
	require.NoError(t, err)
	return raw
}

// Unsupported hubs never receive a mutation; neither same-origin nor cross-
// origin redirects may receive the frozen envelope or its account credential.
func TestPublicationClientCapabilityAndRedirects(t *testing.T) {
	for _, mode := range []string{"old", "wrong-target", "redirect-instance", "redirect-publication", "cross-origin", "success", "preflight", "conflict", "unauthorized", "forbidden", "denied"} {
		t.Run(mode, func(t *testing.T) {
			posts, leaked := 0, 0
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { leaked++; w.WriteHeader(http.StatusInternalServerError) }))
			t.Cleanup(other.Close)
			envelope := clientPublicationEnvelope(t)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer account-fixture", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/instance":
					if mode == "redirect-instance" {
						http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
						return
					}
					version := 1
					if mode == "old" {
						version = 0
					}
					target := publicationTarget
					if mode == "wrong-target" {
						target = "01K00000000000000000000009"
					}
					raw, err := json.Marshal(map[string]any{"instance_uid": target, "relay_publication_version": version})
					require.NoError(t, err)
					_, _ = w.Write(raw)
				case "/api/v1/federation/publications":
					posts++
					raw, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Contains(t, string(raw), string(envelope), "transmit exact frozen bytes")
					if mode == "redirect-publication" {
						http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
						return
					}
					if mode == "cross-origin" {
						http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
						return
					}
					if mode == "conflict" {
						w.WriteHeader(409)
						_, _ = w.Write([]byte(`{"status":409,"error":{"code":"federation_publication_conflict","message":"private upstream diagnostic"}}`))
						return
					}
					if mode == "unauthorized" || mode == "forbidden" || mode == "denied" {
						status, code := 401, "unauthorized"
						if mode == "forbidden" {
							status, code = 403, "token_invalid"
						}
						if mode == "denied" {
							status, code = 404, "not_found"
						}
						w.WriteHeader(status)
						raw, err := json.Marshal(map[string]any{"status": status, "error": map[string]any{"code": code, "message": "private upstream diagnostic"}})
						require.NoError(t, err)
						_, _ = w.Write(raw)
						return
					}
					if mode == "preflight" {
						_, _ = w.Write([]byte(`{"status":"ready","project_uid":"01K00000000000000000000001","project_name":"published-project","account":"publisher","visibility":"all","counts":{"issues":2,"comments":2,"events":2},"replayed":false}`))
						return
					}
					_, _ = w.Write([]byte(`{"status":"published","project_uid":"01K00000000000000000000001","project_id":42,"project_name":"published-project","account":"publisher","visibility":"all","counts":{"issues":2,"comments":2,"events":2},"replayed":false}`))
				default:
					leaked++
					w.WriteHeader(500)
				}
			}))
			t.Cleanup(hub.Close)
			client, err := federation.NewHubClient(t.Context(), config.CatalogDaemonConfig{URL: hub.URL, Token: "account-fixture"})
			require.NoError(t, err)
			result, err := client.PublishFederationProject(t.Context(), envelope, mode == "preflight")
			if mode == "success" || mode == "preflight" {
				require.NoError(t, err)
				if mode == "preflight" {
					require.Zero(t, result.ProjectID)
				} else {
					require.Equal(t, int64(42), result.ProjectID)
				}
			} else {
				require.Error(t, err)
				if mode == "old" || mode == "wrong-target" || mode == "conflict" || mode == "unauthorized" || mode == "forbidden" || mode == "denied" {
					var apiErr *api.APIError
					require.True(t, errors.As(err, &apiErr))
					want := "federation_publication_conflict"
					if mode == "old" {
						want = "federation_publication_unsupported"
					}
					if mode == "unauthorized" {
						want = "unauthorized"
					}
					if mode == "forbidden" {
						want = "token_invalid"
					}
					if mode == "denied" {
						want = "not_found"
					}
					require.Equal(t, want, apiErr.Code)
				}
				require.NotContains(t, err.Error(), "private upstream diagnostic")
				require.NotContains(t, err.Error(), "account-fixture")
			}
			require.Zero(t, leaked)
			if mode == "old" || mode == "wrong-target" || mode == "redirect-instance" {
				require.Zero(t, posts)
			} else {
				require.Equal(t, 1, posts)
			}
			if mode == "redirect-instance" || mode == "redirect-publication" || mode == "cross-origin" {
				mode = "success"
				result, err := client.PublishFederationProject(t.Context(), envelope, false)
				require.NoError(t, err, "redirect refusal retains the original pinned origin")
				require.Equal(t, int64(42), result.ProjectID)
				require.Zero(t, leaked)
			}
		})
	}
}

// Local validation rejects arbitrary malformed envelopes before network access.
func FuzzPublicationClientRejectsMalformed(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if _, err := jsonl.DecodeFederationPublication(raw); err == nil {
			t.Skip("accepted codec documents use the success contract")
		}
		client, err := federation.NewHubClient(t.Context(), config.CatalogDaemonConfig{URL: "http://127.0.0.1:1", Token: "fixture"})
		require.NoError(t, err)
		_, err = client.PublishFederationProject(t.Context(), raw, false)
		var apiErr *api.APIError
		require.True(t, errors.As(err, &apiErr))
		require.Contains(t, []string{"federation_publication_invalid", "federation_publication_too_large"}, apiErr.Code)
	})
}
