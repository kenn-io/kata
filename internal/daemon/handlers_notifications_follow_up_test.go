package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	"hegel.dev/go/hegel"
)

// The public contract requires both fields to commit in one event, on both stores.
func TestNotifyFollowUpAtomicMetadata(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Await decision", Author: "coordinator"})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now()})
		t.Cleanup(func() { _ = server.Close() })
		ts := httptest.NewServer(server.Handler())
		t.Cleanup(ts.Close)
		path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", project.ID, issue.ShortID)
		body := map[string]any{"actor": "coordinator", "to": "alex-outside", "message": "Please decide", "scheduled_on": "2099-11-10"}
		before, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		resp, raw := postJSON(t, ts, path, body)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		var out struct {
			ScheduledOn string   `json:"scheduled_on"`
			Changed     bool     `json:"changed"`
			Event       db.Event `json:"event"`
		}
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Equal(t, "2099-11-10", out.ScheduledOn)
		require.True(t, out.Changed)
		events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{AfterID: before, ProjectID: project.ID, IssueUID: issue.UID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, events[0].UID, out.Event.UID)
		var payload struct {
			Diff map[string]struct {
				To jsontext.Value `json:"to"`
			} `json:"diff"`
		}
		require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
		require.Len(t, payload.Diff, 2)
		require.JSONEq(t, `"2099-11-10"`, string(payload.Diff["scheduled_on"].To))
		require.JSONEq(t, `{"from":"coordinator","message":"Please decide"}`, string(payload.Diff[notification.MetadataKey("alex-outside")].To))
		saved, err := store.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Equal(t, issue.Revision+1, saved.Revision)
		for _, value := range []any{"bad-date", "", "-", "2099-02-30", 123, false} {
			body["scheduled_on"] = value
			body["message"] = "Must not replace the ask"
			resp, raw = postJSON(t, ts, path, body)
			require.Equal(t, 400, resp.StatusCode, string(raw))
			if _, ok := value.(string); ok {
				require.Contains(t, string(raw), `"code":"invalid_metadata_value"`)
			}
			current, err := store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, saved.Metadata, current.Metadata)
			require.Equal(t, saved.Revision, current.Revision)
			after, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, events[0].ID, after)
		}
		body["scheduled_on"] = "2099-11-10"
		body["message"] = "Please decide"
		resp, raw = postJSON(t, ts, path, body)
		require.Equal(t, 200, resp.StatusCode, string(raw))
		require.NoError(t, json.Unmarshal(raw, &out))
		require.False(t, out.Changed)
		require.Equal(t, "2099-11-10", out.ScheduledOn)
		var repeat map[string]any
		require.NoError(t, json.Unmarshal(raw, &repeat))
		require.NotContains(t, repeat, "event")
		unchanged, err := store.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Equal(t, saved, unchanged)
		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, events[0].ID, cursor)
	})
}

