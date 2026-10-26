package daemon_test

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

// One actor remains one actor; the teammate set must not change report counts.
func TestPropertyDigestTeammateProjection(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		project, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "test-token"}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		var seq int
		check := func(tb testing.TB, handles []string) {
			seq++
			actor := fmt.Sprintf("actor-%d", seq)
			for _, h := range handles {
				_, _, err := store.CreateIssue(db.WithEventTeammate(t.Context(), h), db.CreateIssueParams{ProjectID: project.ID, Title: "Digest task", Author: actor, Metadata: map[string]jsontext.Value{"teammate": jsontext.Value(strconv.Quote(h))}})
				require.NoError(tb, err)
			}
			q := url.Values{"since": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}, "until": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, "actor": {actor}}
			for _, path := range []string{"/api/v1/digest", fmt.Sprintf("/api/v1/projects/%d/digest", project.ID)} {
				req := httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), nil)
				req.Header.Set("Authorization", "Bearer test-token")
				out := httptest.NewRecorder()
				server.Handler().ServeHTTP(out, req)
				require.Equal(tb, 200, out.Code, out.Body.String())
				var got struct {
					EventCount int `json:"event_count"`
					Totals     struct {
						Created int `json:"created"`
					} `json:"totals"`
					Actors []struct {
						Actor     string   `json:"actor"`
						Teammates []string `json:"teammates"`
						Totals    struct {
							Created int `json:"created"`
						} `json:"totals"`
					} `json:"actors"`
				}
				require.NoError(tb, json.Unmarshal(out.Body.Bytes(), &got))
				require.Len(tb, got.Actors, 1)
				require.Equal(tb, actor, got.Actors[0].Actor)
				require.Equal(tb, len(handles), got.Totals.Created)
				require.Equal(tb, len(handles), got.Actors[0].Totals.Created)
				require.Equal(tb, len(handles), got.EventCount)
				want := []string{}
				seen := map[string]bool{}
				for _, h := range handles {
					if h != "" && !seen[h] {
						want = append(want, h)
						seen[h] = true
					}
				}
				sort.Strings(want)
				require.NotNil(tb, got.Actors[0].Teammates, "teammates must always be a JSON array")
				require.Equal(tb, want, got.Actors[0].Teammates)
			}
		}
		check(t, []string{"codex", "claude", "codex", ""})
		check(t, []string{"", ""})
		check(t, []string{strings.Repeat("Z", 64), "a._-9"})
		hegel.Test(t, func(ht *hegel.T) {
			a, b := drawForwardedAuditHandle(ht, 0), drawForwardedAuditHandle(ht, 0)
			check(ht, []string{a, b, a, ""})
		}, hegel.WithTestCases(32), hegel.WithSeed(5101), hegel.WithDatabase(t.TempDir()))
	})
}

func TestAuditCloseTeammateProjection(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, event, _, err := store.CloseIssue(db.WithEventTeammate(t.Context(), "source-worker"), f.visible.ID, "done", "member", "Completed this task after focused checks.", []db.Evidence{{Type: "test", Command: "go test ./example"}})
		require.NoError(t, err)
		require.NotNil(t, event)
		for _, actor := range []string{"member", "nonmember"} {
			status, _, body := f.request(t, "GET", fmt.Sprintf("/api/v1/audit/closes?project_id=%d", f.public.ID), actor, nil, nil)
			require.Equal(t, 200, status, string(body))
			var got struct {
				Rows []struct {
					Actor    string `json:"actor"`
					Teammate string `json:"teammate"`
					EventID  int64  `json:"event_id"`
				}
			}
			require.NoError(t, json.Unmarshal(body, &got))
			require.Len(t, got.Rows, 1)
			require.Equal(t, "member", got.Rows[0].Actor)
			require.Equal(t, "source-worker", got.Rows[0].Teammate)
			require.Equal(t, event.ID, got.Rows[0].EventID)
		}
	})
}

