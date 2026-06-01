package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/db"
)

func TestApplyMigrationsSetsOpenConfigFlag(t *testing.T) {
	cfg := db.ApplyOpenOptions(db.ApplyMigrations())
	assert.True(t, cfg.ApplyMigrations)
	assert.False(t, cfg.ReadOnly)
}

func TestApplyMigrationsAndReadOnlyComposeIndependently(t *testing.T) {
	cfg := db.ApplyOpenOptions(db.ApplyMigrations(), db.ReadOnly())
	assert.True(t, cfg.ApplyMigrations)
	assert.True(t, cfg.ReadOnly)
}

func TestMigrationResultZeroValueIsNoop(t *testing.T) {
	var r db.MigrationResult
	assert.Equal(t, 0, r.From)
	assert.Equal(t, 0, r.To)
	assert.Nil(t, r.Applied)
}

func TestBaselineSchemaVersionPinnedAtTwelve(t *testing.T) {
	// BaselineSchemaVersion is the floor of the embedded migration ladder.
	// Phase 2 stamped it at 12 and Phase 3 added a migration on top — so
	// baseline stays at 12 while CurrentSchemaVersion advances past it.
	assert.Equal(t, 12, db.BaselineSchemaVersion)
}

func TestCurrentSchemaVersionAtThirteenAfterIdempotency(t *testing.T) {
	// Phase 3 closed the events idempotency race with a UNIQUE partial
	// index on both backends; that migration bumped current from 12 to 13.
	assert.Equal(t, 13, db.CurrentSchemaVersion())
}
