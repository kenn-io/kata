package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	katauid "go.kenn.io/kata/internal/uid"
)

func TestSyncFederationOncePullsAndAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)

	hubProject, err := hub.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	hubIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     "from hub",
		Author:    "tester",
		Labels:    []string{"area:db"},
	})
	require.NoError(t, err)
	_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{
		IssueID: hubIssue.ID,
		Author:  "tester",
		Body:    "baseline note",
	})
	require.NoError(t, err)

	var meta api.ProjectFederationBody
	postJSON(t, hub.URL, "/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "tester"}, &meta)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "pull-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull",
		Actor:            "tester",
	})
	require.NoError(t, err)

	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
		"actor":                   "tester",
	}, &replica)

	binding, err := spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	})
	require.NoError(t, err)

	mirrored, err := spoke.DB.IssueByUID(ctx, hubIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	syncStatus := requireFederationSyncStatus(t, spoke.DB, replica.Project.ID)
	assertStatusTimeSet(t, syncStatus.LastPullStartedAt)
	assertStatusTimeSet(t, syncStatus.LastPullSuccessAt)
	assert.Nil(t, syncStatus.LastErrorAt)
	assert.Nil(t, syncStatus.LastError)
	assert.Equal(t, "from hub", mirrored.Title)
	assertFoldedIssuesMatch(t, hub.DB, spoke.DB, hubProject.ID, replica.Project.ID, meta.ReplayHorizonEventID-1)

	_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{
		IssueID: hubIssue.ID,
		Author:  "tester",
		Body:    "after cursor",
	})
	require.NoError(t, err)
	beforeSecondSync, err := spoke.DB.MaxEventID(ctx)
	require.NoError(t, err)
	binding, err = spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	})
	require.NoError(t, err)
	afterSecondSync, err := spoke.DB.MaxEventID(ctx)
	require.NoError(t, err)
	assert.Equal(t, beforeSecondSync+1, afterSecondSync, "second sync should pull only the new hub event")
}

func TestClientOptsForCredentialPreservesAllowInsecureOptIn(t *testing.T) {
	opts := clientOptsForCredential(clientpkg.Opts{}, config.FederationCredential{
		AllowInsecure: true,
	})

	assert.True(t, opts.AllowInsecure)
	assert.Equal(t, defaultClientTimeout, opts.Timeout)
}

func TestSyncFederationOnceDuplicateOnlyPullMaterializesStaleProjection(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)

	hubProject, err := hub.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	hubIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     "from hub",
		Author:    "tester",
	})
	require.NoError(t, err)
	var meta api.ProjectFederationBody
	postJSON(t, hub.URL, "/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "tester"}, &meta)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "duplicate-pull-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull",
		Actor:            "tester",
	})
	require.NoError(t, err)
	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
		"actor":                   "tester",
	}, &replica)

	staleBinding, err := spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}
	var delivered []db.Event
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
	require.Error(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, staleBinding, creds, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))
	assert.Empty(t, delivered)
	_, err = spoke.DB.IssueByUID(ctx, hubIssue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)

	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, staleBinding, creds, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))
	mirrored, err := spoke.DB.IssueByUID(ctx, hubIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "from hub", mirrored.Title)
	var foundIssueEvent bool
	for _, event := range delivered {
		if event.IssueUID != nil && *event.IssueUID == hubIssue.UID {
			foundIssueEvent = true
		}
	}
	assert.NotEmpty(t, delivered, "retry of unadvanced duplicate page must deliver pulled events")
	assert.True(t, foundIssueEvent, "delivered events should include the recovered issue event")
}

func TestSyncFederationOnceReportsFreshPulledEvents(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	hubProject, err := hub.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	hubIssue, hubEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     "from hub",
		Author:    "tester",
	})
	require.NoError(t, err)
	var meta api.ProjectFederationBody
	postJSON(t, hub.URL, "/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "tester"}, &meta)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "pull-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull",
		Actor:            "tester",
	})
	require.NoError(t, err)
	spokeProject, err := spoke.DB.CreateProjectWithUID(ctx, "hub", hubProject.UID)
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	var delivered []db.Event

	err = SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}, clientpkg.Opts{}, func(projectID int64, events []db.Event) {
		require.Equal(t, spokeProject.ID, projectID)
		delivered = append(delivered, events...)
	})

	require.NoError(t, err)
	require.NotEmpty(t, delivered)
	var foundCreate bool
	for _, event := range delivered {
		if event.UID == hubEvent.UID {
			foundCreate = true
			require.NotNil(t, event.IssueUID)
			assert.Equal(t, hubIssue.UID, *event.IssueUID)
		}
	}
	assert.True(t, foundCreate, "fresh pulled events should include the hub issue creation")

	binding, err = spoke.DB.FederationBindingByProject(ctx, spokeProject.ID)
	require.NoError(t, err)
	delivered = nil
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))
	assert.Empty(t, delivered, "duplicate/no-op pulls should not be delivered again")
}

func TestSyncFederationOnceAdvancesAcrossIncompleteBaselineLinkPage(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	hubProjectUID := mustTestUID(t)
	sourceUID := mustTestUID(t)
	targetUID := mustTestUID(t)
	project, err := spoke.DB.CreateProjectWithUID(ctx, "hub", hubProjectUID)
	require.NoError(t, err)

	sourcePayload := `{
		"uid":"` + sourceUID + `",
		"short_id":"` + shortIDForSyncTest(sourceUID) + `",
		"title":"source",
		"body":"",
		"author":"hub",
		"status":"open",
		"metadata":{},
		"links":[{"type":"related","to_issue_uid":"` + targetUID + `"}],
		"created_at":"2026-05-23T12:00:00.000Z"
	}`
	targetPayload := `{
		"uid":"` + targetUID + `",
		"short_id":"` + shortIDForSyncTest(targetUID) + `",
		"title":"target",
		"body":"",
		"author":"hub",
		"status":"open",
		"metadata":{},
		"created_at":"2026-05-23T12:00:01.000Z"
	}`
	page1 := []api.EventEnvelope{
		syncTestEnvelope(t, 1, hubProjectUID, "hub", nil, nil, "project.federation_enabled",
			`{"project_uid":"`+hubProjectUID+`","project_name":"hub","metadata":{}}`),
		syncTestEnvelope(t, 2, hubProjectUID, "hub", &sourceUID, nil, "issue.snapshot", sourcePayload),
	}
	page2 := []api.EventEnvelope{
		syncTestEnvelope(t, 3, hubProjectUID, "hub", &targetUID, nil, "issue.snapshot", targetPayload),
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/42/federation/events", r.URL.Path)
		switch r.URL.Query().Get("after_id") {
		case "0":
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{Events: page1, NextAfterID: 2}))
		case "2":
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{Events: page2, NextAfterID: 3}))
		default:
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{Events: []api.EventEnvelope{}, NextAfterID: 3}))
		}
	}))
	t.Cleanup(hub.Close)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         42,
		HubProjectUID:        hubProjectUID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	require.NoError(t, SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	}))
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), binding.PullCursorEventID)

	require.NoError(t, SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	}))
	var linkCount int
	require.NoError(t, spoke.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM links
		 WHERE from_issue_id IN (SELECT id FROM issues WHERE project_id = ?)
		   AND type = 'related'`,
		project.ID).Scan(&linkCount))
	assert.Equal(t, 1, linkCount)
}

func TestSyncFederationOnceHandlesResetRequired(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		Enabled:              true,
	})
	require.NoError(t, err)

	polls := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events":
			polls++
			if polls == 1 {
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
					ResetRequired: true,
					ResetAfterID:  90,
					Events:        []api.EventEnvelope{},
					NextAfterID:   90,
				}))
				return
			}
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{},
				NextAfterID: 90,
			}))
		case "/api/v1/projects/42/federation/metadata":
			require.NoError(t, json.NewEncoder(w).Encode(api.ProjectFederationBody{
				ProjectID:              42,
				ProjectUID:             project.UID,
				ProjectName:            project.Name,
				ReplayHorizonEventID:   91,
				BaselineThroughEventID: 91,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, polls, "reset should refresh metadata and re-poll from the new horizon")
	status := requireFederationSyncStatus(t, spoke.DB, project.ID)
	assertStatusTimeSet(t, status.LastPullStartedAt)
	assertStatusTimeSet(t, status.LastPullSuccessAt)
	assertStatusTimeSet(t, status.LastResetAt)
	assert.Nil(t, status.LastErrorAt)
	assert.Nil(t, status.LastError)
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(90), binding.PullCursorEventID)
	assert.Equal(t, int64(91), binding.ReplayHorizonEventID)
}

func TestSyncFederationOnceErrorsWhenResetStillRequiredAfterRefresh(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		Enabled:              true,
	})
	require.NoError(t, err)

	polls := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events":
			polls++
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				ResetRequired: true,
				ResetAfterID:  90,
				Events:        []api.EventEnvelope{},
				NextAfterID:   90,
			}))
		case "/api/v1/projects/42/federation/metadata":
			require.NoError(t, json.NewEncoder(w).Encode(api.ProjectFederationBody{
				ProjectID:              42,
				ProjectUID:             project.UID,
				ProjectName:            project.Name,
				ReplayHorizonEventID:   91,
				BaselineThroughEventID: 91,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
	})

	require.ErrorIs(t, err, ErrFederationResetRequired)
	assert.Equal(t, 2, polls)
	status := requireFederationSyncStatus(t, spoke.DB, project.ID)
	assertStatusTimeSet(t, status.LastPullStartedAt)
	assert.Nil(t, status.LastPullSuccessAt)
	assert.Nil(t, status.LastResetAt)
	assertStatusTimeSet(t, status.LastErrorAt)
	require.NotNil(t, status.LastError)
	assert.Contains(t, *status.LastError, ErrFederationResetRequired.Error())
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(90), binding.PullCursorEventID, "cursor stays at replay-horizon-1 instead of advancing to a still-reset poll response")
}

func TestSyncFederationOnceRecordsPullErrorStatus(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "hub unavailable", http.StatusBadGateway)
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.Error(t, err)
	status := requireFederationSyncStatus(t, spoke.DB, project.ID)
	assertStatusTimeSet(t, status.LastPullStartedAt)
	assert.Nil(t, status.LastPullSuccessAt)
	assertStatusTimeSet(t, status.LastErrorAt)
	require.NotNil(t, status.LastError)
	assert.Contains(t, *status.LastError, "returned 502")
}

func TestSyncFederationOnceRecordsClientConstructionErrorStatus(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://example.com",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		Token: "token",
	})

	require.Error(t, err)
	status := requireFederationSyncStatus(t, spoke.DB, project.ID)
	assertStatusTimeSet(t, status.LastPullStartedAt)
	assert.Nil(t, status.LastPullSuccessAt)
	assertStatusTimeSet(t, status.LastErrorAt)
	require.NotNil(t, status.LastError)
	assert.Contains(t, *status.LastError, "http://example.com")
}

func TestSyncFederationOncePushPoisonLeavesCursorUnchanged(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	polled := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			http.Error(w, "poison batch", http.StatusConflict)
		case "/api/v1/projects/42/federation/events":
			polled = true
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.Error(t, err)
	require.False(t, polled, "poison push must stop before pull")
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), binding.PushCursorEventID)
	pending, err := spoke.DB.PendingFederationPushEvents(ctx, project.ID, spoke.DB.InstanceUID(), 0, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, localEvent.ID, pending[0].ID)
}

func TestSyncFederationOncePushPoisonRecordsQuarantine(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	issue, firstEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, secondEvent, err := spoke.DB.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID,
		Author:  "tester",
		Body:    "second local event",
	})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/42/federation/events:ingest" {
			http.Error(w, "poison batch", http.StatusConflict)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.Error(t, err)
	q, err := spoke.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	assert.Equal(t, firstEvent.ID, q.FirstEventID)
	assert.Equal(t, secondEvent.ID, q.LastEventID)
	assert.Equal(t, []string{firstEvent.UID, secondEvent.UID}, q.EventUIDs)
	assert.Contains(t, q.Error, "returned 409")
}

func TestSyncFederationOnceValidationBadRequestRecordsQuarantine(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/42/federation/events:ingest" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			require.NoError(t, json.NewEncoder(w).Encode(api.ErrorEnvelope{
				Status: http.StatusBadRequest,
				Error: api.ErrorBody{
					Code:    "validation",
					Message: "invalid federation batch",
				},
			}))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.Error(t, err)
	q, err := spoke.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	assert.Equal(t, localEvent.ID, q.FirstEventID)
	assert.Equal(t, localEvent.ID, q.LastEventID)
	assert.Contains(t, q.Error, "returned 400")
}

func TestSyncFederationOnceUnsupportedSchemaDoesNotQuarantine(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	requests := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/42/federation/events:ingest" {
			requests++
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, db.CurrentSchemaVersion(), body.SchemaVersion)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			require.NoError(t, json.NewEncoder(w).Encode(api.ErrorEnvelope{
				Status: http.StatusBadRequest,
				Error: api.ErrorBody{
					Code: "unsupported_federation_schema",
					Message: fmt.Sprintf("federation ingest schema_version %d is newer than hub schema_version %d",
						body.SchemaVersion, db.CurrentSchemaVersion()-1),
				},
			}))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.Error(t, err)
	assert.Equal(t, 1, requests)
	_, err = spoke.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	assert.ErrorIs(t, err, db.ErrNotFound)
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), binding.PushCursorEventID)
	pending, err := spoke.DB.PendingFederationPushEvents(ctx, project.ID, spoke.DB.InstanceUID(), 0, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, localEvent.ID, pending[0].ID)
}

func TestSyncFederationOnceActiveQuarantineStopsPushBeforeNetwork(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, err = spoke.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 1,
		LastEventID:  2,
		EventUIDs:    []string{"event-1"},
		Error:        "poison",
		CreatedAt:    time.Now().UTC(),
	})
	require.NoError(t, err)
	requests := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.NotFound(w, r)
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.ErrorIs(t, err, ErrFederationPushQuarantined)
	assert.Equal(t, 0, requests)
}

func TestSyncFederationOnceAutoRetriesLegacySchemaSkewQuarantine(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = spoke.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: localEvent.ID,
		LastEventID:  localEvent.ID,
		EventUIDs:    []string{localEvent.UID},
		Error: `hub /api/v1/projects/42/federation/events:ingest returned 400: ` +
			`{"status":400,"error":{"code":"unsupported_federation_schema","message":"federation ingest schema_version 17 is newer than hub schema_version 14"}}`,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	requests := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			requests++
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Len(t, body.Events, 1)
			assert.Equal(t, localEvent.ID, body.Events[0].EventID)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          1,
				PushCursorEventID: localEvent.ID,
			}))
		case "/api/v1/projects/42/federation/events":
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{NextAfterID: 49}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.NoError(t, err)
	assert.Equal(t, 1, requests)
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, localEvent.ID, binding.PushCursorEventID)
	_, err = spoke.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	assert.ErrorIs(t, err, db.ErrNotFound)
	var skipReason string
	require.NoError(t, spoke.DB.QueryRow(`
		SELECT skip_reason
		  FROM federation_quarantine
		 WHERE project_id = ?`,
		project.ID).Scan(&skipReason))
	assert.Equal(t, "retry: auto-retry after transient schema skew", skipReason)
}

func TestSyncFederationOnceAutoRetriesFormerPeerReferenceQuarantine(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	localIssue, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	peerProject, err := spoke.DB.CreateProject(ctx, "peer-project")
	require.NoError(t, err)
	peerIssue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: peerProject.ID,
		Title:     "pending peer",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, linkEvent, err := spoke.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: localIssue.ID,
		ToIssueID:   peerIssue.ID,
		Type:        "blocks",
		Author:      "tester",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: localIssue.ID,
		FromShortID:  localIssue.ShortID,
		FromUID:      localIssue.UID,
		ToShortID:    peerIssue.ShortID,
		ToUID:        peerIssue.UID,
		Actor:        "tester",
	})
	require.NoError(t, err)
	_, err = spoke.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: localEvent.ID,
		LastEventID:  linkEvent.ID,
		EventUIDs:    []string{localEvent.UID, linkEvent.UID},
		Error: `hub /api/v1/projects/42/federation/events:ingest returned 400: ` +
			`{"status":400,"error":{"code":"validation","message":` +
			`"federation ingest validation: event ` + linkEvent.UID +
			` references unknown issue ` + peerIssue.UID + `"}}`,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	ingestRequests := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			ingestRequests++
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Len(t, body.Events, 2)
			assert.Equal(t, localEvent.ID, body.Events[0].EventID)
			assert.Equal(t, linkEvent.ID, body.Events[1].EventID)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          2,
				PushCursorEventID: linkEvent.ID,
			}))
		case "/api/v1/projects/42/federation/events":
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{NextAfterID: 49}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.NoError(t, err)
	assert.Equal(t, 1, ingestRequests)
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, linkEvent.ID, binding.PushCursorEventID)
	_, err = spoke.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	assert.ErrorIs(t, err, db.ErrNotFound)
	var skipReason string
	require.NoError(t, spoke.DB.QueryRow(`
		SELECT skip_reason
		  FROM federation_quarantine
		 WHERE project_id = ?`,
		project.ID).Scan(&skipReason))
	assert.Equal(t, "retry: auto-retry after deferred link peer fix", skipReason)
}

