package sqlitestore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	sqlite3 "modernc.org/sqlite/lib"
)

type codedSQLiteErr int

func (e codedSQLiteErr) Error() string { return "sqlite error" }
func (e codedSQLiteErr) Code() int     { return int(e) }

func TestIsTransientRecognizesSQLiteBusyAndLocked(t *testing.T) {
	assert.True(t, IsTransient(codedSQLiteErr(sqlite3.SQLITE_BUSY)))
	assert.True(t, IsTransient(codedSQLiteErr(sqlite3.SQLITE_LOCKED)))
	assert.True(t, IsTransient(fmt.Errorf("wrapped: %w",
		codedSQLiteErr(sqlite3.SQLITE_LOCKED|(1<<8)))))
	assert.False(t, IsTransient(codedSQLiteErr(sqlite3.SQLITE_CONSTRAINT)))
	assert.False(t, IsTransient(errors.New("database is locked")))
}
