package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/testenv"
)

// Every listener authenticates enrollment and applies its signing policy.
func TestFederationSigningAdmission(t *testing.T) {
	t.Setenv("TEST_REQUIRED_POLICY_KEY", strings.Repeat("k", 64))
	source := signingpolicy.Source{KeyID: "key-a", KeyEnv: "TEST_REQUIRED_POLICY_KEY"}
	env := testenv.New(t)
	project := createFederatedHubProject(t, env, "hub-project")
	enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		Token: "enrollment", SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: "pull", Actor: "example-actor",
	})
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "replay.state")
		require.NoError(t, federationsigning.InitializeReplayState(state))
		v, err := federationsigning.NewVerifier("https://hub.example", []signingpolicy.Key{
			{Source: source, EnrollmentID: enrollment.Enrollment.ID},
		}, state)
		require.NoError(t, err)
		defer func() { require.NoError(t, v.Close()) }()
		time.Sleep(federationsigning.Quarantine)
		const digest = "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:"
		for _, required := range []bool{false, true} {
			srv := daemon.NewServer(daemon.ServerConfig{DB: env.DB, Auth: config.AuthConfig{Token: "owner-bearer"}, FederationSigning: v, FederationSigningRequired: required})
			main, err := srv.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerSharedTCP, Origin: "https://hub.example"})
			require.NoError(t, err)
			ingress, err := srv.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerFederation})
			require.NoError(t, err)
			for name, handler := range map[string]http.Handler{"main": main, "socket": srv.Handler(), "ingress": ingress} {
				if !required && name == "main" {
					continue
				}
				type admissionCase struct {
					name, token, header, value, code string
					signed                           bool
					status                           int
				}
				cases := []admissionCase{
					{"unsigned", enrollment.Token, "", "", "hub_signing_required", false, http.StatusPreconditionRequired},
				}
				if required {
					cases = append(cases,
						admissionCase{"digest only", enrollment.Token, "Content-Digest", digest, "hub_signing_required", false, http.StatusPreconditionRequired},
						admissionCase{"signature input only", enrollment.Token, "Signature-Input", "example", "signature_invalid", false, http.StatusUnauthorized},
						admissionCase{"unknown enrollment", "unknown-enrollment", "", "", "auth_invalid", false, http.StatusForbidden},
					)
				} else if name == "socket" {
					cases[0].status, cases[0].code = http.StatusOK, ""
					cases = append(cases, admissionCase{"digest only", enrollment.Token, "Content-Digest", digest, "", false, http.StatusOK})
				} else {
					cases = append(cases, admissionCase{"signed", enrollment.Token, "", "", "", true, http.StatusOK})
				}
				for _, tc := range cases {
					path := "https://hub.example" + projectPath(project.ID) + "/federation/metadata"
					req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
					req.Header.Set("Authorization", "Bearer "+tc.token)
					if tc.signed {
						require.NoError(t, federationsigning.Sign(req, source))
					}
					if tc.header != "" {
						req.Header.Set(tc.header, tc.value)
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, req)
					require.Equal(t, tc.status, w.Code, "required=%t/%s/%s: %s", required, name, tc.name, w.Body.String())
					if tc.code != "" {
						var envelope api.ErrorEnvelope
						require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
						require.Equal(t, tc.code, envelope.Error.Code, "required=%t/%s/%s", required, name, tc.name)
					}
				}
			}
		}
	})
}

