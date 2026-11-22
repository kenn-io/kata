package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/testfix"
)

const signedPollKeyEnv = "TEST_FEDERATION_LONG_POLL_KEY"

// signedPollFixture is a hub store with one federated project that only the
// enrollment actor's team may read.
type signedPollFixture struct {
	store      db.Storage
	project    db.Project
	team       db.Team
	enrollment db.CreatedFederationEnrollment
	source     signingpolicy.Source
}

func newSignedPollFixture(t *testing.T) signedPollFixture {
	t.Helper()
	ctx := t.Context()
	t.Setenv(signedPollKeyEnv, strings.Repeat("k", 64))
	store, err := testfix.OpenSQLite(ctx, filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "admin")
	require.NoError(t, err)
	team, _, err := store.CreateTeam(ctx, "project-team", "admin")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "example-actor", true, "admin")
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{
		ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID},
	}, "admin")
	require.NoError(t, err)
	enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: "signed-long-poll-token", SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: "pull", Actor: "example-actor",
	})
	require.NoError(t, err)
	return signedPollFixture{
		store: store, project: project, team: team, enrollment: enrollment,
		source: signingpolicy.Source{KeyID: "key-a", KeyEnv: signedPollKeyEnv},
	}
}

// signedPollHub serves the fixture on the federation ingress listener, where
// every poll is signed and runs under the 60 s federation request deadline.
type signedPollHub struct {
	signedPollFixture
	reads       *federationReadProbe
	broadcaster *daemon.EventBroadcaster
	ingress     http.Handler
}

// serve starts the hub inside a synctest bubble. A positive keyLife retires
// the signing key that long after the startup quarantine ends.
func (f signedPollFixture) serve(t *testing.T, keyLife time.Duration) *signedPollHub {
	t.Helper()
	state := filepath.Join(t.TempDir(), "replay.state")
	require.NoError(t, federationsigning.InitializeReplayState(state))
	key := signingpolicy.Key{Source: f.source, EnrollmentID: f.enrollment.Enrollment.ID}
	if keyLife > 0 {
		key.NotAfter = time.Now().Add(federationsigning.Quarantine + keyLife).Unix()
	}
	verifier, err := federationsigning.NewVerifier("https://hub.example", []signingpolicy.Key{key}, state)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, verifier.Close()) })
	//nolint:kennlint // sleeptest: serve runs only inside a synctest bubble, where this sleep advances the fake clock past quarantine.
	time.Sleep(federationsigning.Quarantine)
	reads := newFederationReadProbe(f.store)
	broadcaster := daemon.NewEventBroadcaster()
	server := daemon.NewServer(daemon.ServerConfig{DB: reads.storage(), FederationSigning: verifier, Broadcaster: broadcaster})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	ingress, err := server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerFederation})
	require.NoError(t, err)
	return &signedPollHub{signedPollFixture: f, reads: reads, broadcaster: broadcaster, ingress: ingress}
}

func (h *signedPollHub) request(t *testing.T, afterID int64, wait string) *http.Request {
	t.Helper()
	url := fmt.Sprintf("https://hub.example%s/federation/events?after_id=%d", projectPath(h.project.ID), afterID)
	if wait != "" {
		url += "&wait=" + wait
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	request.Header.Set("Authorization", "Bearer "+h.enrollment.Token)
	require.NoError(t, federationsigning.Sign(request, h.source))
	return request
}

func (h *signedPollHub) send(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	h.ingress.ServeHTTP(response, request)
	return response
}

// cursor polls once without waiting and returns the page's next cursor.
func (h *signedPollHub) cursor(t *testing.T) int64 {
	t.Helper()
	response := h.send(h.request(t, 0, ""))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var page pollBodyForTest
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	return page.NextAfterID
}

// startHeldPoll sends a held poll and returns once the hub has parked it.
func (h *signedPollHub) startHeldPoll(t *testing.T, afterID int64, wait string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	request := h.request(t, afterID, wait)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.send(request) }()
	synctest.Wait()
	select {
	case response := <-done:
		t.Fatalf("poll answered instead of being held: %d %s", response.Code, response.Body.String())
	default:
	}
	return done
}

// commit creates an issue in the project. wake also publishes it to held
// polls; without it the commit reaches them only through a read.
func (h *signedPollHub) commit(t *testing.T, title string, wake bool) {
	t.Helper()
	_, event, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: h.project.ID, Title: title, Author: "admin"})
	require.NoError(t, err)
	if wake {
		h.broadcaster.Broadcast(daemon.StreamMsg{Kind: daemon.StreamKindEvent, Event: &event, ProjectID: h.project.ID})
	}
}

func (h *signedPollHub) revokeEnrollment(t *testing.T) error {
	return h.store.RevokeFederationEnrollment(t.Context(), h.enrollment.Enrollment.ID)
}

func (h *signedPollHub) revokeProjectAccess(t *testing.T) error {
	_, err := h.store.SetTeamMembership(t.Context(), h.team.UID, "example-actor", false, "admin")
	return err
}

