package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

// The oracle is the generated source E, never the production teammate extractor.
// A batch sender W and its replaying successor must not become audit authorship.
func TestPropertyForwardedClaimAuditAttributionAndReplay(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, handler, token := forwardedAuditPropertyHub(t, store)
		var nextSourceID int64
		check := func(tb testing.TB, handles []string, emptyForm int, sender, replaySender string) {
			var sources []db.RemoteEvent
			var ids []int64
			for _, handle := range handles {
				issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Queued source task", Author: "tester"})
				require.NoError(tb, err)
				seedForwardedAuditClaim(tb, store, project.ID, issue.UID)
				suffix := ""
				if handle != "" {
					suffix = `, "teammate":"` + handle + `"`
				} else {
					switch emptyForm {
					case 1:
						suffix = `, "teammate":null`
					case 2:
						suffix = `, "teammate":""`
					}
				}
				// Keep exact raw JSON bytes and an integer beyond binary64 precision.
				payload := fmt.Sprintf(`{ "issue_uid":"%s", "reason":"done", "closed_at":"2026-05-23T12:00:00.000Z", "source_marker":9007199254740993%s }`, issue.UID, suffix)
				source := forwardedAuditSource(tb, project, issue.UID, "issue.closed", payload)
				sources = append(sources, source)
				nextSourceID++
				ids = append(ids, nextSourceID)
			}
			body := forwardedAuditBatch(tb, sources, ids)
			sendForwardedAuditBatch(tb, handler, project.ID, token, sender, body)
			before := make([][]db.Event, len(sources))
			for i, source := range sources {
				audits, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, IssueUID: *source.IssueUID, Types: []string{"claim.violated", "claim.released"}, Limit: 10})
				require.NoError(tb, err)
				require.Len(tb, audits, 2)
				require.Equal(tb, "claim.violated", audits[0].Type)
				require.Equal(tb, "claim.released", audits[1].Type)
				for _, event := range audits {
					require.Equal(tb, handles[i], event.Teammate, "source %s audit %s", source.EventUID, event.Type)
					require.Equal(tb, "tester", event.Actor)
					if event.Type == "claim.violated" {
						var payload struct {
							OffendingEventUID string `json:"offending_event_uid"`
						}
						require.NoError(tb, json.Unmarshal([]byte(event.Payload), &payload))
						require.Equal(tb, source.EventUID, payload.OffendingEventUID)
					}
				}
				before[i] = audits
				assertForwardedAuditSource(tb, store, project.ID, source, handles[i])
				// A duplicate close must not release this later live claim.
				seedForwardedAuditClaim(tb, store, project.ID, *source.IssueUID)
			}
			sendForwardedAuditBatch(tb, handler, project.ID, token, replaySender, body)
			for i, source := range sources {
				after, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, IssueUID: *source.IssueUID, Types: []string{"claim.violated", "claim.released"}, Limit: 10})
				require.NoError(tb, err)
				require.Equal(tb, before[i], after, "replay must retain exact audit rows")
				live, err := store.ClaimStatusReadOnly(t.Context(), project.ID, *source.IssueUID, time.Now().UTC())
				require.NoError(tb, err)
				require.True(tb, live.Held, "duplicate source must not release a later claim")
				assertForwardedAuditSource(tb, store, project.ID, source, handles[i])
			}
		}
		for form := range 3 {
			check(t, []string{strings.Repeat("a", 64), "Z", ""}, form, "batch-sender", "replay-sender")
		}
		hegel.Test(t, func(ht *hegel.T) {
			handles := []string{drawForwardedAuditHandle(ht, 0), drawForwardedAuditHandle(ht, 1), ""}
			sender := distinctForwardedAuditHandle(drawForwardedAuditHandle(ht, 1), handles)
			replay := distinctForwardedAuditHandle(drawForwardedAuditHandle(ht, 1), append(append([]string{}, handles...), sender))
			check(ht, handles, hegel.Draw(ht, hegel.Integers(0, 2)), sender, replay)
		}, hegel.WithTestCases(32), hegel.WithSeed(4404), hegel.WithDatabase(t.TempDir()))
	})
}

func TestForwardedCreationMetadataTeammateControlsPeerAudit(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, handler, token := forwardedAuditPropertyHub(t, store)
		for i, metadata := range []string{`{"teammate":"metadata-worker"}`, `{"teammate":""}`, `{}`} {
			peer, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Claimed peer", Author: "tester"})
			require.NoError(t, err)
			seedForwardedAuditClaim(t, store, project.ID, peer.UID)
			sourceIssueUID, err := uid.New()
			require.NoError(t, err)
			payload := fmt.Sprintf(`{ "uid":"%s", "title":"Linked source", "author":"tester", "status":"open", "metadata":%s, "links":[{"type":"blocks","to_issue_uid":"%s","author":"tester"}], "created_at":"2026-05-23T12:00:00.000Z" }`, sourceIssueUID, metadata, peer.UID)
			source := forwardedAuditSource(t, project, sourceIssueUID, "issue.created", payload)
			sendForwardedAuditBatch(t, handler, project.ID, token, "unrelated-sender", forwardedAuditBatch(t, []db.RemoteEvent{source}, []int64{int64(i + 1)}))
			audits, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, IssueUID: peer.UID, Types: []string{"claim.violated"}, Limit: 10})
			require.NoError(t, err)
			require.Len(t, audits, 1)
			want := ""
			if i == 0 {
				want = "metadata-worker"
			}
			require.Equal(t, want, audits[0].Teammate)
			require.Equal(t, "tester", audits[0].Actor)
			var auditPayload struct {
				OffendingEventUID string `json:"offending_event_uid"`
			}
			require.NoError(t, json.Unmarshal([]byte(audits[0].Payload), &auditPayload))
			require.Equal(t, source.EventUID, auditPayload.OffendingEventUID)
			assertForwardedAuditSource(t, store, project.ID, source, want)
		}
	})
}

