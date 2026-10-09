package dbtest

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/transcript"
)

func checkCloseTranscript(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f, err := createIssueFixture(ctx, store, "example-project", "Completed example work", "worker", nil)
	if err != nil {
		return err
	}
	ref := &transcript.Transcript{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}
	_, events, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.Issue.ID, Reason: "done", Actor: "worker", Transcript: ref, Evidence: []db.Evidence{{Type: "test", Command: "go test ./internal/example"}}})
	if err != nil {
		return err
	}
	require.True(t, changed)
	require.NotEmpty(t, events)
	var payload struct {
		Transcript *transcript.Transcript `json:"transcript"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	require.Equal(t, ref, payload.Transcript)
	return nil
}
