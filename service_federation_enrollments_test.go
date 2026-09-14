package kata_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/testenv"
)

func TestServiceEnsureFederationEnrollment(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			cfg := kata.Config{
				DSN:     filepath.Join(t.TempDir(), "service.db"),
				Auth:    kata.AuthConfig{TrustCallerAuthentication: true},
				Profile: kata.EmbeddingProfileRestricted,
			}
			if backend == "postgres" {
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				cfg.DSN = dsn
				cfg.Postgres = kata.PostgresConfig{Schema: "kata", SchemaMode: kata.PostgresSchemaBootstrap}
			}
			service, err := kata.New(ctx, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			project := ensureEnrollmentTestProject(t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "hub-project")
			other := ensureEnrollmentTestProject(t, service, "01HZNQ7VFPK1XGD8R5MABCD4EY", "other-project")
			spec := kata.FederationEnrollmentSpec{
				ProjectUID:       project.UID,
				SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Capabilities:     "push,pull,claim",
				Actor:            "Example Operator",
			}
			token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			first, err := service.EnsureFederationEnrollment(ctx, spec, token)
			require.NoError(t, err)
			assert.Equal(t, "claim,pull,push", first.Capabilities)
			assertFederationTokenStatus(t, service, project.ID, token, http.StatusOK)
			assertFederationTokenStatus(t, service, other.ID, token, http.StatusForbidden)

			spec.Capabilities = "claim,pull,push"
			again, err := service.EnsureFederationEnrollment(ctx, spec, token)
			require.NoError(t, err)
			assert.Equal(t, first, again)
			history, err := service.ListFederationEnrollments(ctx, project.UID)
			require.NoError(t, err)
			require.Len(t, history, 1, "retry must not create another credential")

			for _, change := range []struct {
				name   string
				mutate func(*kata.FederationEnrollmentSpec)
			}{
				{"project", func(s *kata.FederationEnrollmentSpec) { s.ProjectUID = other.UID }},
				{"instance", func(s *kata.FederationEnrollmentSpec) { s.SpokeInstanceUID = "01HZNQ7VFPK1XGD8R5MABCD4EB" }},
				{"capabilities", func(s *kata.FederationEnrollmentSpec) { s.Capabilities = "pull" }},
				{"actor", func(s *kata.FederationEnrollmentSpec) { s.Actor = "Another Operator" }},
				{"adoption", func(s *kata.FederationEnrollmentSpec) { s.AllowAdoptionSnapshotAuthors = true }},
			} {
				t.Run(change.name, func(t *testing.T) {
					changed := spec
					change.mutate(&changed)
					_, err := service.EnsureFederationEnrollment(ctx, changed, token)
					require.ErrorIs(t, err, kata.ErrFederationEnrollmentTokenConflict)
					assertFederationTokenStatus(t, service, project.ID, token, http.StatusOK)
				})
			}

			// A host may impose a one-live-credential policy, but Kata does not.
			otherSecret := make([]byte, 32)
			otherSecret[0] = 1
			otherToken := base64.RawURLEncoding.EncodeToString(otherSecret)
			independent, err := service.EnsureFederationEnrollment(ctx, spec, otherToken)
			require.NoError(t, err)
			assert.NotEqual(t, first.ID, independent.ID)
			require.NoError(t, service.RevokeFederationEnrollment(ctx, project.UID, first.ID))
			_, err = service.EnsureFederationEnrollment(ctx, spec, token)
			require.ErrorIs(t, err, kata.ErrFederationEnrollmentTokenConflict)
			assertFederationTokenStatus(t, service, project.ID, token, http.StatusForbidden)
			assertFederationTokenStatus(t, service, project.ID, otherToken, http.StatusOK)
			history, err = service.ListFederationEnrollments(ctx, project.UID)
			require.NoError(t, err)
			assert.Len(t, history, 2, "conflicting retries must not create credentials")

			t.Run("archived replay", func(t *testing.T) {
				_, err := service.ArchiveProject(ctx, project.UID, spec.Actor)
				require.NoError(t, err)
				again, err := service.EnsureFederationEnrollment(ctx, spec, otherToken)
				require.NoError(t, err)
				assert.Equal(t, independent, again, "exact replay must return the retained enrollment")

				_, err = service.EnsureFederationEnrollment(ctx, spec, token)
				require.ErrorIs(t, err, kata.ErrFederationEnrollmentTokenConflict)
				changed := spec
				changed.Actor = "Another Operator"
				_, err = service.EnsureFederationEnrollment(ctx, changed, otherToken)
				require.ErrorIs(t, err, kata.ErrFederationEnrollmentTokenConflict)

				newSecret := make([]byte, 32)
				newSecret[0] = 2
				_, err = service.EnsureFederationEnrollment(ctx, spec, base64.RawURLEncoding.EncodeToString(newSecret))
				require.ErrorIs(t, err, kata.ErrProjectNotFound)
				_, err = service.CreateFederationEnrollment(ctx, spec)
				require.ErrorIs(t, err, kata.ErrProjectNotFound)
				retained, err := service.ListFederationEnrollments(ctx, project.UID)
				require.NoError(t, err)
				assert.Equal(t, history, retained, "archived retries must not change enrollment history")
				result, err := service.EnsureProject(ctx, kata.ProjectSpec{UID: project.UID, Name: project.Name})
				require.NoError(t, err)
				assert.Equal(t, kata.ProjectArchived, result.Project.State)
				assertFederationTokenStatus(t, service, project.ID, otherToken, http.StatusForbidden)
			})
		})
	}
}

