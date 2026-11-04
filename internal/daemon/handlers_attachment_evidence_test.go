package daemon_test

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
)

const closeAttachmentID = "01K7E222222222222222222222"

func closeAttachmentEntry() metadata.Attachment {
	return metadata.Attachment{Name: "final-report.pdf", MediaType: "application/pdf", Size: 12, DocbankVersion: uuid.MustParse(linkVersion)}
}

func TestCloseAttachmentSnapshotDryRunAndReceipt(t *testing.T) {
	env := testenv.New(t) // No Docbank client: citing an existing reference is offline.
	p, iss := seedProjectAndIssue(t, env)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", p.ID, iss.ShortID)
	input := map[string]any{"actor": "operator", "reason": "done", "message": "Delivered the requested report and checked its final contents carefully.", "evidence": []any{map[string]any{"type": "attachment", "attachment_id": closeAttachmentID}}, "dry_run": true, "retry_protocol": api.CloseRetryProtocol}
	resp, raw := envDoRaw(t, env, http.MethodPost, endpoint, input, nil)
	assertAPIError(t, resp.StatusCode, raw, 400, "validation")
	entry := closeAttachmentEntry()
	encoded, err := json.Marshal(entry)
	require.NoError(t, err)
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: "operator", Patch: map[string]jsontext.Value{metadata.AttachmentPrefix + closeAttachmentID: encoded}})
	require.NoError(t, err)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	resp, raw = envDoRaw(t, env, http.MethodPost, endpoint, input, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "dry-run emits nothing")
	input["dry_run"] = false
	headers := map[string]string{"Idempotency-Key": "attachment-close"}
	resp, raw = envDoRaw(t, env, http.MethodPost, endpoint, input, headers)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	closed := api.MutationResponse{}.Body
	require.NoError(t, json.Unmarshal(raw, &closed))
	require.True(t, closed.Changed)
	require.NotNil(t, closed.Event)
	var payload struct {
		Evidence []db.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(closed.Event.Payload), &payload))
	require.Len(t, payload.Evidence, 1)
	require.Equal(t, &entry, payload.Evidence[0].Attachment)
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: "operator", Patch: map[string]jsontext.Value{metadata.AttachmentPrefix + closeAttachmentID: nil}})
	require.NoError(t, err)
	resp, raw = envDoRaw(t, env, http.MethodPost, endpoint, input, headers)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	replayed := api.MutationResponse{}.Body
	require.NoError(t, json.Unmarshal(raw, &replayed))
	require.NotNil(t, replayed.OriginalEvent)
	require.Equal(t, closed.Event.UID, replayed.OriginalEvent.UID)
}

type attachmentCloseRaceStore struct {
	db.Storage
	patch map[string]jsontext.Value
}

func (s *attachmentCloseRaceStore) CloseIssueGuarded(ctx context.Context, p db.CloseIssueParams) (db.Issue, []db.Event, bool, error) {
	if s.patch != nil {
		patch := s.patch
		s.patch = nil
		if _, err := s.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: p.IssueID, Actor: "operator", Patch: patch}); err != nil {
			return db.Issue{}, nil, false, err
		}
	}
	return s.Storage.CloseIssueGuarded(ctx, p)
}

