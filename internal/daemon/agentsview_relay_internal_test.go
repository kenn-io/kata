package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type missingAgentsViewEventStore struct{ db.Storage }

type failedAgentsViewBindingStore struct {
	db.Storage
	err error
}

func (failedAgentsViewBindingStore) InstanceUID() string { return "instance" }

func (s failedAgentsViewBindingStore) FederationBindingByProject(context.Context, int64) (db.FederationBinding, error) {
	return db.FederationBinding{}, s.err
}

func TestAgentsViewBindingFailureIsNotReportedAsRouteChange(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{db.ErrNotFound, "agentsview_route_changed"},
		{errors.New("store unavailable"), "internal"},
	} {
		err := requireAgentsViewClaimWhileCoordinated(t.Context(), ServerConfig{DB: failedAgentsViewBindingStore{err: tc.err}}, db.Issue{ProjectID: 1}, "worker", nil, "cli", nil)
		var apiErr *api.APIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, tc.code, apiErr.Code)
	}
}

func (missingAgentsViewEventStore) EventsByUIDs(context.Context, int64, []string) ([]db.Event, error) {
	return nil, db.ErrNotFound
}

func TestAgentsViewMissingOriginEventIsPendingForOpenAndClosedIssue(t *testing.T) {
	for _, status := range []string{"open", "closed"} {
		issue := db.Issue{UID: "issue", ProjectID: 1, Status: status}
		_, err := loadAgentsViewClose(t.Context(), ServerConfig{DB: missingAgentsViewEventStore{}}, issue, agentsviewrelay.Control{IssueUID: issue.UID})
		var apiErr *api.APIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, "close_not_at_origin", apiErr.Code)
	}
}

func TestAgentsViewFederationUsesEnrollmentActor(t *testing.T) {
	actor, err := agentsViewUploadActor(t.Context(), true, "", federationPrincipal{Actor: "enrolled-worker"})
	require.NoError(t, err)
	require.Equal(t, "enrolled-worker", actor)
}

func TestAgentsViewSignerReadinessRejectsMissingAndWrongKeys(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	pin := db.RootKeyPin{AuthorityUID: "root", KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey))}
	for _, signer := range []db.RootAttributionSigner{{AuthorityUID: "root"}, {AuthorityUID: "root", PrivateKey: other}, {AuthorityUID: "other", PrivateKey: key}} {
		require.False(t, agentsViewSignerMatches(signer, pin))
	}
	require.True(t, agentsViewSignerMatches(db.RootAttributionSigner{AuthorityUID: "root", PrivateKey: key}, pin))
}

func TestAgentsViewRootAdmissionDistinguishesPinFromSigner(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	pin := db.RootKeyPin{AuthorityUID: "root", KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey))}
	association := agentsviewrelay.Association{AuthorityUID: pin.AuthorityUID, RootKeyID: pin.KeyID}
	signer := db.RootAttributionSigner{AuthorityUID: "root", PrivateKey: key}
	retired := pin
	retired.Retired = true
	for _, tc := range []struct {
		pin    db.RootKeyPin
		signer db.RootAttributionSigner
		code   string
	}{
		{retired, signer, "agentsview_route_changed"},
		{db.RootKeyPin{AuthorityUID: "other", KeyID: pin.KeyID}, signer, "agentsview_route_changed"},
		{pin, db.RootAttributionSigner{AuthorityUID: "root"}, "agentsview_unavailable"},
	} {
		var apiErr *api.APIError
		require.ErrorAs(t, agentsViewRootAdmission(association, tc.pin, tc.signer), &apiErr)
		require.Equal(t, tc.code, apiErr.Code)
	}
	require.NoError(t, agentsViewRootAdmission(association, pin, signer))
}

