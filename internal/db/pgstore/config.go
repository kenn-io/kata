package pgstore

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultSchema is the isolated schema used by standalone Postgres opens.
const DefaultSchema = "kata"

var schemaNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// SchemaMode controls whether OpenWithConfig may install missing migration
// assets or must only validate an installation prepared by another role.
type SchemaMode string

const (
	// SchemaModeBootstrap installs missing migrations before serving.
	SchemaModeBootstrap SchemaMode = "bootstrap"
	// SchemaModeValidate performs no DDL and requires an exact installed version.
	SchemaModeValidate SchemaMode = "validate"
)

// Config selects the exact Postgres schema and its startup policy. Schema is
// deliberately a single restricted identifier rather than a raw search_path:
// callers cannot add fallback schemas or inject startup SQL.
type Config struct {
	Schema        string
	SchemaMode    SchemaMode
	SchemaOwner   string
	AllowInsecure bool
}

// DefaultConfig is the standalone profile: a dedicated kata schema that is
// installed on first open.
func DefaultConfig() Config {
	return Config{Schema: DefaultSchema, SchemaMode: SchemaModeBootstrap}
}

// ConfigFromValues converts operator-facing string settings into a complete
// config. Empty values retain the standalone defaults.
func ConfigFromValues(schema, mode, schemaOwner string, allowInsecure bool) Config {
	cfg := DefaultConfig()
	if schema = strings.TrimSpace(schema); schema != "" {
		cfg.Schema = schema
	}
	if mode = strings.TrimSpace(mode); mode != "" {
		cfg.SchemaMode = SchemaMode(mode)
	}
	cfg.SchemaOwner = schemaOwner
	cfg.AllowInsecure = allowInsecure
	return cfg
}

// Validate rejects ambiguous, ambient, and system schema names before a DSN
// is parsed or a network connection is attempted.
func (c Config) Validate() error {
	if !schemaNamePattern.MatchString(c.Schema) {
		return fmt.Errorf("invalid postgres schema %q: require a lowercase identifier of at most 63 bytes", c.Schema)
	}
	if c.Schema == "public" || c.Schema == "information_schema" || strings.HasPrefix(c.Schema, "pg_") {
		return fmt.Errorf("invalid postgres schema %q: system and ambient schemas are not allowed", c.Schema)
	}
	if len(c.SchemaOwner) > 63 {
		return fmt.Errorf("invalid postgres schema owner %q: role identifier exceeds 63 bytes", c.SchemaOwner)
	}
	switch c.SchemaMode {
	case SchemaModeBootstrap, SchemaModeValidate:
		return nil
	default:
		return fmt.Errorf("invalid postgres schema mode %q", c.SchemaMode)
	}
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