func TestAutoRetryFederationQuarantineRequiresExactFormerPeerMessage(t *testing.T) {
	legacyMessage := "federation ingest validation: event " +
		"01HZNQ7VFPK1XGD8R5MABCD4EA references unknown issue " +
		"01HZNQ7VFPK1XGD8R5MABCD4EB"
	for _, message := range []string{
		"additional validation: " + legacyMessage,
		legacyMessage + "; primary issue also missing",
	} {
		body, err := json.Marshal(api.ErrorEnvelope{
			Status: http.StatusBadRequest,
			Error: api.ErrorBody{
				Code:    "validation",
				Message: message,
			},
		})
		require.NoError(t, err)

		retry, reason := autoRetryFederationQuarantine(db.FederationQuarantine{
			Direction: db.FederationQuarantineDirectionPush,
			Error: "hub /api/v1/projects/42/federation/events:ingest returned 400: " +
				string(body),
		}, nil)

		assert.False(t, retry)
		assert.Empty(t, reason)
	}
}

func TestSyncFederationOnceNonLinkSecondaryReferenceQuarantineStillStopsBeforeNetwork(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	unknownPeerUID := "01HZNQ7VFPK1XGD8R5MABCD4EB"
	_, err = spoke.DB.ExecContext(ctx,
		`UPDATE events SET related_issue_uid = ? WHERE id = ?`, unknownPeerUID, localEvent.ID)
	require.NoError(t, err)
	_, err = spoke.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: localEvent.ID,
		LastEventID:  localEvent.ID,
		EventUIDs:    []string{localEvent.UID},
		Error: `hub /api/v1/projects/42/federation/events:ingest returned 400: ` +
			`{"status":400,"error":{"code":"validation","message":"federation ingest validation: event ` +
			localEvent.UID + ` references unknown issue ` + unknownPeerUID + `"}}`,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	requests := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.ErrorIs(t, err, ErrFederationPushQuarantined)
	assert.Equal(t, 0, requests)
	_, err = spoke.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
}

func TestSyncFederationOnceUnknownPrimaryQuarantineStillStopsBeforeNetwork(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = spoke.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: localEvent.ID,
		LastEventID:  localEvent.ID,
		EventUIDs:    []string{localEvent.UID},
		Error: `hub /api/v1/projects/42/federation/events:ingest returned 400: ` +
			`{"status":400,"error":{"code":"validation","message":` +
			`"federation ingest validation: issue.updated references unknown issue 01HZNQ7VFPK1XGD8R5MABCD4EB"}}`,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	requests := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.ErrorIs(t, err, ErrFederationPushQuarantined)
	assert.Equal(t, 0, requests)
	_, err = spoke.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
}

func TestSyncFederationOnceAfterQuarantineRetryPushesAgain(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local",
		Author:    "tester",
	})
	require.NoError(t, err)
	recorded, err := spoke.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: localEvent.ID,
		LastEventID:  localEvent.ID,
		EventUIDs:    []string{localEvent.UID},
		Error:        "hub rejected batch",
		CreatedAt:    time.Now().UTC(),
	})
	require.NoError(t, err)
	_, err = spoke.DB.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{
		ID:        recorded.ID,
		ProjectID: project.ID,
		Actor:     "operator",
		Reason:    "hub upgraded",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	requests := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			requests++
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Len(t, body.Events, 1)
			assert.Equal(t, localEvent.ID, body.Events[0].EventID)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          1,
				PushCursorEventID: localEvent.ID,
			}))
		case "/api/v1/projects/42/federation/events":
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{NextAfterID: 49}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.NoError(t, err)
	assert.Equal(t, 1, requests)
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, localEvent.ID, binding.PushCursorEventID)
}

func TestSyncFederationOnceResetBlockedByLocalEventCreatedDuringMetadataRefresh(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 50,
		PullCursorEventID:    49,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events":
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				ResetRequired: true,
				ResetAfterID:  90,
				Events:        []api.EventEnvelope{},
				NextAfterID:   90,
			}))
		case "/api/v1/projects/42/federation/metadata":
			_, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID,
				Title:     "created during reset",
				Author:    "tester",
			})
			require.NoError(t, err)
			require.NoError(t, json.NewEncoder(w).Encode(api.ProjectFederationBody{
				ProjectID:              42,
				ProjectUID:             project.UID,
				ProjectName:            project.Name,
				ReplayHorizonEventID:   91,
				BaselineThroughEventID: 91,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.ErrorIs(t, err, ErrFederationResetBlockedByPendingPush)
	status := requireFederationSyncStatus(t, spoke.DB, project.ID)
	assertStatusTimeSet(t, status.LastPullStartedAt)
	assert.Nil(t, status.LastPullSuccessAt)
	assert.Nil(t, status.LastResetAt)
	assertStatusTimeSet(t, status.LastErrorAt)
	require.NotNil(t, status.LastError)
	assert.Contains(t, *status.LastError, ErrFederationResetBlockedByPendingPush.Error())
	_, err = spoke.DB.IssueByUID(ctx, mustIssueUIDByTitle(t, spoke.DB, "created during reset"), db.IncludeDeletedNo)
	require.NoError(t, err)
}

func TestFederationMultiProjectEnrollmentSyncMatrix(t *testing.T) {
	type scenario struct {
		firstTaskBeforeEnrollment bool
		peerTaskBeforeEnrollment  bool
		eagerSync                 bool
		peerSyncsFirst            bool
	}
	var scenarios []scenario
	for _, firstBefore := range []bool{false, true} {
		for _, peerBefore := range []bool{false, true} {
			for _, eager := range []bool{false, true} {
				for _, peerFirst := range []bool{false, true} {
					scenarios = append(scenarios, scenario{firstBefore, peerBefore, eager, peerFirst})
				}
			}
		}
	}
	require.Len(t, scenarios, 16)

	for _, tc := range scenarios {
		name := fmt.Sprintf("first_before=%t/peer_before=%t/eager=%t/peer_first=%t",
			tc.firstTaskBeforeEnrollment, tc.peerTaskBeforeEnrollment, tc.eagerSync, tc.peerSyncsFirst)
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			hub := testenv.New(t)
			spoke := testenv.New(t)
			firstHub := matrixCreateHubProject(t, hub, "spoke-project")
			peerHub := matrixCreateHubProject(t, hub, "peer-project")
			firstLocal, err := spoke.DB.CreateProject(ctx, firstHub.Name)
			require.NoError(t, err)
			peerLocal, err := spoke.DB.CreateProject(ctx, peerHub.Name)
			require.NoError(t, err)
			firstEnrollment := matrixCreateEnrollment(t, hub, spoke, firstHub, "first-matrix-token")
			peerEnrollment := matrixCreateEnrollment(t, hub, spoke, peerHub, "peer-matrix-token")
			first := &matrixFederationProject{tag: "first", hub: firstHub, local: firstLocal,
				credential: matrixCredential(hub.URL, firstHub.ID, firstEnrollment.Token),
				before:     tc.firstTaskBeforeEnrollment}
			peer := &matrixFederationProject{tag: "peer", hub: peerHub, local: peerLocal,
				credential: matrixCredential(hub.URL, peerHub.ID, peerEnrollment.Token),
				before:     tc.peerTaskBeforeEnrollment}
			projects := []*matrixFederationProject{first, peer}
			syncOrder := []*matrixFederationProject{first, peer}
			if tc.peerSyncsFirst {
				syncOrder[0], syncOrder[1] = syncOrder[1], syncOrder[0]
			}

			for _, project := range projects {
				if project.before {
					project.issue = matrixCreateInitialState(t, spoke.DB, project.local, project.tag)
				}
			}
			linked := false
			if first.issue.ID != 0 && peer.issue.ID != 0 {
				matrixCreateCrossProjectLink(t, spoke.DB, first.issue, peer.issue)
				linked = true
			}
			syncEligible := func() {
				for _, project := range syncOrder {
					if project.enrolled {
						matrixSyncProject(t, spoke.DB, project)
					}
				}
			}
			for _, project := range syncOrder {
				matrixAdoptProject(t, hub.DB, spoke.DB, hub.URL, project)
				if !project.before {
					project.issue = matrixCreateInitialState(t, spoke.DB, project.local, project.tag)
				}
				if !linked && first.issue.ID != 0 && peer.issue.ID != 0 {
					matrixCreateCrossProjectLink(t, spoke.DB, first.issue, peer.issue)
					linked = true
				}
				if tc.eagerSync {
					syncEligible()
					if tc.peerSyncsFirst && project == peer && !first.enrolled && linked {
						matrixAssertLinkCount(t, spoke.DB, first.issue.UID, peer.issue.UID, 1)
					}
				}
			}
			require.True(t, linked)
			for _, project := range projects {
				project.issue = matrixApplyPostEnrollmentState(t, spoke.DB, project.issue, project.tag)
				if tc.eagerSync {
					syncEligible()
				}
			}
			syncEligible()
			syncEligible()

			for _, project := range projects {
				matrixAssertPortableState(t, hub.DB, project.hub.ID, project.issue.UID, project.tag)
				matrixAssertPushDrained(t, spoke.DB, project.local.ID)
			}
			matrixAssertLinkCount(t, hub.DB, first.issue.UID, peer.issue.UID, 1)
			matrixApplyHubFollowup(t, hub.DB, first, peer)
			syncEligible()
			matrixAssertPulledFollowup(t, spoke.DB, first, peer)
			matrixAssertPullCursors(t, hub.DB, spoke.DB, projects)

			beforeRepeat, err := spoke.DB.MaxEventID(ctx)
			require.NoError(t, err)
			syncEligible()
			afterRepeat, err := spoke.DB.MaxEventID(ctx)
			require.NoError(t, err)
			assert.Equal(t, beforeRepeat, afterRepeat)
			matrixAssertLinkCount(t, hub.DB, first.issue.UID, peer.issue.UID, 1)
		})
	}
}

func TestSyncFederationOncePushesAndAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "push-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
		Actor:            "tester",
	})
	require.NoError(t, err)
	spokeProject, err := spoke.DB.CreateProjectWithUID(ctx, "hub", hubProject.UID)
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	localIssue, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spokeProject.ID,
		Title:     "from spoke",
		Author:    "tester",
	})
	require.NoError(t, err)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	})
	require.NoError(t, err)

	pushed, err := hub.DB.IssueByUID(ctx, localIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "from spoke", pushed.Title)
	binding, err = spoke.DB.FederationBindingByProject(ctx, spokeProject.ID)
	require.NoError(t, err)
	assert.Equal(t, localEvent.ID, binding.PushCursorEventID)
	status := requireFederationSyncStatus(t, spoke.DB, spokeProject.ID)
	assertStatusTimeSet(t, status.LastPushStartedAt)
	assertStatusTimeSet(t, status.LastPushSuccessAt)
	assertStatusTimeSet(t, status.LastPullStartedAt)
	assertStatusTimeSet(t, status.LastPullSuccessAt)
	assert.Nil(t, status.LastErrorAt)
	assert.Nil(t, status.LastError)
}

func TestSyncFederationOncePushEchoDoesNotDeliverPulledLocalEvent(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "push-echo-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
		Actor:            "tester",
	})
	require.NoError(t, err)
	hubMaxEventID, err := hub.DB.MaxEventID(ctx)
	require.NoError(t, err)
	spokeProject, err := spoke.DB.CreateProjectWithUID(ctx, "hub", hubProject.UID)
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		PullCursorEventID:    hubMaxEventID,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spokeProject.ID,
		Title:     "from spoke",
		Author:    "tester",
	})
	require.NoError(t, err)
	var delivered []db.Event

	err = SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	})

	require.NoError(t, err)
	for _, event := range delivered {
		assert.NotEqual(t, localEvent.UID, event.UID, "push echo should not redeliver local-origin event")
	}
	assert.Empty(t, delivered, "push echo was the only unadvanced pull event")
}

func TestSyncFederationOnceRejectsLocalPushEchoHashMismatch(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "from spoke",
		Author:    "tester",
	})
	require.NoError(t, err)
	envelope := eventEnvelopeForSyncTest(localEvent, 100)
	envelope.Payload = json.RawMessage(strings.Replace(string(envelope.Payload), `"title":"from spoke"`, `"title":"from hub"`, 1))
	rehashEventEnvelope(t, &envelope)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 100,
		PullCursorEventID:    99,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events":
			require.Equal(t, "99", r.URL.Query().Get("after_id"))
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{envelope},
				NextAfterID: 100,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrRemoteEventConflict)
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(99), binding.PullCursorEventID)
}

func TestSyncFederationOnceCanonicalizesLocalAdoptionPushEcho(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "adopted issue",
		Author:    "historical-author",
	})
	require.NoError(t, err)
	adopted, err := spoke.DB.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID:            project.ID,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 100,
		Actor:                "agent",
	})
	require.NoError(t, err)
	events, err := spoke.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	var snapshot db.Event
	for _, ev := range events {
		if ev.Type == "issue.snapshot" && ev.IssueUID != nil && *ev.IssueUID == issue.UID {
			snapshot = ev
			break
		}
	}
	require.NotEmpty(t, snapshot.UID)
	assert.Contains(t, snapshot.Payload, `"author":"historical-author"`)
	envelope := eventEnvelopeForSyncTest(snapshot, 100)
	envelope.Payload = json.RawMessage(strings.Replace(string(envelope.Payload), `"author":"historical-author"`, `"author":"agent"`, 1))
	rehashEventEnvelope(t, &envelope)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events":
			require.Equal(t, "99", r.URL.Query().Get("after_id"))
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{envelope},
				NextAfterID: 100,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	binding := adopted.Binding
	binding.PushEnabled = false

	require.NoError(t, SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	}))

	events, err = spoke.DB.EventsByUIDs(ctx, project.ID, []string{snapshot.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, envelope.ContentHash, events[0].ContentHash)
	assert.Contains(t, events[0].Payload, `"author":"agent"`)
	materialized, err := spoke.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "agent", materialized.Author)
}

func TestSyncFederationOnceResetRetryDeliversReplayedLocalOriginEvent(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	issue, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "acked before reset",
		Author:    "tester",
	})
	require.NoError(t, err)
	envelope := eventEnvelopeForSyncTest(localEvent, 100)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 51,
		PullCursorEventID:    50,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    localEvent.ID,
		Enabled:              true,
	})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.NotEmpty(t, body.Events)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          len(body.Events),
				Duplicates:        len(body.Events),
				PushCursorEventID: body.Events[len(body.Events)-1].EventID,
			}))
		case "/api/v1/projects/42/federation/events":
			switch r.URL.Query().Get("after_id") {
			case "50":
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
					ResetRequired: true,
					ResetAfterID:  99,
					NextAfterID:   99,
				}))
			case "99":
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
					Events:      []api.EventEnvelope{envelope},
					NextAfterID: 100,
				}))
			default:
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{NextAfterID: 100}))
			}
		case "/api/v1/projects/42/federation/metadata":
			require.NoError(t, json.NewEncoder(w).Encode(api.ProjectFederationBody{
				ProjectID:              42,
				ProjectUID:             project.UID,
				ProjectName:            project.Name,
				ReplayHorizonEventID:   100,
				BaselineThroughEventID: 100,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "token"}
	var delivered []db.Event
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
	require.Error(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, creds, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))
	assert.Empty(t, delivered)
	_, err = spoke.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)

	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, creds, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))

	mirrored, err := spoke.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "acked before reset", mirrored.Title)
	require.NotEmpty(t, delivered)
	assert.Equal(t, localEvent.UID, delivered[0].UID)
	assert.Greater(t, delivered[0].ID, localEvent.ID)
}

func TestSyncFederationOnceResetRetryDeliversReplayedLocalProjectEvent(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	metaOut, err := spoke.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  project.ID,
		IfMatchRev: db.IfMatch(project.Revision),
		Actor:      "tester",
		Patch: map[string]json.RawMessage{
			"area": json.RawMessage(`"ops"`),
		},
	})
	require.NoError(t, err)
	localEvent := metaOut.Event
	require.Nil(t, localEvent.IssueID)
	require.Nil(t, localEvent.IssueUID)
	envelope := eventEnvelopeForSyncTest(localEvent, 100)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 51,
		PullCursorEventID:    50,
		PushEnabled:          true,
		Actor:                "tester",
		PushCursorEventID:    localEvent.ID,
		Enabled:              true,
	})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.NotEmpty(t, body.Events)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          len(body.Events),
				Duplicates:        len(body.Events),
				PushCursorEventID: body.Events[len(body.Events)-1].EventID,
			}))
		case "/api/v1/projects/42/federation/events":
			switch r.URL.Query().Get("after_id") {
			case "50":
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
					ResetRequired: true,
					ResetAfterID:  99,
					NextAfterID:   99,
				}))
			case "99":
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
					Events:      []api.EventEnvelope{envelope},
					NextAfterID: 100,
				}))
			default:
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{NextAfterID: 100}))
			}
		case "/api/v1/projects/42/federation/metadata":
			require.NoError(t, json.NewEncoder(w).Encode(api.ProjectFederationBody{
				ProjectID:              42,
				ProjectUID:             project.UID,
				ProjectName:            project.Name,
				ReplayHorizonEventID:   100,
				BaselineThroughEventID: 100,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: 42, Token: "token"}
	var delivered []db.Event
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "during_spoke_pull_apply_before_materialize=unexpected")
	require.Error(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, creds, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))
	assert.Empty(t, delivered)

	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, creds, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))

	require.NotEmpty(t, delivered)
	assert.Equal(t, localEvent.UID, delivered[0].UID)
	assert.Greater(t, delivered[0].ID, localEvent.ID)
}

func TestSyncFederationOnceRecoveredResetDoesNotDeliverLocalProjectPushEcho(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 100,
		PullCursorEventID:    99,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	resetAt := time.Date(2026, 5, 24, 1, 0, 0, 0, time.UTC)
	require.NoError(t, spoke.DB.RecordFederationSyncReset(ctx, project.ID, resetAt))
	require.NoError(t, spoke.DB.RecordFederationSyncPullSuccess(ctx, project.ID, resetAt.Add(time.Second)))

	metaOut, err := spoke.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  project.ID,
		IfMatchRev: db.IfMatch(project.Revision),
		Actor:      "tester",
		Patch: map[string]json.RawMessage{
			"area": json.RawMessage(`"ops"`),
		},
	})
	require.NoError(t, err)
	localEvent := metaOut.Event
	envelope := eventEnvelopeForSyncTest(localEvent, 100)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.NotEmpty(t, body.Events)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          len(body.Events),
				PushCursorEventID: body.Events[len(body.Events)-1].EventID,
			}))
		case "/api/v1/projects/42/federation/events":
			require.Equal(t, "99", r.URL.Query().Get("after_id"))
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{envelope},
				NextAfterID: 100,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	var delivered []db.Event
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	}, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))

	assert.Empty(t, delivered, "local project metadata push echo should not redeliver pulled side effects after reset recovery")
}

