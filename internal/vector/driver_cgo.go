//go:build !windows && cgo

package vector

import _ "github.com/mattn/go-sqlite3" // cgo SQLite driver registered as "sqlite3"; provides C sqlite symbols for kit's sqlite-vec cgo bindings

// sidecarDriver selects the database/sql driver the vector sidecar opens
// with. On Unix with cgo it is mattn/go-sqlite3, the driver kit's
// sqlitevec cgo build expects its sqlite-vec extension to be loaded into
// (see sqlitevec.Register). On Windows or a no-cgo build, driver_modernc.go
// substitutes the pure-Go modernc driver instead.
const sidecarDriver = "sqlite3"

// sidecarDSN builds the mattn/go-sqlite3 DSN for the sidecar: WAL journal
// mode and a 5s busy timeout, expressed as mattn's `_`-prefixed query
// params.
func sidecarDSN(path string) string {
	return path + "?_journal_mode=WAL&_busy_timeout=5000"
}
