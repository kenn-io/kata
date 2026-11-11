package federation_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/uid"
)

// Root enrollment identity is shared by all leaves behind a relay. The opaque
// forwarded client kind is what prevents another leaf using that root lease.
func TestRelayDocbankRootChecksExactForwardedHolder(t *testing.T) {
	root, relay, leaf, issue := newRelayClaimChain(t)
	vault := docbanktest.New(t)
	serveRelayDocbank(t, root, vault)
	status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "cli")
	require.Equal(t, http.StatusOK, status)
	require.True(t, acquired.Granted)
	otherUID, err := uid.New()
	require.NoError(t, err)
	leafBinding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
	require.NoError(t, err)
	other := db.BoundSpokeClaimPrincipal(leafBinding, db.ClaimPrincipal{HolderInstanceUID: otherUID, Holder: leaf.account, ClientKind: "cli"}, otherUID)
	relayBinding, err := relay.store.FederationBindingByProject(t.Context(), relay.project.ID)
	require.NoError(t, err)
	forwarded := db.BoundSpokeClaimPrincipal(relayBinding, other, relay.store.InstanceUID())
	query := url.Values{"project_uid": {root.project.UID}, "origin": {root.store.InstanceUID()}, "name": {"file.pdf"}, "client_kind": {forwarded.ClientKind}}
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/federation/docbank/issues/%s/uploads?%s", root.http.URL, root.project.ID, issue.UID, query.Encode())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader("fixture bytes"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+relay.credential.Token)
	resp, err := root.http.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Empty(t, vault.Requests(), "another leaf's forwarded holder must fail before Docbank I/O")
}