func TestSyncFederationOncePendingResetDoesNotDeliverPostResetLocalProjectPushEcho(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 100,
		PullCursorEventID:    99,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	resetAt := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, spoke.DB.RecordFederationSyncReset(ctx, project.ID, resetAt))

	metaOut, err := spoke.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  project.ID,
		IfMatchRev: db.IfMatch(project.Revision),
		Actor:      "tester",
		Patch: map[string]json.RawMessage{
			"area": json.RawMessage(`"ops"`),
		},
	})
	require.NoError(t, err)
	localEvent := metaOut.Event
	require.True(t, localEvent.CreatedAt.After(resetAt), "test event must be created after reset marker")
	envelope := eventEnvelopeForSyncTest(localEvent, 100)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.NotEmpty(t, body.Events)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          len(body.Events),
				PushCursorEventID: body.Events[len(body.Events)-1].EventID,
			}))
		case "/api/v1/projects/42/federation/events":
			require.Equal(t, "99", r.URL.Query().Get("after_id"))
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{envelope},
				NextAfterID: 100,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	var delivered []db.Event
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	}, clientpkg.Opts{}, func(_ int64, events []db.Event) {
		delivered = append(delivered, events...)
	}))

	assert.Empty(t, delivered, "post-reset local project metadata push echo should not be treated as reset replay")
}

func TestSyncFederationOncePushRetryDuplicateAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "retry-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
		Actor:            "tester",
	})
	require.NoError(t, err)
	spokeProject, err := spoke.DB.CreateProjectWithUID(ctx, "hub", hubProject.UID)
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spokeProject.ID,
		Title:     "from spoke",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.NoError(t, SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}))
	require.NoError(t, spoke.DB.AdvanceFederationPushCursor(ctx, spokeProject.ID, 0))
	binding, err = spoke.DB.FederationBindingByProject(ctx, spokeProject.ID)
	require.NoError(t, err)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	})
	require.NoError(t, err)
	binding, err = spoke.DB.FederationBindingByProject(ctx, spokeProject.ID)
	require.NoError(t, err)
	assert.Equal(t, localEvent.ID, binding.PushCursorEventID)
}

func TestSyncFederationOnceRejectsPushAckBeyondSubmittedBatch(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	_, localEvent, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending",
		Author:    "tester",
	})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          1,
				Duplicates:        0,
				PushCursorEventID: localEvent.ID + 1,
			}))
		case "/api/v1/projects/42/federation/events":
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{},
				NextAfterID: 1,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "beyond submitted batch")
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Zero(t, binding.PushCursorEventID)
}

func TestFederationPushOfflineReconnect(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "offline-reconnect-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
		Actor:            "tester",
	})
	require.NoError(t, err)
	project, err := spoke.DB.CreateProjectWithUID(ctx, "hub", hubProject.UID)
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "offline first",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, secondEvent, err := spoke.DB.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID,
		Author:  "tester",
		Body:    "offline second",
	})
	require.NoError(t, err)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       "http://127.0.0.1:1",
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	})
	require.Error(t, err)
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), binding.PushCursorEventID)

	creds := config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}
	err = SyncFederationOnce(ctx, spoke.DB, binding, creds)
	require.NoError(t, err)
	require.NoError(t, assertHubOriginEventCount(ctx, hub.DB, hubProject.ID, spoke.DB.InstanceUID(), 2))
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, secondEvent.ID, binding.PushCursorEventID)

	require.NoError(t, spoke.DB.AdvanceFederationPushCursor(ctx, project.ID, 0))
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	err = SyncFederationOnce(ctx, spoke.DB, binding, creds)
	require.NoError(t, err)
	require.NoError(t, assertHubOriginEventCount(ctx, hub.DB, hubProject.ID, spoke.DB.InstanceUID(), 2))
	binding, err = spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, secondEvent.ID, binding.PushCursorEventID)

	pushed, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "offline first", pushed.Title)
	rows, err := hub.DB.QueryContext(ctx, `SELECT body FROM comments WHERE issue_id = ? ORDER BY id`, pushed.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var comments []string
	for rows.Next() {
		var body string
		require.NoError(t, rows.Scan(&body))
		comments = append(comments, body)
	}
	require.NoError(t, rows.Err())
	require.Len(t, comments, 1)
	assert.Equal(t, "offline second", comments[0])
}

func TestSyncFederationOncePushesAdoptedIssueSnapshotsAndLinks(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)

	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:                        "adopt-token",
		SpokeInstanceUID:             spoke.DB.InstanceUID(),
		ProjectID:                    &hubProject.ID,
		Capabilities:                 "pull,push",
		Actor:                        "tester",
		AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)

	localProject, err := spoke.DB.CreateProject(ctx, "shared-foo")
	require.NoError(t, err)
	source, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: localProject.ID,
		Title:     "adopted source",
		Author:    "legacy-source",
	})
	require.NoError(t, err)
	target, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: localProject.ID,
		Title:     "adopted target",
		Author:    "legacy-target",
	})
	require.NoError(t, err)
	deleted, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: localProject.ID,
		Title:     "adopted deleted",
		Author:    "legacy-deleted",
	})
	require.NoError(t, err)
	deleted, _, _, err = spoke.DB.SoftDeleteIssue(ctx, deleted.ID, "tester")
	require.NoError(t, err)
	comment, _, err := spoke.DB.CreateComment(ctx, db.CreateCommentParams{
		IssueID: source.ID,
		Author:  "legacy-commenter",
		Body:    "adopted comment",
	})
	require.NoError(t, err)
	_, err = spoke.DB.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: source.ID,
		ToIssueID:   target.ID,
		Type:        "related",
		Author:      "tester",
	})
	require.NoError(t, err)

	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         hubProject.UID,
		"project_name":            localProject.Name,
		"replay_horizon_event_id": hubBinding.ReplayHorizonEventID,
		"token":                   created.Token,
		"capabilities":            "pull,push",
		"actor":                   "tester",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, &replica)
	require.True(t, replica.Adopted)
	require.Equal(t, int64(3), replica.AdoptionSnapshotCount)

	binding, err := spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
		Capabilities: "pull,push",
	})
	require.NoError(t, err)

	pushedSource, err := hub.DB.IssueByUID(ctx, source.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "adopted source", pushedSource.Title)
	assert.Equal(t, "legacy-source", pushedSource.Author)
	pushedTarget, err := hub.DB.IssueByUID(ctx, target.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "adopted target", pushedTarget.Title)
	assert.Equal(t, "legacy-target", pushedTarget.Author)
	pushedDeleted, err := hub.DB.IssueByUID(ctx, deleted.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "adopted deleted", pushedDeleted.Title)
	assert.Equal(t, "legacy-deleted", pushedDeleted.Author)
	require.NotNil(t, pushedDeleted.DeletedAt)
	pushedComments, err := hub.DB.CommentsByIssue(ctx, pushedSource.ID)
	require.NoError(t, err)
	require.Len(t, pushedComments, 1)
	assert.Equal(t, comment.UID, pushedComments[0].UID)
	assert.Equal(t, "legacy-commenter", pushedComments[0].Author)
	assert.Equal(t, "adopted comment", pushedComments[0].Body)
	var linkCount int
	require.NoError(t, hub.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM links
		 WHERE from_issue_id IN (SELECT id FROM issues WHERE project_id = ?)
		   AND from_issue_uid = ? AND to_issue_uid = ? AND type = 'related'`,
		hubProject.ID, source.UID, target.UID).Scan(&linkCount))
	assert.Equal(t, 1, linkCount)
	var quarantineCount int
	require.NoError(t, hub.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM federation_quarantine
		 WHERE project_id = ? AND direction = 'push' AND skipped_at IS NULL`,
		hubProject.ID).Scan(&quarantineCount))
	assert.Zero(t, quarantineCount)
	require.NoError(t, assertHubOriginEventCount(ctx, hub.DB, hubProject.ID, spoke.DB.InstanceUID(), 3))
}

func TestSyncFederationOncePushesSplitAdoptionSnapshotsWithHistoricalAuthors(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)

	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:                        "split-adopt-token",
		SpokeInstanceUID:             spoke.DB.InstanceUID(),
		ProjectID:                    &hubProject.ID,
		Capabilities:                 "pull,push",
		Actor:                        "agent",
		AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)
	var requestSizes []int
	var baselineStages []string
	var baselineEndEventIDs []int64
	var baselineLastEventIDs []int64
	snapshotAuthorsByUID := map[string]string{}
	var sawContinuationBoundActor bool
	var snapshotRequestAccepted bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		requestHadSnapshot := false
		if r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/federation/events:ingest", hubProject.ID) {
			requestSizes = append(requestSizes, len(raw))
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.Unmarshal(raw, &body))
			require.NotEmpty(t, body.Events)
			isSnapshotContinuation := snapshotRequestAccepted
			baselineStages = append(baselineStages, body.AdoptionBaseline)
			baselineEndEventIDs = append(baselineEndEventIDs, body.AdoptionBaselineEndEventID)
			baselineLastEventIDs = append(baselineLastEventIDs, body.Events[len(body.Events)-1].EventID)
			for _, ev := range body.Events {
				if ev.Type != "issue.snapshot" {
					continue
				}
				var payload struct {
					UID    string `json:"uid"`
					Author string `json:"author"`
				}
				require.NoError(t, json.Unmarshal(ev.Payload, &payload))
				assert.Equal(t, "historical-author", payload.Author)
				projectedAuthor := "historical-author"
				if isSnapshotContinuation {
					projectedAuthor = "agent"
					sawContinuationBoundActor = true
				}
				snapshotAuthorsByUID[payload.UID] = projectedAuthor
				requestHadSnapshot = true
			}
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, hub.URL+r.URL.RequestURI(), bytes.NewReader(raw)) //nolint:gosec // test proxy forwards only to the local httptest hub.
		require.NoError(t, err)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // test proxy forwards only to the local httptest hub.
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, err = io.Copy(w, resp.Body)
		require.NoError(t, err)
		if requestHadSnapshot && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest {
			snapshotRequestAccepted = true
		}
	}))
	t.Cleanup(proxy.Close)

	localProject, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	largeBody := strings.Repeat("example adopted issue body\n", 1536)
	issues := make([]db.Issue, 0, 40)
	issueUIDs := make([]string, 0, 40)
	for i := range 40 {
		issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: localProject.ID,
			Title:     "adopted issue " + strconv.Itoa(i),
			Body:      largeBody,
			Author:    "historical-author",
		})
		require.NoError(t, err)
		issues = append(issues, issue)
		issueUIDs = append(issueUIDs, issue.UID)
	}
	source := issues[0]
	target := issues[len(issues)-1]
	_, _, err = spoke.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: source.ID,
		ToIssueID:   target.ID,
		Type:        "related",
		Author:      "historical-linker",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: source.ID,
		FromShortID:  source.ShortID,
		FromUID:      source.UID,
		ToShortID:    target.ShortID,
		ToUID:        target.UID,
		Actor:        "historical-linker",
	})
	require.NoError(t, err)

	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 proxy.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         hubProject.UID,
		"project_name":            localProject.Name,
		"replay_horizon_event_id": hubBinding.ReplayHorizonEventID,
		"token":                   created.Token,
		"capabilities":            "pull,push",
		"actor":                   "agent",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, &replica)
	require.True(t, replica.Adopted)
	require.Equal(t, int64(len(issueUIDs)), replica.AdoptionSnapshotCount)

	binding, err := spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       proxy.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
		Capabilities: "pull,push",
	})
	require.NoError(t, err)
	require.Greater(t, len(requestSizes), 1)
	for _, requestSize := range requestSizes {
		assert.LessOrEqual(t, requestSize, maxFederationPushIngestBodyBytes)
	}
	require.NotEmpty(t, baselineStages)
	for _, stage := range baselineStages[:len(baselineStages)-1] {
		assert.Equal(t, api.FederationAdoptionBaselineOpen, stage)
	}
	assert.Equal(t, api.FederationAdoptionBaselineComplete, baselineStages[len(baselineStages)-1])
	require.Len(t, baselineEndEventIDs, len(baselineStages))
	require.Len(t, baselineLastEventIDs, len(baselineStages))
	terminalEndEventID := baselineLastEventIDs[len(baselineLastEventIDs)-1]
	for _, endEventID := range baselineEndEventIDs {
		assert.Equal(t, terminalEndEventID, endEventID)
	}
	require.True(t, sawContinuationBoundActor)

	for _, issueUID := range issueUIDs {
		expectedAuthor, ok := snapshotAuthorsByUID[issueUID]
		require.True(t, ok)
		pushed, err := hub.DB.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		require.NoError(t, err)
		assert.Equal(t, expectedAuthor, pushed.Author)
	}
	pullReplica := testenv.New(t)
	pullEnrollment, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "split-adopt-pull-replica-token",
		SpokeInstanceUID: pullReplica.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull",
		Actor:            "replica-agent",
	})
	require.NoError(t, err)
	pullProject, err := pullReplica.DB.CreateProjectWithUID(ctx, "replica-project", hubProject.UID)
	require.NoError(t, err)
	pullBinding, err := pullReplica.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            pullProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: hubBinding.ReplayHorizonEventID,
		PullCursorEventID:    hubBinding.ReplayHorizonEventID - 1,
		Actor:                "replica-agent",
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, SyncFederationOnce(ctx, pullReplica.DB, pullBinding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        pullEnrollment.Token,
		Capabilities: "pull",
	}))
	for _, issueUID := range issueUIDs {
		expectedAuthor, ok := snapshotAuthorsByUID[issueUID]
		require.True(t, ok)
		pulled, err := pullReplica.DB.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		require.NoError(t, err)
		assert.Equal(t, expectedAuthor, pulled.Author)
	}
	var linkCount int
	require.NoError(t, hub.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM links
		 WHERE from_issue_id IN (SELECT id FROM issues WHERE project_id = ?)
		   AND from_issue_uid = ? AND to_issue_uid = ? AND type = 'related'
		   AND author = 'historical-linker'`,
		hubProject.ID, source.UID, target.UID).Scan(&linkCount))
	assert.Equal(t, 1, linkCount)
	var quarantineCount int
	require.NoError(t, hub.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM federation_quarantine
		 WHERE project_id = ? AND direction = 'push' AND skipped_at IS NULL`,
		hubProject.ID).Scan(&quarantineCount))
	assert.Zero(t, quarantineCount)
	require.NoError(t, assertHubOriginEventCount(ctx, hub.DB, hubProject.ID, spoke.DB.InstanceUID(), len(issueUIDs)))
}

