package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// General cron spec: last_run is required and nullable. Omitting it would make
// a never-ran job indistinguishable from an older, non-authoritative server.
func TestCronCoreLastRunRequiredNullable(t *testing.T) {
	wire, err := json.Marshal(CronJobFrom(db.CronJob{}))
	require.NoError(t, err)
	var job map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(wire, &job))
	last, present := job["last_run"]
	require.True(t, present, "last_run must be present for a never-ran job")
	require.Equal(t, "null", string(last))
}

// The generated contract must accept the null emitted for never-ran jobs.
func TestCronCoreLastRunSchemaRequiredNullable(t *testing.T) {
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	schema := registry.Schema(reflect.TypeFor[CronJob](), false, "CronJob")
	require.Contains(t, schema.Required, "last_run")
	last := schema.Properties["last_run"]
	require.NotNil(t, last)
	require.True(t, last.Nullable, "last_run schema must allow null")
}
