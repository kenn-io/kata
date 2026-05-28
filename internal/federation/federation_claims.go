package federation

import (
	"context"
	"fmt"
	"net/url"

	"go.kenn.io/kata/internal/api"
)

// ClaimRequest is the wire body for federation claim actions.
type ClaimRequest = api.ClaimActionBody

// ClaimResponse is the wire body returned by federation claim actions.
type ClaimResponse = api.ClaimActionResponseBody

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
	var body ClaimStatusResponse
	err := c.getJSON(ctx, claimPath(hubProjectID, ref, "lease"), &body)
	return body, err
}

func (c *Client) claimAction(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	action string,
	req ClaimRequest,
) (ClaimResponse, error) {
	var body ClaimResponse
	err := c.postJSON(ctx, claimPath(hubProjectID, ref, "lease/actions/"+action), req, &body)
	return body, err
}

func claimPath(hubProjectID int64, ref, suffix string) string {
	return fmt.Sprintf("/api/v1/projects/%d/issues/%s/%s", hubProjectID, url.PathEscape(ref), suffix)
}