// Two real ingest handlers wait for upload bytes while metadata and leases use
// the same daemon's other admission pool. Channels control upload progress.
func TestFederationSigningUploadAdmissionPreservesControlRequests(t *testing.T) {
	t.Setenv("TEST_ADMISSION_KEY", strings.Repeat("k", 64))
	source := signingpolicy.Source{KeyID: "key-a", KeyEnv: "TEST_ADMISSION_KEY"}
	env := testenv.New(t)
	project, issue := createClaimHubIssueNamed(t, env, "hub-project")
	enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		Token: "enrollment", SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: "pull,push,claim", Actor: "example-actor",
	})
	require.NoError(t, err)
	raw, err := json.Marshal(federationIngestBody())
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "replay.state")
		require.NoError(t, federationsigning.InitializeReplayState(state))
		v, err := federationsigning.NewVerifier("https://hub.example", []signingpolicy.Key{
			{Source: source, EnrollmentID: enrollment.Enrollment.ID},
		}, state)
		require.NoError(t, err)
		defer func() { require.NoError(t, v.Close()) }()
		time.Sleep(federationsigning.Quarantine)
		srv := daemon.NewServer(daemon.ServerConfig{DB: env.DB, FederationSigning: v, FederationSigningRequired: true})
		ingress, err := srv.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerFederation})
		require.NoError(t, err)
		request := func(method, path string, body []byte) *http.Request {
			req := httptest.NewRequestWithContext(t.Context(), method, "https://hub.example"+path, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+enrollment.Token)
			require.NoError(t, federationsigning.Sign(req, source))
			return req
		}
		path := projectPath(project.ID) + "/federation/events:ingest"
		entered, release := make(chan struct{}, 2), make(chan struct{})
		var unblock sync.Once
		defer unblock.Do(func() { close(release) })
		done := make(chan *httptest.ResponseRecorder, 2)
		for _, handler := range []http.Handler{srv.Handler(), ingress} {
			req := request(http.MethodPost, path, raw)
			req.Body = &pausedFederationUpload{Reader: bytes.NewReader(raw), entered: entered, release: release}
			go func() {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				done <- w
			}()
		}
		for range 2 {
			select {
			case <-entered:
			case w := <-done:
				t.Fatalf("upload ended before reading its body: %d %s", w.Code, w.Body.String())
			}
		}
		for _, controlPath := range []string{
			projectPath(project.ID) + "/federation/metadata",
			projectPath(project.ID) + "/federation/events?after=0&limit=10",
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease", project.ID, issue.ShortID),
		} {
			w := httptest.NewRecorder()
			ingress.ServeHTTP(w, request(http.MethodGet, controlPath, nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		}
		for _, action := range []string{"acquire", "renew", "release"} {
			leasePath := fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease/actions/%s", project.ID, issue.ShortID, action)
			body := []byte(`{"holder":"example-actor","client_kind":"cli","claim_kind":"timed","ttl_seconds":300,"purpose":"edit"}`)
			w := httptest.NewRecorder()
			ingress.ServeHTTP(w, request(http.MethodPost, leasePath, body))
			require.Equal(t, http.StatusOK, w.Code, "%s: %s", action, w.Body.String())
		}
		w := httptest.NewRecorder()
		ingress.ServeHTTP(w, request(http.MethodPost, path, raw))
		require.Equal(t, http.StatusTooManyRequests, w.Code)
		require.Equal(t, "1", w.Header().Get("Retry-After"))
		unblock.Do(func() { close(release) })
		for range 2 {
			w := <-done
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		}
		for _, knownLength := range []bool{true, false} {
			req := request(http.MethodGet, projectPath(project.ID)+"/federation/metadata", bytes.Repeat([]byte("x"), (64<<10)+1))
			if !knownLength {
				req.ContentLength = -1
			}
			w := httptest.NewRecorder()
			ingress.ServeHTTP(w, req)
			require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "known length %t: %s", knownLength, w.Body.String())
		}
	})
}

type pausedFederationUpload struct {
	*bytes.Reader
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *pausedFederationUpload) Read(p []byte) (int, error) {
	b.once.Do(func() { b.entered <- struct{}{} })
	<-b.release
	return b.Reader.Read(p)
}

func (*pausedFederationUpload) Close() error { return nil }

// Contract: on ordinary listeners, an auto-start idle deadline cannot expire
// while the daemon reads and verifies a signed federation upload.
func TestSignedUploadHoldsIdleForegroundDuringVerification(t *testing.T) {
	t.Setenv("TEST_IDLE_SIGNING_KEY", strings.Repeat("k", 64))
	source := signingpolicy.Source{KeyID: "key-a", KeyEnv: "TEST_IDLE_SIGNING_KEY"}
	env := testenv.New(t)
	project := createFederatedHubProject(t, env, "hub-project")
	enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		Token: "enrollment", SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: "pull,push", Actor: "example-actor",
	})
	require.NoError(t, err)
	raw, err := json.Marshal(federationIngestBody())
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "replay.state")
		require.NoError(t, federationsigning.InitializeReplayState(state))
		v, err := federationsigning.NewVerifier("https://hub.example", []signingpolicy.Key{
			{Source: source, EnrollmentID: enrollment.Enrollment.ID},
		}, state)
		require.NoError(t, err)
		defer func() { require.NoError(t, v.Close()) }()
		time.Sleep(federationsigning.Quarantine)
		idle := daemon.NewIdleController(time.Hour, func() {})
		idle.Start()
		defer idle.Stop()
		srv := daemon.NewServer(daemon.ServerConfig{DB: env.DB, FederationSigning: v, IdleAdmission: idle})
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"https://hub.example"+projectPath(project.ID)+"/federation/events:ingest", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+enrollment.Token)
		require.NoError(t, federationsigning.Sign(req, source))
		entered, release := make(chan struct{}, 1), make(chan struct{})
		req.Body = &pausedFederationUpload{Reader: bytes.NewReader(raw), entered: entered, release: release}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			done <- w
		}()
		<-entered
		require.Equal(t, daemon.IdleStateForeground, idle.Snapshot().State,
			"signed body verification must run under a foreground idle lease")
		close(release)
		w := <-done
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, daemon.IdleStateArmed, idle.Snapshot().State)
	})
}
