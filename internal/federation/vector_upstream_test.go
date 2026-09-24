package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type memoryCredentials struct {
	config.FederationCredentialStore
	creds map[string]config.FederationCredential
}

func (m memoryCredentials) FederationCredential(_ context.Context, uid string) (config.FederationCredential, bool, error) {
	c, ok := m.creds[uid]
	return c, ok, nil
}

func TestVectorUpstreamUsesBindingTargetAndSpokeToken(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		require.NoError(t, json.NewEncoder(w).Encode(api.FederationVectorLookupBody{
			Generation: &api.FederationVectorGeneration{Fingerprint: "fp"},
		}))
	}))
	t.Cleanup(srv.Close)
	up := VectorUpstream{Credentials: memoryCredentials{creds: map[string]config.FederationCredential{
		"project-uid": {HubURL: "http://stale.invalid", HubProjectID: 1, Token: "spoke-token"},
	}}}
	body, err := up.LookupVectors(context.Background(),
		db.FederationBinding{HubURL: srv.URL, HubProjectID: 42}, "project-uid",
		api.FederationVectorLookupRequestBody{Fingerprint: "fp"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer spoke-token", gotAuth)
	assert.Equal(t, "/api/v1/projects/42/federation/vectors:lookup", gotPath, "the binding, not the credential, names the hub")
	require.NotNil(t, body.Generation)
}

func TestVectorUpstreamMapsOlderHub404ToUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	up := VectorUpstream{Credentials: memoryCredentials{creds: map[string]config.FederationCredential{
		"project-uid": {Token: "spoke-token"},
	}}}
	_, err := up.LookupVectors(context.Background(),
		db.FederationBinding{HubURL: srv.URL, HubProjectID: 42}, "project-uid",
		api.FederationVectorLookupRequestBody{Fingerprint: "fp"})
	require.ErrorIs(t, err, daemon.ErrVectorUpstreamUnsupported)
}

func TestVectorUpstreamWithoutCredentialFails(t *testing.T) {
	up := VectorUpstream{Credentials: memoryCredentials{creds: map[string]config.FederationCredential{}}}
	_, err := up.LookupVectors(context.Background(),
		db.FederationBinding{HubURL: "http://127.0.0.1:1", HubProjectID: 42}, "project-uid",
		api.FederationVectorLookupRequestBody{Fingerprint: "fp"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, daemon.ErrVectorUpstreamUnsupported, "a missing token is unreachable, not unsupported")
}
