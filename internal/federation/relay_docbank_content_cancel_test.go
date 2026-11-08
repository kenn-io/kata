package federation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/metadata"
)

type corruptContentResponse struct {
	http.ResponseWriter
	offset, boundary int
	changed          bool
}

func (w *corruptContentResponse) Write(p []byte) (int, error) {
	if !w.changed && w.offset+len(p) > w.boundary {
		copyOfBody := bytes.Clone(p)
		copyOfBody[max(0, w.boundary-w.offset)] ^= 1
		p = copyOfBody
		w.changed = true
	}
	n, err := w.ResponseWriter.Write(p)
	w.offset += n
	return n, err
}

type contentHostAuthority struct{ denied atomic.Bool }

func (a *contentHostAuthority) AuthorizeFederation(context.Context, daemon.HostFederationAccessRequest) (daemon.HostFederationAccessDecision, error) {
	if a.denied.Load() {
		return daemon.HostFederationAccessDecision{}, daemon.ErrHostAccessDenied
	}
	return daemon.HostFederationAccessDecision{}, nil
}

type contentAuthorityStore struct {
	db.Storage
	fail atomic.Bool
}

func (s *contentAuthorityStore) AuthorizeFederationToken(ctx context.Context, token string, projectID int64, capability string) (db.FederationEnrollment, error) {
	if s.fail.Load() {
		return db.FederationEnrollment{}, errors.New("synthetic authority lookup failure")
	}
	return s.Storage.AuthorizeFederationToken(ctx, token, projectID, capability)
}

// Canceling the leaf must retire every upstream content request. The origin
// waits after a bounded prefix so a passing test cannot rely on normal EOF.
func TestRelayDocbankContentCancellationReachesOrigin(t *testing.T) {
	testRelayDocbankContentInterruption(t, "cancel")
}

func TestRelayDocbankContentProjectRevocationStopsStream(t *testing.T) {
	testRelayDocbankContentInterruption(t, "project-revoke")
}

func TestRelayDocbankContentScopedTokenRevocationStopsStream(t *testing.T) {
	testRelayDocbankContentInterruption(t, "token-revoke")
}

func TestRelayDocbankContentEnrollmentRevocationStopsStream(t *testing.T) {
	for _, hop := range []string{"root", "relay"} {
		for _, phase := range []string{"before", "after"} {
			t.Run(hop+"-"+phase, func(t *testing.T) {
				testRelayDocbankContentInterruption(t, hop+"-enrollment-revoke-"+phase)
			})
		}
		t.Run(hop+"-empty-before", func(t *testing.T) {
			testRelayDocbankContentInterruption(t, hop+"-enrollment-revoke-empty-before")
		})
	}
}

func TestRelayDocbankContentBindingDeactivationStopsStream(t *testing.T) {
	for _, hop := range []string{"root", "relay"} {
		t.Run(hop, func(t *testing.T) {
			testRelayDocbankContentInterruption(t, hop+"-binding-disable-after")
		})
	}
}

func TestRelayDocbankContentReleaseAuthorityChanges(t *testing.T) {
	for _, action := range []string{"host-revoke", "lookup-failure", "issue-move", "subtree-move", "token-expire", "token-root-delete", "project-revoke-empty-before", "token-revoke-empty-before", "unrelated-policy", "disconnect", "relay-corrupt"} {
		t.Run(action, func(t *testing.T) { testRelayDocbankContentInterruption(t, action) })
	}
}

