package main

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestMigrateCmd_BringsFreshDBToCurrent(t *testing.T) {
	home := setupKataEnv(t)

	out, err := runCmdOutput(t, nil, "migrate")
	require.NoError(t, err)
	assert.Contains(t, out, "applied schema_version "+strconv.Itoa(db.CurrentSchemaVersion()))
	assert.Contains(t, out, "migrated from 0 to "+strconv.Itoa(db.CurrentSchemaVersion()))

	ctx := context.Background()
	dbPath := filepath.Join(home, "kata.db")
	v, err := sqlitestore.PeekSchemaVersion(ctx, dbPath)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
}

func TestMigrateCmd_OnCurrentDBReportsAlreadyCurrent(t *testing.T) {
	setupKataEnv(t)

	// First invocation brings the DB to current.
	_, err := runCmdOutput(t, nil, "migrate")
	require.NoError(t, err)

	// Second invocation is a no-op.
	out, err := runCmdOutput(t, nil, "migrate")
	require.NoError(t, err)
	assert.Contains(t, out, "already current")
	assert.NotContains(t, out, "applied schema_version")
}
