package kata_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
)

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

	firstHistory, err = service.ListFederationEnrollments(context.Background(), firstProject.UID)
	require.NoError(t, err)
	require.Len(t, firstHistory, 1)
	assert.NotNil(t, firstHistory[0].RevokedAt)
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