func TestSyncFederationOnceResumesSplitAdoptionBaselineAfterFailure(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)

	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:                        "resume-split-adopt-token",
		SpokeInstanceUID:             spoke.DB.InstanceUID(),
		ProjectID:                    &hubProject.ID,
		Capabilities:                 "pull,push",
		Actor:                        "agent",
		AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)

	var ingestCount int
	var firstAcceptedCursor int64
	failSecondIngest := true
	snapshotAuthorsByUID := map[string]string{}
	var sawContinuationBoundActor bool
	var snapshotRequestAccepted bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		requestSnapshotAuthors := map[string]string{}
		requestHadSnapshot := false
		forwardedIngest := r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/federation/events:ingest", hubProject.ID)
		if forwardedIngest {
			ingestCount++
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.Unmarshal(raw, &body))
			require.NotEmpty(t, body.Events)
			isSnapshotContinuation := snapshotRequestAccepted
			for _, ev := range body.Events {
				if ev.Type != "issue.snapshot" {
					continue
				}
				var payload struct {
					UID    string `json:"uid"`
					Author string `json:"author"`
				}
				require.NoError(t, json.Unmarshal(ev.Payload, &payload))
				assert.Equal(t, "historical-author", payload.Author)
				projectedAuthor := "historical-author"
				if isSnapshotContinuation {
					projectedAuthor = "agent"
					sawContinuationBoundActor = true
				}
				requestSnapshotAuthors[payload.UID] = projectedAuthor
				requestHadSnapshot = true
			}
			if ingestCount == 1 {
				firstAcceptedCursor = body.Events[len(body.Events)-1].EventID
				assert.Equal(t, api.FederationAdoptionBaselineOpen, body.AdoptionBaseline)
			}
			if failSecondIngest && ingestCount == 2 {
				assert.Equal(t, api.FederationAdoptionBaselineOpen, body.AdoptionBaseline)
				http.Error(w, "temporary proxy failure", http.StatusBadGateway)
				return
			}
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, hub.URL+r.URL.RequestURI(), bytes.NewReader(raw)) //nolint:gosec // test proxy forwards only to the local httptest hub.
		require.NoError(t, err)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // test proxy forwards only to the local httptest hub.
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		if forwardedIngest && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest {
			for uid, author := range requestSnapshotAuthors {
				snapshotAuthorsByUID[uid] = author
			}
			if requestHadSnapshot {
				snapshotRequestAccepted = true
			}
		}
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, err = io.Copy(w, resp.Body)
		require.NoError(t, err)
	}))
	t.Cleanup(proxy.Close)

	localProject, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	largeBody := strings.Repeat("example adopted issue body\n", 1536)
	issueUIDs := make([]string, 0, 40)
	for i := range 40 {
		issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: localProject.ID,
			Title:     "adopted issue " + strconv.Itoa(i),
			Body:      largeBody,
			Author:    "historical-author",
		})
		require.NoError(t, err)
		issueUIDs = append(issueUIDs, issue.UID)
	}

	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 proxy.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         hubProject.UID,
		"project_name":            localProject.Name,
		"replay_horizon_event_id": hubBinding.ReplayHorizonEventID,
		"token":                   created.Token,
		"capabilities":            "pull,push",
		"actor":                   "agent",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, &replica)
	require.True(t, replica.Adopted)

	binding, err := spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{
		HubURL:       proxy.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
		Capabilities: "pull,push",
	}
	err = SyncFederationOnce(ctx, spoke.DB, binding, creds)
	require.Error(t, err)
	require.Positive(t, firstAcceptedCursor)
	binding, err = spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	assert.Equal(t, firstAcceptedCursor, binding.PushCursorEventID)
	authorized, err := hub.DB.AuthorizeFederationToken(ctx, created.Token, hubProject.ID, "push")
	require.NoError(t, err)
	assert.False(t, authorized.AllowAdoptionSnapshotAuthors)

	failSecondIngest = false
	err = SyncFederationOnce(ctx, spoke.DB, binding, creds)
	require.NoError(t, err)
	require.True(t, sawContinuationBoundActor)
	for _, issueUID := range issueUIDs {
		expectedAuthor, ok := snapshotAuthorsByUID[issueUID]
		require.True(t, ok)
		pushed, err := hub.DB.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		require.NoError(t, err)
		assert.Equal(t, expectedAuthor, pushed.Author)
	}
	authorized, err = hub.DB.AuthorizeFederationToken(ctx, created.Token, hubProject.ID, "push")
	require.NoError(t, err)
	assert.False(t, authorized.AllowAdoptionSnapshotAuthors)
	require.NoError(t, assertHubOriginEventCount(ctx, hub.DB, hubProject.ID, spoke.DB.InstanceUID(), len(issueUIDs)))
}

func TestSyncFederationOncePushesLargeAdoptionMetadataWithHistoricalSnapshots(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)

	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:                        "split-metadata-adopt-token",
		SpokeInstanceUID:             spoke.DB.InstanceUID(),
		ProjectID:                    &hubProject.ID,
		Capabilities:                 "pull,push",
		Actor:                        "agent",
		AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)

	localProject, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	metadataValue, err := json.Marshal(strings.Repeat("m", 620<<10))
	require.NoError(t, err)
	metadataOut, err := spoke.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  localProject.ID,
		IfMatchRev: db.IfMatch(localProject.Revision),
		Actor:      "agent",
		Patch: map[string]json.RawMessage{
			"large": metadataValue,
		},
	})
	require.NoError(t, err)
	localProject = metadataOut.Project
	issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: localProject.ID,
		Title:     "adopted issue",
		Body:      strings.Repeat("example adopted issue body\n", 4096),
		Author:    "historical-author",
	})
	require.NoError(t, err)

	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         hubProject.UID,
		"project_name":            localProject.Name,
		"replay_horizon_event_id": hubBinding.ReplayHorizonEventID,
		"token":                   created.Token,
		"capabilities":            "pull,push",
		"actor":                   "agent",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, &replica)
	require.True(t, replica.Adopted)
	require.Equal(t, int64(1), replica.AdoptionSnapshotCount)

	binding, err := spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
		Capabilities: "pull,push",
	})
	require.NoError(t, err)

	pushed, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "historical-author", pushed.Author)
	authorized, err := hub.DB.AuthorizeFederationToken(ctx, created.Token, hubProject.ID, "push")
	require.NoError(t, err)
	assert.False(t, authorized.AllowAdoptionSnapshotAuthors)
}

func TestSyncFederationOnceConsumesAdoptionMarkerForMetadataOnlyProject(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)

	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:                        "metadata-only-adopt-token",
		SpokeInstanceUID:             spoke.DB.InstanceUID(),
		ProjectID:                    &hubProject.ID,
		Capabilities:                 "pull,push",
		Actor:                        "agent",
		AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)

	localProject, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	metadataOut, err := spoke.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  localProject.ID,
		IfMatchRev: db.IfMatch(localProject.Revision),
		Actor:      "agent",
		Patch: map[string]json.RawMessage{
			"area": json.RawMessage(`"docs"`),
		},
	})
	require.NoError(t, err)
	localProject = metadataOut.Project

	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         hubProject.UID,
		"project_name":            localProject.Name,
		"replay_horizon_event_id": hubBinding.ReplayHorizonEventID,
		"token":                   created.Token,
		"capabilities":            "pull,push",
		"actor":                   "agent",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, &replica)
	require.True(t, replica.Adopted)
	assert.Equal(t, int64(0), replica.AdoptionSnapshotCount)

	binding, err := spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
		Capabilities: "pull,push",
	})
	require.NoError(t, err)

	authorized, err := hub.DB.AuthorizeFederationToken(ctx, created.Token, hubProject.ID, "push")
	require.NoError(t, err)
	assert.False(t, authorized.AllowAdoptionSnapshotAuthors)
	require.NoError(t, assertHubOriginEventCount(ctx, hub.DB, hubProject.ID, spoke.DB.InstanceUID(), 1))
}

func TestSyncFederationOnceConsumesAdoptionMarkerForEmptyProject(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)

	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:                        "empty-adopt-token",
		SpokeInstanceUID:             spoke.DB.InstanceUID(),
		ProjectID:                    &hubProject.ID,
		Capabilities:                 "pull,push",
		Actor:                        "agent",
		AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)

	localProject, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         hubProject.UID,
		"project_name":            localProject.Name,
		"replay_horizon_event_id": hubBinding.ReplayHorizonEventID,
		"token":                   created.Token,
		"capabilities":            "pull,push",
		"actor":                   "agent",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, &replica)
	require.True(t, replica.Adopted)
	assert.Equal(t, int64(0), replica.AdoptionSnapshotCount)

	binding, err := spoke.DB.FederationBindingByProject(ctx, replica.Project.ID)
	require.NoError(t, err)
	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
		Capabilities: "pull,push",
	})
	require.NoError(t, err)

	authorized, err := hub.DB.AuthorizeFederationToken(ctx, created.Token, hubProject.ID, "push")
	require.NoError(t, err)
	assert.False(t, authorized.AllowAdoptionSnapshotAuthors)
	require.NoError(t, assertHubOriginEventCount(ctx, hub.DB, hubProject.ID, spoke.DB.InstanceUID(), 1))
}

func TestNextFederationPushIngestBatchRejectsOversizedAdoptionSnapshotBaseline(t *testing.T) {
	projectUID := mustTestUID(t)
	issueUID := mustTestUID(t)
	events := []db.Event{
		syncTestEvent(t, 1, projectUID, "spoke-project", nil, "project.metadata_updated",
			`{"diff":{"area":{"from":null,"to":"docs"}}}`),
		syncTestEvent(t, 2, projectUID, "spoke-project", &issueUID, "issue.snapshot",
			`{"uid":"`+issueUID+`","short_id":"`+shortIDForSyncTest(issueUID)+`","title":"huge","body":"`+
				strings.Repeat("x", 64<<20)+
				`","author":"historical-author","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`),
	}

	_, _, _, err := nextFederationPushIngestBatch(events)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "adoption snapshot baseline")
}

