package storageadmin

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/testenv"
)

func TestResolveArtifactRejectsPathEscape(t *testing.T) {
	admin, err := New(Config{Root: t.TempDir(), SourceDSN: "source.db"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	for _, name := range []string{"../outside.jsonl", "/tmp/outside.jsonl", "a/../../outside.jsonl"} {
		_, err := admin.ResolveArtifact(name, false)
		require.Error(t, err, name)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	store, err := storeopen.Open(t.Context(), source)
	require.NoError(t, err)
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Imported issue", Author: "example-agent"})
	require.NoError(t, err)
	require.NoError(t, store.Close())

	admin, err := New(Config{Root: root, SourceDSN: source, Targets: map[string]string{"restore": "restore.db"}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	exported, err := admin.Export(t.Context(), ExportOptions{Artifact: "backup.jsonl", IncludeDeleted: true})
	require.NoError(t, err)
	require.Positive(t, exported.Bytes)
	imported, err := admin.Import(t.Context(), ImportOptions{Artifact: "backup.jsonl", Target: "restore"})
	require.NoError(t, err)
	require.Equal(t, "sqlite", imported.Backend)

	restored, err := storeopen.OpenReadOnly(context.Background(), filepath.Join(root, "restore.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = restored.Close() })
	restoredProject, err := restored.ProjectByName(t.Context(), "example-project")
	require.NoError(t, err)
	issues, err := restored.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: restoredProject.ID})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	require.Equal(t, "Imported issue", issues[0].Title)
	if runtime.GOOS != "windows" {
		// Owner-only Unix modes are not meaningful on Windows (ACL-based).
		restoredInfo, err := os.Stat(filepath.Join(root, "restore.db"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), restoredInfo.Mode().Perm())
	}

	_, err = New(Config{Root: root, SourceDSN: "sqlite://" + source, Targets: map[string]string{"active": "source.db"}})
	require.ErrorContains(t, err, "active SQLite storage")
}

func TestNewRejectsImportTargetOverlappingActiveSQLiteSidecar(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	store, err := storeopen.Open(t.Context(), source)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	_, err = New(Config{
		Root: root, SourceDSN: source,
		Targets: map[string]string{"sidecar": "source.db-wal"},
	})
	require.ErrorContains(t, err, "active SQLite storage")
}

func TestResolveArtifactRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, root+"/escape"))
	admin, err := New(Config{Root: root, SourceDSN: "source.db"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	_, err = admin.ResolveArtifact("escape/data.jsonl", false)
	require.ErrorContains(t, err, "symbolic link")
}

func TestRootAnchoredOperationRejectsDirectorySymlinkSwap(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "exchange"), 0o700))
	outside := t.TempDir()
	admin, err := New(Config{Root: root, SourceDSN: "source.db"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	name, err := admin.resolveArtifactName(filepath.Join("exchange", "artifact.jsonl"), false)
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(root, "exchange"), filepath.Join(root, "moved")))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "exchange")))

	file, err := admin.files.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if file != nil {
		_ = file.Close()
	}
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(outside, "artifact.jsonl"))
}

func TestImportRejectsConfiguredTargetDirectorySymlinkSwap(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "exchange"), 0o700))
	outside := t.TempDir()
	source := filepath.Join(root, "source.db")
	store, err := storeopen.Open(t.Context(), source)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := New(Config{
		Root: root, SourceDSN: source,
		Targets: map[string]string{"restore": filepath.Join("exchange", "restore.db")},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	_, err = admin.Export(t.Context(), ExportOptions{Artifact: "backup.jsonl", IncludeDeleted: true})
	require.NoError(t, err)

	require.NoError(t, os.Rename(filepath.Join(root, "exchange"), filepath.Join(root, "moved")))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "exchange")))
	_, err = admin.Import(t.Context(), ImportOptions{Artifact: "backup.jsonl", Target: "restore"})
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(outside, "restore.db"))
}

