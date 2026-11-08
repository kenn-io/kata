package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// Credentials for a former target may not leave the daemon through either
// capability preflight or the ordinary lease refresh that precedes a write.
func TestDocbankOriginRejectsMismatchedCredentialTarget(t *testing.T) {
	for _, mismatch := range []string{"origin", "project"} {
		for _, operation := range []string{"upload", "link", "content"} {
			t.Run(mismatch+"/"+operation, func(t *testing.T) {
				var calls atomic.Int32
				receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					_, _ = io.WriteString(w, `{}`)
				}))
				t.Cleanup(receiver.Close)
				credentials := newReplicaCredentialStore()
				env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.FederationCredentials = credentials })
				project, issue := seedProjectAndIssue(t, env)
				origin, err := uid.New()
				require.NoError(t, err)
				attachmentID, err := uid.New()
				require.NoError(t, err)
				raw := jsontext.Value(fmt.Sprintf(`{"name":"file.pdf","media_type":"application/pdf","size":7,"docbank_version":%q,"origin":%q}`, linkVersion, origin))
				_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment." + attachmentID: raw}})
				require.NoError(t, err)
				_, err = env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: receiver.URL, HubProjectID: 42, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true})
				require.NoError(t, err)
				credential := config.FederationCredential{HubURL: receiver.URL, HubProjectID: 42, Token: "fixture-enrollment", Actor: "operator", Capabilities: "pull,push,claim", AllowInsecure: true}
				if mismatch == "origin" {
					credential.HubURL = "https://former-hub.example"
				} else {
					credential.HubProjectID++
				}
				require.NoError(t, credentials.StoreFederationCredential(t.Context(), project.UID, credential))
				endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", project.ID, issue.UID)
				var status int
				var body []byte
				switch operation {
				case "upload":
					status, body = attachmentUpload(t, env, endpoint, "file.pdf", strings.NewReader("fixture"))
				case "link":
					status, body, err = postAttachmentLink(t, env, endpoint+"/links", `{"path":"/kata/file.pdf","actor":"operator"}`)
					require.NoError(t, err)
				default:
					status, body = env.Get(t, endpoint+"/"+attachmentID+"/content")
				}
				require.Equal(t, http.StatusServiceUnavailable, status, string(body))
				require.Zero(t, calls.Load(), "a credential must not be retargeted before authentication")
			})
		}
	}
}

type rejectingDocbankFence struct{}

func (rejectingDocbankFence) AuthorizeFederation(context.Context, daemon.HostFederationAccessRequest) (daemon.HostFederationAccessDecision, error) {
	return daemon.HostFederationAccessDecision{TransactionFence: func(context.Context, db.Transaction) error { return daemon.ErrHostAccessDenied }}, nil
}