func TestNextFederationPushIngestBatchMarksSingleSnapshotBaselineComplete(t *testing.T) {
	projectUID := mustTestUID(t)
	issueUID := mustTestUID(t)
	events := []db.Event{
		syncTestEvent(t, 1, projectUID, "spoke-project", &issueUID, "issue.snapshot",
			`{"uid":"`+issueUID+`","short_id":"`+shortIDForSyncTest(issueUID)+`","title":"terminal","body":"","author":"historical-author","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`),
	}

	batch, adoptionBaseline, adoptionBaselineEndEventID, err := nextFederationPushIngestBatch(events)

	require.NoError(t, err)
	require.Len(t, batch, 1)
	assert.Equal(t, api.FederationAdoptionBaselineComplete, adoptionBaseline)
	assert.Equal(t, int64(1), adoptionBaselineEndEventID)
}

func TestSyncFederationOncePushesAllPendingBatchesBeforePull(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	for i := range 1001 {
		_, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID,
			Title:     "pending batch " + strconv.Itoa(i),
			Author:    "tester",
		})
		require.NoError(t, err)
	}
	var batchSizes []int
	polled := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			require.False(t, polled, "push batches must drain before pull")
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			batchSizes = append(batchSizes, len(body.Events))
			require.NotEmpty(t, body.Events)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          len(body.Events),
				Duplicates:        0,
				PushCursorEventID: body.Events[len(body.Events)-1].EventID,
			}))
		case "/api/v1/projects/42/federation/events":
			polled = true
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{},
				NextAfterID: 1,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1000, 1}, batchSizes)
}

func TestSyncFederationOnceSplitsLargePushRequestsBeforePull(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "agent",
		Enabled:              true,
	})
	require.NoError(t, err)
	largeBody := strings.Repeat("example issue body\n", 1536)
	for i := range 40 {
		_, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID,
			Title:     "pending issue " + strconv.Itoa(i),
			Body:      largeBody,
			Author:    "agent",
		})
		require.NoError(t, err)
	}
	var batchSizes []int
	var requestSizes []int
	polled := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events:ingest":
			assert.False(t, polled, "push batches must drain before pull")
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			requestSizes = append(requestSizes, len(raw))
			var body api.FederationIngestEventsRequestBody
			require.NoError(t, json.Unmarshal(raw, &body))
			batchSizes = append(batchSizes, len(body.Events))
			require.NotEmpty(t, body.Events)
			require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
				Accepted:          len(body.Events),
				Duplicates:        0,
				PushCursorEventID: body.Events[len(body.Events)-1].EventID,
			}))
		case "/api/v1/projects/42/federation/events":
			polled = true
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{},
				NextAfterID: 1,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	err = SyncFederationOnce(ctx, spoke.DB, binding, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	})
	require.NoError(t, err)
	require.Greater(t, len(batchSizes), 1)
	pushed := 0
	for _, batchSize := range batchSizes {
		pushed += batchSize
	}
	assert.Equal(t, 40, pushed)
	for _, requestSize := range requestSizes {
		assert.LessOrEqual(t, requestSize, maxFederationPushIngestBodyBytes)
	}
}

func TestFederationRunnerNoBindingsMakesNoRequests(t *testing.T) {
	store, _ := openDaemonclientTestDB(t)
	runner := &Runner{DB: store}

	require.NoError(t, runner.RunOnce(context.Background()))
}

func TestFederationRunnerNoBindingsNoNetwork(t *testing.T) {
	store, _ := openDaemonclientTestDB(t)
	runner := &Runner{DB: store}
	t.Setenv("KATA_HOME", t.TempDir())
	requested := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested = true
		http.Error(w, "unexpected federation request", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	require.NoError(t, config.WriteFederationCredential("unused-project", config.FederationCredential{
		HubURL:       srv.URL,
		HubProjectID: 42,
		Token:        "unused-token",
	}))

	require.NoError(t, runner.RunOnce(context.Background()))
	require.False(t, requested)
}

func TestFederationRunnerUsesClientTimeout(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", spoke.Home)
	project, err := spoke.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events":
			time.Sleep(500 * time.Millisecond)
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
				Events:      []api.EventEnvelope{},
				NextAfterID: 1,
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "runner-timeout-token",
	}))
	runner := &Runner{DB: spoke.DB, Opts: clientpkg.Opts{Timeout: 50 * time.Millisecond}}

	start := time.Now()
	err = runner.RunOnce(ctx)

	require.Error(t, err)
	assert.Less(t, time.Since(start), 300*time.Millisecond)
}

func TestFederationRunnerSkipsArchivedSpokeBindings(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", t.TempDir())
	project, err := spoke.DB.CreateProject(ctx, "archived-spoke")
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, _, err = spoke.DB.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID,
		Actor:     "tester",
		Force:     true,
	})
	require.NoError(t, err)
	requested := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested = true
		http.Error(w, "archived binding should not sync", http.StatusTeapot)
	}))
	t.Cleanup(hub.Close)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "archived-token",
	}))

	require.NoError(t, (&Runner{DB: spoke.DB}).RunOnce(ctx))
	assert.False(t, requested)
}

func TestFederationRunnerRetriesAfterSyncError(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", t.TempDir())
	offlineHubURL := fastFailHubURL(t)
	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "runner-retry-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
		Actor:            "tester",
	})
	require.NoError(t, err)
	spokeProject, err := spoke.DB.CreateProjectWithUID(ctx, "hub", hubProject.UID)
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               offlineHubURL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       offlineHubURL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}))
	issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spokeProject.ID,
		Title:     "retry after outage",
		Author:    "tester",
	})
	require.NoError(t, err)

	wake := make(chan struct{}, 1)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	runner := &Runner{
		DB:       spoke.DB,
		Interval: time.Hour,
		Wake:     wake,
		Debounce: time.Millisecond,
	}
	go func() {
		errCh <- runner.Run(runCtx)
	}()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}))
	wake <- struct{}{}
	require.Eventually(t, func() bool {
		_, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-errCh, context.Canceled)
}

func TestFederationRunnerRunOnceContinuesAfterBindingError(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", t.TempDir())

	badProject, err := spoke.DB.CreateProject(ctx, "bad")
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            badProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         999,
		HubProjectUID:        badProject.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "runner-continues-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
		Actor:            "tester",
	})
	require.NoError(t, err)
	goodProject, err := spoke.DB.CreateProjectWithUID(ctx, hubProject.Name, hubProject.UID)
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            goodProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(goodProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}))
	issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: goodProject.ID,
		Title:     "good binding still syncs",
		Author:    "tester",
	})
	require.NoError(t, err)

	err = (&Runner{DB: spoke.DB}).RunOnce(ctx)

	require.Error(t, err)
	pushed, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "good binding still syncs", pushed.Title)
}

