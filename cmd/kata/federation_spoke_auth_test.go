//go:build !windows

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationEnrollAnonymousSpokeIgnoresUnselectedCredentialFailure(t *testing.T) {
	for _, network := range []string{"tcp", "unix"} {
		for _, source := range []string{"environment", "workspace"} {
			t.Run(network+"/"+source, func(t *testing.T) {
				resetFlags(t)
				hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
				const spokeUID = "01HZZZZZZZZZZZZZZZZZZZZZ01"
				var mu sync.Mutex
				var paths, authorizations []string
				spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					paths = append(paths, r.URL.Path)
					authorizations = append(authorizations, r.Header.Get("Authorization"))
					mu.Unlock()
					switch r.URL.Path {
					case "/api/v1/ping":
						_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"test"}`)
					case "/api/v1/instance":
						_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, spokeUID)
					case "/api/v1/projects":
						_, _ = fmt.Fprint(w, `{"projects":[{"name":"spoke-project","uid":"01HZZZZZZZZZZZZZZZZZZZZZ02"}]}`)
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(spoke.Close)
				endpoint := spoke.URL
				if network == "unix" {
					socket, _ := unixConsumerEndpoint(t, spoke.URL)
					endpoint = "unix://" + socket
				}
				workspace := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("version = 1\n[project]\nname = \"spoke-project\"\n"), 0600))
				t.Setenv("KATA_SERVER", "")
				if source == "environment" {
					t.Setenv("KATA_SERVER", endpoint)
				} else {
					require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte(fmt.Sprintf("version = 1\n[server]\nurl = %q\n", endpoint)), 0600))
				}
				t.Setenv("KATA_AUTH_TOKEN", "")
				t.Setenv("KATA_AUTH_TOKEN_FILE", filepath.Join(t.TempDir(), "missing-ambient-token"))
				t.Setenv("EXAMPLE_HUB_TOKEN", "hub-token")
				cmd := newRootCmd()
				var output strings.Builder
				cmd.SetOut(&output)
				cmd.SetErr(&output)
				cmd.SetArgs([]string{"--workspace", workspace, "--project", "spoke-project", "federation", "enroll", "--spoke-instance", spokeUID, "--hub-url", hub.URL, "--hub-token-env", "EXAMPLE_HUB_TOKEN", "--actor", "operator"})

				require.NoError(t, cmd.Execute())
				assert.Contains(t, output.String(), "--adopt-existing")
				enrollments, err := hub.DB.ListFederationEnrollments(t.Context())
				require.NoError(t, err)
				require.Len(t, enrollments, 1)
				assert.True(t, enrollments[0].AllowAdoptionSnapshotAuthors)
				mu.Lock()
				defer mu.Unlock()
				assert.Contains(t, paths, "/api/v1/instance")
				assert.Contains(t, paths, "/api/v1/projects")
				for _, authorization := range authorizations {
					assert.Empty(t, authorization, "anonymous spoke probes must not inherit ambient or hub credentials")
				}
			})
		}
	}
}

func TestFederationEnrollSelectedSpokeCredentialFailureIsNotDiscarded(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	spoke := testenv.New(t)
	selectedFile := filepath.Join(t.TempDir(), "missing-selected-token")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_AUTH_TOKEN_FILE", "")
	t.Setenv("EXAMPLE_HUB_TOKEN", "hub-token")
	require.NoError(t, os.WriteFile(filepath.Join(spoke.Home, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname = \"spoke\"\nurl = %q\ntoken_file = %q\n", spoke.URL, selectedFile)), 0600))
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--daemon", "spoke", "--project", "spoke-project", "federation", "enroll", "--spoke-instance", spoke.DB.InstanceUID(), "--hub-url", hub.URL, "--hub-token-env", "EXAMPLE_HUB_TOKEN", "--actor", "operator"})

	err := cmd.Execute()
	require.ErrorIs(t, err, config.ErrCredentialSource)
	assert.Contains(t, err.Error(), selectedFile)
	enrollments, err := hub.DB.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	assert.Empty(t, enrollments)
}
