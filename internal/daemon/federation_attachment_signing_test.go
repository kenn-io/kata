package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
)

// These requests deliberately fail authorization after admission. That lets the
// middleware's route selection, body cap and deadline run without a database.
func TestFederationAttachmentSigningProfile(t *testing.T) {
	for _, tc := range []struct {
		name, operation, method, path string
		length                        int64
		wantStatus                    int
		bulk                          bool
	}{
		{"upload", "uploadFederatedDocbank", "POST", "/api/v1/projects/1/federation/docbank/issues/abc4/uploads", 128<<20 + 1, 401, true},
		{"upload overflow", "uploadFederatedDocbank", "POST", "/api/v1/projects/1/federation/docbank/issues/abc4/uploads", 256<<20 + 1, 413, true},
		{"content", "federatedDocbankContent", "GET", "/api/v1/projects/1/federation/docbank/versions/6f1c2e8a-2222-4222-8222-222222222222/content", 0, 401, true},
		{"content body cap", "federatedDocbankContent", "GET", "/api/v1/projects/1/federation/docbank/versions/6f1c2e8a-2222-4222-8222-222222222222/content", 64<<10 + 1, 413, true},
		{"link", "linkFederatedDocbank", "POST", "/api/v1/projects/1/federation/docbank/issues/abc4/links", 64<<10 + 1, 413, false},
		{"capabilities", "federatedDocbankCapabilities", "GET", "/api/v1/projects/1/federation/docbank/capabilities", 0, 401, false},
		{"ordinary", "getFederationProjectMetadata", "GET", "/api/v1/projects/1/federation/metadata", 64<<10 + 1, 413, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := huma.NewAPI(huma.DefaultConfig("fixture", "1"), humago.NewAdapter(http.NewServeMux(), ""))
			path := strings.Replace(tc.path, "/projects/1/", "/projects/{project_id}/", 1)
			huma.Register(api, huma.Operation{OperationID: tc.operation, Method: tc.method, Path: path}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
			s := &Server{api: api, federationControlAdmission: make(chan struct{}, 1), federationIngestAdmission: make(chan struct{}, 1), federationTransferAdmission: make(chan struct{}, 1)}
			// Saturate the opposite pool: binary transfers must avoid control
			// slots, while links/capabilities retain control admission.
			if tc.bulk {
				s.federationControlAdmission <- struct{}{}
				s.federationIngestAdmission <- struct{}{}
			} else {
				s.federationTransferAdmission <- struct{}{}
			}
			r := httptest.NewRequestWithContext(t.Context(), tc.method, "https://hub.example"+tc.path, http.NoBody)
			r.ContentLength = tc.length
			w := &transferDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			before := time.Now()
			s.withFederationSigning(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unauthorized handler called") }), true).ServeHTTP(w, r)
			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			budget := time.Minute
			if tc.bulk {
				budget = 75 * time.Minute
			}
			require.WithinDuration(t, before.Add(budget), w.readDeadline, time.Second)
			require.Equal(t, w.readDeadline, w.writeDeadline)
		})
	}
}

type transferDeadlineRecorder struct {
	*httptest.ResponseRecorder
	readDeadline, writeDeadline time.Time
}

func (w *transferDeadlineRecorder) SetReadDeadline(d time.Time) error { w.readDeadline = d; return nil }
func (w *transferDeadlineRecorder) SetWriteDeadline(d time.Time) error {
	w.writeDeadline = d
	return nil
}

// Contract: verified binary spools survive through the handler and disappear
// when it returns, and enrollment revocation during upload prevents dispatch.
func TestFederationAttachmentSigningSpoolLifetime(t *testing.T) {
	for _, mode := range []string{"spool", "revoke"} {
		t.Run(mode, func(t *testing.T) { testFederationAttachmentSigningMode(t, mode) })
	}
}

func TestFederationAttachmentSigningPushOnlyPreflight(t *testing.T) {
	testFederationAttachmentSigningMode(t, "push-only preflight")
}

func testFederationAttachmentSigningMode(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TEST_ATTACHMENT_SIGNING_KEY", strings.Repeat("k", 64))
	source := signingpolicy.Source{KeyID: "attachment-key", KeyEnv: "TEST_ATTACHMENT_SIGNING_KEY"}
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "fixture.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	project, err := store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(t.Context(), project.ID, "tester")
	require.NoError(t, err)
	capabilities := "push,pull"
	if mode == "push-only preflight" {
		capabilities = "push"
	}
	enrollment, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "enrollment", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", ProjectID: &project.ID, Capabilities: capabilities, Actor: "tester"})
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		state := filepath.Join(stateDir, "replay.state")
		require.NoError(t, federationsigning.InitializeReplayState(state))
		v, err := federationsigning.NewVerifier("https://hub.example", []signingpolicy.Key{{Source: source, EnrollmentID: enrollment.Enrollment.ID}}, state)
		require.NoError(t, err)
		defer func() { require.NoError(t, v.Close()) }()
		time.Sleep(federationsigning.Quarantine)
		api := huma.NewAPI(huma.DefaultConfig("fixture", "1"), humago.NewAdapter(http.NewServeMux(), ""))
		method, operation := http.MethodPost, "uploadFederatedDocbank"
		route := "/api/v1/projects/{project_id}/federation/docbank/issues/abc4/uploads"
		path := fmt.Sprintf("/api/v1/projects/%d/federation/docbank/issues/abc4/uploads", project.ID)
		if mode == "push-only preflight" {
			method, operation = http.MethodGet, "federatedDocbankCapabilities"
			route = "/api/v1/projects/{project_id}/federation/docbank/capabilities"
			path = fmt.Sprintf("/api/v1/projects/%d/federation/docbank/capabilities", project.ID)
		}
		huma.Register(api, huma.Operation{OperationID: operation, Method: method, Path: route}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		s := &Server{api: api, cfg: ServerConfig{DB: store, FederationSigning: v}, federationControlAdmission: make(chan struct{}, 1), federationIngestAdmission: make(chan struct{}, 1), federationTransferAdmission: make(chan struct{}, 1)}
		var body io.Reader = strings.NewReader("binary")
		if mode == "push-only preflight" {
			body = nil
		}
		r := httptest.NewRequestWithContext(t.Context(), method, "https://hub.example"+path, body)
		r.Header.Set("Authorization", "Bearer "+enrollment.Token)
		if mode != "push-only preflight" {
			r.Header.Set("Content-Type", "application/octet-stream")
		}
		require.NoError(t, federationsigning.Sign(r, source))
		sentBody := r.Body
		defer func() { _ = sentBody.Close() }()
		if mode == "revoke" {
			r.Body = &attachmentSigningRevocationBody{Reader: strings.NewReader("binary"), revoke: func() { require.NoError(t, store.RevokeFederationEnrollment(t.Context(), enrollment.Enrollment.ID)) }}
			require.NoError(t, sentBody.Close())
		}
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			called = true
			if mode != "push-only preflight" {
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, "binary", string(body))
				files, err := os.ReadDir(dir)
				require.NoError(t, err)
				require.Len(t, files, 1, "verified spool must remain open through handler")
			}
			w.WriteHeader(http.StatusNoContent)
		})
		w := httptest.NewRecorder()
		s.withFederationSigning(next, true).ServeHTTP(w, r)
		if mode == "revoke" {
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.False(t, called)
		} else {
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
			require.True(t, called)
		}
		files, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Empty(t, files, "handler return or rejected authority must clean spool")
	})
}

type attachmentSigningRevocationBody struct {
	*strings.Reader
	revoke func()
	once   sync.Once
}

func (b *attachmentSigningRevocationBody) Read(p []byte) (int, error) {
	b.once.Do(b.revoke)
	return b.Reader.Read(p)
}
func (*attachmentSigningRevocationBody) Close() error { return nil }
