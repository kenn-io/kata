package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
)

func TestAdoptionMetadataReader(t *testing.T) {
	const uid = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	for _, tc := range []struct {
		name, body, features string
		wantError            bool
	}{
		{"compatible", `{"project_id":41,"project_uid":"` + uid + `"}`, db.SupportedEventFeatures, false},
		{"wrong ID", `{"project_id":42,"project_uid":"` + uid + `"}`, db.SupportedEventFeatures, true},
		{"wrong UID", `{"project_id":41,"project_uid":"other"}`, db.SupportedEventFeatures, true},
		{"invalid", `{remote-secret-body`, "", true},
		{"oversized", strings.Repeat(" ", federationRebindResponseLimit) + `{"project_id":41,"project_uid":"` + uid + `"}`, db.SupportedEventFeatures, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var auth, path, features string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth, path, features = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get(db.EventFeaturesHeader)
				w.Header().Set(db.EventFeaturesHeader, tc.features)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(server.Close)
			signing := &signingpolicy.Source{KeyID: "example", HubURL: server.URL}
			credential := config.FederationCredential{HubURL: server.URL, HubProjectID: 41, Token: "enrollment-secret", Signing: signing}
			factory := func(ctx context.Context, got config.FederationCredential) (*http.Client, error) {
				require.Equal(t, credential, got)
				_, hasDeadline := ctx.Deadline()
				require.True(t, hasDeadline)
				return federationRebindTestBearerClient(t, server.Client(), server.URL, got.Token), nil
			}
			advertised, err := fetchFederationAdoptionMetadata(t.Context(), server.URL, 41, uid, credential, factory)
			if tc.wantError {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "remote-secret-body")
				require.NotContains(t, err.Error(), credential.Token)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.features, advertised)
			}
			require.Equal(t, "Bearer enrollment-secret", auth)
			require.Equal(t, "/api/v1/projects/41/federation/metadata", path)
			require.Equal(t, db.SupportedEventFeatures, features)
		})
	}
}

func TestAdoptionMetadataReaderPinsOriginAndRejectsRedirect(t *testing.T) {
	var reached bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	t.Cleanup(target.Close)
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/destination" {
			reached = true
			w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
			_, _ = fmt.Fprint(w, `{"project_id":41,"project_uid":"uid"}`)
			return
		}
		http.Redirect(w, r, "/destination", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)
	credential := config.FederationCredential{HubURL: source.URL, HubProjectID: 41, Token: "enrollment-secret"}
	called := false
	factory := func(_ context.Context, got config.FederationCredential) (*http.Client, error) {
		called = true
		return federationRebindTestBearerClient(t, source.Client(), got.HubURL, got.Token), nil
	}
	_, err := fetchFederationAdoptionMetadata(t.Context(), target.URL, 41, "uid", credential, factory)
	require.Error(t, err)
	require.False(t, called)
	_, err = fetchFederationAdoptionMetadata(t.Context(), source.URL, 41, "uid", credential, factory)
	require.Error(t, err)
	require.True(t, called)
	require.False(t, reached)
}
