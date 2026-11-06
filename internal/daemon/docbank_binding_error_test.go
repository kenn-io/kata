package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/testenv"
)

type failingDocbankBindingStore struct {
	db.Storage
	reads    atomic.Int32
	failAt   atomic.Int32
	injected atomic.Bool
}

func (s *failingDocbankBindingStore) FederationBindingByProject(ctx context.Context, projectID int64) (db.FederationBinding, error) {
	if n := s.reads.Add(1); n == s.failAt.Load() {
		s.injected.Store(true)
		return db.FederationBinding{}, errors.New("synthetic binding read failure")
	}
	return s.Storage.FederationBindingByProject(ctx, projectID)
}

// A transient binding failure must never turn a federated root write into an
// unmarked legacy attachment or bypass its configured exposure subtree.
func TestDocbankRootBindingLookupFailsClosed(t *testing.T) {
	for _, operation := range []string{"upload", "link"} {
		t.Run(operation, func(t *testing.T) {
			vault := docbanktest.New(t)
			var fault *failingDocbankBindingStore
			env := testenv.New(t, func(cfg *daemon.ServerConfig) {
				fault = &failingDocbankBindingStore{Storage: cfg.DB}
				cfg.DB = fault
				cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
				cfg.DocbankFolder = "/kata"
			})
			project, issue := seedProjectAndIssue(t, env)
			_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "operator")
			require.NoError(t, err)
			endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", project.ID, issue.UID)
			fault.reads.Store(0)
			var status int
			var body []byte
			if operation == "upload" {
				vault.BeforeResponse = func(method, path string) {
					if method == http.MethodPost && path == "/api/v1/uploads" {
						fault.failAt.Store(fault.reads.Load() + 1)
					}
				}
				status, body = attachmentUpload(t, env, endpoint, "file.pdf", strings.NewReader("fixture"))
			} else {
				version := uuid.MustParse(linkVersion)
				vault.Seed("/private/file.pdf", docbank.Node{ID: 42, Name: "file.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("fixture")})
				// The claim gate and routing lookup succeed; the repeated root lookup fails.
				fault.failAt.Store(3)
				status, body, err = postAttachmentLink(t, env, endpoint+"/links", `{"path":"/private/file.pdf","actor":"operator"}`)
				require.NoError(t, err)
			}
			require.True(t, fault.injected.Load(), "fixture must exercise the binding failure")
			require.Equal(t, http.StatusInternalServerError, status, string(body))
			stored, err := env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.NotContains(t, stored.Metadata, "attachment.", "failed origin decision must not persist a reference")
		})
	}
}
