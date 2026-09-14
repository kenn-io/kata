package main

import (
	"fmt"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func validateFederationStartupConfig(cfg *config.DaemonConfig) error {
	for i, mapping := range cfg.Federation.Projects {
		if mapping.CredentialProvider != nil {
			// The config loader forbids a caller-selected actor here. Validate
			// the provider's approved actor when its result arrives instead.
			continue
		}
		if err := db.ValidateTokenActor(mapping.Actor); err != nil {
			return fmt.Errorf("federation.project[%d].actor: %w", i, err)
		}
	}
	return nil
}
