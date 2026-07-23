package main

import (
	"fmt"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func validateFederationStartupConfig(cfg *config.DaemonConfig) error {
	for i, mapping := range cfg.Federation.Projects {
		if err := db.ValidateTokenActor(mapping.Actor); err != nil {
			return fmt.Errorf("federation.project[%d].actor: %w", i, err)
		}
	}
	return nil
}
