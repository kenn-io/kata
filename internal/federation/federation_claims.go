package federation

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/kata/internal/api"
)

// ClaimRequest is the wire body for federation claim actions.
type ClaimRequest = api.ClaimActionBody

// ClaimResponse is the wire body returned by federation claim actions.
type ClaimResponse api.ClaimActionResponseBody

// canonicalLease returns the canonical lease, falling back to the deprecated
// claim alias for responses from older hubs.
func (r ClaimResponse) canonicalLease() *api.IssueClaimOut {
	if r.Lease != nil {
		return r.Lease
	}
	return r.Claim
}

// ClaimStatusResponse is the wire body returned by federation lease status reads.
type ClaimStatusResponse = api.ClaimStatusBody

// AcquireClaim asks the authoritative hub to arbitrate an issue claim.
func (c *Client) AcquireClaim(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	req ClaimRequest,
) (ClaimResponse, error) {
	return c.claimAction(ctx, hubProjectID, ref, "acquire", req)
}

// RenewClaim asks the authoritative hub to renew a timed issue claim.
func (c *Client) RenewClaim(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	req ClaimRequest,
) (ClaimResponse, error) {
	return c.claimAction(ctx, hubProjectID, ref, "renew", req)
}

// ReleaseClaim asks the authoritative hub to release an issue claim.
func (c *Client) ReleaseClaim(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	req ClaimRequest,
) (ClaimResponse, error) {
	return c.claimAction(ctx, hubProjectID, ref, "release", req)
}

// ClaimStatus reads the authoritative hub's current issue claim state.
func (c *Client) ClaimStatus(
	ctx context.Context,
	hubProjectID int64,
	ref string,
) (ClaimStatusResponse, error) {
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(replicationDoer{c.client}))
	if err != nil {
		return ClaimStatusResponse{}, err
	}
	response, callErr := apiClient.GetIssueLeaseStatusWithResponse(ctx, &generated.GetIssueLeaseStatusRequestOptions{PathParams: &generated.GetIssueLeaseStatusPath{ProjectID: hubProjectID, Ref: ref}})
	var body ClaimStatusResponse
	if response == nil {
		return body, callErr
	}
	err = json.Unmarshal(response.Body, &body)
	return body, err
}

func (c *Client) claimAction(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	action string,
	req ClaimRequest,
) (ClaimResponse, error) {
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(replicationDoer{c.client}))
	if err != nil {
		return ClaimResponse{}, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return ClaimResponse{}, err
	}
	var payload generated.ClaimActionBody
	if err := json.Unmarshal(data, &payload); err != nil {
		return ClaimResponse{}, err
	}
	var body ClaimResponse
	switch action {
	case "acquire":
		response, callErr := apiClient.AcquireIssueLeaseWithResponse(ctx, &generated.AcquireIssueLeaseRequestOptions{PathParams: &generated.AcquireIssueLeasePath{ProjectID: hubProjectID, Ref: ref}, Body: &payload})
		if response == nil {
			return body, callErr
		}
		err = json.Unmarshal(response.Body, &body)
	case "renew":
		response, callErr := apiClient.RenewIssueLeaseWithResponse(ctx, &generated.RenewIssueLeaseRequestOptions{PathParams: &generated.RenewIssueLeasePath{ProjectID: hubProjectID, Ref: ref}, Body: &payload})
		if response == nil {
			return body, callErr
		}
		err = json.Unmarshal(response.Body, &body)
	case "release":
		response, callErr := apiClient.ReleaseIssueLeaseWithResponse(ctx, &generated.ReleaseIssueLeaseRequestOptions{PathParams: &generated.ReleaseIssueLeasePath{ProjectID: hubProjectID, Ref: ref}, Body: &payload})
		if response == nil {
			return body, callErr
		}
		err = json.Unmarshal(response.Body, &body)
	default:
		return body, fmt.Errorf("unknown claim action %q", action)
	}
	return body, err
}