func TestExportRejectsProtectedStoragePathsAndRequiresOverwriteConfirmation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	store, err := storeopen.Open(t.Context(), source)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	require.NoError(t, os.Link(source, filepath.Join(root, "source-alias.db")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "restore.db"), []byte("target"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "backup.jsonl"), []byte("old"), 0o600))

	admin, err := New(Config{
		Root: root, SourceDSN: source,
		Targets: map[string]string{"restore": "restore.db"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	for _, artifact := range []string{"source.db", "source-alias.db", "source.db-wal", "restore.db"} {
		_, err := admin.Export(t.Context(), ExportOptions{Artifact: artifact, Force: true, Confirm: "OVERWRITE ARTIFACT " + artifact})
		require.ErrorContains(t, err, "protected storage path", artifact)
	}
	_, err = admin.Export(t.Context(), ExportOptions{Artifact: "backup.jsonl"})
	require.ErrorContains(t, err, "force and exact confirmation")
	_, err = admin.Export(t.Context(), ExportOptions{Artifact: "backup.jsonl", Force: true, Confirm: "wrong"})
	require.ErrorContains(t, err, `confirm must equal "OVERWRITE ARTIFACT backup.jsonl"`)
	exported, err := admin.Export(t.Context(), ExportOptions{Artifact: "backup.jsonl", Force: true, Confirm: "OVERWRITE ARTIFACT backup.jsonl"})
	require.NoError(t, err)
	require.Positive(t, exported.Bytes)
}

func TestStoragePathsEquivalentWithCaseFolding(t *testing.T) {
	require.True(t, storagePathsEquivalent(
		filepath.Join("root", "Source.db"), filepath.Join("root", "source.db"), true,
	))
	require.False(t, storagePathsEquivalent(
		filepath.Join("root", "Source.db"), filepath.Join("root", "source.db"), false,
	))
}

func TestStorageProtectionCanonicalizesActiveSQLiteDirectory(t *testing.T) {
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "storage-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("create directory symlink: %v", err)
	}
	source := filepath.Join(alias, "source.db")
	store, err := storeopen.Open(t.Context(), source)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := New(Config{
		Root:      alias,
		SourceDSN: "sqlite://" + source,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	_, err = admin.Export(t.Context(), ExportOptions{
		Artifact: "source.db-wal",
		Force:    true,
		Confirm:  "OVERWRITE ARTIFACT source.db-wal",
	})
	require.ErrorContains(t, err, "protected storage path")

	_, err = New(Config{
		Root:      alias,
		SourceDSN: "sqlite://" + source,
		Targets:   map[string]string{"active": "source.db"},
	})
	require.ErrorContains(t, err, "active SQLite storage")
}

func TestConcurrentExportsCannotReplaceUnconfirmedArtifact(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	store, err := storeopen.Open(t.Context(), source)
	require.NoError(t, err)
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	for index := 0; index < 200; index++ {
		_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: project.ID, Title: "Concurrent export fixture", Author: "example-agent",
		})
		require.NoError(t, err)
	}
	require.NoError(t, store.Close())
	admin, err := New(Config{Root: root, SourceDSN: source})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	start := make(chan struct{})
	errors := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, exportErr := admin.Export(t.Context(), ExportOptions{Artifact: "concurrent.jsonl"})
			errors <- exportErr
		}()
	}
	close(start)
	group.Wait()
	close(errors)

	succeeded := 0
	failed := 0
	for exportErr := range errors {
		if exportErr == nil {
			succeeded++
		} else {
			failed++
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, failed)
}

func TestMoveSQLiteSetDoesNotReplaceConcurrentDestinationAndRollsBack(t *testing.T) {
	root := t.TempDir()
	from := filepath.Join(root, "from.db")
	to := filepath.Join(root, "to.db")
	require.NoError(t, os.WriteFile(from, []byte("source-main"), 0o600))
	require.NoError(t, os.WriteFile(from+"-wal", []byte("source-wal"), 0o600))

	links := 0
	err := moveSQLiteSetWithLink(from, to, func(oldPath, newPath string) error {
		links++
		if links == 2 {
			require.NoError(t, os.WriteFile(newPath, []byte("concurrent"), 0o600))
		}
		return os.Link(oldPath, newPath)
	})
	require.Error(t, err)
	require.FileExists(t, from)
	require.FileExists(t, from+"-wal")
	require.NoFileExists(t, to)
	concurrent, readErr := os.ReadFile(to + "-wal") //nolint:gosec // test-local path
	require.NoError(t, readErr)
	require.Equal(t, "concurrent", string(concurrent))
}

func TestStageSQLiteSetCleansMainFileWhenSidecarOpenFails(t *testing.T) {
	root := t.TempDir()
	sourceRoot := t.TempDir()
	source := filepath.Join(sourceRoot, "restore.db")
	require.NoError(t, os.WriteFile(source, []byte("database"), 0o600))
	if err := os.Symlink("restore.db-wal", source+"-wal"); err != nil {
		t.Skipf("create sidecar symlink loop: %v", err)
	}
	admin, err := New(Config{Root: root, SourceDSN: source})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	_, err = admin.stageSQLiteSet(source, ".", "staged")
	require.Error(t, err)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestImportRejectsArtifactOverlappingSQLiteTarget(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "backup.jsonl"), []byte("{}\n"), 0o600))
	for _, targetValue := range []string{"backup.jsonl", "backup.jsonl-wal"} {
		admin, err := New(Config{
			Root: root, SourceDSN: filepath.Join(t.TempDir(), "active.db"),
			Targets: map[string]string{"restore": targetValue},
		})
		require.NoError(t, err)
		_, err = admin.Import(t.Context(), ImportOptions{
			Artifact: "backup.jsonl", Target: "restore",
			Force: true, Confirm: "REPLACE STORAGE restore",
		})
		require.ErrorContains(t, err, "artifact must differ from the SQLite target", targetValue)
		require.NoError(t, admin.Close())
	}
	_, err := os.Stat(filepath.Join(root, "backup.jsonl"))
	require.NoError(t, err, "the artifact must survive the refused import")
}

