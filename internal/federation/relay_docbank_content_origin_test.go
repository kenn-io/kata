package federation_test

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"hegel.dev/go/hegel"
)

func TestRelayFederatedDocbankContentRejectsIntermediateOrigin(t *testing.T) {
	_, relay, leaf, _, relayVault, version := newRelayDocbankContentOriginFixture(t)

	status, body := requestRelayFederatedDocbankContent(t, relay, leaf, version, relay.store.InstanceUID())

	require.Equal(t, http.StatusConflict, status, string(body))
	require.Contains(t, string(body), "attachment_origin_mismatch")
	require.Empty(t, relayVault.Requests(), "a relay must not serve its local Docbank for a project rooted upstream")
}

func TestRelayFederatedDocbankContentUsesOnlyConfiguredOriginProperty(t *testing.T) {
	root, relay, leaf, _, relayVault, version := newRelayDocbankContentOriginFixture(t)
	rootOrigin := root.store.InstanceUID()
	origins := []string{rootOrigin, relay.store.InstanceUID()}

	hegel.Test(t, func(ht *hegel.T) {
		origin := hegel.Draw(ht, hegel.SampledFrom(origins))
		status, body := requestRelayFederatedDocbankContent(t, relay, leaf, version, origin)
		if origin == rootOrigin {
			require.Equal(t, http.StatusOK, status, string(body))
			require.Equal(t, "root content", string(body))
		} else {
			require.Equal(t, http.StatusConflict, status, string(body))
			require.Contains(t, string(body), "attachment_origin_mismatch")
		}
	}, hegel.WithTestCases(2048))

	require.Empty(t, relayVault.Requests(), "the configured upstream origin is the only content source")
}

func newRelayDocbankContentOriginFixture(t *testing.T) (root, relay, leaf *relayMatrixNode, rootVault, relayVault *docbanktest.Fake, version uuid.UUID) {
	t.Helper()
	root, relay, leaf, _ = newRelayClaimChain(t)
	rootVault, relayVault = docbanktest.New(t), docbanktest.New(t)
	version = uuid.MustParse("6f1c2e8a-7777-4777-8777-777777777719")
	rootVault.Seed("/kata/root.pdf", docbank.Node{ID: 42, Name: "root.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("root content")})
	relayVault.Seed("/kata/intermediate.pdf", docbank.Node{ID: 43, Name: "intermediate.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("intermediate content")})
	serveRelayDocbank(t, root, rootVault)
	serveRelayDocbank(t, relay, relayVault)
	return root, relay, leaf, rootVault, relayVault, version
}

func requestRelayFederatedDocbankContent(t *testing.T, relay, leaf *relayMatrixNode, version uuid.UUID, origin string) (int, []byte) {
	t.Helper()
	query := url.Values{"project_uid": {relay.project.UID}, "origin": {origin}}
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/federation/docbank/versions/%s/content?%s", relay.http.URL, relay.project.ID, version.String(), query.Encode())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+leaf.credential.Token)
	resp, err := relay.http.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}
