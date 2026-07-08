//go:build windows || !cgo

package vector

import _ "modernc.org/sqlite" // pure-Go SQLite driver registered as "sqlite"; kit's sqlitevec registers modernc.org/sqlite/vec extension at init

// sidecarDriver selects the database/sql driver the vector sidecar opens
// with. The cgo sqlite-vec bindings do not build on Windows, so kit's
// sqlitevec registers the pure-Go modernc.org/sqlite/vec extension there
// (via sqlite3_auto_extension at package init) and expects databases opened
// with modernc's "sqlite" driver; sqlitevec.Register is a no-op in this
// build. driver_cgo.go substitutes mattn/go-sqlite3 on cgo Unix builds.
const sidecarDriver = "sqlite"

// sidecarDSN builds the modernc "sqlite" DSN for the sidecar, matching the
// pragmas driver_cgo.go sets through mattn's `_`-prefixed params: WAL
// journal mode and a 5s busy timeout. modernc expresses connection pragmas
// as repeated `_pragma=name(value)` params.
func sidecarDSN(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
}
