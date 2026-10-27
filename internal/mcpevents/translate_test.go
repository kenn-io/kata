package mcpevents

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/notification"
)

const (
	sourceProject      = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	sourceOtherProject = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	sourceIssue        = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	sourceDaemon       = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
)

func sourceEvent(kind string, n int64, payload string) Occurrence {
	return Occurrence{
		EventID: n, EventUID: "01ARZ3NDEKTSV4RRFFQ" + padID(n), Type: kind, IssueUID: sourceIssue,
		ProjectUID: sourceProject, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Payload: []byte(payload),
	}
}

func TestTranslateOnlyExactInboxChanges(t *testing.T) {
	key := notification.MetadataKey("example-user/worker")
	valid := `{"from":"example-user","message":"do work"}`
	for _, tc := range []struct{ name, kind, payload, reason string }{
		{"notify", "issue.metadata_updated", `{"diff":{"` + key + `":{"from":null,"to":` + valid + `}}}`, "notification"},
		{"clear", "issue.metadata_updated", `{"diff":{"` + key + `":{"from":` + valid + `,"to":null}}}`, "notification"},
		{"create", "issue.created", `{"metadata":{"` + key + `":` + valid + `}}`, "notification"},
		{"snapshot", "issue.snapshot", `{"metadata":{"` + key + `":` + valid + `}}`, "notification"},
		{"snapshot removes", "issue.snapshot", `{"metadata":{}}`, "visibility"},
		{"wrong recipient", "issue.metadata_updated", `{"diff":{"` + notification.MetadataKey("example-user") + `":{"to":` + valid + `}}}`, ""},
		{"attention", "issue.metadata_updated", `{"diff":{"work.attention":{"to":"needs-human"}}}`, ""},
		{"comment", "issue.commented", `{"body":"notify example-user/worker"}`, ""},
		{"assignment", "issue.assigned", `{"owner":"example-user/worker"}`, ""},
		{"invalid notification", "issue.created", `{"metadata":{"` + key + `":{"from":"","message":"x"}}}`, ""},
		{"title", "issue.updated", `{"title":"new title"}`, ""},
		{"status", "issue.updated", `{"status":"closed"}`, "visibility"},
		{"parent attached", "issue.linked", `{"type":"parent"}`, "visibility"},
		{"parent detached", "issue.unlinked", `{"type":"parent"}`, "visibility"},
		{"parent replaced", "issue.links_changed", `{"parent_set_uid":"` + sourceIssue + `"}`, "visibility"},
		{"parent removed", "issue.links_changed", `{"parent_removed_uid":"` + sourceIssue + `"}`, "visibility"},
		{"related attached", "issue.linked", `{"type":"related"}`, ""},
		{"blocker changed", "issue.links_changed", `{"blocks_added_uids":["` + sourceIssue + `"]}`, ""},
		{"closed", "issue.closed", `{}`, "visibility"},
		{"reopened", "issue.reopened", `{}`, "visibility"},
		{"deleted", "issue.soft_deleted", `{}`, "visibility"},
		{"restored", "issue.restored", `{}`, "visibility"},
		{"moved", "issue.moved", `{}`, "visibility"},
		{"project removed", "project.removed", `{}`, "visibility"},
		{"project restored", "project.restored", `{}`, "visibility"},
		{"project merged", "project.merged", `{}`, "visibility"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.reason, inboxEventReason(sourceEvent(tc.kind, 1, tc.payload), Arguments{For: "example-user/worker"}))
		})
	}
}

func TestTranslateIssueFilterCannotWiden(t *testing.T) {
	key := notification.MetadataKey("example-user")
	row := sourceEvent("issue.created", 1, `{"metadata":{"`+key+`":{"from":"sender","message":"work"}}}`)
	args := Arguments{For: "example-user", ProjectUIDs: []string{sourceProject}, IssueUID: sourceIssue}
	assert.Equal(t, "notification", inboxEventReason(row, args))
	row.IssueUID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	assert.Empty(t, inboxEventReason(row, args))
	row.IssueUID = ""
	assert.Empty(t, inboxEventReason(row, args))
	row.Type = "project.removed"
	assert.Equal(t, "visibility", inboxEventReason(row, args))
}

func TestEventIDIsStableAndRecipientSpecific(t *testing.T) {
	a := inboxSourceEventID(sourceDaemon, "event-1", "example-user")
	assert.Equal(t, a, inboxSourceEventID(sourceDaemon, "event-1", "example-user"))
	assert.NotEqual(t, a, inboxSourceEventID(sourceDaemon, "event-1", "example-user/worker"))
	assert.NotEqual(t, a, inboxSourceEventID(sourceDaemon, "event-2", "example-user"))
	assert.NotEqual(t, a, inboxSourceEventID("01ARZ3NDEKTSV4RRFFQ69G5FB0", "event-1", "example-user"))
	assert.Regexp(t, `^[0-9a-f]{64}$`, a)

}

func FuzzTranslateExactRecipient(f *testing.F) {
	f.Add("example-user", "example-user/worker")
	f.Add("example-user/worker", "example-user/worker")
	f.Add("actor/équipe", "actor/équipe")
	f.Fuzz(func(t *testing.T, recipient, target string) {
		recipient, e1 := notification.NormalizeRecipient(recipient)
		target, e2 := notification.NormalizeRecipient(target)
		if e1 != nil || e2 != nil {
			return
		}
		b, err := json.Marshal(map[string]any{"diff": map[string]any{notification.MetadataKey(recipient): map[string]any{"to": notification.Value{From: "sender", Message: "work"}}}})
		require.NoError(t, err)
		reason := inboxEventReason(sourceEvent("issue.metadata_updated", 1, string(b)), Arguments{For: target})
		want := ""
		if recipient == target {
			want = "notification"
		}
		assert.Equal(t, want, reason)
	})
}
