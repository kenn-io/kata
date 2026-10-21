package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRelayHistoryNeedsResetOrder(t *testing.T) {
	event := func(id int64, kind, issue string) db.EventExport {
		return db.EventExport{ID: id, Type: kind, IssueUID: &issue, Payload: jsontext.Value(`{"uid":"` + issue + `"}`), ContentHash: fmt.Sprintf("retained-hash-%d", id), HLCPhysicalMS: 100 - id}
	}
	for _, tc := range []struct {
		name   string
		events []db.EventExport
		reset  bool
	}{
		{"empty", nil, false},
		{"late snapshot", []db.EventExport{event(1, "issue.assigned", "A"), event(2, "issue.snapshot", "A")}, true},
		{"creation first", []db.EventExport{event(1, "issue.created", "A"), event(2, "issue.assigned", "A")}, false},
		{"snapshot first", []db.EventExport{event(1, "issue.snapshot", "A"), event(2, "issue.assigned", "A")}, false},
		{"introductions are issue specific", []db.EventExport{event(1, "issue.created", "B"), event(2, "issue.assigned", "A"), event(3, "issue.snapshot", "A")}, true},
		{"catalog does not introduce issue", []db.EventExport{{ID: 1, Type: "project.created", Payload: jsontext.Value(`{"uid":"A"}`)}, event(2, "issue.assigned", "A")}, true},
		{"payload identity", []db.EventExport{{ID: 1, Type: "issue.snapshot", Payload: jsontext.Value(`{"uid":"A"}`)}, event(2, "issue.assigned", "A")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := json.Marshal(tc.events)
			require.NoError(t, err)
			reset, err := db.RelayHistoryNeedsReset(tc.events)
			require.NoError(t, err)
			require.Equal(t, tc.reset, reset)
			after, err := json.Marshal(tc.events)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after), "retained order, payloads and hashes are immutable")
		})
	}
}

func TestRelayHistoryNeedsResetOrderIncremental(t *testing.T) {
	var history db.RelayHistoryOrderValidator
	issueA, issueB := "A", "B"
	for _, step := range []struct {
		event db.EventExport
		reset bool
	}{
		{db.EventExport{Type: "issue.created", IssueUID: &issueB, Payload: jsontext.Value(`{}`)}, false},
		{db.EventExport{Type: "issue.assigned", IssueUID: &issueB, Payload: jsontext.Value(`{}`)}, false},
		{db.EventExport{Type: "issue.assigned", IssueUID: &issueA, Payload: jsontext.Value(`{}`)}, true},
		{db.EventExport{Type: "issue.snapshot", IssueUID: &issueA, Payload: jsontext.Value(`{}`)}, true},
		{db.EventExport{Type: "issue.snapshot", IssueUID: &issueA, Payload: jsontext.Value(`{}`)}, true},
	} {
		reset, err := history.Observe(step.event)
		require.NoError(t, err)
		require.Equal(t, step.reset, reset, "incremental state retains issue introductions and the first invalid prefix")
	}
}

// The first retained issue event must be an introduction, regardless of later
// snapshots or interleaving with other issues. Native fuzzing needs no dependency.
func FuzzRelayHistoryNeedsResetOrder(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 2})
	f.Add([]byte{0, 1, 2})
	f.Add([]byte{0, 4, 2, 3, 5})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			data = data[:4096]
		}
		var events []db.EventExport
		first := map[string]string{}
		for i, b := range data {
			issue := fmt.Sprintf("issue-%d", b/3)
			kind := []string{"issue.created", "issue.assigned", "issue.snapshot"}[b%3]
			if _, seen := first[issue]; !seen {
				first[issue] = kind
			}
			events = append(events, db.EventExport{ID: int64(i + 1), Type: kind, IssueUID: &issue, Payload: jsontext.Value(`{}`), ContentHash: fmt.Sprintf("hash-%d", i)})
		}
		want := false
		for _, kind := range first {
			want = want || kind == "issue.assigned"
		}
		got, err := db.RelayHistoryNeedsReset(events)
		require.NoError(t, err)
		require.Equal(t, want, got)
		var history db.RelayHistoryOrderValidator
		incremental := false
		for _, event := range events {
			incremental, err = history.Observe(event)
			require.NoError(t, err)
		}
		require.Equal(t, want, incremental)
	})
}
