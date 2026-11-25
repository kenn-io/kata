package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"golang.org/x/sync/semaphore"
)

func TestUnsignedFederationPushReadDeadline(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, err := store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(t.Context(), project.ID, "tester")
	require.NoError(t, err)
	enrollment, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "enrollment", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", ProjectID: &project.ID, Capabilities: "push", Actor: "tester"})
	require.NoError(t, err)
	const budget = 8
	s := &Server{cfg: ServerConfig{DB: store}, federationPushMemory: semaphore.NewWeighted(budget)}
	server := httptest.NewServer(s.withFederationIngestPreauthorization(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	t.Cleanup(server.Close)
	for _, suffix := range []string{"events:ingest", "relay:accept"} {
		t.Run(suffix, func(t *testing.T) {
			t.Parallel()
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(7*time.Second)))
			_, err = fmt.Fprintf(conn, "POST /api/v1/projects/%d/federation/%s HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer %s\r\nContent-Length: 8\r\n\r\nx", project.ID, suffix, enrollment.Token)
			require.NoError(t, err)
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
	t.Cleanup(func() {
		require.True(t, s.federationPushMemory.TryAcquire(budget))
		s.federationPushMemory.Release(budget)
	})
}

type failingPushBody struct{ sent bool }

func (b *failingPushBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("connection reset")
	}
	b.sent = true
	return copy(p, "12"), nil
}
func (*failingPushBody) Close() error { return nil }

// Signed and unsigned federation pushes share one body-byte budget. Bytes are
// charged as they arrive after authorization and returned on every exit.
func TestFederationPushMemoryIsSharedAndReleased(t *testing.T) {
	t.Setenv("TEST_PUSH_MEMORY_KEY", strings.Repeat("k", 64))
	source := signingpolicy.Source{KeyID: "key-a", KeyEnv: "TEST_PUSH_MEMORY_KEY"}
	store := openClaimGateHelperDB(t)
	project, err := store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(t.Context(), project.ID, "tester")
	require.NoError(t, err)
	enrollment, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "enrollment", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", ProjectID: &project.ID, Capabilities: "push", Actor: "tester"})
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "replay.state")
		require.NoError(t, federationsigning.InitializeReplayState(state))
		v, err := federationsigning.NewVerifier("https://hub.example", []signingpolicy.Key{{Source: source, EnrollmentID: enrollment.Enrollment.ID}}, state)
		require.NoError(t, err)
		defer func() { require.NoError(t, v.Close()) }()
		time.Sleep(federationsigning.Quarantine)
		api := huma.NewAPI(huma.DefaultConfig("fixture", "1"), humago.NewAdapter(http.NewServeMux(), ""))
		huma.Register(api, huma.Operation{OperationID: "ingestFederationProjectEvents", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/federation/events:ingest"}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		huma.Register(api, huma.Operation{OperationID: "createCronJob", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/cron/jobs"}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		huma.Register(api, huma.Operation{OperationID: "replaceCronJob", Method: http.MethodPut, Path: "/api/v1/projects/{project_id}/cron/jobs/{cron_uid}"}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		huma.Register(api, huma.Operation{OperationID: "archiveCronJob", Method: http.MethodDelete, Path: "/api/v1/projects/{project_id}/cron/jobs/{cron_uid}"}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		huma.Register(api, huma.Operation{OperationID: "restoreCronJob", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/cron/jobs/{cron_uid}/restore"}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		const budget = 8
		s := &Server{api: api, cfg: ServerConfig{DB: store, FederationSigning: v}, federationIngestAdmission: make(chan struct{}, 2), federationPushMemory: semaphore.NewWeighted(budget)}
		release := make(chan struct{})
		// Listener order: signing, then ingest preauthorization, then routes.
		handler := s.withFederationSigning(s.withFederationIngestPreauthorization(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.ReadAll(r.Body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Header.Get("X-Fixture-Hold") != "" {
				<-release
			}
			w.WriteHeader(http.StatusNoContent)
		})), false)
		ingest := fmt.Sprintf("/api/v1/projects/%d/federation/events:ingest", project.ID)
		request := func(kind, body string) *http.Request {
			path := ingest
			if kind == "cron" {
				path = "/api/v1/projects/1/cron/jobs"
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.example"+path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			if kind != "cron" {
				r.Header.Set("Authorization", "Bearer "+enrollment.Token)
			}
			if kind == "signed" || kind == "chunked" {
				require.NoError(t, federationsigning.Sign(r, source))
			}
			if kind == "chunked" {
				r.ContentLength = -1
			}
			return r
		}
		serve := func(r *http.Request) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w
		}
		requireFree := func(msg string) {
			t.Helper()
			require.True(t, s.federationPushMemory.TryAcquire(budget), msg)
			s.federationPushMemory.Release(budget)
		}
		for _, kind := range []string{"signed", "chunked", "unsigned"} {
			require.Equal(t, http.StatusNoContent, serve(request(kind, "1234")).Code, kind)
			requireFree(kind + " success")
			w := serve(request(kind, "123456789"))
			require.Equal(t, http.StatusTooManyRequests, w.Code, "%s: %s", kind, w.Body.String())
			require.Equal(t, "1", w.Header().Get("Retry-After"))
			requireFree(kind + " full")
			r := request(kind, "1234")
			r.ContentLength = federationsigning.MaxBodyBytes + 1
			require.Equal(t, http.StatusRequestEntityTooLarge, serve(r).Code, kind)
			requireFree(kind + " too large")
			r = request(kind, "1234")
			r.Body = &failingPushBody{}
			require.NotEqual(t, http.StatusNoContent, serve(r).Code, kind)
			requireFree(kind + " bad body")
		}

		// A push that declares a large body but sends nothing holds no budget.
		idle, idleWriter := io.Pipe()
		r := request("unsigned", "1234")
		r.Body, r.ContentLength = idle, 1<<20
		idleDone := make(chan int)
		go func() { idleDone <- serve(r).Code }()
		synctest.Wait()
		requireFree("idle push")

		// Held signed, chunked, and unsigned pushes fill the budget together; the
		// next push is refused until one finishes.
		held := make(chan int, 3)
		for kind, body := range map[string]string{"signed": "123", "chunked": "12", "unsigned": "123"} {
			r := request(kind, body)
			r.Header.Set("X-Fixture-Hold", "yes")
			go func() { held <- serve(r).Code }()
		}
		synctest.Wait()
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/projects/1/cron/jobs"},
			{http.MethodPut, "/api/v1/projects/1/cron/jobs/job"},
			{http.MethodDelete, "/api/v1/projects/1/cron/jobs/job"},
			{http.MethodPost, "/api/v1/projects/1/cron/jobs/job/restore"},
		} {
			r := httptest.NewRequestWithContext(t.Context(), tc.method, "https://hub.example"+tc.path, strings.NewReader("1"))
			require.Equal(t, http.StatusNoContent, serve(r).Code, "%s %s", tc.method, tc.path)
		}
		require.Equal(t, http.StatusTooManyRequests, serve(request("unsigned", "1")).Code)
		close(release)
		for range 3 {
			require.Equal(t, http.StatusNoContent, <-held)
		}
		require.Equal(t, http.StatusNoContent, serve(request("cron", "1")).Code)
		require.NoError(t, idleWriter.CloseWithError(errors.New("member went away")))
		require.NotEqual(t, http.StatusNoContent, <-idleDone)
		requireFree("all pushes finished")
	})
}