// Contract: follow-up acceptance is schedule acceptance, with exactly the same raw date retained.
func TestNotifyFollowUpDateAgreement(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	followIssue, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Follow-up comparison", Author: "coordinator"})
	require.NoError(t, err)
	path := issueURL(pid, followIssue.ID, "notifications")
	metadataPath := issueURL(pid, id, "metadata")
	hegel.Test(t, func(ht *hegel.T) {
		at := time.Date(hegel.Draw(ht, hegel.Integers(0, 9999)), time.Month(hegel.Draw(ht, hegel.Integers(1, 12))),
			hegel.Draw(ht, hegel.Integers(1, 31)), hegel.Draw(ht, hegel.Integers(0, 23)), hegel.Draw(ht, hegel.Integers(0, 59)), hegel.Draw(ht, hegel.Integers(0, 59)), hegel.Draw(ht, hegel.Integers(0, 999999999)), time.UTC)
		format := hegel.Draw(ht, hegel.SampledFrom([]string{"2006-01-02", "2006-01-02T15:04", "2006-01-02T15:04:05", time.RFC3339Nano}))
		value := at.Format(format)
		if format == "2006-01-02T15:04:05" {
			switch hegel.Draw(ht, hegel.Integers(0, 3)) {
			case 1:
				value += ".5"
			case 2:
				value += ",5"
			case 3:
				value = fmt.Sprintf("%sT%d:%02d:%02d", at.Format(time.DateOnly), at.Hour(), at.Minute(), at.Second())
			}
		}
		schedule, raw := postJSON(t, ts, metadataPath, map[string]any{"actor": "coordinator", "patch": map[string]any{"scheduled_on": value}})
		require.Equal(ht, 200, schedule.StatusCode, string(raw))
		follow, raw := postJSON(t, ts, path, map[string]any{"actor": "coordinator", "to": "alex", "message": "Please decide", "scheduled_on": value})
		require.Equal(ht, schedule.StatusCode, follow.StatusCode, string(raw))
		var response struct {
			ScheduledOn string `json:"scheduled_on"`
		}
		require.NoError(ht, json.Unmarshal(raw, &response))
		require.Equal(ht, value, response.ScheduledOn)
		current, err := h.DB().IssueByID(t.Context(), followIssue.ID)
		require.NoError(ht, err)
		var values map[string]any
		require.NoError(ht, json.Unmarshal([]byte(current.Metadata), &values))
		require.Equal(ht, value, values["scheduled_on"])
	}, hegel.WithTestCases(1000))
}

func TestNotifyFollowUpArbitraryDateAgreement(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	followIssue, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Follow-up comparison", Author: "coordinator"})
	require.NoError(t, err)
	path, metadataPath := issueURL(pid, followIssue.ID, "notifications"), issueURL(pid, id, "metadata")
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		before, err := h.DB().IssueByID(t.Context(), followIssue.ID)
		require.NoError(ht, err)
		schedule, _ := postJSON(t, ts, metadataPath, map[string]any{"actor": "coordinator", "patch": map[string]any{"scheduled_on": value}})
		follow, raw := postJSON(t, ts, path, map[string]any{"actor": "coordinator", "to": "alex", "message": "Please decide", "scheduled_on": value})
		require.Contains(ht, []int{200, 400}, schedule.StatusCode)
		require.Equal(ht, schedule.StatusCode, follow.StatusCode, string(raw))
		if follow.StatusCode == 200 {
			after, err := h.DB().IssueByID(t.Context(), followIssue.ID)
			require.NoError(ht, err)
			var response struct {
				ScheduledOn string `json:"scheduled_on"`
			}
			require.NoError(ht, json.Unmarshal(raw, &response))
			require.Equal(ht, value, response.ScheduledOn)
			var values map[string]any
			require.NoError(ht, json.Unmarshal([]byte(after.Metadata), &values))
			require.Equal(ht, value, values["scheduled_on"])
		}
		if follow.StatusCode >= 400 {
			after, err := h.DB().IssueByID(t.Context(), followIssue.ID)
			require.NoError(ht, err)
			require.Equal(ht, before.Metadata, after.Metadata)
			require.Equal(ht, before.Revision, after.Revision)
		}
	}, hegel.WithTestCases(1000))
}

// The optional field cannot weaken existing notification denial or rate-limit gates.
func TestNotifyFollowUpRejectionsPreserveBothFields(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	path := issueURL(pid, id, "notifications")
	_, _, _, err := h.DB().UpdateOwner(t.Context(), id, new("reader"), "sender")
	require.NoError(t, err)
	resp, raw := postJSON(t, ts, path, map[string]any{"actor": "sender", "broadcast": true, "message": "Ask", "scheduled_on": "2099-11-10"})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	saved, err := h.DB().IssueByID(t.Context(), id)
	require.NoError(t, err)
	for _, tc := range []struct {
		body   map[string]any
		status int
	}{
		{map[string]any{"actor": "sender", "broadcast": true, "message": "Ask", "scheduled_on": "2099-11-11"}, 429},
		{map[string]any{"actor": "sender", "to": "alex", "re": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "message": "Ask", "scheduled_on": "2099-11-11"}, 404},
		{map[string]any{"actor": "sender", "to": "alex", "message": "", "scheduled_on": "2099-11-11"}, 400},
	} {
		before, err := h.DB().MaxEventID(t.Context())
		require.NoError(t, err)
		resp, raw = postJSON(t, ts, path, tc.body)
		require.Equal(t, tc.status, resp.StatusCode, string(raw))
		after, err := h.DB().IssueByID(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, saved, after)
		cursor, err := h.DB().MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, cursor)
	}
}

