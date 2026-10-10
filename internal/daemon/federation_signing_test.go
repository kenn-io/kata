package daemon_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// Contract: a real prefix-stripping HTTPS proxy reaches only native scoped
// federation operations; signed metadata/pull/ingest/leases preserve authority.
func TestSignedFederationRestrictedIngressRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("TEST_INGRESS_KEY", strings.Repeat("k", 64))
		source := federationsigning.Source{KeyID: "key-a", KeyEnv: "TEST_INGRESS_KEY"}
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_DB", filepath.Join(home, "spoke.db"))
		hubDB, err := sqlitestore.Open(t.Context(), filepath.Join(home, "hub.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, hubDB.Close()) })
		env := &testenv.Env{DB: hubDB}
		credentials := newReplicaCredentialStore()
		spokeDB, err := sqlitestore.Open(t.Context(), filepath.Join(home, "spoke.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, spokeDB.Close()) })
		spokeServer := daemon.NewServer(daemon.ServerConfig{DB: spokeDB, FederationCredentials: credentials})
		spokeHTTP := httptest.NewTestServer(t, spokeServer.Handler())
		spoke := &testenv.Env{DB: spokeDB, URL: "http://spoke.example", HTTP: spokeHTTP.Client()}
		project, issue := createClaimHubIssueNamed(t, env, "hub-project")
		enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
			Token: "enrollment-secret", SpokeInstanceUID: spoke.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "pull,push,claim", Actor: "enrolled-actor",
		})
		require.NoError(t, err)
		t.Setenv("TEST_GLOBAL_KEY", strings.Repeat("g", 64))
		t.Setenv("TEST_PULL_KEY", strings.Repeat("p", 64))
		t.Setenv("TEST_PUSH_KEY", strings.Repeat("u", 64))
		t.Setenv("TEST_REMOVED_KEY", strings.Repeat("r", 64))
		globalSource := federationsigning.Source{KeyID: "global-key", KeyEnv: "TEST_GLOBAL_KEY"}
		pullSource := federationsigning.Source{KeyID: "pull-key", KeyEnv: "TEST_PULL_KEY"}
		pushSource := federationsigning.Source{KeyID: "push-key", KeyEnv: "TEST_PUSH_KEY"}
		removedSource := federationsigning.Source{KeyID: "removed-key", KeyEnv: "TEST_REMOVED_KEY"}
		globalEnrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "global-enrollment", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Capabilities: "pull", Actor: "global-actor"})
		require.NoError(t, err)
		pullEnrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "pull-enrollment", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EY", ProjectID: &project.ID, Capabilities: "pull", Actor: "pull-actor"})
		require.NoError(t, err)
		pushOriginUID, err := uid.New()
		require.NoError(t, err)
		pushEnrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "push-enrollment", SpokeInstanceUID: pushOriginUID, ProjectID: &project.ID, Capabilities: "push", Actor: "push-actor"})
		require.NoError(t, err)
		removedEnrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "removed-enrollment", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EZ", ProjectID: &project.ID, Capabilities: "pull", Actor: "removed-actor"})
		require.NoError(t, err)
		var ingress http.Handler
		backend := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ingress.ServeHTTP(w, r) }))
		u, err := url.Parse("http://backend.example")
		require.NoError(t, err)
		proxy := &httputil.ReverseProxy{
			Transport: backend.Client().Transport,
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(u)
				r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, "/mount")
			},
		}
		public := httptest.NewTestServer(t, proxy)
		base := "https://hub.example/mount"
		state := filepath.Join(t.TempDir(), "replay.state")
		require.NoError(t, federationsigning.InitializeReplayState(state))
		v, err := federationsigning.NewVerifier(base, []federationsigning.Key{{Source: source, EnrollmentID: enrollment.Enrollment.ID}, {Source: globalSource, EnrollmentID: globalEnrollment.Enrollment.ID}, {Source: pullSource, EnrollmentID: pullEnrollment.Enrollment.ID}, {Source: pushSource, EnrollmentID: pushEnrollment.Enrollment.ID}, {Source: removedSource, EnrollmentID: removedEnrollment.Enrollment.ID}}, state)
		t.Cleanup(func() { _ = v.Close() })
		previous := http.DefaultTransport
		http.DefaultTransport = public.Client().Transport
		t.Cleanup(func() { http.DefaultTransport = previous })
		require.NoError(t, err)
		access := &countingFederationAccess{}
		server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostFederationAccess: access, StartedAt: time.Now(), FederationSigning: v, FederationSigningRequired: true})
		ingress, err = server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerFederation})
		require.NoError(t, err)
		client, err := federation.NewClient(t.Context(), base, enrollment.Token, clientpkg.Opts{FederationSigning: &source, Timeout: 60 * time.Second})
		require.NoError(t, err)
		_, err = client.ProjectFederation(t.Context(), project.ID)
		require.ErrorContains(t, err, "503")
		// Advance the actual quarantine on synctest's clock; handlers and TLS still
		// execute through the HTTP stack on httptest's in-memory network.
		time.Sleep(federationsigning.Quarantine)
		metadata, err := client.ProjectFederation(t.Context(), project.ID)
		require.NoError(t, err)
		require.Equal(t, project.UID, metadata.ProjectUID)
		pushClient, err := federation.NewClient(t.Context(), base, pushEnrollment.Token, clientpkg.Opts{FederationSigning: &pushSource, Timeout: 60 * time.Second})
		require.NoError(t, err)
		cronEvent, cronJobUID := signedCronPublicationEvent(t, project, pushOriginUID, "push-actor")
		_, err = pushClient.IngestProjectEvents(t.Context(), project.ID, []api.FederationIngestEventEnvelope{cronEvent})
		require.NoError(t, err, "signed push-only enrollment must publish cron events")
		cronJob, err := env.DB.CronJob(t.Context(), project.ID, cronJobUID)
		require.NoError(t, err)
		require.Equal(t, "Signed review", cronJob.Name)
		require.Equal(t, "push-actor", cronJob.Author)
		mainHandler, err := server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerSharedTCP, Origin: "https://daemon.example"})
		require.NoError(t, err)
		headRequest := httptest.NewRequest(http.MethodHead, "https://daemon.example"+projectPath(project.ID)+"/federation/metadata", nil)
		headRequest.Header.Set("Authorization", "Bearer "+enrollment.Token)
		headResponse := httptest.NewRecorder()
		mainHandler.ServeHTTP(headResponse, headRequest)
		require.Equal(t, http.StatusUnauthorized, headResponse.Code, "HEAD must not bypass required federation signing")
		// Signed main-listener ingest shares the verified authorization instead of
		// charging the native host federation admission policy a second time.
		mainPayload, err := json.Marshal(federationIngestBody())
		require.NoError(t, err)
		mainPath := projectPath(project.ID) + "/federation/events:ingest"
		mainRequest, err := http.NewRequest(http.MethodPost, base+mainPath, bytes.NewReader(mainPayload))
		require.NoError(t, err)
		mainRequest.Header.Set("Authorization", "Bearer "+enrollment.Token)
		mainRequest.Header.Set(db.EventFeaturesHeader, db.CronEventFeature)
		require.NoError(t, federationsigning.Sign(mainRequest, source))
		mainRequest.URL.Path = mainPath
		before := len(access.snapshot())
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, mainRequest)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Len(t, access.snapshot(), before+1, "verified ingest must reuse native host admission")

		credential := config.FederationCredential{HubURL: base, HubProjectID: project.ID, Token: enrollment.Token, Capabilities: "pull,push,claim", Actor: "enrolled-actor", Signing: &source}
		replica, err := daemon.EnsureFederationReplica(t.Context(), spoke.DB, credentials, nil, daemon.EnsureFederationReplicaParams{HubURL: base, HubProjectID: project.ID, HubProjectUID: project.UID, ProjectName: "spoke-project", ReplayHorizonEventID: metadata.ReplayHorizonEventID, Credential: credential, PushEnabled: true})
		require.NoError(t, err)
		require.NoError(t, federation.SyncFederationOnce(t.Context(), spoke.DB, replica.Binding, credential))
		mirrored, err := spoke.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, issue.Title, mirrored.Title)
		_, _, err = spoke.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: mirrored.ID, Author: "enrolled-actor", Body: "signed spoke edit"})
		require.NoError(t, err)
		binding, err := spoke.DB.FederationBindingByProject(t.Context(), replica.Project.ID)
		require.NoError(t, err)
		require.NoError(t, federation.SyncFederationOnce(t.Context(), spoke.DB, binding, credential))
		comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Equal(t, "signed spoke edit", comments[0].Body)

		_, err = client.PollProjectEvents(t.Context(), project.ID, 0, 10)
		require.NoError(t, err)
		_, err = client.IngestProjectEvents(t.Context(), project.ID, nil)
		require.NoError(t, err)
		claim := federation.ClaimRequest{Holder: "spoofed-actor", ClientKind: "cli", ClaimKind: "timed", TTLSeconds: 300, Purpose: "edit"}
		acquired, err := client.AcquireClaim(t.Context(), project.ID, issue.ShortID, claim)
		require.NoError(t, err)
		require.True(t, acquired.Granted)
		require.Equal(t, "enrolled-actor", acquired.Holder.Holder)
		_, err = client.RenewClaim(t.Context(), project.ID, issue.ShortID, claim)
		require.NoError(t, err)
		_, err = client.ClaimStatus(t.Context(), project.ID, issue.ShortID)
		require.NoError(t, err)
		_, err = client.ReleaseClaim(t.Context(), project.ID, issue.ShortID, claim)
		require.NoError(t, err)
		// The spoke's real local API forwards with its saved signing source.
		for _, action := range []string{"acquire", "renew", "release"} {
			resp, raw := envDoRaw(t, spoke, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease/actions/%s", replica.Project.ID, mirrored.ShortID, action), map[string]any{"holder": "enrolled-actor", "client_kind": "cli", "claim_kind": "timed", "ttl_seconds": 300, "purpose": "edit"}, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		}
		resp, raw := envDoRaw(t, spoke, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease", replica.Project.ID, mirrored.ShortID), nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		resp, raw = envDoRaw(t, spoke, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/claim", replica.Project.ID, mirrored.ShortID), map[string]any{"actor": "enrolled-actor", "ttl_seconds": 300}, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		// Valid MACs cannot widen the bearer capability or global project scope.
		globalClient, err := federation.NewClient(t.Context(), base, globalEnrollment.Token, clientpkg.Opts{FederationSigning: &globalSource})
		require.NoError(t, err)
		_, err = globalClient.ProjectFederation(t.Context(), project.ID)
		require.Error(t, err)
		pullClient, err := federation.NewClient(t.Context(), base, pullEnrollment.Token, clientpkg.Opts{FederationSigning: &pullSource})
		require.NoError(t, err)
		_, err = pullClient.AcquireClaim(t.Context(), project.ID, issue.ShortID, claim)
		require.Error(t, err)
		for _, p := range []string{
			"", "/api/v1/ui/snapshot", "/api/v1/tokens", "/api/v1/projects",
			fmt.Sprintf("/api/v1/projects/%d/federation/enrollments", project.ID),
			fmt.Sprintf("/api/v1/federation/replicas/%s/actions/configure-signing", project.UID),
		} {
			r, err := http.NewRequest(http.MethodPost, base+p, strings.NewReader(`{}`))
			require.NoError(t, err)
			r.Header.Set("Authorization", "Bearer "+enrollment.Token)
			require.NoError(t, federationsigning.Sign(r, source))
			resp, err := public.Client().Do(r)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		}
		other := createFederatedHubProject(t, env, "other-project")
		_, err = client.ProjectFederation(t.Context(), other.ID)
		require.Error(t, err)
		require.NoError(t, env.DB.RevokeFederationEnrollment(context.Background(), enrollment.Enrollment.ID))
		_, err = client.ProjectFederation(t.Context(), project.ID)
		require.Error(t, err)
		removedRequest, err := http.NewRequest(http.MethodGet, base+projectPath(project.ID)+"/federation/metadata", nil)
		require.NoError(t, err)
		removedRequest.Header.Set("Authorization", "Bearer "+removedEnrollment.Token)
		require.NoError(t, federationsigning.Sign(removedRequest, removedSource))
		t.Setenv("TEST_REMOVED_KEY", "")
		removedResponse, err := public.Client().Do(removedRequest)
		require.NoError(t, err)
		_ = removedResponse.Body.Close()
		require.Equal(t, http.StatusUnauthorized, removedResponse.StatusCode)
	})
}