func TestFederationRunnerRunOnceNoBindingsDoesNotCreateSyncStatus(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)

	require.NoError(t, (&Runner{DB: spoke.DB}).RunOnce(ctx))

	var rows int
	require.NoError(t, spoke.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_sync_status`).Scan(&rows))
	assert.Equal(t, 0, rows)
}

func TestFederationRunnerRunOnceMalformedCredentialsRecordsEachSpokeError(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", spoke.Home)
	projectA, err := spoke.DB.CreateProject(ctx, "spoke-a")
	require.NoError(t, err)
	projectB, err := spoke.DB.CreateProject(ctx, "spoke-b")
	require.NoError(t, err)
	for _, project := range []db.Project{projectA, projectB} {
		_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID:            project.ID,
			Role:                 db.FederationRoleSpoke,
			HubURL:               "http://127.0.0.1:1",
			HubProjectID:         42,
			HubProjectUID:        project.UID,
			ReplayHorizonEventID: 1,
			Enabled:              true,
		})
		require.NoError(t, err)
	}
	credPath, err := config.FederationCredentialsPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(credPath, []byte("[projects\n"), 0o600))

	err = (&Runner{DB: spoke.DB}).RunOnce(ctx)

	require.Error(t, err)
	for _, project := range []db.Project{projectA, projectB} {
		status := requireFederationSyncStatus(t, spoke.DB, project.ID)
		assertStatusTimeSet(t, status.LastErrorAt)
		require.NotNil(t, status.LastError)
		assert.Contains(t, *status.LastError, "parse")
	}
}

func TestFederationRunnerRunOnceKeepsClaimRetryErrorWhenPullSucceeds(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", spoke.Home)
	project, issue, binding := createPendingClaimRetrySpoke(t, spoke.DB, "retry-error-preserved")
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/42/federation/events":
			require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{NextAfterID: binding.PullCursorEventID}))
		case "/api/v1/projects/42/issues/" + issue.UID + "/lease/actions/acquire":
			http.Error(w, "temporary claim failure", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: 42,
		Token:        "token",
	}))
	_, err := spoke.DB.EnqueuePendingClaim(ctx, pendingClaimParams(spoke.DB, project.ID, issue.ShortID, "retry-cli"))
	require.NoError(t, err)

	err = (&Runner{DB: spoke.DB}).RunOnce(ctx)

	require.Error(t, err)
	status := requireFederationSyncStatus(t, spoke.DB, project.ID)
	assertStatusTimeSet(t, status.LastPullSuccessAt)
	assertStatusTimeSet(t, status.LastErrorAt)
	require.NotNil(t, status.LastError)
	assert.Contains(t, *status.LastError, "returned 500")
}

func TestPendingClaimRetryResolvesAfterHubReconnectWithFreshTimedTTL(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", spoke.Home)
	hubProject := createFederatedHubForPush(t, hub)
	hubIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     "timed pending",
		Author:    "tester",
	})
	require.NoError(t, err)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "pending-retry-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,claim",
		Actor:            "tester",
	})
	require.NoError(t, err)
	spokeProject, err := spoke.DB.CreateProjectWithUID(ctx, hubProject.Name, hubProject.UID)
	require.NoError(t, err)
	spokeIssue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:       spokeProject.ID,
		Title:           hubIssue.Title,
		Author:          "tester",
		UID:             hubIssue.UID,
		ShortIDOverride: hubIssue.ShortID,
	})
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
		Capabilities: "pull,claim",
	}))
	pending, err := spoke.DB.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: spokeProject.ID,
		IssueRef:  spokeIssue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: spoke.DB.InstanceUID(),
			Holder:            "tester",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       5 * time.Minute,
		Purpose:   "edit",
		Now:       time.Now().UTC().Add(-6 * time.Hour),
	})
	require.NoError(t, err)

	retryStart := time.Now().UTC()
	err = (&Runner{DB: spoke.DB}).RunOnce(ctx)
	require.NoError(t, err)

	var resolvedAt time.Time
	require.NoError(t, spoke.DB.QueryRowContext(ctx,
		`SELECT resolved_at FROM pending_claim_requests WHERE request_uid = ?`, pending.RequestUID).Scan(&resolvedAt))
	status, err := spoke.DB.ClaimStatus(ctx, spokeProject.ID, spokeIssue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, status.Held)
	require.NotNil(t, status.Claim)
	require.NotNil(t, status.Claim.ExpiresAt)
	assert.Equal(t, "tester", status.Holder.Holder)
	assert.Equal(t, "cli", status.Holder.ClientKind)
	assert.True(t, status.Claim.ExpiresAt.After(retryStart.Add(299*time.Second)),
		"timed retry must request a fresh TTL at retry time, got %s from retry start %s",
		status.Claim.ExpiresAt, retryStart)
}

func TestPendingClaimRetryUnknownCapabilitiesTransportFailureRetriesAfterReconnect(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", spoke.Home)
	hubProject, err := hub.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	hubIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     "pending from replica setup",
		Author:    "tester",
	})
	require.NoError(t, err)
	var meta api.ProjectFederationBody
	postJSON(t, hub.URL, "/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "tester"}, &meta)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "replica-claim-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,claim",
		Actor:            "tester",
	})
	require.NoError(t, err)
	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
		"token":                   created.Token,
		"actor":                   "tester",
	}, &replica)
	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	require.Empty(t, creds.Projects[replica.Project.UID].Capabilities)

	require.NoError(t, (&Runner{DB: spoke.DB}).RunOnce(ctx))
	spokeIssue, err := spoke.DB.IssueByUID(ctx, hubIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	pending, err := spoke.DB.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: replica.Project.ID,
		IssueRef:  spokeIssue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: spoke.DB.InstanceUID(),
			Holder:            "tester",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Purpose:   "edit",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(replica.Project.UID, config.FederationCredential{
		HubURL:       "http://127.0.0.1:1",
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}))

	require.Error(t, (&Runner{DB: spoke.DB}).RunOnce(ctx))
	var firstAttemptAt time.Time
	require.NoError(t, spoke.DB.QueryRowContext(ctx,
		`SELECT last_attempt_at FROM pending_claim_requests WHERE request_uid = ?`, pending.RequestUID).Scan(&firstAttemptAt))
	assert.False(t, firstAttemptAt.IsZero())
	require.NoError(t, config.WriteFederationCredential(replica.Project.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        created.Token,
	}))

	require.NoError(t, (&Runner{DB: spoke.DB}).RunOnce(ctx))

	var resolvedAt time.Time
	require.NoError(t, spoke.DB.QueryRowContext(ctx,
		`SELECT resolved_at FROM pending_claim_requests WHERE request_uid = ?`, pending.RequestUID).Scan(&resolvedAt))
	assert.False(t, resolvedAt.IsZero())
	status, err := spoke.DB.ClaimStatus(ctx, replica.Project.ID, spokeIssue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, status.Held)
	assert.Equal(t, "tester", status.Holder.Holder)
}

func TestFederationClaimRetryCapabilityRules(t *testing.T) {
	ctx := context.Background()
	t.Run("known credential without claim rejects without claim request", func(t *testing.T) {
		spoke := testenv.New(t)
		t.Setenv("KATA_HOME", spoke.Home)
		project, issue, binding := createPendingClaimRetrySpoke(t, spoke.DB, "known-no-claim")
		claimRequests := 0
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/projects/42/federation/events" {
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{NextAfterID: binding.PullCursorEventID}))
				return
			}
			if r.URL.Path == "/api/v1/projects/42/issues/"+issue.ShortID+"/lease/actions/acquire" {
				claimRequests++
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(hub.Close)
		require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
			HubURL:       hub.URL,
			HubProjectID: 42,
			Token:        "token",
			Capabilities: "pull,push",
		}))
		pending, err := spoke.DB.EnqueuePendingClaim(ctx, pendingClaimParams(spoke.DB, project.ID, issue.ShortID, "known-cli"))
		require.NoError(t, err)

		require.NoError(t, (&Runner{DB: spoke.DB}).RunOnce(ctx))

		assert.Equal(t, 0, claimRequests)
		assertPendingRejectedWithError(t, spoke.DB, pending.RequestUID, "lease capability unavailable")
		status := requireFederationSyncStatus(t, spoke.DB, project.ID)
		assertStatusTimeSet(t, status.LastPushStartedAt)
		assertStatusTimeSet(t, status.LastPushSuccessAt)
		assert.Nil(t, status.LastErrorAt)
		assert.Nil(t, status.LastError)
	})

	t.Run("older credential without capabilities attempts once and records 403", func(t *testing.T) {
		spoke := testenv.New(t)
		t.Setenv("KATA_HOME", spoke.Home)
		project, issue, binding := createPendingClaimRetrySpoke(t, spoke.DB, "older-no-cap")
		claimRequests := 0
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/projects/42/federation/events":
				require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{NextAfterID: binding.PullCursorEventID}))
			case "/api/v1/projects/42/issues/" + issue.UID + "/lease/actions/acquire":
				claimRequests++
				http.Error(w, "claim forbidden", http.StatusForbidden)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(hub.Close)
		require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
			HubURL:       hub.URL,
			HubProjectID: 42,
			Token:        "token",
		}))
		pending, err := spoke.DB.EnqueuePendingClaim(ctx, pendingClaimParams(spoke.DB, project.ID, issue.ShortID, "older-cli"))
		require.NoError(t, err)

		require.NoError(t, (&Runner{DB: spoke.DB}).RunOnce(ctx))
		require.NoError(t, (&Runner{DB: spoke.DB}).RunOnce(ctx))

		assert.Equal(t, 1, claimRequests)
		assertPendingRejectedWithError(t, spoke.DB, pending.RequestUID, "returned 403")
		status := requireFederationSyncStatus(t, spoke.DB, project.ID)
		assertStatusTimeSet(t, status.LastPushStartedAt)
		assertStatusTimeSet(t, status.LastPushSuccessAt)
		assert.Nil(t, status.LastErrorAt)
		assert.Nil(t, status.LastError)
	})
}

func requireFederationSyncStatus(t *testing.T, store *sqlitestore.Store, projectID int64) db.FederationSyncStatus {
	t.Helper()
	got, err := store.FederationSyncStatusByProject(context.Background(), projectID)
	require.NoError(t, err)
	return got
}

func assertStatusTimeSet(t *testing.T, got *time.Time) {
	t.Helper()
	require.NotNil(t, got)
	assert.False(t, got.IsZero())
}

func mustIssueUIDByTitle(t *testing.T, store *sqlitestore.Store, title string) string {
	t.Helper()
	var uid string
	require.NoError(t, store.QueryRowContext(context.Background(),
		`SELECT uid FROM issues WHERE title = ?`, title).Scan(&uid))
	return uid
}

func createPendingClaimRetrySpoke(t *testing.T, store *sqlitestore.Store, name string) (db.Project, db.Issue, db.FederationBinding) {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, name)
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending claim",
		Author:    "tester",
	})
	require.NoError(t, err)
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		Actor:                "tester",
		Enabled:              true,
	})
	require.NoError(t, err)
	return project, issue, binding
}

func pendingClaimParams(store *sqlitestore.Store, projectID int64, issueRef, holder string) db.PendingClaimParams {
	return db.PendingClaimParams{
		ProjectID: projectID,
		IssueRef:  issueRef,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(),
			Holder:            holder,
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	}
}

func assertPendingRejectedWithError(t *testing.T, store *sqlitestore.Store, requestUID, wantError string) {
	t.Helper()
	var (
		rejectedAt time.Time
		lastError  string
	)
	require.NoError(t, store.QueryRowContext(context.Background(), `
		SELECT rejected_at, last_error
		  FROM pending_claim_requests
		 WHERE request_uid = ?`, requestUID).Scan(&rejectedAt, &lastError))
	assert.False(t, rejectedAt.IsZero())
	assert.Contains(t, lastError, wantError)
}

func assertHubOriginEventCount(ctx context.Context, store *sqlitestore.Store, projectID int64, originInstanceUID string, want int) error {
	var got int
	err := store.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE project_id = ? AND origin_instance_uid = ?`,
		projectID, originInstanceUID).Scan(&got)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("origin event count = %d, want %d", got, want)
	}
	return nil
}

type matrixFederationProject struct {
	tag        string
	hub        db.Project
	local      db.Project
	issue      db.Issue
	credential config.FederationCredential
	before     bool
	enrolled   bool
}

func matrixCreateHubProject(t *testing.T, hub *testenv.Env, name string) db.Project {
	t.Helper()
	project, err := hub.DB.CreateProject(context.Background(), name)
	require.NoError(t, err)
	_, err = hub.DB.EnableProjectFederation(context.Background(), project.ID, "tester")
	require.NoError(t, err)
	return project
}

