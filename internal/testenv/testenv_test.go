package testenv_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestEnv_BootsDaemonAndAnswersPing(t *testing.T) {
	env := testenv.New(t)
	body := env.RequireOK(t, "/api/v1/ping")
	assert.Contains(t, string(body), `"ok":true`)
}

func TestEnv_UsesMemoryDBWhenFastSQLiteHarnessEnabled(t *testing.T) {
	t.Setenv("KATA_TEST_FAST_SQLITE", "1")

	env := testenv.New(t)
	body := env.RequireOK(t, "/api/v1/ping")

	assert.Contains(t, string(body), `"ok":true`)
	assert.Contains(t, env.DB.Path(), "mode=memory")
	_, err := os.Stat(filepath.Join(env.Home, "kata.db"))
	require.True(t, errors.Is(err, os.ErrNotExist), "fast testenv should not create kata.db on disk")
}