func TestShowCloseTeammateLifecycleAndScope(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		check := func(issue db.Issue, actor, teammate string, want bool) {
			for _, reader := range []string{"member", "nonmember"} {
				for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/issues/%s", issue.ProjectID, issue.ShortID), "/api/v1/issues/" + issue.UID} {
					status, _, body := f.request(t, "GET", path, reader, nil, nil)
					if issue.ProjectID == f.private.ID && reader == "nonmember" {
						require.Equal(t, 404, status, string(body))
						if teammate != "" {
							require.NotContains(t, string(body), teammate)
						}
						continue
					}
					require.Equal(t, 200, status, string(body))
					var got struct {
						Close *struct {
							Actor    string `json:"actor"`
							Teammate string `json:"teammate"`
							EventID  int64  `json:"event_id"`
							Reason   string `json:"reason"`
						} `json:"close"`
					}
					require.NoError(t, json.Unmarshal(body, &got))
					if !want {
						require.Nil(t, got.Close, string(body))
						continue
					}
					require.NotNil(t, got.Close, string(body))
					require.Equal(t, actor, got.Close.Actor)
					require.Equal(t, teammate, got.Close.Teammate)
					require.Positive(t, got.Close.EventID)
					require.Equal(t, "done", got.Close.Reason)
				}
			}
		}
		for _, issue := range []db.Issue{f.visible, f.issue} {
			check(issue, "", "", false)
			_, _, _, err := store.CloseIssue(db.WithEventTeammate(t.Context(), "first-worker"), issue.ID, "done", "member", "Completed first cycle with focused checks.", nil)
			require.NoError(t, err)
			check(issue, "member", "first-worker", true)
			_, _, _, err = store.ReopenIssue(t.Context(), issue.ID, "member")
			require.NoError(t, err)
			check(issue, "", "", false)
			_, _, _, err = store.CloseIssue(db.WithEventTeammate(t.Context(), "last-worker"), issue.ID, "done", "nonmember", "Completed second cycle with focused checks.", nil)
			require.NoError(t, err)
			check(issue, "nonmember", "last-worker", true)
		}
	})
}

func TestShowCloseSummaryDefaultsMissingFederatedReason(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(t.Context(), f.public.ID, "member")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Imported close", Author: "creator",
		})
		require.NoError(t, err)
		remote := displayRemoteStatusEvent(t, f.public, issue.UID, "issue.closed", "remote-closer", "source-worker", time.Now().Add(time.Hour).UnixMilli(), nil)
		_, err = store.IngestFederationEvents(t.Context(), db.FederationIngestParams{
			ProjectID: f.public.ID, SpokeInstanceUID: federationTestSpokeUID,
			Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: remote}},
		})
		require.NoError(t, err)

		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, issue.ShortID), "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var got struct {
			Close *struct {
				Reason string `json:"reason"`
			} `json:"close"`
		}
		require.NoError(t, json.Unmarshal(body, &got))
		require.NotNil(t, got.Close, string(body))
		require.Equal(t, "done", got.Close.Reason)
	})
}

type closeSummaryStorageCounter struct {
	db.Storage
	calls             atomic.Int64
	eventPageCalls    atomic.Int64
	eventRowsReturned atomic.Int64
	summaryIssueUID   string
}

func (s *closeSummaryStorageCounter) IssueByID(ctx context.Context, id int64) (db.Issue, error) {
	s.calls.Add(1)
	return s.Storage.IssueByID(ctx, id)
}

func (s *closeSummaryStorageCounter) EventsAfter(ctx context.Context, params db.EventsAfterParams) ([]db.Event, error) {
	track := params.IssueUID == s.summaryIssueUID && hasEventType(params.Types, "issue.closed")
	if track {
		s.eventPageCalls.Add(1)
	}
	events, err := s.Storage.EventsAfter(ctx, params)
	if track {
		s.eventRowsReturned.Add(int64(len(events)))
	}
	return events, err
}

func hasEventType(types []string, target string) bool {
	return slices.Contains(types, target)
}

func TestScopedShowCloseSummaryDoesNotLookUpIssuePerLifecycleEvent(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		expiresAt := time.Now().UTC().Add(time.Hour)
		//nolint:gosec // Deterministic test credential.
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
			PlaintextToken: "scoped-close-summary-test-token",
			Actor:          "member", AdminActor: db.BootstrapActor,
			ExpiresAt: &expiresAt,
			Scope: &db.APITokenScope{
				Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: f.visible.UID,
			},
		})
		require.NoError(t, err)
		for range 12 {
			_, _, _, err = store.CloseIssue(t.Context(), f.visible.ID, "done", "member", "Completed a focused history cycle.", nil)
			require.NoError(t, err)
			_, _, _, err = store.ReopenIssue(t.Context(), f.visible.ID, "member")
			require.NoError(t, err)
		}
		_, _, _, err = store.CloseIssue(t.Context(), f.visible.ID, "done", "member", "Completed the current history cycle.", nil)
		require.NoError(t, err)

		counted := &closeSummaryStorageCounter{Storage: store}
		f = projectAccessFixtureWithStorage(t, f, counted)
		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID), "", nil,
			map[string]string{"Authorization": "Bearer scoped-close-summary-test-token"})
		require.Equal(t, http.StatusOK, status, string(body))
		require.LessOrEqual(t, counted.calls.Load(), int64(3),
			"scoped close-history visibility should use bounded issue lookups as lifecycle history grows")
	})
}

