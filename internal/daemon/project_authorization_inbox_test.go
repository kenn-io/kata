package daemon_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestProjectAccessCannotReassignInboxFromRestrictedProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.DesignateInboxProject(t.Context(), db.DesignateInboxProjectIn{
			ProjectID: f.private.ID,
			Actor:     "admin",
		})
		require.NoError(t, err)

		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/metadata", f.public.ID), "nonmember",
			map[string]any{"actor": "nonmember", "patch": map[string]string{"role": "inbox"}}, nil)
		assert.Equalf(t, http.StatusNotFound, status, "body: %s", body)

		previous, err := store.ProjectByID(t.Context(), f.private.ID)
		require.NoError(t, err)
		target, err := store.ProjectByID(t.Context(), f.public.ID)
		require.NoError(t, err)
		assert.JSONEq(t, `{"role":"inbox"}`, string(previous.Metadata),
			"a project user must not clear an Inbox designation from an inaccessible project")
		assert.JSONEq(t, `{}`, string(target.Metadata),
			"a rejected designation must not update the accessible target project")
	})
}

func TestProjectAccessCanReassignInboxBetweenAccessibleProjects(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		previous := f.public
		target, err := store.CreateProject(t.Context(), "example-workspace")
		require.NoError(t, err)
		_, err = store.DesignateInboxProject(t.Context(), db.DesignateInboxProjectIn{
			ProjectID: previous.ID,
			Actor:     "admin",
		})
		require.NoError(t, err)

		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/metadata", target.ID), "nonmember",
			map[string]any{"actor": "nonmember", "patch": map[string]string{"role": "inbox"}}, nil)
		assert.Equalf(t, http.StatusOK, status, "body: %s", body)

		previous, err = store.ProjectByID(t.Context(), previous.ID)
		require.NoError(t, err)
		target, err = store.ProjectByID(t.Context(), target.ID)
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(previous.Metadata))
		assert.JSONEq(t, `{"role":"inbox"}`, string(target.Metadata))
	})
}

func TestProjectAccessOwnerCanReassignInboxFromRestrictedProject(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ownerServer := daemon.NewServer(daemon.ServerConfig{
			DB:   store,
			Auth: config.AuthConfig{Token: "owner-test-token"},
		})
		t.Cleanup(func() { require.NoError(t, ownerServer.Close()) })
		ownerHTTPServer := httptest.NewServer(ownerServer.Handler())
		t.Cleanup(ownerHTTPServer.Close)
		f.server = ownerHTTPServer
		_, err := store.DesignateInboxProject(t.Context(), db.DesignateInboxProjectIn{
			ProjectID: f.private.ID,
			Actor:     "admin",
		})
		require.NoError(t, err)

		status, _, body := f.request(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%d/metadata", f.public.ID), "owner",
			map[string]any{"actor": "owner", "patch": map[string]string{"role": "inbox"}}, nil)
		assert.Equalf(t, http.StatusOK, status, "body: %s", body)

		previous, err := store.ProjectByID(t.Context(), f.private.ID)
		require.NoError(t, err)
		target, err := store.ProjectByID(t.Context(), f.public.ID)
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(previous.Metadata))
		assert.JSONEq(t, `{"role":"inbox"}`, string(target.Metadata))
	})
}
