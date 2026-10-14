package daemon

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/db"
)

func TestBufferedScopedResponseBoundsMemoryForLargeBodies(t *testing.T) {
	const memoryLimit = 1 << 20
	body := bytes.Repeat([]byte("r"), memoryLimit+1)
	destination := httptest.NewRecorder()
	response := newBufferedScopedResponse(destination)
	t.Cleanup(func() { _ = response.close() })
	response.Header().Set("Content-Type", "application/octet-stream")
	response.WriteHeader(http.StatusAccepted)
	if _, err := response.Write(body); err != nil {
		t.Fatalf("write buffered response: %v", err)
	}
	if response.body.Len() > memoryLimit {
		t.Fatalf("buffer retained %d bytes in memory, want at most %d", response.body.Len(), memoryLimit)
	}
	if err := response.prepare(); err != nil {
		t.Fatalf("prepare buffered response: %v", err)
	}

	response.writeTo(destination)
	if destination.Code != http.StatusAccepted {
		t.Fatalf("response status = %d, want %d", destination.Code, http.StatusAccepted)
	}
	if destination.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("response content type = %q", destination.Header().Get("Content-Type"))
	}
	if !bytes.Equal(destination.Body.Bytes(), body) {
		t.Fatalf("response body did not round-trip: got %d bytes, want %d", destination.Body.Len(), len(body))
	}
	spillPath := response.spill.Name()
	if err := response.close(); err != nil {
		t.Fatalf("close spilled response: %v", err)
	}
	if _, err := os.Stat(spillPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spilled response file still exists: %v", err)
	}
}

// changingProjectAccessRevisionStore answers each successive revision and
// anonymous project-list read from the next entry, so a test can change
// policy between request admission and the response gate.
type changingProjectAccessRevisionStore struct {
	projectAuthorizationProbeStore
	revisions  []int64
	accessible [][]string
}

func (s *changingProjectAccessRevisionStore) ProjectAccessRevision(context.Context) (int64, error) {
	s.revisionReads++
	index := min(s.revisionReads-1, len(s.revisions)-1)
	return s.revisions[index], nil
}

func (s *changingProjectAccessRevisionStore) AnonymousAccessibleProjectUIDs(context.Context) ([]string, error) {
	s.anonymousUIDReads++
	index := min(s.anonymousUIDReads-1, len(s.accessible)-1)
	return s.accessible[index], nil
}

// ProjectByUID reports every admitted project as still existing, so losing
// one is a revocation rather than a project the request itself removed.
func (s *changingProjectAccessRevisionStore) ProjectByUID(_ context.Context, uid string) (db.Project, error) {
	return db.Project{UID: uid}, nil
}

const spilledResponseCanary = "private-response-canary"

// serveSpilledProjectRead runs one anonymous project read whose body exceeds
// the in-memory buffer, with every temporary file confined to tempDir.
func serveSpilledProjectRead(t *testing.T, store *changingProjectAccessRevisionStore) (*httptest.ResponseRecorder, string, string) {
	t.Helper()
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	payload := strings.Repeat(spilledResponseCanary, (1<<20)/len(spilledResponseCanary)+1)
	handler := withProjectAuthorization(store, true, false, selfAuthenticatedRouteMatcher{}, srv.noProjectDataRoutes, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	return response, payload, tempDir
}

func requireNoTemporaryResponseFiles(t *testing.T, tempDir, when string) {
	t.Helper()
	files, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temporary response directory: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("temporary response files remain after %s: %v", when, files)
	}
}

// A caller that loses its admitted project while a large read runs receives
// 404, never the buffered body, and leaves no spilled file behind.
func TestProjectAuthorizationDiscardsAndCleansSpilledResponseAfterRevocation(t *testing.T) {
	store := &changingProjectAccessRevisionStore{
		revisions:  []int64{1, 2},
		accessible: [][]string{{"spoke-project-a"}, {}},
	}
	response, _, tempDir := serveSpilledProjectRead(t, store)

	if response.Code != http.StatusNotFound {
		t.Fatalf("stale response status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if strings.Contains(response.Body.String(), spilledResponseCanary) {
		t.Fatal("stale project response body was written")
	}
	requireNoTemporaryResponseFiles(t, tempDir, "rejection")
}

// A policy change that leaves the caller's admitted project in place does not
// withhold the read, and the spilled file is removed once the body is sent.
func TestProjectAuthorizationSendsSpilledResponseAfterUnrelatedPolicyChange(t *testing.T) {
	store := &changingProjectAccessRevisionStore{
		revisions:  []int64{1, 2},
		accessible: [][]string{{"spoke-project-a"}, {"spoke-project-a", "spoke-project-b"}},
	}
	response, payload, tempDir := serveSpilledProjectRead(t, store)

	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Body.String() != payload {
		t.Fatalf("response body did not round-trip: got %d bytes, want %d", response.Body.Len(), len(payload))
	}
	if store.anonymousUIDReads != 2 {
		t.Fatalf("anonymous project reads = %d, want 2 (admission and refresh)", store.anonymousUIDReads)
	}
	requireNoTemporaryResponseFiles(t, tempDir, "a successful send")
}