func TestServiceEnsureFederationEnrollmentRejectsInvalidToken(t *testing.T) {
	service, err := kata.New(t.Context(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project := ensureEnrollmentTestProject(t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "hub-project")
	spec := kata.FederationEnrollmentSpec{
		ProjectUID:       project.UID,
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Capabilities:     "pull",
		Actor:            "Example Operator",
	}
	valid := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, token := range []string{"", "invalid", valid + "=", valid + "\n", valid[:42] + "B"} {
		_, err := service.EnsureFederationEnrollment(t.Context(), spec, token)
		require.ErrorContains(t, err, "invalid federation token")
	}
	history, err := service.ListFederationEnrollments(t.Context(), project.UID)
	require.NoError(t, err)
	assert.Empty(t, history)
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/federation", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	assert.Equal(t, http.StatusNotFound, response.Code, "invalid input must not enable federation")
}

func TestServiceFederationEnrollmentLifecycleIsProjectScoped(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:     filepath.Join(t.TempDir(), "service.db"),
		Auth:    kata.AuthConfig{TrustCallerAuthentication: true},
		Profile: kata.EmbeddingProfileRestricted,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	firstProject := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "first-project",
	)
	secondProject := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4EY", "second-project",
	)

	created, err := service.CreateFederationEnrollment(context.Background(), kata.FederationEnrollmentSpec{
		ProjectUID:                   firstProject.UID,
		SpokeInstanceUID:             "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Capabilities:                 "push, pull,claim,pull",
		Actor:                        "Example Operator",
		AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	assert.Len(t, created.Token, 43)
	assert.Positive(t, created.Enrollment.ID)
	assert.Equal(t, firstProject.UID, created.Enrollment.ProjectUID)
	assert.Equal(t, "claim,pull,push", created.Enrollment.Capabilities)
	assert.Equal(t, "Example Operator", created.Enrollment.Actor)
	assert.True(t, created.Enrollment.AllowAdoptionSnapshotAuthors)
	assert.Nil(t, created.Enrollment.RevokedAt)

	foundEnrollment, found, err := service.FindActiveFederationEnrollment(
		context.Background(),
		kata.FederationEnrollmentSpec{
			ProjectUID:                   firstProject.UID,
			SpokeInstanceUID:             created.Enrollment.SpokeInstanceUID,
			Capabilities:                 created.Enrollment.Capabilities,
			Actor:                        created.Enrollment.Actor,
			AllowAdoptionSnapshotAuthors: created.Enrollment.AllowAdoptionSnapshotAuthors,
		},
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, created.Enrollment, foundEnrollment)

	_, found, err = service.FindActiveFederationEnrollment(
		context.Background(),
		kata.FederationEnrollmentSpec{
			ProjectUID:       firstProject.UID,
			SpokeInstanceUID: created.Enrollment.SpokeInstanceUID,
			Capabilities:     created.Enrollment.Capabilities,
			Actor:            "Different Operator",
		},
	)
	require.NoError(t, err)
	assert.False(t, found)

	firstHistory, err := service.ListFederationEnrollments(context.Background(), firstProject.UID)
	require.NoError(t, err)
	require.Len(t, firstHistory, 1)
	assert.Equal(t, created.Enrollment, firstHistory[0])

	secondHistory, err := service.ListFederationEnrollments(context.Background(), secondProject.UID)
	require.NoError(t, err)
	assert.Empty(t, secondHistory)

	assertFederationTokenStatus(t, service, firstProject.ID, created.Token, http.StatusOK)

	err = service.RevokeFederationEnrollment(
		context.Background(), secondProject.UID, created.Enrollment.ID,
	)
	require.ErrorIs(t, err, kata.ErrFederationEnrollmentNotFound)
	assertFederationTokenStatus(t, service, firstProject.ID, created.Token, http.StatusOK)

	require.NoError(t, service.RevokeFederationEnrollment(
		context.Background(), firstProject.UID, created.Enrollment.ID,
	))
	require.NoError(t, service.RevokeFederationEnrollment(
		context.Background(), firstProject.UID, created.Enrollment.ID,
	), "exact revocation retries must be harmless")
	assertFederationTokenStatus(t, service, firstProject.ID, created.Token, http.StatusForbidden)
	_, found, err = service.FindActiveFederationEnrollment(
		context.Background(),
		kata.FederationEnrollmentSpec{
			ProjectUID:                   firstProject.UID,
			SpokeInstanceUID:             created.Enrollment.SpokeInstanceUID,
			Capabilities:                 created.Enrollment.Capabilities,
			Actor:                        created.Enrollment.Actor,
			AllowAdoptionSnapshotAuthors: created.Enrollment.AllowAdoptionSnapshotAuthors,
		},
	)
	require.NoError(t, err)
	assert.False(t, found)

	firstHistory, err = service.ListFederationEnrollments(context.Background(), firstProject.UID)
	require.NoError(t, err)
	require.Len(t, firstHistory, 1)
	assert.NotNil(t, firstHistory[0].RevokedAt)
}

// This focused test pins the cross-project revoke boundary and additionally
// proves rejection leaves the enrollment active and exposes no enrollment
// history to the bystander project.
func TestServiceRevokeFederationEnrollmentRejectsCrossProjectID(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	owner := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4E1", "spoke-project",
	)
	bystander := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4E2", "hub-project",
	)
	created, err := service.CreateFederationEnrollment(
		context.Background(),
		kata.FederationEnrollmentSpec{
			ProjectUID:       owner.UID,
			SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
			Capabilities:     "pull",
			Actor:            "Example Operator",
		},
	)
	require.NoError(t, err)

	err = service.RevokeFederationEnrollment(
		context.Background(), bystander.UID, created.Enrollment.ID,
	)
	require.ErrorIs(t, err, kata.ErrFederationEnrollmentNotFound,
		"an enrollment belonging to another project must not be revocable")

	history, err := service.ListFederationEnrollments(context.Background(), owner.UID)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Nil(t, history[0].RevokedAt,
		"the rejected cross-project revoke must not have taken effect")

	empty, err := service.ListFederationEnrollments(context.Background(), bystander.UID)
	require.NoError(t, err)
	assert.Empty(t, empty,
		"a project with no enrollments must not see another project's history")
}

