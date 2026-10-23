package daemon_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestOwnerFederationEnrollment(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.EnableProjectFederation(ctx, f.public.ID, "member")
		require.NoError(t, err)
		projectID := &f.public.ID
		request := map[string]any{"spoke_instance_uid": federationTestSpokeUID, "project_id": projectID, "capabilities": "pull,push", "actor": " member "}
		before, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		status, _, raw := f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "admin", request, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		assert.Equal(t, before, after, "direct enrollment is credential administration without an attributed event")
		var created api.FederationEnrollmentOut
		require.NoError(t, json.Unmarshal(raw, &created))
		assert.Equal(t, "member", created.Actor)
		assert.Equal(t, projectID, created.ProjectID)
		require.NotEmpty(t, created.Token)
		grant, err := store.AuthorizeFederationToken(ctx, created.Token, f.public.ID, "pull")
		require.NoError(t, err)
		assert.Equal(t, "member", grant.Actor)
	})
}

func TestOwnerFederationEnrollmentRotation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.EnableProjectFederation(ctx, f.public.ID, "member")
		require.NoError(t, err)
		old, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: federationTestSpokeUID, ProjectID: &f.public.ID, Actor: "member", Capabilities: "pull"})
		require.NoError(t, err)
		request := map[string]any{"spoke_instance_uid": federationTestSpokeUID, "project_id": f.public.ID, "capabilities": "pull", "actor": "member", "token": "owner-replacement-fixture"}
		status, _, raw := f.request(t, http.MethodPost, "/api/v1/federation/enrollments/actions/rotate", "admin", request, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var rotated api.FederationEnrollmentOut
		require.NoError(t, json.Unmarshal(raw, &rotated))
		assert.Equal(t, "member", rotated.Actor)
		assert.Equal(t, "owner-replacement-fixture", rotated.Token)
		grant, err := store.AuthorizeFederationToken(ctx, rotated.Token, f.public.ID, "pull")
		require.NoError(t, err)
		assert.Equal(t, rotated.ID, grant.ID)
		_, err = store.AuthorizeFederationToken(ctx, old.Token, f.public.ID, "pull")
		require.ErrorIs(t, err, db.ErrNotFound)
	})
}

func TestOwnerFederationEnrollmentRequiresExistingAccount(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.EnableProjectFederation(ctx, f.public.ID, "member")
		require.NoError(t, err)
		for _, state := range []string{"revoked", "expired", "active", "scoped", "", "   ", "bootstrap", "BOOTSTRAP", "unknown-account"} {
			t.Run(state, func(t *testing.T) {
				valid := state == "revoked" || state == "expired" || state == "active"
				if valid || state == "scoped" {
					params := db.CreateAPITokenParams{PlaintextToken: state + "-account-fixture", Actor: state, AdminActor: "bootstrap"}
					if state == "expired" {
						params.ExpiresAt = new(time.Now().Add(-time.Hour))
					}
					if state == "active" || state == "scoped" {
						params.ExpiresAt = new(time.Now().Add(time.Hour))
					}
					if state == "scoped" {
						params.Scope = &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: f.visible.UID}
					}
					token, _, err := store.CreateAPIToken(ctx, params)
					require.NoError(t, err)
					if state == "revoked" {
						_, _, err = store.RevokeAPIToken(ctx, token.ID, "bootstrap")
						require.NoError(t, err)
					}
				}
				old, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: federationTestSpokeUID, ProjectID: &f.public.ID, Actor: "member", Capabilities: "pull"})
				require.NoError(t, err)
				before, err := store.ListFederationEnrollments(ctx)
				require.NoError(t, err)
				for _, path := range []string{"/api/v1/federation/enrollments", "/api/v1/federation/enrollments/actions/rotate"} {
					request := map[string]any{"spoke_instance_uid": federationTestSpokeUID, "project_id": f.public.ID, "capabilities": "pull", "actor": state, "token": state + "-enrollment-fixture"}
					status, _, raw := f.request(t, http.MethodPost, path, "admin", request, nil)
					if !valid {
						assertAPIError(t, status, raw, http.StatusBadRequest, "validation")
						var failure api.ErrorEnvelope
						require.NoError(t, json.Unmarshal(raw, &failure))
						assert.Equal(t, "actor must name an existing hub account", failure.Error.Message)
						_, err := store.AuthorizeFederationToken(ctx, old.Token, f.public.ID, "pull")
						require.NoError(t, err)
						after, err := store.ListFederationEnrollments(ctx)
						require.NoError(t, err)
						assert.Equal(t, before, after, "rejected enrollment preserves grants")
					} else {
						require.Equal(t, http.StatusOK, status, string(raw))
					}
				}
			})
		}
		t.Run("wildcard unknown account", func(t *testing.T) {
			status, _, raw := f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "admin", map[string]any{
				"spoke_instance_uid": federationTestSpokeUID, "project_id": nil, "capabilities": "pull", "actor": "unknown-account",
			}, nil)
			assertAPIError(t, status, raw, http.StatusBadRequest, "validation")
			assert.Contains(t, string(raw), "actor must name an existing hub account")
		})
	})
}

func TestOwnerFederationEnrollmentRequiresActorProjectAccess(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.EnableProjectFederation(ctx, f.private.ID, "member")
		require.NoError(t, err)
		old, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: federationTestSpokeUID, ProjectID: &f.private.ID, Actor: "member", Capabilities: "pull"})
		require.NoError(t, err)
		before, err := store.ListFederationEnrollments(ctx)
		require.NoError(t, err)
		for _, path := range []string{"/api/v1/federation/enrollments", "/api/v1/federation/enrollments/actions/rotate"} {
			t.Run(path, func(t *testing.T) {
				request := map[string]any{"spoke_instance_uid": federationTestSpokeUID, "project_id": f.private.ID, "capabilities": "pull", "actor": "nonmember", "token": "nonmember-enrollment-fixture"}
				status, _, raw := f.request(t, http.MethodPost, path, "admin", request, nil)
				assertAPIError(t, status, raw, http.StatusBadRequest, "validation")
				assert.Contains(t, string(raw), "actor cannot access this project")
				_, err := store.AuthorizeFederationToken(ctx, old.Token, f.private.ID, "pull")
				require.NoError(t, err, "a rejected rotation keeps the spoke's working grant")
				after, err := store.ListFederationEnrollments(ctx)
				require.NoError(t, err)
				assert.Equal(t, before, after)
			})
		}
	})
}
