package daemon_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/testenv"
)

// A missing outbound header loses the caller's attribution at the authoritative
// hub. Pulling the audit event must retain the hub's bytes and content hash.
func TestForwardedTimedClaimCarriesTeammate(t *testing.T) {
	for _, action := range []string{"acquire", "renew", "release"} {
		t.Run(action, func(t *testing.T) {
			hub, spoke, hp, sp, issue, token := createClaimForwardingPair(t, "claim,pull")
			credential := config.FederationCredential{HubURL: hub.URL, HubProjectID: hp.ID, Token: token, Capabilities: "claim,pull"}
			require.NoError(t, config.WriteFederationCredential(sp.UID, credential))
			body := map[string]any{"holder": "request-holder", "client_kind": "cli", "claim_kind": "timed", "ttl_seconds": 300, "purpose": "edit"}
			if action != "acquire" {
				var seed api.ClaimActionResponseBody
				require.Equal(t, http.StatusOK, claimPost(t, spoke, sp.ID, issue.ShortID, "acquire", body, nil, &seed).StatusCode)
				require.True(t, seed.Granted)
			}
			if action == "renew" {
				body["ttl_seconds"] = 600
			}
			before, err := hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: hp.ID, IssueUID: issue.UID, Limit: 100})
			require.NoError(t, err)
			var out api.ClaimActionResponseBody
			response := claimPost(t, spoke, sp.ID, issue.ShortID, action, body, map[string]string{"X-Kata-Teammate": "codex"}, &out)
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.True(t, out.Granted)
			if action == "renew" {
				require.Nil(t, out.Event)
				require.NotNil(t, out.Lease)
				require.Equal(t, int64(2), out.Lease.Revision)
				after, err := hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: hp.ID, IssueUID: issue.UID, Limit: 100})
				require.NoError(t, err)
				require.Equal(t, before, after, "lease renewals update lease state without a new audit event")
				cached, err := spoke.DB.ClaimStatusReadOnly(t.Context(), sp.ID, issue.UID, time.Now().UTC())
				require.NoError(t, err)
				require.Equal(t, out.Lease.Revision, cached.Claim.Revision)
				return
			}
			require.NotNil(t, out.Event)
			require.Equal(t, "tester", out.Holder.Holder, "attribution must not replace the enrollment actor")
			require.Equal(t, spoke.DB.InstanceUID(), out.Holder.HolderInstanceUID)
			require.Equal(t, "codex", out.Event.Teammate)
			binding, err := spoke.DB.FederationBindingByProject(t.Context(), sp.ID)
			require.NoError(t, err)
			require.NoError(t, federation.SyncFederationOnce(t.Context(), spoke.DB, binding, credential))
			requireMirroredTeammateEvent(t, hub, spoke, hp.ID, sp.ID, *out.Event, "codex")
		})
	}
}

func TestForwardedTimedAssignmentCarriesTeammate(t *testing.T) {
	hub, spoke, hp, sp, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(sp.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: hp.ID, Token: token, Capabilities: "claim"}))
	for i, handle := range []string{"codex", "reviewer-2", ""} {
		response, raw := envDoRaw(t, spoke, http.MethodPost, issuePathRef(sp.ID, issue.ShortID, "actions/claim"),
			map[string]any{"actor": "tester", "ttl_seconds": 300 + i}, map[string]string{"X-Kata-Teammate": handle})
		require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		var out api.ClaimResponseBody
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Len(t, out.Events, 1)
		require.Equal(t, "tester", out.Events[0].Actor)
		requireMirroredTeammateEvent(t, hub, spoke, hp.ID, sp.ID, out.Events[0], handle)
	}
}

func TestForwardedTimedClaimAbsentTeammateStaysUntagged(t *testing.T) {
	hub, spoke, hp, sp, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(sp.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: hp.ID, Token: token, Capabilities: "claim"}))
	var out api.ClaimActionResponseBody
	response := claimPost(t, spoke, sp.ID, issue.ShortID, "acquire", map[string]any{"holder": "tester", "claim_kind": "timed", "ttl_seconds": 300}, nil, &out)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.True(t, out.Granted)
	require.NotNil(t, out.Event)
	require.Empty(t, out.Event.Teammate)
}

func TestForwardedClaimGateExpiryKeepsSystemAttribution(t *testing.T) {
	for _, request := range []string{"status", "show", "mutation_gate"} {
		t.Run(request, func(t *testing.T) {
			hub, spoke, hp, sp, issue, token := createClaimForwardingPair(t, "claim,pull,push")
			credential := config.FederationCredential{HubURL: hub.URL, HubProjectID: hp.ID, Token: token, Capabilities: "claim,pull,push"}
			require.NoError(t, config.WriteFederationCredential(sp.UID, credential))
			_, err := hub.DB.AcquireClaim(t.Context(), db.AcquireClaimParams{ProjectID: hp.ID, IssueRef: issue.UID,
				Principal: db.ClaimPrincipal{HolderInstanceUID: spoke.DB.InstanceUID(), Holder: "tester", ClientKind: "cli"},
				ClaimKind: "timed", TTL: time.Minute, Now: time.Now().UTC().Add(-2 * time.Minute)})
			require.NoError(t, err)
			method, path := http.MethodGet, claimStatusPath(sp.ID, issue.ShortID)
			var body any
			switch request {
			case "show":
				path = issuePathRef(sp.ID, issue.ShortID, "")
			case "mutation_gate":
				method, path = http.MethodPost, issuePathRef(sp.ID, issue.ShortID, "actions/assign")
				body = map[string]any{"actor": "tester", "owner": "tester"}
			}
			response, raw := envDoRaw(t, spoke, method, strings.TrimSuffix(path, "/"), body, map[string]string{"X-Kata-Teammate": "codex"})
			require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
			events, err := hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: hp.ID, IssueUID: issue.UID, Types: []string{"claim.expired"}, Limit: 10})
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, "system", events[0].Actor)
			require.Empty(t, events[0].Teammate)
			binding, err := spoke.DB.FederationBindingByProject(t.Context(), sp.ID)
			require.NoError(t, err)
			require.NoError(t, federation.SyncFederationOnce(t.Context(), spoke.DB, binding, credential))
			requireMirroredTeammateEvent(t, hub, spoke, hp.ID, sp.ID, events[0], "")
		})
	}
}

func requireMirroredTeammateEvent(t *testing.T, hub, spoke *testenv.Env, hubProjectID, spokeProjectID int64, response db.Event, teammate string) {
	t.Helper()
	hubEvents, err := hub.DB.EventsByUIDs(t.Context(), hubProjectID, []string{response.UID})
	require.NoError(t, err)
	require.Len(t, hubEvents, 1)
	spokeEvents, err := spoke.DB.EventsByUIDs(t.Context(), spokeProjectID, []string{response.UID})
	require.NoError(t, err)
	require.Len(t, spokeEvents, 1)
	require.Equal(t, teammate, hubEvents[0].Teammate)
	require.Equal(t, teammate, spokeEvents[0].Teammate)
	require.NotEmpty(t, hubEvents[0].ContentHash)
	require.Equal(t, hubEvents[0].ContentHash, spokeEvents[0].ContentHash)
	require.Equal(t, hubEvents[0].Payload, spokeEvents[0].Payload)
	require.Equal(t, hubEvents[0].ContentHash, response.ContentHash)
}
