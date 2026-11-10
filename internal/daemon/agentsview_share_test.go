package daemon_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestAgentsViewAssociationRequiresDiscoveryAdapter(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p, err := env.DB.CreateProject(t.Context(), "associated-project")
	require.NoError(t, err)
	i, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "completed work", Author: "tester"})
	require.NoError(t, err)
	a := agentsviewrelay.Association{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: env.DB.InstanceUID(), ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 123, LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: env.DB.InstanceUID(), ProjectUID: p.UID}}}
	require.NoError(t, a.Validate())
	body, err := json.Marshal(map[string]any{"actor": "tester", "reason": "done", "message": "Completed the scoped work and reviewed its expected behavior.", "evidence": []map[string]any{{"type": "reviewed-paths", "paths": []string{"example.go"}}}, "agentsview_protocol": "relay-v1", "agentsview_association": a, "agentsview_discovery": map[string]any{}, "transcript": map[string]any{"agent": "pi", "session_id": "selected"}})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", env.URL, p.ID, i.ShortID), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "agentsview_unavailable")
	current, err := env.DB.IssueByID(t.Context(), i.ID)
	require.NoError(t, err)
	require.Equal(t, "open", current.Status)
	events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: p.ID, IssueUID: i.UID, Types: []string{"issue.closed"}, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, events)
}

func TestAgentsViewUploadWithoutTransportDoesNotMutateIssue(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p, err := env.DB.CreateProject(t.Context(), "share-project")
	require.NoError(t, err)
	i, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "completed work", Author: "tester"})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/transcript-share", env.URL, p.ID, i.ShortID), strings.NewReader("not a snapshot"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/octet-stream")
	control := agentsviewrelay.Control{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: env.DB.InstanceUID(), CloseEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", ServiceID: strings.Repeat("a", 64), Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 14, Route: []agentsviewrelay.RouteObservation{{InstanceUID: env.DB.InstanceUID(), ProjectUID: p.UID}}, ClientKind: "cli"}
	rawControl, err := json.Marshal(control)
	require.NoError(t, err)
	_, err = agentsviewrelay.DecodeControl(rawControl)
	require.NoError(t, err)
	req.Header.Set(agentsviewrelay.ControlHeader, base64.RawURLEncoding.EncodeToString(rawControl))
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	current, err := env.DB.IssueByID(t.Context(), i.ID)
	require.NoError(t, err)
	require.Equal(t, "open", current.Status)
}

type agentsViewFixtureSpool struct{ *bytes.Reader }

func (*agentsViewFixtureSpool) Close() error { return nil }

