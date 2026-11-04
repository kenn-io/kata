package kata

import "go.kenn.io/kata/internal/config"

// CronExecutionConfig controls adapter execution on replicated projects.
// The zero value excludes replica execution. Local and hub projects use all.
type CronExecutionConfig struct {
	ReplicaExecute string
	Projects       []CronExecutionProjectConfig
}

// CronExecutionProjectConfig overrides policy for a current local project name.
type CronExecutionProjectConfig struct {
	Name    string
	Execute string
}

func (c CronExecutionConfig) internal() config.CronConfig {
	out := config.CronConfig{ReplicaExecute: c.ReplicaExecute}
	for _, p := range c.Projects {
		out.Projects = append(out.Projects, config.CronProjectConfig{Name: p.Name, Execute: p.Execute})
	}
	return out
}
