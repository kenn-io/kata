// Package federationconfig reconciles declarative federation project mappings.
package federationconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

var (
	// ErrHubUnavailable classifies transport failures and transient hub errors.
	ErrHubUnavailable = errors.New("federation hub unavailable")
	// ErrHubAuthentication classifies catalog credential and authorization errors.
	ErrHubAuthentication = errors.New("federation hub authentication")
	// ErrHubValidation classifies rejected or malformed hub requests and responses.
	ErrHubValidation = errors.New("federation hub validation")

	normalizeHubURL       = client.NormalizeRemoteURL
	newHubHTTPClient      = client.NewHTTPClientForTarget
	configureHubRedirects = client.ConfigureOriginPinnedRedirects
)

const (
	hubRequestTimeout   = 30 * time.Second
	maxHubResponseBytes = 1 << 20
)

// HubError is a bounded, secret-free hub failure. It intentionally retains
// only a stable category, operation label, and optional HTTP status.
type HubError struct {
	Kind       error
	Operation  string
	StatusCode int
}

// Error implements error without retaining request coordinates or response
// bodies, either of which may contain private data or echoed credentials.
func (e *HubError) Error() string {
	if e == nil {
		return ""
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("federation hub %s failed with HTTP status %d", e.Operation, e.StatusCode)
	}
	return fmt.Sprintf("federation hub %s failed", e.Operation)
}

// Unwrap exposes the stable error category.
func (e *HubError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}

// HubProject is the hub metadata needed to bind one local replica.
type HubProject struct {
	ID, ReplayHorizonEventID, BaselineThroughEventID int64
	UID, Name                                        string
}

// EnrollmentRequest describes one project-scoped explicit-token grant.
type EnrollmentRequest struct {
	ProjectID                    int64
	SpokeInstanceUID             string
	Token, Capabilities, Actor   string
	AllowAdoptionSnapshotAuthors bool
}

// Enrollment is the identity-bearing portion of a hub enrollment response.
type Enrollment struct {
	ID    int64
	Actor string
}

// Hub is the narrow remote contract used by one reconciliation attempt.
type Hub interface {
	ResolveProject(context.Context, string) (HubProject, error)
	EnsureProject(context.Context, string, string) (HubProject, error)
	EnsureEnrollment(context.Context, EnrollmentRequest) (Enrollment, error)
	RotateEnrollment(context.Context, EnrollmentRequest) (Enrollment, error)
	RevokeEnrollment(context.Context, int64) error
}

// HubClient binds every request and the selected catalog bearer to one
// canonical hub origin.
type HubClient struct {
	baseURL string
	http    *http.Client
}

// NewHubClient resolves only the selected catalog entry's authentication
// policy. In particular, an empty token_env is an authentication error and
// never falls back to KATA_AUTH_TOKEN or [auth].token.
func NewHubClient(
	ctx context.Context, catalog config.CatalogDaemonConfig,
) (*HubClient, error) {
	token, err := catalogToken(catalog)
	if err != nil {
		return nil, err
	}
	baseURL, err := normalizeHubURL(catalog.URL, catalog.AllowInsecure)
	if err != nil {
		return nil, hubError(ErrHubValidation, "hub URL validation", 0)
	}
	httpClient, err := newHubHTTPClient(ctx, baseURL, client.TargetAuth{
		Token:         token,
		AllowInsecure: catalog.AllowInsecure,
	}, client.Opts{Timeout: hubRequestTimeout})
	if err != nil {
		return nil, hubError(ErrHubValidation, "hub transport setup", 0)
	}
	if err := configureHubRedirects(httpClient, baseURL); err != nil {
		return nil, hubError(ErrHubValidation, "hub redirect policy", 0)
	}
	return &HubClient{baseURL: baseURL, http: httpClient}, nil
}

func catalogToken(catalog config.CatalogDaemonConfig) (string, error) {
	if catalog.TokenEnv == "" {
		return strings.TrimSpace(catalog.Token), nil
	}
	token := strings.TrimSpace(os.Getenv(catalog.TokenEnv))
	if token == "" {
		return "", hubError(ErrHubAuthentication, "catalog authentication", 0)
	}
	return token, nil
}

// EnsureProject resolves or creates name, enables federation, and returns the
// enabled project's stable federation metadata.
func (c *HubClient) EnsureProject(
	ctx context.Context, name, actor string,
) (HubProject, error) {
	project, err := c.resolveProject(ctx, name)
	if err != nil {
		var hubErr *HubError
		if !errors.As(err, &hubErr) || hubErr == nil || hubErr.StatusCode != http.StatusNotFound {
			return HubProject{}, err
		}
		project, err = c.createProject(ctx, name)
		if err != nil {
			// A concurrent creator can win between resolve and create. One
			// strict re-resolve makes that race idempotent without hiding other
			// conflicts.
			if errors.As(err, &hubErr) && hubErr != nil && hubErr.StatusCode == http.StatusConflict {
				project, err = c.resolveProject(ctx, name)
			}
			if err != nil {
				return HubProject{}, err
			}
		}
	}

	var enabled projectFederationResponse
	err = c.doJSON(ctx, http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/federation/enable",
		struct {
			Actor string `json:"actor,omitempty"`
		}{Actor: actor},
		&enabled,
		"enable project federation",
	)
	if err != nil {
		return HubProject{}, err
	}
	result := HubProject{
		ID:                     enabled.ProjectID,
		UID:                    enabled.ProjectUID,
		Name:                   enabled.ProjectName,
		ReplayHorizonEventID:   enabled.ReplayHorizonEventID,
		BaselineThroughEventID: enabled.BaselineThroughEventID,
	}
	if result.ID <= 0 || !katauid.Valid(result.UID) || result.Name == "" ||
		result.ReplayHorizonEventID <= 0 ||
		result.ID != project.ID ||
		result.UID != project.UID ||
		result.Name != project.Name {
		return HubProject{}, hubError(ErrHubValidation, "enable project federation", http.StatusOK)
	}
	return result, nil
}