func TestShowCloseSummaryDoesNotPageOrdinaryIssueUpdates(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: f.public.ID, Title: "Frequently edited task", Author: "member",
		})
		require.NoError(t, err)
		for i := range 255 {
			title := fmt.Sprintf("Frequently edited task %d", i)
			_, err = store.EditIssueAtomic(t.Context(), db.EditIssueAtomicParams{
				IssueID: issue.ID, Actor: "member", Title: &title,
			})
			require.NoError(t, err)
		}
		_, _, _, err = store.CloseIssue(t.Context(), issue.ID, "done", "member", "Completed after many ordinary edits.", nil)
		require.NoError(t, err)

		counted := &closeSummaryStorageCounter{Storage: store, summaryIssueUID: issue.UID}
		f = projectAccessFixtureWithStorage(t, f, counted)
		status, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, issue.ShortID), "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		require.LessOrEqual(t, counted.eventPageCalls.Load(), int64(1),
			"ordinary title edits should not grow close-summary history paging")
		require.LessOrEqual(t, counted.eventRowsReturned.Load(), int64(4),
			"close-summary history should load lifecycle events, not ordinary title edits")
		var got struct {
			Close *struct {
				Actor string `json:"actor"`
			} `json:"close"`
		}
		require.NoError(t, json.Unmarshal(body, &got))
		require.NotNil(t, got.Close, string(body))
		require.Equal(t, "member", got.Close.Actor)
	})
}

func TestPropertyShowClosePortableOrder(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, enableErr := store.EnableProjectFederation(t.Context(), f.public.ID, "member")
		require.NoError(t, enableErr)
		var sourceID int64
		check := func(tb testing.TB, order []int) {
			issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.public.ID, Title: "Imported lifecycle", Author: "creator"})
			require.NoError(tb, err)
			base := time.Now().Add(time.Hour).UnixMilli()
			events := []db.RemoteEvent{
				displayRemoteStatusEvent(tb, f.public, issue.UID, "issue.closed", "old-closer", "old-worker", base, map[string]any{"reason": "done"}),
				displayRemoteStatusEvent(tb, f.public, issue.UID, "issue.reopened", "reopener", "", base+1, nil),
				displayRemoteStatusEvent(tb, f.public, issue.UID, "issue.closed", "current-closer", "current-worker", base+2, map[string]any{"reason": "done"}),
			}
			for _, i := range order {
				sourceID++
				_, err = store.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: f.public.ID, SpokeInstanceUID: federationTestSpokeUID, Events: []db.FederationIngestEvent{{SourceEventID: sourceID, Event: events[i]}}})
				require.NoError(tb, err)
			}
			path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, issue.ShortID)
			status, _, body := f.request(t, "GET", path, "member", nil, nil)
			require.Equal(tb, 200, status, string(body))
			var got struct {
				Close *struct {
					Actor    string
					Teammate string
				} `json:"close"`
			}
			require.NoError(tb, json.Unmarshal(body, &got))
			require.NotNil(tb, got.Close, string(body))
			require.Equal(tb, "current-closer", got.Close.Actor)
			require.Equal(tb, "current-worker", got.Close.Teammate)
		}
		check(t, []int{2, 0, 1})
		check(t, []int{1, 2, 0})
		hegel.Test(t, func(ht *hegel.T) {
			order := []int{0, 1, 2}
			for i := 2; i > 0; i-- {
				j := hegel.Draw(ht, hegel.Integers(0, i))
				order[i], order[j] = order[j], order[i]
			}
			check(ht, order)
		}, hegel.WithTestCases(12), hegel.WithSeed(5102), hegel.WithDatabase(t.TempDir()))
	})
}

