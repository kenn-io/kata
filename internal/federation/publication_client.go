package federation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationpublication"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/pkg/client/generated"
)

// PublishFederationProject sends the frozen object through the selected catalog
// account. Capability and target identity are checked before any mutation. The
// publication path never falls back to owner replay or direct enrollment.
func (c *HubClient) PublishFederationProject(ctx context.Context, envelopeBytes []byte, preflight bool) (api.FederationPublicationBody, error) {
	var out api.FederationPublicationBody
	document, counts, err := federationpublication.DecodeWithCounts(envelopeBytes)
	if err != nil {
		code, status := "federation_publication_invalid", http.StatusBadRequest
		if errors.Is(err, db.ErrFederationPublicationTooLarge) {
			code, status = "federation_publication_too_large", http.StatusRequestEntityTooLarge
		}
		return out, api.NewError(status, code, "invalid publication envelope", "", nil)
	}
	origin, err := httpurl.CanonicalHTTPOrigin(c.baseURL)
	if err != nil || origin != c.baseURL {
		return out, hubError(ErrHubValidation, "publication target", 0)
	}
	client, err := c.publicationAPIClient()
	if err != nil {
		return out, err
	}
	instanceResponse, callErr := client.InstanceWithResponse(ctx)
	if instanceResponse == nil {
		if callErr != nil {
			return out, hubCallError(callErr, "publication instance probe")
		}
		return out, hubError(ErrHubUnavailable, "publication instance probe", 0)
	}
	if len(instanceResponse.Body) > maxHubResponseBytes {
		return out, hubError(ErrHubValidation, "publication instance response", instanceResponse.StatusCode)
	}
	if err := publicationResponseError("publication instance probe", instanceResponse.StatusCode, instanceResponse.Body, callErr); err != nil {
		return out, err
	}
	if instanceResponse.JSON200 == nil {
		return out, hubError(ErrHubValidation, "publication instance response", instanceResponse.StatusCode)
	}
	instance := *instanceResponse.JSON200
	if instance.RelayPublicationVersion == nil || *instance.RelayPublicationVersion != 1 {
		return out, publicationUnsupported()
	}
	if instance.InstanceUID != document.TargetInstanceUID {
		return out, api.NewError(http.StatusConflict, "federation_publication_conflict", "selected hub identity does not match the publication target", "", nil)
	}

	var envelope generated.FederationPublicationEnvelope
	if err := json.Unmarshal(envelopeBytes, &envelope); err != nil {
		return out, hubError(ErrHubValidation, "publication envelope", 0)
	}
	body := generated.PublishFederationProjectBody{Preflight: preflight, Envelope: envelope}
	if err := body.Validate(); err != nil {
		return out, hubError(ErrHubValidation, "publication envelope", 0)
	}
	requestBytes, err := json.Marshal(body)
	if err != nil {
		return out, hubError(ErrHubValidation, "publication request", 0)
	}
	if len(requestBytes) > db.MaxFederationPublicationRequestBytes {
		return out, api.NewError(http.StatusRequestEntityTooLarge, "federation_publication_too_large", "publication exceeds request capacity", "", nil)
	}
	publicationResponse, callErr := client.PublishFederationProjectWithResponse(ctx, &generated.PublishFederationProjectRequestOptions{Body: &body})
	if publicationResponse == nil {
		if callErr != nil {
			return out, hubCallError(callErr, "publication request")
		}
		return out, hubError(ErrHubUnavailable, "publication request", 0)
	}
	if len(publicationResponse.Body) > maxHubResponseBytes {
		return out, hubError(ErrHubValidation, "publication response", publicationResponse.StatusCode)
	}
	if err := publicationResponseError("publication request", publicationResponse.StatusCode, publicationResponse.Body, callErr); err != nil {
		return out, err
	}
	if publicationResponse.JSON200 == nil || publicationResponse.JSON200.Validate() != nil {
		return out, hubError(ErrHubValidation, "publication response", publicationResponse.StatusCode)
	}
	result := *publicationResponse.JSON200
	projectID := int64(0)
	if result.ProjectID != nil {
		projectID = *result.ProjectID
	}
	if result.ProjectUID != document.ProjectUID || result.ProjectName == "" || db.ValidateTokenActor(result.Account) != nil ||
		(string(result.Visibility) != "all" && string(result.Visibility) != "teams") ||
		(preflight && (string(result.Status) != "ready" || projectID != 0 || result.Replayed)) ||
		(!preflight && (string(result.Status) != "published" || projectID <= 0)) {
		return api.FederationPublicationBody{}, hubError(ErrHubValidation, "publication response", http.StatusOK)
	}
	if result.Counts != (generated.FederationPublicationCounts{Issues: counts.Issues, Comments: counts.Comments, Events: counts.Events}) {
		return api.FederationPublicationBody{}, hubError(ErrHubValidation, "publication response", http.StatusOK)
	}
	return api.FederationPublicationBody{
		Status: string(result.Status), ProjectUID: result.ProjectUID, ProjectID: projectID,
		ProjectName: result.ProjectName, Account: result.Account, Visibility: string(result.Visibility),
		Counts:   api.FederationPublicationCounts{Issues: result.Counts.Issues, Comments: result.Counts.Comments, Events: result.Counts.Events},
		Replayed: result.Replayed,
	}, nil
}

