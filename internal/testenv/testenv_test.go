package testenv_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestEnv_BootsDaemonAndAnswersPing(t *testing.T) {
	env := testenv.New(t)
	body := env.RequireOK(t, "/api/v1/ping")
	assert.Contains(t, string(body), `"ok":true`)
}

func TestNewUsesCIResilientRequestTimeout(t *testing.T) {
	env := testenv.New(t)

	require.Equal(t, 10*time.Second, env.HTTP.Timeout)
}