func drawForwardedAuditHandle(ht *hegel.T, minimum int) string {
	const first = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	const rest = first + "._-"
	size := hegel.Draw(ht, hegel.Integers(minimum, 64))
	out := make([]byte, size)
	for i := range out {
		chars := rest
		if i == 0 {
			chars = first
		}
		out[i] = chars[hegel.Draw(ht, hegel.Integers(0, len(chars)-1))]
	}
	return string(out)
}

// Construct the unrelated-sender relation without discarding generated cases.
func distinctForwardedAuditHandle(handle string, excluded []string) string {
	for {
		collision := false
		for _, other := range excluded {
			collision = collision || handle == other
		}
		if !collision {
			return handle
		}
		const first = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		handle = string(first[(strings.IndexByte(first, handle[0])+1)%len(first)]) + handle[1:]
	}
}

func forwardedAuditPropertyHub(t *testing.T, store db.Storage) (db.Project, http.Handler, string) {
	t.Helper()
	project, err := store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(t.Context(), project.ID, "tester")
	require.NoError(t, err)
	enrollment, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "property-ingest-token", SpokeInstanceUID: federationTestSpokeUID, ProjectID: &project.ID, Capabilities: "push", Actor: "tester"})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return project, server.Handler(), enrollment.Token
}

func seedForwardedAuditClaim(tb testing.TB, store db.Storage, projectID int64, issueUID string) {
	tb.Helper()
	result, err := store.AcquireClaim(tb.Context(), db.AcquireClaimParams{ProjectID: projectID, IssueRef: issueUID, Principal: db.ClaimPrincipal{HolderInstanceUID: federationTestSpokeUID, Holder: "other-holder", ClientKind: "cli"}, ClaimKind: "hard", Now: time.Now().UTC()})
	require.NoError(tb, err)
	require.True(tb, result.Granted)
}

func forwardedAuditSource(tb testing.TB, project db.Project, issueUID, eventType, payload string) db.RemoteEvent {
	tb.Helper()
	eventUID, err := uid.New()
	require.NoError(tb, err)
	event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: federationTestSpokeUID, ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &issueUID, Type: eventType, Actor: "tester", HLCPhysicalMS: 101, Payload: jsontext.Value(payload), CreatedAt: time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)}
	event.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, CreatedAt: event.CreatedAt.Format(db.EventTimestampFormat), Payload: event.Payload})
	require.NoError(tb, err)
	return event
}

func forwardedAuditBatch(tb testing.TB, sources []db.RemoteEvent, ids []int64) string {
	tb.Helper()
	var envelopes []string
	for i, source := range sources {
		fields := map[string]any{"event_id": ids[i], "event_uid": source.EventUID, "origin_instance_uid": source.OriginInstanceUID, "project_uid": source.ProjectUID, "project_name": source.ProjectName, "issue_uid": source.IssueUID, "type": source.Type, "actor": source.Actor, "hlc_physical_ms": source.HLCPhysicalMS, "hlc_counter": source.HLCCounter, "content_hash": source.ContentHash, "created_at": source.CreatedAt.Format(db.EventTimestampFormat)}
		raw, err := json.Marshal(fields)
		require.NoError(tb, err)
		// Compose the outer envelope around frozen payload bytes; normal marshaling
		// of RawMessage would compact them before the real ingestion boundary.
		envelopes = append(envelopes, string(raw[:len(raw)-1])+`,"payload":`+string(source.Payload)+`}`)
	}
	return fmt.Sprintf(`{"schema_version":%d,"events":[%s]}`, db.CurrentSchemaVersion(), strings.Join(envelopes, ","))
}

func sendForwardedAuditBatch(tb testing.TB, handler http.Handler, projectID int64, token, sender, body string) {
	tb.Helper()
	request := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, projectPath(projectID)+"/federation/events:ingest", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Kata-Teammate", sender)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(tb, http.StatusOK, response.Code, response.Body.String())
}

func assertForwardedAuditSource(tb testing.TB, store db.Storage, projectID int64, source db.RemoteEvent, teammate string) {
	tb.Helper()
	events, err := store.EventsByUIDs(tb.Context(), projectID, []string{source.EventUID})
	require.NoError(tb, err)
	require.Len(tb, events, 1)
	require.Equal(tb, source.ContentHash, events[0].ContentHash)
	require.Equal(tb, string(source.Payload), events[0].Payload)
	require.Equal(tb, source.Actor, events[0].Actor)
	require.Equal(tb, source.OriginInstanceUID, events[0].OriginInstanceUID)
	require.Equal(tb, teammate, events[0].Teammate)
}