func TestFederatedDocbankContentRejectsRelayLocalOrigin(t *testing.T) {
	vault := docbanktest.New(t)
	version := uuid.MustParse(linkVersion)
	vault.Seed("/kata/relay-local.pdf", docbank.Node{ID: 42, Name: "relay-local.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("relay-local bytes")})
	credentials := newReplicaCredentialStore()
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
		cfg.DocbankFolder = "/kata"
		cfg.FederationCredentials = credentials
	})
	project, _ := seedProjectAndIssue(t, env)
	rootUID, err := uid.New()
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"version":1,"project_uid":%q,"origin":%q}`, project.UID, rootUID); err != nil {
			t.Errorf("write upstream capabilities: %v", err)
		}
	}))
	t.Cleanup(upstream.Close)
	_, err = env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL,
		HubProjectID: 42, HubProjectUID: project.UID, Enabled: true, PushEnabled: true,
		Actor: "upstream-member", AllowInsecure: true,
	})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, env.DB.PinRootAuthority(t.Context(), db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}))
	_, err = env.DB.SetRelayBindingConfig(t.Context(), project.ID, db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
		UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
		HubPath: []string{rootUID, env.DB.InstanceUID()}, LocalActor: "tester",
		ServeDownstream: true, ResetEpoch: 1,
	})
	require.NoError(t, err)
	require.NoError(t, credentials.StoreFederationCredential(t.Context(), project.UID, config.FederationCredential{
		HubURL: upstream.URL, HubProjectID: 42, Token: "upstream-docbank-test-token",
		Actor: "upstream-member", Capabilities: "pull,push", AllowInsecure: true,
	}))
	//nolint:gosec // G101: synthetic fixture credential for this isolated daemon test.
	parent, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "relay-docbank-parent-test-token", Actor: "tester", AdminActor: "operator",
	})
	require.NoError(t, err)
	downstreamUID, err := uid.New()
	require.NoError(t, err)
	//nolint:gosec // G101: synthetic fixture credential for this isolated daemon test.
	grant, err := env.DB.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: downstreamUID,
		ProtocolVersion: db.RelayProtocolVersion, Token: "relay-docbank-content-test-token",
	})
	require.NoError(t, err)

	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/federation/docbank/versions/%s/content?project_uid=%s&origin=%s",
		env.URL, project.ID, version, project.UID, env.DB.InstanceUID())
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+grant.Token)
	request.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
	response, err := env.HTTP.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode, string(body))
	require.EqualValues(t, 1, upstreamCalls.Load(), "the relay must resolve the project's upstream Docbank origin")
	require.Empty(t, vault.Requests(), "a downstream request cannot select the relay-local vault")
}

func TestDocbankOriginServiceRejectsHostFenceBeforeIO(t *testing.T) {
	for _, operation := range []string{"uploads", "links"} {
		t.Run(operation, func(t *testing.T) {
			testDocbankServiceDenial(t, operation, false)
		})
	}
}

func TestDocbankOriginServiceRejectsUnexpectedOriginBeforeIO(t *testing.T) {
	for _, operation := range []string{"uploads", "links"} {
		t.Run(operation, func(t *testing.T) {
			testDocbankServiceDenial(t, operation, true)
		})
	}
}

func testDocbankServiceDenial(t *testing.T, operation string, wrongOrigin bool, access ...daemon.HostFederationAccessController) {
	t.Helper()
	vault := docbanktest.New(t)
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
		cfg.DocbankFolder = "/kata"
		if !wrongOrigin {
			cfg.HostFederationAccess = rejectingDocbankFence{}
			if len(access) != 0 {
				cfg.HostFederationAccess = access[0]
			}
		}
	})
	project, issue := seedProjectAndIssue(t, env)
	_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "operator")
	require.NoError(t, err)
	grant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Token: "service-fixture-token", Capabilities: "pull,push", Actor: "operator"})
	require.NoError(t, err)
	version := uuid.MustParse(linkVersion)
	vault.Seed("/kata/file.pdf", docbank.Node{ID: 42, Name: "file.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("fixture")})
	origin := env.DB.InstanceUID()
	want := http.StatusForbidden
	if len(access) != 0 {
		want = http.StatusServiceUnavailable
	}
	if wrongOrigin {
		origin, err = uid.New()
		require.NoError(t, err)
		want = http.StatusConflict
	}
	query := url.Values{"project_uid": {project.UID}, "origin": {origin}, "name": {"file.pdf"}}
	body, mediaType := "fixture", "application/octet-stream"
	if operation == "links" {
		body, mediaType = `{"path":"/kata/file.pdf"}`, "application/json"
	}
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/federation/docbank/issues/%s/%s?%s", env.URL, project.ID, issue.UID, operation, query.Encode())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+grant.Token)
	req.Header.Set("Content-Type", mediaType)
	resp, err := env.HTTP.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, want, resp.StatusCode, string(raw))
	require.Empty(t, vault.Requests(), "denied authority or wrong origin must stop before Docbank I/O")
}

// Failed host recording must remain unavailable, even when the original
// transaction also denied authority. It is not a completed denial decision.
type unavailableDocbankFinalization struct{}

func (unavailableDocbankFinalization) AuthorizeFederation(context.Context, daemon.HostFederationAccessRequest) (daemon.HostFederationAccessDecision, error) {
	return daemon.HostFederationAccessDecision{TransactionFence: func(context.Context, db.Transaction) error {
		return db.AfterTransactionRollback(daemon.ErrHostAccessDenied, func(context.Context) error { return errors.New("fixture recording unavailable") })
	}}, nil
}

func TestDocbankOriginServiceRejectsFinalizationFailureBeforeIO(t *testing.T) {
	for _, operation := range []string{"uploads", "links"} {
		t.Run(operation, func(t *testing.T) { testDocbankServiceDenial(t, operation, false, unavailableDocbankFinalization{}) })
	}
}