func TestShowCloseSnapshotProvenance(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, enableErr := store.EnableProjectFederation(t.Context(), f.public.ID, "member")
		require.NoError(t, enableErr)
		for _, tc := range []struct {
			name            string
			kinds, statuses []string
			want            bool
		}{
			{"snapshot only", []string{"issue.snapshot"}, []string{"closed"}, false},
			{"closed snapshot retains explicit close", []string{"issue.closed", "issue.snapshot"}, []string{"closed", "closed"}, true},
			{"opposite update clears close", []string{"issue.closed", "issue.updated", "issue.snapshot"}, []string{"closed", "open", "closed"}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.public.ID, Title: "Snapshot task", Author: "creator"})
				require.NoError(t, err)
				var events []db.RemoteEvent
				for i, k := range tc.kinds {
					ev := displayRemoteStatusEvent(t, f.public, issue.UID, k, "closer", "source-worker", time.Now().Add(time.Hour).UnixMilli()+int64(i), map[string]any{"uid": issue.UID, "title": "Snapshot task", "status": tc.statuses[i], "reason": "done"})
					events = append(events, ev)
				}
				_, err = store.InsertRemoteEvents(t.Context(), f.public.ID, events)
				require.NoError(t, err)
				err = store.MaterializeFederatedProject(t.Context(), f.public.ID)
				require.NoError(t, err)
				status, _, body := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, issue.ShortID), "member", nil, nil)
				require.Equal(t, 200, status, string(body))
				var got struct {
					Issue struct{ Status string }
					Close *struct {
						Actor    string
						Teammate string
					} `json:"close"`
				}
				require.NoError(t, json.Unmarshal(body, &got))
				require.Equal(t, "closed", got.Issue.Status)
				if tc.want {
					require.NotNil(t, got.Close)
					require.Equal(t, "closer", got.Close.Actor)
					require.Equal(t, "source-worker", got.Close.Teammate)
				} else {
					require.Nil(t, got.Close, string(body))
				}
			})
		}
	})
}

func displayRemoteStatusEvent(tb testing.TB, project db.Project, issueUID, kind, actor, teammate string, clock int64, fields map[string]any) db.RemoteEvent {
	tb.Helper()
	if fields == nil {
		fields = map[string]any{}
	}
	fields["issue_uid"] = issueUID
	if teammate != "" {
		fields["teammate"] = teammate
	}
	payload, err := json.Marshal(fields)
	require.NoError(tb, err)
	eventUID, err := uid.New()
	require.NoError(tb, err)
	ev := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: federationTestSpokeUID, ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &issueUID, Type: kind, Actor: actor, HLCPhysicalMS: clock, CreatedAt: time.Now().UTC().Truncate(time.Millisecond), Payload: jsontext.Value(payload)}
	ev.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: ev.EventUID, OriginInstanceUID: ev.OriginInstanceUID, ProjectUID: ev.ProjectUID, ProjectName: ev.ProjectName, IssueUID: ev.IssueUID, Type: ev.Type, Actor: ev.Actor, HLCPhysicalMS: ev.HLCPhysicalMS, CreatedAt: ev.CreatedAt.Format("2006-01-02T15:04:05.000Z"), Payload: ev.Payload})
	require.NoError(tb, err)
	return ev
}

func TestShowCloseMovedHistoryAuthorization(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		closed, _, _, err := store.CloseIssue(db.WithEventTeammate(t.Context(), "source-worker"), f.issue.ID, "done", "member", "Completed task before moving it to another project.", nil)
		require.NoError(t, err)
		moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: closed.ID, FromProjectID: f.private.ID, ToProjectID: f.public.ID, IfMatchRev: closed.Revision, Actor: "member"})
		require.NoError(t, err)
		expiry := time.Now().Add(time.Hour)
		_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "scoped-test-token", Actor: "member", AdminActor: db.BootstrapActor, ExpiresAt: &expiry, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: moved.Issue.UID}}) //nolint:gosec // Deterministic test credential.
		require.NoError(t, err)
		for _, actor := range []string{"member", "nonmember", "scoped"} {
			status, _, body := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, moved.NewShortID), actor, nil, nil)
			require.Equal(t, 200, status, string(body))
			var got struct {
				Close *struct {
					Actor    string
					Teammate string
				}
			}
			require.NoError(t, json.Unmarshal(body, &got))
			if actor == "member" {
				require.NotNil(t, got.Close, string(body))
				require.Equal(t, "member", got.Close.Actor)
				require.Equal(t, "source-worker", got.Close.Teammate)
			} else {
				require.Nil(t, got.Close, string(body))
				require.NotContains(t, string(body), "source-worker")
			}
		}
	})
}

