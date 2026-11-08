package docbank_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

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

func TestLocalClientAbandonsKeyProofWhenRequestEnds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCBANK_HOME", home)
	proofStarted, proofAbandoned := make(chan struct{}), make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == kitdaemon.DefaultPingPath {
			kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{Service: "docbank", Version: "test"}).ServeHTTP(w, r)
			return
		}
		close(proofStarted)
		<-r.Context().Done() // never answers the key challenge
		close(proofAbandoned)
	}))
	t.Cleanup(stalled.Close)
	rec := kitdaemon.NewRuntimeRecord("docbank", "test", kitdaemon.Endpoint{Network: "tcp", Address: stalled.Listener.Addr().String()})
	rec.Metadata = map[string]string{"api_key": "test-key"}
	_, err := (kitdaemon.RuntimeStore{Dir: home}).Write(rec)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-proofStarted:
		case <-ctx.Done():
		}
		cancel()
	}()

	_, err = docbank.NewLocalClient(publishedOnly).ResolvePath(ctx, "/")

	requireUnreachable(t, err)
	select {
	case <-proofAbandoned:
	case <-time.After(2 * time.Second):
		t.Fatal("the key proof kept its connection open after the request ended")
	}
}