func signedCronPublicationEvent(t *testing.T, project db.Project, originUID, actor string) (api.FederationIngestEventEnvelope, string) {
	t.Helper()
	eventUID, err := uid.New()
	require.NoError(t, err)
	jobUID, err := uid.New()
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	definition, err := json.Marshal(cron.JobDefinition{
		Version: 1,
		Kind:    "job",
		Trigger: cron.Trigger{Kind: "manual"},
		Action:  cron.Action{Kind: "execute", Prompt: "Review"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Review"},
		Overlap: "forbid",
		Catchup: "skip",
	})
	require.NoError(t, err)
	payload, err := json.Marshal(db.CronDefinitionEvent{
		UID: jobUID, ProjectUID: project.UID, Name: "Signed review",
		Definition: jsontext.Value(definition), Author: actor,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	createdAt := now.Format(db.EventTimestampFormat)
	physicalMS := now.UnixMilli()
	contentHash, err := db.EventContentHash(db.EventHashInput{
		UID: eventUID, OriginInstanceUID: originUID, ProjectUID: project.UID,
		ProjectName: project.Name, Type: "cron.job.created", Actor: actor,
		HLCPhysicalMS: physicalMS, Payload: payload, CreatedAt: createdAt,
	})
	require.NoError(t, err)
	return api.FederationIngestEventEnvelope{
		EventID: 1, EventUID: eventUID, OriginInstanceUID: originUID,
		ProjectUID: project.UID, ProjectName: project.Name,
		Type: "cron.job.created", Actor: actor, HLCPhysicalMS: physicalMS,
		ContentHash: contentHash, Payload: jsontext.Value(payload), CreatedAt: now,
	}, jobUID
}

// Contract: the native ingress HTTP server enforces its header budget before
// routing, including requests to endpoints outside its allowlist.
func TestFederationIngressBoundsHeaders(t *testing.T) {
	env := testenv.New(t)
	srv := daemon.NewServer(daemon.ServerConfig{DB: env.DB})
	t.Cleanup(func() { _ = srv.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- srv.ServeListeners(ctx, daemon.ListenerBinding{Listener: listener, Policy: daemon.ListenerPolicy{Kind: daemon.ListenerFederation}})
	}()
	req, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/unavailable", nil)
	require.NoError(t, err)
	req.Header.Set("X-Oversized", strings.Repeat("x", 32<<10))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode)
	cancel()
	require.NoError(t, <-done)
}

func TestFederationIngressBoundsConnections(t *testing.T) {
	env := testenv.New(t)
	srv := daemon.NewServer(daemon.ServerConfig{DB: env.DB})
	t.Cleanup(func() { _ = srv.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- srv.ServeListeners(ctx, daemon.ListenerBinding{Listener: listener, Policy: daemon.ListenerPolicy{Kind: daemon.ListenerFederation}})
	}()
	var connections []net.Conn
	t.Cleanup(func() {
		for _, c := range connections {
			_ = c.Close()
		}
	})
	for range 16 {
		c, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		require.NoError(t, err)
		connections = append(connections, c)
	}
	extra, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	require.NoError(t, err)
	defer func() { _ = extra.Close() }()
	require.NoError(t, extra.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, err = fmt.Fprint(extra, "GET /unavailable HTTP/1.1\r\nHost: daemon.example\r\n\r\n")
	require.NoError(t, err)
	_, err = bufio.NewReader(extra).ReadByte()
	require.Error(t, err, "seventeenth connection waits for admission")
	for _, c := range connections {
		_ = c.Close()
	}
	_ = extra.Close()
	cancel()
	require.NoError(t, <-done)
}

func TestRequiredSigningPreservesLocalBearerLeaseAuthority(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Auth.Token = "owner-bearer"; cfg.FederationSigningRequired = true })
	project, issue := createClaimHubIssueNamed(t, env, "hub-project")
	resp, raw := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease", project.ID, issue.ShortID), nil, map[string]string{"Authorization": "Bearer owner-bearer"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestOptionalSigningRejectsEmptySignatureHeader(t *testing.T) {
	t.Setenv("TEST_OPTIONAL_KEY", strings.Repeat("k", 64))
	state := filepath.Join(t.TempDir(), "state")
	require.NoError(t, federationsigning.InitializeReplayState(state))
	source := federationsigning.Source{KeyID: "key-a", KeyEnv: "TEST_OPTIONAL_KEY"}
	v, err := federationsigning.NewVerifier("https://daemon.example", []federationsigning.Key{{Source: source, EnrollmentID: 1}}, state)
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.FederationSigning = v })
	project := createFederatedHubProject(t, env, "hub-project")
	enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "enrollment", SpokeInstanceUID: federationTestSpokeUID, ProjectID: &project.ID, Capabilities: "pull", Actor: "example-actor"})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/federation/metadata", project.ID)
	resp, raw := envDoRaw(t, env, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer " + enrollment.Token})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	for _, header := range []string{"Signature", "Signature-Input", "Content-Digest"} {
		resp, raw = envDoRaw(t, env, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer " + enrollment.Token, header: ""})
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))
	}
}
