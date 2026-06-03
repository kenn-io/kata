package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// TestInstance_ReturnsLocalUID covers spec §8.8: GET /api/v1/instance returns
// the value db.Open seeded into meta.instance_uid.
func TestInstance_ReturnsLocalUID(t *testing.T) {
	ts, d := startDefaultTestServer(t)

	var body struct {
		InstanceUID string `json:"instance_uid"`
	}
	getAndUnmarshal(t, ts, "/api/v1/instance", http.StatusOK, &body)
	assert.Equal(t, d.db.InstanceUID(), body.InstanceUID)
	assert.True(t, uid.Valid(body.InstanceUID), "instance_uid %q invalid", body.InstanceUID)
}

// TestInstance_503WhenUIDUnset covers spec §8.8 second bullet: the handler
// returns 503 instance_uid_unset when the *sqlitestore.Store's cached InstanceUID() is
// empty. In production this is theoretical (db.Open always seeds the row);
// the test reaches it by routing the server through OpenReadOnly, which
// skips the seed step and yields a *DB with empty cached value.
func TestInstance_503WhenUIDUnset(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	// Materialize a real DB file so OpenReadOnly has something to attach to.
	primary, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, primary.Close())

	// Read-only handle bypasses ensureInstanceUID; cached InstanceUID() is "".
	ro, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	require.Empty(t, ro.InstanceUID(), "OpenReadOnly must yield empty cached InstanceUID")

	ts := startTestServer(t, daemon.ServerConfig{DB: ro, StartedAt: time.Now().UTC()})

	resp, bs := getStatusBody(t, ts, "/api/v1/instance")
	assertAPIError(t, resp.StatusCode, bs, http.StatusServiceUnavailable, "instance_uid_unset")
}

// TestInstanceEndpointReturnsVersionAndSchemaVersion covers spec §B4:
// GET /api/v1/instance must surface the daemon's build version and the
// database's schema_version alongside the instance UID so a connecting
// spoke can decide whether it speaks the same wire/version contract.
func TestInstanceEndpointReturnsVersionAndSchemaVersion(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("test-token"))

	req, err := http.NewRequest(http.MethodGet, env.URL+"/api/v1/instance", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := env.HTTP.Do(req) //nolint:gosec // test request to loopback URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		InstanceUID   string `json:"instance_uid"`
		Version       string `json:"version"`
		SchemaVersion int64  `json:"schema_version"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.NotEmpty(t, out.InstanceUID)
	assert.NotEmpty(t, out.Version,
		"version.Version must be populated even in dev builds")
	assert.Equal(t, int64(db.CurrentSchemaVersion()), out.SchemaVersion)
}

func TestInstanceEndpointIncludesDBTokenActor(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	plaintext := strings.Join([]string{"operator", "bearer"}, "-")
	_, _, err := env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{ //nolint:gosec // test-only bearer credential
		PlaintextToken: plaintext,
		Actor:          "operator",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/instance", nil,
		map[string]string{"Authorization": "Bearer " + plaintext})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))

	var out struct {
		Auth struct {
			Kind  string `json:"kind"`
			Actor string `json:"actor"`
		} `json:"auth"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	assert.Equal(t, "db_token", out.Auth.Kind)
	assert.Equal(t, "operator", out.Auth.Actor)
	assert.NotContains(t, string(bs), plaintext)
}
