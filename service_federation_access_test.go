package kata_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/db"
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

func TestServiceFederationAccessPreservesOrdinaryMutationFence(t *testing.T) {
	tests := []struct {
		name       string
		access     *recordingAccessController
		wantStatus int
	}{
		{
			name:       "missing ordinary fence",
			access:     &recordingAccessController{omitTransactionFence: true},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "denying ordinary fence",
			access: &recordingAccessController{transactionFence: func(context.Context, kata.Transaction) error {
				return kata.ErrAccessDenied
			}},
			wantStatus: http.StatusNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var federationFenceCalls int
			federation := &recordingFederationAccessController{
				decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
					decision := kata.FederationAccessDecision{}
					if request.Operation.Mutation {
						decision.TransactionFence = func(context.Context, kata.Transaction) error {
							federationFenceCalls++
							return nil
						}
					}
					return decision, nil
				},
			}
			service, project, enrollment, databasePath := newComposedFederationAccessService(t, tt.access, federation)
			forceFederationBaselineRefresh(t, databasePath, project)

			response := getFederationMetadataAsPrincipal(t, service, project.ID, enrollment.Token)

			assert.Equal(t, tt.wantStatus, response.Code, response.Body.String())
			assert.Zero(t, federationFenceCalls, "federation fence must not run after ordinary access rejects")
		})
	}
}

func TestServiceFederationAccessRunsBothMutationFencesInOrder(t *testing.T) {
	var calls []string
	access := &recordingAccessController{transactionFence: func(context.Context, kata.Transaction) error {
		calls = append(calls, "ordinary")
		return nil
	}}
	federation := &recordingFederationAccessController{
		decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			decision := kata.FederationAccessDecision{}
			if request.Operation.Mutation {
				decision.TransactionFence = func(context.Context, kata.Transaction) error {
					calls = append(calls, "federation")
					return nil
				}
			}
			return decision, nil
		},
	}
	service, project, enrollment, databasePath := newComposedFederationAccessService(t, access, federation)
	forceFederationBaselineRefresh(t, databasePath, project)

	response := getFederationMetadataAsPrincipal(t, service, project.ID, enrollment.Token)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, []string{"ordinary", "federation"}, calls)
}

func TestServiceFederationIngestRunsBothMutationFencesInOrder(t *testing.T) {
	var calls []string
	access := &recordingAccessController{transactionFence: func(context.Context, kata.Transaction) error {
		calls = append(calls, "ordinary")
		return nil
	}}
	federation := &recordingFederationAccessController{
		decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			decision := kata.FederationAccessDecision{}
			if request.Operation.Mutation {
				decision.TransactionFence = func(context.Context, kata.Transaction) error {
					calls = append(calls, "federation")
					return nil
				}
			}
			return decision, nil
		},
	}
	service, project, enrollment, databasePath := newComposedFederationAccessService(t, access, federation)

	response := ingestFederationIssueAsPrincipal(t, service, project, enrollment.Token)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, []string{"ordinary", "federation"}, calls)
	assert.Equal(t, 1, storedFederationIssueCount(t, databasePath))
}

func TestServiceFederationIngestDenialRollsBackMutation(t *testing.T) {
	var calls []string
	access := &recordingAccessController{transactionFence: func(context.Context, kata.Transaction) error {
		calls = append(calls, "ordinary")
		return nil
	}}
	federation := &recordingFederationAccessController{
		decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			decision := kata.FederationAccessDecision{}
			if request.Operation.Mutation {
				decision.TransactionFence = func(context.Context, kata.Transaction) error {
					calls = append(calls, "federation")
					return kata.ErrAccessDenied
				}
			}
			return decision, nil
		},
	}
	service, project, enrollment, databasePath := newComposedFederationAccessService(t, access, federation)

	response := ingestFederationIssueAsPrincipal(t, service, project, enrollment.Token)

	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Equal(t, []string{"ordinary", "federation"}, calls)
	assert.Zero(t, storedFederationIssueCount(t, databasePath))
}