// This fixture composes the real native close producer and upload consumer.
// Only policy/spool/receiver dependency ports are synthetic; it is not full
// signed multi-hop/AgentsView codec interoperability evidence.
func TestAgentsViewRootConsumesOnlyOriginalAssociatedSnapshot(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	writes := 0
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) {
		cfg.RootSigning = daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: cfg.DB.InstanceUID(), PrivateKey: key})
		cfg.AgentsViewRelay = &daemon.AgentsViewRelayConfig{
			Selection: func(context.Context, db.Issue, agentsviewrelay.Association, jsontext.Value) error { return nil },
			Spool: func(_ context.Context, r io.Reader, _ agentsviewrelay.Control) (daemon.AgentsViewSnapshot, error) {
				b, err := io.ReadAll(io.LimitReader(r, 1<<20))
				return &agentsViewFixtureSpool{bytes.NewReader(b)}, err
			},
			Ingest: func(_ context.Context, _ db.Issue, a agentsviewrelay.Association, _ io.ReadSeeker) (daemon.AgentsViewIngestAck, error) {
				writes++
				return daemon.AgentsViewIngestAck{SessionID: a.SessionID, Agent: a.Agent, Status: "created"}, nil
			},
		}
	})
	p, err := env.DB.CreateProject(t.Context(), "associated-project")
	require.NoError(t, err)
	i, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Completed work", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(t.Context(), p.ID, "tester")
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: env.DB.InstanceUID(), KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), PublicKey: key.Public().(ed25519.PublicKey)}
	require.NoError(t, env.DB.PinRootAuthority(t.Context(), pin))
	body := []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`)
	h := sha256.Sum256(body)
	a := agentsviewrelay.Association{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, ServiceID: strings.Repeat("a", 64), RootKeyID: pin.KeyID, Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: hex.EncodeToString(h[:]), SnapshotBytes: int64(len(body)), LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: pin.AuthorityUID, ProjectUID: p.UID}}}
	closeBody, err := json.Marshal(map[string]any{"actor": "tester", "reason": "done", "message": "Completed the scoped example and reviewed its expected behavior.", "evidence": []map[string]any{{"type": "reviewed-paths", "paths": []string{"example.go"}}}, "agentsview_protocol": "relay-v1", "agentsview_association": a, "agentsview_discovery": map[string]any{}, "transcript": map[string]any{"agent": "pi", "session_id": "selected"}})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", env.URL, p.ID, i.ShortID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/actions/close", bytes.NewReader(closeBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
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
		result, err := env.HTTP.Do(upload)
		require.NoError(t, err)
		data := readClose(t, result)
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

func TestAgentsViewForgedMemberCannotReachReceiver(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	writes := 0
	var receivedBytes int64
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) {
		cfg.RootSigning = daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: cfg.DB.InstanceUID(), PrivateKey: key})
		cfg.AgentsViewRelay = &daemon.AgentsViewRelayConfig{
			Selection: func(context.Context, db.Issue, agentsviewrelay.Association, jsontext.Value) error { return nil },
			Spool: func(_ context.Context, r io.Reader, _ agentsviewrelay.Control) (daemon.AgentsViewSnapshot, error) {
				b, err := io.ReadAll(io.LimitReader(r, 1<<20))
				return &agentsViewFixtureSpool{bytes.NewReader(b)}, err
			},
			Ingest: func(_ context.Context, _ db.Issue, a agentsviewrelay.Association, payload io.ReadSeeker) (daemon.AgentsViewIngestAck, error) {
				writes++
				var err error
				receivedBytes, err = io.Copy(io.Discard, payload)
				if err != nil {
					return daemon.AgentsViewIngestAck{}, err
				}
				return daemon.AgentsViewIngestAck{SessionID: a.SessionID, Agent: a.Agent, Status: "created"}, nil
			},
		}
	})
	p, err := env.DB.CreateProject(t.Context(), "associated-project")
	require.NoError(t, err)
	i, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Completed work", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(t.Context(), p.ID, "tester")
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: env.DB.InstanceUID(), KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), PublicKey: key.Public().(ed25519.PublicKey)}
	require.NoError(t, env.DB.PinRootAuthority(t.Context(), pin))
	body := []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:other","agent":"pi"}},"manifest/session/id":"pi:selected","segments":[]}`)
	h := sha256.Sum256(body)
	a := agentsviewrelay.Association{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, ServiceID: strings.Repeat("a", 64), RootKeyID: pin.KeyID, Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: hex.EncodeToString(h[:]), SnapshotBytes: int64(len(body)), LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: pin.AuthorityUID, ProjectUID: p.UID}}}
	closeBody, err := json.Marshal(map[string]any{"actor": "tester", "reason": "done", "message": "Completed the scoped example and reviewed its expected behavior.", "evidence": []map[string]any{{"type": "reviewed-paths", "paths": []string{"example.go"}}}, "agentsview_protocol": "relay-v1", "agentsview_association": a, "agentsview_discovery": map[string]any{}, "transcript": map[string]any{"agent": "pi", "session_id": "selected"}})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", env.URL, p.ID, i.ShortID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/actions/close", bytes.NewReader(closeBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var closed api.MutationResponse
	require.NoError(t, json.Unmarshal(raw, &closed.Body))
	require.True(t, closed.Body.Changed)
	require.NotNil(t, closed.Body.Event)
	c := agentsviewrelay.Control{Version: 1, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, IssueUID: i.UID, CloseEventUID: closed.Body.Event.UID, ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route, ClientKind: "cli"}
	control, err := json.Marshal(c)
	require.NoError(t, err)
	upload, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/transcript-share", bytes.NewReader(body))
	require.NoError(t, err)
	upload.Header.Set("Authorization", "Bearer tok")
	upload.Header.Set("X-Kata-Actor", "tester")
	upload.Header.Set("Content-Type", "application/octet-stream")
	upload.Header.Set(agentsviewrelay.ControlHeader, base64.RawURLEncoding.EncodeToString(control))
	result, err := env.HTTP.Do(upload)
	require.NoError(t, err)
	data := readClose(t, result)
	require.Equal(t, 400, result.StatusCode, string(data))
	require.Zero(t, writes, "forged identity must not reach the receiver")
	require.Zero(t, receivedBytes, "forged identity must not send bytes to the receiver")
}

