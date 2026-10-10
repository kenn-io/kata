//go:build windows || !cgo

package vector

import (
	"strings"

	_ "modernc.org/sqlite"     // pure-Go SQLite driver registered as "sqlite"
	_ "modernc.org/sqlite/vec" // registers the sqlite-vec extension for every modernc connection at init
)

// sidecarDriver selects the database/sql driver the vector sidecar opens
// with. The cgo sqlite-vec bindings do not build on Windows, so Windows and
// no-cgo builds use modernc's "sqlite" driver with the pure-Go
// modernc.org/sqlite/vec extension, which registers itself at package init.
// driver_cgo.go substitutes mattn/go-sqlite3 on cgo Unix builds.
const sidecarDriver = "sqlite"

// registerSidecarExtension is a no-op here: importing modernc.org/sqlite/vec
// already loads sqlite-vec into every modernc connection.
func registerSidecarExtension() {}

// sidecarDSN builds the modernc "sqlite" DSN for the sidecar. Connection-local
// settings live here so every pooled connection receives them; ConfigureWAL
// enables WAL separately after fast-mode settings take effect.
func sidecarDSN(path string, fast bool) string {
	pragmas := []string{"_pragma=busy_timeout(5000)"}
	if fast {
		pragmas = append(pragmas,
			"_pragma=synchronous(OFF)",
			"_pragma=temp_store(MEMORY)",
		)
	}
	return path + "?" + strings.Join(pragmas, "&")
}