func (c *HubClient) publicationAPIClient() (*generated.Client, error) {
	if c.http == nil {
		return nil, hubError(ErrHubUnavailable, "publication transport", 0)
	}
	transport := *c.http
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(publicationDoer{client: &transport}))
	if err != nil {
		return nil, hubError(ErrHubValidation, "publication client setup", 0)
	}
	return client, nil
}

type publicationDoer struct{ client *http.Client }

func (d publicationDoer) Do(ctx context.Context, request *http.Request) (*http.Response, error) {
	request.Header.Set("Accept", "application/json")
	response, err := d.client.Do(request.WithContext(ctx)) //nolint:gosec // G704: the generated route uses the canonical, origin-pinned catalog client.
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, hubError(ErrHubUnavailable, "publication request", 0)
	}
	if response == nil || response.Body == nil {
		return nil, hubError(ErrHubUnavailable, "publication request", 0)
	}
	response.Body = limitedHubBody{Reader: io.LimitReader(response.Body, maxHubResponseBytes+1), Closer: response.Body}
	return response, nil
}

func publicationResponseError(operation string, status int, raw []byte, callErr error) error {
	if status >= http.StatusBadRequest {
		var envelope api.ErrorEnvelope
		if json.Unmarshal(raw, &envelope) == nil {
			// Carry stable codes, never arbitrary hub messages or echoed input.
			for code, expectedStatus := range map[string]int{
				"federation_publication_unsupported": http.StatusConflict, "federation_publication_conflict": http.StatusConflict,
				"federation_publication_invalid": http.StatusBadRequest, "federation_publication_too_large": http.StatusRequestEntityTooLarge,
				"unauthorized": http.StatusUnauthorized, "auth_required": http.StatusUnauthorized, "token_invalid": http.StatusForbidden,
				"bootstrap_token_write_forbidden": http.StatusForbidden, "scoped_operation_forbidden": http.StatusForbidden,
				"relay_user_credential_required": http.StatusForbidden, "not_found": http.StatusNotFound, "access_unavailable": http.StatusServiceUnavailable,
			} {
				if envelope.Error.Code == code && status == expectedStatus {
					return api.NewError(status, code, "hub refused publication", "", nil)
				}
			}
		}
		if status == http.StatusNotFound {
			return publicationUnsupported()
		}
		return hubError(hubStatusKind(status), operation, status)
	}
	if callErr != nil {
		return hubCallError(callErr, operation)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		if status == http.StatusNotFound {
			return publicationUnsupported()
		}
		return hubError(hubStatusKind(status), operation, status)
	}
	return nil
}

func publicationUnsupported() error {
	return api.NewError(http.StatusConflict, "federation_publication_unsupported", "hub does not support relay publication", "upgrade the selected hub before publishing", nil)
}
