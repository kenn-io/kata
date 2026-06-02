package db

// OpenConfig carries the mode flags shared across storage backends. New fields
// must be safe to default: the zero value is "read-write, create-if-missing"
// SQLite behavior.
type OpenConfig struct {
	// ReadOnly opens an existing database without bootstrapping or PRAGMA
	// writes. The cutover and preflight paths use this to inspect an old
	// source DB before the destructive replace.
	ReadOnly bool
}

// OpenOption mutates an OpenConfig. The functional-options style keeps the
// Storage-construction signature stable while individual backends add flags
// over time.
type OpenOption func(*OpenConfig)

// ReadOnly opens the database without bootstrap, schema-version writes, or
// PRAGMA mutations. Equivalent to the prior OpenReadOnly entry point.
func ReadOnly() OpenOption {
	return func(c *OpenConfig) { c.ReadOnly = true }
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