func TestNotifyFollowUpEmptyAudienceAndOmission(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), id)
	require.NoError(t, err)
	path := issueURL(pid, id, "notifications")
	resp, raw := postJSON(t, ts, path, map[string]any{"actor": issue.Author, "broadcast": true, "message": "Ask", "scheduled_on": "2099-11-10"})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var result map[string]any
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, []any{}, result["recipients"])
	require.Equal(t, true, result["changed"])
	require.Equal(t, "2099-11-10", result["scheduled_on"])
	resp, raw = postJSON(t, ts, path, map[string]any{"actor": "sender", "to": "alex", "message": "Ask", "scheduled_on": nil})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	result = nil
	require.NoError(t, json.Unmarshal(raw, &result))
	require.NotContains(t, result, "scheduled_on")
	saved, err := h.DB().IssueByID(t.Context(), id)
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, json.Unmarshal([]byte(saved.Metadata), &values))
	require.Equal(t, "2099-11-10", values["scheduled_on"])
}

func TestNotifyFollowUpPushSpokeActorAndClaim(t *testing.T) {
	env, project, issue := pushSpokeNotificationFixture(t)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", project.ID, issue.ShortID)
	resp, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "request-agent", "to": "alex", "message": "Ask", "scheduled_on": "2099-11-10"}, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	saved, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(saved.Metadata), &values))
	require.JSONEq(t, `"2099-11-10"`, string(values["scheduled_on"]))
	var request notification.Value
	require.NoError(t, json.Unmarshal(values[notification.MetadataKey("alex")], &request))
	require.Equal(t, "bound-agent", request.From)
	// Another holder's hard claim rejects the combined request, including its date.
	require.NoError(t, env.DB.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{Held: true, Holder: db.ClaimPrincipal{HolderInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Holder: "other-agent"}, Claim: &db.IssueClaim{ClaimUID: "01HZNQ7VFPK1XGD8R5MABCD4CB", IssueUID: issue.UID, HolderInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Holder: "other-agent", ClaimKind: "hard", AcquiredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Revision: 2}, HubNow: time.Now().UTC()}))
	resp, raw = envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "request-agent", "to": "alex", "message": "Replace", "scheduled_on": "2099-11-11"}, nil)
	require.Equal(t, 409, resp.StatusCode, string(raw))
	after, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, saved.Metadata, after.Metadata)
}

// A combined replacement preserves exact unrelated custom metadata and inbox values.
func TestNotifyFollowUpPreservesMetadata(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	path := issueURL(pid, id, "notifications")
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		value, err := json.Marshal(map[string]any{"nested": map[string]any{"text": text}, "flag": hegel.Draw(ht, hegel.Booleans()), "count": hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))})
		require.NoError(ht, err)
		other, err := json.Marshal(notification.Value{From: "other-sender", Message: "Keep this ask"})
		require.NoError(ht, err)
		_, err = h.DB().PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: id, Actor: "coordinator", Patch: map[string]jsontext.Value{"context": value, notification.MetadataKey("other-person"): other}})
		require.NoError(ht, err)
		resp, raw := postJSON(t, ts, path, map[string]any{"actor": "coordinator", "to": "alex", "message": "Replace this ask", "scheduled_on": "2099-11-10"})
		require.Equal(ht, 200, resp.StatusCode, string(raw))
		after, err := h.DB().IssueByID(t.Context(), id)
		require.NoError(ht, err)
		var values map[string]jsontext.Value
		require.NoError(ht, json.Unmarshal([]byte(after.Metadata), &values))
		require.JSONEq(ht, string(value), string(values["context"]))
		require.JSONEq(ht, string(other), string(values[notification.MetadataKey("other-person")]))
	}, hegel.WithTestCases(1000))
}
