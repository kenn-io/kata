package daemon_test

import (
	"context"
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
)

type revokeFederationProjectAccessOnRead struct {
	db.Storage
	beforeRead func() error
	afterRead  func() error
}

// ReadFederation is the poll's single consistent read, so membership revoked
// here is revoked after admission and before the response is released.
func (s *revokeFederationProjectAccessOnRead) ReadFederation(
	ctx context.Context,
	params db.FederationReadParams,
) (db.FederationReadResult, error) {
	if s.beforeRead != nil {
		beforeRead := s.beforeRead
		s.beforeRead = nil
		if err := beforeRead(); err != nil {
			return db.FederationReadResult{}, err
		}
	}
	result, err := s.Storage.ReadFederation(ctx, params)
	if err == nil && s.afterRead != nil {
		afterRead := s.afterRead
		s.afterRead = nil
		if err := afterRead(); err != nil {
			return db.FederationReadResult{}, err
		}
	}
	return result, err
}

func TestFederationPollDiscardsEventsReadAfterProjectMembershipRevocation(t *testing.T) {
	for name, afterRead := range map[string]bool{"revoked during the read": false, "revoked after the read committed": true} {
		t.Run(name, func(t *testing.T) { runFederationPollRevocation(t, afterRead) })
	}
}

// runFederationPollRevocation revokes the enrollment actor's membership either
// inside the poll's read or after it committed; both must withhold the
// restricted project's events, since the poll is a read and not a mutation.
func runFederationPollRevocation(t *testing.T, afterRead bool) {
	t.Setenv("TEST_FEDERATION_PROJECT_ACCESS_KEY", strings.Repeat("k", 64))
	source := signingpolicy.Source{KeyID: "key-a", KeyEnv: "TEST_FEDERATION_PROJECT_ACCESS_KEY"}
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "spoke-project")
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, project.ID, "admin")
		require.NoError(t, err)
		team, _, err := store.CreateTeam(ctx, "project-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(ctx, team.UID, "example-actor", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{
			ProjectUID: project.UID,
			Visibility: "teams",
			TeamUIDs:   []string{team.UID},
		}, "admin")
		require.NoError(t, err)
		enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token:            "project-events-enrollment",
			SpokeInstanceUID: federationTestSpokeUID,
			ProjectID:        &project.ID,
			Capabilities:     "pull",
			Actor:            "example-actor",
		})
		require.NoError(t, err)

		const canary = "post-revocation-canary"
		var revoked bool
		raceStore := &revokeFederationProjectAccessOnRead{Storage: store}
		revoke := func() error {
			_, err := store.SetTeamMembership(context.Background(), team.UID, "example-actor", false, "admin")
			if err != nil {
				return err
			}
			_, _, err = store.CreateIssue(context.Background(), db.CreateIssueParams{
				ProjectID: project.ID,
				Title:     canary,
				Author:    "admin",
			})
			if err != nil {
				return err
			}
			revoked = true
			return nil
		}
		if afterRead {
			raceStore.afterRead = revoke
		} else {
			raceStore.beforeRead = revoke
		}

		synctest.Test(t, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "replay.state")
			require.NoError(t, federationsigning.InitializeReplayState(state))
			verifier, err := federationsigning.NewVerifier("https://hub.example", []signingpolicy.Key{
				{Source: source, EnrollmentID: enrollment.Enrollment.ID},
			}, state)
			require.NoError(t, err)
			defer func() { require.NoError(t, verifier.Close()) }()
			time.Sleep(federationsigning.Quarantine)

			server := daemon.NewServer(daemon.ServerConfig{DB: raceStore, FederationSigning: verifier})
			defer func() { require.NoError(t, server.Close()) }()
			ingress, err := server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerFederation})
			require.NoError(t, err)

			request := httptest.NewRequest(http.MethodGet,
				"https://hub.example"+projectPath(project.ID)+"/federation/events?after_id=0&limit=10", nil)
			request.Header.Set("Authorization", "Bearer "+enrollment.Token)
			require.NoError(t, federationsigning.Sign(request, source))
			response := httptest.NewRecorder()
			ingress.ServeHTTP(response, request)

			require.True(t, revoked, "the test must revoke membership and insert an event during the poll read")
			require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), canary)
		})
	})
}
