package daemon

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/tracker"
	"testing"
	"time"
)

type workerNotionFixture struct {
	contentPage notionsync.Page
	calls       int
}

func (s *workerNotionFixture) ForRun(context.Context) (notionsync.Session, error) { return s, nil }
func (s *workerNotionFixture) DataSource(context.Context, string) (notionsync.DataSource, error) {
	return notionsync.DataSource{ID: "source", DatabaseID: "parent"}, nil
}
func (s *workerNotionFixture) Database(context.Context, string) (notionsync.Database, error) {
	return notionsync.Database{ID: "parent"}, nil
}
func (s *workerNotionFixture) Pages(context.Context, notionsync.Config, *time.Time) ([]notionsync.Page, error) {
	return []notionsync.Page{{ID: "known", DataSourceID: "source", DatabaseID: "parent"}}, nil
}
func (s *workerNotionFixture) Content(_ context.Context, _ notionsync.Config, p notionsync.Page) (notionsync.PageContent, error) {
	s.calls++
	s.contentPage = p
	return notionsync.PageContent{Page: p, Title: "Example"}, nil
}

func TestWorkerNotionContentUsesOwnerVerifiedMembership(t *testing.T) {
	session := &trackerOwnerRun{}
	provider := &workerNotionFixture{}
	connection := trackerLocalConnection{clients: trackerClients{notion: provider}}
	ticket := tracker.WorkerTicket{Provider: "notion", Method: "notion.content", Resource: "notion:source"}
	cfg := notionsync.Config{DataSourceID: "source", DatabaseID: "parent"}
	args := trackerWorkerArguments{Notion: &cfg, ID: "known"}
	_, err := session.providerCall(t.Context(), connection, ticket, args, nil)
	require.Error(t, err, "root-echoed page identity alone proves no source membership")
	require.Zero(t, provider.calls)
	ticket.Method = "notion.pages"
	_, err = session.providerCall(t.Context(), connection, ticket, args, nil)
	require.NoError(t, err)
	ticket.Method = "notion.content"
	_, err = session.providerCall(t.Context(), connection, ticket, args, nil)
	require.NoError(t, err)
	require.Equal(t, "parent", provider.contentPage.DatabaseID)
	wrong := cfg
	wrong.DatabaseID = "other"
	args.Notion = &wrong
	_, err = session.providerCall(t.Context(), connection, ticket, args, nil)
	require.Error(t, err)
	require.Equal(t, 1, provider.calls)
	args.Notion = &cfg
	args.ID = "not-listed"
	_, err = session.providerCall(t.Context(), connection, ticket, args, nil)
	require.Error(t, err)
	require.Equal(t, 1, provider.calls)
}

func TestWorkerDispatchRejectsForeignProviderConfigBeforeSession(t *testing.T) {
	session := &trackerOwnerRun{}
	_, err := session.providerCall(t.Context(), trackerLocalConnection{}, tracker.WorkerTicket{Provider: "notion", Method: "notion.pages", Resource: "notion:source"}, trackerWorkerArguments{Linear: &linearsync.Config{}}, nil)
	require.ErrorIs(t, err, tracker.ErrInvalid)
}

func TestWorkerStatusRequiresServiceAndBoundNativeSource(t *testing.T) {
	runtime := &TrackerRuntime{}
	ticket := tracker.WorkerTicket{Provider: "notion", Principal: tracker.Principal{Kind: tracker.SubjectAccount, Subject: "member"}}
	_, err := runtime.executeOwnerStatus(t.Context(), ServerConfig{}, nil, nil, trackerLocalConnection{}, ticket, trackerWorkerArguments{ID: "item", Target: "closed"})
	require.ErrorIs(t, err, tracker.ErrDenied, "no provider or database work may precede this authority check")
	ticket.Principal.Kind = tracker.SubjectService
	_, err = runtime.executeOwnerStatus(t.Context(), ServerConfig{}, nil, nil, trackerLocalConnection{}, ticket, trackerWorkerArguments{ID: "item", Target: "closed"})
	require.ErrorIs(t, err, tracker.ErrDenied, "service labels without the original native source do not authorize dispatch")
}