func TestAgentsViewRevokedTokenCannotReachReceiver(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	writes := 0
	var store db.Storage
	var tokenID int64
	env := testenv.New(t, testenv.WithAuthToken("tok"), testenv.WithRequireTokenIdentity(), func(cfg *daemon.ServerConfig) {
		cfg.RootSigning = daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: cfg.DB.InstanceUID(), PrivateKey: key})
		cfg.AgentsViewRelay = &daemon.AgentsViewRelayConfig{
			Selection: func(context.Context, db.Issue, agentsviewrelay.Association, jsontext.Value) error { return nil },
			Spool: func(_ context.Context, r io.Reader, _ agentsviewrelay.Control) (daemon.AgentsViewSnapshot, error) {
				b, err := io.ReadAll(io.LimitReader(r, 1<<20))
				_, _, revokeErr := store.RevokeAPIToken(t.Context(), tokenID, "admin")
				if revokeErr != nil {
					return nil, revokeErr
				}
				return &agentsViewFixtureSpool{bytes.NewReader(b)}, err
			},
			Ingest: func(_ context.Context, _ db.Issue, a agentsviewrelay.Association, _ io.ReadSeeker) (daemon.AgentsViewIngestAck, error) {
				writes++
				return daemon.AgentsViewIngestAck{SessionID: a.SessionID, Agent: a.Agent, Status: "created"}, nil
			},
		}
	})
	store = env.DB
	token, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "upload-token", Actor: "tester", AdminActor: "admin"})
	require.NoError(t, err)
	tokenID = token.ID
	p, err := env.DB.CreateProject(t.Context(), "associated-project")
	require.NoError(t, err)
	i, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Completed work", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(t.Context(), p.ID, "tester")
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: env.DB.InstanceUID(), KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), PublicKey: key.Public().(ed25519.PublicKey)}
	require.NoError(t, env.DB.PinRootAuthority(t.Context(), pin))
	body := []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`)
	h := sha256.Sum256(body)
	a := agentsviewrelay.Association{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, ServiceID: strings.Repeat("a", 64), RootKeyID: pin.KeyID, Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: hex.EncodeToString(h[:]), SnapshotBytes: int64(len(body)), LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: pin.AuthorityUID, ProjectUID: p.UID}}}
	closeBody, err := json.Marshal(map[string]any{"actor": "tester", "reason": "done", "message": "Completed the scoped example and reviewed its expected behavior.", "evidence": []map[string]any{{"type": "reviewed-paths", "paths": []string{"example.go"}}}, "agentsview_protocol": "relay-v1", "agentsview_association": a, "agentsview_discovery": map[string]any{}, "transcript": map[string]any{"agent": "pi", "session_id": "selected"}})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", env.URL, p.ID, i.ShortID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/actions/close", bytes.NewReader(closeBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer upload-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var closed api.MutationResponse
	require.NoError(t, json.Unmarshal(raw, &closed.Body))
	require.True(t, closed.Body.Changed)
	require.NotNil(t, closed.Body.Event)
	c := agentsviewrelay.Control{Version: 1, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, IssueUID: i.UID, CloseEventUID: closed.Body.Event.UID, ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route, ClientKind: "cli"}
	control, err := json.Marshal(c)
	require.NoError(t, err)
	upload, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/transcript-share", bytes.NewReader(body))
	require.NoError(t, err)
	upload.Header.Set("Authorization", "Bearer upload-token")
	upload.Header.Set("X-Kata-Actor", "tester")
	upload.Header.Set("Content-Type", "application/octet-stream")
	upload.Header.Set(agentsviewrelay.ControlHeader, base64.RawURLEncoding.EncodeToString(control))
	result, err := env.HTTP.Do(upload)
	require.NoError(t, err)
	data := readClose(t, result)
	require.Equal(t, 401, result.StatusCode, string(data))
	require.Zero(t, writes, "revoked token must not reach the receiver")
}

func TestAgentsViewMissingRootReceiverDoesNotSpool(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	spools := 0
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) {
		cfg.RootSigning = daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: cfg.DB.InstanceUID(), PrivateKey: key})
		cfg.AgentsViewRelay = &daemon.AgentsViewRelayConfig{
			Selection: func(context.Context, db.Issue, agentsviewrelay.Association, jsontext.Value) error { return nil },
			Spool: func(_ context.Context, r io.Reader, _ agentsviewrelay.Control) (daemon.AgentsViewSnapshot, error) {
				spools++
				b, err := io.ReadAll(io.LimitReader(r, 1<<20))
				return &agentsViewFixtureSpool{bytes.NewReader(b)}, err
			},
		}
	})
	p, err := env.DB.CreateProject(t.Context(), "associated-project")
	require.NoError(t, err)
	i, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Completed work", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(t.Context(), p.ID, "tester")
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: env.DB.InstanceUID(), KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), PublicKey: key.Public().(ed25519.PublicKey)}
	require.NoError(t, env.DB.PinRootAuthority(t.Context(), pin))
	body := []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`)
	h := sha256.Sum256(body)
	a := agentsviewrelay.Association{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, ServiceID: strings.Repeat("a", 64), RootKeyID: pin.KeyID, Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: hex.EncodeToString(h[:]), SnapshotBytes: int64(len(body)), LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: pin.AuthorityUID, ProjectUID: p.UID}}}
	closeBody, err := json.Marshal(map[string]any{"actor": "tester", "reason": "done", "message": "Completed the scoped example and reviewed its expected behavior.", "evidence": []map[string]any{{"type": "reviewed-paths", "paths": []string{"example.go"}}}, "agentsview_protocol": "relay-v1", "agentsview_association": a, "agentsview_discovery": map[string]any{}, "transcript": map[string]any{"agent": "pi", "session_id": "selected"}})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", env.URL, p.ID, i.ShortID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/actions/close", bytes.NewReader(closeBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var closed api.MutationResponse
	require.NoError(t, json.Unmarshal(raw, &closed.Body))
	require.True(t, closed.Body.Changed)
	require.NotNil(t, closed.Body.Event)
	c := agentsviewrelay.Control{Version: 1, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, IssueUID: i.UID, CloseEventUID: closed.Body.Event.UID, ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route, ClientKind: "cli"}
	control, err := json.Marshal(c)
	require.NoError(t, err)
	upload, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/transcript-share", bytes.NewReader(body))
	require.NoError(t, err)
	upload.Header.Set("Authorization", "Bearer tok")
	upload.Header.Set("X-Kata-Actor", "tester")
	upload.Header.Set("Content-Type", "application/octet-stream")
	upload.Header.Set(agentsviewrelay.ControlHeader, base64.RawURLEncoding.EncodeToString(control))
	result, err := env.HTTP.Do(upload)
	require.NoError(t, err)
	data := readClose(t, result)
	require.Equal(t, 503, result.StatusCode, string(data))
	require.Zero(t, spools)
}

