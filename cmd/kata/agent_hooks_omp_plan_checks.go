package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

func checkOmpPlanPreconditions(ctx context.Context, home string) error {
	output, err := runOmp(ctx, home, "config", "get", "todo.enabled", "--json")
	if err != nil {
		return fmt.Errorf("query omp todo.enabled: %w", err)
	}
	var config struct {
		Value *bool `json:"value"`
	}
	if err := json.Unmarshal(output, &config); err != nil {
		return fmt.Errorf("parse omp todo.enabled: %w", err)
	}
	if config.Value == nil {
		return fmt.Errorf("omp todo.enabled response requires a boolean value")
	}
	if *config.Value {
		return fmt.Errorf("omp's built-in todo is enabled; run omp config set todo.enabled false before rerunning this command (Kata does not change omp's settings)")
	}
	output, err = runOmp(ctx, home, "plugin", "list", "--json")
	if err != nil {
		return fmt.Errorf("query omp plugins: %w", err)
	}
	var plugins struct {
		Marketplace *[]struct {
			ID      string `json:"id"`
			Enabled *bool  `json:"enabled"`
		} `json:"marketplace"`
	}
	if err := json.Unmarshal(output, &plugins); err != nil {
		return fmt.Errorf("parse omp plugins: %w", err)
	}
	if plugins.Marketplace == nil {
		return fmt.Errorf("omp plugin response requires a marketplace list")
	}
	for _, plugin := range *plugins.Marketplace {
		if plugin.ID == "kata@kata-plugin" && (plugin.Enabled == nil || *plugin.Enabled) {
			return fmt.Errorf("the kata-plugin omp plugin also provides a todo tool; remove it with omp plugin uninstall kata@kata-plugin")
		}
	}
	return nil
}