func TestShowCloseIssueScopedCaller(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		expiry := time.Now().Add(time.Hour)
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "scoped-token", Actor: "reader", AdminActor: db.BootstrapActor, ExpiresAt: &expiry, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: f.visible.UID}})
		require.NoError(t, err)
		_, _, _, err = store.CloseIssue(db.WithEventTeammate(t.Context(), "source-worker"), f.visible.ID, "done", "member", "Completed task with scoped caller read coverage.", nil)
		require.NoError(t, err)
		for _, issue := range []db.Issue{f.visible, f.issue} {
			status, _, body := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/issues/%s", issue.ProjectID, issue.ShortID), "", nil, map[string]string{"Authorization": "Bearer scoped-token"})
			if issue.ID == f.visible.ID {
				require.Equal(t, 200, status, string(body))
				var got struct {
					Close *struct {
						Actor    string
						Teammate string
					}
				}
				require.NoError(t, json.Unmarshal(body, &got))
				require.NotNil(t, got.Close, string(body))
				require.Equal(t, "source-worker", got.Close.Teammate)
				require.Equal(t, "member", got.Close.Actor)
			} else {
				require.Equal(t, 404, status, string(body))
				require.NotContains(t, string(body), "source-worker")
			}
		}
	})
}

func TestShowClosePaginatesPortableHistory(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(t.Context(), f.public.ID, "member")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.public.ID, Title: "Long lifecycle", Author: "creator"})
		require.NoError(t, err)
		base := time.Now().Add(time.Hour).UnixMilli()
		var events []db.RemoteEvent
		// The newest portable writer arrives first. More than one read page follows,
		// so local row order and stopping after one page both choose the wrong close.
		for i := 259; i >= 0; i-- {
			events = append(events, displayRemoteStatusEvent(t, f.public, issue.UID, "issue.closed", fmt.Sprintf("closer-%d", i), "source-worker", base+int64(i), map[string]any{"reason": "done"}))
		}
		_, err = store.InsertRemoteEvents(t.Context(), f.public.ID, events)
		require.NoError(t, err)
		require.NoError(t, store.MaterializeFederatedProject(t.Context(), f.public.ID))
		status, _, body := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, issue.ShortID), "member", nil, nil)
		require.Equal(t, 200, status, string(body))
		var got struct {
			Close *struct {
				Actor    string
				Teammate string
			}
		}
		require.NoError(t, json.Unmarshal(body, &got))
		require.NotNil(t, got.Close, string(body))
		require.Equal(t, "closer-259", got.Close.Actor)
		require.Equal(t, "source-worker", got.Close.Teammate)
	})
}

func TestShowCloseTimestampRepresentation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(t.Context(), f.public.ID, "member")
		require.NoError(t, err)
		ev := displayRemoteStatusEvent(t, f.public, f.visible.UID, "issue.closed", "closer", "source-worker", time.Now().Add(time.Hour).UnixMilli(), map[string]any{"reason": "done", "closed_at": "2026-05-23T12:00:00Z"})
		_, err = store.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: f.public.ID, SpokeInstanceUID: federationTestSpokeUID, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}}})
		require.NoError(t, err)
		status, _, body := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, f.visible.ShortID), "member", nil, nil)
		require.Equal(t, 200, status, string(body))
		var got struct {
			Close *struct {
				Actor    string
				Teammate string
			}
		}
		require.NoError(t, json.Unmarshal(body, &got))
		require.NotNil(t, got.Close, string(body))
		require.Equal(t, "closer", got.Close.Actor)
		require.Equal(t, "source-worker", got.Close.Teammate)
	})
}