func matrixCreateEnrollment(
	t *testing.T,
	hub, spoke *testenv.Env,
	project db.Project,
	token string,
) db.CreatedFederationEnrollment {
	t.Helper()
	created, err := hub.DB.CreateFederationEnrollment(context.Background(), db.CreateFederationEnrollmentParams{
		Token:                        token,
		SpokeInstanceUID:             spoke.DB.InstanceUID(),
		ProjectID:                    &project.ID,
		Capabilities:                 "pull,push",
		Actor:                        "tester",
		AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	return created
}

func matrixCredential(hubURL string, projectID int64, token string) config.FederationCredential {
	return config.FederationCredential{
		HubURL:       hubURL,
		HubProjectID: projectID,
		Token:        token,
		Capabilities: "pull,push",
	}
}

func matrixCreateInitialState(t *testing.T, store *sqlitestore.Store, project db.Project, tag string) db.Issue {
	t.Helper()
	owner := tag + "-initial-owner"
	priority := int64(3)
	issue, _, err := store.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     tag + " initial title",
		Body:      tag + " initial body",
		Author:    "tester",
		Labels:    []string{tag + "-initial-label"},
		Owner:     &owner,
		Priority:  &priority,
		Metadata: map[string]json.RawMessage{
			"phase": json.RawMessage(`"initial"`),
		},
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(context.Background(), db.CreateCommentParams{
		IssueID: issue.ID,
		Author:  "tester",
		Body:    tag + " initial comment",
	})
	require.NoError(t, err)
	_, _, changed, err := store.CloseIssue(context.Background(), issue.ID, "done", "tester", "initial close", nil)
	require.NoError(t, err)
	require.True(t, changed)
	issue, _, changed, err = store.ReopenIssue(context.Background(), issue.ID, "tester")
	require.NoError(t, err)
	require.True(t, changed)
	_, err = store.PatchProjectMetadata(context.Background(), db.PatchProjectMetadataIn{
		ProjectID: project.ID,
		Actor:     "tester",
		Patch: map[string]json.RawMessage{
			"phase": json.RawMessage(`"` + tag + `-initial"`),
		},
	})
	require.NoError(t, err)
	return issue
}

func matrixCreateCrossProjectLink(t *testing.T, store *sqlitestore.Store, first, peer db.Issue) {
	t.Helper()
	_, _, err := store.CreateLinkAndEvent(context.Background(), db.CreateLinkParams{
		FromIssueID: first.ID,
		ToIssueID:   peer.ID,
		Type:        "blocks",
		Author:      "tester",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: first.ID,
		FromShortID:  first.ShortID,
		FromUID:      first.UID,
		ToShortID:    peer.ShortID,
		ToUID:        peer.UID,
		Actor:        "tester",
	})
	require.NoError(t, err)
}

func matrixAdoptProject(
	t *testing.T,
	hub, spoke *sqlitestore.Store,
	hubURL string,
	project *matrixFederationProject,
) {
	t.Helper()
	hubBinding, err := hub.FederationBindingByProject(context.Background(), project.hub.ID)
	require.NoError(t, err)
	result, err := spoke.AdoptProjectIntoFederation(context.Background(), db.AdoptProjectIntoFederationParams{
		ProjectID:            project.local.ID,
		HubURL:               hubURL,
		HubProjectID:         project.hub.ID,
		HubProjectUID:        project.hub.UID,
		ReplayHorizonEventID: hubBinding.ReplayHorizonEventID,
		Actor:                "tester",
	})
	require.NoError(t, err)
	project.local = result.Project
	if project.issue.ID != 0 {
		project.issue, err = spoke.IssueByUID(context.Background(), project.issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
	}
	project.enrolled = true
}

func matrixApplyPostEnrollmentState(
	t *testing.T,
	store *sqlitestore.Store,
	issue db.Issue,
	tag string,
) db.Issue {
	t.Helper()
	title := tag + " final title"
	body := tag + " final body"
	owner := tag + "-final-owner"
	priority := int64(1)
	result, err := store.EditIssueAtomic(context.Background(), db.EditIssueAtomicParams{
		IssueID: issue.ID, Actor: "tester", Title: &title, Body: &body,
		Owner: &owner, SetPriority: &priority,
	})
	require.NoError(t, err)
	_, _, err = store.AddLabelAndEvent(context.Background(), issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: tag + "-final-label", Actor: "tester",
	})
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(context.Background(), db.PatchIssueMetadataIn{
		IssueID: issue.ID,
		Actor:   "tester",
		Patch: map[string]json.RawMessage{
			"phase": json.RawMessage(`"final"`),
		},
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(context.Background(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "tester", Body: tag + " final comment",
	})
	require.NoError(t, err)
	return result.Issue
}

func matrixSyncProject(t *testing.T, spoke *sqlitestore.Store, project *matrixFederationProject) {
	t.Helper()
	binding, err := spoke.FederationBindingByProject(context.Background(), project.local.ID)
	require.NoError(t, err)
	require.NoError(t, SyncFederationOnce(context.Background(), spoke, binding, project.credential))
}

func matrixAssertPortableState(
	t *testing.T,
	hub *sqlitestore.Store,
	hubProjectID int64,
	issueUID, tag string,
) {
	t.Helper()
	issue, err := hub.IssueByUID(context.Background(), issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, hubProjectID, issue.ProjectID)
	assert.Equal(t, tag+" final title", issue.Title)
	assert.Equal(t, tag+" final body", issue.Body)
	require.NotNil(t, issue.Owner)
	assert.Equal(t, tag+"-final-owner", *issue.Owner)
	require.NotNil(t, issue.Priority)
	assert.Equal(t, int64(1), *issue.Priority)
	assert.Equal(t, "open", issue.Status)
	assert.JSONEq(t, `{"phase":"final"}`, string(issue.Metadata))
	labels, err := hub.LabelsByIssue(context.Background(), issue.ID)
	require.NoError(t, err)
	var gotLabels []string
	for _, label := range labels {
		gotLabels = append(gotLabels, label.Label)
	}
	assert.ElementsMatch(t, []string{tag + "-initial-label", tag + "-final-label"}, gotLabels)
	comments, err := hub.CommentsByIssue(context.Background(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	assert.Equal(t, tag+" initial comment", comments[0].Body)
	assert.Equal(t, tag+" final comment", comments[1].Body)
	project, err := hub.ProjectByID(context.Background(), hubProjectID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"phase":"`+tag+`-initial"}`, string(project.Metadata))
}

func matrixAssertPushDrained(t *testing.T, spoke *sqlitestore.Store, projectID int64) {
	t.Helper()
	binding, err := spoke.FederationBindingByProject(context.Background(), projectID)
	require.NoError(t, err)
	pending, _, err := spoke.PendingFederationPushStats(
		context.Background(), projectID, spoke.InstanceUID(), binding.PushCursorEventID)
	require.NoError(t, err)
	assert.Zero(t, pending)
	_, err = spoke.ActiveFederationQuarantine(context.Background(), projectID, db.FederationQuarantineDirectionPush)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func matrixAssertLinkCount(t *testing.T, store *sqlitestore.Store, firstUID, peerUID string, want int) {
	t.Helper()
	var got int
	require.NoError(t, store.QueryRowContext(context.Background(), `
		SELECT COUNT(*)
		  FROM links
		 WHERE from_issue_uid = ? AND to_issue_uid = ? AND type = 'blocks'`,
		firstUID, peerUID).Scan(&got))
	assert.Equal(t, want, got)
}

func matrixApplyHubFollowup(
	t *testing.T,
	hub *sqlitestore.Store,
	first, peer *matrixFederationProject,
) {
	t.Helper()
	firstIssue, err := hub.IssueByUID(context.Background(), first.issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	title := "first hub followup"
	_, err = hub.EditIssueAtomic(context.Background(), db.EditIssueAtomicParams{
		IssueID: firstIssue.ID, Actor: "hub-operator", Title: &title,
	})
	require.NoError(t, err)
	_, _, err = hub.CreateComment(context.Background(), db.CreateCommentParams{
		IssueID: firstIssue.ID, Author: "hub-operator", Body: "first hub comment",
	})
	require.NoError(t, err)
	peerIssue, err := hub.IssueByUID(context.Background(), peer.issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	_, _, err = hub.AddLabelAndEvent(context.Background(), peerIssue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "peer-hub-label", Actor: "hub-operator",
	})
	require.NoError(t, err)
	_, _, changed, err := hub.CloseIssue(
		context.Background(), peerIssue.ID, "done", "hub-operator", "hub close", nil)
	require.NoError(t, err)
	require.True(t, changed)
}

func matrixAssertPulledFollowup(
	t *testing.T,
	spoke *sqlitestore.Store,
	first, peer *matrixFederationProject,
) {
	t.Helper()
	firstIssue, err := spoke.IssueByUID(context.Background(), first.issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, first.local.ID, firstIssue.ProjectID)
	assert.Equal(t, "first hub followup", firstIssue.Title)
	comments, err := spoke.CommentsByIssue(context.Background(), firstIssue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 3)
	assert.Equal(t, "first hub comment", comments[2].Body)
	peerIssue, err := spoke.IssueByUID(context.Background(), peer.issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, peer.local.ID, peerIssue.ProjectID)
	assert.Equal(t, "closed", peerIssue.Status)
	labels, err := spoke.LabelsByIssue(context.Background(), peerIssue.ID)
	require.NoError(t, err)
	var hasHubLabel bool
	for _, label := range labels {
		hasHubLabel = hasHubLabel || label.Label == "peer-hub-label"
	}
	assert.True(t, hasHubLabel)
}

func matrixAssertPullCursors(
	t *testing.T,
	hub, spoke *sqlitestore.Store,
	projects []*matrixFederationProject,
) {
	t.Helper()
	for _, project := range projects {
		var hubHighWater int64
		require.NoError(t, hub.QueryRowContext(context.Background(),
			`SELECT COALESCE(MAX(id), 0) FROM events WHERE project_id = ?`, project.hub.ID).Scan(&hubHighWater))
		binding, err := spoke.FederationBindingByProject(context.Background(), project.local.ID)
		require.NoError(t, err)
		assert.Equal(t, hubHighWater, binding.PullCursorEventID)
	}
}

func createFederatedHubForPush(t *testing.T, env *testenv.Env) db.Project {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	return project
}

func fastFailHubURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func postJSON(t *testing.T, baseURL, path string, body, out any) {
	t.Helper()
	bs, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(baseURL+path, "application/json", bytes.NewReader(bs)) //nolint:gosec,noctx // test helper against loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equalf(t, http.StatusOK, resp.StatusCode, "POST %s", path)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

func assertFoldedIssuesMatch(t *testing.T, hub, spoke *sqlitestore.Store, hubProjectID, spokeProjectID, hubAfterID int64) {
	t.Helper()
	ctx := context.Background()
	hubEvents, err := hub.EventsAfter(ctx, db.EventsAfterParams{ProjectID: hubProjectID, AfterID: hubAfterID, Limit: 1000})
	require.NoError(t, err)
	spokeEvents, err := spoke.EventsAfter(ctx, db.EventsAfterParams{ProjectID: spokeProjectID, Limit: 1000})
	require.NoError(t, err)
	hubFold := db.FoldEvents(eventsToFold(hubEvents))
	spokeFold := db.FoldEvents(eventsToFold(spokeEvents))
	assert.Equal(t, hubFold.Issues, spokeFold.Issues)
	assert.Equal(t, hubFold.Comments, spokeFold.Comments)
	assert.Equal(t, hubFold.Labels, spokeFold.Labels)
}

func eventsToFold(events []db.Event) []db.FoldEvent {
	out := make([]db.FoldEvent, 0, len(events))
	for _, ev := range events {
		var issueUID string
		if ev.IssueUID != nil {
			issueUID = *ev.IssueUID
		}
		var relatedUID string
		if ev.RelatedIssueUID != nil {
			relatedUID = *ev.RelatedIssueUID
		}
		out = append(out, db.FoldEvent{
			UID:               ev.UID,
			OriginInstanceUID: ev.OriginInstanceUID,
			ProjectUID:        ev.ProjectUID,
			IssueUID:          issueUID,
			RelatedIssueUID:   relatedUID,
			Type:              ev.Type,
			Actor:             ev.Actor,
			HLCPhysicalMS:     ev.HLCPhysicalMS,
			HLCCounter:        ev.HLCCounter,
			CreatedAt:         ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Payload:           json.RawMessage(ev.Payload),
		})
	}
	return out
}

func syncTestEnvelope(
	t *testing.T,
	eventID int64,
	projectUID string,
	projectName string,
	issueUID *string,
	relatedIssueUID *string,
	eventType string,
	payload string,
) api.EventEnvelope {
	t.Helper()
	createdAt := time.Date(2026, 5, 23, 12, 0, int(eventID), 0, time.UTC)
	eventUID := mustTestUID(t)
	raw := json.RawMessage(payload)
	const originInstanceUID = "01HZNQ7VFPK1XGD8R5MABCD4EZ"
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               eventUID,
		OriginInstanceUID: originInstanceUID,
		ProjectUID:        projectUID,
		ProjectName:       projectName,
		IssueUID:          issueUID,
		RelatedIssueUID:   relatedIssueUID,
		Type:              eventType,
		Actor:             "hub",
		HLCPhysicalMS:     eventID,
		HLCCounter:        0,
		CreatedAt:         createdAt.Format("2006-01-02T15:04:05.000Z"),
		Payload:           raw,
	})
	require.NoError(t, err)
	return api.EventEnvelope{
		EventID:           eventID,
		EventUID:          eventUID,
		OriginInstanceUID: originInstanceUID,
		Type:              eventType,
		ProjectID:         42,
		ProjectUID:        projectUID,
		ProjectName:       projectName,
		IssueUID:          issueUID,
		RelatedIssueUID:   relatedIssueUID,
		Actor:             "hub",
		HLCPhysicalMS:     eventID,
		HLCCounter:        0,
		ContentHash:       hash,
		Payload:           raw,
		CreatedAt:         createdAt,
	}
}

func syncTestEvent(
	t *testing.T,
	eventID int64,
	projectUID string,
	projectName string,
	issueUID *string,
	eventType string,
	payload string,
) db.Event {
	t.Helper()
	createdAt := time.Date(2026, 5, 23, 12, 0, int(eventID), 0, time.UTC)
	eventUID := mustTestUID(t)
	raw := json.RawMessage(payload)
	const originInstanceUID = "01HZNQ7VFPK1XGD8R5MABCD4EY"
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               eventUID,
		OriginInstanceUID: originInstanceUID,
		ProjectUID:        projectUID,
		ProjectName:       projectName,
		IssueUID:          issueUID,
		Type:              eventType,
		Actor:             "agent",
		HLCPhysicalMS:     eventID,
		HLCCounter:        0,
		CreatedAt:         createdAt.Format("2006-01-02T15:04:05.000Z"),
		Payload:           raw,
	})
	require.NoError(t, err)
	return db.Event{
		ID:                eventID,
		UID:               eventUID,
		OriginInstanceUID: originInstanceUID,
		ProjectID:         1,
		ProjectUID:        projectUID,
		ProjectName:       projectName,
		IssueUID:          issueUID,
		Type:              eventType,
		Actor:             "agent",
		Payload:           payload,
		HLCPhysicalMS:     eventID,
		HLCCounter:        0,
		ContentHash:       hash,
		CreatedAt:         createdAt,
	}
}

func eventEnvelopeForSyncTest(event db.Event, eventID int64) api.EventEnvelope {
	var payload json.RawMessage
	if event.Payload != "" {
		payload = json.RawMessage(event.Payload)
	}
	return api.EventEnvelope{
		EventID:           eventID,
		EventUID:          event.UID,
		OriginInstanceUID: event.OriginInstanceUID,
		Type:              event.Type,
		ProjectID:         event.ProjectID,
		ProjectUID:        event.ProjectUID,
		ProjectName:       event.ProjectName,
		IssueID:           event.IssueID,
		IssueUID:          event.IssueUID,
		IssueShortID:      event.IssueShortID,
		RelatedIssueID:    event.RelatedIssueID,
		RelatedIssueUID:   event.RelatedIssueUID,
		Actor:             event.Actor,
		HLCPhysicalMS:     event.HLCPhysicalMS,
		HLCCounter:        event.HLCCounter,
		ContentHash:       event.ContentHash,
		Payload:           payload,
		CreatedAt:         event.CreatedAt,
	}
}

func rehashEventEnvelope(t *testing.T, ev *api.EventEnvelope) {
	t.Helper()
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		CreatedAt:         ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Payload:           payload,
	})
	require.NoError(t, err)
	ev.ContentHash = hash
}

func mustTestUID(t *testing.T) string {
	t.Helper()
	uid, err := katauid.New()
	require.NoError(t, err)
	return uid
}

func shortIDForSyncTest(uid string) string {
	if len(uid) <= 4 {
		return strings.ToLower(uid)
	}
	return strings.ToLower(uid[len(uid)-4:])
}

func openDaemonclientTestDB(t *testing.T) (*sqlitestore.Store, string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}
