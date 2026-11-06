package db_test

import (
	"cmp"
	"encoding/json/jsontext"
	"math"
	"testing"

	"pgregory.net/rapid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestReconcileFoldStatusIntent(t *testing.T) {
	event := func(uid, kind, status string, clock int64) db.FoldEvent {
		return db.FoldEvent{UID: uid, IssueUID: "task", Type: kind, HLCPhysicalMS: clock, Payload: jsontext.Value(`{"status":"` + status + `"}`)}
	}
	previous := db.FoldIssue{UID: "task", Status: "closed", StatusClock: db.FoldClock{HLCPhysicalMS: 100, EventUID: "snapshot"}}
	for _, tc := range []struct {
		name, currentStatus, currentIntent, pending, want string
		accepted                                          []db.FoldEvent
	}{
		{"duplicate", "closed", "local-close", "local-close", "local-close", nil},
		{"old same-state explicit cannot replace pending", "closed", "old-close", "local-close", "local-close", []db.FoldEvent{event("old-close", "issue.closed", "closed", 50)}},
		{"old opposite explicit cannot cancel pending", "closed", "", "local-close", "local-close", []db.FoldEvent{event("old-open", "issue.reopened", "open", 50)}},
		{"old explicit cannot manufacture intent", "closed", "old-close", "", "", []db.FoldEvent{event("old-close", "issue.closed", "closed", 50)}},
		{"new same-state snapshot retains pending", "closed", "local-close", "local-close", "local-close", []db.FoldEvent{event("snapshot-new", "issue.snapshot", "closed", 150)}},
		{"opposite then returning snapshot clears", "closed", "", "local-close", "", []db.FoldEvent{event("return", "issue.snapshot", "closed", 200), event("opposite", "issue.updated", "open", 150)}},
		{"explicit then same-state restatement survives", "closed", "new-close", "", "new-close", []db.FoldEvent{event("restatement", "issue.updated", "closed", 200), event("new-close", "issue.closed", "closed", 150)}},
		{"new reopen replaces pending", "open", "new-open", "local-close", "new-open", []db.FoldEvent{event("new-open", "issue.reopened", "open", 150)}},
		{"explicit then overriding transition loses", "open", "", "local-close", "", []db.FoldEvent{event("new-close", "issue.closed", "closed", 150), event("opposite", "issue.updated", "open", 200)}},
		{"new snapshot never manufactures intent", "open", "", "", "", []db.FoldEvent{event("snapshot-new", "issue.snapshot", "open", 150)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := db.FoldIssue{UID: "task", Status: tc.currentStatus, StatusIntentUID: tc.currentIntent}
			require.Equal(t, tc.want, db.ReconcileFoldStatusIntent(previous, current, tc.pending, tc.accepted))
		})
	}
}

// The independent oracle is lexicographic writer order. Generation deliberately
// includes tied prefixes, so all four clock components and equality are reached.
func TestReconcileFoldStatusIntentWriterFence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		previousClock := db.FoldClock{HLCPhysicalMS: rapid.Int64Range(0, math.MaxInt64).Draw(t, "previous physical"), HLCCounter: rapid.Int64Range(0, math.MaxInt64).Draw(t, "previous counter"), OriginInstanceUID: "origin-" + rapid.String().Draw(t, "previous origin"), EventUID: "event-" + rapid.String().Draw(t, "previous event")}
		incomingClock := db.FoldClock{HLCPhysicalMS: rapid.Int64Range(0, math.MaxInt64).Draw(t, "incoming physical"), HLCCounter: rapid.Int64Range(0, math.MaxInt64).Draw(t, "incoming counter"), OriginInstanceUID: "origin-" + rapid.String().Draw(t, "incoming origin"), EventUID: "event-" + rapid.String().Draw(t, "incoming event")}
		if rapid.Bool().Draw(t, "tie physical") {
			incomingClock.HLCPhysicalMS = previousClock.HLCPhysicalMS
		}
		if rapid.Bool().Draw(t, "tie counter") {
			incomingClock.HLCCounter = previousClock.HLCCounter
		}
		if rapid.Bool().Draw(t, "tie origin") {
			incomingClock.OriginInstanceUID = previousClock.OriginInstanceUID
		}
		if rapid.Bool().Draw(t, "tie event") {
			incomingClock.EventUID = previousClock.EventUID
		}
		order := []int{cmp.Compare(incomingClock.HLCPhysicalMS, previousClock.HLCPhysicalMS), cmp.Compare(incomingClock.HLCCounter, previousClock.HLCCounter), cmp.Compare(incomingClock.OriginInstanceUID, previousClock.OriginInstanceUID), cmp.Compare(incomingClock.EventUID, previousClock.EventUID)}
		newer := false
		for _, component := range order {
			if component != 0 {
				newer = component > 0
				break
			}
		}
		previous := db.FoldIssue{UID: "task", Status: "closed", StatusClock: previousClock}
		current := db.FoldIssue{UID: "task", Status: "closed"}
		want := "pending-close"
		if newer {
			current.Status = "open"
			current.StatusIntentUID = incomingClock.EventUID
			want = incomingClock.EventUID
		}
		incoming := db.FoldEvent{UID: incomingClock.EventUID, OriginInstanceUID: incomingClock.OriginInstanceUID, IssueUID: "task", Type: "issue.reopened", HLCPhysicalMS: incomingClock.HLCPhysicalMS, HLCCounter: incomingClock.HLCCounter, Payload: jsontext.Value(`{}`)}
		require.Equal(t, want, db.ReconcileFoldStatusIntent(previous, current, "pending-close", []db.FoldEvent{incoming}))
	})
}
