package docbank_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

func requireUnreachable(t *testing.T, err error) {
	t.Helper()
	apiErr, ok := errors.AsType[*docbank.Error](err)
	require.True(t, ok)
	require.Zero(t, apiErr.Status)
}

// publishedOnly finds only fakes the test published; a discovery regression must not launch a real Docbank.
var publishedOnly = docbank.Local{Start: func(context.Context, string) error {
	return errors.New("test must not start docbank")
}}

func TestLocalClientFollowsRestartedDocbank(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCBANK_HOME", home)
	a := docbanktest.New(t)
	a.Publish(t, home)
	client := docbank.NewLocalClient(publishedOnly)

	_, err := client.ResolvePath(t.Context(), "/")
	require.NoError(t, err)
	b := docbanktest.New(t)
	b.Key = "rotated-key"
	b.Publish(t, home)
	aSeen := len(a.Requests())
	_, err = client.ResolvePath(t.Context(), "/")
	require.NoError(t, err)
	require.Len(t, b.Requests(), 1)
	require.Len(t, a.Requests(), aSeen)
}

func TestLocalClientStartsStoppedDocbankInConfiguredHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCBANK_HOME", home)
	fake := docbanktest.New(t)
	var starts []string
	client := docbank.NewLocalClient(docbank.Local{Start: func(_ context.Context, h string) error {
		starts = append(starts, h)
		fake.Publish(t, h)
		return nil
	}})

	_, err := client.ResolvePath(t.Context(), "/")
	require.NoError(t, err)
	require.Equal(t, []string{home}, starts)
}

func TestLocalClientReportsStartFailureAsUnreachable(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	client := docbank.NewLocalClient(docbank.Local{Start: func(context.Context, string) error {
		return errors.New("docbank not installed")
	}})

	_, err := client.ResolvePath(t.Context(), "/")
	requireUnreachable(t, err)
}

func TestLocalClientNeverSendsKeyToImpostor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCBANK_HOME", home)
	impostor := docbanktest.New(t)
	impostor.Publish(t, home)
	impostor.Key = "not-the-recorded-key"
	client := docbank.NewLocalClient(publishedOnly)

	_, err := client.ResolvePath(t.Context(), "/")
	requireUnreachable(t, err)
	require.Empty(t, impostor.Requests())
}

func TestLocalClientFollowsSymlinkedHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	vault := t.TempDir()
	link := filepath.Join(t.TempDir(), "docbank-link")
	require.NoError(t, os.Symlink(vault, link))
	t.Setenv("DOCBANK_HOME", link)
	fake := docbanktest.New(t)
	fake.Publish(t, vault)
	client := docbank.NewLocalClient(publishedOnly)

	_, err := client.ResolvePath(t.Context(), "/")
	require.NoError(t, err)
}

func TestLocalClientCarriesAttachmentUploadAndContentOverPinnedConnection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCBANK_HOME", home)
	docbanktest.New(t).Publish(t, home)
	client := docbank.NewLocalClient(publishedOnly)
	payload := bytes.Repeat([]byte("kata attachment "), 64<<10)
	sum := sha256.Sum256(payload)

	folder, err := client.EnsureFolder(t.Context(), "/kata/p")
	require.NoError(t, err)
	version, err := client.Upload(t.Context(), folder, "shot.bin", bytes.NewReader(payload), int64(len(payload)), hex.EncodeToString(sum[:]))
	require.NoError(t, err)
	resp, err := client.Content(t.Context(), version)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.True(t, bytes.Equal(payload, got), "content read back differs from upload")
}
