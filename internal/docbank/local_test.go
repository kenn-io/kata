package docbank_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

func TestLocalClientFollowsRestartedDocbank(t *testing.T) {
	home := t.TempDir()
	a := docbanktest.New(t)
	a.Publish(t, home)
	client := docbank.NewLocalClient(docbank.Local{Home: home})

	_, err := client.ResolvePath(t.Context(), "/")
	require.NoError(t, err)
	b := docbanktest.New(t)
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
	client := docbank.NewLocalClient(docbank.Local{Home: t.TempDir(), Start: func(context.Context, string) error {
		return errors.New("docbank not installed")
	}})

	_, err := client.ResolvePath(t.Context(), "/")
	apiErr, ok := errors.AsType[*docbank.Error](err)
	require.True(t, ok)
	require.Zero(t, apiErr.Status)
}
