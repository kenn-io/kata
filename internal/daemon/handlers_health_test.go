package daemon_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestHealth_ReportsSchemaAndUptime(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	var body struct {
		OK               bool   `json:"ok"`
		SchemaVersion    int    `json:"schema_version"`
		APISchemaVersion string `json:"api_schema_version"`
		Uptime           string `json:"uptime"`
		DBPath           string `json:"db_path"`
	}
	getAndUnmarshal(t, ts, "/api/v1/health", http.StatusOK, &body)
	assert.True(t, body.OK)
	assert.Equal(t, db.CurrentSchemaVersion(), body.SchemaVersion)
	assert.Equal(t, daemon.APISchemaVersion, body.APISchemaVersion)
	assert.NotEmpty(t, body.APISchemaVersion)
	assert.NotEmpty(t, body.Uptime)
	assert.NotEmpty(t, body.DBPath)
}
