package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

// A scheduled full snapshot restores both projects and soft-deleted issues.
func TestDaemonScheduledBackupRestoresAllProjectsAndDeletedIssues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		home := deploymentHome(t)
		store := openKataTestDB(t, filepath.Join(home, "source.db"))
		defer func() { _ = store.Close() }()
		var originals []db.Issue
		for _, name := range []string{"first-project", "second-project"} {
			project, err := store.CreateProject(t.Context(), name)
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "restore this issue", Author: "tester"})
			require.NoError(t, err)
			originals = append(originals, issue)
		}
		_, _, _, err := store.SoftDeleteIssue(t.Context(), originals[1].ID, "tester")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		workers := &daemonWorkerGroup{}
		root := filepath.Join(home, "backups")
		err = startDaemonBackups(ctx, workers, store, config.BackupConfig{Dir: root, Interval: "24h", Retain: "720h"}, "012345abcdef", log.New(io.Discard, "", 0))
		require.NoError(t, err)
		synctest.Wait()
		files, err := filepath.Glob(filepath.Join(root, "012345abcdef", "*.jsonl"))
		require.NoError(t, err)
		require.Len(t, files, 1)
		cancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer joinCancel()
		require.True(t, workers.Wait(joinCtx))
		file, err := os.Open(files[0])
		require.NoError(t, err)
		defer func() { _ = file.Close() }()
		if runtime.GOOS != "windows" {
			info, err := file.Stat()
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
		restored := openKataTestDB(t, filepath.Join(home, "restored.db"))
		defer func() { _ = restored.Close() }()
		require.NoError(t, jsonl.Import(t.Context(), file, restored))
		projects, err := restored.ListProjects(t.Context())
		require.NoError(t, err)
		require.Len(t, projects, 2)
		for _, original := range originals {
			got, err := restored.IssueByID(t.Context(), original.ID)
			require.NoError(t, err)
			require.Equal(t, original.UID, got.UID)
			require.Equal(t, original.Title, got.Title)
		}
		deleted, err := restored.IssueByID(t.Context(), originals[1].ID)
		require.NoError(t, err)
		require.NotNil(t, deleted.DeletedAt)
	})
}