func TestCloseAttachmentChecksCurrentTransactionMetadata(t *testing.T) {
	var race *attachmentCloseRaceStore
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { race = &attachmentCloseRaceStore{Storage: cfg.DB}; cfg.DB = race })
	checkCloseAttachmentCurrentMetadata(t, env, env.DB, race)
}
func TestCloseAttachmentChecksCurrentPostgresTransactionMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	race := &attachmentCloseRaceStore{Storage: store}
	d := daemon.NewServer(daemon.ServerConfig{DB: race, StartedAt: time.Now().UTC(), Broadcaster: daemon.NewEventBroadcaster()})
	t.Cleanup(func() { _ = d.Close() })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	checkCloseAttachmentCurrentMetadata(t, &testenv.Env{URL: server.URL, HTTP: server.Client()}, store, race)
}
func checkCloseAttachmentCurrentMetadata(t *testing.T, env *testenv.Env, store db.Storage, race *attachmentCloseRaceStore) {
	t.Helper()
	p, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	for _, removed := range []bool{true, false} {
		t.Run(fmt.Sprint(removed), func(t *testing.T) {
			iss, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Deliver report", Author: "operator"})
			require.NoError(t, err)
			encoded, err := json.Marshal(closeAttachmentEntry())
			require.NoError(t, err)
			key := metadata.AttachmentPrefix + closeAttachmentID
			_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: "operator", Patch: map[string]jsontext.Value{key: encoded}})
			require.NoError(t, err)
			next := closeAttachmentEntry()
			next.Name = "new-report.pdf"
			next.DocbankVersion = uuid.MustParse("6f1c2e8a-3333-4333-8333-333333333333")
			replacement, err := json.Marshal(next)
			require.NoError(t, err)
			if removed {
				replacement = nil
			}
			race.patch = map[string]jsontext.Value{key: replacement}
			input := map[string]any{"actor": "operator", "reason": "done", "message": attachmentCloseTestMessage, "evidence": []any{map[string]any{"type": "attachment", "attachment_id": closeAttachmentID}}}
			resp, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", p.ID, iss.ShortID), input, nil)
			if removed {
				assertAPIError(t, resp.StatusCode, raw, 400, "validation")
				stored, err := store.IssueByID(t.Context(), iss.ID)
				require.NoError(t, err)
				require.Equal(t, "open", stored.Status)
			} else {
				require.Equal(t, 200, resp.StatusCode, string(raw))
				result := api.MutationResponse{}.Body
				require.NoError(t, json.Unmarshal(raw, &result))
				var payload struct {
					Evidence []db.Evidence `json:"evidence"`
				}
				require.NoError(t, json.Unmarshal([]byte(result.Event.Payload), &payload))
				require.Equal(t, &next, payload.Evidence[0].Attachment)
			}
		})
	}
}

const attachmentCloseTestMessage = "Delivered the requested report and checked its final contents carefully."

func TestMovedIssueHistoryRetainsAttachmentEvidence(t *testing.T) {
	env := testenv.New(t)
	project, issue := seedProjectAndIssue(t, env)
	destination, err := env.DB.CreateProject(t.Context(), "destination-project")
	require.NoError(t, err)
	encoded, err := json.Marshal(closeAttachmentEntry())
	require.NoError(t, err)
	key := metadata.AttachmentPrefix + closeAttachmentID
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{key: encoded}})
	require.NoError(t, err)
	input := map[string]any{"actor": "operator", "reason": "done", "message": attachmentCloseTestMessage, "evidence": []any{map[string]any{"type": "attachment", "attachment_id": closeAttachmentID}}}
	resp, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", project.ID, issue.ShortID), input, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{key: nil}})
	require.NoError(t, err)
	current, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: project.ID, ToProjectID: destination.ID, IfMatchRev: current.Revision, Actor: "operator"})
	require.NoError(t, err)
	resp, raw = envGetRaw(t, env, fmt.Sprintf("/api/v1/projects/%d/events?issue=%s", destination.ID, issue.UID))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Equal(t, issue.UID, resp.Header.Get("X-Kata-Events-Issue-UID"))
	var page api.PollEventsBody
	require.NoError(t, json.Unmarshal(raw, &page))
	for _, event := range page.Events {
		if event.Type != "issue.closed" {
			continue
		}
		var payload struct {
			Evidence []db.Evidence `json:"evidence"`
		}
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		require.Len(t, payload.Evidence, 1)
		require.Equal(t, closeAttachmentEntry(), *payload.Evidence[0].Attachment)
		return
	}
	t.Fatal("moved issue history omitted the close event")
}