// ResolveProject looks up a project without creating it or enabling
// federation.
func (c *HubClient) ResolveProject(ctx context.Context, name string) (HubProject, error) {
	project, err := c.resolveProject(ctx, name)
	if err != nil {
		return HubProject{}, err
	}
	return HubProject{
		ID:   project.ID,
		UID:  project.UID,
		Name: project.Name,
	}, nil
}

func (c *HubClient) resolveProject(ctx context.Context, name string) (projectResponse, error) {
	var response struct {
		Project projectResponse `json:"project"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/projects/resolve",
		struct {
			Name string `json:"name"`
		}{Name: name},
		&response,
		"resolve project",
	)
	if err != nil {
		return projectResponse{}, err
	}
	if response.Project.ID <= 0 ||
		!katauid.Valid(response.Project.UID) ||
		response.Project.Name == "" {
		return projectResponse{}, hubError(ErrHubValidation, "resolve project", http.StatusOK)
	}
	return response.Project, nil
}

func (c *HubClient) createProject(ctx context.Context, name string) (projectResponse, error) {
	var response struct {
		Project projectResponse `json:"project"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/projects",
		struct {
			Name string `json:"name"`
		}{Name: name},
		&response,
		"create project",
	)
	if err != nil {
		return projectResponse{}, err
	}
	if response.Project.ID <= 0 ||
		!katauid.Valid(response.Project.UID) ||
		response.Project.Name == "" {
		return projectResponse{}, hubError(ErrHubValidation, "create project", http.StatusOK)
	}
	return response.Project, nil
}

type projectResponse struct {
	ID   int64  `json:"id"`
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type projectFederationResponse struct {
	ProjectID              int64  `json:"project_id"`
	ProjectUID             string `json:"project_uid"`
	ProjectName            string `json:"project_name"`
	ReplayHorizonEventID   int64  `json:"replay_horizon_event_id"`
	BaselineThroughEventID int64  `json:"baseline_through_event_id"`
}

type enrollmentWireRequest struct {
	ProjectID                    int64  `json:"project_id"`
	SpokeInstanceUID             string `json:"spoke_instance_uid"`
	Token                        string `json:"token"`
	Capabilities                 string `json:"capabilities"`
	Actor                        string `json:"actor,omitempty"`
	AllowAdoptionSnapshotAuthors bool   `json:"allow_adoption_snapshot_authors,omitempty"`
}

type enrollmentWireResponse struct {
	ID    int64  `json:"id"`
	Actor string `json:"actor"`
}

// EnsureEnrollment idempotently ensures the request's explicit token.
func (c *HubClient) EnsureEnrollment(
	ctx context.Context, request EnrollmentRequest,
) (Enrollment, error) {
	return c.enrollmentRequest(ctx, "/api/v1/federation/enrollments",
		"ensure enrollment", request)
}

// RotateEnrollment replaces only the matching spoke/project enrollment scope.
func (c *HubClient) RotateEnrollment(
	ctx context.Context, request EnrollmentRequest,
) (Enrollment, error) {
	return c.enrollmentRequest(ctx, "/api/v1/federation/enrollments/actions/rotate",
		"rotate enrollment", request)
}

// RevokeEnrollment compensates an enrollment or rotation whose local
// reservation was explicitly removed while the hub request was in flight.
func (c *HubClient) RevokeEnrollment(ctx context.Context, enrollmentID int64) error {
	if enrollmentID <= 0 {
		return hubError(ErrHubValidation, "revoke enrollment", 0)
	}
	return c.doJSON(
		ctx,
		http.MethodPost,
		"/api/v1/federation/enrollments/"+strconv.FormatInt(enrollmentID, 10)+"/revoke",
		nil,
		nil,
		"revoke enrollment",
	)
}

func (c *HubClient) enrollmentRequest(
	ctx context.Context, path, operation string, request EnrollmentRequest,
) (Enrollment, error) {
	var response enrollmentWireResponse
	err := c.doJSON(
		ctx, http.MethodPost, path, enrollmentWireRequest(request), &response, operation,
	)
	if err != nil {
		return Enrollment{}, err
	}
	actor := strings.TrimSpace(response.Actor)
	if response.ID <= 0 || db.ValidateTokenActor(actor) != nil {
		return Enrollment{}, hubError(ErrHubValidation, operation, http.StatusOK)
	}
	return Enrollment{ID: response.ID, Actor: actor}, nil
}

func (c *HubClient) doJSON(
	ctx context.Context,
	method, path string,
	input, output any,
	operation string,
) error {
	body, err := json.Marshal(input)
	if err != nil {
		return hubError(ErrHubValidation, operation, 0)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return hubError(ErrHubValidation, operation, 0)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return hubError(ErrHubUnavailable, operation, 0)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return hubError(hubStatusKind(response.StatusCode), operation, response.StatusCode)
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxHubResponseBytes))
	if err := decoder.Decode(output); err != nil {
		return hubError(ErrHubValidation, operation, response.StatusCode)
	}
	return nil
}

func hubStatusKind(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrHubAuthentication
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		return ErrHubUnavailable
	case status >= http.StatusInternalServerError:
		return ErrHubUnavailable
	default:
		return ErrHubValidation
	}
}

func hubError(kind error, operation string, status int) *HubError {
	return &HubError{Kind: kind, Operation: operation, StatusCode: status}
}

var _ Hub = (*HubClient)(nil)
