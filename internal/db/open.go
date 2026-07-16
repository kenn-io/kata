package db

// OpenConfig carries the mode flags shared across storage backends. New fields
// must be safe to default: the zero value is "read-write, create-if-missing"
// behavior for both backends.
type OpenConfig struct {
	// ReadOnly opens an existing database without bootstrapping, migrations, or
	// backend-specific setup writes. Cutover and export paths use it for
	// non-mutating inspection.
	ReadOnly bool
	// Serving holds the backend's process-lifetime serving fence. It is used by
	// daemons, not short-lived CLI handles, so offline restore can quiesce every
	// host before replacing state.
	Serving bool
}

// OpenOption mutates an OpenConfig. The functional-options style keeps the
// Storage-construction signature stable while individual backends add flags
// over time.
type OpenOption func(*OpenConfig)

// ReadOnly opens the database without bootstrap or schema-version writes.
func ReadOnly() OpenOption {
	return func(c *OpenConfig) { c.ReadOnly = true }
}

// Serving marks a long-running daemon storage handle.
func Serving() OpenOption {
	return func(c *OpenConfig) { c.Serving = true }
}

// ApplyOpenOptions folds the variadic options into a fresh OpenConfig. Backends
// call this at the top of their Open function so option handling lives in one
// place.
func ApplyOpenOptions(opts ...OpenOption) OpenConfig {
	var c OpenConfig
	for _, o := range opts {
		if o == nil {
			continue
		}
		o(&c)
	}
	return c
}
