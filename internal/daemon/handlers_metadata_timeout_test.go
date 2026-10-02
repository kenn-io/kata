package daemon_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Ordinary spoke edits are local-first: transport failure falls back to cached
// lease state. A stalled hub must leave time to check that state and commit the
// edit within the CLI's request budget, including when response headers arrive.
func TestPatchIssueMetadataBoundsStalledHubLeaseRefresh(t *testing.T) {
	for _, stall := range []string{"headers", "body", "forbidden body", "canceled parent"} {
		for _, heldByOther := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/other-holder=%t", stall, heldByOther), func(t *testing.T) {
				env := testenv.New(t)
				project, issue := createShowClaimSpokeProject(t, env)
				canceled := make(chan struct{})
				cancelFromHub := make(chan context.CancelFunc, 1)
				hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stall == "body" || stall == "forbidden body" {
						w.Header().Set("Content-Type", "application/json")
						status := http.StatusOK
						if stall == "forbidden body" {
							status = http.StatusForbidden
						}
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"held":`)
						w.(http.Flusher).Flush()
					}
					if stall == "canceled parent" {
						// Cancel only after the hub receives the request, so the
						// daemon is already inside the refresh under test.
						(<-cancelFromHub)()
					}
					<-r.Context().Done()
					close(canceled)
				}))
				t.Cleanup(hub.Close)
				_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{
					ProjectID: project.ID, Role: db.FederationRoleSpoke,
					HubURL: hub.URL, HubProjectID: 42, HubProjectUID: project.UID,
					PushEnabled: true, Enabled: true, Actor: "tester",
				})
				require.NoError(t, err)
				require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
					HubURL: hub.URL, HubProjectID: 42, Token: "example-lease-token", Actor: "tester",
				}))
				if heldByOther {
					at := time.Now().UTC()
					require.NoError(t, env.DB.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{
						Held: true, Claim: showIssueCachedClaim(issue, "other-holder", at), HubNow: at,
						Holder: db.ClaimPrincipal{
							Holder: "other-holder", HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD", ClientKind: "cli",
						},
					}))
				}

				ctx, cancel := context.WithTimeout(t.Context(), client.DefaultHTTPTimeout)
				defer cancel()
				cancelFromHub <- cancel
				path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata", env.URL, project.ID, issue.ShortID)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, path,
					strings.NewReader(`{"actor":"tester","patch":{"custom":"updated"}}`))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				resp, err := env.HTTP.Do(req) //nolint:gosec // disposable loopback daemon
				if stall == "canceled parent" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err, "stalled lease refresh exhausted the local mutation budget")
					defer func() { _ = resp.Body.Close() }()
					raw, err := io.ReadAll(resp.Body)
					require.NoError(t, err)
					if stall == "forbidden body" {
						assertAPIError(t, resp.StatusCode, raw, http.StatusForbidden, "hub_claim_failed")
					} else if heldByOther {
						assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
					} else {
						require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
					}
				}
				current, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				if heldByOther || stall == "forbidden body" || stall == "canceled parent" {
					assert.JSONEq(t, string(issue.Metadata), string(current.Metadata))
				} else {
					assert.JSONEq(t, `{"custom":"updated"}`, string(current.Metadata))
				}
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("stalled hub request was not canceled")
				}
			})
		}
	}
}
