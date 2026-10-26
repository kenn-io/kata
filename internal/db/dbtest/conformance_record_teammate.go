package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

// RecordTeammateCase separates presence from the value, including suppression.
type RecordTeammateCase struct {
	Header, Body string
	Present      bool
	Null         bool
}

// Want is the contract oracle: a present record value wins, even when empty.
func (c RecordTeammateCase) Want() string {
	if c.Null {
		return ""
	}
	if c.Present {
		return c.Body
	}
	return c.Header
}

// RecordTeammateCases enumerates fixed request-header and record-field presence cases.
func RecordTeammateCases() []RecordTeammateCase {
	var cases []RecordTeammateCase
	for _, header := range []string{"", "codex"} {
		cases = append(cases, RecordTeammateCase{Header: header})
		for _, body := range []string{"", "codex", "teammate-1"} {
			cases = append(cases, RecordTeammateCase{Header: header, Body: body, Present: true})
		}
	}
	return cases
}

// DrawRecordTeammate covers the complete portable handle and presence domains.
func DrawRecordTeammate(ht *hegel.T) RecordTeammateCase {
	drawHandle := func() string {
		const first = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		const rest = first + "._-"
		size := hegel.Draw(ht, hegel.Integers(0, 64))
		b := make([]byte, size)
		for i := range b {
			chars := rest
			if i == 0 {
				chars = first
			}
			b[i] = chars[hegel.Draw(ht, hegel.Integers(0, len(chars)-1))]
		}
		return string(b)
	}
	return RecordTeammateCase{Header: drawHandle(), Body: drawHandle(), Present: hegel.Draw(ht, hegel.Booleans())}
}

// RecordTeammateRoot reuses the conformance root-signing fixtures for HTTP tests.
func RecordTeammateRoot(t *testing.T, store db.Storage) (db.Project, db.RootAttributionSigner, db.RootKeyPin) {
	project := rootRecoveryHub(t, store, "example-project")
	pin, key := rootRecoveryKey(t, store, project.UID)
	require.NoError(t, store.PinRootAuthority(t.Context(), pin))
	return project, db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: key}, pin
}

// AssertRecordTeammate checks durable and derived agreement, including signed receipts.
func AssertRecordTeammate(t testing.TB, store db.Storage, project db.Project, pin db.RootKeyPin, event db.Event, want string) {
	t.Helper()
	require.Equal(t, want, event.Teammate, "derived %s", event.Type)
	require.Equal(t, want, db.EventTeammate(event.Type, event.Payload), "payload %s", event.Type)
	remote := remoteEventFromStored(event)
	_, _, err := db.ValidateRemoteEventContentHash(remote)
	require.NoError(t, err)
	events, err := store.EventsByUIDs(t.Context(), project.ID, []string{event.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, want, events[0].Teammate)
	var payload map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	switch event.Type {
	case "issue.created":
		issue, err := store.IssueByUID(t.Context(), *event.IssueUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		var metadata, eventMetadata map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(issue.Metadata), &metadata))
		require.NoError(t, json.Unmarshal(payload["metadata"], &eventMetadata))
		for _, values := range []map[string]jsontext.Value{metadata, eventMetadata} {
			var value string
			if raw, present := values["teammate"]; present {
				require.NoError(t, json.Unmarshal(raw, &value))
			}
			require.Equal(t, want, value)
		}
		require.NotContains(t, payload, "teammate", "creation attribution belongs in metadata")
	case "issue.commented":
		var id string
		require.NoError(t, json.Unmarshal(payload["comment_uid"], &id))
		comments, err := store.CommentsByIssue(t.Context(), *event.IssueID)
		require.NoError(t, err)
		found := false
		for _, comment := range comments {
			if comment.UID == id {
				found = true
				require.Equal(t, want, comment.Teammate)
			}
		}
		require.True(t, found)
		if want == "" {
			require.NotContains(t, payload, "teammate")
		} else {
			var value string
			require.NoError(t, json.Unmarshal(payload["teammate"], &value))
			require.Equal(t, want, value)
		}
	case "cron.run.observed":
		var id string
		require.NoError(t, json.Unmarshal(payload["uid"], &id))
		run, err := store.CronRun(t.Context(), project.ID, id)
		require.NoError(t, err)
		if want == "" {
			require.Nil(t, run.Teammate)
			require.NotContains(t, payload, "teammate")
		} else {
			require.NotNil(t, run.Teammate)
			require.Equal(t, want, *run.Teammate)
			var value string
			require.NoError(t, json.Unmarshal(payload["teammate"], &value))
			require.Equal(t, want, value)
		}
	}
	receipts, err := store.AttributionReceiptsAfter(t.Context(), project.UID, 1, 0, 1000)
	require.NoError(t, err)
	found := false
	for _, receipt := range receipts {
		if receipt.EventUID == event.UID {
			found = true
			require.Equal(t, want, receipt.Teammate, "receipt %s", event.Type)
			require.NoError(t, db.VerifyRootReceipt(pin, receipt))
			require.Equal(t, event.ContentHash, receipt.ContentHash)
		}
	}
	require.True(t, found, "signed receipt for %s", event.Type)
}