func ingestFederationIssueAsPrincipal(
	t *testing.T,
	service *kata.Service,
	project kata.Project,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	createdAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	issueUID := "01HZNQ7VFPK1XGD8R5MABCD4EC"
	payload := json.RawMessage(`{"uid":"01HZNQ7VFPK1XGD8R5MABCD4EC","short_id":"cd4ec","title":"remote work","body":"","author":"Example Operator","status":"open","metadata":{},"created_at":"2026-07-22T12:00:00.000Z"}`)
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               "01HZNQ7VFPK1XGD8R5MABCD4EB",
		OriginInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		ProjectUID:        project.UID,
		ProjectName:       project.Name,
		IssueUID:          &issueUID,
		Type:              "issue.created",
		Actor:             "Example Operator",
		HLCPhysicalMS:     1,
		CreatedAt:         "2026-07-22T12:00:00.000Z",
		Payload:           payload,
	})
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"schema_version": db.CurrentSchemaVersion(),
		"events": []map[string]any{{
			"event_id": 17, "event_uid": "01HZNQ7VFPK1XGD8R5MABCD4EB",
			"origin_instance_uid": "01HZNQ7VFPK1XGD8R5MABCD4EA",
			"project_uid":         project.UID, "project_name": project.Name,
			"issue_uid": issueUID, "type": "issue.created", "actor": "Example Operator",
			"hlc_physical_ms": 1, "hlc_counter": 0, "content_hash": hash,
			"payload": json.RawMessage(payload), "created_at": createdAt,
		}},
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+
		strconv.FormatInt(project.ID, 10)+"/federation/events:ingest", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request = request.WithContext(kata.WithPrincipal(request.Context(), kata.Principal{
		Subject: "user-123", Actor: "Example User",
	}))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func storedFederationIssueCount(t *testing.T, databasePath string) int {
	t.Helper()
	inspection, err := sql.Open("sqlite", databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	var count int
	require.NoError(t, inspection.QueryRow(
		`SELECT count(*) FROM issues WHERE uid = ?`, "01HZNQ7VFPK1XGD8R5MABCD4EC",
	).Scan(&count))
	return count
}

func newComposedFederationAccessService(
	t *testing.T,
	access kata.AccessController,
	federation kata.FederationAccessController,
) (*kata.Service, kata.Project, kata.CreatedFederationEnrollment, string) {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "service.db")
	service, err := kata.New(context.Background(), kata.Config{
		DSN: databasePath, Access: access, FederationAccess: federation,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "example-project",
	)
	enrollment, err := service.CreateFederationEnrollment(context.Background(), kata.FederationEnrollmentSpec{
		ProjectUID:       project.UID,
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Capabilities:     "pull,push",
		Actor:            "Example Operator",
	})
	require.NoError(t, err)
	return service, project, enrollment, databasePath
}

func forceFederationBaselineRefresh(t *testing.T, databasePath string, project kata.Project) {
	t.Helper()
	inspection, err := sql.Open("sqlite", databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	_, err = inspection.Exec(`
		INSERT INTO purge_log(
			uid, origin_instance_uid, project_id, purged_issue_id,
			project_uid, project_name, issue_title, issue_author,
			comment_count, link_count, label_count, event_count,
			purge_reset_after_event_id, short_id, actor
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, 0, ?, ?, ?)`,
		"01HZNQ7VFPK1XGD8R5MABCD4EB", "01HZNQ7VFPK1XGD8R5MABCD4EC",
		project.ID, 999, project.UID, project.Name, "purged issue", "Example User",
		999999, "d4eb", "Example User")
	require.NoError(t, err)
}

func getFederationMetadataAsPrincipal(
	t *testing.T,
	service *kata.Service,
	projectID int64,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+
		strconv.FormatInt(projectID, 10)+"/federation/metadata", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request = request.WithContext(kata.WithPrincipal(request.Context(), kata.Principal{
		Subject: "user-123", Actor: "Example User",
	}))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
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