func TestServiceFederationEnrollmentRejectsInvalidOrArchivedProjects(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	project := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "archived-project",
	)
	_, err = service.ArchiveProject(context.Background(), project.UID, "Example Operator")
	require.NoError(t, err)

	for _, projectUID := range []string{"not-a-uid", "01HZNQ7VFPK1XGD8R5MABCD4EY", project.UID} {
		_, createErr := service.CreateFederationEnrollment(
			context.Background(),
			kata.FederationEnrollmentSpec{
				ProjectUID:       projectUID,
				SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Capabilities:     "pull",
				Actor:            "Example Operator",
			},
		)
		require.ErrorIs(t, createErr, kata.ErrProjectNotFound, projectUID)
	}
}

func TestServiceInvalidEnrollmentDoesNotEnableFederation(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	project := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "inactive-project",
	)
	_, err = service.CreateFederationEnrollment(context.Background(), kata.FederationEnrollmentSpec{
		ProjectUID:       project.UID,
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Capabilities:     "pull,unknown",
		Actor:            "Example Operator",
	})
	require.ErrorContains(t, err, "unknown federation capability")

	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/federation",
		nil,
	)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	assert.Equal(t, http.StatusNotFound, response.Code)
}

func ensureEnrollmentTestProject(
	t *testing.T,
	service *kata.Service,
	uid string,
	name string,
) kata.Project {
	t.Helper()
	result, err := service.EnsureProject(context.Background(), kata.ProjectSpec{UID: uid, Name: name})
	require.NoError(t, err)
	return result.Project
}

func assertFederationTokenStatus(
	t *testing.T,
	service *kata.Service,
	projectID int64,
	token string,
	want int,
) {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/federation/metadata",
		nil,
	)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	assert.Equal(t, want, response.Code)
}
