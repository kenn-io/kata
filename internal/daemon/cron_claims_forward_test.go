package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func newCronClaimSpoke(t *testing.T, upstream *testenv.Env, project db.Project, actor string) (*testenv.Env, db.Project, *replicaCredentialStore, db.CreatedFederationEnrollment) {
	t.Helper()
	env := testenv.New(t)
	p, err := env.DB.CreateProjectWithUID(t.Context(), "spoke-project", project.UID)
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Actor: actor, PushEnabled: true, Enabled: true, AllowInsecure: true})
	require.NoError(t, err)
	grant, err := upstream.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: env.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "claim,pull,push", Actor: actor})
	require.NoError(t, err)
	creds := newReplicaCredentialStore()
	require.NoError(t, creds.StoreFederationCredential(t.Context(), p.UID, config.FederationCredential{HubURL: upstream.URL, HubProjectID: project.ID, Token: grant.Token, Capabilities: "claim,pull,push", Actor: actor, AllowInsecure: true}))
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, FederationCredentials: creds})
	t.Cleanup(func() { _ = server.Close() })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	env.URL = endpoint.URL
	env.HTTP = endpoint.Client()
	return env, p, creds, grant
}

func TestCronOccurrenceClaimsTwoSpokesConcurrent(t *testing.T) {
	root := testenv.New(t)
	p, err := root.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = root.DB.EnableProjectFederation(t.Context(), p.ID, "worker")
	require.NoError(t, err)
	job := cronClaimJob(t, root, p)
	a, pa, _, _ := newCronClaimSpoke(t, root, p, "member-a")
	b, pb, _, _ := newCronClaimSpoke(t, root, p, "member-b")
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i, peer := range []*testenv.Env{a, b} {
		local := []db.Project{pa, pb}[i]
		body := cronClaimBody(job, fmt.Sprintf("000000000000000000000000%02d", i+3))
		wg.Go(func() {
			<-start
			resp, raw := envDoRaw(t, peer, http.MethodPost, cronClaimPath(local.ID, job.UID), body, nil)
			require.Contains(t, []int{201, 409}, resp.StatusCode, string(raw))
			codes <- resp.StatusCode
		})
	}
	close(start)
	wg.Wait()
	close(codes)
	wins := 0
	for code := range codes {
		if code == 201 {
			wins++
		}
	}
	require.Equal(t, 1, wins)
	for _, peer := range []*testenv.Env{a, b} {
		var count int
		require.NoError(t, peer.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cron_occurrence_claims`).Scan(&count))
		require.Zero(t, count, "spokes never cache arbitration rows")
	}
	resp, raw := envDoRaw(t, a, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", pa.ID), nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "cron_occurrence_claims_v1")
}

func TestCronOccurrenceClaimsRootOfflinePending(t *testing.T) {
	root := testenv.New(t)
	p, err := root.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = root.DB.EnableProjectFederation(t.Context(), p.ID, "worker")
	require.NoError(t, err)
	job := cronClaimJob(t, root, p)
	spoke, local, _, _ := newCronClaimSpoke(t, root, p, "member")
	setClaimBindingHubURL(t, spoke, local.ID, fastFailClaimHubURL(t))
	body := cronClaimBody(job, "00000000000000000000000003")
	resp, raw := envDoRaw(t, spoke, http.MethodPost, cronClaimPath(local.ID, job.UID), body, nil)
	assertAPIError(t, resp.StatusCode, raw, 503, "cron_claim_pending")
	require.NotContains(t, string(raw), `"claimed":true`)
	setClaimBindingHubURL(t, spoke, local.ID, root.URL)
	body["expected_authority_instance_uid"] = "00000000000000000000000009"
	resp, raw = envDoRaw(t, spoke, http.MethodPost, cronClaimPath(local.ID, job.UID), body, nil)
	assertAPIError(t, resp.StatusCode, raw, 503, "cron_claim_pending")
	delete(body, "expected_authority_instance_uid")
	resp, raw = envDoRaw(t, spoke, http.MethodPost, cronClaimPath(local.ID, job.UID), body, nil)
	require.Equal(t, 201, resp.StatusCode, string(raw))
	// Discarding that response and looking up the saved UID resolves the same grant.
	resp, raw = envDoRaw(t, spoke, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/claims/00000000000000000000000003", local.ID), nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var out struct {
		Claim db.CronOccurrenceClaim `json:"claim"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, spoke.DB.InstanceUID(), out.Claim.HolderInstanceUID)
	require.Equal(t, "member", out.Claim.Actor)
}

func TestCronOccurrenceClaimsUnavailableCapabilityAndRedirect(t *testing.T) {
	for _, mode := range []string{"old-root", "redirect", "malformed-grant", "changed-evidence"} {
		t.Run(mode, func(t *testing.T) {
			root := testenv.New(t)
			p, err := root.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			_, err = root.DB.EnableProjectFederation(t.Context(), p.ID, "worker")
			require.NoError(t, err)
			j := cronClaimJob(t, root, p)
			var posts, leaks atomic.Int32
			var holder string
			target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { leaks.Add(1) }))
			t.Cleanup(target.Close)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, target.URL, http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/capabilities") {
					caps := []string{}
					if mode == "malformed-grant" || mode == "changed-evidence" {
						caps = append(caps, "cron_occurrence_claims_v1")
					}
					raw, _ := json.Marshal(map[string]any{"project_uid": p.UID, "event_features": []string{"cron_v1"}, "capabilities": caps, "claim_authority_instance_uid": root.DB.InstanceUID()})
					_, _ = w.Write(raw)
					return
				}
				posts.Add(1)
				if mode == "changed-evidence" {
					raw, _ := json.Marshal(map[string]any{"claimed": true, "replayed": false, "claim": db.CronOccurrenceClaim{ProjectID: p.ID, JobUID: j.UID, RunUID: "00000000000000000000000003", DefinitionEventUID: j.DefinitionEventUID, OccurrenceKey: "2030-01-02T03:04:05Z", HolderInstanceUID: holder, Actor: "member", ExecutorLabel: new("other-executor"), AuthorityInstanceUID: root.DB.InstanceUID(), CreatedAt: time.Now().UTC()}})
					w.WriteHeader(201)
					_, _ = w.Write(raw)
					return
				}
				_, _ = w.Write([]byte(`{"claimed":true}`))
			}))
			t.Cleanup(fake.Close)
			spoke, local, _, _ := newCronClaimSpoke(t, root, p, "member")
			holder = spoke.DB.InstanceUID()
			setClaimBindingHubURL(t, spoke, local.ID, fake.URL)
			resp, raw := envDoRaw(t, spoke, http.MethodPost, cronClaimPath(local.ID, j.UID), cronClaimBody(j, "00000000000000000000000003"), nil)
			assertAPIError(t, resp.StatusCode, raw, 503, "cron_claim_pending")
			require.Zero(t, leaks.Load())
			if mode != "malformed-grant" && mode != "changed-evidence" {
				require.Zero(t, posts.Load())
			}
		})
	}
}