func testRelayDocbankContentInterruption(t *testing.T, action string) {
	t.Helper()
	root, relay, leaf, issue := newRelayClaimChain(t)
	revokeTransport := func() {
		t.Helper()
		require.True(t, strings.Contains(action, "-enrollment-revoke-") || strings.Contains(action, "-binding-disable-"), "unknown transport fixture action: %s", action)
		hub, child := root, relay
		if strings.HasPrefix(action, "relay-") {
			hub, child = relay, leaf
		}
		if strings.Contains(action, "-binding-disable-") {
			binding, err := hub.store.FederationBindingByProject(t.Context(), hub.project.ID)
			require.NoError(t, err)
			binding.Enabled = false
			_, err = hub.store.UpsertFederationBinding(t.Context(), binding)
			require.NoError(t, err)
			return
		}
		policyBefore, err := hub.store.ProjectAccessRevision(t.Context())
		require.NoError(t, err)
		enrollments, err := hub.store.ListFederationEnrollments(t.Context())
		require.NoError(t, err)
		found := false
		for _, enrollment := range enrollments {
			if enrollment.SpokeInstanceUID == child.store.InstanceUID() && enrollment.ProjectID != nil && *enrollment.ProjectID == hub.project.ID {
				require.NoError(t, hub.store.RevokeFederationEnrollment(t.Context(), enrollment.ID))
				found = true
			}
		}
		require.True(t, found, "fixture must revoke the admitted transport credential")
		policyAfter, err := hub.store.ProjectAccessRevision(t.Context())
		require.NoError(t, err)
		require.Equal(t, policyBefore, policyAfter, "enrollment revocation must not rely on a project policy revision")
	}
	version := uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222219")
	data := append(bytes.Repeat([]byte("a"), 256<<10), bytes.Repeat([]byte("S"), 768<<10)...)
	if strings.Contains(action, "-empty-") {
		data = nil
	}
	prefix := min(len(data), 256<<10)
	releaseRest := make(chan struct{})
	allowPrefix := make(chan struct{})
	originStarted := make(chan struct{})
	beforeRelease := strings.HasSuffix(action, "-before")
	sum := sha256.Sum256(data)
	canceled := make(chan struct{})
	stopFixture := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			http.Error(w, "fixture auth", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/content"):
			close(originStarted)
			if beforeRelease {
				select {
				case <-allowPrefix:
				case <-r.Context().Done():
					return
				case <-stopFixture:
					return
				}
			}
			w.Header().Set("X-Docbank-Blob-Hash", hex.EncodeToString(sum[:]))
			w.Header().Set("X-Docbank-Blob-Size", fmt.Sprint(len(data)))
			if _, err := w.Write(data[:prefix]); err != nil {
				once.Do(func() { close(canceled) })
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				once.Do(func() { close(canceled) })
			case <-stopFixture:
			case <-releaseRest:
				_, _ = w.Write(data[prefix:])
			}
		case strings.HasPrefix(r.URL.Path, "/api/v1/versions/"):
			_ = json.MarshalWrite(w, docbank.Version{ID: version, NodeID: 42, Size: int64(len(data))})
		case r.URL.Path == "/api/v1/nodes/42":
			_ = json.MarshalWrite(w, docbank.Node{ID: 42, Path: "/kata/file.pdf", Name: "file.pdf", Kind: "file", Size: int64(len(data)), CurrentVersionID: version})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { close(stopFixture); upstream.Close() })
	host := &contentHostAuthority{}
	authorityStore := &contentAuthorityStore{Storage: root.store}
	server := daemon.NewServer(daemon.ServerConfig{DB: authorityStore, HostFederationAccess: host, FederationCredentials: root.credentials, RootSigning: daemon.NewRootSigning(root.signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, Docbank: docbank.NewClient(upstream.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler := server.Handler()
	root.handler.Store(&handler)
	if action == "relay-corrupt" {
		original := *relay.handler.Load()
		var intercept http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/content") {
				w = &corruptContentResponse{ResponseWriter: w, boundary: prefix}
			}
			original.ServeHTTP(w, r)
		})
		relay.handler.Store(&intercept)
	}
	reader := leaf
	tokenText := leaf.userToken
	var scopedToken db.APIToken
	var scopedExpires time.Time
	scopeRoot := issue
	rootCtx := db.WithRootAttribution(t.Context(), root.signer, root.account)
	if strings.HasPrefix(action, "token-") || action == "subtree-move" {
		// Native subtree tokens are deliberately unavailable on spokes. Exercise
		// that independent authorization layer at the hub's public content route.
		reader = root
		tokenText = "content-subtree-test-token"
		scopedExpires = time.Now().Add(time.Hour)
		if action == "token-expire" {
			scopedExpires = time.Now().Add(time.Second)
		}
		if action == "subtree-move" {
			child, _, err := reader.store.CreateIssue(rootCtx, db.CreateIssueParams{ProjectID: reader.project.ID, Title: "content child", Author: reader.account})
			require.NoError(t, err)
			_, err = reader.store.EditIssueAtomic(rootCtx, db.EditIssueAtomicParams{IssueID: child.ID, Actor: reader.account, SetParent: &scopeRoot.ID})
			require.NoError(t, err)
			issue = child
		}
		var err error
		scopedToken, _, err = reader.store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
			PlaintextToken: tokenText, Actor: reader.account, AdminActor: "admin",
			Scope:     &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: reader.project.UID, RootIssueUID: scopeRoot.UID},
			ExpiresAt: &scopedExpires,
		})
		require.NoError(t, err)
	}
	if action == "issue-move" {
		// Moving out of a federated project is explicitly unsupported. Exercise
		// this release invariant on a native project with an own-origin file.
		local, err := root.store.CreateProject(t.Context(), "local-project")
		require.NoError(t, err)
		selected, _, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: local.ID, Title: "movable content", Author: root.account})
		require.NoError(t, err)
		root.project, issue = local, selected
		reader, tokenText = root, root.userToken
	}
	changeAuthority := func() {
		t.Helper()
		switch {
		case strings.HasPrefix(action, "project-revoke"):
			team, _, err := reader.store.CreateTeam(t.Context(), "other-team", "admin")
			require.NoError(t, err)
			_, _, err = reader.store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: reader.project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
			require.NoError(t, err)
		case strings.HasPrefix(action, "token-revoke"):
			_, _, err := reader.store.RevokeAPIToken(t.Context(), scopedToken.ID, "admin")
			require.NoError(t, err)
		case action == "token-expire":
			timer := time.NewTimer(time.Until(scopedExpires))
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-t.Context().Done():
				t.Fatal("fixture expired while waiting for token deadline")
			}
		case action == "token-root-delete":
			_, _, _, err := reader.store.SoftDeleteIssue(rootCtx, scopeRoot.ID, reader.account)
			require.NoError(t, err)
		case action == "subtree-move":
			_, err := reader.store.EditIssueAtomic(rootCtx, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: reader.account, RemoveParent: &scopeRoot.ID})
			require.NoError(t, err)
		case action == "issue-move", action == "unrelated-policy":
			other, err := reader.store.CreateProject(t.Context(), "other-project")
			require.NoError(t, err)
			team, _, err := reader.store.CreateTeam(t.Context(), "other-team", "admin")
			require.NoError(t, err)
			_, _, err = reader.store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: other.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
			require.NoError(t, err)
			if action == "issue-move" {
				current, err := reader.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				_, err = reader.store.MoveIssueProject(rootCtx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: reader.project.ID, ToProjectID: other.ID, IfMatchRev: current.Revision, Actor: reader.account})
				require.NoError(t, err)
			}
		case action == "host-revoke":
			host.denied.Store(true)
		case action == "lookup-failure":
			authorityStore.fail.Store(true)
		default:
			revokeTransport()
		}
	}
	raw, err := json.Marshal(metadata.Attachment{Origin: root.store.InstanceUID(), Name: "file.pdf", MediaType: "application/pdf", Size: int64(len(data)), DocbankVersion: version})
	require.NoError(t, err)
	_, err = reader.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issueIDForRelayMatrix(t, reader, issue.UID), Actor: reader.account, Patch: map[string]jsontext.Value{"attachment.cancel": raw}})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/attachments/cancel/content", reader.http.URL, reader.project.ID, issue.UID), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tokenText)
	if beforeRelease {
		type result struct {
			response *http.Response
			err      error
		}
		completed := make(chan result, 1)
		go func() {
			response, err := reader.http.Client().Do(req)
			completed <- result{response: response, err: err}
		}()
		select {
		case <-originStarted:
		case <-ctx.Done():
			t.Fatal("origin fixture did not admit the content request")
		}
		changeAuthority()
		close(allowPrefix)
		close(releaseRest)
		got := <-completed
		require.NoError(t, got.err)
		defer func() { _ = got.response.Body.Close() }()
		body, err := io.ReadAll(got.response.Body)
		require.NoError(t, err)
		want := http.StatusForbidden
		if strings.HasPrefix(action, "project-revoke") {
			want = http.StatusNotFound
		} else if strings.HasPrefix(action, "token-revoke") {
			want = http.StatusUnauthorized
		}
		require.Equal(t, want, got.response.StatusCode, "response bytes=%d", len(body))
		require.Empty(t, got.response.Header.Get("X-Docbank-Blob-Hash"))
		require.Empty(t, got.response.Header.Get("X-Docbank-Blob-Size"))
		require.NotContains(t, string(body), "SSSS")
		return
	}
	resp, err := reader.http.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var first [1]byte
	_, err = io.ReadFull(resp.Body, first[:])
	require.NoError(t, err)
	require.Equal(t, byte('a'), first[0])
	switch action {
	case "cancel", "disconnect":
		if action == "cancel" {
			cancel()
			_, err = io.Copy(io.Discard, resp.Body)
			require.Error(t, err)
		} else {
			require.NoError(t, resp.Body.Close())
		}
		select {
		case <-canceled:
		case <-time.After(2 * time.Second):
			t.Fatal("origin content request survived downstream interruption")
		}
	default:
		if action != "relay-corrupt" {
			changeAuthority()
		}
		close(releaseRest)
		remaining, err := io.ReadAll(resp.Body)
		if action == "unrelated-policy" {
			require.NoError(t, err)
			require.Equal(t, data[1:], remaining)
			return
		}
		require.Error(t, err, "revoked authority must abort the declared-length response")
		if action != "relay-corrupt" {
			require.NotContains(t, string(remaining), "S", "no post-revocation content may be released")
		}
		require.Less(t, len(remaining)+1, len(data))
	}
}
