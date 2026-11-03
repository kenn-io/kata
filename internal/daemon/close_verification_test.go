package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCloseVerificationGuard(t *testing.T) {
	for _, mode := range []string{"block", "warn", "off", ""} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CloseVerification = mode })
			pid := initLocalWorkspace(t, env, "example-workspace")
			id := createIssueViaHTTP(t, env, pid, "Claim")
			claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Claim"})
			require.NoError(t, err)
			r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "The proposed implementation does not meet the contract.", ReplyToUID: claim.UID, ReplyKind: "refute"})
			require.NoError(t, err)
			body := map[string]any{"actor": "worker", "reason": "done", "message": "Implemented the feature and checked the focused package.", "evidence": []map[string]any{{"type": "test", "command": "go test ./example"}}}
			closeIssue := func() (int, []byte) {
				resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), body, nil)
				return resp.StatusCode, raw
			}
			code, raw := closeIssue()
			if mode == "block" {
				require.Equal(t, 409, code, string(raw))
				require.Contains(t, string(raw), "open_refutations")
				body["overrule"] = []string{"c:unknown"}
				code, raw = closeIssue()
				require.Equal(t, 400, code, string(raw))
				body["overrule"] = []string{r.UID}
				code, raw = closeIssue()
				require.Equal(t, 200, code, string(raw))
				require.Contains(t, string(raw), "overruled_refutations")
			} else {
				require.Equal(t, 200, code, string(raw))
				if mode == "off" {
					require.NotContains(t, string(raw), "verification_warnings")
				} else {
					require.Contains(t, string(raw), "verification_warnings")
					require.Contains(t, string(raw), "warnings")
				}
			}
		})
	}
}

func TestShowUsesFederationOrderForCloseEvidence(t *testing.T) {
	env := testenv.New(t)
	project := createFederatedHubProject(t, env, "example-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Federated completion", Author: "example-agent",
	})
	require.NoError(t, err)
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "example-agent", Body: "The requested behavior is complete.",
	})
	require.NoError(t, err)

	created, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{
		IssueUID: issue.UID, Types: []string{"issue.created"}, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, created, 1)

	closeEvent := func(eventUID string, hlc int64, evidence db.Evidence) db.RemoteEvent {
		t.Helper()
		createdAt := time.UnixMilli(hlc).UTC()
		payload, err := json.Marshal(struct {
			IssueUID string        `json:"issue_uid"`
			Reason   string        `json:"reason"`
			ClosedAt string        `json:"closed_at"`
			Evidence []db.Evidence `json:"evidence"`
		}{
			IssueUID: issue.UID,
			Reason:   "done",
			ClosedAt: createdAt.Format(db.EventTimestampFormat),
			Evidence: []db.Evidence{evidence},
		})
		require.NoError(t, err)
		event := db.RemoteEvent{
			EventUID: eventUID, OriginInstanceUID: federationTestSpokeUID,
			ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &issue.UID,
			Type: "issue.closed", Actor: "example-agent", HLCPhysicalMS: hlc,
			Payload: jsontext.Value(payload), CreatedAt: createdAt,
		}
		event.ContentHash, err = db.EventContentHash(db.EventHashInput{
			UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID,
			ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
			IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
			Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
			HLCCounter: event.HLCCounter,
			CreatedAt:  event.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: event.Payload,
		})
		require.NoError(t, err)
		return event
	}
	ingestClose := func(sourceEventID int64, event db.RemoteEvent) {
		t.Helper()
		_, err := env.DB.IngestFederationEvents(t.Context(), db.FederationIngestParams{
			ProjectID: project.ID, SpokeInstanceUID: federationTestSpokeUID,
			Events: []db.FederationIngestEvent{{SourceEventID: sourceEventID, Event: event}},
		})
		require.NoError(t, err)
	}

	newer := closeEvent("01HZNQ7VFPK1XGD8R5MABCD4EV", created[0].HLCPhysicalMS+2000,
		db.Evidence{Type: "confirmed", Comment: claim.UID})
	older := closeEvent("01HZNQ7VFPK1XGD8R5MABCD4EW", created[0].HLCPhysicalMS+1000,
		db.Evidence{Type: "refuted", Comment: claim.UID})
	ingestClose(1, newer)
	ingestClose(2, older)

	issue, err = env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", issue.Status)
	closes, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{
		IssueUID: issue.UID, Types: []string{"issue.closed"}, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, closes, 2)
	require.Equal(t, newer.EventUID, closes[0].UID)
	require.Equal(t, older.EventUID, closes[1].UID)
	require.Less(t, closes[0].ID, closes[1].ID, "the older close must arrive later locally")
	require.Greater(t, closes[0].HLCPhysicalMS, closes[1].HLCPhysicalMS,
		"federation order must keep the first close effective")

	resp, raw := envDoRaw(t, env, http.MethodGet, issuePathRef(project.ID, issue.ShortID, ""), nil, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "%s", string(raw))
	var shown struct {
		Verification *db.Verification `json:"verification"`
	}
	require.NoError(t, json.Unmarshal(raw, &shown))
	require.NotNil(t, shown.Verification)
	require.Len(t, shown.Verification.Evidence, 1)
	require.Equal(t, "confirmed", shown.Verification.Evidence[0].Type)
}

