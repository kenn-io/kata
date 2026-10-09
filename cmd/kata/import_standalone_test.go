package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestImportAsStandaloneDetachesCopyWithoutChangingRestore(t *testing.T) {
	home := setupKataEnv(t)
	ctx := t.Context()
	source := openKataTestDB(t, filepath.Join(home, "source.db"))
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	project, err := source.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = source.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	const apiToken = "synthetic-source-api-token"
	const federationToken = "synthetic-source-federation-token"
	_, _, err = source.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: apiToken, Actor: "user-a", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	_, err = source.CreateProjectFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token: federationToken, SpokeInstanceUID: source.InstanceUID(),
		ProjectID: &project.ID, Capabilities: "pull,push", Actor: "user-b",
	})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &exported, jsonl.ExportOptions{IncludeDeleted: true}))
	input := filepath.Join(home, "source.jsonl")
	require.NoError(t, os.WriteFile(input, exported.Bytes(), 0o600))

	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			for _, mode := range []string{"--as-standalone", "--new-instance"} {
				t.Run(mode, func(t *testing.T) {
					target := filepath.Join(home, mode+".db")
					if backend == "postgres" {
						if testing.Short() {
							t.Skip("requires postgres testcontainer")
						}
						dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
						t.Cleanup(cleanup)
						target = dsn
					}
					_, err := runCmdOutput(t, nil, "import", "--input", input, "--target", target, mode)
					require.NoError(t, err)
					copied, err := storeopen.Open(ctx, target)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, copied.Close()) })
					assert.NotEqual(t, source.InstanceUID(), copied.InstanceUID())
					got, err := copied.ProjectByName(ctx, project.Name)
					require.NoError(t, err)
					assert.Equal(t, project.UID, got.UID)
					_, apiErr := copied.ResolveAPIToken(ctx, apiToken)
					_, federationErr := copied.AuthorizeFederationToken(ctx, federationToken, got.ID, "pull")
					bindings, err := copied.ListFederationBindings(ctx)
					require.NoError(t, err)
					if mode == "--as-standalone" {
						assert.ErrorIs(t, apiErr, db.ErrNotFound)
						assert.Error(t, federationErr)
						assert.Empty(t, bindings)
						_, err = runCmdOutput(t, nil, "import", "--as-standalone", "--input", input, "--target", target)
						ce := requireCLIError(t, err, ExitValidation)
						assert.Contains(t, ce.Message, "choose another --target")
						assert.NotContains(t, ce.Message, "--force")
					} else {
						require.NoError(t, apiErr)
						require.NoError(t, federationErr)
						require.Len(t, bindings, 1)
					}
				})
			}
		})
	}

	got, err := os.ReadFile(input) //nolint:gosec // test-owned export under TempDir
	require.NoError(t, err)
	assert.Equal(t, exported.Bytes(), got, "the source export is read-only")
	_, err = source.ResolveAPIToken(ctx, apiToken)
	require.NoError(t, err, "copying must not revoke source authority")
	_, err = source.AuthorizeFederationToken(ctx, federationToken, project.ID, "pull")
	require.NoError(t, err)
}

func TestImportAsStandaloneRejectsReplacementAndMerge(t *testing.T) {
	setupKataEnv(t)
	for _, incompatible := range []string{"--force", "--merge"} {
		t.Run(incompatible, func(t *testing.T) {
			_, err := runCmdOutput(t, nil, "import", "--as-standalone", incompatible,
				"--input", "source.jsonl", "--target", "copy.db")
			ce := requireCLIError(t, err, ExitValidation)
			assert.Contains(t, ce.Message, "--as-standalone cannot be combined with "+incompatible)
		})
	}
}
