package sqlitestore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSQLiteURIPathEscapesQueryPunctuation(t *testing.T) {
	require.Equal(t, "/example%3Fmode=ro.db", sqliteURIPath("/example?mode=ro.db"))
}