func checkRecordTeammateAgreement(t *testing.T, store db.Storage, backend Backend) error {
	project, signer, pin := RecordTeammateRoot(t, store)
	job, _ := nativeFederationDefinitions(t, store, project)
	ctx := db.WithRootAttribution(t.Context(), signer, "coordinator")
	for i, c := range RecordTeammateCases() {
		name := fmt.Sprint(i)
		if c.Header == "" && c.Present && c.Body == "" {
			name = "empty_suppression_minimal"
		}
		t.Run(name, func(t *testing.T) {
			want := c.Want()
			ctx := db.WithEventTeammate(ctx, want)
			metadata := map[string]jsontext.Value{}
			if c.Present || want != "" {
				raw, err := json.Marshal(want)
				require.NoError(t, err)
				metadata["teammate"] = raw
			}
			issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("Task %d", i), Author: "coordinator", Metadata: metadata})
			require.NoError(t, err)
			_, commented, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "coordinator", Body: "Finding", Teammate: want})
			require.NoError(t, err)
			runID, err := uid.New()
			require.NoError(t, err)
			var handle *string
			if want != "" {
				handle = &want
			}
			run, err := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "coordinator", Teammate: handle, Status: "running", Summary: cron.Summary{Version: 1}})
			require.NoError(t, err)
			require.Len(t, run.Events, 1)
			for _, event := range []db.Event{created, commented, run.Events[0]} {
				AssertRecordTeammate(t, store, project, pin, event, want)
			}
		})
	}
	peer := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, peer.Close()) })
	local, err := peer.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	adopted, err := peer.AdoptProjectIntoFederation(t.Context(), db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "coordinator", EmptyOnly: true})
	require.NoError(t, err)
	events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 1000})
	require.NoError(t, err)
	for _, event := range events {
		inserted, err := peer.InsertRemoteEvent(db.WithEventTeammate(t.Context(), "ignored-agent"), adopted.Project.ID, remoteEventFromStored(event))
		require.NoError(t, err)
		require.True(t, inserted)
	}
	fold, err := foldProjectIssues(t.Context(), peer, adopted.Project.ID)
	require.NoError(t, err)
	for _, event := range events {
		var payload map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
		switch event.Type {
		case "issue.created":
			value, _ := db.IssueTeammate(db.JSONBlob(fold.IssueMetadata[*event.IssueUID]))
			require.Equal(t, event.Teammate, value)
		case "issue.commented":
			var id string
			require.NoError(t, json.Unmarshal(payload["comment_uid"], &id))
			require.Equal(t, event.Teammate, fold.Comments[id].Teammate)
		case "cron.run.observed":
			var id string
			require.NoError(t, json.Unmarshal(payload["uid"], &id))
			run := fold.CronRuns[id]
			if event.Teammate == "" {
				require.Nil(t, run.Teammate)
			} else {
				require.NotNil(t, run.Teammate)
				require.Equal(t, event.Teammate, *run.Teammate)
			}
		}
	}
	return nil
}