func TestFederationPollRechecksAuthorizationAfterItsRead(t *testing.T) {
	const canary = "post-revocation-canary"
	revocations := []struct {
		name   string
		revoke func(*signedPollHub, *testing.T) error
		status int
	}{
		{"enrollment revoked", (*signedPollHub).revokeEnrollment, http.StatusForbidden},
		// Project access hides the project itself, as every other route does.
		{"project access revoked", (*signedPollHub).revokeProjectAccess, http.StatusNotFound},
	}
	for _, revocation := range revocations {
		t.Run(revocation.name+" before an immediate poll reads", func(t *testing.T) {
			f := newSignedPollFixture(t)
			synctest.Test(t, func(t *testing.T) {
				h := f.serve(t, 0)
				cursor := h.cursor(t)
				h.reads.beforeRead(func(int) error {
					if err := revocation.revoke(h, t); err != nil {
						return err
					}
					h.commit(t, canary, false)
					return nil
				})
				response := h.send(h.request(t, cursor, "0"))
				assert.Equal(t, revocation.status, response.Code, response.Body.String())
				assert.NotContains(t, response.Body.String(), canary)
			})
		})
	}
}

func TestFederationPollWaitReturnsSavedPageWhenItExpires(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke bool
	}{{"commit without a wake", false}, {"project access revocation", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignedPollFixture(t)
			synctest.Test(t, func(t *testing.T) {
				h := f.serve(t, 0)
				cursor := h.cursor(t)
				reads := h.reads.readsReturned()
				done := h.startHeldPoll(t, cursor, "30")
				authCalls, revisionCalls := h.reads.accessCalls()
				if tc.revoke {
					require.NoError(t, h.revokeProjectAccess(t))
				} else {
					h.commit(t, "Committed without a wakeup", false)
				}
				response := <-done
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var page pollBodyForTest
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
				assert.Empty(t, page.Events)
				assert.Equal(t, cursor, page.NextAfterID)
				assert.Equal(t, reads+1, h.reads.readsReturned())
				authAfter, revisionAfter := h.reads.accessCalls()
				assert.Equal(t, authCalls, authAfter)
				assert.Equal(t, revisionCalls, revisionAfter)
				response = h.send(h.request(t, cursor, ""))
				if tc.revoke {
					assert.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
				} else {
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					assert.Contains(t, response.Body.String(), "Committed without a wakeup")
				}
			})
		})
	}
}

func TestFederationPollWaitReturnsEmptyWhenReadCrossesDeadline(t *testing.T) {
	for _, wake := range []bool{false, true} {
		name := "initial read"
		if wake {
			name = "read after wake"
		}
		t.Run(name, func(t *testing.T) {
			f := newSignedPollFixture(t)
			synctest.Test(t, func(t *testing.T) {
				h := f.serve(t, 0)
				cursor := h.cursor(t)
				reads := h.reads.readsReturned()
				var done <-chan *httptest.ResponseRecorder
				if wake {
					done = h.startHeldPoll(t, cursor, "30")
				}
				h.reads.beforeRead(func(int) error {
					time.Sleep(31 * time.Second)
					return h.revokeProjectAccess(t)
				})
				var response *httptest.ResponseRecorder
				if wake {
					h.broadcaster.Broadcast(daemon.NewResetMsg(h.project.ID, 1))
					response = <-done
				} else {
					response = h.send(h.request(t, cursor, "30"))
				}
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var page pollBodyForTest
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
				assert.Empty(t, page.Events)
				wantReads := reads + 1
				if wake {
					wantReads++
				}
				assert.Equal(t, wantReads, h.reads.readsReturned())
				response = h.send(h.request(t, page.NextAfterID, ""))
				assert.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
				assert.Equal(t, wantReads, h.reads.readsReturned())
			})
		})
	}
}

func TestFederationPollWaitEndsBeforeTheRequestDeadlineAfterASlowRead(t *testing.T) {
	f := newSignedPollFixture(t)
	synctest.Test(t, func(t *testing.T) {
		h := f.serve(t, 0)
		cursor := h.cursor(t)
		slowRead := h.reads.readsReturned() + 1
		h.reads.beforeRead(func(read int) error {
			if read == slowRead {
				time.Sleep(20 * time.Second)
			}
			return nil
		})
		started := time.Now()
		response := h.send(h.request(t, cursor, "45"))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		// The 60 s federation request deadline must still be ahead: the
		// hold's time counts from the request, not from its first read.
		assert.Less(t, time.Since(started), 60*time.Second)
	})
}

func TestFederationPollWaitRejectsARetiredSigningKey(t *testing.T) {
	const canary = "committed-after-the-key-retired"
	t.Run("key expires while an immediate poll reads", func(t *testing.T) {
		f := newSignedPollFixture(t)
		synctest.Test(t, func(t *testing.T) {
			h := f.serve(t, 20*time.Second)
			cursor := h.cursor(t)
			h.commit(t, canary, false)
			h.reads.beforeRead(func(int) error { time.Sleep(25 * time.Second); return nil })
			response := h.send(h.request(t, cursor, "0"))
			assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), canary)
		})
	})
	for _, tc := range []struct {
		name string
		wake bool
	}{{"woken after the key expires", true}, {"retirement applies to the next poll", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignedPollFixture(t)
			synctest.Test(t, func(t *testing.T) {
				h := f.serve(t, 20*time.Second)
				cursor := h.cursor(t)
				done := h.startHeldPoll(t, cursor, "30")
				reads := h.reads.readsReturned()
				time.Sleep(25 * time.Second)
				h.commit(t, canary, tc.wake)
				response := <-done
				if !tc.wake {
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					var page pollBodyForTest
					require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
					assert.Empty(t, page.Events)
					assert.Equal(t, reads, h.reads.readsReturned())
					response = h.send(h.request(t, page.NextAfterID, ""))
				}
				assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
				assert.NotContains(t, response.Body.String(), canary)
				assert.Equal(t, reads, h.reads.readsReturned())
			})
		})
	}
}
