package kata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
)

type recordingFederationAccessController struct {
	mu       sync.Mutex
	requests []kata.FederationAccessRequest
	decide   func(kata.FederationAccessRequest) (kata.FederationAccessDecision, error)
}

func (c *recordingFederationAccessController) AuthorizeFederation(
	_ context.Context,
	request kata.FederationAccessRequest,
) (kata.FederationAccessDecision, error) {
	c.mu.Lock()
	c.requests = append(c.requests, request)
	c.mu.Unlock()
	if c.decide != nil {
		return c.decide(request)
	}
	return kata.FederationAccessDecision{}, nil
}

func (c *recordingFederationAccessController) snapshot() []kata.FederationAccessRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]kata.FederationAccessRequest(nil), c.requests...)
}

func TestServiceFederationAccessReceivesAuthenticatedEnrollment(t *testing.T) {
	controller := &recordingFederationAccessController{decide: allowFederationMutation}
	service, project, enrollment := newFederationAccessService(t, controller)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/federation/metadata", nil)
	request.Header.Set("Authorization", "Bearer "+enrollment.Token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, controller.snapshot(), 1)
	assert.Equal(t, kata.FederationAccessRequest{
		Enrollment: enrollment.Enrollment,
		Project:    project,
		Capability: kata.FederationCapabilityPull,
		Operation: kata.FederationOperation{
			ID: "getFederationProjectMetadata", Mutation: true,
		},
	}, controller.snapshot()[0])
}

