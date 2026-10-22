package pgstore

import (
	"github.com/jackc/pgx/v5"

	"go.kenn.io/kata/internal/config"
)

// Every program that can open PostgreSQL storage links this package, so each
// one resolves routing options exactly as the daemon does when it derives its
// runtime namespace.
func init() {
	config.UsePostgresTargetParser(ParseTargets)
}

// ParseTargets returns the database and the connection targets, primary first
// and including fallbacks, that pgx selects for dsn.
func ParseTargets(dsn string) (string, []config.PostgresTarget, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", nil, err
	}
	targets := make([]config.PostgresTarget, 0, 1+len(cfg.Fallbacks))
	targets = append(targets, config.PostgresTarget{Host: cfg.Host, Port: cfg.Port})
	for _, fallback := range cfg.Fallbacks {
		targets = append(targets, config.PostgresTarget{Host: fallback.Host, Port: fallback.Port})
	}
	return cfg.Database, targets, nil
}
