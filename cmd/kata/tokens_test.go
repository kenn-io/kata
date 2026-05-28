package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestTokensCreateCommand_PrintsPlaintextOnce(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())

	out := requireCmdOutput(t, env, "tokens", "create", "--actor", "wesm", "--name", "laptop")

	assert.Contains(t, out, "created token id=")
	assert.Contains(t, out, "actor=wesm")
	assert.Contains(t, out, "name=laptop")
	assert.Contains(t, out, "Copy this token now. It will not be shown again.")
	assert.Contains(t, out, "Store it in the user's client config.toml or environment.")
	assert.NotContains(t, out, "token_hash")

	plaintext := extractTokenPlaintext(t, out)
	assert.Equal(t, 1, strings.Count(out, plaintext))
	resolved, err := env.DB.ResolveAPIToken(context.Background(), plaintext)
	require.NoError(t, err)
	assert.Equal(t, "wesm", resolved.Actor)
	require.NotNil(t, resolved.Name)
	assert.Equal(t, "laptop", *resolved.Name)
}

func TestTokensCreateCommand_QuietSuppressesAdvisoryText(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())

	out := requireCmdOutput(t, env, "--quiet", "tokens", "create", "--actor", "wesm")

	assert.NotContains(t, out, "Copy this token now")
	assert.NotContains(t, out, "Store it")
	plaintext := extractTokenPlaintext(t, out)
	assert.Equal(t, 1, strings.Count(out, plaintext))
}

func TestTokensListCommand_RedactsPlaintextAndHash(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	createOut := requireCmdOutput(t, env, "tokens", "create", "--actor", "wesm", "--name", "laptop")
	plaintext := extractTokenPlaintext(t, createOut)

	out := requireCmdOutput(t, env, "tokens", "list")

	assert.Contains(t, out, "id  actor  name")
	assert.Contains(t, out, "wesm")
	assert.Contains(t, out, "laptop")
	assert.NotContains(t, out, plaintext)
	assert.NotContains(t, out, "token_hash")
}

func TestTokensRevokeCommand_RevokesToken(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	tok, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "user-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "tokens", "revoke", strconv.FormatInt(tok.ID, 10))

	assert.Contains(t, out, "revoked token id="+strconv.FormatInt(tok.ID, 10))
	assert.Contains(t, out, "actor=alice")
	_, err = env.DB.ResolveAPIToken(context.Background(), "user-token")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestTokensCommands_JSONAndAgentOutput(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())

	jsonOut := requireCmdOutput(t, env, "--json", "tokens", "create", "--actor", "wesm")
	var created struct {
		APIVersion int `json:"kata_api_version"`
		Token      struct {
			ID    int64  `json:"id"`
			Actor string `json:"actor"`
		} `json:"token"`
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &created))
	assert.Equal(t, 1, created.APIVersion)
	assert.Equal(t, "wesm", created.Token.Actor)
	assert.True(t, strings.HasPrefix(created.Plaintext, "kata_"))
	assert.Equal(t, 1, strings.Count(jsonOut, created.Plaintext))
	assert.NotContains(t, jsonOut, "token_hash")

	agentOut := requireCmdOutput(t, env, "--agent", "tokens", "list")
	assert.Contains(t, agentOut, "OK tokens count=1\n")
	assert.Contains(t, agentOut, "- id="+strconv.FormatInt(created.Token.ID, 10)+" actor=wesm")
	assert.NotContains(t, agentOut, created.Plaintext)
	assert.NotContains(t, agentOut, "token_hash")
}

func extractTokenPlaintext(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if token, ok := strings.CutPrefix(line, "token="); ok {
			require.NotEmpty(t, token)
			require.True(t, strings.HasPrefix(token, "kata_"), "plaintext line %q", line)
			return token
		}
	}
	t.Fatalf("missing token= line in output:\n%s", out)
	return ""
}
