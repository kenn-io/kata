package kata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type spokeNameScopeAccess struct {
	projectID         int64
	allowGlobal       bool
	rejectGlobalFence bool
	globalRequests    int
	globalFences      int
}

func (a *spokeNameScopeAccess) Authorize(_ context.Context, request kata.AccessRequest) (kata.AccessDecision, error) {
	if request.Operation.ID != "createFederationEnrollment" {
		return kata.AccessDecision{}, kata.ErrAccessDenied
	}
	global := request.Operation.AllProjects
	if global {
		a.globalRequests++
		if !a.allowGlobal {
			return kata.AccessDecision{}, kata.ErrAccessDenied
		}
	} else if !slices.Equal(request.Operation.ProjectIDs, []int64{a.projectID}) {
		return kata.AccessDecision{}, kata.ErrAccessDenied
	}
	return kata.AccessDecision{TransactionFence: func(context.Context, kata.Transaction) error {
		if global {
			a.globalFences++
			if a.rejectGlobalFence {
				return kata.ErrAccessDenied
			}
		}
		return nil
	}}, nil
}

func TestServiceSpokeNameEnrollmentRequiresGlobalAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, spokeName          string
		allowGlobal, rejectFence bool
		status                   int
		finalName                string
		enrollments              int
	}{
		{name: "scoped unnamed", status: http.StatusOK, finalName: "laptop", enrollments: 2},
		{name: "scoped naming denied", spokeName: "desktop", status: http.StatusNotFound, finalName: "laptop", enrollments: 1},
		{name: "global fence denied", spokeName: "desktop", allowGlobal: true, rejectFence: true, status: http.StatusNotFound, finalName: "laptop", enrollments: 1},
		{name: "global naming allowed", spokeName: "desktop", allowGlobal: true, status: http.StatusOK, finalName: "desktop", enrollments: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			databasePath := filepath.Join(t.TempDir(), "service.db")
			store, err := sqlitestore.Open(t.Context(), databasePath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			project, err := store.CreateProject(t.Context(), "project-one")
			require.NoError(t, err)
			other, err := store.CreateProject(t.Context(), "project-two")
			require.NoError(t, err)
			const peer = "01HZNQ7VFPK1XGD8R5MABCD4EA"
			_, err = store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ProjectID: &other.ID, SpokeInstanceUID: peer, SpokeName: "laptop", Actor: "member", Capabilities: "pull"})
			require.NoError(t, err)
			access := &spokeNameScopeAccess{projectID: project.ID, allowGlobal: tc.allowGlobal, rejectGlobalFence: tc.rejectFence}
			service, err := kata.New(t.Context(), kata.Config{DSN: databasePath, Access: access})
			require.NoError(t, err)
			t.Cleanup(func() { _ = service.Close() })
			raw, err := json.Marshal(map[string]any{"project_id": project.ID, "spoke_instance_uid": peer, "spoke_name": tc.spokeName, "capabilities": "pull"})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/federation/enrollments", bytes.NewReader(raw))
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(kata.WithPrincipal(request.Context(), kata.Principal{Subject: "host-member", Actor: "member"}))
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			names, err := store.ListFederationSpokeNames(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.finalName, names[peer])
			grants, err := store.ListFederationEnrollments(t.Context())
			require.NoError(t, err)
			require.Len(t, grants, tc.enrollments)
			if tc.spokeName == "" {
				require.Zero(t, access.globalRequests)
			} else {
				require.Positive(t, access.globalRequests)
			}
			if tc.allowGlobal {
				require.Positive(t, access.globalFences)
			} else {
				require.Zero(t, access.globalFences)
			}
		})
	}
}