func TestCloseVerificationResolution(t *testing.T) {
	for _, resolution := range []string{"supersede-claim", "supersede-refutation", "rebuttal", "conflict", "refuted-rebuttal"} {
		t.Run(resolution, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CloseVerification = "block" })
			pid := initLocalWorkspace(t, env, "example-workspace")
			id := createIssueViaHTTP(t, env, pid, "Claim")
			claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Claim"})
			require.NoError(t, err)
			r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Disputed claim", ReplyToUID: claim.UID, ReplyKind: "refute"})
			require.NoError(t, err)
			kind, target := "supersede", claim.UID
			switch resolution {
			case "supersede-refutation":
				target = r.UID
			case "rebuttal", "refuted-rebuttal":
				kind, target = "refute", r.UID
			case "conflict":
				kind = "confirm"
			}
			b, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Teammate: "other", Body: strings.Repeat("Evidence ", 8), ReplyToUID: target, ReplyKind: kind})
			require.NoError(t, err)
			if resolution == "refuted-rebuttal" {
				_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Reopened", ReplyToUID: b.UID, ReplyKind: "refute"})
				require.NoError(t, err)
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), map[string]any{"actor": "worker", "reason": "done", "message": "Implemented the feature and checked the focused package.", "evidence": []map[string]any{{"type": "test", "command": "go test ./example"}}}, nil)
			want := 200
			if resolution == "conflict" || resolution == "refuted-rebuttal" {
				want = 409
			}
			require.Equal(t, want, resp.StatusCode, string(raw))
		})
	}
}

func TestCloseVerificationReasonsAndOverrules(t *testing.T) {
	for _, policy := range []string{"block", "warn", "off"} {
		for _, reason := range []string{"audit-no-change", "wontfix", "duplicate", "superseded"} {
			t.Run(policy+"/"+reason, func(t *testing.T) {
				env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CloseVerification = policy })
				pid := initLocalWorkspace(t, env, "example-workspace")
				id := createIssueViaHTTP(t, env, pid, "Claim")
				claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Claim"})
				require.NoError(t, err)
				_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Disputed", ReplyToUID: claim.UID, ReplyKind: "refute"})
				require.NoError(t, err)
				body := map[string]any{"actor": "worker", "reason": reason, "message": strings.Repeat("Reviewed the claim and recorded the completion decision. ", 2)}
				switch reason {
				case "audit-no-change":
					body["evidence"] = []map[string]any{{"type": "no-change-audit", "rationale": "Reviewed the implementation and found no required change."}}
				case "duplicate", "superseded":
					other := createIssueViaHTTP(t, env, pid, "Target")
					target, err := env.DB.IssueByID(t.Context(), other)
					require.NoError(t, err)
					kind := "duplicate-of"
					if reason == "superseded" {
						kind = "superseded-by"
					}
					body["evidence"] = []map[string]any{{"type": kind, "issue_ref": target.ShortID}}
				}
				response, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), body, nil)
				want := 200
				if reason == "audit-no-change" && policy == "block" {
					want = 409
				}
				require.Equal(t, want, response.StatusCode, string(raw))
			})
		}
	}
	for _, selection := range []string{"partial", "complete", "duplicate", "resolved"} {
		t.Run(selection, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.CloseVerification = "block" })
			pid := initLocalWorkspace(t, env, "example-workspace")
			id := createIssueViaHTTP(t, env, pid, "Claim")
			claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Claim"})
			require.NoError(t, err)
			refs := []string{}
			for _, author := range []string{"reviewer-a", "reviewer-b"} {
				r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: author, Body: "Disputed", ReplyToUID: claim.UID, ReplyKind: "refute"})
				require.NoError(t, err)
				refs = append(refs, r.UID)
			}
			switch selection {
			case "partial":
				refs = refs[:1]
			case "duplicate":
				refs = append(refs, refs[0])
			case "resolved":
				_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer-a", Body: "Withdrawn", ReplyToUID: refs[0], ReplyKind: "supersede"})
				require.NoError(t, err)
			}
			response, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), map[string]any{"actor": "worker", "reason": "done", "message": "Completed the requested fix and checked the focused tests.", "evidence": []map[string]any{{"type": "test", "command": "go test ./example"}}, "overrule": refs}, nil)
			want := 200
			if selection == "partial" {
				want = 409
			}
			if selection == "resolved" {
				want = 400
			}
			require.Equal(t, want, response.StatusCode, string(raw))
		})
	}
}