func TestCronOccurrenceClaimsAuthorityRebind(t *testing.T) {
	root := testenv.New(t)
	p, err := root.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = root.DB.EnableProjectFederation(t.Context(), p.ID, "worker")
	require.NoError(t, err)
	j := cronClaimJob(t, root, p)
	spoke, local, creds, _ := newCronClaimSpoke(t, root, p, "member")
	body := cronClaimBody(j, "00000000000000000000000003")
	resp, raw := envDoRaw(t, spoke, http.MethodPost, cronClaimPath(local.ID, j.UID), body, nil)
	require.Equal(t, 201, resp.StatusCode, string(raw))
	other := testenv.New(t)
	p2, err := other.DB.CreateProjectWithUID(t.Context(), "example-project", p.UID)
	require.NoError(t, err)
	_, err = other.DB.EnableProjectFederation(t.Context(), p2.ID, "worker")
	require.NoError(t, err)
	_, _, err = other.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p2.ID, UID: j.UID, Name: j.Name, Definition: j.Definition, Actor: "worker"})
	require.NoError(t, err)
	_, err = other.DB.ExecContext(t.Context(), `UPDATE cron_jobs SET definition_event_uid=? WHERE uid=?`, j.DefinitionEventUID, j.UID)
	require.NoError(t, err)
	grant, err := other.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ProjectID: &p2.ID, SpokeInstanceUID: spoke.DB.InstanceUID(), Actor: "member", Capabilities: "claim,pull,push"})
	require.NoError(t, err)
	require.NoError(t, creds.StoreFederationCredential(t.Context(), p.UID, config.FederationCredential{HubURL: other.URL, HubProjectID: p2.ID, Token: grant.Token, Actor: "member", Capabilities: "claim,pull,push", AllowInsecure: true}))
	setClaimBindingHubURL(t, spoke, local.ID, other.URL)
	body["run_uid"] = "00000000000000000000000004"
	resp, raw = envDoRaw(t, spoke, http.MethodPost, cronClaimPath(local.ID, j.UID), body, nil)
	assertAPIError(t, resp.StatusCode, raw, 503, "cron_claim_pending")
	var count int
	require.NoError(t, other.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cron_occurrence_claims`).Scan(&count))
	require.Zero(t, count, "a changed root must not receive a grant")
}

type revokingCronAdmissionStore struct {
	db.Storage
	before func()
}

func (s revokingCronAdmissionStore) AdmitCronClaim(ctx context.Context, p int64, authority string) error {
	s.before()
	return s.Storage.AdmitCronClaim(ctx, p, authority)
}

func TestCronOccurrenceClaimsForwardAdmissionFence(t *testing.T) {
	root := testenv.New(t)
	p, err := root.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = root.DB.EnableProjectFederation(t.Context(), p.ID, "worker")
	require.NoError(t, err)
	j := cronClaimJob(t, root, p)
	spoke, local, creds, _ := newCronClaimSpoke(t, root, p, "member")
	token, _, err := spoke.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "leaf-actor-test-token", Actor: "worker", AdminActor: "admin"})
	require.NoError(t, err)
	store := revokingCronAdmissionStore{Storage: spoke.DB, before: func() { _, _, e := spoke.DB.RevokeAPIToken(t.Context(), token.ID, "admin"); require.NoError(t, e) }}
	server := daemon.NewServer(daemon.ServerConfig{DB: store, FederationCredentials: creds, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { _ = server.Close() })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	copyEnv := *spoke
	copyEnv.URL = endpoint.URL
	resp, raw := envDoRaw(t, &copyEnv, http.MethodPost, cronClaimPath(local.ID, j.UID), cronClaimBody(j, "00000000000000000000000003"), bearer("leaf-actor-test-token"))
	require.Equal(t, 401, resp.StatusCode, string(raw))
	var count int
	require.NoError(t, root.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cron_occurrence_claims`).Scan(&count))
	require.Zero(t, count)
}