// A timestamp is not unique provenance: a hidden later cycle can close at the
// exact same instant as an older readable cycle before the issue returns home.
func TestShowCloseOmitsSameTimestampHiddenMoveCycle(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		first, firstEvent, _, err := store.CloseIssue(db.WithEventTeammate(t.Context(), "first-worker"), f.visible.ID, "done", "member", "Completed the first visible lifecycle with focused checks.", nil)
		require.NoError(t, err)
		require.NotNil(t, firstEvent)
		require.NotNil(t, first.ClosedAt)
		away, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: first.ID, FromProjectID: f.public.ID, ToProjectID: f.private.ID, IfMatchRev: first.Revision, Actor: "member"})
		require.NoError(t, err)
		_, _, _, err = store.ReopenIssue(t.Context(), away.Issue.ID, "member")
		require.NoError(t, err)
		current, secondEvent, _, err := store.CloseIssue(db.WithEventTeammate(t.Context(), "second-worker"), first.ID, "done", "second-closer", "Completed the second lifecycle in the restricted project.", nil)
		require.NoError(t, err)
		require.NotNil(t, secondEvent)
		require.NotNil(t, current.ClosedAt)
		back, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: f.private.ID, ToProjectID: f.public.ID, IfMatchRev: current.Revision, Actor: "member"})
		require.NoError(t, err)
		// Native timestamps cannot be frozen through both database drivers.
		// Insert a valid delayed historical close with the first close's HLC
		// and the second close's millisecond timestamp. Its UID breaks the old
		// writer's tie, while the native reopen and second close remain later.
		earlier := displayRemoteStatusEvent(t, f.public, first.UID, "issue.closed", "first-closer", "first-worker", firstEvent.HLCPhysicalMS, map[string]any{"reason": "done", "closed_at": current.ClosedAt.UTC().Format(db.EventTimestampFormat)})
		earlier.OriginInstanceUID = firstEvent.OriginInstanceUID
		earlier.HLCCounter = firstEvent.HLCCounter
		earlier.CreatedAt = firstEvent.CreatedAt
		earlier.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: earlier.EventUID, OriginInstanceUID: earlier.OriginInstanceUID, ProjectUID: earlier.ProjectUID, ProjectName: earlier.ProjectName, IssueUID: earlier.IssueUID, Type: earlier.Type, Actor: earlier.Actor, HLCPhysicalMS: earlier.HLCPhysicalMS, HLCCounter: earlier.HLCCounter, CreatedAt: earlier.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: earlier.Payload})
		require.NoError(t, err)
		require.NotEqual(t, earlier.EventUID, secondEvent.UID)
		require.True(t, secondEvent.HLCPhysicalMS > earlier.HLCPhysicalMS || (secondEvent.HLCPhysicalMS == earlier.HLCPhysicalMS && secondEvent.HLCCounter > earlier.HLCCounter), "native second close must be a later portable writer")
		require.Greater(t, earlier.EventUID, firstEvent.UID)
		inserted, err := store.InsertRemoteEvents(t.Context(), f.public.ID, []db.RemoteEvent{earlier})
		require.NoError(t, err)
		require.Equal(t, []bool{true}, inserted)
		expiry := time.Now().Add(time.Hour)
		_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "return-subtree-token", Actor: "nonmember", AdminActor: db.BootstrapActor, ExpiresAt: &expiry, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: current.UID}}) //nolint:gosec // Deterministic test credential.
		require.NoError(t, err)
		for _, reader := range []string{"member", "nonmember", "subtree"} {
			t.Run(reader, func(t *testing.T) {
				actor := reader
				var headers map[string]string
				if reader == "subtree" {
					actor = ""
					headers = map[string]string{"Authorization": "Bearer return-subtree-token"}
				}
				status, _, body := f.request(t, "GET", fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.public.ID, back.NewShortID), actor, nil, headers)
				require.Equal(t, 200, status, string(body))
				var got struct {
					Issue struct{ Status string }
					Close *struct {
						EventID  int64 `json:"event_id"`
						Actor    string
						Teammate string
					}
				}
				require.NoError(t, json.Unmarshal(body, &got))
				require.Equal(t, "closed", got.Issue.Status)
				if reader == "member" {
					require.NotNil(t, got.Close, string(body))
					require.Equal(t, secondEvent.ID, got.Close.EventID, "summary must identify the native second close event, UID %s", secondEvent.UID)
					require.Equal(t, "second-closer", got.Close.Actor)
					require.Equal(t, "second-worker", got.Close.Teammate)
				} else {
					require.Nil(t, got.Close, "filtered history must not substitute the earlier visible close: %s", body)
					require.NotContains(t, string(body), "first-worker")
					require.NotContains(t, string(body), "second-worker")
				}
			})
		}
	})
}
