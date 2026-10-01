//go:build windows || !cgo

package vector

import (
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver registered as "sqlite"; kit's sqlitevec registers modernc.org/sqlite/vec extension at init
)

// sidecarDriver selects the database/sql driver the vector sidecar opens
// with. The cgo sqlite-vec bindings do not build on Windows, so kit's
// sqlitevec registers the pure-Go modernc.org/sqlite/vec extension there
// (via sqlite3_auto_extension at package init) and expects databases opened
// with modernc's "sqlite" driver; sqlitevec.Register is a no-op in this
// build. driver_cgo.go substitutes mattn/go-sqlite3 on cgo Unix builds.
const sidecarDriver = "sqlite"

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
