package client_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client"
)

func writeDaemonCatalog(t *testing.T, home, catalog string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(catalog), 0o600))
}

func TestDaemonCatalogIsEmptyWithoutConfig(t *testing.T) {
	isolateKataHome(t)

	daemons, err := client.DaemonCatalog()
	require.NoError(t, err)
	assert.Empty(t, daemons)
}

func TestDaemonCatalogListsEntriesInFileOrderWithoutCredentials(t *testing.T) {
	home := isolateKataHome(t)
	writeDaemonCatalog(t, home, `active_daemon = "hub"

[[daemon]]
name = "personal"
local = true

[[daemon]]
name = "hub"
url = "https://daemon.example:7443/ignored"
token = "inline-secret"

[[daemon]]
name = "staging"
url = "http://operator:password@100.64.0.5:7777"
token_env = "EXAMPLE_STAGING_TOKEN"
allow_insecure = true

[[daemon]]
name = "work"
local = true
home = "`+filepath.ToSlash(t.TempDir())+`"
instance_uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"
`)

	daemons, err := client.DaemonCatalog()
	require.NoError(t, err)
	assert.Equal(t, []client.CatalogDaemon{
		{Name: "personal", Local: true, Endpoint: "local"},
		{Name: "hub", Endpoint: "daemon.example:7443", HasToken: true, Active: true},
		{Name: "staging", Endpoint: "100.64.0.5:7777", HasToken: true},
		{Name: "work", Local: true, Endpoint: "local"},
	}, daemons)
}
