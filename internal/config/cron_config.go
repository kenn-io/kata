package config

import (
	"fmt"
	"strings"
)

// CronExecuteMode controls which replica definitions a local adapter may execute.
type CronExecuteMode string

const (
	// ModeNone excludes all replica jobs and workflows.
	ModeNone CronExecuteMode = "none"
	// ModeOwned requires the member actor to be the stored creator.
	ModeOwned CronExecuteMode = "owned"
	// ModeAll allows all replica jobs and workflows.
	ModeAll CronExecuteMode = "all"
)

func validExecuteMode(value string) bool {
	switch CronExecuteMode(value) {
	case ModeNone, ModeOwned, ModeAll:
		return true
	default:
		return false
	}
}

// CronConfig is local execution policy, never replicated with definitions.
type CronConfig struct {
	ReplicaExecute string              `toml:"replica_execute"`
	Projects       []CronProjectConfig `toml:"project"`
}

// CronProjectConfig overrides execution policy for the current local name.
type CronProjectConfig struct {
	Name    string `toml:"name"`
	Execute string `toml:"execute"`
}

func trimCronConfig(cfg *CronConfig) {
	cfg.ReplicaExecute = strings.TrimSpace(cfg.ReplicaExecute)
	for i := range cfg.Projects {
		cfg.Projects[i].Execute = strings.TrimSpace(cfg.Projects[i].Execute)
	}
}

// Validate checks local policy without accessing storage. An absent global
// mode selects the replica default of none; a project override needs a mode.
func (cfg CronConfig) Validate() error {
	if cfg.ReplicaExecute != "" && !validExecuteMode(cfg.ReplicaExecute) {
		return fmt.Errorf("cron.replica_execute must be none, owned or all")
	}
	seen := make(map[string]bool, len(cfg.Projects))
	for i, project := range cfg.Projects {
		if err := ValidateProjectName(project.Name); err != nil {
			return fmt.Errorf("cron.project[%d].name: %w", i, err)
		}
		if seen[project.Name] {
			return fmt.Errorf("cron.project[%d]: duplicate name %q", i, project.Name)
		}
		seen[project.Name] = true
		if !validExecuteMode(project.Execute) {
			return fmt.Errorf("cron.project[%d].execute must be none, owned or all", i)
		}
	}
	return nil
}
