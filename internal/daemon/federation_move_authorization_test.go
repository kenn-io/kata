package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

type moveSourceHostAccess struct {
	sourceUID, mode string
	sourceChecks    atomic.Int64
	finished        atomic.Bool
	afterRollback   func(context.Context) error
}

func (h *moveSourceHostAccess) AuthorizeFederation(_ context.Context, request daemon.HostFederationAccessRequest) (daemon.HostFederationAccessDecision, error) {
	fence := func(context.Context, db.Transaction) error { return nil }
	if request.Project.UID != h.sourceUID {
		return daemon.HostFederationAccessDecision{TransactionFence: fence}, nil
	}
	h.sourceChecks.Add(1)
	switch h.mode {
	case "admission-denied":
		return daemon.HostFederationAccessDecision{}, daemon.ErrHostAccessDenied
	case "limited":
		return daemon.HostFederationAccessDecision{}, daemon.ErrHostFederationAdmissionLimited
	case "missing-fence":
		return daemon.HostFederationAccessDecision{}, nil
	case "unavailable":
		return daemon.HostFederationAccessDecision{}, errors.New("source host unavailable")
	case "fence-denied":
		fence = func(context.Context, db.Transaction) error {
			return db.AfterTransactionRollback(daemon.ErrHostAccessDenied, func(ctx context.Context) error {
				if err := h.afterRollback(ctx); err != nil {
					return err
				}
				h.finished.Store(true)
				return nil
			})
		}
	}
	return daemon.HostFederationAccessDecision{TransactionFence: fence}, nil
}

// A move using ordinary per-project grants must obtain host authority for both
// endpoints, and an expanded source fence must reject the whole transaction.
func TestFederationMoveSourceHostAuthority(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		status int
	}{
		{"allow", 200}, {"global-grant", 200}, {"missing-proof", 403}, {"invalid-proof", 403}, {"proof-wrong-actor", 403}, {"proof-wrong-spoke", 403}, {"proof-pull-only", 403}, {"proof-revoked", 403}, {"admission-denied", 403}, {"fence-denied", 403},
		{"missing-fence", 503}, {"unavailable", 503}, {"limited", 429},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			access := &moveSourceHostAccess{mode: tc.mode}
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.HostFederationAccess = access })
			source, target, issue := seedMovePair(t, env)
			access.sourceUID = source.UID
			for _, project := range []db.Project{source, target} {
				_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "tester")
				require.NoError(t, err)
			}
			origin, err := uid.New()
			require.NoError(t, err)
			sourceGrant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "tester"})
			require.NoError(t, err)
			targetProjectID := &target.ID
			if tc.mode == "global-grant" {
				targetProjectID = nil
			}
			targetGrant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: targetProjectID, Capabilities: "push,pull", Actor: "tester"})
			require.NoError(t, err)
			require.NotEqual(t, sourceGrant.Enrollment.ID, targetGrant.Enrollment.ID)
			eventUID, err := uid.New()
			require.NoError(t, err)
			at := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
			payload, err := json.Marshal(map[string]any{
				"issue_uid": issue.UID, "from_project_uid": source.UID, "from_short_id": issue.ShortID,
				"to_project_uid": target.UID, "to_short_id": issue.ShortID, "updated_at": at.Format(time.RFC3339Nano),
				"snapshot": map[string]string{"uid": issue.UID, "title": issue.Title, "author": "tester", "short_id": issue.ShortID, "status": "open"},
			})
			require.NoError(t, err)
			remote := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: target.UID, ProjectName: target.Name, IssueUID: &issue.UID, Type: "issue.moved", Actor: "tester", Payload: jsontext.Value(payload), HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
			remote.ContentHash, err = db.RemoteEventContentHash(remote)
			require.NoError(t, err)
			body := api.FederationIngestEventsRequestBody{SchemaVersion: db.CurrentSchemaVersion(), Events: []api.FederationIngestEventEnvelope{{
				EventID: 1, EventUID: remote.EventUID, OriginInstanceUID: remote.OriginInstanceUID,
				ProjectUID: remote.ProjectUID, ProjectName: remote.ProjectName, IssueUID: remote.IssueUID,
				Type: remote.Type, Actor: remote.Actor, Payload: remote.Payload, HLCPhysicalMS: remote.HLCPhysicalMS,
				ContentHash: remote.ContentHash, CreatedAt: remote.CreatedAt,
			}}}
			cursor, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			access.afterRollback = func(context.Context) error {
				got, err := env.DB.IssueByID(t.Context(), issue.ID)
				if err == nil {
					require.Equal(t, source.ID, got.ProjectID)
				}
				return err
			}
			encoded, err := json.Marshal(body)
			require.NoError(t, err)
			var request map[string]any
			require.NoError(t, json.Unmarshal(encoded, &request))
			if tc.mode != "missing-proof" && tc.mode != "global-grant" {
				proof := sourceGrant.Token
				if tc.mode == "invalid-proof" {
					proof = targetGrant.Token
				}
				if strings.HasPrefix(tc.mode, "proof-") {
					params := db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "tester"}
					switch tc.mode {
					case "proof-wrong-actor":
						params.Actor = "other-actor"
					case "proof-wrong-spoke":
						params.SpokeInstanceUID, err = uid.New()
						require.NoError(t, err)
					case "proof-pull-only":
						params.Capabilities = "pull"
					}
					other, err := env.DB.CreateFederationEnrollment(t.Context(), params)
					require.NoError(t, err)
					if tc.mode == "proof-revoked" {
						require.NoError(t, env.DB.RevokeFederationEnrollment(t.Context(), other.Enrollment.ID))
					}
					proof = other.Token
				}
				request["move_source_tokens"] = map[string]string{source.UID: proof}
			}
			headers := bearer(targetGrant.Token)
			headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
			response, raw := envDoRaw(t, env, http.MethodPost, projectPath(target.ID)+"/federation/events:ingest", request, headers)
			require.Equalf(t, tc.status, response.StatusCode, "%s", raw)
			if tc.mode != "missing-proof" && tc.mode != "invalid-proof" && !strings.HasPrefix(tc.mode, "proof-") {
				require.Positive(t, access.sourceChecks.Load(), "source host authority must be checked")
			}
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			if tc.status == http.StatusOK {
				require.Equal(t, target.ID, got.ProjectID)
			} else {
				require.Equal(t, source.ID, got.ProjectID)
				after, err := env.DB.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, cursor, after, "a source rejection rolls back every move effect")
			}
			if tc.mode == "fence-denied" {
				require.True(t, access.finished.Load(), "source rejection cleanup must finish after rollback")
			}
		})
	}
}
