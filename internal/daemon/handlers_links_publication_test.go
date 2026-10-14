package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

var errResponseIssueLookup = errors.New("response issue lookup failed")

type postCommitIssueLookupFailureStore struct {
	db.Storage
	failNext bool
}

func (s *postCommitIssueLookupFailureStore) CreateLinkAndEvent(
	ctx context.Context, link db.CreateLinkParams, event db.LinkEventParams,
) (db.Link, db.Event, error) {
	created, evt, err := s.Storage.CreateLinkAndEvent(ctx, link, event)
	if err == nil {
		s.failNext = true
	}
	return created, evt, err
}

func (s *postCommitIssueLookupFailureStore) DeleteLinkAndEvent(
	ctx context.Context, link db.Link, event db.LinkEventParams,
) (db.Event, error) {
	evt, err := s.Storage.DeleteLinkAndEvent(ctx, link, event)
	if err == nil {
		s.failNext = true
	}
	return evt, err
}

func (s *postCommitIssueLookupFailureStore) IssueByID(ctx context.Context, id int64) (db.Issue, error) {
	if s.failNext {
		s.failNext = false
		return db.Issue{}, errResponseIssueLookup
	}
	return s.Storage.IssueByID(ctx, id)
}

func TestLinkMutationsPublishCommittedEventsBeforeResponseIssueLookup(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, mutation := range []string{"create", "delete"} {
			t.Run(mutation, func(t *testing.T) {
				f := newProjectAccessFixture(t, store, mutation)
				ctx := t.Context()
				peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: f.public.ID, Title: "Link peer", Author: "member",
				})
				require.NoError(t, err)
				from := f.visible
				if mutation == "delete" {
					_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
						FromIssueID: from.ID, ToIssueID: peer.ID, Type: "blocks", Author: "member",
					}, db.LinkEventParams{
						EventType: "issue.linked", EventIssueID: from.ID,
						FromShortID: from.ShortID, FromUID: from.UID,
						ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "member",
					})
					require.NoError(t, err)
				}
				before, err := store.MaxEventID(ctx)
				require.NoError(t, err)

				broadcaster := daemon.NewEventBroadcaster()
				sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: f.public.ID})
				defer sub.Unsub()
				hooks := &publisherSink{}
				wrapped := &postCommitIssueLookupFailureStore{Storage: store}
				server := daemon.NewServer(daemon.ServerConfig{
					DB: wrapped, Broadcaster: broadcaster, Hooks: hooks,
					Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true},
				})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				httpServer := httptest.NewServer(server.Handler())
				t.Cleanup(httpServer.Close)
				f.server = httpServer

				path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/links", f.public.ID, from.ShortID)
				method := http.MethodPost
				var body any = map[string]any{"actor": "member", "type": "blocks", "to_ref": peer.ShortID}
				if mutation == "delete" {
					link, lookupErr := store.LinkByEndpoints(ctx, from.ID, peer.ID, "blocks")
					require.NoError(t, lookupErr)
					path += "/" + fmt.Sprint(link.ID) + "?actor=member"
					method = http.MethodDelete
					body = nil
				}
				status, _, _ := f.request(t, method, path, "member", body, nil)
				require.Equal(t, http.StatusInternalServerError, status,
					"the injected response-only issue lookup should fail after commit")

				events, err := store.EventsAfter(ctx, db.EventsAfterParams{
					ProjectID: f.public.ID, AfterID: before, Limit: 20,
				})
				require.NoError(t, err)
				wantType := "issue.linked"
				if mutation == "delete" {
					wantType = "issue.unlinked"
				}
				var committed db.Event
				for _, event := range events {
					if event.Type == wantType {
						committed = event
						break
					}
				}
				require.NotZero(t, committed.ID, "the link mutation commits its matching event")
				assert.Contains(t, hooks.ids(), committed.ID,
					"the committed event must reach the mutation hook before response hydration")
				msg := receiveMsg(t, sub.Ch, time.Second, "committed link mutation event")
				require.Equal(t, daemon.StreamKindEvent, msg.Kind)
				require.NotNil(t, msg.Event)
				assert.Equal(t, committed.ID, msg.Event.ID)
			})
		}
	})
}
