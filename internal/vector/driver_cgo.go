//go:build !windows && cgo

package vector

import (
	"sync"

	vecext "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3" // cgo SQLite driver registered as "sqlite3"; provides the C sqlite symbols the sqlite-vec cgo binding links against
)

// sidecarDriver selects the database/sql driver the vector sidecar opens
// with. On Unix with cgo it is mattn/go-sqlite3, with the sqlite-vec
// extension loaded through registerSidecarExtension. On Windows or a no-cgo
// build, driver_modernc.go substitutes the pure-Go modernc driver instead.
const sidecarDriver = "sqlite3"

var registerVecOnce sync.Once

// registerSidecarExtension loads sqlite-vec into every SQLite connection the
// process opens after the call. kit's sqlitevec leaves extension
// registration to the caller; for go-sqlite3 that is the binding's Auto,
// which must run before the sidecar opens its first connection.
func registerSidecarExtension() {
	registerVecOnce.Do(vecext.Auto)
}

// sidecarDSN builds the mattn/go-sqlite3 DSN for the sidecar. Connection-local
// settings live here so every pooled connection receives them; ConfigureWAL
// enables WAL separately after fast-mode settings take effect.
func sidecarDSN(path string, fast bool) string {
	dsn := path + "?_busy_timeout=5000"
	if fast {
		dsn += "&_synchronous=OFF"
	}
	return dsn
}
