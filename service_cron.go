package kata

import "go.kenn.io/kata/internal/config"

// CronExecutionConfig controls adapter execution on replicated projects.
// The zero value excludes replica execution. Local and hub projects use all.
type CronExecutionConfig = config.CronConfig

// CronExecutionProjectConfig overrides policy for a current local project name.
type CronExecutionProjectConfig = config.CronProjectConfig