func TestAgentsViewBinaryUploadContentTypeScope(t *testing.T) {
	for _, tc := range []struct {
		path, method, contentType string
		want                      bool
	}{
		{"/api/v1/projects/1/issues/example/transcript-share", "POST", "application/octet-stream", true},
		{"/api/v1/projects/1/federation/transcript-service/issues/example", "POST", "application/octet-stream", true},
		{"/api/v1/projects/1/issues/example/transcript-share/extra", "POST", "application/octet-stream", false},
		{"/api/v1/projects/1/issues/example/transcript-share", "PUT", "application/octet-stream", false},
		{"/api/v1/projects/1/issues/example/transcript-share", "POST", "text/plain", false},
	} {
		req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
		req.Header.Set("Content-Type", tc.contentType)
		require.Equal(t, tc.want, isAgentsViewBinaryUpload(req), tc.path)
	}
}

func TestAgentsViewHistoryUsesEventClockInsteadOfInsertionOrder(t *testing.T) {
	closeEvent := db.Event{ID: 1, UID: "close", OriginInstanceUID: "node", HLCPhysicalMS: 100}
	older := db.Event{ID: 2, UID: "older", OriginInstanceUID: "node", HLCPhysicalMS: 90}
	newer := db.Event{ID: 0, UID: "newer", OriginInstanceUID: "node", HLCPhysicalMS: 110}
	require.False(t, agentsViewEventAfter(older, closeEvent))
	require.True(t, agentsViewEventAfter(newer, closeEvent))
}

type agentsViewHistoryStore struct {
	db.Storage
	events []db.Event
}

func (s agentsViewHistoryStore) EventsByUIDs(_ context.Context, _ int64, uids []string) ([]db.Event, error) {
	for _, e := range s.events {
		if e.UID == uids[0] {
			return []db.Event{e}, nil
		}
	}
	return nil, db.ErrNotFound
}
func (s agentsViewHistoryStore) EventsAfter(_ context.Context, p db.EventsAfterParams) ([]db.Event, error) {
	var out []db.Event
	for _, e := range s.events {
		if e.ID <= p.AfterID {
			continue
		}
		if len(p.Types) > 0 && !slices.Contains(p.Types, e.Type) {
			continue
		}
		out = append(out, e)
		if len(out) == p.Limit {
			break
		}
	}
	return out, nil
}

func TestAgentsViewDelayedOlderTransitionDoesNotRevokeClose(t *testing.T) {
	issueUID, projectUID, root := "01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	a := agentsviewrelay.Association{Version: 1, IssueUID: issueUID, ProjectUID: projectUID, AuthorityUID: root, ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 12, LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: root, ProjectUID: projectUID}}}
	payload, err := json.Marshal(map[string]any{"agentsview_association": a})
	require.NoError(t, err)
	issue := db.Issue{ID: 1, UID: issueUID, ProjectID: 1, Status: "closed"}
	closeUID := "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	events := []db.Event{
		{ID: 1, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAP", Type: "issue.created", IssueUID: &issueUID, ProjectUID: projectUID, OriginInstanceUID: root, HLCPhysicalMS: 1, Payload: `{"title":"example"}`},
		{ID: 2, UID: closeUID, Type: "issue.closed", IssueUID: &issueUID, ProjectUID: projectUID, OriginInstanceUID: root, HLCPhysicalMS: 3, Payload: string(payload)},
		{ID: 3, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAQ", Type: "issue.reopened", IssueUID: &issueUID, ProjectUID: projectUID, OriginInstanceUID: root, HLCPhysicalMS: 2, Payload: `{}`},
	}
	c := agentsviewrelay.Control{Version: 1, IssueUID: issueUID, ProjectUID: projectUID, AuthorityUID: root, CloseEventUID: closeUID, ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route}
	store := agentsViewHistoryStore{events: events}
	_, err = loadAgentsViewClose(t.Context(), ServerConfig{DB: store}, issue, c)
	require.NoError(t, err)
	store.events = append(store.events, db.Event{ID: 4, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAR", Type: "issue.moved", IssueUID: &issueUID, ProjectUID: projectUID, OriginInstanceUID: root, HLCPhysicalMS: 4, Payload: `{}`})
	_, err = loadAgentsViewClose(t.Context(), ServerConfig{DB: store}, issue, c)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "agentsview_close_changed", apiErr.Code)
}
