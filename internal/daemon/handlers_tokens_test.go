package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCreateTokenEndpoint_BootstrapCreatesToken(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())

	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm", "name": "laptop"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	assert.NotContains(t, string(bs), "token_hash")

	var out struct {
		Token struct {
			ID    int64   `json:"id"`
			Actor string  `json:"actor"`
			Name  *string `json:"name"`
		} `json:"token"`
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	assert.Equal(t, "wesm", out.Token.Actor)
	require.NotNil(t, out.Token.Name)
	assert.Equal(t, "laptop", *out.Token.Name)
	assert.True(t, strings.HasPrefix(out.Plaintext, "kata_"))
	assert.Equal(t, 1, strings.Count(string(bs), out.Plaintext))

	resolved, err := env.DB.ResolveAPIToken(context.Background(), out.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, out.Token.ID, resolved.ID)
	assert.Equal(t, "wesm", resolved.Actor)
}

func TestCreateTokenEndpoint_DBTokenForbidden(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "user-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm"},
		map[string]string{"Authorization": "Bearer user-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusForbidden, "token_admin_forbidden")
}

func TestListTokenEndpoint_RedactsPlaintextAndHash(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	createResp, createBody := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm", "name": "laptop"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equal(t, http.StatusOK, createResp.StatusCode)
	var created struct {
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal(createBody, &created))

	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/tokens", nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	assert.NotContains(t, string(bs), "token_hash")
	assert.NotContains(t, string(bs), created.Plaintext)
	var out struct {
		Tokens []struct {
			Actor string  `json:"actor"`
			Name  *string `json:"name"`
		} `json:"tokens"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.Len(t, out.Tokens, 1)
	assert.Equal(t, "wesm", out.Tokens[0].Actor)
}

func TestRevokeTokenEndpoint_AppendsRevocation(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	tok, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "user-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost,
		"/api/v1/tokens/"+strconv.FormatInt(tok.ID, 10)+"/actions/revoke", nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	var out struct {
		Token struct {
			RevokedAt *string `json:"revoked_at"`
		} `json:"token"`
		Event struct {
			Type  string `json:"type"`
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.NotNil(t, out.Token.RevokedAt)
	assert.Equal(t, "token.revoked", out.Event.Type)
	assert.Equal(t, db.BootstrapActor, out.Event.Actor)
	_, err = env.DB.ResolveAPIToken(context.Background(), "user-token")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestCreateTokenEndpoint_RejectsReservedBootstrapActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "Bootstrap"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")
}

func TestCreateTokenEndpoint_FlagOffStaticTokenCanAdmin(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"))
	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	var out struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	var actor string
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT actor FROM events WHERE type = 'token.created' AND json_extract(payload, '$.token_id') = ?`,
		out.Token.ID).Scan(&actor))
	assert.Equal(t, db.BootstrapActor, actor)
}

func TestCreateTokenEndpoint_LocalNoAuthCanAdmin(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm"}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))

	var out struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	var actor string
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT actor FROM events WHERE type = 'token.created' AND json_extract(payload, '$.token_id') = ?`,
		out.Token.ID).Scan(&actor))
	assert.Equal(t, db.BootstrapActor, actor)
}
