package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestClaimGatePendingMoveUsesCachedLease(t *testing.T) {
	for _, competing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unclaimed", true: "competing lease"}[competing], func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, issue := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "issue_not_found", "message": "not synchronized yet"}})
			}))
			t.Cleanup(hub.Close)
			enableClaimGateHelperSpoke(t, store, source, hub.URL, 99)
			enableClaimGateHelperSpoke(t, store, target, hub.URL, 100)
			moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "agent"})
			require.NoError(t, err)
			if competing {
				now := time.Now().UTC()
				principal := db.ClaimPrincipal{HolderInstanceUID: store.InstanceUID(), Holder: "other"}
				require.NoError(t, store.ApplyClaimStatus(t.Context(), target.ID, issue.UID, db.ClaimStatus{Held: true, Holder: principal, HubNow: now, Claim: &db.IssueClaim{ClaimUID: newClaimGateHelperUID(t), IssueUID: issue.UID, HolderInstanceUID: principal.HolderInstanceUID, Holder: principal.Holder, ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1}}))
			}
			err = requireFederatedIssueClaim(t.Context(), ServerConfig{DB: store}, target.ID, moved.Issue, "agent")
			if competing {
				assertClaimGateHelperAPIError(t, err, http.StatusConflict, "claim_denied")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestFederatedMovedIssueLeaseForwardsCurrentBindingAndUID(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	claimUID := newClaimGateHelperUID(t)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/100/issues/"+issue.UID+"/lease/actions/acquire" || r.Header.Get("Authorization") != "Bearer destination-token" {
			http.Error(w, "wrong current project transport", http.StatusBadRequest)
			return
		}
		var body struct {
			Holder string `json:"holder"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Holder != "destination-worker" {
			http.Error(w, "wrong bound principal", http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		_ = json.NewEncoder(w).Encode(map[string]any{"granted": true, "holder": map[string]any{"holder_instance_uid": store.InstanceUID(), "holder": "destination-worker", "client_kind": "cli"}, "lease": map[string]any{"claim_uid": claimUID, "issue_uid": issue.UID, "holder_instance_uid": store.InstanceUID(), "holder": "destination-worker", "client_kind": "cli", "claim_kind": "hard", "acquired_at": now, "updated_at": now, "revision": 1}})
	}))
	t.Cleanup(hub.Close)
	enableClaimGateHelperSpoke(t, store, source, hub.URL, 99)
	enableClaimGateHelperSpoke(t, store, target, hub.URL, 100)
	binding, err := store.FederationBindingByProject(t.Context(), target.ID)
	require.NoError(t, err)
	binding.Actor = "destination-worker"
	_, err = store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(target.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: 100, Token: "destination-token", Capabilities: "claim", Actor: "destination-worker"}))
	_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "agent"})
	require.NoError(t, err)
	daemon := NewServer(ServerConfig{DB: store})
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })
	local := httptest.NewServer(daemon.Handler())
	t.Cleanup(local.Close)
	response, err := local.Client().Post(fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/lease/actions/acquire", local.URL, source.ID, issue.ShortID), "application/json", bytes.NewBufferString(`{"holder":"local-worker","client_kind":"cli","claim_kind":"hard"}`))
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	cached, err := store.ClaimStatusReadOnly(t.Context(), target.ID, issue.UID, time.Now())
	require.NoError(t, err)
	require.True(t, cached.Held)
	require.Equal(t, "destination-worker", cached.Holder.Holder)
}
