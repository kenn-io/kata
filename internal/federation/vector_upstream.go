package federation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// VectorUpstream is the daemon.VectorUpstream that reaches a spoke binding's
// hub over the enrollment-authenticated vectors:lookup route, with the same
// credential and transport resolution the sync runner uses.
type VectorUpstream struct {
	// Credentials resolves the spoke's enrollment token. Nil uses the
	// default credential store, like Runner.
	Credentials config.FederationCredentialStore
	Opts        clientpkg.Opts
}

var _ daemon.VectorUpstream = VectorUpstream{}

// LookupVectors implements daemon.VectorUpstream. A 404 from a hub that
// predates the route is reported as daemon.ErrVectorUpstreamUnsupported.
func (u VectorUpstream) LookupVectors(
	ctx context.Context,
	binding db.FederationBinding,
	projectUID string,
	request api.FederationVectorLookupRequestBody,
) (api.FederationVectorLookupBody, error) {
	store := u.Credentials
	if store == nil {
		store = config.DefaultFederationCredentialStore()
	}
	cred, found, err := store.FederationCredential(ctx, projectUID)
	if err != nil {
		return api.FederationVectorLookupBody{}, err
	}
	if !found || strings.TrimSpace(cred.Token) == "" {
		return api.FederationVectorLookupBody{}, errors.New("federation credential missing")
	}
	cred = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, cred)
	client, err := NewClient(ctx, cred.HubURL, cred.Token, clientOptsForCredential(u.Opts, cred))
	if err != nil {
		return api.FederationVectorLookupBody{}, err
	}
	body, err := client.LookupVectors(ctx, cred.HubProjectID, request)
	var statusErr *HubStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return body, fmt.Errorf("%w: %w", daemon.ErrVectorUpstreamUnsupported, err)
	}
	return body, err
}
