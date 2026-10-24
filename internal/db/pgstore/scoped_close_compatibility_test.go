package pgstore_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"

	"go.kenn.io/kata/internal/db/dbtest"
)

func TestScopedCloseLegacyNativeAdmission(t *testing.T) {
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunScopedCloseLegacyNativeAdmission(t, store)
}

func TestScopedCloseNativeEnvelopeAdmission(t *testing.T) {
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunScopedCloseNativeEnvelopeAdmission(t, store)
}

func TestScopedCloseLegacyMetadataNativeAdmission(t *testing.T) {
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunScopedCloseLegacyMetadataNativeAdmission(t, store)
}

func TestScopedOrdinaryCloseStandalone(t *testing.T) {
	open := func() db.Storage {
		store, _ := openConformanceStore(t)
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	source := open()
	project, err := source.CreateProject(t.Context(), "ordinary-project")
	require.NoError(t, err)
	copySnapshot := func() error {
		var raw bytes.Buffer
		if err := jsonl.Export(t.Context(), source, &raw, jsonl.ExportOptions{IncludeDeleted: true}); err != nil {
			return err
		}
		return jsonl.ImportWithOptions(t.Context(), &raw, open(), jsonl.ImportOptions{AsStandalone: true})
	}
	require.NoError(t, copySnapshot(), "ordinary standalone copy is a supported native path")
	issue, _, err := source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Ordinary close"})
	require.NoError(t, err)
	_, _, _, err = source.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: issue.ID, Actor: "member", Reason: "done"})
	require.NoError(t, err)
	require.NoError(t, copySnapshot(), "ordinary status cannot silently opt into v2 owner custody")
}