func TestAgentsViewBrowserLogoutDuringSpoolRefusesDispatch(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	writes := 0
	spooled := 0
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) {
		cfg.RootSigning = daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: cfg.DB.InstanceUID(), PrivateKey: key})
		cfg.AgentsViewRelay = &daemon.AgentsViewRelayConfig{
			Selection: func(context.Context, db.Issue, agentsviewrelay.Association, jsontext.Value) error { return nil },
			Spool: func(_ context.Context, r io.Reader, _ agentsviewrelay.Control) (daemon.AgentsViewSnapshot, error) {
				b, err := io.ReadAll(io.LimitReader(r, 1<<20))
				return &agentsViewFixtureSpool{bytes.NewReader(b)}, err
			},
			Ingest: func(_ context.Context, _ db.Issue, a agentsviewrelay.Association, _ io.ReadSeeker) (daemon.AgentsViewIngestAck, error) {
				writes++
				return daemon.AgentsViewIngestAck{SessionID: a.SessionID, Agent: a.Agent, Status: "created"}, nil
			},
		}
	})
	p, err := env.DB.CreateProject(t.Context(), "associated-project")
	require.NoError(t, err)
	i, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Completed work", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(t.Context(), p.ID, "tester")
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: env.DB.InstanceUID(), KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), PublicKey: key.Public().(ed25519.PublicKey)}
	require.NoError(t, env.DB.PinRootAuthority(t.Context(), pin))
	body := []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`)
	h := sha256.Sum256(body)
	a := agentsviewrelay.Association{Version: 1, IssueUID: i.UID, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, ServiceID: strings.Repeat("a", 64), RootKeyID: pin.KeyID, Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: hex.EncodeToString(h[:]), SnapshotBytes: int64(len(body)), LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: pin.AuthorityUID, ProjectUID: p.UID}}}
	closeBody, err := json.Marshal(map[string]any{"actor": "tester", "reason": "done", "message": "Completed the scoped example and reviewed its expected behavior.", "evidence": []map[string]any{{"type": "reviewed-paths", "paths": []string{"example.go"}}}, "agentsview_protocol": "relay-v1", "agentsview_association": a, "agentsview_discovery": map[string]any{}, "transcript": map[string]any{"agent": "pi", "session_id": "selected"}})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", env.URL, p.ID, i.ShortID)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path+"/actions/close", bytes.NewReader(closeBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var closed api.MutationResponse
	require.NoError(t, json.Unmarshal(raw, &closed.Body))
	require.True(t, closed.Body.Changed)
	require.NotNil(t, closed.Body.Event)
	c := agentsviewrelay.Control{Version: 1, ProjectUID: p.UID, AuthorityUID: pin.AuthorityUID, IssueUID: i.UID, CloseEventUID: closed.Body.Event.UID, ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route, ClientKind: "cli"}
	control, err := json.Marshal(c)
	require.NoError(t, err)

	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{Origin: "http://127.0.0.1:27123", InstanceID: "instance_a", Writable: true, Auth: config.AuthConfig{Token: "tok"}, DB: env.DB})
	require.NoError(t, err)
	issued, err := manager.IssueSession(daemon.Principal{Kind: daemon.PrincipalStaticToken}, "/kata")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, Auth: config.AuthConfig{Token: "tok"}, WebSessions: manager, RootSigning: daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: env.DB.InstanceUID(), PrivateKey: key}), AgentsViewRelay: &daemon.AgentsViewRelayConfig{
		Selection: func(context.Context, db.Issue, agentsviewrelay.Association, jsontext.Value) error { return nil },
		Spool: func(_ context.Context, r io.Reader, _ agentsviewrelay.Control) (daemon.AgentsViewSnapshot, error) {
			spooled++
			b, err := io.ReadAll(r)
			manager.Logout(issued.Session)
			return &agentsViewFixtureSpool{bytes.NewReader(b)}, err
		},
		Ingest: func(_ context.Context, _ db.Issue, a agentsviewrelay.Association, _ io.ReadSeeker) (daemon.AgentsViewIngestAck, error) {
			writes++
			return daemon.AgentsViewIngestAck{SessionID: a.SessionID, Agent: a.Agent, Status: "created"}, nil
		},
	}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler, err := server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true})
	require.NoError(t, err)
	upload := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("http://127.0.0.1:27123/api/v1/projects/%d/issues/%s/transcript-share", p.ID, i.ShortID), bytes.NewReader(body))
	upload.Host = "127.0.0.1:27123"
	upload.Header.Set("Origin", "http://127.0.0.1:27123")
	upload.Header.Set("X-Kata-Web-Session", issued.Session)
	upload.Header.Set("X-Kata-CSRF", issued.CSRF)
	upload.Header.Set("X-Kata-Actor", "tester")
	upload.Header.Set("Content-Type", "application/octet-stream")
	upload.Header.Set(agentsviewrelay.ControlHeader, base64.RawURLEncoding.EncodeToString(control))
	upload.AddCookie(manager.Cookie(issued.Cookie))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, upload)
	require.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "web_session_required")
	require.Equal(t, 1, spooled)
	require.Zero(t, writes)
}