func TestCronOccurrenceClaimsRelayPreservesOrigin(t *testing.T) {
	root := testenv.New(t)
	p, err := root.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = root.DB.EnableProjectFederation(t.Context(), p.ID, "worker")
	require.NoError(t, err)
	j := cronClaimJob(t, root, p)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: root.DB.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, root.DB.PinRootAuthority(t.Context(), pin))
	relay, rp, creds, oldGrant := newCronClaimSpoke(t, root, p, "relay-member")
	err = root.DB.RevokeFederationEnrollment(t.Context(), oldGrant.Enrollment.ID)
	require.NoError(t, err)
	parent, _, err := root.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "relay-parent-test-token", Actor: "relay-member", AdminActor: "admin"}) // #nosec G101 -- synthetic token used only by this test.
	require.NoError(t, err)
	grant, err := root.DB.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: p.ID, ParentTokenID: parent.ID, SpokeInstanceUID: relay.DB.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion, ServeDownstream: true})
	require.NoError(t, err)
	require.NoError(t, relay.DB.PinRootAuthority(t.Context(), pin))
	_, err = relay.DB.SetRelayBindingConfig(t.Context(), rp.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, UpstreamInstanceUID: root.DB.InstanceUID(), AuthorityUID: root.DB.InstanceUID(), HubPath: []string{root.DB.InstanceUID(), relay.DB.InstanceUID()}, LocalActor: "relay-member", ServeDownstream: true, ResetEpoch: 1})
	require.NoError(t, err)
	require.NoError(t, creds.StoreFederationCredential(t.Context(), rp.UID, config.FederationCredential{HubURL: root.URL, HubProjectID: p.ID, Token: grant.Token, Actor: "relay-member", Capabilities: "claim,pull,push", AllowInsecure: true}))
	leaf, lp, leafCreds, leafGrant := newCronClaimSpoke(t, relay, rp, "leaf-member")
	require.NoError(t, relay.DB.RevokeFederationEnrollment(t.Context(), leafGrant.Enrollment.ID))
	leafParent, _, err := relay.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "leaf-parent-test-token", Actor: "leaf-member", AdminActor: "admin"})
	require.NoError(t, err)
	leafGrant, err = relay.DB.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: rp.ID, ParentTokenID: leafParent.ID, SpokeInstanceUID: leaf.DB.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion})
	require.NoError(t, err)
	require.NoError(t, leafCreds.StoreFederationCredential(t.Context(), lp.UID, config.FederationCredential{HubURL: relay.URL, HubProjectID: rp.ID, Token: leafGrant.Token, Actor: "leaf-member", Capabilities: "claim,pull,push", AllowInsecure: true}))
	require.NoError(t, leaf.DB.PinRootAuthority(t.Context(), pin))
	_, err = leaf.DB.SetRelayBindingConfig(t.Context(), lp.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: leafGrant.Enrollment.RelayBindingUID, UpstreamInstanceUID: relay.DB.InstanceUID(), AuthorityUID: root.DB.InstanceUID(), HubPath: []string{root.DB.InstanceUID(), relay.DB.InstanceUID(), leaf.DB.InstanceUID()}, LocalActor: "leaf-member", ServeDownstream: false, ResetEpoch: 1})
	require.NoError(t, err)
	for _, peer := range []*testenv.Env{root, relay, leaf} {
		resp, raw := envDoRaw(t, peer, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", p.ID), nil, nil)
		require.Equal(t, 200, resp.StatusCode, string(raw))
		require.Contains(t, string(raw), "cron_occurrence_claims_v1")
	}
	body := cronClaimBody(j, "00000000000000000000000003")
	resp, raw := envDoRaw(t, leaf, http.MethodPost, cronClaimPath(lp.ID, j.UID), body, nil)
	require.Equal(t, 201, resp.StatusCode, string(raw))
	stored, err := root.DB.CronOccurrenceClaim(t.Context(), p.ID, "00000000000000000000000003")
	require.NoError(t, err)
	require.Equal(t, leaf.DB.InstanceUID(), stored.HolderInstanceUID)
	require.Equal(t, "leaf-member", stored.Actor)
	resp, raw = envDoRaw(t, leaf, http.MethodPost, cronClaimPath(lp.ID, j.UID), body, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	resp, raw = envDoRaw(t, leaf, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/claims/%s", lp.ID, stored.RunUID), nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	body["run_uid"] = "00000000000000000000000004"
	resp, raw = envDoRaw(t, relay, http.MethodPost, cronClaimPath(rp.ID, j.UID), body, nil)
	require.Equal(t, 409, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), leaf.DB.InstanceUID())
	// Revoke a negotiated leaf credential after authentication but before the
	// relay's admission transaction. The retained root winner must not be read
	// or replaced by another forwarded mutation.
	store := revokingCronAdmissionStore{Storage: relay.DB, before: func() { require.NoError(t, relay.DB.RevokeFederationEnrollment(t.Context(), leafGrant.Enrollment.ID)) }}
	fenced := daemon.NewServer(daemon.ServerConfig{DB: store, FederationCredentials: creds})
	t.Cleanup(func() { _ = fenced.Close() })
	endpoint := httptest.NewServer(fenced.Handler())
	t.Cleanup(endpoint.Close)
	fencedEnv := *relay
	fencedEnv.URL = endpoint.URL
	resp, raw = envDoRaw(t, &fencedEnv, http.MethodPost, cronClaimPath(rp.ID, j.UID), body, bearer(leafGrant.Token))
	require.Equal(t, 403, resp.StatusCode, string(raw))
	var count int
	require.NoError(t, relay.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cron_occurrence_claims`).Scan(&count))
	require.Zero(t, count)
}

func TestCronOccurrenceClaimsLostResponseLookup(t *testing.T) {
	root := testenv.New(t)
	p, err := root.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = root.DB.EnableProjectFederation(t.Context(), p.ID, "worker")
	require.NoError(t, err)
	j := cronClaimJob(t, root, p)
	var lose atomic.Bool
	lose.Store(true)
	rootURL, err := url.Parse(root.URL)
	require.NoError(t, err)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := r.Clone(r.Context())
		request.RequestURI = ""
		request.URL.Scheme = rootURL.Scheme
		request.URL.Host = rootURL.Host
		request.Host = rootURL.Host
		response, e := root.HTTP.Do(request) //nolint:gosec // G704: destination is the fixed local httptest root, not the incoming URL.
		if e != nil {
			http.Error(w, "transport failed", http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = response.Body.Close() }()
		raw, e := io.ReadAll(response.Body)
		if e != nil {
			http.Error(w, "read failed", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims") && lose.Swap(false) {
			http.Error(w, "response lost after root commit", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(proxy.Close)
	spoke, local, _, _ := newCronClaimSpoke(t, root, p, "member")
	setClaimBindingHubURL(t, spoke, local.ID, proxy.URL)
	body := cronClaimBody(j, "00000000000000000000000003")
	resp, raw := envDoRaw(t, spoke, http.MethodPost, cronClaimPath(local.ID, j.UID), body, nil)
	assertAPIError(t, resp.StatusCode, raw, 503, "cron_claim_pending")
	stored, err := root.DB.CronOccurrenceClaim(t.Context(), p.ID, "00000000000000000000000003")
	require.NoError(t, err)
	resp, raw = envDoRaw(t, spoke, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/claims/%s", local.ID, stored.RunUID), nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), stored.RunUID)
	resp, raw = envDoRaw(t, spoke, http.MethodPost, cronClaimPath(local.ID, j.UID), body, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"replayed":true`)
}
