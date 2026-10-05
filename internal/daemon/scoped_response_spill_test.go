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

type changingProjectAccessRevisionStore struct {
	projectAuthorizationProbeStore
	revisions []int64
}

func (s *changingProjectAccessRevisionStore) ProjectAccessRevision(context.Context) (int64, error) {
	s.revisionReads++
	index := min(s.revisionReads-1, len(s.revisions)-1)
	return s.revisions[index], nil
}

func TestProjectAuthorizationDiscardsAndCleansSpilledResponseAfterPolicyChange(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	store := &changingProjectAccessRevisionStore{
		revisions: []int64{1, 2},
	}
	payload := strings.Repeat("private-response-canary", (1<<20)/len("private-response-canary")+1)
	handler := withProjectAuthorization(store, true, false, selfAuthenticatedRouteMatcher{}, srv.noProjectDataRoutes, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("stale response status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if strings.Contains(response.Body.String(), "private-response-canary") {
		t.Fatal("stale project response body was written")
	}
	files, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temporary response directory: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("temporary response files remain after rejection: %v", files)
	}
}