func TestNewRejectsPairwiseOverlappingSQLiteTargets(t *testing.T) {
	root := t.TempDir()
	// Pairwise overlap is enforced even when the active source is not
	// SQLite, since force-importing one alias moves and deletes the whole
	// sidecar set of its target.
	for _, source := range []string{filepath.Join(t.TempDir(), "active.db"), "postgres://db.example/kata"} {
		_, err := New(Config{
			Root: root, SourceDSN: source,
			Targets: map[string]string{"primary": "restore.db", "secondary": "restore.db-wal"},
		})
		require.ErrorContains(t, err, `storage targets "primary" and "secondary" overlap`, source)
	}
	admin, err := New(Config{
		Root: root, SourceDSN: filepath.Join(t.TempDir(), "active.db"),
		Targets: map[string]string{"primary": "restore.db", "secondary": "other.db"},
	})
	require.NoError(t, err, "distinct targets remain accepted")
	require.NoError(t, admin.Close())
}

func TestFailedPostgresImportRemovesFreshSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_POSTGRES_SCHEMA", "mcp_restore_cleanup")

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "invalid.jsonl"), []byte("not-json\n"), 0o600))
	admin, err := New(Config{
		Root: root, SourceDSN: filepath.Join(root, "active.db"),
		Targets: map[string]string{"restore": dsn},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	_, err = admin.Import(t.Context(), ImportOptions{Artifact: "invalid.jsonl", Target: "restore"})
	require.Error(t, err)
	version, err := storeopen.PeekSchemaVersion(t.Context(), dsn)
	require.NoError(t, err)
	require.Zero(t, version)
}

func TestPostgresImportRejectsInitializedTargetWithoutMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_POSTGRES_SCHEMA", "mcp_restore_initialized")

	// Initialize the target so it is a real, non-fresh Kata database that
	// is not the active source.
	initialized, err := storeopen.Open(t.Context(), dsn)
	require.NoError(t, err)
	require.NoError(t, initialized.Close())
	version, err := storeopen.PeekSchemaVersion(t.Context(), dsn)
	require.NoError(t, err)
	require.NotZero(t, version)

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "backup.jsonl"), []byte("{}\n"), 0o600))
	sourcePath := filepath.Join(t.TempDir(), "active.db")
	source, err := storeopen.Open(t.Context(), sourcePath)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	admin, err := New(Config{Root: root, SourceDSN: sourcePath, Targets: map[string]string{"restore": dsn}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	_, err = admin.Import(t.Context(), ImportOptions{Artifact: "backup.jsonl", Target: "restore"})
	require.ErrorContains(t, err, "already contains a Kata schema")
	after, err := storeopen.PeekSchemaVersion(t.Context(), dsn)
	require.NoError(t, err)
	require.Equal(t, version, after, "the preflight rejection must not touch the initialized schema")
}

func TestPostgresImportRefusesActiveDatabaseByIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_POSTGRES_SCHEMA", "mcp_restore_identity")

	alias, swappable := swapPostgresHost(t, dsn)
	if !swappable {
		t.Skip("container DSN has no swappable localhost/127.0.0.1 host")
	}
	require.NotEqual(t, config.DBHash(dsn), config.DBHash(alias),
		"equivalent DSNs must hash differently for this test to exercise the identity guard")

	// Give the active source a persisted instance identity, then confirm
	// the alias spelling reaches the same server.
	active, err := storeopen.Open(t.Context(), dsn)
	require.NoError(t, err)
	require.NoError(t, active.Close())
	probe, probeErr := storeopen.OpenReadOnly(t.Context(), alias)
	if probeErr != nil {
		t.Skipf("alias host is not reachable in this environment: %v", probeErr)
	}
	require.NoError(t, probe.Close())

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "backup.jsonl"), []byte("{}\n"), 0o600))
	admin, err := New(Config{Root: root, SourceDSN: dsn, Targets: map[string]string{"restore": alias}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })

	_, err = admin.Import(t.Context(), ImportOptions{Artifact: "backup.jsonl", Target: "restore"})
	require.ErrorContains(t, err, "must differ from the active daemon storage")
	version, err := storeopen.PeekSchemaVersion(t.Context(), dsn)
	require.NoError(t, err)
	require.NotZero(t, version, "identity refusal must not remove the active schema")
}

func swapPostgresHost(t *testing.T, dsn string) (string, bool) {
	t.Helper()
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	port := parsed.Port()
	switch parsed.Hostname() {
	case "localhost":
		parsed.Host = "127.0.0.1"
	case "127.0.0.1":
		parsed.Host = "localhost"
	default:
		return "", false
	}
	if port != "" {
		parsed.Host += ":" + port
	}
	return parsed.String(), true
}
