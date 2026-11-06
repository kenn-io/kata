package docbank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"uuid"
)

// ErrContentIntegrity means the origin could not prove its declared content.
var ErrContentIntegrity = errors.New("docbank: content integrity mismatch")

// OriginContent requires complete size and digest headers for an origin-routed
// response. Legacy local references retain Content's existing behavior.
func (c *Client) OriginContent(ctx context.Context, version uuid.UUID) (*http.Response, error) {
	resp, err := c.request(ctx, http.MethodGet, "/api/v1/versions/"+version.String()+"/content", nil, nil)
	if err != nil {
		return nil, err
	}
	return VerifyContentResponse(resp)
}

// VerifyContentResponse checks headers before returning a bounded streaming
// verifier. Each relay must preserve the blob hash and size for the next hop.
func VerifyContentResponse(resp *http.Response) (*http.Response, error) {
	size, sizeErr := strconv.ParseInt(resp.Header.Get("X-Docbank-Blob-Size"), 10, 64)
	digest := resp.Header.Get("X-Docbank-Blob-Hash")
	_, hashErr := hex.DecodeString(digest)
	if sizeErr != nil || size < 0 || len(digest) != sha256.Size*2 || hashErr != nil {
		_ = resp.Body.Close()
		return nil, ErrContentIntegrity
	}
	resp.ContentLength = size
	resp.Body = newContentStream(resp.Body, strings.ToLower(digest), size)
	return resp, nil
}
