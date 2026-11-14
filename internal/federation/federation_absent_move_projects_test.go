package federation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

type failingDeliveryLookup struct{ db.Storage }

func (failingDeliveryLookup) EventsByUIDs(context.Context, int64, []string) ([]db.Event, error) {
	return nil, errors.New("delivery lookup failed")
}

// A direct spoke must be able to consume B's history without local A or C rows.
func TestSyncFederationMoveHistoryWithAbsentCounterpartProjects(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "source-project", "subscribed-project", "target-project")
	spoke := newMoveDeliverySpoke(t, hub, projects[1:2])

	inbound, created, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Inbound issue", Author: "worker"})
	require.NoError(t, err)
	inMove, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: inbound.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: inbound.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NotNil(t, inMove.Event)
	outbound, outboundCreated, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "Outbound issue", Author: "worker"})
	require.NoError(t, err)
	outMove, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: outbound.ID, FromProjectID: projects[1].ID, ToProjectID: projects[2].ID, IfMatchRev: outbound.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NotNil(t, outMove.Event)
	_, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "Later work", Author: "worker"})
	require.NoError(t, err)

	expected := []string{created.UID, inMove.Event.UID, outboundCreated.UID, outMove.Event.UID, laterEvent.UID}
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	err = SyncFederationOnceWithPulledEvents(ctx, failingDeliveryLookup{spoke.env.DB}, binding, spoke.credentials[projects[1].UID], clientpkg.Opts{}, func(int64, []db.Event) {}, spoke.credentials)
	require.ErrorContains(t, err, "load delivery project")
	require.ErrorContains(t, err, projects[1].UID)
	require.ErrorContains(t, err, outboundCreated.UID)
	status, err := spoke.env.DB.FederationSyncStatusByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, status.LastError)
	require.Contains(t, *status.LastError, "load delivery project")
	require.Contains(t, *status.LastError, projects[1].UID)
	require.Contains(t, *status.LastError, outboundCreated.UID)
	var delivered []string
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[1].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
		for _, event := range events {
			if slices.Contains(expected, event.UID) {
				delivered = append(delivered, event.UID)
			}
		}
	}, spoke.credentials), "missing counterpart project rows must not stop the pull")
	require.Equal(t, expected, delivered, "delivery preserves the hub history order")
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID)
}

func TestSyncFederationReportsInvalidPullHashAndRetries(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) { testSyncFederationInvalidPullHash(t, local) })
	}
}

func testSyncFederationInvalidPullHash(t *testing.T, local bool) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "hash-project")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	eventStore, eventProject := hub.DB, projects[0].ID
	if local {
		eventStore, eventProject = spoke.env.DB, spoke.projects[0].ID
	}
	_, event, err := eventStore.CreateIssue(ctx, db.CreateIssueParams{ProjectID: eventProject, Title: "Pull event", Author: "worker"})
	require.NoError(t, err)
	require.NotEmpty(t, event.ContentHash)
	target, err := url.Parse(hub.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(request *httputil.ProxyRequest) {
		request.SetURL(target)
		request.Out.Header.Set("Accept-Encoding", "identity")
	}}
	var corrupt atomic.Bool
	corrupt.Store(true)
	proxy.ModifyResponse = func(response *http.Response) error {
		if !corrupt.Load() {
			return nil
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		if err := response.Body.Close(); err != nil {
			return err
		}
		body = bytes.ReplaceAll(body, []byte(event.ContentHash), []byte(strings.Repeat("0", len(event.ContentHash))))
		response.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	credential := spoke.credentials[projects[0].UID]
	credential.HubURL = server.URL
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	sync := func(binding db.FederationBinding) error {
		if local {
			return SyncFederationOnce(ctx, spoke.env.DB, binding, credential, spoke.credentials)
		}
		return SyncFederationPullOnlyOnce(ctx, spoke.env.DB, binding, credential, spoke.credentials)
	}
	err = sync(binding)
	operation := "validate remote event"
	if local {
		operation = "reconcile local echo"
	}
	require.ErrorContains(t, err, operation)
	status, err := spoke.env.DB.FederationSyncStatusByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, status.LastError)
	require.Contains(t, *status.LastError, operation)
	require.Contains(t, *status.LastError, projects[0].UID)
	require.Contains(t, *status.LastError, event.UID)
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	if !local {
		require.Less(t, binding.PullCursorEventID, event.ID)
	}
	corrupt.Store(false)
	require.NoError(t, sync(binding))
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	if !local {
		require.GreaterOrEqual(t, binding.PullCursorEventID, event.ID)
	}
}
