package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	})
	require.NoError(t, err)

	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
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
	})
	require.NoError(t, err)
	var replica api.CreateFederationReplicaBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
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
		 WHERE project_id = ? AND type = 'related'`,
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
	assert.Contains(t, *status.LastError, "refusing to attach bearer token")
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
		IfMatchRev: project.Revision,
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
		Enabled:              true,
	})
	require.NoError(t, err)
	resetAt := time.Date(2026, 5, 24, 1, 0, 0, 0, time.UTC)
	require.NoError(t, spoke.DB.RecordFederationSyncReset(ctx, project.ID, resetAt))
	require.NoError(t, spoke.DB.RecordFederationSyncPullSuccess(ctx, project.ID, resetAt.Add(time.Second)))

	metaOut, err := spoke.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  project.ID,
		IfMatchRev: project.Revision,
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
		Enabled:              true,
	})
	require.NoError(t, err)
	resetAt := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, spoke.DB.RecordFederationSyncReset(ctx, project.ID, resetAt))

	metaOut, err := spoke.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  project.ID,
		IfMatchRev: project.Revision,
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
		Token:            "adopt-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)

	localProject, err := spoke.DB.CreateProject(ctx, "shared-foo")
	require.NoError(t, err)
	source, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: localProject.ID,
		Title:     "adopted source",
		Author:    "tester",
	})
	require.NoError(t, err)
	target, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: localProject.ID,
		Title:     "adopted target",
		Author:    "tester",
	})
	require.NoError(t, err)
	deleted, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: localProject.ID,
		Title:     "adopted deleted",
		Author:    "tester",
	})
	require.NoError(t, err)
	deleted, _, _, err = spoke.DB.SoftDeleteIssue(ctx, deleted.ID, "tester")
	require.NoError(t, err)
	_, err = spoke.DB.CreateLink(ctx, db.CreateLinkParams{
		ProjectID:   localProject.ID,
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
	pushedTarget, err := hub.DB.IssueByUID(ctx, target.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "adopted target", pushedTarget.Title)
	pushedDeleted, err := hub.DB.IssueByUID(ctx, deleted.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "adopted deleted", pushedDeleted.Title)
	require.NotNil(t, pushedDeleted.DeletedAt)
	var linkCount int
	require.NoError(t, hub.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM links
		 WHERE project_id = ? AND from_issue_uid = ? AND to_issue_uid = ? AND type = 'related'`,
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
	hubProject := createFederatedHubForPush(t, hub)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "runner-retry-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)
	spokeProject, err := spoke.DB.CreateProjectWithUID(ctx, "hub", hubProject.UID)
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       "http://127.0.0.1:1",
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
			Holder:            "retry-cli",
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
	assert.Equal(t, "retry-cli", status.Holder.Holder)
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
			Holder:            "replica-cli",
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
	assert.Equal(t, "replica-cli", status.Holder.Holder)
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

func createFederatedHubForPush(t *testing.T, env *testenv.Env) db.Project {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	return project
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