func TestServiceFederationAccessDenialDoesNotUseValidCredential(t *testing.T) {
	controller := &recordingFederationAccessController{
		decide: func(kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			return kata.FederationAccessDecision{}, kata.ErrAccessDenied
		},
	}
	service, project, enrollment := newFederationAccessService(t, controller)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/federation/metadata", nil)
	request.Header.Set("Authorization", "Bearer "+enrollment.Token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.JSONEq(t,
		`{"status":403,"error":{"code":"auth_invalid","message":"federation credential is not currently authorized"}}`,
		response.Body.String())
}

func TestServiceFederationAccessRejectsInvalidCredentialBeforeHostDecision(t *testing.T) {
	controller := &recordingFederationAccessController{}
	service, project, _ := newFederationAccessService(t, controller)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/federation/metadata", nil)
	request.Header.Set("Authorization", "Bearer invalid-credential")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.Empty(t, controller.snapshot())
}

func TestServiceFederationAccessChecksBearerOnLeaseStatus(t *testing.T) {
	controller := &recordingFederationAccessController{
		decide: func(kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			return kata.FederationAccessDecision{}, kata.ErrAccessDenied
		},
	}
	service, project, enrollment := newFederationAccessService(t, controller)
	issue := createFederationAccessIssue(t, service, project.ID)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/issues/"+issue+"/lease", nil)
	request.Header.Set("Authorization", "Bearer "+enrollment.Token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Len(t, controller.snapshot(), 1)
	assert.Equal(t, kata.FederationOperation{
		ID: "getIssueLeaseStatus", Mutation: true,
	}, controller.snapshot()[0].Operation)
}

func TestServiceFederationAccessRejectsInvalidLeaseStatusCredentialBeforeHostDecision(t *testing.T) {
	controller := &recordingFederationAccessController{}
	service, project, _ := newFederationAccessService(t, controller)
	issue := createFederationAccessIssue(t, service, project.ID)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/issues/"+issue+"/lease", nil)
	request.Header.Set("Authorization", "Bearer invalid-credential")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Empty(t, controller.snapshot())
}

func TestServiceFederationAccessKeepsTokenlessTrustedLeaseStatus(t *testing.T) {
	controller := &recordingFederationAccessController{
		decide: func(kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			return kata.FederationAccessDecision{}, kata.ErrAccessDenied
		},
	}
	service, project, _ := newFederationAccessService(t, controller)
	issue := createFederationAccessIssue(t, service, project.ID)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/issues/"+issue+"/lease", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Empty(t, controller.snapshot())
}

func TestServiceFederationAccessMetadataRequiresTransactionFence(t *testing.T) {
	controller := &recordingFederationAccessController{}
	service, project, enrollment := newFederationAccessService(t, controller)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/federation/metadata", nil)
	request.Header.Set("Authorization", "Bearer "+enrollment.Token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.Len(t, controller.snapshot(), 1)
	assert.Equal(t, kata.FederationOperation{
		ID: "getFederationProjectMetadata", Mutation: true,
	}, controller.snapshot()[0].Operation)
}

func TestServiceFederationAccessMutationRequiresTransactionFence(t *testing.T) {
	controller := &recordingFederationAccessController{}
	service, project, enrollment := newFederationAccessService(t, controller)
	issue := createFederationAccessIssue(t, service, project.ID)

	response := acquireFederationAccessClaim(t, service, project.ID, issue, enrollment.Token)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.JSONEq(t,
		`{"status":503,"error":{"code":"access_unavailable","message":"federation transaction access decision unavailable"}}`,
		response.Body.String())
	require.Len(t, controller.snapshot(), 1)
	assert.Equal(t, kata.FederationCapabilityClaim, controller.snapshot()[0].Capability)
	assert.Equal(t, kata.FederationOperation{
		ID: "acquireIssueLease", Mutation: true,
	}, controller.snapshot()[0].Operation)
}

func TestServiceFederationAccessFenceRollsBackClaimMutation(t *testing.T) {
	controller := &recordingFederationAccessController{
		decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			decision := kata.FederationAccessDecision{}
			if request.Operation.Mutation {
				decision.TransactionFence = func(context.Context, kata.Transaction) error {
					return kata.ErrAccessDenied
				}
			}
			return decision, nil
		},
	}
	service, project, enrollment := newFederationAccessService(t, controller)
	issue := createFederationAccessIssue(t, service, project.ID)

	response := acquireFederationAccessClaim(t, service, project.ID, issue, enrollment.Token)

	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())

	controller.decide = allowFederationMutation
	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/issues/"+issue+"/lease", nil)
	statusRequest.Header.Set("Authorization", "Bearer "+enrollment.Token)
	statusResponse := httptest.NewRecorder()
	service.Handler().ServeHTTP(statusResponse, statusRequest)
	require.Equal(t, http.StatusOK, statusResponse.Code, statusResponse.Body.String())
	var status struct {
		Held bool `json:"held"`
	}
	require.NoError(t, json.Unmarshal(statusResponse.Body.Bytes(), &status))
	assert.False(t, status.Held)
}

func allowFederationMutation(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
	decision := kata.FederationAccessDecision{}
	if request.Operation.Mutation {
		decision.TransactionFence = func(context.Context, kata.Transaction) error { return nil }
	}
	return decision, nil
}

func TestServiceFederationAccessSanitizesFenceFailure(t *testing.T) {
	controller := &recordingFederationAccessController{
		decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			decision := kata.FederationAccessDecision{}
			if request.Operation.Mutation {
				decision.TransactionFence = func(context.Context, kata.Transaction) error {
					return errors.New("private controller detail")
				}
			}
			return decision, nil
		},
	}
	service, project, enrollment := newFederationAccessService(t, controller)
	issue := createFederationAccessIssue(t, service, project.ID)

	response := acquireFederationAccessClaim(t, service, project.ID, issue, enrollment.Token)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.JSONEq(t,
		`{"status":503,"error":{"code":"access_unavailable","message":"federation credential authorization is unavailable"}}`,
		response.Body.String())
	assert.NotContains(t, response.Body.String(), "private controller detail")
}

func acquireFederationAccessClaim(
	t *testing.T,
	service *kata.Service,
	projectID int64,
	issue string,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+
		strconv.FormatInt(projectID, 10)+"/issues/"+issue+"/lease/actions/acquire",
		bytes.NewBufferString(`{"holder":"worker-1","claim_kind":"timed","ttl_seconds":300}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func newFederationAccessService(
	t *testing.T,
	controller kata.FederationAccessController,
) (*kata.Service, kata.Project, kata.CreatedFederationEnrollment) {
	t.Helper()
	service, err := kata.New(context.Background(), kata.Config{
		DSN:              filepath.Join(t.TempDir(), "service.db"),
		Auth:             kata.AuthConfig{TrustCallerAuthentication: true},
		FederationAccess: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "example-project",
	)
	enrollment, err := service.CreateFederationEnrollment(context.Background(), kata.FederationEnrollmentSpec{
		ProjectUID:       project.UID,
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Capabilities:     "claim,pull,push",
		Actor:            "Example Operator",
	})
	require.NoError(t, err)
	return service, project, enrollment
}

func createFederationAccessIssue(t *testing.T, service *kata.Service, projectID int64) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+
		strconv.FormatInt(projectID, 10)+"/issues",
		bytes.NewBufferString(`{"actor":"Example Operator","title":"claim fence"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.NotEmpty(t, body.Issue.ShortID)
	return body.Issue.ShortID
}
