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
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationIngressAlwaysRequiresSigning(t *testing.T) {
	t.Setenv("TEST_INGRESS_POLICY_KEY", strings.Repeat("k", 64))
	source := federationsigning.Source{KeyID: "key-a", KeyEnv: "TEST_INGRESS_POLICY_KEY"}
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
		v, err := federationsigning.NewVerifier("https://hub.example", []federationsigning.Key{
			{Source: source, EnrollmentID: enrollment.Enrollment.ID},
		}, state)
		require.NoError(t, err)
		defer func() { require.NoError(t, v.Close()) }()
		time.Sleep(federationsigning.Quarantine)
		srv := daemon.NewServer(daemon.ServerConfig{DB: env.DB, FederationSigning: v})
		ingress, err := srv.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerFederation})
		require.NoError(t, err)
		path := "https://hub.example" + projectPath(project.ID) + "/federation/metadata"
		for _, tc := range []struct {
			name    string
			handler http.Handler
			signed  bool
			want    int
		}{
			{"private unsigned", srv.Handler(), false, http.StatusOK},
			{"ingress unsigned", ingress, false, http.StatusUnauthorized},
			{"ingress signed", ingress, true, http.StatusOK},
		} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+enrollment.Token)
			if tc.signed {
				require.NoError(t, federationsigning.Sign(req, source))
			}
			w := httptest.NewRecorder()
			tc.handler.ServeHTTP(w, req)
			require.Equal(t, tc.want, w.Code, "%s: %s", tc.name, w.Body.String())
		}
	})
}

// Two real ingest handlers wait for upload bytes while metadata and leases use
// the same daemon's other admission pool. Channels control upload progress.
func TestFederationSigningUploadAdmissionPreservesControlRequests(t *testing.T) {
	t.Setenv("TEST_ADMISSION_KEY", strings.Repeat("k", 64))
	source := federationsigning.Source{KeyID: "key-a", KeyEnv: "TEST_ADMISSION_KEY"}
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
		v, err := federationsigning.NewVerifier("https://hub.example", []federationsigning.Key{
			{Source: source, EnrollmentID: enrollment.Enrollment.ID},
		}, state)
		require.NoError(t, err)
		defer func() { require.NoError(t, v.Close()) }()
		time.Sleep(federationsigning.Quarantine)
		srv := daemon.NewServer(daemon.ServerConfig{DB: env.DB, FederationSigning: v, FederationSigningRequired: true})
		ingress, err := srv.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerFederation})
		require.NoError(t, err)
		request := func(method, path string, body []byte) *http.Request {
			req := httptest.NewRequest(method, "https://hub.example"+path, bytes.NewReader(body))
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
