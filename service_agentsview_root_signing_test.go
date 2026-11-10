package kata

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type serviceFixtureSpool struct{ *bytes.Reader }

func (*serviceFixtureSpool) Close() error { return nil }
func readServiceFixtureResponse(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	b, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return b
}

func TestServiceAgentsViewRootConsumesSnapshot(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	writes := 0
	service, err := New(t.Context(), Config{
		DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, RootSigningKey: key,
		AgentsViewRelay: &AgentsViewRelayConfig{
			Selection: func(context.Context, db.Issue, agentsviewrelay.Association, jsontext.Value) error { return nil },
			Spool: func(_ context.Context, r io.Reader, _ agentsviewrelay.Control) (AgentsViewSnapshot, error) {
				b, err := io.ReadAll(io.LimitReader(r, 1<<20))
				return &serviceFixtureSpool{bytes.NewReader(b)}, err
			},
			Ingest: func(_ context.Context, _ db.Issue, a agentsviewrelay.Association, _ io.ReadSeeker) (AgentsViewIngestAck, error) {
				writes++
				return AgentsViewIngestAck{SessionID: a.SessionID, Agent: a.Agent, Status: "created"}, nil
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)

	p, err := service.store.CreateProject(t.Context(), "associated-project")
	require.NoError(t, err)
	i, _, err := service.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Completed work", Author: "tester"})
	require.NoError(t, err)
	_, err = service.store.EnableProjectFederation(t.Context(), p.ID, "tester")
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: service.store.InstanceUID(), KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), PublicKey: key.Public().(ed25519.PublicKey)}
	require.NoError(t, service.store.PinRootAuthority(t.Context(), pin))
	body := []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`)
	h := sha256.Sum256(body)
	a := agentsviewrelay.Association{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, ServiceID: strings.Repeat("a", 64), RootKeyID: pin.KeyID, Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: hex.EncodeToString(h[:]), SnapshotBytes: int64(len(body)), LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: pin.AuthorityUID, ProjectUID: p.UID}}}
	closeBody, err := json.Marshal(map[string]any{"actor": "tester", "reason": "done", "message": "Completed the scoped example and reviewed its expected behavior.", "evidence": []map[string]any{{"type": "reviewed-paths", "paths": []string{"example.go"}}}, "agentsview_protocol": "relay-v1", "agentsview_association": a, "agentsview_discovery": map[string]any{}, "transcript": map[string]any{"agent": "pi", "session_id": "selected"}})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", server.URL, p.ID, i.ShortID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/actions/close", bytes.NewReader(closeBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	raw := readServiceFixtureResponse(t, resp)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var closed api.MutationResponse
	require.NoError(t, json.Unmarshal(raw, &closed.Body))
	require.True(t, closed.Body.Changed)
	require.NotNil(t, closed.Body.Event)
	c := agentsviewrelay.Control{Version: 1, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, IssueUID: i.UID, CloseEventUID: closed.Body.Event.UID, ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route, ClientKind: "cli"}
	control, err := json.Marshal(c)
	require.NoError(t, err)
	for _, payload := range [][]byte{bytes.ReplaceAll(body, []byte("selected"), []byte("different")), body} {
		upload, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/transcript-share", bytes.NewReader(payload))
		require.NoError(t, err)
		upload.Header.Set("Authorization", "Bearer tok")
		upload.Header.Set("X-Kata-Actor", "tester")
		upload.Header.Set("Content-Type", "application/octet-stream")
		upload.Header.Set(agentsviewrelay.ControlHeader, base64.RawURLEncoding.EncodeToString(control))
		result, err := server.Client().Do(upload)
		require.NoError(t, err)
		data := readServiceFixtureResponse(t, result)
		if !bytes.Equal(payload, body) {
			require.Equal(t, 400, result.StatusCode, string(data))
			require.Zero(t, writes)
		} else {
			require.Equal(t, 200, result.StatusCode, string(data))
			require.Equal(t, 1, writes)
			var receipt agentsviewrelay.Receipt
			require.NoError(t, json.Unmarshal(data, &receipt))
			require.NoError(t, agentsviewrelay.VerifyReceipt(receipt, key.Public().(ed25519.PublicKey)))
			require.Equal(t, c.CloseEventUID, receipt.CloseEventUID)
		}
	}
}
